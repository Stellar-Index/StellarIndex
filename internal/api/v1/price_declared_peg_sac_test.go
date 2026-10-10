// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// The declared USD peg has two spellings: the classic id in
// `[trades].usd_pegged_classic_assets` and the SAC wrapping it. They are one
// asset, so /v1/price sends both down the peg's own route (XLM cross, then
// the declaration), not the sibling walk. The tests run in the deployed
// single-peg shape (price_declared_peg_test.go) with the wrapper registry
// installed, and pin the route, its spelling order and the reads it must
// never make.

var (
	pegClassicXLMBook = pegAliasUSDCClassic + "/native"
	pegSACPool        = pegAliasUSDCSAC + "/" + canonical.XLMSacContractID
)

const xlmDollarPair = "crypto:XLM/fiat:USD"

var pegSpellings = []string{pegAliasUSDCClassic, pegAliasUSDCSAC}

// pegSelfPairs are the two orientations of the pair the SAC twin would
// build on the sibling walk: itself against its own classic form. Never a
// market, never read.
var pegSelfPairs = []string{
	pegAliasUSDCSAC + "/" + pegAliasUSDCClassic,
	pegAliasUSDCClassic + "/" + pegAliasUSDCSAC,
}

func assertNoPegSelfPairRead(t *testing.T, asked []string) {
	t.Helper()
	for _, pair := range asked {
		for _, self := range pegSelfPairs {
			if pair == self {
				t.Errorf("reader asked for %s — the two sides are one asset, never a market", pair)
			}
		}
	}
}

func pegSnap(base, quote, price string, at time.Time) v1.PriceSnapshot {
	return v1.PriceSnapshot{
		AssetID: base, Quote: quote,
		Price: price, PriceType: "vwap", ObservedAt: v1.WireTime(at), WindowSeconds: 60,
	}
}

// pegServerFor serves prices with the given declared pegs.
func pegServerFor(t *testing.T, prices v1.PriceReader, pegs ...canonical.Asset) string {
	t.Helper()
	srv := v1.New(v1.Options{
		Prices:            prices,
		USDPeggedClassics: pegs,
		PegDeclaredAt:     declaredPegAdoptedAt,
	})
	return startHTTPTest(t, srv.Handler()).URL
}

// pegServer installs the wrapper registry and serves prices with its USDC
// declared as the peg.
func pegServer(t *testing.T, prices v1.PriceReader) string {
	t.Helper()
	return pegServerFor(t, prices, installPegAliasRegistry(t))
}

func pegPriceURL(base, spelling string) string {
	return base + "/v1/price?asset=" + spelling + "&quote=fiat:USD"
}

func assertSACPoolUnread(t *testing.T, asked []string, why string) {
	t.Helper()
	if callIndex(asked, pegSACPool) >= 0 {
		t.Errorf("the peg's SAC book was read %s (asked=%v)", why, asked)
	}
}

// xlmCrossReader is the shared cross fixture: the peg's XLM book where SDEX
// prints it (9.5 XLM per USDC) and XLM's dollar market (0.10), a cross of
// 0.95 with the pivot the staler leg.
func xlmCrossReader(pegLegAt, pivotAt time.Time) *recordingPriceReader {
	return &recordingPriceReader{stubPriceReader: stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			pegClassicXLMBook: pegSnap(pegAliasUSDCClassic, "native", "9.5", pegLegAt),
			xlmDollarPair:     pegSnap("crypto:XLM", "fiat:USD", "0.10", pivotAt),
		},
		sources: map[string][]string{
			pegClassicXLMBook: {"sdex"},
			xlmDollarPair:     {"coinbase", "bitstamp"},
		},
	}}
}

