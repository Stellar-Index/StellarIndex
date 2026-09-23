// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// Decimals LOCKSTEP on GET /v1/assets/{id} (C1-050). The response depends on
// TWO decimals resolvers: the lake's on-chain decimals() (TokenDecimals →
// detail.Decimals, the supply divisor) and the `nonstandard_decimals_assets`
// projection (NonstandardDecimals, which the USD price is normalised
// through). Pre-fix the cap was computed regardless of whether they agreed.
//
// Fixture throughout: flaggedAsset with 1000 tokens circulating / 2000 max
// (both at 9 dp), raw asset/fiat:USD ratio 41.32. With both resolvers at 9
// the truth is price 4132, cap 4,132,000.00, FDV 8,264,000.00 (the existing
// TestAssetsF2_NonstandardDecimals_NormalizesPriceAndCaps).
//
// PROVEN-RED: with the lockstep check in applyTokenDecimals and the refusal
// in populateMarketCap removed, the three mismatch cases below serve a
// NUMBER — 4,132,000,000.00, 41,320.00 and 413,200,000.00 respectively —
// each wrong by a power of ten.

// lockstepDecStub is a TokenDecimalsReader with error injection (decStub
// has none).
type lockstepDecStub struct {
	d     uint32
	found bool
	err   error
}

func (s *lockstepDecStub) TokenDecimals(_ context.Context, _ string) (uint32, bool, error) {
	return s.d, s.found, s.err
}

func lockstepSupply() *stubSupplyLooker {
	circ := new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(9), nil))
	maxSupply := new(big.Int).Mul(big.NewInt(2000), new(big.Int).Exp(big.NewInt(10), big.NewInt(9), nil))
	return &stubSupplyLooker{hit: true, snap: supply.Supply{CirculatingSupply: circ, MaxSupply: maxSupply}}
}

func lockstepPrices() *stubPriceReader {
	return &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{flaggedAsset + "/fiat:USD": {
			AssetID: flaggedAsset, Quote: "fiat:USD", Price: "41.32", PriceType: "vwap",
		}},
	}
}

func lockstepGet(t *testing.T, srv *v1.Server) string {
	t.Helper()
	tsrv := startHTTPTest(t, srv.Handler())
	resp := mustGet(t, tsrv.URL+"/v1/assets/"+flaggedAsset)
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	return body
}

// GH-1059: the asset_detail site is request-driven (any contract id a
// client asks for), so the counter no longer carries the contract id as a
// label — only the bounded guard_reconcile site does.
func assetDetailMismatchVal() float64 {
	return testutil.ToFloat64(obs.NonstandardDecimalsLockstepMismatchTotal.WithLabelValues("asset_detail", ""))
}

// The projection says 9 (so the price is normalised ×100 → 4132) but the
// lake says 6. Supply at 6 dp × price at 9 dp is 4,132,000,000.00 — a
// thousand times the truth. The cap must be REFUSED with the reason flag;
// decimals still reports the lake, price_usd still serves.
func TestAssetsF2_DecimalsLockstep_ProjectionDisagreesWithLake(t *testing.T) {
	before := assetDetailMismatchVal()
	srv := v1.New(v1.Options{
		Prices:              lockstepPrices(),
		Supply:              lockstepSupply(),
		TokenDecimals:       &lockstepDecStub{d: 6, found: true},
		NonstandardDecimals: nonstandardDecimalsCacheWith(t, flaggedAsset, 9),
	})
	body := lockstepGet(t, srv)

	if strings.Contains(body, `"market_cap_usd"`) {
		t.Errorf("market_cap_usd served on mismatched scales: %s", body)
	}
	if strings.Contains(body, `"fdv_usd"`) {
		t.Errorf("fdv_usd served on mismatched scales: %s", body)
	}
	if !strings.Contains(body, `"market_cap_decimals_mismatch":true`) {
		t.Errorf("market_cap_decimals_mismatch reason flag missing: %s", body)
	}
	if strings.Contains(body, `"market_cap_low_liquidity"`) {
		t.Errorf("mismatch must not be reported as a liquidity verdict: %s", body)
	}
	if !strings.Contains(body, `"decimals":6`) {
		t.Errorf("decimals must still report the lake reading (6): %s", body)
	}
	if !strings.Contains(body, `"price_usd":"4132.0000000000"`) {
		t.Errorf("price_usd must still serve (projection-normalised 4132): %s", body)
	}
	if !strings.Contains(body, `"circulating_supply":"1000000000000"`) {
		t.Errorf("circulating_supply (a raw fact) must still serve: %s", body)
	}
	if got := assetDetailMismatchVal() - before; got != 1 {
		t.Errorf("asset_detail mismatch counter delta = %v, want 1", got)
	}
}

// The lake says 9 but the projection has NO row for this token (the guard
// has not seeded it yet — dormant token, or guard lag). The price is then
// the RAW ratio 41.32 (nothing normalised it) while the supply is divided
// by 10^9: 1000 × 41.32 = 41,320.00, a hundredth of the truth. Refuse.
func TestAssetsF2_DecimalsLockstep_LakeNonstandardProjectionUnseeded(t *testing.T) {
	before := assetDetailMismatchVal()
	const otherAsset = "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"
	srv := v1.New(v1.Options{
		Prices:              lockstepPrices(),
		Supply:              lockstepSupply(),
		TokenDecimals:       &lockstepDecStub{d: 9, found: true},
		NonstandardDecimals: nonstandardDecimalsCacheWith(t, otherAsset, 8), // populated, but not for flaggedAsset
	})
	body := lockstepGet(t, srv)

	if strings.Contains(body, `"market_cap_usd"`) || strings.Contains(body, `"fdv_usd"`) {
		t.Errorf("cap/FDV served although the projection never normalised the price: %s", body)
	}
	if !strings.Contains(body, `"market_cap_decimals_mismatch":true`) {
		t.Errorf("market_cap_decimals_mismatch reason flag missing: %s", body)
	}
	if !strings.Contains(body, `"decimals":9`) {
		t.Errorf("decimals must report the lake reading (9): %s", body)
	}
	if !strings.Contains(body, `"price_usd":"41.32"`) {
		t.Errorf("price_usd is the raw ratio on this path and must still serve as-is: %s", body)
	}
	if got := assetDetailMismatchVal() - before; got != 1 {
		t.Errorf("asset_detail mismatch counter delta = %v, want 1", got)
	}
}

