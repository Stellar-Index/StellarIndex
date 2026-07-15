// Package consumer defines the transport-neutral ingest contract:
// the [Event] sum-type interface that every source's emitted value
// implements.
//
// [Event] is the type the indexer's event sink type-switches on to
// attribute each row to its source. Concrete shapes — e.g.
// `soroswap.TradeEvent`, `reflector.UpdateEvent`,
// `external.TradeEvent` — are defined in the source packages and
// all satisfy this interface.
//
// Every value emitted by a decoder, whether dispatched through the
// [internal/dispatcher] hot path or produced by an
// [internal/sources/external] connector goroutine, lands on a
// `chan consumer.Event` and gets sunk by
// `internal/pipeline` (driven from `cmd/stellarindex-indexer`).
//
// # Retired: the per-source-goroutine Orchestrator
//
// This package used to also hold a `Source` interface
// (`BackfillRange` / `StreamLive` / `Health`) and an `Orchestrator`
// that ran one goroutine per source over stellar-rpc. That topology
// was retired when r1 dropped stellar-rpc (2026-04-23) and the
// one-writer-per-domain projection architecture landed (ADR-0031 /
// ADR-0032); the code was deleted once it had zero production
// callers. Production ingest is dispatcher-based:
// Galexie MinIO → internal/ledgerstream → internal/dispatcher →
// per-source decoders. New on-chain sources register a
// [github.com/Stellar-Index/StellarIndex/internal/dispatcher.Decoder]
// (or OpDecoder / ContractCallDecoder / LedgerEntryChangeDecoder) —
// never a per-source goroutine with its own RPC client. See
// docs/architecture/ingest-pipeline.md for the binding rules.
//
// Off-chain CEX/FX venues DO run per-venue goroutines, but through
// the sibling framework in `internal/sources/external/` — not
// through anything in this package.
//
// # Invariants for Event values
//
//   - Every emitted event wraps a fully-formed value from
//     [internal/canonical] — never a partial / unvalidated
//     struct.
//   - Amount fields are *big.Int via canonical.Amount. See
//     ADR-0003.
package consumer
