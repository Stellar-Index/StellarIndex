// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The thin-pool third-alias shape (launch-plan D7, C4-012/13) on
// /v1/price/tip — the one served-price surface that MERGES trades across
// alias forms rather than taking the first form that answers.
//
// The merge exists for XLM: `native` (SDEX) and `crypto:XLM` (the CEX
// feeds) are two deep, disjoint venue populations of one asset, and a
// tip that read only one of them failed the ≤30s freshness contract for
// the natural spelling. Crossing BOTH sides' alias families, though,
// also pulls in every SAC-form combination — and once the operator's
// `[supply].sac_wrappers` registry is installed, a classic-quoted tip
// for a wrapped classic (`AQUA-G…` in `USDC-G…`) merged the Soroban
// SAC/SAC pool's raw prints into a window VWAP with NO gate on that pool
// itself: the substance gate measures the alias UNION, which the deep
// SDEX book clears on the pool's behalf; the trailing-baseline guard
// never runs on the tip; and the window VWAP is computed straight from
// raw trades. In any 30s window in which the SDEX book was silent — most
// windows, for most classic assets — one trade on a few-hundred-dollar
// pool WAS the served tip, `stale:false`, ahead of the deep book's
// closed bucket one tier down.
//
// The tip now merges only the established forms (tipMergePairs). A
// SAC-form combination the caller did not name is read LAST — after the
// window, the closed-bucket read and every other fallback have missed —
// so a classic-keyed, classic-quoted tip falls to the closed-bucket read
// (the deep book, guarded) exactly as /v1/price resolves the same pair,
// a wrapped classic whose ONLY market is its Soroban pool still serves
// from that pool, and a pool print can neither displace nor blend into
// an answer the classic book can give.
//
// NOT parallel: installPegAliasRegistry publishes the process-wide
// registry.

// tipCallLog is the order in which the tip consulted its readers — one
// entry per TradesInRange ("trades:<pair>") and LatestPrice
// ("latest:<pair>") call — so a test can prove not just WHICH alias
// combinations were read but WHEN, relative to the closed-bucket read.
type tipCallLog struct{ calls []string }

func firstIndexWhere(calls []string, pred func(string) bool) int {
	for i, c := range calls {
		if pred(c) {
			return i
		}
	}
	return -1
}

func lastIndexWhere(calls []string, pred func(string) bool) int {
	for i := len(calls) - 1; i >= 0; i-- {
		if pred(calls[i]) {
			return i
		}
	}
	return -1
}

// sacFormPair reports whether either side of a "<base>/<quote>" string
// is a Soroban (SAC) form.
func sacFormPair(p string) bool {
	for _, side := range strings.SplitN(p, "/", 2) {
		if a, err := canonical.ParseAsset(side); err == nil && a.Type == canonical.AssetSoroban {
			return true
		}
	}
	return false
}

// recordingHistoryReader wraps stubHistoryReader (whose TradesInRange
// already filters the fixture by each trade's own Pair) and records every
// pair TradesInRange was asked for, so a test can prove which alias
// combinations the tip consulted.
type recordingHistoryReader struct {
	*stubHistoryReader
	pairs []string
	log   *tipCallLog
}

func (r *recordingHistoryReader) TradesInRange(ctx context.Context, pair canonical.Pair, from, to time.Time, limit int) ([]canonical.Trade, error) {
	key := pair.Base.String() + "/" + pair.Quote.String()
	r.pairs = append(r.pairs, key)
	if r.log != nil {
		r.log.calls = append(r.log.calls, "trades:"+key)
	}
	return r.stubHistoryReader.TradesInRange(ctx, pair, from, to, limit)
}

// tipOrderPriceReader wraps stubPriceReader and records every
// LatestPrice call into the shared tipCallLog, so the closed-bucket
// read's place in the tip's order is observable.
type tipOrderPriceReader struct {
	*stubPriceReader
	log *tipCallLog
}

