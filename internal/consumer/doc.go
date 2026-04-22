// Package consumer defines the stable interface every source
// indexer implements — SDEX, Soroswap, Aquarius, Phoenix, Comet,
// Blend, Reflector, Redstone, Band, CEX connectors, FX feeds,
// and reference aggregators.
//
// # Contract
//
// A Source has two orchestration modes:
//
//   - [Source.StreamLive]     — subscribes to the live source and
//     emits canonical trades / prices as they happen.
//   - [Source.BackfillRange]  — walks a bounded historical range
//     and emits every matching canonical trade / price for replay.
//
// Both emit on the same output channel type. Consumers treat
// them uniformly; orchestration in cmd/ratesengine-indexer
// decides which mode to run per source.
//
// # Invariants
//
//   - Every emitted event is a fully-formed value from
//     internal/canonical — never a partial / unvalidated struct.
//   - Amount fields are *big.Int via canonical.Amount. See
//     ADR-0003.
//   - Sources honour ctx.Done() promptly. No unbounded blocking.
//   - Sources are safe to Stop + re-create; orchestrator
//     restarts on unrecoverable error and feeds the resumption
//     cursor back in.
//
// # Adding a new source
//
// See [docs/development/contributing-a-source.md] (lands with
// Week-2 of the delivery plan). Short form:
//
//  1. Create internal/sources/<name>/ with doc.go + events.go +
//     decode.go + consumer.go + source_test.go.
//  2. Implement Source.
//  3. Register in internal/sources/registry.go.
//  4. Add fixtures under test/fixtures/<name>/.
//
// [docs/development/contributing-a-source.md]: ../../docs/development/contributing-a-source.md
package consumer
