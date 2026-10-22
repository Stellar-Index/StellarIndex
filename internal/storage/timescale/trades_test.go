package timescale

import (
	"context"
	"fmt"
	"math/big"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestSortTradesByConflictKey_FullKeyOrder: concurrent batch inserts must lock rows in the same order or
// deadlock (40P01), which needs the full (source, ledger, tx_hash, op_index, ts) key. Shuffled CEX rows
// tying on four columns must sort identically, so ts is a real tiebreaker.
func TestSortTradesByConflictKey_FullKeyOrder(t *testing.T) {
	base := time.Date(2026, 7, 8, 18, 12, 0, 0, time.UTC)
	mk := func(source string, ledger uint32, txHash string, opIndex uint32, tsOffsetSeconds int) canonical.Trade {
		return canonical.Trade{
			Source:    source,
			Ledger:    ledger,
			TxHash:    txHash,
			OpIndex:   opIndex,
			Timestamp: base.Add(time.Duration(tsOffsetSeconds) * time.Second),
		}
	}

	// Rows that tie on (source, ledger, tx_hash, op_index), the CEX-shaped collision, but differ on ts. Includes
	// several sources and tx_hash values so the test also exercises the prefix ordering.
	want := []canonical.Trade{
		mk("kraken", 0, "aaaa", 0, 0),
		mk("kraken", 0, "aaaa", 0, 1),
		mk("kraken", 0, "aaaa", 0, 2),
		mk("kraken", 0, "bbbb", 0, 0),
		mk("kraken", 0, "bbbb", 0, 5),
		mk("sdex", 100, "cccc", 0, 0),
		mk("sdex", 100, "cccc", 1, 0),
		mk("sdex", 200, "cccc", 0, 0),
	}

	keyOf := func(tr canonical.Trade) [5]string {
		return [5]string{tr.Source, itoa(tr.Ledger), tr.TxHash, itoa(tr.OpIndex), tr.Timestamp.String()}
	}
	wantKeys := make([][5]string, len(want))
	for i, tr := range want {
		wantKeys[i] = keyOf(tr)
	}

	rng := rand.New(rand.NewSource(1))
	for trial := range 50 {
		shuffled := append([]canonical.Trade(nil), want...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		sortTradesByConflictKey(shuffled)

		if len(shuffled) != len(wantKeys) {
			t.Fatalf("trial %d: length changed: got %d, want %d", trial, len(shuffled), len(wantKeys))
		}
		for i, tr := range shuffled {
			if got := keyOf(tr); got != wantKeys[i] {
				t.Fatalf("trial %d: position %d: got key %v, want %v (non-deterministic sort — ts tiebreak missing)",
					trial, i, got, wantKeys[i])
			}
		}
	}
}

func itoa(u uint32) string {
	return fmt.Sprintf("%d", u)
}

// One multi-row INSERT may carry at most 65,535 bind parameters, including the 2 ts bounds; every row must land in exactly one sub-batch.
func TestTradeInsertChunkBounds(t *testing.T) {
	if tradeInsertMaxRows*tradeColsPerRow+2 > 65535 {
		t.Fatalf("tradeInsertMaxRows=%d × %d params + 2 ts bounds exceeds the 65,535-parameter ceiling", tradeInsertMaxRows, tradeColsPerRow)
	}
	for _, n := range []int{0, 1, tradeInsertMaxRows - 1, tradeInsertMaxRows, tradeInsertMaxRows + 1, 100_000} {
		bounds := tradeInsertChunkBounds(n)
		covered, prevEnd := 0, 0
		for _, b := range bounds {
			if b[0] != prevEnd || b[1] <= b[0] || b[1]-b[0] > tradeInsertMaxRows {
				t.Fatalf("n=%d: bad chunk %v after end %d", n, b, prevEnd)
			}
			covered += b[1] - b[0]
			prevEnd = b[1]
		}
		if covered != n {
			t.Errorf("n=%d: chunks cover %d rows", n, covered)
		}
		if want := (100_000 + tradeInsertMaxRows - 1) / tradeInsertMaxRows; n == 100_000 && len(bounds) != want {
			t.Errorf("100,000 rows → %d chunks, want %d", len(bounds), want)
		}
	}
}

func TestUsdPopulatedLabel(t *testing.T) {
	const (
		issuerA = "GBGRBCUB6L7LH4JQ6EPDP7REH2DDACMCUSCQNBA3CULWJQJHOS6VKUMH"
		issuerB = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		sac     = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	)
	classic := func(code, issuer string) canonical.Asset {
		return canonical.Asset{Type: canonical.AssetClassic, Code: code, Issuer: issuer}
	}
	tests := []struct {
		name      string
		pair      canonical.Pair
		populated bool
		refused   bool
		thin      bool
		want      string
	}{
		{"priced same-issuer pair stays yes", canonical.Pair{Base: classic("AAA", issuerA), Quote: classic("BBB", issuerA)}, true, false, false, "yes"},
		{"priced cross-issuer pair", canonical.Pair{Base: classic("AAA", issuerA), Quote: classic("USDC", issuerB)}, true, false, false, "yes"},
		{"unpriced same-issuer pair is unroutable", canonical.Pair{Base: classic("AAA", issuerA), Quote: classic("BBB", issuerA)}, false, false, false, "unroutable"},
		{"unpriced cross-issuer pair is a gap", canonical.Pair{Base: classic("AAA", issuerA), Quote: classic("BBB", issuerB)}, false, false, false, "no"},
		{"unpriced native leg is a gap", canonical.Pair{Base: classic("AAA", issuerA), Quote: canonical.NativeAsset()}, false, false, false, "no"},
		{"unpriced soroban legs are a gap", canonical.Pair{
			Base:  canonical.Asset{Type: canonical.AssetSoroban, ContractID: sac},
			Quote: canonical.Asset{Type: canonical.AssetSoroban, ContractID: sac},
		}, false, false, false, "no"},
		{"thin refusal on a cross-issuer pair", canonical.Pair{Base: classic("AAA", issuerA), Quote: classic("BBB", issuerB)}, false, false, true, "thin"},
		{"unroutable wins over thin", canonical.Pair{Base: classic("AAA", issuerA), Quote: classic("BBB", issuerA)}, false, false, true, "unroutable"},
		{"populated wins over thin", canonical.Pair{Base: classic("AAA", issuerA), Quote: classic("BBB", issuerB)}, true, false, true, "yes"},
		{"empty issuers never match", canonical.Pair{Base: classic("AAA", ""), Quote: classic("BBB", "")}, false, false, false, "no"},
		{"refused on a cross-issuer pair", canonical.Pair{Base: classic("AAA", issuerA), Quote: classic("BBB", issuerB)}, false, true, false, "refused"},
		{"refused wins over thin", canonical.Pair{Base: classic("AAA", issuerA), Quote: classic("BBB", issuerB)}, false, true, true, "refused"},
		{"refused wins over unroutable", canonical.Pair{Base: classic("AAA", issuerA), Quote: classic("BBB", issuerA)}, false, true, false, "refused"},
		{"populated wins over refused", canonical.Pair{Base: classic("AAA", issuerA), Quote: classic("BBB", issuerB)}, true, true, false, "yes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := usdPopulatedLabel(tc.pair, tc.populated, tc.refused, tc.thin); got != tc.want {
				t.Errorf("usdPopulatedLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRawTradeReads_ExcludeOffChainSources pins the on-chain-only rule of /v1/history and /v1/observations in
// the query itself, so a keyset page is filled from on-chain rows rather than thinned after the fetch.
func TestRawTradeReads_ExcludeOffChainSources(t *testing.T) {
	pair := dirAQUAUSDCPair(t)
	now := time.Now()

	reads := []struct {
		name string
		argN int
		run  func(*Store) error
	}{
		{"LatestTradePerSource", 4, func(s *Store) error {
			_, err := s.LatestTradePerSource(context.Background(), pair, "")
			return err
		}},
		{"TradesInRangeAfter", 11, func(s *Store) error {
			_, err := s.TradesInRangeAfter(context.Background(), pair,
				now.Add(-time.Hour), now, time.Time{}, 0, "", "", 0, 10)
			return err
		}},
	}
	for _, r := range reads {
		t.Run(r.name, func(t *testing.T) {
			store, conn := newScriptedStore(t, scriptedResult{cols: latestTradeCols})
			if err := r.run(store); err != nil {
				t.Fatalf("%s: %v", r.name, err)
			}
			stmt := conn.only(t)
			if !strings.Contains(stmt.sql, "source <> ALL(string_to_array($") {
				t.Errorf("%s SQL has no off-chain source exclusion:\n%s", r.name, stmt.sql)
			}
			list, _ := stmt.arg(t, r.argN).(string)
			set := map[string]bool{}
			for _, n := range strings.Split(list, ",") {
				set[n] = true
			}
			for _, cex := range []string{"binance", "kraken", "bitstamp", "coinbase"} {
				if !set[cex] {
					t.Errorf("%s: exchange %q not excluded (arg $%d = %q)", r.name, cex, r.argN, list)
				}
			}
			for _, onchain := range []string{"sdex", "soroswap", "aquarius"} {
				if set[onchain] {
					t.Errorf("%s: on-chain source %q excluded", r.name, onchain)
				}
			}
		})
	}
}

// TestIsDexUnitRatioTrade covers the pure predicate in isolation —
// the Phoenix incident signature (on-chain, base_amount ==
// quote_amount, both nonzero) vs. the cases it must NOT flag.
func TestIsDexUnitRatioTrade(t *testing.T) {
	tests := []struct {
		name   string
		ledger uint32
		base   canonical.Amount
		quote  canonical.Amount
		want   bool
	}{
		{
			name:   "on-chain unit-ratio trade — the Phoenix incident signature",
			ledger: 1000,
			base:   canonical.NewAmount(big.NewInt(500)),
			quote:  canonical.NewAmount(big.NewInt(500)),
			want:   true,
		},
		{
			name:   "on-chain normal trade — base != quote",
			ledger: 1000,
			base:   canonical.NewAmount(big.NewInt(500)),
			quote:  canonical.NewAmount(big.NewInt(1234)),
			want:   false,
		},
		{
			name:   "off-chain (CEX/FX) trade — ledger==0 excluded even if base==quote",
			ledger: 0,
			base:   canonical.NewAmount(big.NewInt(500)),
			quote:  canonical.NewAmount(big.NewInt(500)),
			want:   false,
		},
		{
			name:   "zero amounts — defence-in-depth, never flagged even though equal",
			ledger: 1000,
			base:   canonical.NewAmount(big.NewInt(0)),
			quote:  canonical.NewAmount(big.NewInt(0)),
			want:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDexUnitRatioTrade(tc.ledger, tc.base, tc.quote); got != tc.want {
				t.Errorf("isDexUnitRatioTrade(ledger=%d, base=%s, quote=%s) = %v; want %v",
					tc.ledger, tc.base.String(), tc.quote.String(), got, tc.want)
			}
		})
	}
}

// TestRecordDexTradeUnitRatio_MetricIncrements exercises the production call site against the real
// obs.DexTradeUnitRatioTotal counter without a database. Each case uses a distinct fake source label so
// counter state cannot leak between cases.
func TestRecordDexTradeUnitRatio_MetricIncrements(t *testing.T) {
	tests := []struct {
		name        string
		source      string
		ledger      uint32
		base, quote int64
		wantBump    bool
	}{
		{
			name:     "on-chain base==quote trade bumps the counter",
			source:   "test-unit-ratio-onchain-hit",
			ledger:   12345,
			base:     500,
			quote:    500,
			wantBump: true,
		},
		{
			name:     "CEX trade (ledger==0) does NOT bump even though base==quote",
			source:   "test-unit-ratio-cex-noop",
			ledger:   0,
			base:     500,
			quote:    500,
			wantBump: false,
		},
		{
			name:     "normal on-chain trade (base != quote) does NOT bump",
			source:   "test-unit-ratio-normal-noop",
			ledger:   12345,
			base:     500,
			quote:    1234,
			wantBump: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := testutil.ToFloat64(obs.DexTradeUnitRatioTotal.WithLabelValues(tc.source))

			trade := canonical.Trade{
				Source:      tc.source,
				Ledger:      tc.ledger,
				TxHash:      "deadbeef",
				BaseAmount:  canonical.NewAmount(big.NewInt(tc.base)),
				QuoteAmount: canonical.NewAmount(big.NewInt(tc.quote)),
			}
			recordDexTradeUnitRatio(trade)

			after := testutil.ToFloat64(obs.DexTradeUnitRatioTotal.WithLabelValues(tc.source))
			gotBump := after > before
			if gotBump != tc.wantBump {
				t.Errorf("recordDexTradeUnitRatio bumped=%v (before=%v after=%v); want bumped=%v",
					gotBump, before, after, tc.wantBump)
			}
		})
	}
}

// osztAmt is a terse canonical.Amount for the tables below.
func osztAmt(n int64) canonical.Amount { return canonical.NewAmount(big.NewInt(n)) }

// osztPair builds the native→USDC pair every case in this file trades on.
func osztPair(t *testing.T) canonical.Pair {
	t.Helper()
	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return pair
}

// osztHash returns a valid 64-char lowercase-hex tx hash seeded by n.
func osztHash(n uint32) string { return fmt.Sprintf("%064x", n) }

// TestIsOneSideZeroFill pins the predicate separating the benign SDEX rounding artifact
// (exactly one leg == 0) from every other Validate failure: a false positive would silence a
// real decoder bug, a false negative would re-fire the spurious insert-error alert.
func TestIsOneSideZeroFill(t *testing.T) {
	t.Parallel()
	pair := osztPair(t)
	mk := func(base, quote int64) canonical.Trade {
		return canonical.Trade{Pair: pair, BaseAmount: osztAmt(base), QuoteAmount: osztAmt(quote)}
	}
	cases := []struct {
		name string
		t    canonical.Trade
		want bool
	}{
		{"quote leg zero", mk(100, 0), true},
		{"base leg zero", mk(0, 100), true},
		{"both legs positive", mk(100, 25), false},
		{"both legs zero", mk(0, 0), false},
		{"negative base leg", mk(-100, 25), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsOneSideZeroFill(tc.t); got != tc.want {
				t.Errorf("IsOneSideZeroFill(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestFilterStorableTrades: valid rows pass in order, a one-side-zero fill is admitted and counted on
// TradesZeroLegAdmittedTotal, and any other Validate failure is dropped and counted in
// SourceInsertErrorsTotal, so one bad row cannot sink the batch INSERT.
func TestFilterStorableTrades(t *testing.T) {
	pair := osztPair(t)
	ts := time.Now().UTC().Add(-time.Hour)
	valid := func(txHash string) canonical.Trade {
		return canonical.Trade{
			Source: "sdex", Ledger: 60_000_000, TxHash: txHash, OpIndex: 0,
			Timestamp: ts, Pair: pair,
			BaseAmount: osztAmt(1_000), QuoteAmount: osztAmt(25),
		}
	}
	valid1 := valid(osztHash(1))
	valid2 := valid(osztHash(2))

	oneSideZero := valid(osztHash(3))
	oneSideZero.QuoteAmount = osztAmt(0) // quote leg rounded to 0 — a real fill, stored unpriceable

	badHash := valid("not-a-hash") // Validate fails for a NON-amount reason → must stay loud

	s := &Store{}
	const src, kind = "sdex", "trade"
	errBefore := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues(src, kind))
	zeroBefore := testutil.ToFloat64(obs.TradesZeroLegAdmittedTotal.WithLabelValues(src))

	got := s.filterStorableTrades([]canonical.Trade{valid1, oneSideZero, valid2, badHash})

	if len(got) != 3 {
		t.Fatalf("storable count = %d, want 3 (two valid trades + the one-side-zero fill; only the bad-hash row is filtered)", len(got))
	}
	if got[0].TxHash != valid1.TxHash || got[1].TxHash != oneSideZero.TxHash || got[2].TxHash != valid2.TxHash {
		t.Errorf("storable order/content = [%s, %s, %s], want [%s, %s, %s]",
			got[0].TxHash, got[1].TxHash, got[2].TxHash, valid1.TxHash, oneSideZero.TxHash, valid2.TxHash)
	}

	errAfter := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues(src, kind))
	if delta := errAfter - errBefore; delta != 1 {
		t.Errorf("SourceInsertErrorsTotal{sdex,trade} delta = %v, want 1 "+
			"(ONLY the malformed-tx_hash row is an error; the one-side-zero fill is admitted)", delta)
	}
	zeroAfter := testutil.ToFloat64(obs.TradesZeroLegAdmittedTotal.WithLabelValues(src))
	if delta := zeroAfter - zeroBefore; delta != 1 {
		t.Errorf("TradesZeroLegAdmittedTotal{sdex} delta = %v, want 1 (exactly the one-side-zero fill)", delta)
	}
}

// TestFilterStorableTrades_BothZeroStaysLoud pins that the relaxation is
// exactly one zero leg: a both-zero row still fails Validate, is dropped and
// counted as an insert error, and never reaches TradesZeroLegAdmittedTotal.
func TestFilterStorableTrades_BothZeroStaysLoud(t *testing.T) {
	pair := osztPair(t)
	bothZero := canonical.Trade{
		Source: "sdex", Ledger: 60_000_000, TxHash: osztHash(4), OpIndex: 0,
		Timestamp: time.Now().UTC().Add(-time.Hour), Pair: pair,
		BaseAmount: osztAmt(0), QuoteAmount: osztAmt(0),
	}
	s := &Store{}
	errBefore := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues("sdex", "trade"))
	zeroBefore := testutil.ToFloat64(obs.TradesZeroLegAdmittedTotal.WithLabelValues("sdex"))

	got := s.filterStorableTrades([]canonical.Trade{bothZero})

	if len(got) != 0 {
		t.Fatalf("storable count = %d, want 0 (both-zero is not a trade)", len(got))
	}
	if delta := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues("sdex", "trade")) - errBefore; delta != 1 {
		t.Errorf("SourceInsertErrorsTotal{sdex,trade} delta = %v, want 1", delta)
	}
	if delta := testutil.ToFloat64(obs.TradesZeroLegAdmittedTotal.WithLabelValues("sdex")) - zeroBefore; delta != 0 {
		t.Errorf("TradesZeroLegAdmittedTotal{sdex} delta = %v, want 0", delta)
	}
}

// TestFilterStorableTrades_AllValidFastPath proves the common case returns the
// input untouched with no error-metric noise, and that a one-side-zero fill in
// an all-valid batch is still counted on the fast path.
func TestFilterStorableTrades_AllValidFastPath(t *testing.T) {
	pair := osztPair(t)
	ts := time.Now().UTC().Add(-time.Hour)
	mk := func(txHash string) canonical.Trade {
		return canonical.Trade{
			Source: "sdex", Ledger: 60_000_000, TxHash: txHash, OpIndex: 0,
			Timestamp: ts, Pair: pair,
			BaseAmount: osztAmt(1_000), QuoteAmount: osztAmt(25),
		}
	}
	zeroBase := mk(osztHash(12))
	zeroBase.BaseAmount = osztAmt(0)
	in := []canonical.Trade{mk(osztHash(10)), mk(osztHash(11)), zeroBase}
	s := &Store{}

	errBefore := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues("sdex", "trade"))
	zeroBefore := testutil.ToFloat64(obs.TradesZeroLegAdmittedTotal.WithLabelValues("sdex"))
	got := s.filterStorableTrades(in)
	errAfter := testutil.ToFloat64(obs.SourceInsertErrorsTotal.WithLabelValues("sdex", "trade"))
	zeroAfter := testutil.ToFloat64(obs.TradesZeroLegAdmittedTotal.WithLabelValues("sdex"))

	if len(got) != len(in) {
		t.Fatalf("all-valid storable count = %d, want %d", len(got), len(in))
	}
	if errAfter != errBefore {
		t.Errorf("all-valid batch bumped SourceInsertErrorsTotal by %v, want 0", errAfter-errBefore)
	}
	if delta := zeroAfter - zeroBefore; delta != 1 {
		t.Errorf("TradesZeroLegAdmittedTotal{sdex} delta = %v, want 1 (the zero-base fill on the fast path)", delta)
	}
}

// TestDex24hBucketWindowMatchesSourceVolumeHistory pins that the readers of source_volume_1h's 24h window
// select the SAME hourly buckets, or the served parity note is false. Flooring with date_trunc gives a
// >= 25-bucket window while `bucket > now() - $2::interval` gives 24.
func TestDex24hBucketWindowMatchesSourceVolumeHistory(t *testing.T) {
	kpi := dexWindowKPIQuery(1)
	series := dexActivitySeriesQuery(1)
	history := sourceVolumeHistoryQuery()

	for _, bad := range []string{"date_trunc(", ">= NOW() - $", ">= now() - $"} {
		if strings.Contains(history, bad) {
			t.Errorf("sourceVolumeHistoryQuery must not widen its window with %q — it must select the same buckets as the bespoke 24h readers:\n%s", bad, history)
		}
	}

	wantKPI := "bucket > now() - $2::interval"
	wantHistory := "bucket > now() - $1::interval"
	if !strings.Contains(kpi, wantKPI) {
		t.Errorf("dexWindowKPIQuery(1) must bind the shared strict bucket predicate %q:\n%s", wantKPI, kpi)
	}
	if !strings.Contains(series, wantKPI) {
		t.Errorf("dexActivitySeriesQuery(1) must bind the shared strict bucket predicate %q:\n%s", wantKPI, series)
	}
	if !strings.Contains(history, wantHistory) {
		t.Errorf("sourceVolumeHistoryQuery must bind the shared strict bucket predicate %q — got a different window than the bespoke 24h readers:\n%s", wantHistory, history)
	}
}
