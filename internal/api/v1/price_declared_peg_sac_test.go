// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The declared USD peg has two spellings on the wire: the classic id the
// operator typed into `[trades].usd_pegged_classic_assets` and the SAC
// that wraps it, declared in `[supply].sac_wrappers`. They are one asset,
// so /v1/price must send both down the peg's own route — the XLM cross,
// then the declaration — rather than the sibling walk, and that route
// composes ONE value for the two of them: the cross reads the peg's whole
// family, and the declaration is a constant. (The direct fiat:USD read
// one tier above is literal-first and is not part of that claim.) The
// tests here run in the deployed single-peg shape (see
// price_declared_peg_test.go) with the registry the served binary builds
// from the wrapper map installed (installPegAliasRegistry), and pin the
// route, its spelling order, and the reads it must never make.

// pegSelfPairs are the two orientations of the one pair the SAC twin
// used to build on the sibling walk: itself against its own classic form.
// Neither is a market, and neither may ever be read.
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

// xlmCrossReader is the fixture every cross test here shares: the peg's
// XLM book stored where SDEX prints it (classic id quoted in `native`,
// 9.5 XLM per USDC) and XLM's dollar market under the CEX spelling
// (0.10) — a cross of 0.95, with the pivot the staler leg.
func xlmCrossReader(pegLegAt, pivotAt time.Time) *recordingPriceReader {
	return &recordingPriceReader{stubPriceReader: stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			pegAliasUSDCClassic + "/native": {
				AssetID: pegAliasUSDCClassic, Quote: "native",
				Price: "9.5", PriceType: "vwap", ObservedAt: pegLegAt, WindowSeconds: 60,
			},
			"crypto:XLM/fiat:USD": {
				AssetID: "crypto:XLM", Quote: "fiat:USD",
				Price: "0.10", PriceType: "vwap", ObservedAt: pivotAt, WindowSeconds: 60,
			},
		},
		sources: map[string][]string{
			pegAliasUSDCClassic + "/native": {"sdex"},
			"crypto:XLM/fiat:USD":           {"coinbase", "bitstamp"},
		},
	}}
}

// TestPrice_DeclaredPegSACTwinServesTheClassicSpellingsCross pins the
// route for the SAC spelling of the declared peg: the same XLM cross the
// classic id serves, value, price_type, window, observed_at and flags
// all identical, with `asset_id` echoing the C-address the caller sent.
//
// RED before the alias-aware match: the SAC spelling was not recognised
// as the declared peg, took the sibling walk, and 404'd.
func TestPrice_DeclaredPegSACTwinServesTheClassicSpellingsCross(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	pegLegAt := time.Unix(1745000000, 0).UTC()
	pivotAt := pegLegAt.Add(-3 * time.Minute)
	reader := xlmCrossReader(pegLegAt, pivotAt)
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
		PegDeclaredAt:     declaredPegAdoptedAt,
	})
	ts := startHTTPTest(t, srv.Handler())

	classic := getPegEnvelope(t, ts.URL+"/v1/price?asset="+pegAliasUSDCClassic+"&quote=fiat:USD")
	sac := getPegEnvelope(t, ts.URL+"/v1/price?asset="+pegAliasUSDCSAC+"&quote=fiat:USD")

	if sac.Data.Price != "0.9500000000" {
		t.Errorf("SAC price = %q, want 0.9500000000 — the observed XLM cross the classic id serves", sac.Data.Price)
	}
	if sac.Data.PriceType != "vwap" || sac.Data.WindowSeconds != 60 {
		t.Errorf("SAC snapshot = %s/%d, want vwap/60", sac.Data.PriceType, sac.Data.WindowSeconds)
	}
	if !sac.Data.ObservedAt.Equal(pivotAt) {
		t.Errorf("SAC observed_at = %s, want the older leg's %s", sac.Data.ObservedAt, pivotAt)
	}
	if sac.Data.AssetID != pegAliasUSDCSAC || sac.Data.Quote != "fiat:USD" {
		t.Errorf("echo = %s/%s, want the requested %s/fiat:USD", sac.Data.AssetID, sac.Data.Quote, pegAliasUSDCSAC)
	}
	// One asset, one snapshot: everything but the echo is byte-equal.
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
	resp := mustGet(t, ts.URL+"/v1/price?asset="+pegAliasUSDCSAC+"&quote=fiat:USD")
	body, _ := readAll(resp)
	if !strings.Contains(body, `"sources":["bitstamp","coinbase","sdex"]`) {
		t.Errorf("sources must credit both legs' venues, sorted: %s", body)
	}
	assertNoPegSelfPairRead(t, reader.pairsAsked())
}

