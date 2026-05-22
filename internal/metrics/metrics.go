// Package metrics exposes Nomi's runtime metrics through Prometheus.
//
// Goal of this package: an operator running nomid can scrape /metrics
// behind a reverse proxy and answer "what changed when run failure
// rate spiked?" without grepping logs. The series shape mirrors the
// SPACE / DORA framings the engineering review called out — counters
// on rare events, histograms on durations.
//
// Metric naming follows Prometheus conventions: nomi_<subsystem>_<name>_<unit>.
// Buckets default to a wide range that covers Ollama-on-laptop latencies
// (slow seconds) and OpenAI streaming responses (sub-second first token).
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Registry is a project-local registry that callers register against.
// Using a custom registry (rather than prometheus.DefaultRegisterer)
// keeps tests isolated and avoids global mutation order surprises.
var Registry = prometheus.NewRegistry()

var (
	// RunsCreatedTotal counts CreateRun calls regardless of outcome.
	// Tags none: per-status counts emerge from RunsCompletedTotal.
	RunsCreatedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "nomi_runs_created_total",
		Help: "Total runs created.",
	})

	// RunsCompletedTotal counts terminal-state arrivals, partitioned
	// by status so an operator sees failed/cancelled vs completed
	// separately.
	RunsCompletedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nomi_runs_completed_total",
		Help: "Total runs that reached a terminal status.",
	}, []string{"status"})

	// RunDurationSeconds tracks end-to-end run wall-clock time.
	// Buckets cover 250 ms (cached LLM hits) up to 5 minutes (slow
	// local Ollama with multi-step plans).
	RunDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "nomi_run_duration_seconds",
		Help:    "End-to-end run duration partitioned by terminal status.",
		Buckets: []float64{0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
	}, []string{"status"})

	// StepDurationSeconds tracks individual tool execution latency.
	// Tagging by tool exposes which tool dominates a slow run.
	StepDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "nomi_step_duration_seconds",
		Help:    "Step execution duration partitioned by tool.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60},
	}, []string{"tool"})

	// StepFailedTotal counts terminal step failures partitioned by tool
	// + reason ('exec_error', 'timeout', 'permission_denied', etc).
	StepFailedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nomi_step_failed_total",
		Help: "Steps that failed, partitioned by tool and reason.",
	}, []string{"tool", "reason"})

	// StepRetryTotal counts retry-loop attempts (every retry beyond
	// the first) so flapping tools surface in dashboards.
	StepRetryTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nomi_step_retry_total",
		Help: "Step retries triggered by the retry-with-backoff loop.",
	}, []string{"tool"})

	// PlannerCallsTotal counts planner LLM calls partitioned by
	// provider (openai|anthropic|local) and outcome.
	//
	// outcome ∈ {ok, parse_fail, tool_unknown, schema_invalid, llm_error}
	// — matches the reasons askPlanner returns from validation, plus
	// 'llm_error' for non-recoverable failures.
	PlannerCallsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nomi_planner_calls_total",
		Help: "Planner LLM calls partitioned by provider type and outcome.",
	}, []string{"provider", "outcome"})

	// PlannerLatencySeconds tracks the round-trip of one planner call.
	PlannerLatencySeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "nomi_planner_latency_seconds",
		Help:    "Planner LLM call latency partitioned by provider type.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120},
	}, []string{"provider"})

	// ApprovalWaitSeconds tracks how long approval cards sit before
	// resolving. Outcome ∈ {approved, denied, expired}.
	ApprovalWaitSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "nomi_approval_wait_seconds",
		Help:    "Approval wait time from request to resolution.",
		Buckets: []float64{1, 5, 15, 60, 300, 900, 1800, 3600, 86400},
	}, []string{"outcome"})

	// PlannerEditDistance counts how many step changes a user makes
	// when they edit a planner-proposed plan. The leading indicator
	// of planner quality drop: when EditPlan starts firing more
	// often, or with higher edit counts, the model is producing
	// worse plans. Counter (not histogram) so we can see {provider}
	// rates per outcome bucket.
	//
	// edit_kind ∈ {add, remove, replace} — captured separately so a
	// dashboard can split "user added a missing step" from "user
	// removed a hallucinated step".
	PlannerEditDistance = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nomi_planner_edit_distance_total",
		Help: "Step-level edits applied during plan review, partitioned by provider and edit kind.",
	}, []string{"provider", "edit_kind"})

	// ExecutorRunsTotal counts subprocess invocations through each
	// execution backend, split by outcome. outcome ∈
	// {success, exit_nonzero, oom, timeout, error}.
	ExecutorRunsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nomi_executor_runs_total",
		Help: "Subprocess invocations grouped by execution backend and outcome.",
	}, []string{"backend", "outcome"})

	// ExecutorDurationSeconds tracks wall-clock duration per backend. Wide
	// buckets — local exec is sub-second, container backends incur cold-
	// start cost in the seconds range.
	ExecutorDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "nomi_executor_duration_seconds",
		Help:    "Wall-clock duration of a subprocess execution, by backend.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
	}, []string{"backend"})

	// ExecutorOOMTotal counts OOM-killed processes per backend. Local is
	// always zero (host OOM is opaque); container backends report based
	// on the runtime's OOMKilled signal or the exit-137 heuristic.
	ExecutorOOMTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nomi_executor_oom_total",
		Help: "OOM-killed subprocess executions by backend.",
	}, []string{"backend"})
)

func init() {
	Registry.MustRegister(
		RunsCreatedTotal,
		RunsCompletedTotal,
		RunDurationSeconds,
		StepDurationSeconds,
		StepFailedTotal,
		StepRetryTotal,
		PlannerCallsTotal,
		PlannerLatencySeconds,
		ApprovalWaitSeconds,
		PlannerEditDistance,
		ExecutorRunsTotal,
		ExecutorDurationSeconds,
		ExecutorOOMTotal,
	)
}
