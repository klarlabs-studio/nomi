//go:build !linux

package egress

// New on non-Linux platforms always returns ErrUnsupported. The docker
// backend treats that as a soft failure and falls back to the DNS-only
// allowlist path. Keeping this stub here means the rest of the runtime
// can call egress.New unconditionally without build-tagging every
// caller.
func New(_ Config) (Filter, error) {
	return nil, ErrUnsupported
}