// The lake read FAILS (timeout / ClickHouse down) while the projection
// carries 9. The price was normalised with 9, so the supply must be scaled
// by 9 too: 1000 × 4132 = 4,132,000.00. Pre-fix decimals stayed at the
// default 7 and the cap came out 413,200,000.00 — a hundred times the
// truth — on every lake blip.
func TestAssetsF2_DecimalsLockstep_LakeUnavailableUsesProjection(t *testing.T) {
	before := assetDetailMismatchVal()
	srv := v1.New(v1.Options{
		Prices:              lockstepPrices(),
		Supply:              lockstepSupply(),
		TokenDecimals:       &lockstepDecStub{err: errors.New("clickhouse: context deadline exceeded")},
		NonstandardDecimals: nonstandardDecimalsCacheWith(t, flaggedAsset, 9),
	})
	body := lockstepGet(t, srv)

	if !strings.Contains(body, `"decimals":9`) {
		t.Errorf("decimals must fall back to the projection's confirmed 9: %s", body)
	}
	if !strings.Contains(body, `"market_cap_usd":"4132000.00"`) {
		t.Errorf("market_cap_usd = want 4132000.00 (supply and price on ONE scale): %s", body)
	}
	if !strings.Contains(body, `"fdv_usd":"8264000.00"`) {
		t.Errorf("fdv_usd = want 8264000.00: %s", body)
	}
	if strings.Contains(body, `"market_cap_decimals_mismatch"`) {
		t.Errorf("no mismatch on this path — the projection is the only reading: %s", body)
	}
	if got := assetDetailMismatchVal() - before; got != 0 {
		t.Errorf("asset_detail mismatch counter delta = %v, want 0", got)
	}
}

// Same fallback when the lake is readable but NOT DERIVABLE for this token
// (found=false): the projection's confirmed value is the only reading.
func TestAssetsF2_DecimalsLockstep_LakeNotDerivableUsesProjection(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices:              lockstepPrices(),
		Supply:              lockstepSupply(),
		TokenDecimals:       &lockstepDecStub{found: false},
		NonstandardDecimals: nonstandardDecimalsCacheWith(t, flaggedAsset, 9),
	})
	body := lockstepGet(t, srv)

	if !strings.Contains(body, `"decimals":9`) || !strings.Contains(body, `"market_cap_usd":"4132000.00"`) {
		t.Errorf("want decimals 9 and market_cap_usd 4132000.00 from the projection: %s", body)
	}
}

// Agreement is byte-for-byte the pre-lockstep happy path: both at 9, cap
// and FDV served, no flag.
func TestAssetsF2_DecimalsLockstep_AgreementServesCap(t *testing.T) {
	before := assetDetailMismatchVal()
	srv := v1.New(v1.Options{
		Prices:              lockstepPrices(),
		Supply:              lockstepSupply(),
		TokenDecimals:       &lockstepDecStub{d: 9, found: true},
		NonstandardDecimals: nonstandardDecimalsCacheWith(t, flaggedAsset, 9),
	})
	body := lockstepGet(t, srv)

	if !strings.Contains(body, `"market_cap_usd":"4132000.00"`) || !strings.Contains(body, `"fdv_usd":"8264000.00"`) {
		t.Errorf("agreement must serve cap 4132000.00 / FDV 8264000.00: %s", body)
	}
	if strings.Contains(body, `"market_cap_decimals_mismatch"`) {
		t.Errorf("flag must be omitted on agreement: %s", body)
	}
	if got := assetDetailMismatchVal() - before; got != 0 {
		t.Errorf("asset_detail mismatch counter delta = %v, want 0", got)
	}
}

// A 7-dp token with no projection row is the overwhelmingly common case and
// is in lockstep by definition (row present ⇔ lake ≠ 7): served as before.
func TestAssetsF2_DecimalsLockstep_StandardTokenUnaffected(t *testing.T) {
	circ := new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(7), nil))
	srv := v1.New(v1.Options{
		Prices:              lockstepPrices(),
		Supply:              &stubSupplyLooker{hit: true, snap: supply.Supply{CirculatingSupply: circ}},
		TokenDecimals:       &lockstepDecStub{d: 7, found: true},
		NonstandardDecimals: nonstandardDecimalsCacheWith(t, "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J", 8),
	})
	body := lockstepGet(t, srv)

	// 1000 tokens × raw 41.32 (no normalisation for a 7dp/7dp pair).
	if !strings.Contains(body, `"market_cap_usd":"41320.00"`) {
		t.Errorf("7dp token must serve its cap unchanged (41320.00): %s", body)
	}
	if strings.Contains(body, `"market_cap_decimals_mismatch"`) {
		t.Errorf("flag must be omitted for a 7dp token: %s", body)
	}
}