// TestPrice_DeclaredPegSACTwinServesTheClassicSpellingsCross: the SAC
// spelling serves the same XLM cross as the classic id, everything but the
// asset_id echo identical.
func TestPrice_DeclaredPegSACTwinServesTheClassicSpellingsCross(t *testing.T) {
	pegLegAt := time.Unix(1745000000, 0).UTC()
	pivotAt := pegLegAt.Add(-3 * time.Minute)
	reader := xlmCrossReader(pegLegAt, pivotAt)
	base := pegServer(t, reader)

	classic := getPegEnvelope(t, pegPriceURL(base, pegAliasUSDCClassic))
	sac := getPegEnvelope(t, pegPriceURL(base, pegAliasUSDCSAC))

	if sac.Data.Price != "0.9500000000" {
		t.Errorf("SAC price = %q, want 0.9500000000 — the observed XLM cross the classic id serves", sac.Data.Price)
	}
	if sac.Data.PriceType != "vwap" || sac.Data.WindowSeconds != 60 {
		t.Errorf("SAC snapshot = %s/%d, want vwap/60", sac.Data.PriceType, sac.Data.WindowSeconds)
	}
	if !sac.Data.ObservedAt.Time().Equal(pivotAt) {
		t.Errorf("SAC observed_at = %s, want the older leg's %s", sac.Data.ObservedAt, pivotAt)
	}
	if sac.Data.AssetID != pegAliasUSDCSAC || sac.Data.Quote != "fiat:USD" {
		t.Errorf("echo = %s/%s, want the requested %s/fiat:USD", sac.Data.AssetID, sac.Data.Quote, pegAliasUSDCSAC)
	}
	want := classic.Data
	want.AssetID = pegAliasUSDCSAC
	if !reflect.DeepEqual(sac.Data, want) {
		t.Errorf("SAC snapshot differs from the classic spelling's beyond asset_id:\n sac     = %+v\n classic = %+v", sac.Data, classic.Data)
	}
	if !reflect.DeepEqual(sac.Flags, classic.Flags) {
		t.Errorf("flags differ between spellings: sac=%+v classic=%+v", sac.Flags, classic.Flags)
	}
	if !sac.Flags.Triangulated {
		t.Errorf("flags.triangulated = false, want true — the value is composed through XLM")
	}
	body, _ := readAll(mustGet(t, pegPriceURL(base, pegAliasUSDCSAC)))
	if !strings.Contains(body, `"sources":["bitstamp","coinbase","sdex"]`) {
		t.Errorf("sources must credit both legs' venues, sorted: %s", body)
	}
	assertNoPegSelfPairRead(t, reader.pairsAsked())
}

// dormantBookVsFreshPoolReader: the peg's classic SDEX book is dormant
// (closed bucket, optionally flagged stale) and a thin Soroban pool under
// the peg's SAC id is fresh. Crossed with the 0.10 pivot they disagree by
// more than 2x: 0.95 from the book, 2.00 from the pool.
func dormantBookVsFreshPoolReader(at time.Time, bookStale bool) *recordingPriceReader {
	return &recordingPriceReader{stubPriceReader: stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			pegClassicXLMBook: pegSnap(pegAliasUSDCClassic, "native", "9.5", at),
			pegSACPool:        pegSnap(pegAliasUSDCSAC, canonical.XLMSacContractID, "20.0", at),
			xlmDollarPair:     pegSnap("crypto:XLM", "fiat:USD", "0.10", at),
		},
		stale: map[string]bool{pegClassicXLMBook: bookStale},
		sources: map[string][]string{
			pegClassicXLMBook: {"sdex"},
			pegSACPool:        {"soroswap"},
			xlmDollarPair:     {"coinbase"},
		},
	}}
}

// TestPrice_DeclaredPegXLMCrossDormantClassicBookOutranksFreshSACPool: the
// leg walks ONE spelling at a time and reaches the SAC form only when the
// classic form found nothing. A dormant (stale) classic book still answers;
// a fresh thin pool must not displace it, and is never read.
func TestPrice_DeclaredPegXLMCrossDormantClassicBookOutranksFreshSACPool(t *testing.T) {
	reader := dormantBookVsFreshPoolReader(time.Unix(1745000000, 0).UTC(), true)
	base := pegServer(t, reader)

	env := getPegEnvelope(t, pegPriceURL(base, pegAliasUSDCClassic))
	if env.Data.Price != "0.9500000000" {
		t.Errorf("price = %q, want 0.9500000000 — the dormant SDEX book, not the fresh pool's 2.0000000000",
			env.Data.Price)
	}
	body, _ := readAll(mustGet(t, pegPriceURL(base, pegAliasUSDCClassic)))
	if !strings.Contains(body, `"sources":["coinbase","sdex"]`) {
		t.Errorf("sources must be the book's venues and the pivot's, without the pool: %s", body)
	}
	assertSACPoolUnread(t, reader.pairsAsked(), "although its classic form answered")
	assertNoPegSelfPairRead(t, reader.pairsAsked())
}

// TestPrice_DeclaredPegXLMCrossOrdersTheSpellingsCanonicalFirst: spellings
// are walked classic first, SAC last, not literal-first. With both books
// fresh and disagreeing, either spelling serves the deep book's value.
func TestPrice_DeclaredPegXLMCrossOrdersTheSpellingsCanonicalFirst(t *testing.T) {
	reader := dormantBookVsFreshPoolReader(time.Unix(1745000000, 0).UTC(), false)
	base := pegServer(t, reader)

	for _, spelling := range pegSpellings {
		env := getPegEnvelope(t, pegPriceURL(base, spelling))
		if env.Data.Price != "0.9500000000" {
			t.Errorf("%s: price = %q, want 0.9500000000 — the classic book prices the peg under either spelling, not the pool's 2.0000000000",
				spelling, env.Data.Price)
		}
		if env.Data.AssetID != spelling {
			t.Errorf("asset_id = %q, want the requested %q", env.Data.AssetID, spelling)
		}
	}
	assertNoPegSelfPairRead(t, reader.pairsAsked())
}

