package pricingguard

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

// TestNewSubstanceGate_LogsTheEffectivePolicy (GH-1053): r1 sets no
// substance keys, so it runs at library defaults no config file shows.
// The gate states the floors it resolved at construction, so a boot log
// answers "what floor is this deployment enforcing" without a code read.
func TestNewSubstanceGate_LogsTheEffectivePolicy(t *testing.T) {
	var buf bytes.Buffer
	NewSubstanceGate(nil, SubstanceGateOptions{
		Policy: SubstancePolicyFromValues(0, 0, 0, 0),
		Logger: slog.New(slog.NewJSONHandler(&buf, nil)),
	})
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("want one JSON log record, got %q: %v", buf.String(), err)
	}
	want := map[string]any{
		"msg":            "substance gate armed",
		"min_volume_usd": "1000",
		"min_buckets":    float64(DefaultSubstanceMinBuckets),
		"min_span":       DefaultSubstanceMinSpan.String(),
		"window":         DefaultSubstanceWindow.String(),
	}
	for k, v := range want {
		if rec[k] != v {
			t.Errorf("boot log %s = %v, want %v (record %s)", k, rec[k], v, buf.String())
		}
	}
}
