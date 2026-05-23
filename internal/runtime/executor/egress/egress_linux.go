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
// cgroup under /sys/fs/cgroup. The map starts empty — call AddIP for
// each allowlisted destination before starting the container. Loading
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

	cgroupPath := filepath.Join(root, "nomi-sandbox-"+runID)
	if err := os.Mkdir(cgroupPath, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("egress: mkdir cgroup %s: %w", cgroupPath, err)
	}

	allowMap, err := ebpf.NewMap(&ebpf.MapSpec{
		Name:       "nomi_egress_allow",
		Type:       ebpf.Hash,
		KeySize:    4, // IPv4 destination, big-endian
		ValueSize:  1, // unused — presence in the map = allowed
		MaxEntries: 1024,
	})
	if err != nil {
		_ = os.Remove(cgroupPath)
		return nil, fmt.Errorf("egress: create map: %w", err)
	}

	prog, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:    "nomi_egress",
		Type:    ebpf.CGroupSKB,
		License: "GPL",
		Instructions: buildProgram(allowMap),
	})
	if err != nil {
		_ = allowMap.Close()
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
		_ = allowMap.Close()
		_ = os.Remove(cgroupPath)
		return nil, fmt.Errorf("egress: attach cgroup: %w", err)
	}

	return &linuxFilter{
		cgroupPath: cgroupPath,
		prog:       prog,
		allowMap:   allowMap,
		attached:   attached,
	}, nil
}

type linuxFilter struct {
	cgroupPath string
	prog       *ebpf.Program
	allowMap   *ebpf.Map
	attached   link.Link
}

func (f *linuxFilter) CgroupPath() string { return f.cgroupPath }

// AddIP inserts an IPv4 destination into the allowlist map. IPv6 is
// currently dropped on the floor — the program only filters IPv4
// because cgroup_skb/egress sees the IP header directly and supporting
// v6 doubles program size for a marginal real-world payoff in v1.
func (f *linuxFilter) AddIP(ip net.IP) error {
	v4 := ip.To4()
	if v4 == nil {
		// IPv6 not yet supported; skip silently so callers can pass mixed
		// resolver output without filtering upstream.
		return nil
	}
	var key [4]byte
	copy(key[:], v4)
	var value uint8 = 1
	return f.allowMap.Put(key[:], value)
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
	if f.allowMap != nil {
		if err := f.allowMap.Close(); err != nil && first == nil {
			first = err
		}
		f.allowMap = nil
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
//	if ctx.protocol != ETH_P_IP (0x0008 BE): return 1 (allow non-IPv4)
//	bpf_skb_load_bytes(ctx, offsetof(iphdr, daddr) = 16, &daddr, 4)
//	if load failed (r0 != 0): return 1 (fail-open on load errors;
//	   verifier guarantees the call shape but a parse failure could
//	   happen on truncated packets — letting them through is the safer
//	   default since the alternative is bricking the container)
//	if bpf_map_lookup_elem(&allow, &daddr) != NULL: return 1 (allow)
//	return 0 (drop)
//
// __sk_buff layout — protocol at offset 16, all other fields exposed
// via the verifier rewriter at fixed offsets that match
// include/uapi/linux/bpf.h:
//
//	struct __sk_buff { __u32 len; __u32 pkt_type; __u32 mark;
//	                   __u32 queue_mapping; __u32 protocol; ... }
func buildProgram(allowMap *ebpf.Map) asm.Instructions {
	const (
		skbProtocolOffset = 16 // offsetof(__sk_buff, protocol)
		ipHdrDaddrOffset  = 16 // offsetof(struct iphdr, daddr) for v4
		ethPIPBE          = 0x0008
		retDrop           = 0
		retAllow          = 1
	)

	return asm.Instructions{
		// r6 = ctx (preserve across helper calls)
		asm.Mov.Reg(asm.R6, asm.R1),

		// r7 = ctx->protocol (u32 load)
		asm.LoadMem(asm.R7, asm.R6, skbProtocolOffset, asm.Word),

		// if r7 != 0x0008 (ETH_P_IP big-endian on wire, stored as native u32) → allow
		// Linux exposes ctx->protocol as host-byte-order on most archs; on
		// little-endian that's the swapped value 0x00000008. cgroup_skb
		// programs see __sk_buff which normalises this to host order, so
		// comparing against 8 is right on every supported architecture.
		asm.JNE.Imm(asm.R7, 8, "load_daddr"),
		asm.Mov.Imm(asm.R0, retAllow),
		asm.Return(),

		// load_daddr: bpf_skb_load_bytes(ctx, 16, &daddr_stack, 4)
		asm.Mov.Reg(asm.R1, asm.R6).WithSymbol("load_daddr"),
		asm.Mov.Imm(asm.R2, ipHdrDaddrOffset),
		asm.Mov.Reg(asm.R3, asm.RFP),
		asm.Add.Imm(asm.R3, -4),
		asm.Mov.Imm(asm.R4, 4),
		asm.FnSkbLoadBytes.Call(),

		// if r0 != 0: load failed (e.g. truncated packet) — fail-open
		asm.JEq.Imm(asm.R0, 0, "lookup"),
		asm.Mov.Imm(asm.R0, retAllow),
		asm.Return(),

		// lookup: bpf_map_lookup_elem(&allow, &daddr_stack)
		asm.LoadMapPtr(asm.R1, allowMap.FD()).WithSymbol("lookup"),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, -4),
		asm.FnMapLookupElem.Call(),

		// if r0 != NULL: hit — allow
		asm.JEq.Imm(asm.R0, 0, "drop"),
		asm.Mov.Imm(asm.R0, retAllow),
		asm.Return(),

		// drop: return 0
		asm.Mov.Imm(asm.R0, retDrop).WithSymbol("drop"),
		asm.Return(),
	}
}

// ipv4ToBE is a small helper exported for tests: encodes the 4-byte
// big-endian wire form the BPF map keys use. Keeping it package-local
// since it's currently only used by tests via the package's own
// _test.go file.
func ipv4ToBE(ip net.IP) []byte {
	v4 := ip.To4()
	if v4 == nil {
		return nil
	}
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, binary.BigEndian.Uint32(v4))
	return out
}
