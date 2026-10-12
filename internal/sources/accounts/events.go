package accounts

import (
	"github.com/Stellar-Index/StellarIndex/internal/domain"
)

// SourceName is the stable identifier for the AccountEntry observer, stamped on metric labels and every [Observation.Source].
const SourceName = "accounts"

// ObservationKind is the consumer.Event.EventKind the observer emits; the sink type-switches on it to route to the account_observations hypertable.
const ObservationKind = "accounts.observation"

// Observation is the post-change AccountEntry state at the producing ledger (ADR-0021), one per (account, ledger) deduped on the primary key (last-writer-wins is safe: post-state is deterministic); removed accounts emit Balance=0 plus [Observation.IsRemoval]. A named type, not an alias of [domain.AccountObservation], so it can carry the consumer.Event methods; convert explicitly across the storage boundary.
type Observation domain.AccountObservation
