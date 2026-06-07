# ADR 0004 — Mnemos Integration as a Plugin

- **Status:** Accepted (revised)
- **Date:** 2026-05-21 (proposed), 2026-05-22 (accepted), 2026-05-22 (revised after upstream discovery)
- **Authors:** Felix Geelhaar
- **Relates to:** ADR 0001 (Plugin Architecture) — Mnemos integration follows the plugin contract; this ADR adds nothing new to the runtime's core abstractions.
- **Supersedes:** The original 2026-05-22 acceptance of this ADR proposed an internal `mnemos.Client` interface + `EmbeddedClient` extraction, premised on Mnemos being a flat key-value memory store that did not yet exist. That premise was wrong (see Context); this revision rewrites the decision around Mnemos's actual shape.

## Context

Real Mnemos already exists at `go.klarlabs.de/mnemos`, with a stable HTTP/gRPC service surface and a Go client SDK at `go.klarlabs.de/mnemos/client`. The data model is a **knowledge graph**, not a key-value memory store:

- `Events` — raw knowledge events with `RunID`, `Content`, `Metadata`, `IngestedAt`.
- `Claims` — extracted assertions with `Type` (fact/hypothesis/decision/test_result), `Confidence`, `Status` (active/contested/resolved/deprecated), `Visibility` (personal/team/org).
- `EvidenceLinks` — ties a claim to its source events.
- `Relationships` — directed edges between claims (`supports`/`contradicts`).
- `Embeddings` — vector index for semantic recall.

Public API surface (mirrored in `client/client.go`):

```go
c.Events().Append(ctx, events)
c.Claims().Type("decision").Limit(25).List(ctx)
c.Claims().Append(ctx, claims, evidence)
c.Relationships().Type("contradicts").List(ctx)
c.Embeddings().Append(ctx, embs)
```

Auth is bearer-token; reads are open, writes require a token. The server is a long-running HTTP service (`mnemos serve`), backed by SQL (sqlc, pgx, libsql, sqlite). It is **its own runtime**, not a library that embeds into Nomi.

The original ADR 0004 (2026-05-22) misread Mnemos as a yet-to-be-built embedded SQLite library matching the shape of Nomi's `internal/memory` package. That premise produced an `mnemos.Client` interface that does not match the real upstream and an extraction plan that landed in a fake local repository. Both have been reverted (Nomi commits `2f72e34`, `7967e77`).

What remains true after the correction:

- Nomi's runtime still wants persistent context across runs.
- Nomi's product narrative still mentions persistent memory + optional cognitive layer.
- Mnemos is the right upstream for **organizational cognition** (claims, evidence, contradictions across teams).
- Mnemos is not the right shape for **per-assistant scratch memory** (run goal + step outputs, mined into planner hints).

These are two distinct needs.

## Decision

Treat Mnemos as a **plugin under ADR 0001**, not a runtime subsystem. Keep Nomi's existing `internal/memory.Manager` (SQLite-backed) for per-assistant scratch memory. Add a future Mnemos plugin that exposes the upstream HTTP client as capability-gated tools + a context source, for runtimes whose users opt in.

### 1. Layering

```
┌──────────────────────────────────────────────┐
│  Nomi runtime (nomid)                        │
│  ─ internal/memory.Manager   (local SQLite)  │  always present
│  ─ Plugins                                   │
│       ├─ telegram                            │  shipping
│       ├─ gmail / calendar / …                │  roadmap
│       └─ mnemos                              │  this ADR
└──────────────────────────────────────────────┘
                  │
                  ▼ HTTP (bearer auth)
┌──────────────────────────────────────────────┐
│  Mnemos service (go.klarlabs.de/mnemos,      │
│    `mnemos serve`)                           │
│  ─ Events / Claims / Relationships / Embeds  │
│  ─ Visibility: personal | team | org         │
└──────────────────────────────────────────────┘
```

Two memory layers, deliberately separate:

- **Local scratch** (`internal/memory.Manager`): every run, per assistant. Workspace/profile/preferences scope. Cheap, fast, no network. Already shipping.
- **Mnemos plugin**: structured knowledge — decisions, contradictions, evidence — surfaced when the user opts in by installing the plugin and providing a server URL + token. Optional.

### 2. Mnemos plugin contract (per ADR 0001)

The plugin's `PluginManifest` declares:

- **No channel role.** Mnemos doesn't carry conversational threads; it's a knowledge store.
- **No trigger role.** Inbound events come from agents writing claims, not external pings.
- **Tools** (the entire integration):
  - `mnemos.events.append` — capability `mnemos.write`, append one or more `Event`s to the registry.
  - `mnemos.claims.append` — capability `mnemos.write`, append claims with evidence links.
  - `mnemos.claims.list` — capability `mnemos.read`, query claims filtered by type/visibility/limit.
  - `mnemos.relationships.list` — capability `mnemos.read`, list edges between claims.
  - `mnemos.embeddings.append` — capability `mnemos.write`, push vectors for semantic recall.
- **Context source** (optional): pulls the N most recent claims relevant to the run's goal into the planner context. Subject to capability `mnemos.read`.

Capabilities split read vs write so a user can grant an assistant read-only access to the organization's knowledge graph without letting it write back.

