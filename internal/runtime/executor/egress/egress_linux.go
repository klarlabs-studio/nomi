//go:build linux

package egress

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
)

// New attaches a cgroup_skb/egress BPF program to a freshly-created
// cgroup under /sys/fs/cgroup. Two HASH maps back the program: one
// keyed by 4-byte IPv4 destinations, one keyed by 16-byte IPv6
// destinations. Both start empty — call AddIP for each allowlisted
// destination (any IP family) before starting the container. Loading
// requires CAP_BPF + CAP_NET_ADMIN; without those, expect a
// "operation not permitted" error which the docker backend should
// surface and fall back from.
func New(cfg Config) (Filter, error) {
	root := cfg.CgroupRoot
	if root == "" {
		root = "/sys/fs/cgroup"
	}

	runID := cfg.RunID
	if runID == "" {
		var buf [8]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return nil, fmt.Errorf("egress: random run id: %w", err)
		}
		runID = hex.EncodeToString(buf[:])
	}

	// Driver-dependent cgroup naming:
	//   cgroupfs: flat dir, docker accepts the absolute path
	//   systemd:  must end in .slice, docker accepts the bare name
	// We create the cgroup ourselves in both cases — systemd doesn't
	// strictly require its own slice-creation API for an empty parent
	// slice; it just won't track our dir under any service unit, which
	// is fine because docker creates the actual workload scope as a
	// child once it spawns the container.
	driver := cfg.DockerCgroupDriver
	if driver == "" {
		driver = DriverCgroupfs
	}
	var cgroupName, dockerParent string
	switch driver {
	case DriverSystemd:
		cgroupName = "nomi-sandbox-" + runID + ".slice"
		dockerParent = cgroupName
	default: // DriverCgroupfs
		cgroupName = "nomi-sandbox-" + runID
		dockerParent = filepath.Join(root, cgroupName)
	}
	cgroupPath := filepath.Join(root, cgroupName)
	if err := os.Mkdir(cgroupPath, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("egress: mkdir cgroup %s: %w", cgroupPath, err)
	}

	v4Map, err := ebpf.NewMap(&ebpf.MapSpec{
		Name:       "nomi_egress_v4",
		Type:       ebpf.Hash,
		KeySize:    4, // IPv4 destination, big-endian
		ValueSize:  1, // unused — presence in the map = allowed
		MaxEntries: 1024,
	})
	if err != nil {
		_ = os.Remove(cgroupPath)
		return nil, fmt.Errorf("egress: create v4 map: %w", err)
	}

	v6Map, err := ebpf.NewMap(&ebpf.MapSpec{
		Name:       "nomi_egress_v6",
		Type:       ebpf.Hash,
		KeySize:    16, // IPv6 destination, big-endian
		ValueSize:  1,
		MaxEntries: 1024,
	})
	if err != nil {
		_ = v4Map.Close()
		_ = os.Remove(cgroupPath)
		return nil, fmt.Errorf("egress: create v6 map: %w", err)
	}

	prog, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:         "nomi_egress",
		Type:         ebpf.CGroupSKB,
		License:      "GPL",
		Instructions: buildProgram(v4Map, v6Map),
	})
	if err != nil {
		_ = v4Map.Close()
		_ = v6Map.Close()
		_ = os.Remove(cgroupPath)
		return nil, fmt.Errorf("egress: load program: %w", err)
	}

	attached, err := link.AttachCgroup(link.CgroupOptions{
		Path:    cgroupPath,
		Attach:  ebpf.AttachCGroupInetEgress,
		Program: prog,
	})
	if err != nil {
		_ = prog.Close()
		_ = v4Map.Close()
		_ = v6Map.Close()
		_ = os.Remove(cgroupPath)
		return nil, fmt.Errorf("egress: attach cgroup: %w", err)
	}

	return &linuxFilter{
		cgroupPath:   cgroupPath,
		dockerParent: dockerParent,
		prog:         prog,
		v4Map:        v4Map,
		v6Map:        v6Map,
		attached:     attached,
	}, nil
}

