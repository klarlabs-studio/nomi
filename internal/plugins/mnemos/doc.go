// Package mnemos is the Nomi-side plugin that integrates the standalone
// Mnemos knowledge-graph service (go.klarlabs.de/mnemos) into
// the runtime via ADR 0001's plugin contract.
//
// Mnemos is a separate HTTP service that stores Events, Claims (fact /
// hypothesis / decision / test_result), EvidenceLinks, Relationships
// (supports / contradicts), and Embeddings. This plugin exposes a thin
// set of capability-gated tools so agents can append claims, list them
// by type, walk relationships, etc. The Mnemos server's audit, schema,
// and storage stay upstream — this plugin is a typed wire-up only.
//
// Roles played:
//   - ToolProvider: 6 tools across mnemos.read / mnemos.write capabilities.
//   - ContextSourceProvider: pulls recent relevant claims into planner
//     context via the upstream Client.Context primitive.
//
// Roles NOT played:
//   - ChannelProvider — Mnemos isn't conversational.
//   - TriggerProvider — Mnemos doesn't push inbound events.
//   - WebhookReceiver — same.
//
// See docs/adr/0004-nomi-mnemos-cognitive-boundary.md (revised) and
// roady feature #113.
package mnemos