// dormantBookVsFreshPoolReader is the shape the widened peg leg must not
// invert: the peg's SDEX book under its classic id is DORMANT (a closed
// bucket the reader still serves, flagged stale) and a few-hundred-dollar
// Soroban pool under the peg's SAC id, quoted in the XLM SAC, is fresh.
// Crossed with the 0.10 pivot the two disagree by more than a factor of
// two — 0.95 from the book, 2.00 from the pool.
func dormantBookVsFreshPoolReader(at time.Time, bookStale bool) *recordingPriceReader {
	return &recordingPriceReader{stubPriceReader: stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			pegAliasUSDCClassic + "/native": {
				AssetID: pegAliasUSDCClassic, Quote: "native",
				Price: "9.5", PriceType: "vwap", ObservedAt: at, WindowSeconds: 60,
			},
			pegAliasUSDCSAC + "/" + canonical.XLMSacContractID: {
				AssetID: pegAliasUSDCSAC, Quote: canonical.XLMSacContractID,
				Price: "20.0", PriceType: "vwap", ObservedAt: at, WindowSeconds: 60,
			},
			"crypto:XLM/fiat:USD": {
				AssetID: "crypto:XLM", Quote: "fiat:USD",
				Price: "0.10", PriceType: "vwap", ObservedAt: at, WindowSeconds: 60,
			},
		},
		stale: map[string]bool{pegAliasUSDCClassic + "/native": bookStale},
		sources: map[string][]string{
			pegAliasUSDCClassic + "/native":                    {"sdex"},
			pegAliasUSDCSAC + "/" + canonical.XLMSacContractID: {"soroswap"},
			"crypto:XLM/fiat:USD":                              {"coinbase"},
		},
	}}
}

// TestPrice_DeclaredPegXLMCrossDormantClassicBookOutranksFreshSACPool
// pins the rule that keeps the classic spelling's served value exactly
// where it was before the peg leg learned the peg's SAC id: the leg
// walks ONE spelling at a time, and the peg's SAC form is reached only
// when the classic form FOUND NOTHING — every combination of it either
// missed the gate probe or read not-found. Not merely nothing fresh,
// which is this test; and not a refusal or a broken read, which end the
// walk (the two tests below).
//
// The classic id's SDEX book here is dormant, not absent: the reader
// still serves its closed bucket and flags it stale. A fresh print on a
// thin Soroban pool must not displace it. Ranking fresh-beats-stale
// across the peg's spellings would do exactly that — the thin-pool
// third-alias shape the family's SAC-last ordering exists to stop,
// arriving on a classic-keyed request, which is the shape
// `tipMergePairs` closed on /v1/price/tip.
//
// RED with the preference applied across spellings: price
// 2.0000000000, sources ["coinbase","soroswap"].
func TestPrice_DeclaredPegXLMCrossDormantClassicBookOutranksFreshSACPool(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	at := time.Unix(1745000000, 0).UTC()
	reader := dormantBookVsFreshPoolReader(at, true)
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
		PegDeclaredAt:     declaredPegAdoptedAt,
	})
	ts := startHTTPTest(t, srv.Handler())

	env := getPegEnvelope(t, ts.URL+"/v1/price?asset="+pegAliasUSDCClassic+"&quote=fiat:USD")
	if env.Data.Price != "0.9500000000" {
		t.Errorf("price = %q, want 0.9500000000 — the dormant SDEX book, not the fresh pool's 2.0000000000",
			env.Data.Price)
	}
	resp := mustGet(t, ts.URL+"/v1/price?asset="+pegAliasUSDCClassic+"&quote=fiat:USD")
	body, _ := readAll(resp)
	if !strings.Contains(body, `"sources":["coinbase","sdex"]`) {
		t.Errorf("sources must be the book's venues and the pivot's, without the pool: %s", body)
	}
	// The pool is not merely outranked — it is never read. A spelling
	// that answered at all ends the walk.
	if poolAt := callIndex(reader.pairsAsked(), pegAliasUSDCSAC+"/"+canonical.XLMSacContractID); poolAt >= 0 {
		t.Errorf("the peg's SAC book was read although its classic form answered (asked=%v)", reader.pairsAsked())
	}
	assertNoPegSelfPairRead(t, reader.pairsAsked())
}

