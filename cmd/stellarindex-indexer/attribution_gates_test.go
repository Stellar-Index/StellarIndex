package main

import (
	"testing"

	soroswap_router "github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
)

// TestRouterEnabled_FoldsWhitespaceAndCase pins T187: routerEnabled gates
// the routed-via attribution sweeper on membership in
// ingestion.enabled_sources, but internal/config/validate.go and
// internal/pipeline/dispatcher.go both fold that list with
// strings.ToLower(strings.TrimSpace(...)) before comparing/dispatching.
// An operator entry like " Soroswap-Router " (accepted by config.Validate,
// dispatched correctly) must also be recognised here — otherwise the
// sweeper silently never starts even though the source is live and
// writing soroswap_router_swaps.
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

// TestAmmSignerEnabled_FoldsWhitespaceAndCase is T187's ammSignerEnabled
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
