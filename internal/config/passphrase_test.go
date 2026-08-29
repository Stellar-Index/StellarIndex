package config

import "testing"

// Passphrase is the network-passphrase the indexer hands to
// stellar-rpc / SDK code paths to compute transaction hashes.
// Validate() catches unknown network names at startup, so the
// fallback empty-string return is unreachable in practice — but
// the test pins it to surface a regression in case validation
// ever drifts.

func TestStellarConfig_Passphrase(t *testing.T) {
	cases := []struct {
		network string
		want    string
	}{
		{"pubnet", pubnetPassphrase},
		{"testnet", testnetPassphrase},
		{"futurenet", futurenetPassphrase},
	}
	for _, tc := range cases {
		t.Run(tc.network, func(t *testing.T) {
			s := StellarConfig{Network: tc.network}
			if got := s.Passphrase(); got != tc.want {
				t.Errorf("Passphrase(%q) = %q, want %q", tc.network, got, tc.want)
			}
		})
	}
}

func TestStellarConfig_Passphrase_unknownNetwork(t *testing.T) {
	// Unreachable in practice (Validate rejects), but the function
	// must not panic on a junk Network — return empty string so
	// callers see a clearly-wrong passphrase rather than a crash.
	s := StellarConfig{Network: "definitely-not-a-real-network"}
	if got := s.Passphrase(); got != "" {
		t.Errorf("Passphrase(unknown) = %q, want empty string", got)
	}
}

func TestStellarConfig_Passphrase_emptyNetwork(t *testing.T) {
	// Zero-value StellarConfig: Network=="". Same fallback path —
	// must return "" without panicking.
	var s StellarConfig
	if got := s.Passphrase(); got != "" {
		t.Errorf("Passphrase(zero-value) = %q, want empty string", got)
	}
}

// TestStellarConfig_Passphrase_LiteralPin asserts the exact
// github.com/stellar/go-stellar-sdk/network strings. The package
// constants alias internal/canonical, so the table test above only
// proves Passphrase() returns whatever canonical says — this test
// proves canonical says the right thing.
func TestStellarConfig_Passphrase_LiteralPin(t *testing.T) {
	cases := map[string]string{
		"pubnet":    "Public Global Stellar Network ; September 2015",
		"testnet":   "Test SDF Network ; September 2015",
		"futurenet": "Test SDF Future Network ; October 2022",
	}
	for network, want := range cases {
		if got := (StellarConfig{Network: network}).Passphrase(); got != want {
			t.Errorf("Passphrase(%q) = %q, want %q", network, got, want)
		}
	}
}
