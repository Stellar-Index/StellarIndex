package accounts

import (
	"github.com/Stellar-Index/StellarIndex/internal/domain"
)

// SourceName is the canonical identifier for the AccountEntry
// observer. Stamped on metrics labels and on every
// [Observation.Source]. Stable.
const SourceName = "accounts"

// ObservationKind is the consumer.Event.EventKind value emitted by
// the observer. The indexer's sink type-switches on this string
// to route observations to the account_observations hypertable.
const ObservationKind = "accounts.observation"

// Observation is one AccountEntry-delta record. Captures the state
// of the observed account at the ledger that produced the change —
// per ADR-0021 the observer doesn't try to infer "what changed"
// (the reader can compute that from successive observations);
// it just records the post-change AccountEntry fields the
// downstream readers consume.
//
// One Observation per (account, ledger) pair. When an account is
// touched multiple times in a single ledger (e.g. multiple ops in
// one tx, or fee + op in the same tx), the observer emits one
// Observation per change; the writer dedupes via the
// (account_id, ledger) primary key (last-writer-wins, which is
// fine — the AccountEntry is monotonic within a ledger so the
// final post-state is deterministic).
//
// Removed accounts emit an Observation with Balance=0 + a flag —
// see [Observation.IsRemoval]. The reader interprets this as
// "account no longer exists at this ledger."
//
// Field-for-field identical to [domain.AccountObservation] — the
// canonical, persisted-shape definition (D8 M0-1:
// internal/storage/timescale reads/writes this shape and must not
// import upward into this package to do so). Observation is declared
// as its OWN named type (not a `= domain.AccountObservation` alias)
// because it carries the EventKind()/Source() methods (consumer.go)
// that satisfy consumer.Event — Go permits methods on any type
// declared in this package, even one whose underlying type comes
// from elsewhere, but NOT on a type alias to a foreign type. The one
// consequence: the couple of call sites that hand an Observation
// across the storage boundary (internal/pipeline/sink.go,
// cmd/stellarindex-ops/supply_seed.go) convert explicitly via
// domain.AccountObservation(o) — legal because the underlying struct
// shape is identical, and the compiler catches every site.
type Observation domain.AccountObservation