### 3. Configuration

The plugin's `Connection` shape (one per Mnemos backend the user wants to address):

```yaml
mnemos:
  - id: company-mnemos
    base_url: https://mnemos.example.com
    token_ref: secret://mnemos/company  # via secrets.Store
    visibility_default: team             # personal | team | org
```

Multiple connections supported (e.g. personal + company instances). Assistants pick one via the existing channel-config mechanism that already binds connector instances to assistants.

### 4. What stays in Nomi

- `internal/memory.Manager` and the SQLite `memory` table — unchanged. Per-assistant scratch memory.
- The current runtime call sites (`lifecycle.go:257`, `engine.go:650+662`, `planner.go:105`) — unchanged. They write/read the local store.
- The existing REST `/memory/*` CRUD endpoints — unchanged.
- The runtime's hash-chained audit log — unchanged.

### 5. What lives in the Mnemos plugin

- A thin wrapper around `go.klarlabs.de/mnemos/client.Client` per connection.
- Tool implementations that translate Nomi tool-call input → Mnemos client calls → Nomi tool-call output. Capability strings declared in the manifest.
- A context-source implementation for the optional planner-context retrieval.
- Connection persistence via the existing `ConnectorConfigRepository` (Nomi's plugin-config layer).

### 6. Audit chain

Writes through the plugin emit two events:

- Local: `tool.executed` with `tool=mnemos.events.append` (or whichever) — already happens through the runtime's tool-execution audit path.
- Remote: Mnemos's own audit chain (the upstream service's responsibility, not Nomi's).

This is the same posture as every other plugin. Mnemos doesn't get special audit treatment because it's not special — it's a third-party service Nomi calls.

### 7. Migration from the reverted approach

The Nomi commits that built around the misread Mnemos shape have already been reverted:

- `2f72e34` — reverts REST CRUD migration.
- `7967e77` — reverts external mnemos consumption + `go.mod` replace + step-1 extraction.

The tree is back to commit `ac42baf` state: `internal/mnemos` package + `internal/memory/EmbeddedClient` exist inside Nomi as a Nomi-internal abstraction. Those are still useful (typed memory boundary inside the runtime) so they stay.

The fake mnemos repo at `/Users/felixgeelhaar/Developer/projects/open_source/mnemos` is deleted.

No upstream changes were made to the real Mnemos repository (it lives at `projects/business-felix-geelhaar/mnemos`). Mnemos is unchanged by this ADR.

## Consequences

### Positive

- Architectural shape matches reality. No fake repositories, no premature interface extraction.
- Mnemos integration is opt-in via the plugin system, matching how telegram / gmail / calendar / github will land.
- Capability split (read vs write) gives users fine control without inventing new gating primitives.
- Nomi's local memory and Mnemos's organizational knowledge are clean concerns: scratch vs structured. No double-store divergence.
- `internal/memory` keeps its current shape; no breaking changes to the runtime or to the REST surface.
- The Mnemos OSS narrative stays credible — Mnemos is its own product, consumed via its own client library.

### Negative

- The "memory-native runtime" narrative in the landing page over-promised: Mnemos is not the runtime's spine, it's an optional plugin. Landing-page copy needs softening (see follow-up work).
- The previous ADR's "two implementations (embedded + remote)" framing is gone. Anyone reading old commit messages or the prior ADR text needs the correction note (provided at the top).
- The cross-runtime memory motivation (desktop + headless sharing state) doesn't get solved by this ADR. Two `nomid` instances pointing at the same Mnemos URL would share *claims*, not scratch memory. That's a different problem; out of scope here.

### Neutral

- `internal/mnemos` package + `EmbeddedClient` inside Nomi remain as a typed boundary. Whether they survive long-term is a separate code-review question, not an ADR decision.
- The plugin doesn't ship in this ADR — only the design does. A separate roady feature tracks the implementation.

## Alternatives considered

### A. Extract Nomi's `internal/memory` as a public library and call it Mnemos

The original (now-superseded) decision. Rejected because real Mnemos already exists and owns that name with a different shape. Forking the name would cause confusion and clash on `go.mod` import paths.

### B. Replace `internal/memory` with Mnemos entirely

Considered briefly. Rejected because Mnemos is a structured-knowledge store; every per-run output written as a `Claim` is wrong (low-signal noise drowns the high-value decisions; visibility model doesn't fit scratch data). Local memory and structured cognition are different problems.

### C. Build a Nomi-side SQLite library that mirrors Mnemos's shape

Pure duplication. Rejected.

## Open questions

- **Plugin authoring**: the Mnemos plugin lives in Nomi's `internal/plugins/mnemos/` or as a separate `.nomi-plugin` WASM bundle? Both are valid under ADR 0001; the decision depends on whether Mnemos is "first-party" enough to ship in-tree.
- **Embedding tool**: `mnemos.embeddings.append` is straightforward, but vector retrieval (`mnemos.embeddings.similar`?) needs the upstream client to expose it. Track separately.
- **Visibility enforcement on read**: when a plugin tool returns claims, who decides what visibility the assistant is allowed to see? Likely a per-connection setting (`max_visibility`).
