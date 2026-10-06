package client

import "time"

// SubstanceEvidence is the trailing substance measurement behind a
// thin-market verdict. Volumes are decimal strings.
type SubstanceEvidence struct {
	Base          string         `json:"base"`
	Quote         string         `json:"quote"`
	WindowSeconds int64          `json:"window_seconds"`
	MeasuredAt    time.Time      `json:"measured_at"`
	WindowEnd     *time.Time     `json:"window_end,omitempty"`
	VolumeUSD     string         `json:"volume_usd"`
	Buckets       int64          `json:"buckets"`
	ValuedBuckets int64          `json:"valued_buckets"`
	SpanSeconds   int64          `json:"span_seconds"`
	Floor         SubstanceFloor `json:"floor"`
	// Failed is the first floor failed: "buckets", "span", "volume" or "volume_unvalued".
	Failed string `json:"failed"`
}
