package divergence

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Divergence must never read a `raw:` oracle row (an unmapped symbol recorded verbatim); it is safe only by a chain of three facts in three packages: Pair.Validate refuses a raw leg,
// OracleReference.LookupQuote keys derive from validated legs, and Store.LatestOracleObservation matches `asset = ANY($2)` exactly.
// This test asserts the PROPERTY (no raw key reaches the reader), so it survives refactors and fails if any link breaks.

// keyRecordingOracleReader captures every key set it is asked for, so a
// test can assert on what divergence REQUESTED — not merely on what it
// did with the answer. A guard that only checked the response would pass
// against a reader that was never asked the dangerous question.
type keyRecordingOracleReader struct {
	baseKeys  []string
	quoteKeys []string
}

func (r *keyRecordingOracleReader) LatestOracleObservation(
	_ context.Context, _ string, baseKeys, quoteKeys []string,
) (*canonical.OracleUpdate, error) {
	r.baseKeys = append(r.baseKeys, baseKeys...)
	r.quoteKeys = append(r.quoteKeys, quoteKeys...)
	return nil, nil
}

func (r *keyRecordingOracleReader) allKeys() []string {
	return append(append([]string{}, r.baseKeys...), r.quoteKeys...)
}

// TestRawSymbolCannotBecomeAPairLeg pins link (1), which the others rest on: if Pair.Validate accepts a raw leg, the other test would ask about a pair that should never exist.
func TestRawSymbolCannotBecomeAPairLeg(t *testing.T) {
	raw, err := canonical.NewOracleRawAsset("USDT0")
	if err != nil {
		t.Fatalf("NewOracleRawAsset: %v", err)
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("NewFiatAsset: %v", err)
	}

	if _, err := canonical.NewPair(raw, usd); err == nil {
		t.Error("NewPair accepted a raw oracle symbol as the BASE leg — a raw row is " +
			"record-layer evidence of an unmapped symbol, not an asset with a price")
	}
	if _, err := canonical.NewPair(usd, raw); err == nil {
		t.Error("NewPair accepted a raw oracle symbol as the QUOTE leg")
	}
}

// TestDivergenceNeverAsksTheOracleForARawKey pins the property itself.
// Every pair divergence can legitimately be handed is built from mapped
// assets, so no raw key may ever reach the read seam.
func TestDivergenceNeverAsksTheOracleForARawKey(t *testing.T) {
	rec := &keyRecordingOracleReader{}
	ref, err := NewOracleReference(OracleReferenceOptions{
		Source: OracleSourceRedstone,
		Reader: rec,
	})
	if err != nil {
		t.Fatalf("NewOracleReference: %v", err)
	}

	native, err := canonical.ParseAsset("native")
	if err != nil {
		t.Fatalf("parse native: %v", err)
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("NewFiatAsset: %v", err)
	}
	pair, err := canonical.NewPair(native, usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	// The (nil, nil) response maps to ErrAssetUnsupported; the return
	// value is not what this test is about — the REQUEST is.
	_, _ = priceOf(ref.LookupQuote(context.Background(), pair, time.Now().UTC()))

	if len(rec.allKeys()) == 0 {
		t.Fatal("the reader was never called, so this test proved nothing about " +
			"which keys divergence asks for — the vacuous-pass shape this guard exists to avoid")
	}
	for _, k := range rec.allKeys() {
		if strings.HasPrefix(k, "raw:") {
			t.Errorf("divergence asked the oracle read seam for %q — an interpretation-layer "+
				"consumer must never key on a raw oracle symbol", k)
		}
	}
}

// TestOracleAssetKeysNeverSynthesisesARawKey guards the expander this
// package binds its oracle keys through (the alias registry). Its alias
// families are exactly the kind of expansion that could grow a raw arm
// later without anyone noticing the layer violation.
func TestOracleAssetKeysNeverSynthesisesARawKey(t *testing.T) {
	native, _ := canonical.ParseAsset("native")
	usd, _ := canonical.NewFiatAsset("USD")
	xlm, err := canonical.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatalf("parse crypto:XLM: %v", err)
	}

	for _, a := range []canonical.Asset{native, usd, xlm} {
		keys := canonical.AssetAliasStrings(a)
		if len(keys) == 0 {
			t.Errorf("AssetAliasStrings(%s) returned no keys", a.String())
		}
		for _, k := range keys {
			if strings.HasPrefix(k, "raw:") {
				t.Errorf("AssetAliasStrings(%s) synthesised the raw key %q", a.String(), k)
			}
		}
	}
}
