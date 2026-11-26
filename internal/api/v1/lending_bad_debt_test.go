package v1_test

import (
	"context"
	"math/big"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubBadDebtLending serves fixed bad-debt events and records the backstop it was asked about.
type stubBadDebtLending struct {
	stubLendingReader
	events   []timescale.LendingBadDebtEvent
	backstop string
}

func (r *stubBadDebtLending) LendingBadDebtEvents(_ context.Context, _, backstop string, _ bool) ([]timescale.LendingBadDebtEvent, error) {
	r.backstop = backstop
	return r.events, nil
}

// badDebtExplorer serves the backstop's position and d_rate 1.0 at every event's transaction.
type badDebtExplorer struct {
	*stubExplorerReader
	pos clickhouse.LendingAccountPositions
}

func (e badDebtExplorer) LendingAccountPositions(context.Context, string, string) (clickhouse.LendingAccountPositions, error) {
	return e.pos, nil
}

func (badDebtExplorer) LendingReservesAtTxs(_ context.Context, _, _ string, assets []string, txs []clickhouse.LendingTxRef) (map[clickhouse.LendingTxReserve]blend.ReserveData, error) {
	out := map[clickhouse.LendingTxReserve]blend.ReserveData{}
	for _, tx := range txs {
		for _, a := range assets {
			out[clickhouse.LendingTxReserve{LendingTxRef: tx, Asset: a}] = blend.ReserveData{Version: blend.PoolV2, DRate: big.NewInt(1_000_000_000_000)}
		}
	}
	return out, nil
}

// badDebtRoute: a V2 pool whose instance names a backstop; 2 tokens of d-tokens moved to it, 0.5 covered by a fill, 1 written off,
// 0.5 still on its position; the asset trades at 0.50 two hours ago on volumeUSD.
func badDebtRoute(t *testing.T, volumeUSD string) (map[string]any, *stubBadDebtLending, string) {
	t.Helper()
	return badDebtRouteWith(t, volumeUSD, nil)
}

// badDebtRouteWith is badDebtRoute with the operator's source exclusions.
func badDebtRouteWith(t *testing.T, volumeUSD string, excl []timescale.SourceExclusion) (map[string]any, *stubBadDebtLending, string) {
	t.Helper()
	asset, backstop := mkCStrkey(t, 4), mkCStrkey(t, 9)
	now := time.Now().UTC()
	ev := func(kind, d string, ledger uint32) timescale.LendingBadDebtEvent {
		return timescale.LendingBadDebtEvent{Kind: kind, Asset: asset, DTokens: d, Ledger: ledger, TxHash: liqTxA, Ts: now.Add(-30 * time.Minute)}
	}
	lr := &stubBadDebtLending{stubLendingReader: stubLendingReader{assets: []string{asset}}, events: []timescale.LendingBadDebtEvent{
		ev(timescale.BadDebtKindMoved, "20000000", 101), ev(timescale.BadDebtKindFill, "5000000", 102), ev(timescale.BadDebtKindDefaulted, "10000000", 103),
	}}
	ex := badDebtExplorer{
		stubExplorerReader: &stubExplorerReader{
			poolInstance: &clickhouse.PoolInstance{Backstop: backstop},
			reserves:     []clickhouse.BlendReserveState{{Asset: asset, Data: blend.ReserveData{Version: blend.PoolV2, DRate: big.NewInt(1_000_000_000_000)}}},
		},
		pos: clickhouse.LendingAccountPositions{Readable: true, Borrow: map[string]*big.Int{asset: big.NewInt(5_000_000)}, Ledger: 103},
	}
	pts := []v1.HistoryPoint{{Bucket: now.Add(-2 * time.Hour).Truncate(time.Hour), VWAP: "0.50", VolumeUSD: &volumeUSD, Sources: []string{"soroswap"}}}
	opts := v1.Options{
		Explorer: ex, Lending: lr, History: &stubHistoryReader{points: pts, twapPoints: pts}, VerifiedCurrencies: newTestCatalogue(t),
		Substance: pricingguard.NewSubstanceGate(thinStore{thin: map[string]bool{}}, pricingguard.SubstanceGateOptions{}),
	}
	if excl != nil {
		opts.Exclusions = pricingguard.NewSourceExclusionGate(exclTable{rows: excl}, pricingguard.SourceExclusionGateOptions{})
	}
	ts := startHTTPTest(t, v1.New(opts).Handler())
	status, body := fetch(t, ts.URL+"/v1/lending/pools/"+liqTestPool+"/bad-debt")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	data, _ := decodeMap(t, body)["data"].(map[string]any)
	return data, lr, backstop
}

func badDebtReserve(t *testing.T, data map[string]any) map[string]any {
	t.Helper()
	res, _ := data["reserves"].([]any)
	if len(res) != 1 {
		t.Fatalf("reserves = %v, want 1", data["reserves"])
	}
	r, _ := res[0].(map[string]any)
	return r
}

// TestLendingPoolBadDebt_ReconcilesAndValuesAtEvent: the backstop comes from the pool's instance storage, each part is valued at
// its event, outstanding at the current rate and price, and the d-token identity holds.
func TestLendingPoolBadDebt_ReconcilesAndValuesAtEvent(t *testing.T) {
	data, lr, backstop := badDebtRoute(t, "1000")
	if lr.backstop != backstop || data["backstop"] != backstop || data["protocol"] != "blend" {
		t.Fatalf("backstop read %q served %v, want %q", lr.backstop, data["backstop"], backstop)
	}
	r := badDebtReserve(t, data)
	moved, _ := r["moved_to_backstop"].(map[string]any)
	out, _ := r["outstanding"].(map[string]any)
	if moved["d_tokens"] != "20000000" || moved["usd"] != "1.00" || out["usd"] != "0.25" || out["underlying"] != "5000000" {
		t.Errorf("moved %v outstanding %v", moved, out)
	}
	if r["reconciled"] != true || r["residual_d_tokens"] != "0" {
		t.Errorf("reconciled %v residual %v", r["reconciled"], r["residual_d_tokens"])
	}
	tot, _ := data["totals_usd"].(map[string]any)["covered_by_backstop"].(map[string]any)
	if tot["usd"] != "0.25" || tot["lower_bound"] != false {
		t.Errorf("covered total %v", tot)
	}
}

// TestLendingPoolBadDebt_ThinPriceIsNotValued: the bucket that would price the events traded under the substance floor, so no
// part is valued and the pool total names the reserve instead of serving 0.
func TestLendingPoolBadDebt_ThinPriceIsNotValued(t *testing.T) {
	data, _, _ := badDebtRoute(t, "999.99")
	moved, _ := badDebtReserve(t, data)["moved_to_backstop"].(map[string]any)
	if moved["usd"] != nil || moved["usd_reason"] != "price_thin_at_fill" || moved["d_tokens"] != "20000000" {
		t.Fatalf("moved %v, want d-tokens with usd null (price_thin_at_fill)", moved)
	}
	tot, _ := data["totals_usd"].(map[string]any)["moved_to_backstop"].(map[string]any)
	if tot["usd"] != nil || len(tot["excluded"].([]any)) != 1 {
		t.Errorf("moved total %v", tot)
	}
}

// TestLendingPoolBadDebt_ExcludedSourceDoesNotValueEvent: the bucket that would price the events came from an excluded source,
// so the moved part keeps its d-tokens and is unvalued (price_withheld), never valued from an older bucket.
func TestLendingPoolBadDebt_ExcludedSourceDoesNotValueEvent(t *testing.T) {
	bucket := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	excl := []timescale.SourceExclusion{{ID: 1, Source: "soroswap", From: bucket.Add(-time.Hour), Until: bucket.Add(2 * time.Hour)}}
	data, _, _ := badDebtRouteWith(t, "1000", excl)
	moved, _ := badDebtReserve(t, data)["moved_to_backstop"].(map[string]any)
	if moved["usd"] != nil || moved["usd_reason"] != "price_withheld" || moved["d_tokens"] != "20000000" {
		t.Fatalf("moved %v, want d-tokens with usd null (price_withheld)", moved)
	}
}

func TestLendingPoolBadDebt_UnknownPoolIs404(t *testing.T) {
	unknown := blend.PoolVersionUnknown
	ts := startHTTPTest(t, v1.New(v1.Options{Lending: &stubBadDebtLending{stubLendingReader: stubLendingReader{version: &unknown}}}).Handler())
	if status, body := fetch(t, ts.URL+"/v1/lending/pools/"+liqTestPool+"/bad-debt"); status != http.StatusNotFound {
		t.Fatalf("status %d: %s", status, body)
	}
}
