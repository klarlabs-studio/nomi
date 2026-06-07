package runtime

import (
	"sync"
	"time"
)

// tokenBucket is a minimal, concurrency-safe token-bucket limiter. It accrues
// tokens at `ratePerSec` up to `burst`, and Allow() consumes one token per
// call. Returning false tells the caller to refuse / throttle the operation.
type tokenBucket struct {
	mu         sync.Mutex
	tokens     float64
	burst      float64
	ratePerSec float64
	lastRefill time.Time
}

func newTokenBucket(perMinute, burst int) *tokenBucket {
	if perMinute <= 0 {
		perMinute = 1
	}
	if burst <= 0 {
		burst = 1
	}
	return &tokenBucket{
		tokens:     float64(burst),
		burst:      float64(burst),
		ratePerSec: float64(perMinute) / 60.0,
		lastRefill: time.Now(),
	}
}

func (b *tokenBucket) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(b.lastRefill).Seconds()
	b.tokens += elapsed * b.ratePerSec
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.lastRefill = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// rateLimiter holds per-source, per-run, and per-connection token buckets.
// It's safe for concurrent use; entries are created lazily on first access.
//
// The per-connection budget (ADR 0001 §7) is a defense-in-depth control:
// if one Connection's credentials leak or a plugin misbehaves, the blast
// radius for "how many tool calls can run through that connection per
// minute" is bounded even when the per-run budget would otherwise allow
// more.
type rateLimiter struct {
	mu                           sync.Mutex
	runsPerMinuteSource          map[string]*tokenBucket
	toolCallsPerMinuteRun        map[string]*tokenBucket
	toolCallsPerMinuteConnection map[string]*tokenBucket
	runsPerMinute                int
	runsBurst                    int
	toolCallsPerMinute           int
	toolCallsBurst               int
	connectionToolCallsPerMinute int
	connectionToolCallsBurst     int
}

func newRateLimiter(runsPerMinute, runsBurst, toolCallsPerMinute, toolCallsBurst int) *rateLimiter {
	return &rateLimiter{
		runsPerMinuteSource:          make(map[string]*tokenBucket),
		toolCallsPerMinuteRun:        make(map[string]*tokenBucket),
		toolCallsPerMinuteConnection: make(map[string]*tokenBucket),
		runsPerMinute:                runsPerMinute,
		runsBurst:                    runsBurst,
		toolCallsPerMinute:           toolCallsPerMinute,
		toolCallsBurst:               toolCallsBurst,
		// Per-connection defaults mirror the per-run defaults; tighter
		// per-connection limits can be surfaced via a config knob once
		// there's user demand.
		connectionToolCallsPerMinute: toolCallsPerMinute,
		connectionToolCallsBurst:     toolCallsBurst,
	}
}

// AllowRun checks the per-source run-creation budget. Sources without a
// declared limit (e.g. the local desktop UI if the operator chose to opt
// them out) can be handled by skipping the call; all connector sources go
// through this path.
func (rl *rateLimiter) AllowRun(source string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.runsPerMinuteSource[source]
	if !ok {
		b = newTokenBucket(rl.runsPerMinute, rl.runsBurst)
		rl.runsPerMinuteSource[source] = b
	}
	return b.Allow()
}

// AllowToolCall checks the per-run tool-call budget. Runs stuck in an
// infinite agent loop will saturate their bucket and be denied further tool
// executions until the rate catches up, which surfaces as a StepFailed with
// a "rate limited" error rather than a daemon meltdown.
func (rl *rateLimiter) AllowToolCall(runID string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.toolCallsPerMinuteRun[runID]
	if !ok {
		b = newTokenBucket(rl.toolCallsPerMinute, rl.toolCallsBurst)
		rl.toolCallsPerMinuteRun[runID] = b
	}
	return b.Allow()
}

// ForgetRun drops the per-run bucket for a terminated run so memory doesn't
// grow unbounded with historical runs.
func (rl *rateLimiter) ForgetRun(runID string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	delete(rl.toolCallsPerMinuteRun, runID)
}

// AllowConnectionToolCall checks the per-Connection tool-call budget.
// connectionID == "" opts out (system tools that don't route through a
// plugin connection); non-empty IDs consume a token from a per-Connection
// bucket that persists for the process lifetime.
func (rl *rateLimiter) AllowConnectionToolCall(connectionID string) bool {
	if connectionID == "" {
		return true
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.toolCallsPerMinuteConnection[connectionID]
	if !ok {
		b = newTokenBucket(rl.connectionToolCallsPerMinute, rl.connectionToolCallsBurst)
		rl.toolCallsPerMinuteConnection[connectionID] = b
	}
	return b.Allow()
}

// ForgetConnection drops the per-Connection bucket. Called when a
// Connection is deleted so memory is released promptly.
func (rl *rateLimiter) ForgetConnection(connectionID string) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	delete(rl.toolCallsPerMinuteConnection, connectionID)
}
