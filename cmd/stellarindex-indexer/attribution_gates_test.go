package main

import (
	"testing"

	soroswap_router "github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
)

// TestRouterEnabled_FoldsWhitespaceAndCase pins that routerEnabled folds enabled_sources with ToLower(TrimSpace) like
// config validation and the dispatcher; an entry like " Soroswap-Router " must match, or the sweeper silently never
// starts.
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

// TestAmmSignerEnabled_FoldsWhitespaceAndCase is the ammSignerEnabled analog: a fold mismatch against
// timescale.AMMSignerSources silently disables signer attribution for a correctly configured source.
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