// TestPrice_DeclaredPegXLMCrossOrdersTheSpellingsCanonicalFirst pins the
// ordering decision the widened leg makes: the peg's spellings are walked
// in the family's canonical order — classic first, the SAC wrapper last —
// NOT literal-first as [canonical.AssetAliases] documents for a caller's
// own spelling.
//
// Both books are populated and FRESH here, and they disagree: the SDEX
// book crosses to 0.95, the Soroban pool to 2.00. The leg prices one
// asset whichever id the caller typed, so both spellings must serve the
// deep book's value.
//
// RED with a literal-first walk (assetAliases(asset) in place of
// assetAliases(canonical.CanonicalAsset(asset))): the C-address serves
// 2.0000000000 from the pool while the classic id serves 0.9500000000
// from the book — two prices for one asset in one minute.
func TestPrice_DeclaredPegXLMCrossOrdersTheSpellingsCanonicalFirst(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	at := time.Unix(1745000000, 0).UTC()
	reader := dormantBookVsFreshPoolReader(at, false)
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
		PegDeclaredAt:     declaredPegAdoptedAt,
	})
	ts := startHTTPTest(t, srv.Handler())

	for _, spelling := range []string{pegAliasUSDCClassic, pegAliasUSDCSAC} {
		env := getPegEnvelope(t, ts.URL+"/v1/price?asset="+spelling+"&quote=fiat:USD")
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

// TestPrice_DeclaredPegXLMCrossReadsThePegsOwnSACBook pins the other
// half of "one asset": the peg leg of the cross reads the peg's whole
// family, not the spelling the caller typed. Soroban AMMs trade the SAC
// wrapper, so a peg whose only XLM book is a pool quoted in the XLM SAC
// stores that book under USDC-SAC / XLM-SAC — and both spellings must
// cross through it rather than fall to the declaration.
//
// RED before the leg looped the base family: the classic id read
// USDC-GA5Z… against XLM's three forms only, missed the pool, and served
// the declaration (1.000000000000, price_type peg) where the pool prints
// an observation (1.0000000000, price_type vwap).
func TestPrice_DeclaredPegXLMCrossReadsThePegsOwnSACBook(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	poolAt := time.Unix(1745000000, 0).UTC()
	reader := &recordingPriceReader{stubPriceReader: stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			pegAliasUSDCSAC + "/" + canonical.XLMSacContractID: {
				AssetID: pegAliasUSDCSAC, Quote: canonical.XLMSacContractID,
				Price: "10.0", PriceType: "vwap", ObservedAt: poolAt, WindowSeconds: 60,
			},
			"crypto:XLM/fiat:USD": {
				AssetID: "crypto:XLM", Quote: "fiat:USD",
				Price: "0.10", PriceType: "vwap", ObservedAt: poolAt, WindowSeconds: 60,
			},
		},
	}}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
		PegDeclaredAt:     declaredPegAdoptedAt,
	})
	ts := startHTTPTest(t, srv.Handler())

	for _, spelling := range []string{pegAliasUSDCClassic, pegAliasUSDCSAC} {
		env := getPegEnvelope(t, ts.URL+"/v1/price?asset="+spelling+"&quote=fiat:USD")
		if env.Data.Price != "1.0000000000" || env.Data.PriceType != "vwap" {
			t.Errorf("%s: served %s (%s), want the pool's cross 1.0000000000 (vwap), not the declaration",
				spelling, env.Data.Price, env.Data.PriceType)
		}
		if !env.Data.ObservedAt.Equal(poolAt) {
			t.Errorf("%s: observed_at = %s, want the pool's %s", spelling, env.Data.ObservedAt, poolAt)
		}
		if env.Data.AssetID != spelling {
			t.Errorf("asset_id = %q, want the requested %q", env.Data.AssetID, spelling)
		}
	}
	assertNoPegSelfPairRead(t, reader.pairsAsked())
}

