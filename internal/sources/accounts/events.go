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

// Observation is the post-change AccountEntry state at the ledger that produced the change;
// readers diff successive observations (ADR-0021). One per (account, ledger): the writer dedupes
// on that primary key, and last-writer-wins is safe because the post-state is deterministic.
// Removed accounts emit Balance=0 plus [Observation.IsRemoval].
//
// It mirrors [domain.AccountObservation] (which storage reads and must not import upward) as its
// own named type, not an alias, so it can carry the consumer.Event methods. Callers crossing the
// storage boundary convert explicitly via domain.AccountObservation(o).
type Observation domain.AccountObservation
