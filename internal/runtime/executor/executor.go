// Package executor provides pluggable subprocess execution backends. The
// runtime resolves a backend per assistant (default: local) and command.exec
// (and any future side-effecting tool) delegates the actual process spawn to
// the chosen Backend. This is the substrate that future container-isolated
// backends (docker, gvisor) plug into without touching the tool layer.
package executor

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// Known backend names. Adding a new backend means defining a constant here,
// implementing Backend, registering it at boot, and exposing it in the
// assistant builder UI.
const (
	BackendLocal  = "local"
	BackendDocker = "docker"
	BackendGvisor = "gvisor"
)

// DefaultBackend is used when an assistant has no explicit choice or the
// requested backend is not registered. Preserves pre-sandboxing behavior.
const DefaultBackend = BackendLocal

// NetworkMode controls outbound connectivity for container backends. Local
// backend ignores this field. "" defaults to NetworkNone — the deny-by-
// default posture a permission policy with no explicit network.egress
// rule should produce.
type NetworkMode string

const (
	// NetworkNone disables outbound networking entirely. Maps to
	// `docker run --network=none`.
	NetworkNone NetworkMode = "none"

	// NetworkBridge enables full outbound networking through the host's
	// default docker bridge. Maps to `docker run --network=bridge`.
	// Domain-allowlist enforcement is not yet implemented; the rule's
	// constraint is informational only at this stage.
	NetworkBridge NetworkMode = "bridge"
)

// Request describes a single subprocess invocation. Validation (binary
// allowlist, argv parsing, workspace_root, env allowlist) happens in the
// tool layer; the backend only runs what it is given.
//
// WorkspaceRoot and Image are backend-specific: the local backend ignores
// both; container backends bind-mount WorkspaceRoot at a fixed path inside
// the container and require Image to know what to run. NetworkMode is also
// container-only.
//
// HostAllowlist narrows egress when NetworkMode == NetworkBridge: the
// docker backend pre-resolves each hostname on the host, injects
// --add-host pins for the resolved IPs, and points the container's
// resolver at an unroutable address so DNS lookups for anything outside
// the list fail. Threat model: prevents accidental egress to unintended
// hosts; not hard isolation against malicious code that hardcodes IPs.
// eBPF cgroup_skb is the next pass for that.
type Request struct {
	Argv          []string
	WorkDir       string
	WorkspaceRoot string
	Image         string
	Env           []string
	Timeout       time.Duration
	NetworkMode   NetworkMode
	HostAllowlist []string
}

// Result reports how the process ended. ExitCode is -1 when the process
// never started or was killed by the deadline.
type Result struct {
	Output   []byte
	ExitCode int
	TimedOut bool
	OOM      bool
}

// ErrNotStarted indicates the backend never managed to spawn the process.
// Distinct from a non-zero exit so callers can surface a clearer message.
var ErrNotStarted = errors.New("executor: backend failed to start process")

// Backend executes a subprocess request. Implementations must be safe for
// concurrent use; each Run call is independent.
type Backend interface {
	Name() string
	Run(ctx context.Context, req Request) (*Result, error)
}

// Registry resolves a backend by name. The runtime owns one Registry and
// looks up the backend per-step based on the assistant's ExecutorBackend
// field. Empty or unknown names fall back to DefaultBackend.
type Registry struct {
	mu       sync.RWMutex
	backends map[string]Backend
}

// NewRegistry returns an empty registry. Callers must Register at least the
// default backend before calling Resolve.
func NewRegistry() *Registry {
	return &Registry{backends: make(map[string]Backend)}
}

// Register adds a backend by its Name(). Re-registering the same name
// replaces the previous entry.
func (r *Registry) Register(b Backend) {
	if b == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backends[b.Name()] = b
}

// Resolve returns the backend for name, or the default backend when name is
// empty or unknown. Returns nil only when even the default isn't registered.
func (r *Registry) Resolve(name string) Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if name == "" {
		name = DefaultBackend
	}
	if b, ok := r.backends[name]; ok {
		return b
	}
	return r.backends[DefaultBackend]
}

// Names returns the sorted list of registered backend names. Used by the
// settings API to populate the assistant builder dropdown.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.backends))
	for name := range r.backends {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