// TestPrice_DeclaredPegXLMCrossReadsThePegsOwnSACBook: the peg leg reads the
// peg's whole family. A peg whose only XLM book is a pool under its SAC id
// crosses through it for both spellings instead of falling to the declaration.
func TestPrice_DeclaredPegXLMCrossReadsThePegsOwnSACBook(t *testing.T) {
	poolAt := time.Unix(1745000000, 0).UTC()
	reader := &recordingPriceReader{stubPriceReader: stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			pegSACPool:    pegSnap(pegAliasUSDCSAC, canonical.XLMSacContractID, "10.0", poolAt),
			xlmDollarPair: pegSnap("crypto:XLM", "fiat:USD", "0.10", poolAt),
		},
	}}
	base := pegServer(t, reader)

	for _, spelling := range pegSpellings {
		env := getPegEnvelope(t, pegPriceURL(base, spelling))
		if env.Data.Price != "1.0000000000" || env.Data.PriceType != "vwap" {
			t.Errorf("%s: served %s (%s), want the pool's cross 1.0000000000 (vwap), not the declaration",
				spelling, env.Data.Price, env.Data.PriceType)
		}
		if !env.Data.ObservedAt.Time().Equal(poolAt) {
			t.Errorf("%s: observed_at = %s, want the pool's %s", spelling, env.Data.ObservedAt, poolAt)
		}
		if env.Data.AssetID != spelling {
			t.Errorf("asset_id = %q, want the requested %q", env.Data.AssetID, spelling)
		}
	}
	assertNoPegSelfPairRead(t, reader.pairsAsked())
}

// TestPrice_DeclaredPegSACTwinWithNoObservationServesTheDeclaration: with no
// market anywhere the SAC spelling serves the declaration (peg, adoption
// stamp, C-address echo) and never probes its own classic form.
func TestPrice_DeclaredPegSACTwinWithNoObservationServesTheDeclaration(t *testing.T) {
	reader := &recordingPriceReader{}
	base := pegServer(t, reader)

	env := getPegEnvelope(t, pegPriceURL(base, pegAliasUSDCSAC))
	assertDeclarationServed(t, env, pegAliasUSDCSAC)
	if !env.Flags.Triangulated {
		t.Errorf("flags.triangulated = false, want true")
	}
	asked := reader.pairsAsked()
	if len(asked) == 0 {
		t.Fatal("the reader was never consulted — the direct read and the cross must both run before the declaration")
	}
	assertNoPegSelfPairRead(t, asked)
}

// TestPrice_NonPegSACAssetStillWalksTheDeclaredPegs: a SAC asset that is not
// the declared peg takes the sibling walk, and the XLM cross is not run.
func TestPrice_NonPegSACAssetStillWalksTheDeclaredPegs(t *testing.T) {
	poolAt := time.Unix(1745000000, 0).UTC()
	reader := &recordingPriceReader{stubPriceReader: stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			pegAliasAquaSAC + "/" + pegAliasUSDCClassic: pegSnap(pegAliasAquaSAC, pegAliasUSDCClassic, "0.0041", poolAt),
			// Present but must not be consulted: the cross is the declared peg's route only.
			pegAliasAquaSAC + "/native": pegSnap(pegAliasAquaSAC, "native", "0.02", poolAt),
			xlmDollarPair:               pegSnap("crypto:XLM", "fiat:USD", "0.10", poolAt),
		},
	}}
	base := pegServer(t, reader)

	env := getPegEnvelope(t, pegPriceURL(base, pegAliasAquaSAC))
	if env.Data.Price != "0.0041" {
		t.Errorf("price = %q, want the proxy walk's 0.0041 (not the XLM cross 0.0020000000)", env.Data.Price)
	}
	if !env.Flags.Triangulated {
		t.Errorf("flags.triangulated = false, want true — served through the peg, not the requested quote")
	}
	if env.Data.AssetID != pegAliasAquaSAC || env.Data.Quote != "fiat:USD" {
		t.Errorf("echo = %s/%s, want %s/fiat:USD", env.Data.AssetID, env.Data.Quote, pegAliasAquaSAC)
	}
	asked := reader.pairsAsked()
	if callIndex(asked, pegAliasAquaSAC+"/"+pegAliasUSDCClassic) < 0 {
		t.Errorf("the walk never read %s/%s (asked=%v)", pegAliasAquaSAC, pegAliasUSDCClassic, asked)
	}
	for _, pair := range asked {
		if strings.HasSuffix(pair, "/native") || strings.HasSuffix(pair, "/crypto:XLM") ||
			strings.HasSuffix(pair, "/"+canonical.XLMSacContractID) {
			t.Errorf("reader asked for %s — the XLM cross must not run for an asset that is not the declared peg", pair)
		}
	}
}