// TestPrice_DeclaredPegSACTwinWithNoObservationServesTheDeclaration pins
// the last resort under the SAC spelling: with no market anywhere, the
// declaration serves — labelled `peg`, stamped with the adoption time —
// exactly as it does for the classic id, and echoes the C-address.
//
// RED before the alias-aware match: 404.
func TestPrice_DeclaredPegSACTwinWithNoObservationServesTheDeclaration(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	reader := &stubPriceReader{err: v1.ErrPriceNotFound}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
		PegDeclaredAt:     declaredPegAdoptedAt,
	})
	ts := startHTTPTest(t, srv.Handler())

	env := getPegEnvelope(t, ts.URL+"/v1/price?asset="+pegAliasUSDCSAC+"&quote=fiat:USD")
	if env.Data.Price != "1.000000000000" {
		t.Errorf("price = %q, want 1.000000000000 — the declaration is the last resort, not a 404", env.Data.Price)
	}
	if env.Data.PriceType != "peg" {
		t.Errorf("price_type = %q, want peg", env.Data.PriceType)
	}
	if !env.Data.ObservedAt.Equal(declaredPegAdoptedAt) {
		t.Errorf("observed_at = %s, want the declaration stamp %s", env.Data.ObservedAt, declaredPegAdoptedAt)
	}
	if !env.Flags.Triangulated {
		t.Errorf("flags.triangulated = false, want true")
	}
	if env.Data.AssetID != pegAliasUSDCSAC {
		t.Errorf("asset_id = %q, want the requested %q", env.Data.AssetID, pegAliasUSDCSAC)
	}
}

// TestPrice_DeclaredPegSACTwinNeverProbesItsOwnClassicForm pins what the
// sibling walk used to do with the SAC twin: with one declared peg, the
// only pair it could build was USDC-SAC / USDC-GA5Z… — one asset on both
// sides, no rows by construction, and a slot of the handler's deadline
// spent on it. That pair is never read under either orientation.
//
// RED before the alias-aware match: the reader was asked for
// CCW67…/USDC-GA5Z….
func TestPrice_DeclaredPegSACTwinNeverProbesItsOwnClassicForm(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	reader := &recordingPriceReader{}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
		PegDeclaredAt:     declaredPegAdoptedAt,
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price?asset="+pegAliasUSDCSAC+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("status = %d, want 200 (the declaration). Body: %s", resp.StatusCode, body)
	}
	asked := reader.pairsAsked()
	if len(asked) == 0 {
		t.Fatal("the reader was never consulted — the direct read and the cross must both run before the declaration")
	}
	assertNoPegSelfPairRead(t, asked)
}

// TestPrice_NonPegSACAssetStillWalksTheDeclaredPegs pins the boundary
// of the change: a SAC asset that is NOT the declared peg is untouched.
// It takes the sibling walk — its own spelling against the declared
// classic peg — and the XLM cross is not run for it.
func TestPrice_NonPegSACAssetStillWalksTheDeclaredPegs(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	poolAt := time.Unix(1745000000, 0).UTC()
	reader := &recordingPriceReader{stubPriceReader: stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			pegAliasAquaSAC + "/" + pegAliasUSDCClassic: {
				AssetID: pegAliasAquaSAC, Quote: pegAliasUSDCClassic,
				Price: "0.0041", PriceType: "vwap", ObservedAt: poolAt, WindowSeconds: 60,
			},
			// An XLM book for the same asset: present, and must not be
			// consulted — the cross is the declared peg's route only.
			pegAliasAquaSAC + "/native": {
				AssetID: pegAliasAquaSAC, Quote: "native",
				Price: "0.02", PriceType: "vwap", ObservedAt: poolAt, WindowSeconds: 60,
			},
			"crypto:XLM/fiat:USD": {
				AssetID: "crypto:XLM", Quote: "fiat:USD",
				Price: "0.10", PriceType: "vwap", ObservedAt: poolAt, WindowSeconds: 60,
			},
		},
	}}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
		PegDeclaredAt:     declaredPegAdoptedAt,
	})
	ts := startHTTPTest(t, srv.Handler())

	env := getPegEnvelope(t, ts.URL+"/v1/price?asset="+pegAliasAquaSAC+"&quote=fiat:USD")
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

// recordingRecentReader records every pair RecentClosedSnapshots was
// asked for, so a /v1/oracle/prices test can prove which markets the
// stablecoin fallback consulted.
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

// TestOraclePrices_DeclaredPegSACTwinNeverProbesItsOwnClassicForm pins
// the same self-pair guard on the sibling surface that walks the pegs
// with its own reader call: /v1/oracle/prices asked for the SAC twin must
// not read USDC-SAC / USDC-GA5Z… either.
//
// RED before the guard folded through the registry: the reader was asked
// for CCW67…/USDC-GA5Z….
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
// "<base>/<quote>" and records every pair it was asked for, so the
// point-in-time surfaces can prove which markets their peg walk
// consulted. A hit is stamped at the requested ts, so it is always
// inside the lookback and inside the changes surface's staleness bound.
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

