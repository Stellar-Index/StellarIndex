// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The thin-pool third-alias shape (launch-plan D7, C4-012/13), pinned on
// /v1/price — the surface that publishes "the price of X is P".
//
// Every classic asset with a configured SAC wrapper has two markets that
// are the SAME asset in two spellings: the SDEX book, stored
// classic/classic (`AQUA-G…/USDC-G…`), and the Soroban pools, stored
// SAC/SAC (`CAUIK…/CCW67…`). The pools are routinely orders of magnitude
// thinner. The alias walk in readPriceWithAliases prefers a FRESH alias
// over a STALE one, so the one arrangement that would let a few hundred
// dollars of Soroban depth become the served price of an asset whose
// real depth sits on SDEX is: the deep book quiet for longer than the
// freshness window, the thin pool printed this minute. These tests fix
// that shape as data and assert what the walk may and may not read.
//
// The load-bearing property is that the walk crosses the base's alias
// family with the LITERAL quote — never with the quote's alias family.
// A Soroban pool carries the SAC form on BOTH legs (the decoders stamp
// contract ids: internal/sources/{soroswap,aquarius,phoenix}/decode.go),
// so a classic-quoted read can never land on it, and the deep book's
// stale bucket is served, flagged stale, rather than the pool's fresh
// one. Nothing in the code spells that property out — it falls out of
// the loop shape — which is exactly why it needs a test: widening
// readPriceWithAliases to walk the quote aliases too (as the series
// paths and /v1/price/at legitimately do for their own reasons) would
// silently turn the thin pool into the served price here.
//
// NOT parallel: installPegAliasRegistry publishes the process-wide
// registry (see canonical.InstallAliasRegistry).

// orderedPriceReader is a stubPriceReader that records every (base,
// quote) pair LatestPrice was asked for, in call order, so a test can
// prove not just which pair answered but which pairs were NEVER
// consulted. It also satisfies the optional proxyPairGate seam, so the
// stablecoin proxy walk runs in its production (gated) shape: a pair is
// "recent" exactly when the stub holds a snapshot for it.
type orderedPriceReader struct {
	stubPriceReader
	calls []string
}

func (r *orderedPriceReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	r.calls = append(r.calls, a.String()+"/"+q.String())
	return r.stubPriceReader.LatestPrice(ctx, a, q)
}

func (r *orderedPriceReader) RecentClosedVWAP1mExists(_ context.Context, base, quote canonical.Asset) (bool, error) {
	_, ok := r.snapshots[base.String()+"/"+quote.String()]
	return ok, nil
}

// thinPoolFixture is the D7 shape for one wrapped classic asset:
//
//   - deepPair  — the SDEX book, classic/classic, deep but QUIET: its
//     latest closed bucket is older than the freshness window, so the
//     reader reports it stale.
//   - thinPair  — the Soroban pool, SAC/SAC, thin but FRESH: one trade
//     printed this minute at five times the book's price.
type thinPoolFixture struct {
	reader   *orderedPriceReader
	deepPair string
	thinPair string
}

const (
	thinPoolDeepPrice = "0.0010"
	thinPoolThinPrice = "0.0050"
)

func newThinPoolFixture(t *testing.T, deepBase, deepQuote, thinBase, thinQuote string) thinPoolFixture {
	t.Helper()
	deep := deepBase + "/" + deepQuote
	thin := thinBase + "/" + thinQuote
	reader := &orderedPriceReader{stubPriceReader: stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			deep: {
				AssetID: deepBase, Quote: deepQuote, Price: thinPoolDeepPrice, PriceType: "vwap",
				ObservedAt: time.Now().Add(-2 * time.Hour).UTC(),
			},
			thin: {
				AssetID: thinBase, Quote: thinQuote, Price: thinPoolThinPrice, PriceType: "vwap",
				ObservedAt: time.Now().UTC(),
			},
		},
		stale:   map[string]bool{deep: true, thin: false},
		sources: map[string][]string{deep: {"sdex"}, thin: {"soroswap"}},
	}}
	return thinPoolFixture{reader: reader, deepPair: deep, thinPair: thin}
}