// recordingRecentReader records every pair RecentClosedSnapshots was asked for.
type recordingRecentReader struct {
	stubPriceReader
	mu    sync.Mutex
	calls []string
}

func (r *recordingRecentReader) RecentClosedSnapshots(ctx context.Context, a, q canonical.Asset, n int) ([]v1.PriceSnapshot, error) {
	r.mu.Lock()
	r.calls = append(r.calls, a.String()+"/"+q.String())
	r.mu.Unlock()
	return r.stubPriceReader.RecentClosedSnapshots(ctx, a, q, n)
}

// TestOraclePrices_DeclaredPegSACTwinNeverProbesItsOwnClassicForm: the
// self-pair guard on the surface that walks the pegs with its own reader call.
func TestOraclePrices_DeclaredPegSACTwinNeverProbesItsOwnClassicForm(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	reader := &recordingRecentReader{}
	srv := v1.New(v1.Options{Prices: reader, USDPeggedClassics: []canonical.Asset{usdc}})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/oracle/prices?asset="+pegAliasUSDCSAC)
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("status = %d, want 200. Body: %s", resp.StatusCode, body)
	}
	reader.mu.Lock()
	asked := append([]string(nil), reader.calls...)
	reader.mu.Unlock()
	if callIndex(asked, pegAliasUSDCSAC+"/fiat:USD") < 0 {
		t.Errorf("the requested pair was never read (asked=%v)", asked)
	}
	assertNoPegSelfPairRead(t, asked)
}

// recordingPriceAtReader implements v1.PriceAtReader keyed on
// "<base>/<quote>" and records every pair asked. A hit is stamped at the
// requested ts, so it is inside the lookback and the staleness bound.
type recordingPriceAtReader struct {
	byPair map[string]string
	mu     sync.Mutex
	calls  []string
}

func (r *recordingPriceAtReader) PriceAt(
	_ context.Context, pair canonical.Pair, ts time.Time, _ time.Duration,
) (string, time.Time, int, error) {
	key := pair.Base.String() + "/" + pair.Quote.String()
	r.mu.Lock()
	r.calls = append(r.calls, key)
	r.mu.Unlock()
	value, ok := r.byPair[key]
	if !ok {
		return "", time.Time{}, 0, v1.ErrPriceAtUnavailable
	}
	return value, ts, 60, nil
}

func (r *recordingPriceAtReader) pairsAsked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// pegPriceAtServer serves a two-peg (USDC, PYUSD) point-in-time reader where
// only SAC/PYUSD has a row, so a guard that skips the SAC twin's own form
// must still walk on to the second peg.
func pegPriceAtServer(t *testing.T) (string, *recordingPriceAtReader) {
	t.Helper()
	usdc := installPegAliasRegistry(t)
	pyusd := mustClassicAsset(t, "PYUSD", pegAliasPYUSDIssuer)
	reader := &recordingPriceAtReader{byPair: map[string]string{
		pegAliasUSDCSAC + "/" + pegAliasPYUSDClassic: "1.0004",
	}}
	srv := v1.New(v1.Options{PriceAt: reader, USDPeggedClassics: []canonical.Asset{usdc, pyusd}})
	return startHTTPTest(t, srv.Handler()).URL, reader
}

func assertSecondPegWalked(t *testing.T, reader *recordingPriceAtReader) {
	t.Helper()
	asked := reader.pairsAsked()
	if callIndex(asked, pegAliasUSDCSAC+"/"+pegAliasPYUSDClassic) < 0 {
		t.Errorf("the second peg was never read (asked=%v)", asked)
	}
	assertNoPegSelfPairRead(t, asked)
}

// TestPriceAt_DeclaredPegSACTwinSkipsItsOwnFormAndWalksOn: the self-pair
// guard on /v1/price/at folds through the alias registry and skips only
// THAT peg; a second declared peg is still walked.
func TestPriceAt_DeclaredPegSACTwinSkipsItsOwnFormAndWalksOn(t *testing.T) {
	base, reader := pegPriceAtServer(t)
	at := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	env := getPegEnvelope(t, base+"/v1/price/at?asset="+pegAliasUSDCSAC+"&quote=fiat:USD&ts="+at.Format(time.RFC3339))
	if env.Data.Price != "1.0004" {
		t.Errorf("price = %q, want 1.0004 — the second declared peg must still be walked", env.Data.Price)
	}
	if env.Data.AssetID != pegAliasUSDCSAC || env.Data.Quote != "fiat:USD" {
		t.Errorf("echo = %s/%s, want the requested %s/fiat:USD", env.Data.AssetID, env.Data.Quote, pegAliasUSDCSAC)
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false, want true — served through a peg, not the requested quote")
	}
	assertSecondPegWalked(t, reader)
}