// TestPriceAt_DeclaredPegSACTwinSkipsItsOwnFormAndWalksOn pins the
// self-pair guard on /v1/price/at, the CAGG sibling of the /v1/price
// walk. The guard must fold through the alias registry — the SAC twin
// asked for by its C-address is the same asset as the declared classic
// peg, and their pair is not a market — and it must skip only THAT peg:
// a second declared peg with no wrapper is still walked and still
// answers.
//
// RED with the exact-spelling guard (peg.Equal(asset)): the reader is
// asked for CCW67…/USDC-GA5Z… and USDC-GA5Z…/CCW67…, two pairs with no
// rows by construction, each spending a slot of the handler's deadline.
func TestPriceAt_DeclaredPegSACTwinSkipsItsOwnFormAndWalksOn(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	pyusd := mustClassicAsset(t, "PYUSD", pegAliasPYUSDIssuer)
	at := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	reader := &recordingPriceAtReader{byPair: map[string]string{
		pegAliasUSDCSAC + "/" + pegAliasPYUSDClassic: "1.0004",
	}}
	srv := v1.New(v1.Options{
		PriceAt:           reader,
		USDPeggedClassics: []canonical.Asset{usdc, pyusd},
	})
	ts := startHTTPTest(t, srv.Handler())

	env := getPegEnvelope(t, ts.URL+"/v1/price/at?asset="+pegAliasUSDCSAC+
		"&quote=fiat:USD&ts="+at.Format(time.RFC3339))
	if env.Data.Price != "1.0004" {
		t.Errorf("price = %q, want 1.0004 — the second declared peg must still be walked", env.Data.Price)
	}
	if env.Data.AssetID != pegAliasUSDCSAC || env.Data.Quote != "fiat:USD" {
		t.Errorf("echo = %s/%s, want the requested %s/fiat:USD", env.Data.AssetID, env.Data.Quote, pegAliasUSDCSAC)
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false, want true — served through a peg, not the requested quote")
	}
	asked := reader.pairsAsked()
	if callIndex(asked, pegAliasUSDCSAC+"/"+pegAliasPYUSDClassic) < 0 {
		t.Errorf("the second peg was never read (asked=%v)", asked)
	}
	assertNoPegSelfPairRead(t, asked)
}

// TestPriceChanges_DeclaredPegSACTwinSkipsItsOwnFormAndWalksOn is the
// same guard on /v1/price/changes, which anchors every horizon on the
// pair its own peg walk resolves. Same three claims: the SAC twin never
// probes its own classic form, a second declared peg is still admitted,
// and the answer is flagged triangulated.
//
// RED with the exact-spelling guard (peg.Equal(asset)): the reader is
// asked for CCW67…/USDC-GA5Z… and USDC-GA5Z…/CCW67…
func TestPriceChanges_DeclaredPegSACTwinSkipsItsOwnFormAndWalksOn(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	pyusd := mustClassicAsset(t, "PYUSD", pegAliasPYUSDIssuer)
	reader := &recordingPriceAtReader{byPair: map[string]string{
		pegAliasUSDCSAC + "/" + pegAliasPYUSDClassic: "1.0004",
	}}
	srv := v1.New(v1.Options{
		PriceAt:           reader,
		USDPeggedClassics: []canonical.Asset{usdc, pyusd},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/changes?asset="+pegAliasUSDCSAC+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		body, _ := readAll(resp)
		t.Fatalf("status = %d, want 200. Body: %s", resp.StatusCode, body)
	}
	body, _ := readAll(resp)
	for _, want := range []string{
		`"current_price":"1.0004"`,
		`"quote":"fiat:USD"`,
		`"triangulated":true`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\n%s", want, body)
		}
	}
	asked := reader.pairsAsked()
	if callIndex(asked, pegAliasUSDCSAC+"/"+pegAliasPYUSDClassic) < 0 {
		t.Errorf("the second peg was never read (asked=%v)", asked)
	}
	assertNoPegSelfPairRead(t, asked)
}

