// Package consumer defines [Event], the sum-type interface every value a
// source emits implements. Decoders on the dispatcher path and the
// off-chain connectors in internal/sources/external send on a
// `chan consumer.Event`; internal/pipeline sinks it, type-switching on
// the concrete type to attribute each row to its source.
//
// New on-chain sources register a dispatcher Decoder, never a goroutine
// with its own RPC client (docs/architecture/ingest-pipeline.md).
//
// An Event always wraps a fully validated internal/canonical value, and
// its amounts are canonical.Amount (ADR-0003).
package consumer