type thinPoolEnvelope struct {
	Data    v1.PriceSnapshot `json:"data"`
	Flags   v1.Flags         `json:"flags"`
	Sources []string         `json:"sources"`
}

func getThinPoolPrice(t *testing.T, url string) (int, thinPoolEnvelope, string) {
	t.Helper()
	resp := mustGet(t, url)
	body, _ := readAll(resp)
	var env thinPoolEnvelope
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatalf("decode: %v: %s", err, body)
		}
	}
	return resp.StatusCode, env, body
}

// assertNoSACQuotedRead is the invariant every classic-quoted test below
// shares: no LatestPrice call may bind a SAC form as the QUOTE.
func assertNoSACQuotedRead(t *testing.T, calls []string) {
	t.Helper()
	for _, c := range calls {
		q := c[strings.LastIndex(c, "/")+1:]
		if a, err := canonical.ParseAsset(q); err == nil && a.Type == canonical.AssetSoroban {
			t.Errorf("LatestPrice read %q — the walk bound a SAC form as the quote; the thin SAC/SAC pool is reachable from a classic-quoted request", c)
		}
	}
}

func assertCallOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("LatestPrice call order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("LatestPrice call[%d] = %q, want %q (full order %v)", i, got[i], want[i], got)
		}
	}
}

// TestPrice_ThinPoolThirdAlias_ClassicQuoteServesTheQuietBook: the
// classic-keyed, classic-quoted read serves the deep book's STALE bucket,
// flagged stale, and never consults the fresh SAC/SAC pool. The base's
// SAC form IS walked (second, against the literal classic quote — a pair
// no venue produces), which is what makes the assertion non-vacuous: the
// alias family reached the SAC spelling and still could not land on the
// pool.
func TestPrice_ThinPoolThirdAlias_ClassicQuoteServesTheQuietBook(t *testing.T) {
	installPegAliasRegistry(t)
	fx := newThinPoolFixture(t, pegAliasAquaClassic, pegAliasUSDCClassic, pegAliasAquaSAC, pegAliasUSDCSAC)
	srv := v1.New(v1.Options{Prices: fx.reader})
	ts := startHTTPTest(t, srv.Handler())

	status, env, body := getThinPoolPrice(t, ts.URL+"/v1/price?asset="+pegAliasAquaClassic+"&quote="+pegAliasUSDCClassic)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if env.Data.Price != thinPoolDeepPrice {
		t.Errorf("price = %q, want the quiet SDEX book's %q — the fresh Soroban pool must not displace it: %s", env.Data.Price, thinPoolDeepPrice, body)
	}
	if !env.Flags.Stale {
		t.Errorf("stale = false, want true — a quiet book served as a price must say so: %s", body)
	}
	if got := strings.Join(env.Sources, ","); got != "sdex" {
		t.Errorf("sources = %q, want sdex", got)
	}
	assertCallOrder(t, fx.reader.calls, []string{
		pegAliasAquaClassic + "/" + pegAliasUSDCClassic,
		pegAliasAquaSAC + "/" + pegAliasUSDCClassic,
	})
	assertNoSACQuotedRead(t, fx.reader.calls)
}

// TestPrice_ThinPoolThirdAlias_NativeQuoteWalkStaysOnTheLiteralQuote is
// the same shape for XLM, whose three-way family is unconditional
// (native / crypto:XLM / the XLM SAC) and whose Soroban book is stored
// SAC/SAC (measured on r1 2026-09-03 — see the queryDB doc in
// internal/storage/timescale/usd_fx_resolver.go). All three base forms
// are walked, in canonical order, every one against classic USDC.
func TestPrice_ThinPoolThirdAlias_NativeQuoteWalkStaysOnTheLiteralQuote(t *testing.T) {
	installPegAliasRegistry(t)
	fx := newThinPoolFixture(t, "native", pegAliasUSDCClassic, canonical.XLMSacContractID, pegAliasUSDCSAC)
	srv := v1.New(v1.Options{Prices: fx.reader})
	ts := startHTTPTest(t, srv.Handler())

	status, env, body := getThinPoolPrice(t, ts.URL+"/v1/price?asset=native&quote="+pegAliasUSDCClassic)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if env.Data.Price != thinPoolDeepPrice || !env.Flags.Stale {
		t.Errorf("price/stale = %q/%t, want %q/true (the SDEX bucket, flagged stale): %s", env.Data.Price, env.Flags.Stale, thinPoolDeepPrice, body)
	}
	assertCallOrder(t, fx.reader.calls, []string{
		"native/" + pegAliasUSDCClassic,
		"crypto:XLM/" + pegAliasUSDCClassic,
		canonical.XLMSacContractID + "/" + pegAliasUSDCClassic,
	})
	assertNoSACQuotedRead(t, fx.reader.calls)
}

