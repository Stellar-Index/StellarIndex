// Package domain holds PERSISTED data shapes that both the storage
// tier (internal/storage/timescale, internal/storage/clickhouse) and
// one or more compute/source packages (internal/aggregate/mev,
// internal/divergence, internal/sources/accounts,
// internal/sources/blend, internal/sources/sorobanevents,
// internal/aggregate/baseline) need to share.
//
// # Why this package exists (D8 M0-1)
//
// Before this package existed, internal/storage/timescale imported
// UPWARD into compute + source packages (internal/aggregate/mev,
// internal/divergence, internal/sources/accounts,
// internal/sources/blend, internal/sources/sorobanevents,
// internal/aggregate/baseline) purely to reference the plain data
// shape a Store method reads or writes — e.g. mev.StoredEvent,
// accounts.Observation, blend.PositionEvent. That inversion made
// "storage is the persistence tier below compute" (the D8 dependency-
// direction map, docs/maintainability-audit-2026-07-01/
// D8-dependency-direction.md) unstatable and un-enforceable: the
// import-lint's L/storage-below-compute rule
// (scripts/ci/lint-imports.sh) had to grandfather the whole set via
// scripts/ci/lint-imports.baseline.
//
// The fix: the struct definitions live HERE — a leaf package with NO
// internal dependency beyond internal/canonical (itself dependency-
// free) — and storage imports domain directly instead of reaching
// into aggregate/divergence/sources. The origin packages keep their
// existing public names (mev.StoredEvent, accounts.Observation, …)
// unchanged for every OTHER caller: a type with no methods anywhere
// in its origin package re-exports via a plain alias
// (`type StoredEvent = domain.MEVStoredEvent`); a type that carries
// methods in its origin package (e.g. accounts.Observation implements
// consumer.Event via EventKind()/Source()) re-declares as a locally
// DEFINED type over the domain shape
// (`type Observation domain.AccountObservation`), which keeps the
// methods legal (Go allows methods on any type declared in the
// package, even one whose underlying type comes from elsewhere) at
// the cost of an explicit conversion at the few call sites that hand
// a value across the storage boundary (internal/pipeline/sink.go and
// friends) — Go's compiler enforces every one of those sites, so
// there's no way to miss one.
//
// # Scope
//
// Only the plain, method-free-at-storage-boundary PERSISTED shapes
// move here — not compute logic, not Sink/Scanner interfaces (those
// stay in the upward package: Go's structural typing means storage
// never has to import an interface it merely satisfies), and not
// types whose fields recursively pull in policy logic (e.g.
// aggregate/anomaly.Decision's Class/Thresholds fields, or
// aggregate.FiatProxy / sources/external's registry functions).
// Those remain grandfathered in lint-imports.baseline pending a
// larger redesign — see the baseline file's comments for the current
// list and why each entry couldn't move mechanically.
package domain