func (r *tipOrderPriceReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	r.log.calls = append(r.log.calls, "latest:"+a.String()+"/"+q.String())
	return r.stubPriceReader.LatestPrice(ctx, a, q)
}

// mkPairTrade builds one on-chain trade on an explicit pair: base and
// quote are stroop-scale integers (7dp), so (10_000_000, 50_000) is one
// unit of base for 0.005 units of quote.
func mkPairTrade(t *testing.T, base, quote string, ts time.Time, baseAmt, quoteAmt int64, source string) canonical.Trade {
	t.Helper()
	b, err := canonical.ParseAsset(base)
	if err != nil {
		t.Fatalf("ParseAsset(%s): %v", base, err)
	}
	q, err := canonical.ParseAsset(quote)
	if err != nil {
		t.Fatalf("ParseAsset(%s): %v", quote, err)
	}
	pair, err := canonical.NewPair(b, q)
	if err != nil {
		t.Fatalf("NewPair(%s/%s): %v", base, quote, err)
	}
	return canonical.Trade{
		Source:      source,
		Ledger:      1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		OpIndex:     0,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(baseAmt)),
		QuoteAmount: canonical.NewAmount(big.NewInt(quoteAmt)),
	}
}

func assertNoSACPairConsulted(t *testing.T, pairs []string) {
	t.Helper()
	for _, p := range pairs {
		if sacFormPair(p) {
			t.Errorf("tip window consulted %q — a SAC-form combination the caller did not name", p)
		}
	}
}

// TestPriceTip_ThinPoolThirdAlias_ClassicQuoteFallsToTheClosedBook: the
// SDEX book is silent in the window, the Soroban SAC/SAC pool printed
// one trade 2s ago at five times the book. The classic-quoted tip must
// NOT serve that print; with the established forms silent it serves the
// closed-bucket read — the deep book — and never consults a SAC pair,
// because the closed bucket answered before the SAC set's turn came.
//
// RED before tipMergePairs: price 0.0050000000 from soroswap.
func TestPriceTip_ThinPoolThirdAlias_ClassicQuoteFallsToTheClosedBook(t *testing.T) {
	installPegAliasRegistry(t)
	now := time.Now().UTC()
	hist := &recordingHistoryReader{stubHistoryReader: &stubHistoryReader{trades: []canonical.Trade{
		mkPairTrade(t, pegAliasAquaSAC, pegAliasUSDCSAC, now.Add(-2*time.Second), 10_000_000, 50_000, "soroswap"),
	}}}
	prices := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			pegAliasAquaClassic + "/" + pegAliasUSDCClassic: {
				AssetID: pegAliasAquaClassic, Quote: pegAliasUSDCClassic,
				Price: "0.0010", PriceType: "vwap", ObservedAt: now.Add(-40 * time.Second),
			},
		},
		sources: map[string][]string{pegAliasAquaClassic + "/" + pegAliasUSDCClassic: {"sdex"}},
	}
	srv := v1.New(v1.Options{Prices: prices, History: hist})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset="+pegAliasAquaClassic+"&quote="+pegAliasUSDCClassic)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.0010"`) {
		t.Errorf("price: want the closed SDEX bucket 0.0010 — a fresh SAC/SAC print must not become the classic-quoted tip: %s", body)
	}
	if !strings.Contains(body, `"sources":["sdex"]`) {
		t.Errorf("sources: want [sdex]: %s", body)
	}
	assertNoSACPairConsulted(t, hist.pairs)
	// The established forms WERE consulted (5s then the 30s escalation),
	// so the exclusion is a choice of candidates, not a disabled merge.
	if callIndex(hist.pairs, pegAliasAquaClassic+"/"+pegAliasUSDCClassic) < 0 {
		t.Errorf("tip window never read the classic pair; consulted %v", hist.pairs)
	}
}

// TestPriceTip_ThinPoolThirdAlias_XLMMergesEstablishedFormsOnly: for XLM
// the established forms (`native`, `crypto:XLM`) still merge — that is
// the merge's reason to exist — and the XLM SAC pool is left out of a
// classic-quoted window even when it printed inside it.
//
// RED before tipMergePairs: the SAC print is blended in, price
// 0.7100000000 with sources [sdex soroswap].
func TestPriceTip_ThinPoolThirdAlias_XLMMergesEstablishedFormsOnly(t *testing.T) {
	installPegAliasRegistry(t)
	now := time.Now().UTC()
	hist := &recordingHistoryReader{stubHistoryReader: &stubHistoryReader{trades: []canonical.Trade{
		// SDEX: 1 XLM for 0.315 USDC, twice.
		mkPairTrade(t, "native", pegAliasUSDCClassic, now.Add(-3*time.Second), 10_000_000, 3_150_000, "sdex"),
		mkPairTrade(t, "native", pegAliasUSDCClassic, now.Add(-1*time.Second), 10_000_000, 3_150_000, "sdex"),
		// The Soroban XLM pool, SAC/SAC: 1 XLM for 1.5 USDC — a print
		// that would drag a merged VWAP to 0.71.
		mkPairTrade(t, canonical.XLMSacContractID, pegAliasUSDCSAC, now.Add(-2*time.Second), 10_000_000, 15_000_000, "soroswap"),
	}}}
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}, History: hist})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote="+pegAliasUSDCClassic+"&window_seconds=5")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.3150000000"`) {
		t.Errorf("price: want the SDEX-only window VWAP 0.3150000000: %s", body)
	}
	if !strings.Contains(body, `"sources":["sdex"]`) {
		t.Errorf("sources: want [sdex] — the SAC pool must not be merged into a native-keyed tip: %s", body)
	}
	assertNoSACPairConsulted(t, hist.pairs)
	// Both established forms are still in the merge set.
	for _, want := range []string{"native/" + pegAliasUSDCClassic, "crypto:XLM/" + pegAliasUSDCClassic} {
		if callIndex(hist.pairs, want) < 0 {
			t.Errorf("tip window did not consult %q; consulted %v", want, hist.pairs)
		}
	}
}

