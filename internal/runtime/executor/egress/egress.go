// Package egress provides a kernel-level egress filter for the docker
// executor backend. The docker backend's host_allowlist already pins
// allowlisted DNS names via --add-host and breaks in-container DNS for
// the rest, but that's bypassable by code that hardcodes IPs. This
// package layers a cgroup_skb/egress eBPF program on top: every
// outbound packet from the container's cgroup gets its destination
// looked up in a HASH map; misses are dropped at the kernel.
//
// Linux-only. Other platforms compile against the stub in
// egress_other.go which returns ErrUnsupported from New, letting the
// docker backend fall back to the DNS-only path.
//
// Experimental: gated by the NOMI_EGRESS_EBPF=1 env var on the daemon
// process so the default behaviour is unchanged. Loading also requires
// CAP_BPF + CAP_NET_ADMIN; absence is surfaced as an error rather than
// a silent no-op.
package egress

import (
	"errors"
	"net"
)

// ErrUnsupported indicates the platform / kernel / capabilities can't
// run a cgroup_skb egress filter. The docker backend treats this as a
// soft failure and falls back to DNS-only allowlisting.
var ErrUnsupported = errors.New("egress: cgroup_skb eBPF filter unsupported on this platform/kernel")

// Filter is a single attached cgroup_skb/egress program plus its
// associated allowlist map and cgroup. Close detaches the program,
// closes the map, and removes the cgroup directory. Filters are
// per-container and not reusable across runs.
type Filter interface {
	// CgroupPath returns the absolute path under /sys/fs/cgroup that
	// docker should target via --cgroup-parent. Caller passes the
	// returned path to docker so the container ends up inside the
	// cgroup the BPF program is attached to.
	CgroupPath() string

	// AddIP appends an allowlisted destination IP to the BPF map.
	// Both IPv4 and IPv6 are accepted; the map key encoding follows
	// the program's wire format (see egress_linux.go).
	AddIP(ip net.IP) error

	// Close detaches the program, closes the map, and removes the
	// cgroup directory. Safe to call multiple times.
	Close() error
}

// Config controls the per-filter setup.
type Config struct {
	// RunID is mixed into the cgroup directory name so concurrent runs
	// don't collide. Required; empty means "use a generated random id".
	RunID string

	// CgroupRoot defaults to "/sys/fs/cgroup". Tests override.
	CgroupRoot string
}