// pegLegErrorReader is a recordingPriceReader with PER-PAIR errors: the
// shape `stubPriceReader.err` cannot express, and the one the peg leg's
// spelling walk turns on. A pair listed in `errs` fails with exactly
// that error AFTER the call is recorded, so a test can prove both that
// the pair was asked for and what the walk did with the answer. Every
// other pair reads normally.
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
// classic XLM book is WITHHELD (a directory-flagged issuer, or trailing
// substance under the serve floor — either way a gate declined to
// publish it) while a live Soroban pool holds the peg's SAC id against
// the XLM SAC. `pricingguard.ScamGate.Withheld` returns false for any
// non-classic base, so that pool is scam-ungated: a walk that treated
// the refusal as "no market here" would republish the refused market
// through the ungated spelling.
func withheldClassicBookLivePoolReader(at time.Time, classicErr error) *pegLegErrorReader {
	return &pegLegErrorReader{
		recordingPriceReader: recordingPriceReader{stubPriceReader: stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{
				pegAliasUSDCClassic + "/native": {
					AssetID: pegAliasUSDCClassic, Quote: "native",
					Price: "9.5", PriceType: "vwap", ObservedAt: at, WindowSeconds: 60,
				},
				pegAliasUSDCSAC + "/" + canonical.XLMSacContractID: {
					AssetID: pegAliasUSDCSAC, Quote: canonical.XLMSacContractID,
					Price: "20.0", PriceType: "vwap", ObservedAt: at, WindowSeconds: 60,
				},
				"crypto:XLM/fiat:USD": {
					AssetID: "crypto:XLM", Quote: "fiat:USD",
					Price: "0.10", PriceType: "vwap", ObservedAt: at, WindowSeconds: 60,
				},
			},
			sources: map[string][]string{
				pegAliasUSDCClassic + "/native":                    {"sdex"},
				pegAliasUSDCSAC + "/" + canonical.XLMSacContractID: {"soroswap"},
				"crypto:XLM/fiat:USD":                              {"coinbase"},
			},
		}},
		errs: map[string]error{pegAliasUSDCClassic + "/native": classicErr},
	}
}

// assertDeclarationServed checks the wire shape the declaration prints:
// the flat 1:1 constant, labelled `peg`, stamped with the adoption time.
func assertDeclarationServed(t *testing.T, env pegEnvelope, spelling string) {
	t.Helper()
	if env.Data.Price != "1.000000000000" || env.Data.PriceType != "peg" {
		t.Errorf("%s: served %s (%s), want the declaration 1.000000000000 (peg)",
			spelling, env.Data.Price, env.Data.PriceType)
	}
	if !env.Data.ObservedAt.Equal(declaredPegAdoptedAt) {
		t.Errorf("%s: observed_at = %s, want the declaration stamp %s",
			spelling, env.Data.ObservedAt, declaredPegAdoptedAt)
	}
	if env.Data.AssetID != spelling {
		t.Errorf("asset_id = %q, want the requested %q", env.Data.AssetID, spelling)
	}
}

// TestPrice_DeclaredPegXLMCrossWithheldClassicBookNeverReachesTheSACBook
// pins the gate boundary the widened peg leg must not open. A withheld
// leg is a miss, not a price — this route's own contract — and it is
// also not permission to go looking for ANOTHER SPELLING of the same
// asset, because the two spellings are not gated alike:
// `pricingguard.SubstanceGate.measure` unions aliases on both sides and
// so reaches the same verdict for either, but
// `pricingguard.ScamGate.Withheld` (internal/pricingguard/scam.go:163)
// returns false for any base that is not classic-with-issuer, so the
// peg's SAC book is scam-ungated. Advancing on a refusal would publish,
// through the ungated spelling, exactly the market a directory-flagged
// issuer had withheld.
//
// Both spellings must serve what the classic id served before the peg's
// SAC form joined the walk: the declaration.
//
// RED with a withheld leg treated as "no market" (pegXLMLegNoMarket in
// place of pegXLMLegRefused): price 2.0000000000, price_type vwap,
// sources ["coinbase","soroswap"] — the refused market republished
// through the pool.
func TestPrice_DeclaredPegXLMCrossWithheldClassicBookNeverReachesTheSACBook(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	at := time.Unix(1745000000, 0).UTC()
	reader := withheldClassicBookLivePoolReader(at, v1.ErrPriceWithheld)
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
		PegDeclaredAt:     declaredPegAdoptedAt,
	})
	ts := startHTTPTest(t, srv.Handler())

	for _, spelling := range []string{pegAliasUSDCClassic, pegAliasUSDCSAC} {
		env := getPegEnvelope(t, ts.URL+"/v1/price?asset="+spelling+"&quote=fiat:USD")
		assertDeclarationServed(t, env, spelling)
	}
	asked := reader.pairsAsked()
	if callIndex(asked, pegAliasUSDCClassic+"/native") < 0 {
		t.Errorf("the classic book was never read (asked=%v)", asked)
	}
	if callIndex(asked, pegAliasUSDCSAC+"/"+canonical.XLMSacContractID) >= 0 {
		t.Errorf("the peg's SAC book was read after its classic book was WITHHELD (asked=%v) — "+
			"a refusal must end the spelling walk, not redirect it to an ungated spelling", asked)
	}
	assertNoPegSelfPairRead(t, asked)
}