// TestPriceChanges_DeclaredPegSACTwinSkipsItsOwnFormAndWalksOn is the same
// guard on /v1/price/changes.
func TestPriceChanges_DeclaredPegSACTwinSkipsItsOwnFormAndWalksOn(t *testing.T) {
	base, reader := pegPriceAtServer(t)

	resp := mustGet(t, base+"/v1/price/changes?asset="+pegAliasUSDCSAC+"&quote=fiat:USD")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", resp.StatusCode, body)
	}
	for _, want := range []string{`"current_price":"1.0004"`, `"quote":"fiat:USD"`, `"triangulated":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\n%s", want, body)
		}
	}
	assertSecondPegWalked(t, reader)
}

// pegLegErrorReader is a recordingPriceReader with PER-PAIR errors: a pair
// in `errs` fails with exactly that error AFTER the call is recorded.
type pegLegErrorReader struct {
	recordingPriceReader
	errs map[string]error
}

func (r *pegLegErrorReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	snap, srcs, stale, err := r.recordingPriceReader.LatestPrice(ctx, a, q)
	if perPair, listed := r.errs[a.String()+"/"+q.String()]; listed {
		return v1.PriceSnapshot{}, nil, false, perPair
	}
	return snap, srcs, stale, err
}

// withheldClassicBookLivePoolReader is the gate-bypass shape: the peg's
// classic XLM book fails with classicErr while a live pool holds the SAC id.
// ScamGate.Withheld is false for any non-classic base, so a walk that took
// the failure as "no market" would republish the refused market through the
// ungated spelling.
func withheldClassicBookLivePoolReader(at time.Time, classicErr error) *pegLegErrorReader {
	return &pegLegErrorReader{
		recordingPriceReader: *dormantBookVsFreshPoolReader(at, false),
		errs:                 map[string]error{pegClassicXLMBook: classicErr},
	}
}

// assertDeclarationServed checks the declaration's wire shape: the flat 1:1
// constant, labelled `peg`, stamped with the adoption time.
func assertDeclarationServed(t *testing.T, env pegEnvelope, spelling string) {
	t.Helper()
	if env.Data.Price != "1.000000000000" || env.Data.PriceType != "peg" {
		t.Errorf("%s: served %s (%s), want the declaration 1.000000000000 (peg)",
			spelling, env.Data.Price, env.Data.PriceType)
	}
	if !env.Data.ObservedAt.Time().Equal(declaredPegAdoptedAt) {
		t.Errorf("%s: observed_at = %s, want the declaration stamp %s",
			spelling, env.Data.ObservedAt, declaredPegAdoptedAt)
	}
	if env.Data.AssetID != spelling {
		t.Errorf("asset_id = %q, want the requested %q", env.Data.AssetID, spelling)
	}
}

// TestPrice_DeclaredPegXLMCrossClassicBookFailureEndsTheSpellingWalk: a
// flagged-issuer refusal or a per-pair read failure (v0.60.0's 42883 planning
// error) on the classic book ends the walk and serves the declaration. Taking
// either as "no market" would reprice the peg off the SAC spelling's thin
// pool (2.0000000000), republishing, for a refusal, a market the scam gate
// withheld.
func TestPrice_DeclaredPegXLMCrossClassicBookFailureEndsTheSpellingWalk(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		spellings []string
	}{
		{"flagged-issuer refusal", v1.PriceWithheldError(pricingguard.WithheldFlaggedIssuer), pegSpellings},
		{"read failure", errors.New(`ERROR: operator does not exist: numeric = text (SQLSTATE 42883)`), pegSpellings[:1]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := withheldClassicBookLivePoolReader(time.Unix(1745000000, 0).UTC(), tc.err)
			base := pegServer(t, reader)

			for _, spelling := range tc.spellings {
				assertDeclarationServed(t, getPegEnvelope(t, pegPriceURL(base, spelling)), spelling)
			}
			asked := reader.pairsAsked()
			if callIndex(asked, pegClassicXLMBook) < 0 {
				t.Errorf("the classic book was never read (asked=%v)", asked)
			}
			assertSACPoolUnread(t, asked, "after its classic book failed — a refusal or broken read must end the walk, not redirect it")
			assertNoPegSelfPairRead(t, asked)
		})
	}
}

// TestPrice_DeclaredPegXLMCrossSubstanceRefusalWithholds: a peg whose XLM
// book the substance gate refuses is withheld on every surface that reads
// the stablecoin fallback, not answered with the declaration. A refusal that
// names no gate fails closed the same way; only a flagged-issuer refusal
// keeps the declaration.
func TestPrice_DeclaredPegXLMCrossSubstanceRefusalWithholds(t *testing.T) {
	cases := map[string]error{
		"substance":    v1.PriceWithheldError(pricingguard.WithheldThinMarket),
		"unattributed": v1.ErrPriceWithheld,
	}
	for name, refusal := range cases {
		t.Run(name, func(t *testing.T) {
			reader := withheldClassicBookLivePoolReader(time.Unix(1745000000, 0).UTC(), refusal)
			base := pegServer(t, reader)

			for _, spelling := range pegSpellings {
				assertPriceWithheld(t, pegPriceURL(base, spelling))
				assertPriceWithheld(t, base+"/v1/oracle/x_last_price?base="+spelling+"&quote=fiat:USD")
			}
			asked := reader.pairsAsked()
			if callIndex(asked, pegClassicXLMBook) < 0 {
				t.Errorf("the classic book was never read (asked=%v) — the withholding would then not be the gate's", asked)
			}
			assertSACPoolUnread(t, asked, "after its classic book was WITHHELD")
		})
	}
}

// TestPrice_DeclaredPegXLMCrossPivotRefusalWithholds: the same rule on the
// cross's other leg. A withheld XLM/fiat:USD pivot is not "no market", so
// the declaration must not answer over it.
func TestPrice_DeclaredPegXLMCrossPivotRefusalWithholds(t *testing.T) {
	reader := withheldClassicBookLivePoolReader(time.Unix(1745000000, 0).UTC(), nil)
	reader.errs = map[string]error{xlmDollarPair: v1.PriceWithheldError(pricingguard.WithheldThinMarket)}
	base := pegServer(t, reader)

	assertPriceWithheld(t, pegPriceURL(base, pegAliasUSDCClassic))
	if callIndex(reader.pairsAsked(), xlmDollarPair) < 0 {
		t.Errorf("the pivot was never read (asked=%v)", reader.pairsAsked())
	}
}

// assertPriceWithheld checks url answers 404 with the price-withheld
// problem type and serves no price.
func assertPriceWithheld(t *testing.T, url string) {
	t.Helper()
	resp := mustGet(t, url)
	body, err := readAll(resp)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "errors/price-withheld") {
		t.Errorf("%s: status = %d body %s, want 404 errors/price-withheld", url, resp.StatusCode, body)
	}
	if strings.Contains(body, "1.000000000000") {
		t.Errorf("%s: the declaration was served over a refused market: %s", url, body)
	}
}

// gatingPegReader wires the optional `proxyPairGate` (the bounded
// recent-bucket probe run before an unbounded last-trade scan) onto the
// recording reader, and can make the probe MISS or FAIL. `exists` is keyed
// "<base>/<quote>" and defaults to false (no closed 1m bucket recently).
type gatingPegReader struct {
	recordingPriceReader
	exists    map[string]bool
	existsErr map[string]error

	pmu    sync.Mutex
	probes []string
}

func (r *gatingPegReader) RecentClosedVWAP1mExists(_ context.Context, base, quote canonical.Asset) (bool, error) {
	key := base.String() + "/" + quote.String()
	r.pmu.Lock()
	r.probes = append(r.probes, key)
	r.pmu.Unlock()
	if err, failing := r.existsErr[key]; failing {
		return false, err
	}
	return r.exists[key], nil
}

func (r *gatingPegReader) probesMade() []string {
	r.pmu.Lock()
	defer r.pmu.Unlock()
	return append([]string(nil), r.probes...)
}

// gatingDormantBookReader is the dormant-book/fresh-pool fixture with the
// gate reporting only the SAC pool live.
func gatingDormantBookReader(at time.Time) *gatingPegReader {
	return &gatingPegReader{
		recordingPriceReader: *dormantBookVsFreshPoolReader(at, false),
		exists:               map[string]bool{pegSACPool: true},
	}
}

// TestPrice_DeclaredPegXLMCrossGateMissIsWhatAdvancesTheSpellingWalk: a
// probe MISS is what means "this spelling found nothing". The classic book is
// held but gated out, so it is never read and the live SAC pool prices the
// cross.
func TestPrice_DeclaredPegXLMCrossGateMissIsWhatAdvancesTheSpellingWalk(t *testing.T) {
	reader := gatingDormantBookReader(time.Unix(1745000000, 0).UTC())
	base := pegServer(t, reader)

	env := getPegEnvelope(t, pegPriceURL(base, pegAliasUSDCClassic))
	if env.Data.Price != "2.0000000000" || env.Data.PriceType != "vwap" {
		t.Errorf("served %s (%s), want the pool's cross 2.0000000000 (vwap) — a gate miss is the one "+
			"verdict that advances the spelling walk", env.Data.Price, env.Data.PriceType)
	}
	if probed := reader.probesMade(); callIndex(probed, pegClassicXLMBook) < 0 {
		t.Errorf("the classic book was never probed (probes=%v)", probed)
	}
	if asked := reader.pairsAsked(); callIndex(asked, pegClassicXLMBook) >= 0 {
		t.Errorf("the gated-out classic book was READ (asked=%v) — the probe exists to skip that "+
			"unbounded last-trade scan", asked)
	}
}

// TestPrice_DeclaredPegXLMCrossGateErrorReadsThroughToTheClassicBook: a probe
// OUTAGE must not hide a price nor hand it to another spelling. The classic
// book is read anyway, answers, and the SAC pool is never reached.
func TestPrice_DeclaredPegXLMCrossGateErrorReadsThroughToTheClassicBook(t *testing.T) {
	reader := gatingDormantBookReader(time.Unix(1745000000, 0).UTC())
	reader.existsErr = map[string]error{pegClassicXLMBook: errors.New("probe unavailable")}
	base := pegServer(t, reader)

	env := getPegEnvelope(t, pegPriceURL(base, pegAliasUSDCClassic))
	if env.Data.Price != "0.9500000000" || env.Data.PriceType != "vwap" {
		t.Errorf("served %s (%s), want the classic book's cross 0.9500000000 (vwap) — a probe blip "+
			"must not hide a price", env.Data.Price, env.Data.PriceType)
	}
	assertSACPoolUnread(t, reader.pairsAsked(), "although the classic book answered")
}

// TestPrice_DeclaredPegXLMCrossDegenerateClassicPriceNeverReachesTheSACBook:
// a classic price that READ but is zero ends the walk (the spelling
// answered), crossThroughPivot declines it, and the declaration serves
// rather than a 2x move onto the SAC pool.
func TestPrice_DeclaredPegXLMCrossDegenerateClassicPriceNeverReachesTheSACBook(t *testing.T) {
	reader := dormantBookVsFreshPoolReader(time.Unix(1745000000, 0).UTC(), false)
	book := reader.snapshots[pegClassicXLMBook]
	book.Price = "0"
	reader.snapshots[pegClassicXLMBook] = book
	base := pegServer(t, reader)

	assertDeclarationServed(t, getPegEnvelope(t, pegPriceURL(base, pegAliasUSDCClassic)), pegAliasUSDCClassic)
	assertSACPoolUnread(t, reader.pairsAsked(), "although the classic book returned a row — the walk advances on FOUND NOTHING, never on found-something-unusable")
}

// scamGatedPegReader wires the REAL pricingguard.ScamGate onto the recording
// reader the way storePriceReader does on the served binary: the raw
// requested base goes to the gate once a row is found, and a refusal becomes
// a flagged-issuer v1.ErrPriceWithheld. /v1/price has no handler-side gate
// call, so this seam is the only place a withholding reaches it. The gate is
// real over a fake directory because the resolution under test lives inside
// it; a stub keyed on the asset id would pin nothing.
type scamGatedPegReader struct {
	recordingPriceReader
	scam *pricingguard.ScamGate
}

func (r *scamGatedPegReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	snap, srcs, stale, err := r.recordingPriceReader.LatestPrice(ctx, a, q)
	if err == nil && r.scam.Withheld(ctx, a, "price_read") {
		return v1.PriceSnapshot{}, nil, false, v1.PriceWithheldError(pricingguard.WithheldFlaggedIssuer)
	}
	return snap, srcs, stale, err
}

// volatilePriceBodyFields are the only /v1/price members two spellings of
// ONE asset may differ on: `as_of` (response clock) and `asset_id` (echo).
var volatilePriceBodyFields = regexp.MustCompile(`"(as_of|asset_id)":"[^"]*"`)

