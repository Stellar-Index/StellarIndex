package redispub

import "time"

// DefaultChannel is the Redis pub/sub channel the [Publisher]
// writes to and the matching Subscriber listens on by default.
// Operators with multiple deployments sharing one Redis override it
// per environment with `[storage].redis_closed_bucket_channel`, read
// by both the aggregator and the API.
const DefaultChannel = "stellarindex:closed-bucket:v1"

// ClosedBucketEvent is the JSON wire shape published per
// successful (pair, window) VWAP cache write. Carries the minimum
// the API-side subscriber needs to reconstruct the SSE topic and
// payload.
//
// Versioning: schema additions are non-breaking via JSON's
// "ignore unknown fields" decoding; subscribers tolerant of new
// fields, no schema bump needed. Removals or type changes are
// breaking and require a new channel name (see [DefaultChannel]).
type ClosedBucketEvent struct {
	// Asset + Quote echo the canonical asset strings the
	// aggregator just published a closed bucket for. The
	// API-side subscriber routes by (Asset, Quote, WindowSeconds)
	// into `closed:<asset>/<quote>/<window_seconds>` Hub topics.
	Asset string `json:"asset"`
	Quote string `json:"quote"`

	// WindowSeconds is the closed-bucket window expressed as
	// integer seconds. Encoded as a number rather than a Go
	// duration string for cross-language subscriber friendliness.
	WindowSeconds int64 `json:"window_seconds"`

	// ValueDecimal is the VWAP rendered as a fixed-precision
	// decimal string (12 fractional digits) — the same form the
	// aggregator wrote to the Redis cache key. Strings preserve
	// big.Rat precision the JSON `number` type would lose.
	ValueDecimal string `json:"value_decimal"`

	// ObservedAt is the end of the closed 1-minute bucket the
	// aggregator computed this VWAP at; the VWAP covers
	// [ObservedAt-WindowSeconds, ObservedAt). RFC 3339 UTC.
	// With the topic it is the event's identity: the subscriber
	// forwards one event per (topic, ObservedAt) and drops a repeat or
	// an older bucket.
	ObservedAt time.Time `json:"observed_at"`

	// ProducerID names the publishing aggregator process (minted once
	// per [Publisher]). Never forwarded to clients; the subscriber uses
	// it to report two aggregators publishing the same bucket.
	ProducerID string `json:"producer_id,omitempty"`
}