type linuxFilter struct {
	cgroupPath   string
	dockerParent string
	prog         *ebpf.Program
	v4Map        *ebpf.Map
	v6Map        *ebpf.Map
	attached     link.Link
}

func (f *linuxFilter) CgroupPath() string         { return f.cgroupPath }
func (f *linuxFilter) DockerCgroupParent() string { return f.dockerParent }

// AddIP inserts an allowlisted destination into the appropriate map.
// IPv4 addresses land in the 4-byte v4 map; IPv6 in the 16-byte v6
// map. The wire form is big-endian — matching the byte order
// bpf_skb_load_bytes pulls off the IP header.
func (f *linuxFilter) AddIP(ip net.IP) error {
	if v4 := ip.To4(); v4 != nil {
		var key [4]byte
		copy(key[:], v4)
		var value uint8 = 1
		return f.v4Map.Put(key[:], value)
	}
	v6 := ip.To16()
	if v6 == nil {
		return fmt.Errorf("egress: AddIP: %q is neither IPv4 nor IPv6", ip)
	}
	var key [16]byte
	copy(key[:], v6)
	var value uint8 = 1
	return f.v6Map.Put(key[:], value)
}

func (f *linuxFilter) Close() error {
	var first error
	if f.attached != nil {
		if err := f.attached.Close(); err != nil && first == nil {
			first = err
		}
		f.attached = nil
	}
	if f.prog != nil {
		if err := f.prog.Close(); err != nil && first == nil {
			first = err
		}
		f.prog = nil
	}
	if f.v4Map != nil {
		if err := f.v4Map.Close(); err != nil && first == nil {
			first = err
		}
		f.v4Map = nil
	}
	if f.v6Map != nil {
		if err := f.v6Map.Close(); err != nil && first == nil {
			first = err
		}
		f.v6Map = nil
	}
	if f.cgroupPath != "" {
		if err := os.Remove(f.cgroupPath); err != nil && !errors.Is(err, os.ErrNotExist) && first == nil {
			first = err
		}
		f.cgroupPath = ""
	}
	return first
}