// maskedPriceBody fetches a /v1/price body with those two fields blanked, as
// served bytes rather than a decoded snapshot: `sources` and flags are not
// struct members and a disagreement there is what this looks for.
func maskedPriceBody(t *testing.T, url string) string {
	t.Helper()
	resp := mustGet(t, url)
	body, err := readAll(resp)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", resp.StatusCode, body)
	}
	return volatilePriceBodyFields.ReplaceAllString(body, `"$1":"<masked>"`)
}

// flaggedPegLivePoolReader: the declared peg's issuer carries a scam-class
// directory tag, it has NO classic XLM book, and its SAC spelling holds a
// live pool that crosses to 2.00. Absent (not refused) is deliberate: "found
// nothing" is the one verdict that reaches the SAC book at all. The peg is
// the flagged issuance newSACSpellingFixture keeps for this role, declared
// per server so installPegAliasRegistry is left as the rest of the file finds it.
func flaggedPegLivePoolReader(at time.Time, f sacSpellingFixture) *scamGatedPegReader {
	pool := f.sac.String() + "/" + canonical.XLMSacContractID
	return &scamGatedPegReader{
		recordingPriceReader: recordingPriceReader{stubPriceReader: stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{
				pool:          pegSnap(f.sac.String(), canonical.XLMSacContractID, "20.0", at),
				xlmDollarPair: pegSnap("crypto:XLM", "fiat:USD", "0.10", at),
			},
			sources: map[string][]string{pool: {"soroswap"}, xlmDollarPair: {"coinbase"}},
		}},
		scam: f.gate(),
	}
}

