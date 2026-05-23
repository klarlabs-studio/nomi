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
	// IPv6 must be silently accepted (not yet enforced — see comment
	// in egress_linux.go); regression-guarding that v6 doesn't crash.
	if err := f.AddIP(net.ParseIP("2001:db8::1")); err != nil {
		t.Errorf("AddIP v6: %v", err)
	}
	if f.CgroupPath() == "" {
		t.Error("CgroupPath should be set after attach")
	}
}