// buildProgram emits the cgroup_skb/egress BPF program. Semantics:
//
//	r1 = __sk_buff *ctx
//	switch ctx.protocol:
//	  ETH_P_IP   (host-order 0x0008 on LE): goto v4_path
//	  ETH_P_IPV6 (host-order 0xDD86 on LE): goto v6_path
//	  default:                              return 1 (allow non-IP)
//
//	v4_path:
//	  bpf_skb_load_bytes(ctx, 16, &stack[0..4), 4)
//	  if load failed: return 1 (fail-open on truncated packets)
//	  if bpf_map_lookup_elem(&v4_map, &stack[0..4)) != NULL: return 1
//	  return 0 (drop)
//
//	v6_path:
//	  bpf_skb_load_bytes(ctx, 24, &stack[0..16), 16)
//	  if load failed: return 1
//	  if bpf_map_lookup_elem(&v6_map, &stack[0..16)) != NULL: return 1
//	  return 0
//
// __sk_buff layout — protocol at offset 16. The verifier exposes the
// __be16 wire field as a native u32 in host byte order, so on the
// little-endian architectures we ship to (x86_64, arm64) ETH_P_IP
// becomes 0x00000008 and ETH_P_IPV6 becomes 0x0000DD86.
//
// IPv6 header layout — destination address at offset 24:
//
//	0: version + traffic class + flow label (4 bytes)
//	4: payload length (2)
//	6: next header (1)
//	7: hop limit (1)
//	8: source address (16)
//	24: destination address (16) ← what we load
//
// Stack frame: one 16-byte slot at RFP-16 holds the destination for
// either family. v4 path writes only the low 4 bytes (offsets -16 to
// -12) and looks up with the v4 map's 4-byte key against the same
// pointer; the upper 12 bytes are read by neither path.
func buildProgram(v4Map, v6Map *ebpf.Map) asm.Instructions {
	const (
		skbProtocolOffset = 16     // offsetof(__sk_buff, protocol)
		ipHdrDaddrOffset  = 16     // offsetof(struct iphdr, daddr)
		ip6HdrDaddrOffset = 24     // offsetof(struct ipv6hdr, daddr)
		ethPIPHostOrderLE = 8      // bpf_htons(ETH_P_IP)   on LE
		ethPIPV6HostOrdLE = 0xDD86 // bpf_htons(ETH_P_IPV6) on LE
		retDrop           = 0
		retAllow          = 1
		stackDaddrOffset  = -16
	)

	return asm.Instructions{
		// r6 = ctx (preserve across helper calls)
		asm.Mov.Reg(asm.R6, asm.R1),

		// r7 = ctx->protocol (u32 load)
		asm.LoadMem(asm.R7, asm.R6, skbProtocolOffset, asm.Word),

		// Branch on protocol family.
		asm.JEq.Imm(asm.R7, ethPIPHostOrderLE, "v4_load"),
		asm.JEq.Imm(asm.R7, ethPIPV6HostOrdLE, "v6_load"),
		// Neither IPv4 nor IPv6 → allow (ICMP, ARP at L3 if ever seen,
		// link-local control traffic). The threat model here is
		// application-level egress, not L3 stack hardening.
		asm.Mov.Imm(asm.R0, retAllow),
		asm.Return(),

		// v4_load: bpf_skb_load_bytes(ctx, 16, &stack[-16], 4)
		asm.Mov.Reg(asm.R1, asm.R6).WithSymbol("v4_load"),
		asm.Mov.Imm(asm.R2, ipHdrDaddrOffset),
		asm.Mov.Reg(asm.R3, asm.RFP),
		asm.Add.Imm(asm.R3, stackDaddrOffset),
		asm.Mov.Imm(asm.R4, 4),
		asm.FnSkbLoadBytes.Call(),
		// if r0 != 0: load failed — fail-open
		asm.JEq.Imm(asm.R0, 0, "v4_lookup"),
		asm.Mov.Imm(asm.R0, retAllow),
		asm.Return(),

		// v4_lookup: bpf_map_lookup_elem(&v4_map, &stack[-16])
		asm.LoadMapPtr(asm.R1, v4Map.FD()).WithSymbol("v4_lookup"),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, stackDaddrOffset),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "drop"),
		asm.Mov.Imm(asm.R0, retAllow),
		asm.Return(),

		// v6_load: bpf_skb_load_bytes(ctx, 24, &stack[-16], 16)
		asm.Mov.Reg(asm.R1, asm.R6).WithSymbol("v6_load"),
		asm.Mov.Imm(asm.R2, ip6HdrDaddrOffset),
		asm.Mov.Reg(asm.R3, asm.RFP),
		asm.Add.Imm(asm.R3, stackDaddrOffset),
		asm.Mov.Imm(asm.R4, 16),
		asm.FnSkbLoadBytes.Call(),
		asm.JEq.Imm(asm.R0, 0, "v6_lookup"),
		asm.Mov.Imm(asm.R0, retAllow),
		asm.Return(),

		// v6_lookup: bpf_map_lookup_elem(&v6_map, &stack[-16])
		asm.LoadMapPtr(asm.R1, v6Map.FD()).WithSymbol("v6_lookup"),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, stackDaddrOffset),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "drop"),
		asm.Mov.Imm(asm.R0, retAllow),
		asm.Return(),

		// drop: return 0
		asm.Mov.Imm(asm.R0, retDrop).WithSymbol("drop"),
		asm.Return(),
	}
}

// ipv4ToBE is a small helper retained for tests; the IPv4 wire form
// the BPF v4 map keys use is the big-endian 4-byte encoding.
func ipv4ToBE(ip net.IP) []byte {
	v4 := ip.To4()
	if v4 == nil {
		return nil
	}
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, binary.BigEndian.Uint32(v4))
	return out
}