// TestPriceTip_ThinPoolThirdAlias_SACKeyedRequestMergesTheNamedPool: a
// caller who names the SAC forms is asking about that pool and gets its
// window VWAP — unchanged behaviour, and the proof that the pool is
// reachable at all (the two tests above are not passing against a pair
// the stub could never have returned).
func TestPriceTip_ThinPoolThirdAlias_SACKeyedRequestMergesTheNamedPool(t *testing.T) {
	installPegAliasRegistry(t)
	now := time.Now().UTC()
	hist := &recordingHistoryReader{stubHistoryReader: &stubHistoryReader{trades: []canonical.Trade{
		mkPairTrade(t, pegAliasAquaSAC, pegAliasUSDCSAC, now.Add(-2*time.Second), 10_000_000, 50_000, "soroswap"),
	}}}
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}, History: hist})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset="+pegAliasAquaSAC+"&quote="+pegAliasUSDCSAC)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.0050000000"`) || !strings.Contains(body, `"sources":["soroswap"]`) {
		t.Errorf("SAC-keyed tip: want the named pool's own 0.0050000000 from soroswap: %s", body)
	}
	if callIndex(hist.pairs, pegAliasAquaSAC+"/"+pegAliasUSDCSAC) < 0 {
		t.Errorf("tip window never read the named SAC pair; consulted %v", hist.pairs)
	}
}

// TestPriceTip_ThinPoolThirdAlias_SorobanOnlyWrappedClassicServesThePoolLast:
// a wrapped classic with NO classic venue — no SDEX trade in the window
// and no closed bucket for the classic pair — whose Soroban SAC/SAC pool
// printed 2s ago. The classic-keyed tip serves that print, from the pool
// (the alternative is 404), and reaches it only AFTER the established
// combinations' window at both bounds and the closed-bucket read have
// missed.
//
// RED with the SAC set dropped instead of read last: 404.
func TestPriceTip_ThinPoolThirdAlias_SorobanOnlyWrappedClassicServesThePoolLast(t *testing.T) {
	installPegAliasRegistry(t)
	now := time.Now().UTC()
	log := &tipCallLog{}
	hist := &recordingHistoryReader{stubHistoryReader: &stubHistoryReader{trades: []canonical.Trade{
		mkPairTrade(t, pegAliasAquaSAC, pegAliasUSDCSAC, now.Add(-2*time.Second), 10_000_000, 50_000, "soroswap"),
	}}, log: log}
	prices := &tipOrderPriceReader{stubPriceReader: &stubPriceReader{}, log: log}
	srv := v1.New(v1.Options{Prices: prices, History: hist})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset="+pegAliasAquaClassic+"&quote="+pegAliasUSDCClassic)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a wrapped classic whose only market is its pool must still serve from it", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.0050000000"`) || !strings.Contains(body, `"sources":["soroswap"]`) {
		t.Errorf("want the pool's own 0.0050000000 from soroswap: %s", body)
	}
	// Order: the classic window (5s, then the 30s escalation), then the
	// closed-bucket read, and only then the first SAC-form window read.
	classicPair := pegAliasAquaClassic + "/" + pegAliasUSDCClassic
	lastClassicWindow := lastIndexWhere(log.calls, func(c string) bool { return c == "trades:"+classicPair })
	closedBucket := firstIndexWhere(log.calls, func(c string) bool { return strings.HasPrefix(c, "latest:") })
	firstSACWindow := firstIndexWhere(log.calls, func(c string) bool {
		return strings.HasPrefix(c, "trades:") && sacFormPair(strings.TrimPrefix(c, "trades:"))
	})
	if lastClassicWindow < 0 || closedBucket < 0 || firstSACWindow < 0 {
		t.Fatalf("want the classic window, the closed-bucket read and a SAC window read; got %v", log.calls)
	}
	if lastClassicWindow > closedBucket || closedBucket > firstSACWindow {
		t.Errorf("SAC-form combinations must be read LAST — after the classic window and the closed-bucket read: %v", log.calls)
	}
}

