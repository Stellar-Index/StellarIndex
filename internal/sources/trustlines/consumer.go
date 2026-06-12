package trustlines

import "github.com/StellarIndex/stellar-index/internal/consumer"

func (Observation) EventKind() string { return ObservationKind }
func (Observation) Source() string    { return SourceName }

var _ consumer.Event = Observation{}
