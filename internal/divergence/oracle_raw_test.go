package divergence_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
)

// TestOracleReference_RawRowIsNeverAReference pins the oracle
// capture-totality rule on the divergence seam (and, through the
// divergence cache, the confidence cross-oracle factor and the
// Phase-2 freeze lens): a `raw:<symbol>` row — an unmapped oracle
// symbol recorded verbatim (canonical.AssetOracleRaw) — is
// reference-only, orientation-unknown data. Two properties:
//
//  1. Keying is exact canonical strings: the keys asked of the reader
//     for a mapped pair never contain a raw form, so a raw row can
//     never match by construction.
//  2. Defence in depth: should a reader hand back a raw row anyway,
//     LookupPrice must refuse it as ErrAssetUnsupported rather than
//     scale it into a comparison.
func TestOracleReference_RawRowIsNeverAReference(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("parse USD: %v", err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usd)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}

	rawRow := oracleRow(t, divergence.OracleSourceReflectorCEX, "12420000000000", 14, now)
	rawRow.Asset, err = canonical.NewOracleRawAsset("NOTACOIN")
	if err != nil {
		t.Fatalf("NewOracleRawAsset: %v", err)
	}
	reader := &fakeOracleReader{row: rawRow}
	ref := newOracleRef(t, divergence.OracleSourceReflectorCEX, reader, time.Hour)

	got, err := ref.LookupPrice(context.Background(), pair, now)
	if !errors.Is(err, divergence.ErrAssetUnsupported) {
		t.Fatalf("LookupPrice with a raw row = (%v, %v), want ErrAssetUnsupported", got, err)
	}
	if got != 0 {
		t.Errorf("price = %v, want 0 on refusal", got)
	}

	// Exact-string keying: neither key set carries a raw form.
	for _, k := range append(append([]string{}, reader.gotBaseKeys...), reader.gotQuoteKeys...) {
		if strings.HasPrefix(k, "raw:") {
			t.Errorf("reader keyed with raw form %q — keys must be exact mapped canonical strings", k)
		}
	}
	if len(reader.gotBaseKeys) == 0 || reader.gotBaseKeys[0] != "native" {
		t.Errorf("base keys = %v, want exact [native crypto:XLM]", reader.gotBaseKeys)
	}
	if len(reader.gotQuoteKeys) != 1 || reader.gotQuoteKeys[0] != "fiat:USD" {
		t.Errorf("quote keys = %v, want exact [fiat:USD]", reader.gotQuoteKeys)
	}
}
