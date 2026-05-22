# ADR 0004 — Nomi ↔ Mnemos Cognitive Boundary

- **Status:** Accepted
- **Date:** 2026-05-21 (proposed), 2026-05-22 (accepted)
- **Authors:** Felix Geelhaar
- **Relates to:** ADR 0001 (Plugin Architecture) — Mnemos is *not* a plugin; it's a first-class runtime subsystem.

## Context

Today `internal/memory` is wired as if Mnemos were "a feature of Nomi": a `memory.Manager` over a `MemoryRepository` (SQLite `memory` table, see `internal/storage/db/migrations/000001_init_schema.up.sql:74`). The runtime calls `r.memManager.Save(...)` directly at run completion (`internal/runtime/lifecycle.go:257`); there is no capability gate, no tool, no client interface. The schema is private to Nomi, the data lives in Nomi's SQLite, the access path is internal.

The product narrative shifted on 2026-05-21 (see hero rewrite in `docs/index.html` + `README.md`):

> Nomi is a **personal AI runtime**; Mnemos is the **cognitive layer**. The runtime executes; the cognitive layer remembers and reasons across time.

Two concrete pressures force the boundary question now, plus one anticipated one:

1. **(Concrete) Organizational cognition — Phase 4 roadmap.** Teams want shared workflows, shared institutional memory, shared semantic context. That requires Mnemos to be addressable as a service, with auth, scoping, and policy enforcement that is *not* Nomi-specific.
2. **(Concrete) Mnemos as a standalone OSS library.** The repo at `github.com/felixgeelhaar/mnemos` is already drawn as an independent project on the landing page ("a real database for your agent's context — not a vector blob"). It cannot credibly claim independence if its only consumer is Nomi via internal APIs.
3. **(Anticipated, not yet observed)** Users with desktop Nomi + headless `nomid` wanting shared memory across both. No issue or user request to point to; included because the architecture would unlock it cheaply if (1) and (2) are addressed.

The current "memory is an internal package" shape blocks (1) and (2).

## Decision

Treat **Mnemos as a separable cognitive service** with a defined wire contract, accessed by Nomi through a thin client. Nomi keeps the runtime concerns (execution, capability gating, plan/step state, approvals). Mnemos owns memory concerns (storage, retrieval, scoping, semantic operations).

### 1. Layer boundary

```
┌──────────────────────────────────────────────┐
│  Nomi runtime (nomid)                        │
│  ─ Run / Plan / Step state machines          │
│  ─ Tool registry + capability engine         │
│  ─ Permissions / approvals                   │
│  ─ Connectors + plugins                      │
│  ─ Event bus + audit chain                   │
│              │                                │
│              ▼ mnemos.Client                  │
└──────────────┼───────────────────────────────┘
               │  defined wire protocol
┌──────────────▼───────────────────────────────┐
│  Mnemos cognitive layer                      │
│  ─ Scoped storage (workspace, profile, org)  │
│  ─ Structured retrieval + semantic search    │
│  ─ Memory policy execution (summarize, ...)  │
│  ─ Schema versioning + migrations            │
└──────────────────────────────────────────────┘
```

Nomi **never reaches around the client**: no direct SQLite reads of memory tables, no schema knowledge in the runtime. The current `internal/memory` package becomes the in-process implementation of `mnemos.Client` — same logic, behind an interface.

### 2. Wire contract

`mnemos.Client` is a Go interface that defines exactly what the runtime needs:

```go
type Client interface {
    Store(ctx context.Context, scope Scope, entry Entry) error
    Retrieve(ctx context.Context, scope Scope, query Query) ([]Entry, error)
    Search(ctx context.Context, scope Scope, q string, opts SearchOpts) ([]Entry, error)
    Forget(ctx context.Context, scope Scope, id string) error
    Tombstone(ctx context.Context, ref EntityRef) error // see §6 cleanup
}
```

Vector retrieval, summarization, and lineage operations are deferred to a follow-up ADR — they are not required to extract the boundary.

Two implementations ship together in the Mnemos repo:

- **`mnemos/embedded`** — in-process, SQLite-backed. What Nomi uses today; zero behavior change for the laptop user.
- **`mnemos/remote`** — HTTP + bearer-token client against a Mnemos service. Same interface, network on the other side. Enables Phase 4 organizational deployments.

Nomi picks the implementation at boot from `app_settings.memory_backend` (`embedded` | `remote://<url>`).

### 3. Scope model

Replace today's flat `"profile" | "workspace"` string with a tuple:

```
Scope = (owner_id, kind, key)
  owner_id — user or org identity. In embedded mode, single hardcoded "local"
  kind     — workspace | profile | session | org
  key      — workspace path, session ID, org slug
```

**Honest about isolation:**

- **Embedded mode**: scope enforcement is *advisory*. The `Client` interface is an in-process Go call; a buggy or malicious runtime call could pass any `Scope` and the embedded implementation would honor it. Acceptable because the runtime is the trust boundary anyway — same process, same disk, same secrets. Mnemos `embedded` enforces scope at the **query layer** (every SQL touches `WHERE owner_id = ? AND kind = ? AND key = ?`), which catches accidental mis-routing but is not a security control.
- **Remote mode**: scope enforcement is *authoritative*. The bearer token is bound to an `owner_id` server-side; the server rejects requests whose declared `Scope.owner_id` doesn't match the token. Same posture as any other REST API with row-level auth.

This is the correct symmetry: in-process = trust; cross-process = verify. The ADR is honest that embedded mode does not add a security boundary over today's posture.

### 4. What stays in Nomi

- `MemoryPolicy` on `AssistantDefinition` (which scopes an assistant attaches to, whether to remember after each run, summary template) — this is *policy*, not memory.
- The decision to call `Store` at run completion. Runtime stays the trigger.
- The mapping from a `Run` to the `Scope` it operates in.

### 5. What moves to Mnemos