// TestPrice_ThinPoolThirdAlias_FiatProxyWalksClassicPegsOnly covers the
// default request shape, ?quote=fiat:USD, which no on-chain venue quotes
// and which therefore resolves through the stablecoin proxy. The proxy
// walks the operator's declared pegs in their CLASSIC spelling only
// (parseUSDPeggedClassics in the API binary rejects any other form), so
// the deep book answers and the SAC-quoted pool is never a candidate —
// even though it is fresher.
func TestPrice_ThinPoolThirdAlias_FiatProxyWalksClassicPegsOnly(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	fx := newThinPoolFixture(t, pegAliasAquaClassic, pegAliasUSDCClassic, pegAliasAquaSAC, pegAliasUSDCSAC)
	srv := v1.New(v1.Options{Prices: fx.reader, USDPeggedClassics: []canonical.Asset{usdc}})
	ts := startHTTPTest(t, srv.Handler())

	status, env, body := getThinPoolPrice(t, ts.URL+"/v1/price?asset="+pegAliasAquaClassic+"&quote=fiat:USD")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if env.Data.Price != thinPoolDeepPrice || !env.Flags.Triangulated {
		t.Errorf("price/triangulated = %q/%t, want %q/true (the classic-peg proxy of the SDEX book): %s", env.Data.Price, env.Flags.Triangulated, thinPoolDeepPrice, body)
	}
	assertCallOrder(t, fx.reader.calls, []string{
		pegAliasAquaClassic + "/fiat:USD",
		pegAliasAquaSAC + "/fiat:USD",
		pegAliasAquaClassic + "/" + pegAliasUSDCClassic,
	})
	assertNoSACQuotedRead(t, fx.reader.calls)
}

// TestPrice_ThinPoolThirdAlias_SACKeyedRequestServesTheNamedPool states
// the other half of the contract, and proves the pool is reachable at
// all (so the three tests above are not passing against a pair the stub
// could never have answered): a caller who names the SAC form on BOTH
// sides is asking about that pool, and gets that pool's own price, its
// own sources, and no alias fallback ahead of it. The thin-market gates
// that then apply are the reader's (substance, trailing-baseline guard,
// freshness) — documented in docs/audit/d7-thin-pool-third-alias-vwap-review-2026-09-04.md.
func TestPrice_ThinPoolThirdAlias_SACKeyedRequestServesTheNamedPool(t *testing.T) {
	installPegAliasRegistry(t)
	fx := newThinPoolFixture(t, pegAliasAquaClassic, pegAliasUSDCClassic, pegAliasAquaSAC, pegAliasUSDCSAC)
	srv := v1.New(v1.Options{Prices: fx.reader})
	ts := startHTTPTest(t, srv.Handler())

	status, env, body := getThinPoolPrice(t, ts.URL+"/v1/price?asset="+pegAliasAquaSAC+"&quote="+pegAliasUSDCSAC)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if env.Data.Price != thinPoolThinPrice || env.Flags.Stale {
		t.Errorf("price/stale = %q/%t, want %q/false (the named pool's own bucket): %s", env.Data.Price, env.Flags.Stale, thinPoolThinPrice, body)
	}
	if env.Data.AssetID != pegAliasAquaSAC || env.Data.Quote != pegAliasUSDCSAC {
		t.Errorf("echo = %s/%s, want the requested SAC spellings", env.Data.AssetID, env.Data.Quote)
	}
	assertCallOrder(t, fx.reader.calls, []string{pegAliasAquaSAC + "/" + pegAliasUSDCSAC})
}
