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
	// the BPF program is attached to. Used for diagnostics and tests;
	// docker integration goes through DockerCgroupParent instead.
	CgroupPath() string

	// DockerCgroupParent returns the value to pass to
	// `docker run --cgroup-parent`. For the cgroupfs driver this is
	// the absolute cgroup path; for the systemd driver it's the bare
	// slice name (e.g. "nomi-sandbox-abc123.slice"). cgroup_skb
	// attached on the parent applies to every child scope docker
	// creates underneath.
	DockerCgroupParent() string

	// AddIP appends an allowlisted destination IP to the BPF map.
	// Both IPv4 and IPv6 are accepted; the map key encoding follows
	// the program's wire format (see egress_linux.go).
	AddIP(ip net.IP) error

	// Close detaches the program, closes the map, and removes the
	// cgroup directory. Safe to call multiple times.
	Close() error
}

// Driver names the docker cgroup driver the filter must target. Empty
// or unknown values default to DriverCgroupfs (the historical Nomi
// behaviour). Detected via `docker info --format '{{.CgroupDriver}}'`
// in the docker backend and cached for the daemon's lifetime.
type Driver string

const (
	// DriverCgroupfs: docker creates child cgroups directly under
	// --cgroup-parent's filesystem path. Filter uses a flat
	// `nomi-sandbox-<id>` directory and passes its absolute path.
	DriverCgroupfs Driver = "cgroupfs"

	// DriverSystemd: docker creates a transient .scope under the
	// named .slice. Filter creates `nomi-sandbox-<id>.slice` so
	// docker's child scope inherits the BPF attachment.
	DriverSystemd Driver = "systemd"
)

// Config controls the per-filter setup.
type Config struct {
	// RunID is mixed into the cgroup directory name so concurrent runs
	// don't collide. Required; empty means "use a generated random id".
	RunID string

	// CgroupRoot defaults to "/sys/fs/cgroup". Tests override.
	CgroupRoot string

	// DockerCgroupDriver selects the cgroup naming scheme + the value
	// returned by DockerCgroupParent. Empty defaults to cgroupfs to
	// preserve historical behaviour on hosts where detection wasn't
	// possible.
	DockerCgroupDriver Driver
}
