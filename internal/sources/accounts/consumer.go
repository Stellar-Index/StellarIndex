package accounts

import "github.com/Stellar-Index/StellarIndex/internal/consumer"

// EventKind / Source on Observation implement [consumer.Event]; the sink type-switches on
// ObservationKind to route into the account_observations hypertable.

// EventKind implements [consumer.Event].
func (Observation) EventKind() string { return ObservationKind }

// Source implements [consumer.Event].
func (Observation) Source() string { return SourceName }

var _ consumer.Event = Observation{}