- The `MemoryEntry` struct + JSON serialization.
- The SQLite schema for memory tables (extracted from `internal/storage/db/migrations/000001_init_schema.up.sql` into Mnemos's own migration set).
- Search, ranking, and any future semantic operations (vector retrieval, summarization, lineage).
- Memory schema versioning — Mnemos has its own migration story, independent of Nomi's.

### 6. Database split — own DB, event-driven cleanup

**Decision: Mnemos owns `mnemos.db`, separate file from `nomi.db`.**

Today the memory table has `ON DELETE SET NULL` FKs to `assistants(id)` and `runs(id)`. Splitting into separate SQLite files breaks those FKs — SQLite cannot enforce FKs across attached databases reliably, and the remote case can't share a DB connection at all.

Cost of the split:

- Memory rows can outlive their referenced assistant/run.
- No automatic cleanup on assistant deletion.

Mitigation: Nomi's existing event bus emits `assistant.deleted` and `run.deleted` events. Mnemos subscribes (in embedded mode via direct subscription; in remote mode via a webhook or periodic tombstone sync) and processes `Client.Tombstone(ref)` calls. Orphan sweep runs hourly as a safety net. The event bus is already audit-chained, so the cleanup record stays inspectable.

Alternative rejected: single shared `nomi.db` with Mnemos owning a namespace of tables. Works for embedded mode, breaks the symmetry with remote mode entirely, and re-introduces the two-migrator-one-file problem when both repos want to evolve schemas independently.

### 7. Memory policy execution — where the LLM call lives

Today `MemoryPolicy.SummaryTemplate` is **declared but unused**: `lifecycle.go:244-249` just concatenates step titles + outputs into a plain string. No LLM call.

When LLM-based summarization lands (follow-up feature), it goes in Mnemos, not Nomi. Reasoning:

- Mnemos has the data and the retrieval primitives.
- Keeps Nomi runtime as pure orchestration; runtime calls `Client.Store(entry)` and Mnemos decides whether/how to summarize before persisting.
- Avoids a second LLM client in the runtime — Mnemos service in remote mode is the natural home for its own model selection.
- `SummaryTemplate` becomes input to Mnemos, not a Nomi-side instruction.

For embedded mode this means Mnemos needs an LLM client too; it can use the same provider-profile infrastructure (passed in via dependency injection) or be configured separately. Decision deferred until summarization lands.

### 8. Migration path

Three steps, with honest reversibility:

1. **Define `mnemos.Client` interface in Nomi.** Refactor `internal/memory` so the runtime depends on the interface, not concrete types. Add `Tombstone` plumbing on assistant/run delete events. Behavior unchanged.
   - **Reversible**: yes — pure interface introduction.
2. **Extract `mnemos/embedded` to the Mnemos repo with its own SQLite file.** Nomi imports it as a Go module. Memory data migrates from `nomi.db` `memory` table → `mnemos.db` on first boot post-upgrade. One-shot export + import script ships with the migration.
   - **Reversible**: only via data export. Once migrated, rolling back requires re-importing rows into `nomi.db` and reverting the Nomi version. A `nomid memory export` command must exist before this step ships.
3. **Ship `mnemos/remote`.** New `nomid` setting `memory_backend: remote://...`. Auth via bearer token in OS keyring.
   - **Reversible**: yes by flipping config; data divergence between embedded and remote stores is the user's problem to resolve (see §10).

### 9. Cross-repo dependency management

Nomi imports `github.com/felixgeelhaar/mnemos/embedded` as a Go module. This introduces:

- **Version pinning**: Nomi pins a specific Mnemos semver. Bumps via PR like any other dep.
- **Interface stability contract**: `mnemos.Client` follows semver — additions are minor, signature changes are major. Mnemos owners agree (in writing in the Mnemos README) not to break the interface on a minor bump.
- **Coordinated releases**: when both repos change together (e.g. interface addition + Nomi consumer), Mnemos releases first with the new method as optional, Nomi releases consuming it. Same pattern any Go ecosystem uses.

No cyclic dependencies: Mnemos must not import any Nomi types. `MemoryEntry` is defined in Mnemos and re-exported by Nomi if needed.

### 10. Embedded → remote data migration

User flips `memory_backend` from `embedded` to `remote://...`. Default behavior: **do not auto-migrate**. The remote endpoint may already contain organizational data the user doesn't want clobbered.

Explicit commands ship with Nomi:

```
nomid memory export --scope workspace > workspace.jsonl
nomid memory import --backend remote --scope workspace < workspace.jsonl
```

Export format is the Mnemos JSONL wire format; same file works between any two implementations. UI exposes the same two operations under Settings → Memory → Migrate.

### 11. Out of scope for this ADR

- **Vector retrieval / embedding store.** Mnemos today is keyword/substring (case-insensitive scan). Vector is on the spec roadmap. Follow-up ADR will decide whether it lives behind the same `Client` interface or as a `mnemos.VectorClient` extension.
- **Multi-tenant remote Mnemos.** Phase 4 implies multiple users hitting one Mnemos. Auth, rate limiting, quota, billing — all blocked on this ADR but not solved by it. Track as a separate Mnemos-side ADR before Phase 4 ships.
- **Memory tool with capability gate.** Today there is no `memory.read` / `memory.write` tool; the runtime calls memory directly. Whether to expose memory access to plugins via a capability-gated tool is a follow-up; the answer affects ADR 0001's plugin contract but does not block this boundary work.

## Consequences

### Positive

- The Mnemos OSS narrative becomes credible — wire contract, two implementations, clear surface area.
- Organizational cognition (Phase 4) has a concrete shape rather than an aspirational paragraph.
- Memory schema evolves on Mnemos's release cadence, not Nomi's.
- Easier to mock memory in Nomi tests — the interface is small.
- Cross-runtime memory sharing (#3 in Context) is unlocked as a side-effect, even though it wasn't the driver.

### Negative

- **Audit-chain break in remote mode.** Today `/audit/verify` covers Nomi's hash-chained event log. When memory ops happen remotely, the chain stops at the network boundary. The Nomi value-prop "reasoning is replayable" weakens unless Mnemos extends the chain.
  - **Mitigation**: every remote `Client.Store/Forget/Tombstone` call emits a Nomi-side audit event with a signed reference (`mnemos_op_id`, `content_hash`) before the network call returns. Verification: Nomi's chain proves intent; Mnemos's own chain (which Mnemos must implement before remote ships) proves persistence. Cross-verify by comparing hashes. Track this as a Mnemos-side requirement.
- **Performance budget.** Embedded retrieve is microseconds (single SQLite query). Remote retrieve is tens of ms (network round-trip + auth + query). Planner that consults memory N times per step inflates by N×latency. Budget: remote retrieve P95 ≤ 50ms over LAN, ≤ 200ms over WAN. If planner exceeds budget, Nomi caches per-run memory snapshots at run start and reads from the snapshot for the rest of the run.
- **Failure-mode behavior.** Embedded: memory failure is local DB corruption, propagates as error today. Remote: memory unavailable (network, auth expired, service down) is a new failure mode. **Policy**: degrade gracefully — log the failure, publish a `memory.unavailable` event, and continue the run without memory. The user sees a warning surface in the UI; the run does not fail. Memory is augmentation, not load-bearing for correctness. This policy is testable.
- **Encryption posture is unchanged for embedded, new problem for remote.** Today `nomi.db` and (post-extraction) `mnemos.db` are plaintext SQLite. Confidentiality relies on whatever filesystem encryption the user has (FileVault on macOS opt-in, dm-crypt on Linux opt-in, BitLocker on Windows). Nomi does not change this. Remote Mnemos cannot rely on the user's disk encryption — it has to encrypt at rest server-side (TBD; default to whatever Mnemos service deployment provides — operator responsibility, documented but not enforced by Nomi).
- **A new abstraction layer.** In embedded mode this is effectively a no-op, but it's still a layer.
- **Step 2 is data-migration-reversible only.** Once `mnemos.db` is canonical, rolling back requires re-import. A pre-ship requirement: `nomid memory export` must exist and be tested as part of step 1's acceptance.

### Neutral

- The current `internal/memory` package effectively becomes `mnemos/embedded` upstream. Most code moves verbatim.
- Plugins still don't see memory directly — they go through the runtime, which goes through `mnemos.Client`. If a future plugin needs memory access, it goes through a capability-gated tool (deferred, §11).
- FK loss on the `memory` table is real but bounded — orphan rows accumulate at the rate of assistant/run deletions, and the tombstone path keeps them sweep-able.

## Acceptance criteria

This ADR is **accepted** when all of the following are observable:

1. `internal/memory` depends only on `mnemos.Client` interface; no other Nomi package imports concrete memory types.
2. `mnemos.Client` interface is defined in the Mnemos repo and re-exported / vendored by Nomi. Nomi tests use a fake implementation, not the SQLite one.
3. `nomid memory export` command exists, writes Mnemos JSONL, round-trips through `nomid memory import` with zero data loss (test: full export → wipe DB → import → compare).
4. `assistant.deleted` and `run.deleted` events drive Mnemos tombstone calls; integration test asserts orphaned memory rows are gone within one sweep cycle.
5. Audit log includes a `memory.op` event for every Store/Forget/Tombstone with `content_hash` populated.
6. Performance test: remote-mode `Retrieve` P95 under 50ms over loopback (no network); planner consults memory ≤ once per step or operates on a cached snapshot.
7. Failure-mode test: remote Mnemos unreachable → run still completes, `memory.unavailable` event published, UI shows a warning, no error propagates to the user's goal.

## Alternatives considered

### A. Keep memory internal to Nomi; expose a REST endpoint when needed

Cheaper short-term. Fails the Mnemos-as-standalone-library claim and ties Mnemos's evolution to Nomi's release cycle. Rejected.

### B. Pull memory out as a sibling daemon today

Cleanest end-state but enormous up-front cost. The wire contract isn't yet load-bearing, and there's no second consumer in production. Rejected for now; revisit when organizational cognition has design pressure.

### C. Treat Mnemos as a plugin under ADR 0001

Conceptually tempting (everything-is-a-plugin), but Mnemos is *not* peripheral. It's load-bearing for the cognitive-layer narrative and called on every run with memory enabled. Forcing it through the plugin manifest path adds ceremony without isolation benefit. Rejected.

### D. Shared `nomi.db`, Mnemos owns a table namespace

Works for embedded, breaks symmetry with remote, re-introduces two-migrator-one-file. Rejected; see §6.