// TestPriceTip_ThinPoolThirdAlias_DeeperPoolNeverBlendsIntoTheClassicBook:
// the wrapped classic's DEEPER venue is the Soroban pool — ten prints in
// the window against one SDEX trade. The classic-keyed tip is the SDEX
// book alone. The exclusion is by form, not by depth: depth on a pool is
// exactly what an attacker can manufacture, and the classic-quoted
// answer is the classic book's, as on /v1/price.
//
// RED before tipMergePairs: a blended VWAP with sources [sdex soroswap].
func TestPriceTip_ThinPoolThirdAlias_DeeperPoolNeverBlendsIntoTheClassicBook(t *testing.T) {
	installPegAliasRegistry(t)
	now := time.Now().UTC()
	trades := []canonical.Trade{
		// SDEX: one trade, 1 AQUA for 0.001 USDC.
		mkPairTrade(t, pegAliasAquaClassic, pegAliasUSDCClassic, now.Add(-3*time.Second), 10_000_000, 10_000, "sdex"),
	}
	for i := 1; i <= 10; i++ {
		// The pool: ten prints at five times the book.
		trades = append(trades, mkPairTrade(t, pegAliasAquaSAC, pegAliasUSDCSAC,
			now.Add(-time.Duration(i)*100*time.Millisecond), 10_000_000, 50_000, "soroswap"))
	}
	hist := &recordingHistoryReader{stubHistoryReader: &stubHistoryReader{trades: trades}}
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}, History: hist})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset="+pegAliasAquaClassic+"&quote="+pegAliasUSDCClassic)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"price":"0.0010000000"`) {
		t.Errorf("price: want the SDEX-only window VWAP 0.0010000000 — the deeper pool is not a reason to merge it: %s", body)
	}
	if !strings.Contains(body, `"sources":["sdex"]`) {
		t.Errorf("sources: want [sdex]: %s", body)
	}
	assertNoSACPairConsulted(t, hist.pairs)
}
