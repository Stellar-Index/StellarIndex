package main

import "testing"

// TestFormatDivergencePrice pins our_price/median to the decimal-as-string
// shape DivergenceFiringWebhookPayload documents (openapi/stellar-index.v1.yaml).
// Marshaling the raw float64 fields directly emits a JSON number, which is
// what every live delivery sent before this fix (RLT-081).
func TestFormatDivergencePrice(t *testing.T) {
	got := formatDivergencePrice(0.1234567)
	want := "0.1234567"
	if got != want {
		t.Errorf("formatDivergencePrice(0.1234567) = %q, want %q", got, want)
	}
}

// TestDivergenceSourceNames pins `sources` to an array of source NAMES,
// per the spec's `type: array, items: {type: string}`. Marshaling the raw
// map[string]float64 directly emits a JSON object (name -> price), which
// is what every live delivery sent before this fix (RLT-081).
func TestDivergenceSourceNames(t *testing.T) {
	got := divergenceSourceNames(map[string]float64{
		"coingecko": 1.05,
		"chainlink": 1.06,
	})
	want := []string{"chainlink", "coingecko"}
	if len(got) != len(want) {
		t.Fatalf("divergenceSourceNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("divergenceSourceNames()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