// TestPrice_FlaggedDeclaredPegServesTheDeclarationUnderBothSpellings: the
// peg route widening the walk carries a flagged issuer's price to a spelling
// the gate could not see, and the gate resolving its base closes it. The
// classic form finds nothing, the walk reaches the SAC pool (2x off the
// declaration), and both spellings must serve the SAME declaration.
//
// RED without the gate's `base = canonical.CanonicalAsset(base)`, or with the
// peg match narrowed to `peg.Equal(asset)` (the contract id 404s).
// NOT parallel: newSACSpellingFixture installs the process-global alias registry.
func TestPrice_FlaggedDeclaredPegServesTheDeclarationUnderBothSpellings(t *testing.T) {
	f := newSACSpellingFixture(t, true)
	reader := flaggedPegLivePoolReader(time.Unix(1745000000, 0).UTC(), f)
	base := pegServerFor(t, reader, f.classic)

	classic, sac := f.classic.String(), f.sac.String()
	bodies := map[string]string{}
	for _, spelling := range []string{classic, sac} {
		assertDeclarationServed(t, getPegEnvelope(t, pegPriceURL(base, spelling)), spelling)
		bodies[spelling] = maskedPriceBody(t, pegPriceURL(base, spelling))
		if strings.Contains(bodies[spelling], "soroswap") {
			t.Errorf("%s: the served body credits the pool's venue — the flagged issuer's price "+
				"was republished through the peg's SAC spelling: %s", spelling, bodies[spelling])
		}
	}
	if bodies[classic] != bodies[sac] {
		t.Errorf("the two spellings of one asset disagree:\n classic = %s\n sac     = %s",
			bodies[classic], bodies[sac])
	}

	// Non-vacuity: the pool was read, so the declaration serves because the
	// gate refused it, not because the walk never got there.
	asked := reader.pairsAsked()
	if callIndex(asked, sac+"/"+canonical.XLMSacContractID) < 0 {
		t.Errorf("the peg's SAC pool was never read (asked=%v) — the declaration would then be "+
			"serving for want of a market rather than because the gate refused one", asked)
	}
	// The mechanism: the gate asked the directory about the classic issuance's G-address.
	if len(f.dir.asked) == 0 || f.dir.asked[0] != sacSpellingFlaggedIssuer {
		t.Errorf("directory asked about %v, want the peg's issuer %q — the gate never resolved the "+
			"Soroban base to the issuance it wraps", f.dir.asked, sacSpellingFlaggedIssuer)
	}
	for _, pair := range asked {
		switch pair {
		case sac + "/" + classic, classic + "/" + sac:
			t.Errorf("reader asked for %s — the two sides are one asset, never a market", pair)
		}
	}
}

