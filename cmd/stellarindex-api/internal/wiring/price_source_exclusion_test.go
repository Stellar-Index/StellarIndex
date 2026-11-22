package wiring

import (
	"context"
	"database/sql"
	"errors"
	"math/big"
	"reflect"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// exclPriceStore is a PriceStore with canned rows, so the real StorePriceReader runs without a database.
type exclPriceStore struct {
	latest   *timescale.Vwap1mRow
	trailing []timescale.Vwap1mRow
	recent   []timescale.Vwap1mRow
	trades   []canonical.Trade
}

func (s exclPriceStore) LatestClosedVWAP1mForPair(context.Context, canonical.Pair) (timescale.Vwap1mRow, error) {
	if s.latest == nil {
		return timescale.Vwap1mRow{}, sql.ErrNoRows
	}
	return *s.latest, nil
}

func (s exclPriceStore) RecentClosedVWAP1mCombined(context.Context, canonical.Pair, int) ([]timescale.Vwap1mRow, error) {
	return s.trailing, nil
}

func (s exclPriceStore) RecentClosedVWAP1mForPair(context.Context, canonical.Pair, int) ([]timescale.Vwap1mRow, error) {
	return s.recent, nil
}

func (exclPriceStore) RecentClosedVWAP1mExists(context.Context, canonical.Pair) (bool, error) {
	return true, nil
}

func (s exclPriceStore) LatestTradesForPair(context.Context, canonical.Pair, int, time.Time) ([]canonical.Trade, error) {
	return s.trades, nil
}

type exclRows struct {
	rows []timescale.SourceExclusion
	err  error
}

func (e exclRows) UnliftedSourceExclusions(context.Context) ([]timescale.SourceExclusion, error) {
	return e.rows, e.err
}

var exclNow = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func exclRow(minutesAgo int, vwap string, sources ...string) timescale.Vwap1mRow {
	return timescale.Vwap1mRow{
		Bucket: exclNow.Add(-time.Duration(minutesAgo+1) * time.Minute), VWAP: vwap, TradeCount: 9, Sources: sources,
	}
}

func exclTrailing() []timescale.Vwap1mRow {
	var out []timescale.Vwap1mRow
	for i := 1; i < 16; i++ {
		out = append(out, exclRow(i, "1.0", "sdex"))
	}
	return out
}

func krakenExcluded() []timescale.SourceExclusion {
	return []timescale.SourceExclusion{{ID: 1, Source: "kraken", From: exclNow.Add(-time.Hour)}}
}

func exclGateOver(e exclRows) *pricingguard.SourceExclusionGate {
	return pricingguard.NewSourceExclusionGate(e, pricingguard.SourceExclusionGateOptions{})
}

type latestPriceResult struct {
	snap    v1.PriceSnapshot
	sources []string
	stale   bool
	err     error
}

func readLatest(t *testing.T, store exclPriceStore, gate *pricingguard.SourceExclusionGate) latestPriceResult {
	t.Helper()
	r := StorePriceReader{S: store, Now: func() time.Time { return exclNow }, Exclusions: gate}
	snap, sources, stale, err := r.LatestPrice(context.Background(), canonical.NativeAsset(), mustFiatUSD(t))
	return latestPriceResult{snap, sources, stale, err}
}

func wantWithheld(t *testing.T, err error, reason v1.PriceWithheldReason) {
	t.Helper()
	if !errors.Is(err, v1.ErrPriceWithheld) {
		t.Fatalf("err = %v, want ErrPriceWithheld", err)
	}
	if got := v1.PriceWithheldReasonOf(err); got != reason {
		t.Fatalf("reason = %q, want %q", got, reason)
	}
}

func TestStorePriceReader_SourceExclusion(t *testing.T) {
	latest := exclRow(0, "1.1", "kraken", "sdex")
	store := exclPriceStore{latest: &latest, trailing: exclTrailing()}
	baseline := readLatest(t, store, nil)
	if baseline.err != nil || baseline.snap.Price == "" {
		t.Fatalf("no-gate read: %+v", baseline)
	}

	t.Run("empty exclusion set is identical to no gate", func(t *testing.T) {
		if got := readLatest(t, store, exclGateOver(exclRows{})); !reflect.DeepEqual(got, baseline) {
			t.Fatalf("got %+v, want %+v", got, baseline)
		}
	})
	t.Run("excluded source contributing is withheld", func(t *testing.T) {
		wantWithheld(t, readLatest(t, store, exclGateOver(exclRows{rows: krakenExcluded()})).err, v1.PriceWithheldSourceExcluded)
	})
	t.Run("excluded source not contributing is unchanged", func(t *testing.T) {
		other := []timescale.SourceExclusion{{Source: "binance", From: exclNow.Add(-time.Hour)}}
		if got := readLatest(t, store, exclGateOver(exclRows{rows: other})); !reflect.DeepEqual(got, baseline) {
			t.Fatalf("got %+v, want %+v", got, baseline)
		}
	})
	t.Run("unreadable exclusions withhold, unattributed", func(t *testing.T) {
		wantWithheld(t, readLatest(t, store, exclGateOver(exclRows{err: errors.New("db down")})).err, v1.PriceWithheldUnattributed)
	})
}

// The last-trade fallback serves one source's trade; an excluded one must not stand in for the missing bucket.
func TestStorePriceReader_LastTradeFallbackSourceExclusion(t *testing.T) {
	issuer := "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	base, err := canonical.NewClassicAsset("USDC", issuer)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(base, canonical.NativeAsset())
	if err != nil {
		t.Fatal(err)
	}
	trade := canonical.Trade{
		Source: "kraken", Timestamp: exclNow.Add(-5 * time.Minute), Pair: pair,
		BaseAmount: canonical.NewAmount(big.NewInt(10_000_000)), QuoteAmount: canonical.NewAmount(big.NewInt(20_000_000)),
	}
	store := exclPriceStore{trades: []canonical.Trade{trade}}
	read := func(gate *pricingguard.SourceExclusionGate) error {
		r := StorePriceReader{S: store, Now: func() time.Time { return exclNow }, Exclusions: gate}
		_, _, _, err := r.LatestPrice(context.Background(), base, canonical.NativeAsset())
		return err
	}
	if err := read(exclGateOver(exclRows{})); err != nil {
		t.Fatalf("clean fallback: %v", err)
	}
	wantWithheld(t, read(exclGateOver(exclRows{rows: krakenExcluded()})), v1.PriceWithheldSourceExcluded)
}

// SEP-40 series: an excluded source's minute is dropped, the clean ones are kept, and an unreadable set withholds.
func TestStorePriceReader_RecentClosedSnapshotsSourceExclusion(t *testing.T) {
	recent := []timescale.Vwap1mRow{exclRow(0, "1.0", "kraken", "sdex"), exclRow(1, "1.0", "sdex"), exclRow(2, "1.0", "sdex")}
	read := func(gate *pricingguard.SourceExclusionGate) ([]v1.PriceSnapshot, error) {
		r := StorePriceReader{S: exclPriceStore{recent: recent}, Exclusions: gate}
		return r.RecentClosedSnapshots(context.Background(), canonical.NativeAsset(), mustFiatUSD(t), 3)
	}
	clean, err := read(nil)
	if err != nil {
		t.Fatal(err)
	}
	if same, err := read(exclGateOver(exclRows{})); err != nil || !reflect.DeepEqual(same, clean) {
		t.Fatalf("empty set: %v %v, want %v", same, err, clean)
	}
	got, err := read(exclGateOver(exclRows{rows: krakenExcluded()}))
	if err != nil {
		t.Fatal(err)
	}
	excluded := v1.VWAP1mToSnapshot(canonical.NativeAsset().String(), mustFiatUSD(t).String(), recent[0].VWAP, recent[0].Bucket)
	for _, s := range got {
		if reflect.DeepEqual(s, excluded) {
			t.Fatalf("excluded bucket %v served: %+v", recent[0].Bucket, got)
		}
	}
	if len(got) != len(clean)-1 {
		t.Fatalf("served %d buckets, want %d", len(got), len(clean)-1)
	}
	_, err = read(exclGateOver(exclRows{err: errors.New("db down")}))
	wantWithheld(t, err, v1.PriceWithheldUnattributed)
}

type exclGlobalStore struct {
	exclPriceStore
	agg []canonical.OracleUpdate
}

func (s exclGlobalStore) LatestAggregatorPricesForPair(context.Context, canonical.Asset, canonical.Asset, []string) ([]canonical.OracleUpdate, error) {
	return s.agg, nil
}

// The asset headline: an excluded VWAP bucket is no data, and the aggregator tier is recomputed without the excluded source.
func TestGlobalPriceReader_SourceExclusion(t *testing.T) {
	latest := exclRow(0, "1.1", "kraken", "sdex")
	agg := []canonical.OracleUpdate{{Source: "kraken", Timestamp: exclNow}, {Source: "coingecko", Timestamp: exclNow}}
	store := exclGlobalStore{exclPriceStore: exclPriceStore{latest: &latest, trailing: exclTrailing()}, agg: agg}
	reader := func(gate *pricingguard.SourceExclusionGate) GlobalPriceReader {
		return GlobalPriceReader{S: store, PKPairFor: canonical.NewPair, Exclusions: gate}
	}
	base, quote := canonical.NativeAsset(), mustFiatUSD(t)
	ctx := context.Background()

	if _, _, _, _, ok, _ := reader(exclGateOver(exclRows{})).LatestVWAP(ctx, base, quote); !ok {
		t.Fatal("empty set: headline VWAP not served")
	}
	if rows, _ := reader(exclGateOver(exclRows{})).LatestAggregatorPrices(ctx, base, quote, nil); !reflect.DeepEqual(rows, agg) {
		t.Fatalf("empty set: aggregator rows %v, want %v", rows, agg)
	}

	excl := reader(exclGateOver(exclRows{rows: krakenExcluded()}))
	if v, _, _, _, ok, err := excl.LatestVWAP(ctx, base, quote); ok || err != nil {
		t.Fatalf("excluded bucket served as headline %q (err %v)", v, err)
	}
	rows, err := excl.LatestAggregatorPrices(ctx, base, quote, nil)
	if err != nil || len(rows) != 1 || rows[0].Source != "coingecko" {
		t.Fatalf("aggregator rows = %v (err %v), want only coingecko", rows, err)
	}

	aggAt := func(source string, price int64) canonical.OracleUpdate {
		return canonical.OracleUpdate{Source: source, Timestamp: exclNow, Price: canonical.NewAmount(big.NewInt(price)), Decimals: 8}
	}
	// Excluding one of three sources switches the tier's outlier filter off: survivors the full set's band rejects are no data,
	// survivors inside it are served.
	for _, tc := range []struct {
		name string
		agg  []canonical.OracleUpdate
		want int
	}{
		{"survivors disagree", []canonical.OracleUpdate{aggAt("kraken", 1000), aggAt("coingecko", 100), aggAt("cmc", 1000)}, 0},
		{"survivors agree", []canonical.OracleUpdate{aggAt("kraken", 100), aggAt("coingecko", 1000), aggAt("cmc", 1001)}, 2},
	} {
		s := exclGlobalStore{exclPriceStore: store.exclPriceStore, agg: tc.agg}
		g := GlobalPriceReader{S: s, PKPairFor: canonical.NewPair, Exclusions: exclGateOver(exclRows{rows: krakenExcluded()})}
		if rows, err := g.LatestAggregatorPrices(ctx, base, quote, nil); err != nil || len(rows) != tc.want {
			t.Errorf("%s: aggregator rows %v (err %v), want %d", tc.name, rows, err, tc.want)
		}
	}

	unread := reader(exclGateOver(exclRows{err: errors.New("db down")}))
	if _, _, _, _, ok, _ := unread.LatestVWAP(ctx, base, quote); ok {
		t.Fatal("unreadable set: headline VWAP served")
	}
	if rows, _ := unread.LatestAggregatorPrices(ctx, base, quote, nil); len(rows) != 0 {
		t.Fatalf("unreadable set: aggregator rows %v, want none", rows)
	}
}
