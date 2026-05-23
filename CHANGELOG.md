# Changelog

All notable changes to Nomi are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/) and
[Semantic Versioning](https://semver.org/).

## [Unreleased] - eBPF egress filter: docker systemd cgroup driver

Closes the last deferred from the V1 polish closeout. Until now the
eBPF egress filter only worked under docker's cgroupfs driver — the
filter created `/sys/fs/cgroup/nomi-sandbox-<id>` and passed the
absolute path to `--cgroup-parent`, which docker's systemd driver
rejects (it requires a slice name ending in .slice). On hosts using
the systemd driver the filter would either fail outright at docker
run time or fall back to the DNS-only path even when CAP_BPF was
present.

### Changed
- `internal/runtime/executor/egress/egress.go`:
  - New `Driver` type with `DriverCgroupfs` + `DriverSystemd`
    constants. `Config.DockerCgroupDriver` selects the cgroup-naming
    scheme.
  - `Filter` interface grows `DockerCgroupParent() string` separate
    from `CgroupPath()`. The former is what docker wants on the
    command line; the latter is the filesystem path the BPF program
    is attached to (now distinct on systemd hosts).
- `internal/runtime/executor/egress/egress_linux.go`:
  - Branches on `cfg.DockerCgroupDriver`. cgroupfs → flat dir
    `nomi-sandbox-<id>`, docker gets the absolute path. systemd →
    `nomi-sandbox-<id>.slice`, docker gets the bare slice name. The
    cgroup_skb attachment lives on the parent in both cases; child
    scopes docker creates inherit the program.
- `internal/runtime/executor/docker.go`:
  - `DockerBackend` grows `driverOnce` + `cachedDriver`. Methods
    converted to pointer receivers so the cached state actually
    persists (sync.Once on a value receiver is a no-op).
  - `resolveCgroupDriver` probes via `docker info --format
    '{{.CgroupDriver}}'` exactly once per backend lifetime and
    feeds the result into `egress.New`. Detection failure falls
    back to cgroupfs (the historical behaviour, safer than
    bricking the eBPF path entirely).

### Tests
- `TestDockerRunUsesSystemdSliceForSystemdDriver` stubs the
  detector + filter to assert: (a) driver value reaches
  `egress.New`, (b) docker's `--cgroup-parent` carries the bare
  slice name, (c) the absolute path doesn't leak into argv on
  systemd hosts.
- `TestDockerResolveCgroupDriverCaches` proves the `sync.Once`
  memoisation — 5 calls = 1 detector invocation.

### Operational
- IPv6 deferred is closed too; both v4 and v6 enforcement work
  under either cgroup driver.
- Detection still needs docker installed + reachable. Hosts without
  docker fall back to cgroupfs (default), which is harmless: if the
  eBPF path is attempted but the daemon's actually using systemd
  underneath, docker will reject the cgroup-parent and the egress
  filter soft-falls back to DNS-only with the existing warning.

## [Unreleased] - eBPF egress filter: IPv6 enforcement

Closes the v0.2.x deferred IPv6 gap on the cgroup_skb/egress BPF
program. Before: v6 packets passed through unfiltered because the
program only branched on ETH_P_IP. After: a second HASH map (16-byte
key) holds allowlisted v6 destinations; the program branches on
ctx.protocol to ETH_P_IPV6 too, loads the 16-byte daddr from offset
24 of the IPv6 header via bpf_skb_load_bytes, looks it up, drops on
miss.

### Changed
- `internal/runtime/executor/egress/egress_linux.go`:
  - `New` provisions two HASH maps now (`nomi_egress_v4`,
    `nomi_egress_v6`). Both get closed/torn down in `Close`.
  - `buildProgram` grows two parallel branches off the protocol
    switch — `v4_load → v4_lookup` and `v6_load → v6_lookup`. A
    single 16-byte stack slot at RFP-16 holds the destination for
    either family; the v4 path writes only the bottom 4 bytes.
    Non-IP packets (ARP, ICMPv6 link-local control) still pass —
    the threat model is application egress, not L3 hardening.
  - `AddIP` routes v4 to the v4 map and v6 (incl. v4-mapped v6
    via `net.IP.To4`) to the v6 map. Malformed `net.IP` inputs
    now error rather than silently succeeding.

### Tests
- Existing Linux integration test (`TestNewLinuxAttachesAndCloses`)
  now exercises a v4-mapped v6 path. New `TestAddIPRejectsGarbage`
  guards the malformed-IP rejection branch — a security primitive
  silently accepting garbage is the wrong default.

### Operational notes
- IPv6 enforcement is automatic when the rule's `host_allowlist`
  resolves to AAAA records. No new env vars or config.
- The systemd-cgroup-driver path translation is still deferred —
  same as before, `--cgroup-parent` assumes cgroupfs driver.

## [Unreleased] - DiffPreview: Shiki per-hunk highlighting

Closes the last deferred item from the V1 polish wave. DiffPreview
used to call HighlightedCode per source line, which meant Shiki
re-tokenised every line from scratch — losing multi-line scope
(template literals, JSX, block comments, multi-line string literals
in Go) at every line boundary. Now one Shiki call per hunk.

### Changed
- `app/src/lib/highlighter.ts` — new `highlightLines(code, lang)`
  helper that runs Shiki once on the whole block, strips the
  `<span class="line">` wrappers Shiki emits, and returns one HTML
  string per source line for the caller to render with their own
  per-line chrome (markers, gutters, bg tints).
- `app/src/components/diff-preview.tsx` — `useHunkHighlight` hook
  memoises one tokenisation per (lines, lang) pair. `HunkBody`
  passes the resulting per-line tokens to `HighlightedDiffHunk`.
  `HighlightedDiffHunk` no longer owns its own per-line Shiki
  calls — it just lays out the +/- gutter + bg tint over the
  token HTML and falls back to plain content when the tokens
  array isn't available (Shiki still loading, lang not bundled,
  Shiki layout change that breaks the per-line split).

### Behaviour
- Same visual API as before; pure correctness/perf win.
- N independent Shiki promises per render → 1 per hunk.
- Multi-line tokens stay intact across the hunk so a template
  literal opening on line 1 and closing on line 4 is highlighted
  as a single string instead of four mis-tokenised fragments.

### Tests
- `app/src/lib/__tests__/highlighter.test.ts` covers the lang
  normalisation table, file-extension sniffing, and the null-lang
  short-circuit. The Shiki dynamic import is exercised by the
  Playwright e2e — mocking it at the unit layer was brittle and
  the value was negligible.

## [Unreleased] - eBPF cgroup_skb egress filter (Linux, experimental)

Hard kernel-level egress isolation for the docker backend, layered on
top of the existing DNS allowlist. A cgroup_skb/egress BPF program is
loaded via `github.com/cilium/ebpf` (pure-Go assembly, no clang at
build time), attached to a per-run cgroup, and consults a HASH map of
allowlisted IPv4 destinations. Packets whose destination isn't in the
map are dropped at the kernel — closing the hardcoded-IP gap the
DNS-only path leaves open.

### Added
- `internal/runtime/executor/egress` package. `Filter` interface +
  `New(Config) (Filter, error)`. Linux build (`egress_linux.go`)
  creates a cgroup under `/sys/fs/cgroup`, builds the BPF program in
  pure-Go assembly, populates a HASH map for `__sk_buff` destination
  lookups, and attaches via `link.AttachCgroup`. The `!linux` stub
  always returns `ErrUnsupported`.
- Docker backend integration: when `NOMI_EGRESS_EBPF=1` and an
  allowlist is set, the backend creates the filter, populates IPs
  from the shared allowlist resolution, sets `--cgroup-parent`, and
  defers `Close()` to detach + clean up. Any error (no kernel
  support, missing CAP_BPF/CAP_NET_ADMIN, missing cgroup v2) is a
  soft failure that logs a structured warning and falls back to the
  DNS-only path.
- Tests: stub-platform `TestNewReturnsUnsupportedOffLinux`;
  Linux-only `TestNewLinuxAttachesAndCloses` (skipped when not root
  or no cgroup v2); docker-backend integration tests stub
  `newEgressFilter` to verify map population, cgroup-parent
  injection, soft fallback on `ErrUnsupported`, and that the filter
  is never created when the env gate is off.

### Threat-model update
- DNS allowlist alone: prevents accidental egress; bypassable by
  hardcoded IPs. With eBPF: kernel drops the packet regardless of
  how the destination was discovered. IPv6 enforcement is not yet
  implemented — IPv6 packets pass through (documented in the BPF
  program comment).

