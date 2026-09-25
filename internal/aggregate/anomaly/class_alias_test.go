package anomaly_test

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func mustAsset(t *testing.T, s string) canonical.Asset {
	t.Helper()
	a, err := canonical.ParseAsset(s)
	if err != nil {
		t.Fatalf("ParseAsset(%q): %v", s, err)
	}
	return a
}

// TestClassifier_FoldsAliases: XLM trades as both native and crypto:XLM
// pairs, and the default pair set carries both. A class set on either
// spelling must apply to the other, or that population silently falls to
// ClassDefault's looser freeze threshold.
func TestClassifier_FoldsAliases(t *testing.T) {
	for _, key := range []string{"native", "crypto:XLM"} {
		c := anomaly.NewClassifier(map[string]anomaly.AssetClass{key: anomaly.ClassCrypto})
		for _, asset := range []string{"native", "crypto:XLM"} {
			if got := c.ClassOf(mustAsset(t, asset)); got != anomaly.ClassCrypto {
				t.Errorf("classified %q: ClassOf(%s) = %q, want crypto", key, asset, got)
			}
		}
	}
}

// TestValidateOverrides_RejectsAliasConflict: two spellings of one asset
// with different classes has no correct reading and must fail at boot.
func TestValidateOverrides_RejectsAliasConflict(t *testing.T) {
	conflicting := map[string]anomaly.AssetClass{
		"native":     anomaly.ClassCrypto,
		"crypto:XLM": anomaly.ClassStablecoin,
	}
	if err := anomaly.ValidateOverrides(conflicting); err == nil {
		t.Error("ValidateOverrides accepted native=crypto and crypto:XLM=stablecoin")
	}
	agreeing := map[string]anomaly.AssetClass{
		"native":     anomaly.ClassCrypto,
		"crypto:XLM": anomaly.ClassCrypto,
		"fiat:USD":   anomaly.ClassStablecoin,
	}
	if err := anomaly.ValidateOverrides(agreeing); err != nil {
		t.Errorf("ValidateOverrides rejected consistent aliases: %v", err)
	}
}
