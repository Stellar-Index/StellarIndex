package divergence

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Divergence is an INTERPRETATION-layer consumer of oracle_updates, and
// the oracle capture-totality contract is explicit that it must never
// read a `raw:` row: an unmapped oracle symbol is recorded verbatim as
// evidence, and nothing that interprets prices may treat it as an asset.
//
// The reassuring part is that divergence cannot currently do so. The
// unreassuring part is WHY: it is safe by construction, through a chain
// of three separate facts that live in three different packages —
//
//  1. canonical.Pair.Validate refuses a raw asset as either leg;
//  2. OracleReference.LookupPrice takes a canonical.Pair, so its keys
//     are derived from legs that already passed (1);
//  3. Store.LatestOracleObservation matches `asset = ANY($2)`, an exact
//     set membership, so it can only return what it was asked for.
//
// Break any one link and divergence starts interpreting raw rows, with
// nothing in this package objecting. The test that pinned this was
// deleted by #305's squash merge (issue #339) along with two siblings,
// so the property has been unproven since.
//
// This restores the proof. It is deliberately written against the
// PROPERTY (no raw key ever reaches the reader) rather than against any
// one link, so it keeps holding if the implementation is refactored and
// starts failing if the chain is broken anywhere along it.

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

// TestRawSymbolCannotBecomeAPairLeg pins link (1) — the one the other
// two rest on. If Pair.Validate ever accepts a raw leg, divergence
// starts interpreting unmapped oracle symbols and this suite's other
// test would still pass, because it would be asking about a pair that
// should never have existed.
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
	_, _ = ref.LookupPrice(context.Background(), pair, time.Now().UTC())

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

// TestOracleAssetKeysNeverSynthesisesARawKey guards the one place in
// this package that BUILDS keys rather than passing them through. Its
// XLM dual-form aliasing is exactly the kind of expansion that could
// grow a raw arm later without anyone noticing the layer violation.
func TestOracleAssetKeysNeverSynthesisesARawKey(t *testing.T) {
	native, _ := canonical.ParseAsset("native")
	usd, _ := canonical.NewFiatAsset("USD")
	xlm, err := canonical.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatalf("parse crypto:XLM: %v", err)
	}

	for _, a := range []canonical.Asset{native, usd, xlm} {
		keys := oracleAssetKeys(a)
		if len(keys) == 0 {
			t.Errorf("oracleAssetKeys(%s) returned no keys", a.String())
		}
		for _, k := range keys {
			if strings.HasPrefix(k, "raw:") {
				t.Errorf("oracleAssetKeys(%s) synthesised the raw key %q", a.String(), k)
			}
		}
	}
}