// TestPrice_DeclaredPegXLMCrossReadFailureNeverReachesTheSACBook pins
// the same boundary for a per-pair READ FAILURE, which says nothing
// about whether a market exists. The failure class is not hypothetical:
// v0.60.0 shipped a Postgres 42883 planning error that failed 1,651
// times on one pair while every other pair read normally. Handing over
// to the peg's SAC spelling on such an error would silently reprice the
// peg off whatever thin pool that spelling holds — here a 2x move — the
// moment one pair's plan broke.
//
// RED with a read error treated as "no market" (pegXLMLegNoMarket in
// place of pegXLMLegReadFailed): price 2.0000000000, price_type vwap.
func TestPrice_DeclaredPegXLMCrossReadFailureNeverReachesTheSACBook(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	at := time.Unix(1745000000, 0).UTC()
	planningErr := errors.New(
		`ERROR: operator does not exist: numeric = text (SQLSTATE 42883)`)
	reader := withheldClassicBookLivePoolReader(at, planningErr)
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
		PegDeclaredAt:     declaredPegAdoptedAt,
	})
	ts := startHTTPTest(t, srv.Handler())

	env := getPegEnvelope(t, ts.URL+"/v1/price?asset="+pegAliasUSDCClassic+"&quote=fiat:USD")
	assertDeclarationServed(t, env, pegAliasUSDCClassic)
	asked := reader.pairsAsked()
	if callIndex(asked, pegAliasUSDCSAC+"/"+canonical.XLMSacContractID) >= 0 {
		t.Errorf("the peg's SAC book was read after the classic book's read FAILED (asked=%v) — "+
			"a broken read is not evidence that the classic book is empty", asked)
	}
	assertNoPegSelfPairRead(t, asked)
}

// gatingPegReader wires the optional `proxyPairGate` — the bounded
// recent-bucket probe that decides, before any unbounded last-trade
// scan, whether a peg-leg combination is worth reading — onto the
// recording reader, and can make that probe MISS (no recent bucket) or
// FAIL (a probe outage). Both are answers the gate gives on the live
// path and neither was exercised by any test of this route.
//
// `exists` is keyed "<base>/<quote>" and defaults to false, matching the
// probe's own meaning: no closed 1m bucket in the freshness horizon.
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

func gatingDormantBookReader(at time.Time) *gatingPegReader {
	return &gatingPegReader{recordingPriceReader: *dormantBookVsFreshPoolReader(at, false)}
}

// TestPrice_DeclaredPegXLMCrossGateMissIsWhatAdvancesTheSpellingWalk
// pins the probe MISS as the thing that means "this spelling found
// nothing". The reader here HOLDS the classic book, but the gate reports
// no recent closed bucket for it — the dormant-pair shape the probe
// exists to skip before an unbounded last-trade scan — so the classic
// spelling must never be read at all, and the peg's SAC pool, which the
// gate reports live, prices the cross.
//
// Nothing wired this gate on the peg leg before, so neither the skip nor
// the read-through below was exercised by any test, on either side of
// the walk that turns on them.
//
// RED with a gate miss reported as a read failure (pegXLMLegReadFailed
// in place of pegXLMLegNoMarket): the walk stops on the gated-out
// classic spelling and serves the declaration 1.000000000000 (peg).
func TestPrice_DeclaredPegXLMCrossGateMissIsWhatAdvancesTheSpellingWalk(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	at := time.Unix(1745000000, 0).UTC()
	reader := gatingDormantBookReader(at)
	reader.exists = map[string]bool{
		pegAliasUSDCSAC + "/" + canonical.XLMSacContractID: true,
	}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
		PegDeclaredAt:     declaredPegAdoptedAt,
	})
	ts := startHTTPTest(t, srv.Handler())

	env := getPegEnvelope(t, ts.URL+"/v1/price?asset="+pegAliasUSDCClassic+"&quote=fiat:USD")
	if env.Data.Price != "2.0000000000" || env.Data.PriceType != "vwap" {
		t.Errorf("served %s (%s), want the pool's cross 2.0000000000 (vwap) — a gate miss is the one "+
			"verdict that advances the spelling walk", env.Data.Price, env.Data.PriceType)
	}
	if probed := reader.probesMade(); callIndex(probed, pegAliasUSDCClassic+"/native") < 0 {
		t.Errorf("the classic book was never probed (probes=%v)", probed)
	}
	if asked := reader.pairsAsked(); callIndex(asked, pegAliasUSDCClassic+"/native") >= 0 {
		t.Errorf("the gated-out classic book was READ (asked=%v) — the probe exists to skip that "+
			"unbounded last-trade scan", asked)
	}
}

