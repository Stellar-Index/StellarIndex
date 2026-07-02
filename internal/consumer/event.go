package consumer

// Event is the sum type of every payload a source can emit.
// Concrete shapes are defined in internal/canonical; consumers
// type-switch on the concrete type, not this interface.
type Event interface {
	// EventKind is a constant string — e.g. "soroswap.trade",
	// "aquarius.trade". Used as the "kind" label on
	// event-classification metrics.
	EventKind() string

	// Source returns the stable source-name so the event-sink
	// pipeline can attribute metrics + logs without
	// type-switching. It must be [a-z0-9_-]+, short, lowercase —
	// it appears as a metrics label and in the trades.source
	// column.
	Source() string
}