### Operational notes
- Off by default. Set `NOMI_EGRESS_EBPF=1` on the daemon process to
  opt in. Daemon must run with `CAP_BPF + CAP_NET_ADMIN` and a
  cgroup v2 mount at `/sys/fs/cgroup`.
- macOS and Windows hosts always fall back to the DNS-only path
  because the kernel hook doesn't exist there.
- Docker must use the cgroupfs driver (systemd-driver `.slice`
  naming isn't supported by the v1 cgroup path scheme).

## [Unreleased] - DNS egress allowlist (docker)

Tightens the `network.egress` capability beyond the present
`--network=none` / `--network=bridge` flip. An Allow rule on
network.egress that carries a `host_allowlist` constraint now causes
the docker backend to pre-resolve each host on the host's DNS, inject
`--add-host=name:ip` for every resolved IP, and steer in-container DNS
at `127.255.255.255` so lookups for anything outside the list time
out. NetworkMode is forced to `bridge` when an allowlist is present
so the pinned hosts are actually reachable.

### Added
- `executor.Request.HostAllowlist []string`. Runtime extracts the
  `host_allowlist` constraint from the matched network.egress rule
  (via `permissions.Engine.MatchingRule`) and propagates it through
  the reserved `__host_allowlist` tool-input key, matching the
  existing `__sandbox` / `__network_mode` escape-hatch pattern.
- Docker backend: `--add-host` pinning + `--dns=127.255.255.255` for
  every allowlisted host, with unresolvable hosts surfaced as a
  startup error (silently dropping would let the assistant reach
  unintended endpoints if upstream DNS later recovers).
- `host_allowlist` field on the permission rule editor in the
  assistant builder, gated to `network.egress` + Allow.

### Threat-model note
- Prevents accidental egress to unintended hosts. Not a hard isolation
  against malicious code that hardcodes IPs — that requires kernel-
  level packet filtering (eBPF cgroup_skb), which is the next pass on
  the roady backlog.

## [Unreleased] - Scout browser plugin

First-party browser automation. Nomi connects to a Scout MCP server
(stdio subprocess or HTTP+SSE) via `github.com/felixgeelhaar/mcp-go`
and exposes six browser primitives through the existing tool +
capability + plan-review surface.

### Added
- `internal/plugins/scout` — Plugin + ToolProvider +
  ConnectionHealthReporter. Lazy client construction (first tool
  call wakes the subprocess), cached per connection so the stdio
  process doesn't restart per invocation. Stop() closes every
  cached client.
- Six tools, each gated by capability `scout.browse`:
  `scout.navigate`, `scout.observe`, `scout.click`, `scout.type`,
  `scout.screenshot`, `scout.extract`. They map to the upstream
  Scout MCP server's `navigate` / `annotated_screenshot` / `click`
  / `type` / `screenshot` / `extract` tools.
- Connection config schema covers both transports:
  - `transport=stdio` (default): `command` (default `scout`),
    `args` (default `mcp`, comma- or space-separated).
  - `transport=http`: `endpoint` + optional `token` credential
    (Bearer auth) resolved through the secrets store.

### Behaviour notes
- Connection isn't established at boot — a missing or broken
  `scout` binary doesn't fail `nomid` startup. The first tool
  invocation surfaces the connection error.
- Reserved input keys (`connection_id`, anything `__*`) are
  stripped before forwarding to the Scout server.
- Tool output flattens the MCP ContentItem array into `text` +
  optional `image_data` for screenshot endpoints; raw structured
  content stays available under the `content` key.
- The any-MCP-server generic plugin (not just Scout) remains on
  the roady backlog — the client glue here is reusable.

## [0.2.2] - 2026-05-23 — Polish wave

Five user-visible improvements on top of v0.2.1. Each entry below
landed on `main` before the cut; section headings match the
individual change descriptions further down.

### Highlights
- Recipe catalog expands from 3 to 9 built-in entries
  (inbox-triage, release-notes-drafter, meeting-summarizer,
  content-creator, codebase-explorer, daily-standup join the
  existing three).
- FTS5 memory search with bm25 ranking replaces the LIKE scan;
  relevance wins over recency. LIKE fallback for unmigrated DBs.
- OS notifications for pending approvals via
  `tauri-plugin-notification` (+ Web Notification API fallback).
  Closes the "agents that ask before they act" loop async — user
  doesn't need the window focused.
- Shiki syntax highlighting in chat code blocks + diff hunks. Lazy
  ~700KB bundle, idle-warmed; falls back cleanly when offline /
  language unbundled.
- `NOMI_EVAL_LIVE` provider matrix — run the planner golden corpus
  against real LLM providers (Ollama / OpenAI / Anthropic) with
  per-provider thresholds.

---

## [Unreleased] - NOMI_EVAL_LIVE provider matrix

Existing `make eval-live` runs the planner golden corpus + adversarial
fixtures against a deterministic httptest LLM fake — the regression
gate for parser / validator / routing behaviour. This adds a parallel
target that runs the same corpus against real providers (Ollama,
OpenAI, Anthropic) and emits per-provider pass-rate numbers, so
prompt regressions in real models surface as a metric rather than as
an anecdote.

### Added
- `internal/runtime/evals/planner_live_test.go` —
  `TestPlannerGoldenSet_Live`. Iterates known providers, runs the
  goldenCases against each, and prints
  `planner golden pass rate [provider=… model=…]: P/N = R%` per
  provider. Skips silently when no provider envs are configured —
  contributors run only the providers they have credentials for.
- Tolerance: real LLMs legitimately produce slightly different plan
  shapes from the fake. Step count is matched within ±1; only the
  first step's tool routing has to match exactly. The aggregate
  pass rate is the gate, not individual fixtures.
- `Makefile` target `eval-live-providers` with documented env
  variables and per-provider threshold overrides
  (`NOMI_GOLDEN_THRESHOLD_OLLAMA`, `NOMI_GOLDEN_THRESHOLD_OPENAI`,
  `NOMI_GOLDEN_THRESHOLD_ANTHROPIC`).

### Observed (informational, not gated)
- qwen2.5:14b on Ollama: 4-5/5 across two runs (~80-100% pass rate).
  Single-digit-percent variance per fixture between runs is expected
  with real models.

## [Unreleased] - Shiki syntax highlighting

Chat fenced code blocks and diff hunks now render through Shiki with
the GitHub light/dark theme pair. Lazy-loaded singleton so the
~700KB grammar bundle stays out of the initial Vite chunk; warms on
idle so the first render doesn't pay the cold-start cost. Languages
outside the bundle fall through to the previous plain-text rendering,
no UX regression.

### Added
- `app/src/lib/highlighter.ts` — Shiki core wrapped behind a single
  promise + grammar resolver covering go / typescript / tsx /
  javascript / jsx / python / rust / bash / shell / json / yaml /
  markdown / sql / diff.
- `app/src/components/highlighted-code.tsx` — async-aware component
  that swaps in the Shiki-rendered HTML when ready, shows a styled
  `<pre>` fallback while loading or for un-bundled languages.
- `MarkdownMessage` code-block override routes fenced blocks through
  `HighlightedCode` using the `language-*` class react-markdown
  attaches.
- `DiffPreview` HunkBody refactored to render each hunk line as
  marker + Shiki-highlighted content; line tinting (add/remove)
  composes with syntax tokens rather than fighting them.

## [Unreleased] - OS notifications for pending approvals

Closes the "agents that ask before they act" loop on the user side.
When an agent pauses for approval, Nomi now fires an OS notification
(macOS Notification Center, Linux org.freedesktop, Windows toast)
so the user can respond without keeping the Nomi window focused.

### Added
- `tauri-plugin-notification` 2.x wired in `app/src-tauri/src/main.rs`,
  capability declared in `capabilities/default.json`, JS binding via
  `@tauri-apps/plugin-notification`.
- `app/src/lib/notifications.ts` — notification helper. Tauri path
  via the plugin; non-Tauri fallback (vite preview / Playwright /
  Scout) via the Notification Web API. Lazy permission request on
  the first approval event so a fresh tab doesn't pop a permission
  prompt before any agent activity.
- Event-provider hook on `approval.requested` events — fires the
  notification asynchronously so a slow OS permission dialog doesn't
  block other event-driven invalidations.
- New "Approval notifications" card in the Safety settings tab with
  a toggle backed by `localStorage` (`nomi.notifications.disabled`).

### Behavior notes
- Toggle defaults to enabled. Permission state is OS-managed; users
  who deny at the OS prompt can re-enable via system settings.
- Notification click focuses the Nomi window (best-effort in browser
  fallback; no-op in headless).

## [Unreleased] - FTS5 memory search

`MemoryRepository.Search` switches from the substring `LIKE` scan to
SQLite FTS5 ranked by bm25. The most relevant entries surface first
instead of the most recent. Falls back to the previous LIKE path on
any FTS5 error so an unmigrated DB or a future driver build without
FTS5 compiled in stays operational.

### Added
- Migration #31 — `memory_fts` FTS5 virtual table over `memory.content`
  with porter + unicode61 tokenizer, plus INSERT/UPDATE/DELETE
  triggers that keep it in sync inside the same transaction as the
  underlying mutation. Backfills from existing memory rows on first
  boot post-upgrade; idempotent (skips IDs already in the FTS table).
- `ftsQuery` helper that wraps every whitespace-separated token in
  double quotes and joins with implicit AND, so user input with
  hyphens, slashes, or operator-like words (`AND`, `OR`, `NOT`)
  doesn't trip the FTS5 parser.

### Behavior notes
- Search ranking uses `bm25(memory_fts)`; ties broken by FTS internal
  order. No explicit `created_at` desc anymore — relevance wins.
- LIKE fallback preserves the prior behaviour so older DBs without
  the FTS migration still work after a binary upgrade.

## [Unreleased] - Recipe catalog expansion

Built-in catalog grows from 3 to 9 entries, covering the common
prosumer + small-team workflows the "reviewable agents" wedge
serves. Each new Recipe ships with a curated capability set scoped
to the minimum needed (most read-only, one or two scoped writes
behind `confirm` mode). No code changes — the existing Recipe
registry + install/export flow consumes them automatically.

### Added (6 new built-in recipes)
- `inbox-triage` — read-only email summariser. Reads a folder of
  exports, groups by sender + topic, surfaces the 3-5 messages that
  need a reply. Never sends.
- `release-notes-drafter` — turns `git log` between two refs into
  end-user-readable release notes. Drops merge commits + behaviour-
  neutral dep bumps; categorises feat / fix / docs / breaking.
- `meeting-summarizer` — reads transcript files, produces TL;DR +
  decisions + action items (owner / due date) + open questions.
  Output is markdown for direct paste.
- `content-creator` — drafts long-form content from a notes folder,
  matching voice from existing writing. Includes an outline check
  + `[citation needed]` placeholders for unsupported claims.
- `codebase-explorer` — read-only repo walker for first-day
  onboarding. Cites file paths + line numbers; admits unknowns
  instead of hallucinating.
- `daily-standup` — reads yesterday's commits + optional todo file,
  drafts yesterday / today / blockers. Pair with the Schedules tab
  for an async-standup loop.

## [0.2.1] - 2026-05-23 — Patch: chat markdown + mobile image fix

Two paper-cuts caught post-v0.2.0:

### Fixed
- **LLM responses now render as markdown** in chat bubbles. Streaming
  buffer + final response both go through `react-markdown` +
  `remark-gfm` with `skipHtml` (no raw HTML pass-through, so a
  prompt-injected response can't break out of the bubble). Headings
  render compactly, fenced code blocks scroll horizontally, tables
  get a min-width wrapper, links open in a new tab with
  `noopener noreferrer`.
- **Mobile image skew on the website.** `.step img` carried
  `max-width: 100%` but no `height: auto`, so explicit
  `width`/`height` attributes broke aspect ratio on narrow viewports.
  Added `height: auto` to `.step img` plus a global `img` safety net
  so future images can't regress.
- **Browser-only UI sessions** — short-circuit `getAuthToken` +
  `getApiBase` when `__TAURI_INTERNALS__` is absent, plus a
  `?nomi_dev_token=…` / `#nomi_dev_token=…` URL bootstrap in
  `app/index.html`. Vite preview / Playwright / Scout can now drive
  the UI without the desktop shell.
- **`/memory` default scope** — `ListMemory` with no `?scope` filter
  now returns workspace + profile + **preferences**, so the
  Memory tab's Preferences subtab correctly shows learned and
  manual entries on the default fetch.

### Dependencies
- Added `react-markdown` and `remark-gfm`.

## [0.2.0] - 2026-05-23 — "Reviewable agents"

Major feature release. Repositions Nomi from "personal AI runtime" to
**reviewable agents** — a desktop agent platform that always shows you
the plan before it runs and learns with your approval. Every entry
below is shipped, tested, and visible in the UI. Migrations land
automatically on first boot of `nomid` 0.2.0.

### Headline features

- **Sandboxed executor backends** — pick `local`, `docker`
  (rootless, --network=none, capped memory + CPU + PIDs, --init), or
  `gvisor` (runsc — user-space kernel) per assistant from a UI
  dropdown. Boot-time probe; only registered backends appear.
- **Recipe registry + sharing** — versioned, SHA-256-signed YAML
  bundles. Built-in catalog (coding-agent, research-assistant,
  ops-runbook), one-click install, export-as-recipe button on every
  assistant.
- **Scheduled runs + NL cron** — `POST /schedules/translate` turns
  "every weekday at 8am" into a cron expression via the configured
  LLM, gated by the same cron parser the ticker uses. Background
  scheduler fires Runs on cadence through the existing audit + plan
  review surface.
- **Skill induction with LLM synthesis** — heuristic Jaccard
  clustering OR embedding-based cosine clustering (cosine ≥ 0.78) over
  past successful Runs surfaces candidate Recipes; "Generate with AI"
  uses the LLM to propose `system_prompt` + capability set from the
  cluster; user reviews + promotes.
- **Auto-learning loop** — `RunCompleted` event subscriber asks the
  LLM for short preference statements ("Run tests before
  committing"), writes them to `LocalPreferences` memory the planner
  already reads on every new Run. Closed loop, fully reviewable in
  Memory tab.
- **Embeddings provider integration** — `llm.EmbeddingClient`
  interface + OpenAI-compat impl. Provider profile gains
  `embedding_model_id` (migration #30); resolver builds embedding
  clients on demand.
- **WhatsApp Cloud API plugin** — closes connector parity vs
  Hermes/OpenClaw. Now four first-party channels: Telegram + Slack +
  Discord + WhatsApp.
- **Plugin ContextSource consumption in planner** — bindings with
  `role=context_source` now feed the planner prompt under a
  `plugin_context` trust tag; Mnemos's claims context source +
  Obsidian's vault context source actually fire instead of sitting
  dormant.
- **Enum ConfigField type** — plugin manifests can declare
  fixed-choice config fields; UI renders a `<select>` dropdown.
- **Desktop UI surfaces** — new "Schedules" tab, "Recipes" tab,
  "Sandbox" section in assistant editor, "Suggested skills" panel in
  Recipes (with Generate with AI), "Auto-learned" vs "Manual" split
  in Memory's Preferences subtab.
- **Browser-only UI sessions** — dev-token URL bootstrap in
  `app/index.html` + Tauri-bridge short-circuit in `api.ts` so vite
  preview / Playwright / Scout can drive the UI without the desktop
  shell.

### Operational

- Hash-chained audit log + `/audit/verify` walks the chain; reasoning
  is replayable.
- Prometheus per-executor-backend metrics
  (`nomi_executor_runs_total{backend,outcome}`,
  `nomi_executor_duration_seconds{backend}`,
  `nomi_executor_oom_total{backend}`).
- `network.egress` capability gates outbound from container backends.
- All previous Mnemos lineage work closed; ADR 0005 records the
  decision to keep `internal/memstore` as the typed memory boundary
  even with one implementation.

### Roady (planning) state at release

255/255 tasks done in the project's Roady plan; e2e Mnemos workflow
verified green against `mnemos serve` v0.15.3 in GitHub Actions.

## [Unreleased] - 2026-05-22 (Embeddings + auto-learning loop)

Closes the "self-learning" gap vs Hermes while preserving the
reviewable-agents wedge. Three coupled changes:

1. **Embedding client** — `llm.EmbeddingClient` interface +
   OpenAI-compat implementation (works against any /v1/embeddings
   endpoint: OpenAI, Ollama, Together, vLLM, LM Studio).
   `ProviderProfile.EmbeddingModelID` carries the per-provider model
   selection (migration #30). Anthropic skipped — no native
   embeddings endpoint.

2. **Embedding-backed skill clustering** — `skills.Induce` consumes
   an optional `EmbeddingClient` via `Config.EmbeddingClient`. When
   present, cosine-similarity clustering replaces Jaccard tokens;
   clusters get sorted by average pairwise cosine so the most
   cohesive surface first. Threshold tunable (default 0.78).
   Embedding errors fall back to the heuristic path silently — the
   clustering is a quality lever, not a load-bearing dependency.

3. **Auto-extracted user preferences** — new `internal/learning`
   package subscribes to `RunCompleted` events. On a successful run
   above MinRunDuration, asks the default LLM for ≤3 short
   preference statements ("Run tests before committing", "Prefer
   yarn over npm"), validates them (length cap, dedup, JSON parse),
   and writes them to `memstore.LocalPreferences` under the
   assistant's scope. The planner already reads that surface —
   future runs reflect the inferred preference without any extra
   wiring.

### Added
- `internal/llm/embeddings.go` — interface + OpenAI-compat impl with
  Authorization header + 30s default timeout + auth-error
  surfacing.
- `Resolver.DefaultEmbeddingClient()` — builds an embedding client
  from the default provider's endpoint when an embedding model is
  configured; nil otherwise (graceful degrade).
- Migration #30 — `embedding_model_id` column on
  `provider_profiles`.
- `domain.ProviderProfile.EmbeddingModelID` + API request/response
  fields + TS types updated.
- `internal/skills/embedding_cluster.go` — cosine-similarity
  clustering, normalised dot product, embedding centroid, cluster
  ordering by average pairwise cosine.
- `internal/learning/preferences.go` — RunCompleted subscriber that
  extracts preferences via LLM and stores them in
  `memstore.LocalPreferences`.

### Behavior notes
- The loop closes: planner already reads `LocalPreferences` at plan
  time and annotates plans with "Why: Based on your preference for
  …". Auto-extracted prefs now feed that read path automatically.
- Reviewable-agents wedge preserved: every learned preference lands
  in Mnemos memory and is visible/deletable by the user — same
  surface that exists today.
- Capability allowlist + size cap + LLM JSON-mode guard against
  prompt-injected "remember everything I say" goals.

## [Unreleased] - 2026-05-22 (Mnemos lineage polish — context-source wiring, enum config, memstore ADR, comparison refresh)

Closes the Mnemos-lineage backlog: ContextSource plumbing now actually
fires at plan time, plugin manifests carry typed enum config fields,
the memstore boundary has a written-down decision, and the comparison
table reflects the features that landed this session.

### Added
- `runtime.PluginContextResolver` callback type + `SetPluginContextResolver`
  hook on `Runtime`. The lifecycle layer invokes it after folder context
  is loaded, wraps the result in a `plugin_context` trust-tagged block,
  and splices it into the planner prompt. Wired in `cmd/nomid/main.go`:
  walks each assistant's connection bindings, picks ones with
  `role=context_source`, matches them to the right `ContextSource` on
  the plugin registry, and concatenates the rendered blocks. Mnemos's
  `claimsContextSource` (and Obsidian's `vaultContextSource`) now feed
  the planner instead of sitting dormant. Closes roady #119.
- `plugins.ConfigOption` + `Type: "enum"` on `plugins.ConfigField` —
  manifests can declare fixed-choice fields with labeled values.
  Mnemos `visibility_default` and WhatsApp `first_contact_policy`
  migrated to enums; the desktop plugin dialog now renders a `<select>`
  for them, eliminating the silent-typo failure mode. Closes roady #120.
- ADR 0005 — written-down decision to keep `internal/memstore` as the
  typed memory boundary. The audit-chain + Tombstone + typed-Scope
  value pays for the one-implementation overhead. Closes roady #118.

### Changed
- `docs/comparison.md` — Nomi vs OpenClaw/NanoClaw/Hermes/Pi table
  refreshed with sandboxed-exec, scheduled-runs, recipe-registry, and
  skill-induction rows. Connector list updated to reflect Telegram +
  Slack + Discord + WhatsApp shipped this session.
- `internal/memstore/doc.go` rewritten to reflect ADR 0004's revision
  (Mnemos-as-plugin) + ADR 0005's keep-the-boundary decision.

## [Unreleased] - 2026-05-22 (LLM-driven skill synthesis)

Augments the heuristic skill induction pipeline with an LLM-driven
synthesis step. Given a cluster of similar past Runs, the configured
LLM provider proposes a Recipe shape: suggested name, role, reusable
system prompt extracted from the cluster's goal patterns, and the
minimum capability set the assistant needs. Output goes through a
strict JSON envelope with a closed allowlist for capabilities.

### Added
- `skills.Synthesize(ctx, client, suggestion, sourceGoals)` — runs the
  LLM call with a deterministic system prompt + JSONMode, parses the
  envelope leniently (strips code fences), filters proposed
  capabilities against the runtime's closed allowlist, and falls back
  to safe read-only defaults if the model returns a zero-capability
  list.
- `POST /skills/synthesize` REST endpoint — re-runs induction to
  locate the cluster, loads source-run goals from the state store,
  and returns the synthesized recipe. Returns 503 when no LLM
  provider is configured (graceful-degrade pattern).
- `skillsApi.synthesize(suggestionID)` TS client method.
- "Generate with AI" button in the skill suggestions panel — runs
  synthesis on demand, expands an inline preview showing the
  proposed role + capabilities + collapsible system-prompt, and
  pre-fills the promote form's name field with the suggestion. The
  synthesized fields are threaded through `/skills/promote` so the
  resulting Recipe + Assistant land with the LLM's authored prompt +
  capabilities, not the source-assistant copy or the read-only
  default.
- `promoteRequest.synthesized_*` fields override source-assistant
  defaults when present; the order of precedence is explicit
  (synthesis > source-assistant copy > read-only default).

### Scope notes
- Capability allowlist is closed: filesystem.read/write, command.exec,
  network.egress, llm.chat. Hallucinations outside that set are
  silently dropped.
- Clustering stays heuristic (Jaccard); embedding-based clustering
  plugs into the same Synthesize path and is the next pluggable
  extension.

## [Unreleased] - 2026-05-22 (Desktop UI surfaces — schedules, skills, recipe export)

Ships the deferred Tauri front-end surfaces for three backend features
that landed without UI in the prior cycle. No new backend work; all
three components consume the existing TypeScript API client.

### Added
- `SchedulesManager` (new "Schedules" tab, System section) —
  natural-language phrase input wired to `/schedules/translate` with
  a parsed-cron confirmation step before save. Active schedules list
  with enable toggle, last-fire / next-fire times, last-error
  surfacing, and delete button.
- Skill suggestions panel inside the `RecipesManager` Recipes tab —
  on-demand "Scan history" action that calls `/skills/suggestions`,
  with one-click promote that hits `/skills/promote` and refreshes
  both the recipe list and the suggestions panel.
- "Export as recipe" button on the assistant editor footer (visible
  only when editing an existing assistant) — calls `/recipes/export`
  and opens an inline YAML viewer with Copy + Close actions.

## [Unreleased] - 2026-05-22 (Natural-language cron translation)

Closes the last flagship gap vs Hermes Agent: schedules can now be
created from a natural-language phrase ("every weekday at 8am", "first
Monday of the month") instead of requiring the user to write a cron
expression by hand. The translation goes through the configured LLM
provider with a strict JSON response shape and is gated by the
scheduler's existing cron parser before being returned to the caller.

### Added
- `scheduler.TranslateNL(ctx, client, phrase)` — two-stage translator:
  asks the LLM for a JSON envelope `{cron_expr, explanation}`, then
  runs `cron.Parse` on the result. Invalid expressions come back with
  `Valid=false` and the parser error in `Explanation` so the UI can
  ask the user to retype rather than silently persisting bad input.
- `POST /schedules/translate` — REST surface for the translator;
  returns 503 when no LLM provider is configured (matching the
  existing graceful-degrade pattern).
- `nl_phrase` column on the `schedules` table (migration #29) so the
  UI can re-display the original phrase next to the persisted cron.
  `Schedule.NLPhrase` field added to the domain model + repo CRUD.
- `schedulesApi.translate(phrase)` + a complete `schedulesApi` TS
  client (list/get/create/patch/delete + translate).

### Scope notes
- Few-shot prompt covers the common cases (daily, weekday filter,
  monthly, sub-hourly, business hours, multi-time-per-day refusal).
- Tolerant parsing strips ``` ```json fences ``` ``` an over-eager
  model may wrap the JSON envelope in even when asked not to.
- UI surface for the translate endpoint is a follow-up.

## [Unreleased] - 2026-05-22 (Skill induction from run history)

Adds skill induction (roady #126). Reads the user's past successful
runs, clusters them heuristically by Jaccard similarity over goal
tokens, and surfaces candidate Recipes the user can promote into a
real assistant + recipe via the registry.

v1 ships the scaffolding — heuristic clustering, on-demand pass,
basic promote flow. Richer variants (embeddings-based similarity,
LLM-driven prompt synthesis, parameterized slots, PII/secret scan,
periodic background job) are additive without changing the wire shape.

### Added
- `internal/skills` package — token-based goal clustering with
  configurable similarity threshold + minimum cluster size.
  Suggestion ID is a stable hash over sorted source run IDs so the UI
  can dedupe and the promote call has a reproducible reference.
- REST endpoints:
  - `GET /skills/suggestions` — runs an induction pass on demand and
    returns ranked suggestions (largest cluster first).
  - `POST /skills/promote` — materialises a suggestion as a Recipe
    (`source: induced`) and creates a fresh Assistant; supports
    copying capabilities + permission policy from a source assistant.
- `skillsApi` in the frontend client (TS types only — no UI panel
  in v1; the inline "Suggested skills" surface is a follow-up).

### Scope notes
- Heuristic only — no embeddings, no LLM prompt synthesis. Cluster
  centroid is the run whose token set has the highest summed Jaccard
  to its peers; representative goal becomes the new assistant's
  system_prompt.
- Defaults: min 3 successful runs per cluster, Jaccard ≥ 0.5,
  max 10 suggestions, scan up to 500 most-recent successful runs.

## [Unreleased] - 2026-05-22 (Recipe registry + sharing)

Adds the Recipe registry (roady #125). A Recipe is a versioned,
shareable bundle of assistant config + permission policy + executor
backend pin. Three recipes ship in the built-in catalog:
coding-agent, research-assistant, ops-runbook. Users can install any
of them as a fresh assistant with one click, or export an existing
assistant as a Recipe YAML for sharing.

### Added
- `internal/recipes` package — Recipe document, YAML parse/marshal,
  SHA-256 hashing, FromAssistant export, ToAssistantDefinition install,
  built-in catalog loader (`//go:embed builtin/*.yaml`).
- Built-in catalog: coding-agent, research-assistant, ops-runbook —
  each with a curated capability set + permission policy.
- Migration #28 + `recipes` table for imported/exported recipes.
- `db.RecipeRepository` CRUD with Upsert semantics keyed on recipe ID.
- REST endpoints:
  - `GET /recipes` — union of built-in catalog + local rows
  - `GET /recipes/:id` — full document + sha256 + source
  - `GET /recipes/:id/preview` — assistant preview for the UI's
    confirmation step
  - `POST /recipes/install` — creates an assistant from a recipe; the
    optional `expected_sha256` field pins the version a caller saw
    during preview against catalog drift
  - `POST /recipes/export` — bundles an existing assistant into a
    Recipe YAML and stores it locally tagged `source=exported`
- Desktop `Recipes` tab — browses the catalog, shows version + author
  + tags + truncated sha256, install button with confirmation prompt.

### Scope notes
- Real cryptographic signing (Ed25519) is reserved for a follow-up;
  v1 establishes integrity via SHA-256 over the canonical YAML.
- Recipes capture assistant + permission policy + executor backend.
  Tool registrations and planner-prompt overrides are tracked as
  follow-up additions to the schema without changing the wire format.

## [Unreleased] - 2026-05-22 (WhatsApp Cloud API plugin)

Adds the WhatsApp plugin (roady #123) under `com.nomi.whatsapp`.
Inbound traffic arrives through the existing `/webhooks/:plugin_id/:connection_id`
path with HMAC-SHA256 signature verification (X-Hub-Signature-256
against the Meta App Secret). Outbound replies go through the
WhatsApp Cloud API via the new `whatsapp.send_message` tool.

This closes the messaging connector parity gap vs Hermes / OpenClaw /
NanoClaw — Telegram, Slack, Discord, and WhatsApp are now all
first-class channel providers.

### Added
- `internal/plugins/whatsapp` package implementing
  `plugins.Plugin`, `plugins.WebhookReceiver`, `plugins.ToolProvider`,
  and `plugins.ConnectionHealthReporter`.
- WhatsApp Cloud API outbound client (`SendText`) at
  `graph.facebook.com/v18.0/{phone_number_id}/messages` — pinned to
  v18.0 because the body shape is stable; bumps go through a manual
  schema-diff check.
- Webhook verifier for plugin IDs containing "whatsapp" (HMAC-SHA256
  with the `sha256=` prefix, identical scheme to GitHub but a distinct
  verifier so the event-type extraction can evolve independently).
- Boot wire in `cmd/nomid/main.go` registers the plugin alongside
  Slack, Discord, Telegram.

### Scope notes
- v1 handles text messages only. Media (images, audio), interactive
  templates, and message-status callbacks (`delivered`/`read`) are
  out of scope but additive without changing the wire contract.
- Identity allowlist enforcement is via the existing
  `channel_identities` mechanism — same shape as Slack/Discord.

## [Unreleased] - 2026-05-22 (Scheduled runs)

First cut of cron-driven scheduled runs (roady #124). A Schedule fires
a Run on a recurring cadence against a chosen assistant with a fixed
prompt. Background ticker polls every 30s and triggers via the runtime
exactly as a connector source would — schedule fires show up in the
audit log with `source = "schedule"`.

### Added
- `schedules` table (migration #27) with `assistant_id` FK + index on
  `next_fire_at` (partial, enabled-only).
- `domain.Schedule` + `db.ScheduleRepository` CRUD + `DueBefore`.
- `internal/scheduler` package — background ticker, cron parsing
  (robfig/cron/v3), fire dispatch through the existing
  `runtime.CreateRunFromSource` path.
- REST endpoints: `POST/GET/PATCH/DELETE /schedules`, `GET /schedules/:id`.
- Boot wire in `cmd/nomid/main.go` starts the scheduler with a 30s
  tick interval.

### Behavior notes
- Missed-fire policy: skip. A daemon down for 8 hours doesn't unleash
  8 catch-up runs on restart — it fires once on the next tick and
  advances `next_fire_at` to the next future cron slot.
- Invalid cron expressions disable the schedule and record the cause in
  `last_error`, rather than busy-looping.
- Schedules respect the assistant's permission policy — they go
  through the same approval flow a manual run would.

### Deferred
- Natural-language cron translation (e.g. "every weekday at 8am") —
  needs an LLM call + grammar guard; ship in a follow-up.
- Tauri Settings UI surface for schedule CRUD.

## [Unreleased] - 2026-05-22 (Sandboxed executor backends — PR5: assistant editor UI)

Exposes the executor backend choice in the desktop app's assistant editor.
A new Sandbox section sits between Memory and Permissions with a backend
dropdown and (for container backends) an image input. The dropdown is
populated from the live runtime — only backends nomid successfully
probed at startup appear, so a machine without Docker doesn't see
"docker" as an option.

### Added
- `GET /runtime/executor-backends` — returns the list of registered
  backend names (`local` always, plus `docker` and/or `gvisor` when
  available).
- `runtimeApi.executorBackends()` in the frontend API client.
- `Assistant.executor_backend` + `Assistant.sandbox_image` fields on
  the TypeScript API types and the `CreateAssistantRequest` shape.
- Sandbox section in the assistant editor: backend dropdown,
  contextual help describing each backend's isolation properties,
  conditional image input (hidden for the local backend), and a hint
  pointing users at the `network.egress` permission rule.

### Changed
- `internal/api/assistants.go` — request/response shape carries
  `executor_backend` + `sandbox_image`; UpdateAssistant persists both.

## [Unreleased] - 2026-05-22 (Sandboxed executor backends — PR4: network.egress + Prometheus)

Wires container-backend network egress through the existing permission
engine and emits per-backend Prometheus counters and duration
histograms. Deny-by-default holds: a container backend without an
explicit Allow on `network.egress` runs with `--network=none`. An
Allow rule promotes the container to `--network=bridge` (full
outbound). Domain allowlist enforcement at the bridge is reserved for
a follow-up — needs a userland firewall (eBPF or DNS allowlist) and
isn't shippable through `docker run` flags alone.

### Added
- `executor.NetworkMode` (`none` | `bridge`) on `Request`. Local
  backend ignores; container backends apply via `--network=<mode>`.
- Runtime evaluates the assistant's policy for `network.egress` at
  step time and injects the mode through `__network_mode`. Allow →
  bridge; everything else (Confirm/Deny/absent) → none.
- `nomi_executor_runs_total{backend,outcome}` counter — outcomes:
  `success` | `exit_nonzero` | `oom` | `timeout` | `error`.
- `nomi_executor_duration_seconds{backend}` histogram with buckets
  spanning sub-second (local exec) through 60s (container coldstart).
- `nomi_executor_oom_total{backend}` counter.
- `runtime.instrumentedBackend` decorator wraps every registered
  backend so metric emission is uniform and the executor package
  doesn't take a metrics dependency.

### Security
- `network.egress` is the first capability gated at the backend-
  config layer rather than tool-call layer. Confirm mode is treated
  as deny here since there's no approval UX for "should this run be
  allowed network access?" outside the existing tool-approval flow.

## [Unreleased] - 2026-05-22 (Sandboxed executor backends — PR3: gVisor)

Adds the gVisor (runsc) execution backend. Reuses the Docker code path
with `--runtime=runsc` so the container traps syscalls through a
user-space kernel instead of the host kernel — a meaningful upgrade
against kernel-exploit escapes for assistants running untrusted code.

### Added
- `executor.GvisorBackend` — thin composition over `DockerBackend` with
  `Runtime = "runsc"` pinned.
- `executor.DockerBackend.Runtime` field — supports any installed docker
  runtime (runsc, kata, custom) as a future extension point.
- `cmd/nomid/main.go` boot probe for the runsc runtime via `docker info`;
  backend registered only when reachable.

## [Unreleased] - 2026-05-22 (Sandboxed executor backends — PR2: docker)

Adds the Docker execution backend. Containers are rootless on the host
side (--user is image-default for now), pinned to --network=none,
--memory/--memory-swap equal so OOMKilled fires deterministically,
--cpus and --pids-limit bounded, --init reaping zombies, and
--rm so containers don't accumulate. Workspace bind-mounts at /workspace
and the container working directory translates host paths into the
mount. Exit code 137 is heuristically classified as OOM under the pinned
memory + swap config.

### Added
- `executor.DockerBackend` implementing `Backend` via the `docker` CLI
  (no SDK dependency).
- `executor.DockerBackend.Available(ctx)` boot probe — checks `docker`
  on PATH and `docker info` reachability.
- `executor.Request.WorkspaceRoot` + `executor.Request.Image` — backend-
  specific fields that the local backend ignores and container backends
  consume.
- Migration `000026_assistant_sandbox_image` — adds `sandbox_image TEXT
  NOT NULL DEFAULT ''` to `assistants`.
- `AssistantDefinition.SandboxImage` field + repo CRUD.
- `cmd/nomid/main.go` probes Docker availability at boot with a 3s
  timeout and registers the backend when present. Absence is normal
  (logged at debug level only when present).
- Live integration test `TestDockerLiveEcho` runs against the real
  daemon when available; skipped automatically without docker.

### Changed
- `tools/command.go` now forwards `workspace_root` and the runtime-
  injected `__sandbox_image` to the backend via `executor.Request`.
- `execution.go` injects `__sandbox_image` alongside `__sandbox` when
  the assistant has a SandboxImage configured.

### Roadmap
- PR3: gVisor (runsc) backend — reuses the docker code path with
  `--runtime=runsc`.
- PR4: `network.egress` capability, restricted bridge with domain
  allowlist, Prometheus counters per backend.
- PR5: Tauri Settings UI dropdown + image input + "Test sandbox"
  probe button.

## [Unreleased] - 2026-05-22 (Sandboxed executor backends — PR1: interface + local)

Introduces a pluggable execution-backend interface so future
container-isolated runtimes (Docker, gVisor) can replace the host-exec
path without touching the tool layer. PR1 ships the interface and
extracts current host-exec behavior into a `local` backend; zero
behavior change.

### Added
- `internal/runtime/executor` package — `Backend` interface,
  `Request`/`Result` types, `Registry` keyed by backend name,
  `LocalBackend` preserving the prior `os/exec.CommandContext` +
  `Setsid`/new-process-group behavior. Backend selection per-assistant
  via the new `AssistantDefinition.ExecutorBackend` field (default
  `"local"`).
- Migration `000025_assistant_executor_backend` — adds
  `executor_backend TEXT NOT NULL DEFAULT 'local'` to `assistants`.
- `Runtime.RegisterExecutorBackend` + `Runtime.ExecutorBackends()`
  hooks for future docker/gvisor backend registration at boot.

### Changed
- `tools/command.go` now delegates the actual process spawn to the
  backend the runtime injects via a reserved `__sandbox` key on the
  tool input (matching the existing `__on_delta` escape-hatch pattern).
  Validation (argv parsing, allowed_binaries, workspace_root,
  env allowlist) stays in the tool layer. Result map now includes a
  `"backend"` field naming which backend ran the command.

### Removed
- `internal/tools/sandbox_unix.go` + `sandbox_windows.go` — the
  platform-specific `sandboxSysProcAttr` helpers move into the new
  `executor` package alongside the local backend.

### Roadmap
- Roady feature "Sandboxed executor backends" — PR1 (this change)
  lays the interface + local default. PR2 will add the docker backend
  (rootless containers, bind-mounted workspace, CPU/mem limits,
  `network.egress` capability). PR3 adds gVisor (runsc) variant.
  PR4 adds the `network.egress` permission rule and Prometheus
  counters. PR5 adds the Tauri settings UI dropdown.

## [Unreleased] - 2026-05-22 (ADR 0004 revised — Mnemos is a plugin)

Course correction after discovering that real Mnemos
(`github.com/felixgeelhaar/mnemos`) is a knowledge-graph HTTP service
with a typed Go client at `mnemos/client`, not a yet-to-be-built
embedded memory library. The earlier extraction work has been
reverted; the integration redesign treats Mnemos as a plugin under
ADR 0001.

### Reverted
- `9731023 feat(api): migrate REST CRUD memory handlers to mnemos.Client`
  → revert commit `2f72e34`.
- `ba4ea22 feat(mnemos): consume external mnemos package (ADR 0004 step 2)`
  → revert commit `7967e77`. Restores `internal/mnemos` + the prior
  `internal/memory.EmbeddedClient`, removes the `go.mod` require +
  local replace directive, restores the `events.run_id` NOT NULL +
  FK constraints, and rolls migration `000024_events_optional_run_id`
  off the schema.

### Changed
- **ADR 0004** revised in place. Status remains Accepted but the
  decision rewrites from "extract Mnemos package + introduce
  `mnemos.Client` interface" to "integrate real Mnemos as a plugin
  per ADR 0001." Two-layer architecture clarified: local
  `internal/memory.Manager` for per-assistant scratch (unchanged) +
  optional Mnemos plugin for organizational knowledge graph (claims,
  evidence, relationships, visibility scopes).
- **`docs/index.html`** — softens the Mnemos prominence: the
  "Cognitive layer" primary card becomes "Persistent memory you can
  read" with Mnemos demoted to "drops in as an optional plugin." OG
  meta description drops the "memory-native" framing. Phase 4
  roadmap card explicitly frames Mnemos as a plugin. Built-on lib
  card describes real Mnemos shape (events / claims / evidence /
  relationships) instead of the imagined embedded library.
- **`README.md`** — same softening. Mnemos referenced as an optional
  plugin for team-scale structured knowledge; per-assistant scratch
  framed as local SQLite. Architecture diagram updates to "Local
  memory" instead of "Memory → mnemos."

### Removed
- The fake local Mnemos repo at
  `/Users/felixgeelhaar/Developer/projects/open_source/mnemos`
  (created during the misread; deleted manually).

### Roadmap
- Roady feature #113 — "Mnemos Plugin (ADR 0001 + ADR 0004 revised)"
  supersedes the now-stale feature #112. Tracks the actual plugin
  implementation: tools (`mnemos.events.append`,
  `mnemos.claims.append`, `mnemos.claims.list`,
  `mnemos.relationships.list`, `mnemos.embeddings.append`,
  potentially `mnemos.embeddings.similar`), context source, capability
  split (`mnemos.read` / `mnemos.write`), connection configuration via
  the existing `ConnectorConfigRepository`.
- Feature #112 (Mnemos package extraction) stays in the spec for
  history but is inactive — superseded by #113.

## [Unreleased] - 2026-05-22 (ADR 0004 step 1 — mnemos.Client extraction)

Implements step 1 of ADR 0004's migration path: runtime depends on the
`mnemos.Client` interface instead of `*memory.Manager`. Foundation for
step 2 (extract package to standalone Mnemos repo with its own SQLite
file) and step 3 (HTTP-backed `mnemos/remote`). Zero behavior change
for the laptop user; full repo test suite (40 packages) green under
`-race`.

### Added
- **`internal/mnemos`** — wire-shaped types and interface. `Scope`
  (owner_id, kind, key) replaces flat string scope; `ScopeKind` enum
  covers workspace/profile/session/org/preferences. `Client` interface:
  Store, Retrieve, Search, Forget, Tombstone. Sentinel errors
  (`ErrNotFound`, `ErrInvalidScope`, `ErrScopeMismatch`).
  `ValidateScope` enforces kind/key well-formedness. Helpers
  `LocalWorkspace()`, `LocalProfile()`, `LocalPreferences()`.
- **`internal/memory.EmbeddedClient`** — in-process implementation over
  the existing `*db.MemoryRepository`. SHA-256 `ContentHash` computed
  on Store. Scope-isolated Forget rejects cross-scope IDs as
  `ErrNotFound`. Tombstone routes to `AnonymizeByAssistant` /
  `AnonymizeByRun`. Optional `WithEventBus(bus)` enables audit
  emission.
- **Audit events** — new `EventMemoryStore`, `EventMemoryForget`,
  `EventMemoryTombstone` event types. `EmbeddedClient` emits with
  `content_hash` populated; events feed into the existing
  hash-chained audit log via the same `EventBus.Publish` path used by
  every other domain event.
- **Tombstone wiring** — new `EventAssistantDeleted` /
  `EventRunDeleted` event types. `Runtime.DeleteRun` and
  `api.AssistantServer.DeleteAssistant` publish them; runtime
  subscribes at boot in `startTombstoneSubscriber` and routes to
  `Client.Tombstone`. `AssistantServer` constructor extended to take
  an `*events.EventBus`.
- **`AnonymizeByAssistant` / `AnonymizeByRun`** on `MemoryRepository`
  — mirrors today's `ON DELETE SET NULL` FK behavior. Idempotent.
- **JSONL export/import** — `memory.Export(ctx, client, scope, w)` and
  `memory.Import(ctx, client, r)` with versioned header
  (`mnemos.export` v1). REST: `GET /memory/export?scope=&key=` streams
  JSONL with `application/x-ndjson` + Content-Disposition; `POST
  /memory/import` returns `{"imported": N}`. CLI: `nomi memory export
  [--scope] [--key] [-o file]` and `nomi memory import file|-`.
- **Migration `000024_events_optional_run_id`** — makes `events.run_id`
  nullable and drops the FK to `runs(id)`, enabling entity-scoped
  events (assistant/run deletion + memory ops) to persist. Down
  migration aborts if NULL rows present.

### Changed
- `Runtime.memManager *memory.Manager` → `Runtime.memClient
  mnemos.Client`. Constructor signature updated. All four runtime call
  sites (`lifecycle.go:257`, `engine.go:650+662`, `planner.go:104`)
  rewritten to use Client methods. `scopeFromPolicy()` helper maps
  legacy `MemoryPolicy.Scope` strings to typed `mnemos.Scope`.
- `events.validateEvent` relaxed via `isEntityScopedEvent` allowlist —
  events targeting an entity other than a run (assistant.deleted,
  run.deleted, memory.*) may carry empty RunID.
- `cmd/nomid/main.go` constructs both `*memory.Manager` (for the REST
  CRUD endpoints that haven't migrated yet) and `*memory.EmbeddedClient`
  (for the runtime + the new export/import endpoints).
- 9 test files updated: `memory.NewManager(repo)` →
  `memory.NewEmbeddedClient(repo)`; `setupTestRuntimeWithMemory` return
  type widened; one assertion `rt.memManager.ListByScope(...)` rewritten
  as `rt.memClient.Retrieve(ctx, mnemos.LocalWorkspace(), Query{})`.

### Tests
- `internal/mnemos/client_test.go` — 9 ValidateScope cases (happy
  paths, missing owner, unknown kind, kind/key mismatch).
- `internal/memory/client_test.go` — 14 cases: ID/CreatedAt/ContentHash
  assignment, round-trip, scope isolation, query filters
  (assistant/run/since), substring search, Forget happy/unknown/
  cross-scope, Tombstone assistant/run/idempotent, invalid-scope
  reject, context-cancel reject.
- `internal/memory/exportimport_test.go` — 5 cases: round-trip,
  unknown format reject, bad version reject, invalid scope in header,
  invalid scope on export.
- `internal/memory/audit_integration_test.go` — verifies
  memory.store/forget/tombstone events fire with `content_hash`.
- `internal/api/memory_export_test.go` — REST round-trip + 400 on
  invalid scope.

### Roadmap
- Roady feature #112 (next) — extract `internal/mnemos` +
  `EmbeddedClient` to `github.com/felixgeelhaar/mnemos` standalone
  repo; introduce separate `mnemos.db` SQLite file; data-migration on
  first boot post-upgrade (ADR 0004 step 2).

## [Unreleased] - 2026-05-22 (narrative & positioning)

Repositioning pass: Nomi reframed from "local-first coding agent" to
"personal AI runtime with persistent cognition (Mnemos)." Coding stays
as the flagship workflow; the runtime + capability engine + memory
architecture is now the category claim.

### Changed
- **Landing page (`docs/index.html`)** — new hero ("Your personal AI
  runtime"), problem section reframed around stateless-and-confident
  agents, diff grid restructured to "Runtime + capability engine +
  memory" with Mnemos promoted to a primary card and a new
  "Daemon-not-IDE-plugin" card, Compared section expanded.
- **`README.md`** — hero rewritten to match; "Why Nomi" leads with the
  runtime/cognitive-layer framing; Compared-to table extended.
- **`docs/comparison.md`** — new "Personal AI assistant category"
  section comparing Nomi to OpenClaw, NanoClaw, Hermes Agent (Nous
  Research), and Pi (Inflection AI) with feature matrix + per-product
  detail subsections. Threat-model row added.

### Added
- **ADR 0004 — Nomi ↔ Mnemos Cognitive Boundary** (Accepted 2026-05-22).
  Defines `mnemos.Client` interface, embedded vs remote implementations,
  separate-DB decision with event-driven cleanup for FK loss, scope
  isolation honesty (advisory in embedded, authoritative in remote),
  three-step migration path, audit-chain mitigation, performance and
  failure-mode budgets, acceptance criteria.
- **`docs/context-budget.md`** — auto-loaded files table, model
  windows, on-demand reference sizes. Current auto-load ~43K tokens
  (~22% of 200K window). Review cadence quarterly.
- **`docs/vendor-notes.md`** — append-only gotchas log: Tauri WKWebView
  SSE workaround, Vite port mismatch (5173 vs 4173), SQLite FK pragma
  trap, golang-migrate up+down pair requirement, Playwright needs
  `nomid` running, Gin CORS posture, Ollama port detection, shadcn
  primitives are vendored.

### Roadmap
- Roady feature #110 — "Repositioning: AI Runtime + Mnemos Cognitive
  Layer Narrative" tracks remaining narrative work (OG image verify,
  Phase 4 org-cognition copy, ADR 0004 acceptance).

## [Unreleased] - 2026-05-09 (post-v0.1 cycle 2)

Two rounds of expert review (product + ai expert agents) drove this
cycle. 9 features shipped end-to-end through the roady backlog.

### Added
- **Replan-on-failure loop** — `Runtime.Replan` re-prompts the planner
  with prior step outputs + the failure as a `<previous_attempts
  trusted="false">` block, bounded by `MaxReplansPerRun`. Wired both
  automatic (executor calls before falling back to `failRun`) and
  manual (`POST /runs/:id/replan` + "Fix this with the agent" CTA on
  failed runs in chat-detail). Closes the wedge gap with Claude Code /
  Cursor / Aider, which all iterate on test failures.
- **`filesystem.patch` hardening** — diff size cap (1 MB), structured
  `--- / +++` header parsing, on-disk pre-flight (modify/delete must
  exist; create-blocks must not collide), `git apply --check` dry-run
  with `-3 --whitespace=fix` 3-way fallback. New `UserError` codes:
  `patch_file_missing`, `patch_too_large`, `patch_apply_failed` so the
  replan loop can react with appropriate retry strategy.
- **Coding Agent as onboarding default** — wizard's quickstart now
  picks `coding-agent` over `code-reviewer`. `pickOllamaModel` biases
  toward `qwen2.5-coder` / `deepseek-coder` / `codellama` when the
  template wants a coder. README quickstart pulls `qwen2.5-coder:7b`.
- **Planner-context budget envelope** — `summarizePriorAttempts` lives
  in `internal/runtime/context_window.go` with two-level budgets:
  `StepOutputBudget` (512 B per step) + `PriorAttemptsBudget` (8 KB
  total) with recency-biased truncation and an explicit `[N earlier
  step(s) elided]` breadcrumb so the planner knows context is partial.
- **Real provider labels** — `llm.Client.Provider() string` discriminates
  openai / anthropic / ollama / openai-compat from baseURL. Threaded
  through `metrics.PlannerCallsTotal` so each backend gets its own
  series.
- **`nomi_planner_edit_distance_total{provider, edit_kind}`** —
  counts step titles added vs removed when the user edits a plan; the
  leading indicator of planner quality drop.
- **Adversarial planner evals + threshold gate** —
  `internal/runtime/evals/planner_adversarial_test.go` covers
  markdown-fenced JSON, prose-wrapped JSON, unknown-tool hallucination.
  `NOMI_GOLDEN_THRESHOLD` is now an enforced gate (was dead code that
  only logged). New `make eval-live` runs golden + adversarial corpus.
- **Chat-list run search** — case-insensitive substring filter on
  `ChatItem.title` with empty-state messaging. Server-side
  `RunRepository.Search` + `GET /runs?search=<q>` available for once
  the corpus outgrows client-side filtering.
- **Branch-from-here CTA** — completed/failed runs in chat-detail
  expose a button that calls the existing fork endpoint with the last
  step ID.
- **DiffPreview parity overhaul** — structured per-file / per-hunk
  parser, per-hunk Skip / Include toggle (rebuilds the diff payload
  via `onDiffChange` callback), Unified ↔ Side-by-side toggle
  persisted to `localStorage`. Class names already match what a
  future Shiki migration will reuse.
- **HN/launch FAQ** — `docs/launch/hn-faq.md` pre-stages the first
  comment for a public launch (privacy posture, prompt-injection
  handling, replan loop, what's still rough).

### Changed
- Memory search is now case-insensitive token-aware substring match
  (`strings.Contains(strings.ToLower(...), token)` over each
  whitespace-delimited query token). Until SQLite FTS5 lands, this is
  the cheapest fix that prevents `"auth"` from missing capitalised
  rows.
- README + `docs/comparison.md` purged of stale "planner emits one
  hardcoded step" / "one-step plans for non-LLM intents" claims —
  multi-step LLM-generated plans + replan have shipped.
- ApprovalPanel is now a list-only deep-link surface; the in-context
  ApprovalCard inside chat-detail is the single source of truth for
  approve/deny. Pre-refactor the same approval was resolvable from
  two places, leading to stale state.
- ApprovalCard color tokens swapped to semantic destructive / amber+dark
  pairs so light and dark themes both pass WCAG AA contrast (no more
  `text-red-700` on `bg-red-50`). `aria-live="assertive"` reserved for
  irreversible-only; routine approvals use `polite` so screen readers
  don't get spammed.
- Runtime CAS-style transitions (`runRepo.CASUpdateStatusTx`) prevent
  two writers racing the same `RunCreated → RunPlanning` transition.

### Fixed
- Pre-existing JSX/TS errors in `assistant-manager.tsx` (Memory scope
  + Permissions sections lost during a Select-component migration).
- 26c9b7d corrupted half the form during a shadcn-Select migration;
  restored verbatim from the parent commit. CI lint now passes clean.

## [0.1.0] - 2026-05-01

First public beta. Local-first, state-driven agent platform.

### Added
- **Runtime engine** with `Run` / `Plan` / `Step` state machines
  (`pkg/statekit`) and capability-gated tool execution.
- **Permission engine** with `allow` / `confirm` / `deny` modes,
  remembered approvals, declared-capability ceiling, and TOCTOU
  re-check at the tool boundary.
- **Core tools**: `llm.chat` (streaming-capable), `filesystem.read`,
  `filesystem.write`, `filesystem.context`, `command.exec`. Per-tool
  argument schema validates planner output before persistence.
- **Plugin architecture** (ADR 0001) with cardinality-aware
  connections, multi-turn conversations, identity allowlisting, and
  WASM marketplace plugins (filesystem / command / secrets / HTTP host
  bridges).
- **Bundled plugins**: Telegram, Email (IMAP/SMTP), Slack, Discord,
  Gmail, Calendar, GitHub, Obsidian, Browser (Scout), Media (Piper TTS
  + whisper.cpp STT).
- **LLM integration**: OpenAI-compatible + Anthropic native adapters,
  per-assistant model overrides, streaming token output via SSE,
  endpoint normalisation (`/v1` auto-append), provider probe endpoint
  for pre-save validation.
- **Persistence**: SQLite with embedded migrations, per-step argument
  storage, branched runs, hash-chained event audit log, rotating
  bearer-token auth.
- **REST API + SSE event stream** on `:8080`. Tauri shell consumes
  events through native IPC bridge with auto-reconnect.
- **Desktop UI** (React 19 + Tauri 2 + shadcn/ui): chats, assistants,
  approvals, memory, events, plugins, AI providers, safety profiles,
  about. macOS menu-bar tray with idle / active / awaiting states.
  Onboarding wizard.
- **22 end-to-end user journeys** (`test/journeys/run.sh`) covering
  first-run, Q&A, file write with approval, plan editing, branching,
  pause/resume, memory persistence, late policy deny (TOCTOU close),
  endpoint hardening, audit export, provider rotation, manual
  provider/assistant CRUD, plugin enable/disable, multi-assistant
  routing, per-assistant model override, event stream consistency,
  provider probe, auth-token rotation, streaming `llm.chat`. All
  passing against real Ollama (`qwen2.5:14b`).
- **Distribution**: Homebrew Cask (`brew install --cask
  felixgeelhaar/tap/nomi`), Scoop (`scoop install nomi`), Docker
  (headless `nomid` daemon), `go install
  github.com/felixgeelhaar/nomi/cmd/nomid@latest`.

### Security
- Auth-token rotation HTTP endpoint with atomic in-memory store; old
  token invalidated immediately.
- Provider endpoint validation rejects non-http(s) schemes, missing
  hosts, and `file://` / `javascript:` / `gopher://` URLs.
- Planner argument allowlist prevents a poisoned LLM from injecting
  reserved keys (`workspace_root`, `system_prompt`, `allowed_binaries`,
  …) into tool input.
- Approval signature hashes the resolved tool arguments, not the
  natural-language step description, so a remembered "approve writes
  to notes.md" doesn't apply to a later step with `path=/etc/hosts`.
- Migration `000018` backfills `llm.chat` capability for legacy
  assistants without elevating any pre-existing `*=deny` rules.
- WASM host imports gate every call through the assistant's policy
  before forwarding to the tool executor.

## [0.1.1] - 2026-05-08

Quality and UX hardening release focused on approvals, chat ergonomics,
auth resilience, and UI consistency.

### Added
- **Approval UX hardening** with plain-English summaries, pending approval
  screen-reader announcements (`role="alert"`, `aria-live="assertive"`),
  and unified approval visual treatment between chat-inline approvals and
  the approvals panel.
- **New shadcn Select primitive** (`app/src/components/ui/select.tsx`) and
  migration of key assistant-selection/model-selection controls off native
  `<select>` to align focus/keyboard behavior with the rest of the design
  system.

### Changed
- **Chat input ergonomics**: IME-safe enter handling, multiline semantics,
  scroll-position awareness, debounced auto-scroll, and "Scroll to bottom"
  affordance for long-running conversations.
- **Plugin UI refresh behavior**: moved from aggressive 5s/10s polling to
  event-driven invalidation with 60s safety-net intervals.
- **Dependency stability**: pinned `lucide-react` to exact version `1.14.0`
  to avoid drift from unreviewed minor updates.

### Fixed
- **Final response rendering bug**: final LLM conclusion is now rendered as
  the chat bubble output instead of being obscured by the thinking block.
- **Auth cache invalidation on 401**:
  - LLM clients now surface typed auth failures (`AuthError`),
  - resolver cache invalidates on auth errors,
  - tool path invalidates provider cache on auth failures,
  - Google OAuth/Gmail paths invalidate account auth state on 401.

[0.1.1]: https://github.com/felixgeelhaar/nomi/releases/tag/v0.1.1

[0.1.0]: https://github.com/felixgeelhaar/nomi/releases/tag/v0.1.0
