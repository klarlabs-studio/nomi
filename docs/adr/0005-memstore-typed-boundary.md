# ADR 0005 — Keep `internal/memstore` as the typed memory boundary

- **Status:** Accepted
- **Date:** 2026-05-22
- **Relates:** ADR 0004 (revised — Mnemos is a plugin), roady #118
- **Supersedes:** —

## Context

`internal/memstore` (renamed from `internal/mnemos` in commit `0d9b414`) was
introduced in `ac42baf` as a typed interface between the runtime and the
underlying `*db.MemoryRepository`. The original motivation was step 1 of the
since-revised ADR 0004 migration path: extract a Mnemos package so a future
embedded SQLite implementation and a future remote HTTP implementation could
both implement the same interface.

ADR 0004 was revised on `2026-05-22` to treat Mnemos as a plugin
(`internal/plugins/mnemos`), not a runtime subsystem. The extraction path
was abandoned. `memstore.Client` is therefore an interface with one
implementation (`internal/memory.EmbeddedClient`) and no planned second.

Question: keep, simplify, or revert?

## Decision

**Keep `internal/memstore` as-is, with documentation tightened to reflect
its real purpose.**

The audit-chain wiring + typed `Scope` tuple + `Tombstone` plumbing +
export/import format that landed alongside the interface in `ac42baf` are
load-bearing. The interface no longer points at a second implementation,
but it remains a useful internal boundary:

- **Typed `Scope`** (`owner_id`, `kind`, `key`) replaces the flat `string`
  scope `*memory.Manager` previously used. Every runtime call site
  (`internal/runtime/lifecycle.go`, `planner.go`, the tombstone subscriber)
  consumes the typed shape. Reverting forces those call sites back to
  stringly-typed scope, eroding type safety on the hot path.
- **`memory.*` audit events** + content hashing are emitted from inside
  `EmbeddedClient.Store/Forget/Tombstone`. Reverting drops the audit trail
  for memory ops, breaking `/audit/verify` coverage for memory rows.
- **`memory.Export` / `memory.Import` JSONL wire format** is built on the
  interface and is the migration path for any future remote-Mnemos cutover.
- **Entity-deletion tombstone** wiring (`assistant.deleted`, `run.deleted`)
  routes through `Client.Tombstone`; it replaces the previous
  `ON DELETE SET NULL` FK and is what makes the Mnemos plugin's
  separate-store extraction tractable later if we ever do it.

The cost of keeping the interface is one extra type in the import graph
and a small docstring obligation explaining "no second implementation
planned." The cost of reverting is real audit + type-safety regressions
for ~400 LOC of code-shape symmetry. Trade goes the other way.

## Consequences

### Positive

- Audit chain stays intact for memory ops.
- Runtime call sites continue to consume `memstore.Scope`, not flat
  strings.
- Export/import format stays load-bearing for any future Mnemos
  migration.
- `EmbeddedClient` continues to be the single implementation; no second
  is needed to justify the boundary.

### Negative

- One more type in the import graph than strictly necessary.
- New contributors may briefly wonder why the interface exists when only
  one implementation exists. The package-level docstring is updated to
  answer this inline.

### Not done

- No code changes. The decision is preservation.
- `internal/memory.Manager` continues to serve REST CRUD endpoints; the
  divergence-risk noted in roady #118's "Against" column is accepted as
  a documentation obligation rather than a refactor.

## Out of scope

- Merging `*memory.Manager` and `memstore.Client` (the prior Option B).
  Defer until a concrete reason emerges; both serve distinct callers
  (Manager → REST CRUD, EmbeddedClient → runtime) and the divergence
  cost is theoretical rather than observed.
- Removing `*memory.Manager` and porting REST CRUD to `memstore.Client`
  (the prior Option C). Same reasoning — defer until concretely needed.