// TestPrice_XLMIdentitiesUnchangedOnTheCombinedPath is a CONTROL that cannot
// currently fail: XLM canonicalises to `native` (no issuer for the gate) and
// is not a declared peg, so neither the gate's alias resolution nor the peg
// match has a path to it. It guards a later widening that would reach XLM
// (a base resolved into the peg match, an XLM form in the peg list).
//
// The dollar bucket is held under crypto:XLM alone, so `native` answers only
// by the literal-first alias fallback; this pins that the combined path
// leaves that behaviour where it was.
func TestPrice_XLMIdentitiesUnchangedOnTheCombinedPath(t *testing.T) {
	at := time.Unix(1745000000, 0).UTC()
	// Armed gate (a flagged issuer in the directory) so XLM passing it is a verdict.
	dir := &sacScamDirectory{flagged: map[string]bool{sacSpellingFlaggedIssuer: true}}
	reader := &scamGatedPegReader{
		recordingPriceReader: recordingPriceReader{stubPriceReader: stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{xlmDollarPair: pegSnap("crypto:XLM", "fiat:USD", "0.10", at)},
			sources:   map[string][]string{xlmDollarPair: {"coinbase"}},
		}},
		scam: pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{}),
	}
	base := pegServer(t, reader)

	want := maskedPriceBody(t, pegPriceURL(base, "native"))
	for _, spelling := range []string{"crypto:XLM", canonical.XLMSacContractID} {
		got := maskedPriceBody(t, pegPriceURL(base, spelling))
		if got != want {
			t.Errorf("%s serves a different body from `native`:\n %s = %s\n native = %s",
				spelling, spelling, got, want)
		}
	}
	// Agreement must not be on the wrong thing: the observed CEX bucket, not the declaration.
	for _, member := range []string{`"price":"0.10"`, `"price_type":"vwap"`, `"sources":["coinbase"]`} {
		if !strings.Contains(want, member) {
			t.Errorf("the `native` body is missing %s — an XLM form must still serve its own "+
				"observed market on the combined path: %s", member, want)
		}
	}
}
