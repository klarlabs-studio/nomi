//go:build linux

package egress

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// TestNewLinuxAttachesAndCloses spins up a real filter, populates a
// few IPs, and tears it down. Skipped if CAP_BPF / CAP_NET_ADMIN
// aren't available (e.g. CI without privileged mode). The point of
// the test is to catch verifier rejections of buildProgram so a
// botched edit lands as a test failure rather than a runtime panic
// in production.
func TestNewLinuxAttachesAndCloses(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("eBPF cgroup_skb load requires root / CAP_BPF / CAP_NET_ADMIN")
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skipf("cgroup v2 not mounted at /sys/fs/cgroup: %v", err)
	}

	root := t.TempDir()
	// The kernel doesn't accept arbitrary tmpfs paths for cgroup
	// attach; use the real cgroup root but a unique child name.
	cfg := Config{
		RunID:      "test-" + filepath.Base(root),
		CgroupRoot: "/sys/fs/cgroup",
	}
	f, err := New(cfg)
	if err != nil {
		// Verifier failure surfaces here — bubble up the full error
		// so debugging the program asm is direct.
		if errors.Is(err, ErrUnsupported) {
			t.Skipf("kernel reports unsupported: %v", err)
		}
		t.Fatalf("New failed: %v", err)
	}
	t.Cleanup(func() {
		_ = f.Close()
	})

	if err := f.AddIP(net.ParseIP("203.0.113.10")); err != nil {
		t.Errorf("AddIP v4: %v", err)
	}
	// IPv6 is now actively enforced via the v6 HASH map; verifier-
	// rejecting buildProgram on a botched v6 branch would surface
	// here too.
	if err := f.AddIP(net.ParseIP("2001:db8::1")); err != nil {
		t.Errorf("AddIP v6: %v", err)
	}
	if err := f.AddIP(net.ParseIP("::ffff:203.0.113.10")); err != nil {
		// v4-mapped v6 — net.IP.To4 returns the v4 form, so this should
		// land in the v4 map, not the v6 one. Just checking it doesn't
		// error out.
		t.Errorf("AddIP v4-mapped v6: %v", err)
	}
	if f.CgroupPath() == "" {
		t.Error("CgroupPath should be set after attach")
	}
}

// TestAddIPRejectsGarbage guards the AddIP rejection branch — passing
// a non-IP (nil, empty bytes) should error rather than silently
// succeed against an empty map, which would be the wrong default for
// a security primitive.
func TestAddIPRejectsGarbage(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("eBPF cgroup_skb load requires root")
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skipf("cgroup v2 not mounted: %v", err)
	}
	f, err := New(Config{
		RunID:      "test-reject-" + filepath.Base(t.TempDir()),
		CgroupRoot: "/sys/fs/cgroup",
	})
	if err != nil {
		t.Skipf("filter setup unavailable: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	if err := f.AddIP(net.IP{0x01, 0x02}); err == nil {
		t.Error("expected error for malformed IP (2-byte slice)")
	}
}
