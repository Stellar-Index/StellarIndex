package redispub

import "time"

// DefaultChannel is the Redis pub/sub channel the [Publisher] writes and the Subscriber reads.
const DefaultChannel = "stellarindex:closed-bucket:v1"

// KindFrozen marks a [ClosedBucketEvent] for a bucket a freeze refused: identity, no value. The empty Kind is a price, so producers omitting the field still decode as prices.
const KindFrozen = "frozen"

// ClosedBucketEvent is the JSON wire shape per (pair, window) VWAP cache write. Additions are non-breaking (unknown fields are ignored); removals or type changes need a new channel name (see [DefaultChannel]).
type ClosedBucketEvent struct {
	// Asset + Quote are the canonical asset strings; the subscriber routes by (Asset, Quote,
	// WindowSeconds) into `closed:<asset>/<quote>/<window_seconds>` Hub topics.
	Asset string `json:"asset"`
	Quote string `json:"quote"`

	// WindowSeconds is the window in integer seconds, not a Go duration string, so non-Go
	// subscribers can read it.
	WindowSeconds int64 `json:"window_seconds"`

	// ValueDecimal is the VWAP as a 12-fractional-digit decimal string, the form the aggregator
	// wrote to the Redis cache key; a string keeps big.Rat precision a JSON number would lose.
	ValueDecimal string `json:"value_decimal"`

	// ObservedAt is the end of the closed 1-minute bucket the VWAP was computed at, covering
	// [ObservedAt-WindowSeconds, ObservedAt) unless Truncated. RFC 3339 UTC. With the topic it is
	// the event's identity: the subscriber forwards one event per (topic, ObservedAt) and drops a
	// repeat or an older bucket.
	ObservedAt time.Time `json:"observed_at"`

	// ProducerID names the publishing aggregator process. Never forwarded to clients; the
	// subscriber uses it to report two aggregators publishing the same bucket.
	ProducerID string `json:"producer_id,omitempty"`

	// Truncated is true when the trade read hit the row cap, so the value covers only trades
	// after CoveredFrom completely. Both are absent when coverage is unknown (a triangulated
	// composite).
	Truncated   *bool      `json:"truncated,omitempty"`
	CoveredFrom *time.Time `json:"covered_from,omitempty"`

	// Kind is empty for a published price and [KindFrozen] for a refused
	// bucket, which has no ValueDecimal, Truncated or CoveredFrom.
	Kind string `json:"kind,omitempty"`

	// FrozenSince is the end of the first bucket the freeze refused; set
	// only on a [KindFrozen] event, and absent when the producer does not
	// know when the freeze engaged.
	FrozenSince *time.Time `json:"frozen_since,omitempty"`
}
