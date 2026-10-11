package main

import (
	"testing"

	soroswap_router "github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
)

// TestRouterEnabled_FoldsWhitespaceAndCase pins that routerEnabled, which gates the routed-via
// attribution sweeper on ingestion.enabled_sources, folds the list with
// strings.ToLower(strings.TrimSpace(...)) like internal/config/validate.go and
// internal/pipeline/dispatcher.go. An entry like " Soroswap-Router " (accepted by
// config.Validate, dispatched correctly) must be recognised here too, or the sweeper silently
// never starts while the source is live and writing soroswap_router_swaps.
func TestRouterEnabled_FoldsWhitespaceAndCase(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want bool
	}{
		{"exact", []string{soroswap_router.SourceName}, true},
		{"mixed case + padding", []string{"  Soroswap-Router  "}, true},
		{"absent", []string{"comet"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := routerEnabled(tc.in); got != tc.want {
				t.Errorf("routerEnabled(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestAmmSignerEnabled_FoldsWhitespaceAndCase is the ammSignerEnabled
// analog: it reads the same timescale.AMMSignerSources list the tagger
// UPDATE filters on, so a fold mismatch here silently disables signer
// attribution for a correctly-configured, correctly-dispatched source.
func TestAmmSignerEnabled_FoldsWhitespaceAndCase(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want bool
	}{
		{"exact", []string{"comet"}, true},
		{"mixed case + padding", []string{"  Comet  "}, true},
		{"absent", []string{"reflector-dex"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ammSignerEnabled(tc.in); got != tc.want {
				t.Errorf("ammSignerEnabled(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