// TestPrice_DeclaredPegXLMCrossGateErrorReadsThroughToTheClassicBook
// pins the other half: a probe OUTAGE must not hide a price, and must
// not hand the peg's price to another spelling either. The gate errors
// on the classic book, the walk reads it anyway, it answers, and the
// SAC pool — which would cross more than 2x higher — is never reached.
//
// RED with the gate's error ignored (`if !exists` in place of
// `if gerr == nil && !exists`): the failing probe skips the classic
// book, the walk advances, and the pool serves 2.0000000000.
func TestPrice_DeclaredPegXLMCrossGateErrorReadsThroughToTheClassicBook(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	at := time.Unix(1745000000, 0).UTC()
	reader := gatingDormantBookReader(at)
	reader.exists = map[string]bool{
		pegAliasUSDCSAC + "/" + canonical.XLMSacContractID: true,
	}
	reader.existsErr = map[string]error{
		pegAliasUSDCClassic + "/native": errors.New("probe unavailable"),
	}
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
		PegDeclaredAt:     declaredPegAdoptedAt,
	})
	ts := startHTTPTest(t, srv.Handler())

	env := getPegEnvelope(t, ts.URL+"/v1/price?asset="+pegAliasUSDCClassic+"&quote=fiat:USD")
	if env.Data.Price != "0.9500000000" || env.Data.PriceType != "vwap" {
		t.Errorf("served %s (%s), want the classic book's cross 0.9500000000 (vwap) — a probe blip "+
			"must not hide a price", env.Data.Price, env.Data.PriceType)
	}
	if asked := reader.pairsAsked(); callIndex(asked, pegAliasUSDCSAC+"/"+canonical.XLMSacContractID) >= 0 {
		t.Errorf("the peg's SAC book was read although the classic book answered (asked=%v)", asked)
	}
}

// TestPrice_DeclaredPegXLMCrossDegenerateClassicPriceNeverReachesTheSACBook
// pins the resolution the leg's godoc states for the third non-price a
// combination can produce: a price that READ but is zero, negative or
// unparsable. The spelling ANSWERED, so the walk ends there — the same
// way it ends on a refusal and on a broken read — `crossThroughPivot`
// then declines the degenerate product and the declaration serves.
//
// Resolving it the other way, as "found nothing", would let one
// degenerate print on the deep book hand the peg's price to whatever
// thin pool its SAC spelling holds: here a 2x move off a single zero.
// This arm is what the classic id already did before its SAC spelling
// joined the walk, so the pin is a no-change guard on that behaviour,
// not a new rule.
func TestPrice_DeclaredPegXLMCrossDegenerateClassicPriceNeverReachesTheSACBook(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	at := time.Unix(1745000000, 0).UTC()
	reader := dormantBookVsFreshPoolReader(at, false)
	book := reader.snapshots[pegAliasUSDCClassic+"/native"]
	book.Price = "0"
	reader.snapshots[pegAliasUSDCClassic+"/native"] = book
	srv := v1.New(v1.Options{
		Prices:            reader,
		USDPeggedClassics: []canonical.Asset{usdc},
		PegDeclaredAt:     declaredPegAdoptedAt,
	})
	ts := startHTTPTest(t, srv.Handler())

	env := getPegEnvelope(t, ts.URL+"/v1/price?asset="+pegAliasUSDCClassic+"&quote=fiat:USD")
	assertDeclarationServed(t, env, pegAliasUSDCClassic)
	if asked := reader.pairsAsked(); callIndex(asked, pegAliasUSDCSAC+"/"+canonical.XLMSacContractID) >= 0 {
		t.Errorf("the peg's SAC book was read although the classic book returned a row (asked=%v) — "+
			"the walk advances on FOUND NOTHING, never on found-something-unusable", asked)
	}
}
