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

// Observation is one AccountEntry-delta record: the post-change
// AccountEntry fields at the ledger that produced the change. Per
// ADR-0021 the observer does not infer "what changed"; readers diff
// successive observations.
//
// One Observation per (account, ledger); the writer dedupes via the
// (account_id, ledger) primary key (last-writer-wins is safe: the final
// post-state is deterministic).
//
// Removed accounts emit Balance=0 plus a flag, see [Observation.IsRemoval].
//
// Field-for-field identical to [domain.AccountObservation], the
// persisted-shape definition (internal/storage/timescale reads/writes it
// and must not import upward). It is its OWN named type, not an alias,
// because it carries the EventKind()/Source() methods (consumer.go) that
// satisfy consumer.Event. Call sites crossing the storage boundary
// (internal/pipeline/sink.go, cmd/stellarindex-ops/supply_seed.go)
// convert explicitly via domain.AccountObservation(o).
type Observation domain.AccountObservation
