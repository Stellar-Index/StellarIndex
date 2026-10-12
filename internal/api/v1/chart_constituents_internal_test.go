// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"sort"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The chart's proxy enumeration is ordered: established quote spellings across every peg
// family first, held-back SAC spellings after all of them. Under the per-bucket merge the
// order decides which source owns a bucket. Only order may change; losing a market is a defect.

const (
	chartConstUSDCIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	chartConstUSDCSAC    = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	chartConstPYUSDIsser = "GDQE7IXJ4HUHV6RQHIUPRJSEZE4DRS5WY577O2FY6YQ5LVWZ7JZTU2V5"
)

// chartConstServer wires a server with two declared USD pegs, one of
// which has a SAC wrapper — the deployed shape, and the one where the
// two passes differ from a per-family interleave.
func chartConstServer(t *testing.T) *Server {
	t.Helper()
	reg, err := canonical.NewAliasRegistry(canonical.PubnetPassphrase, map[string]string{
		chartConstUSDCSAC: "USDC:" + chartConstUSDCIssuer,
	})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	usdc, err := canonical.NewClassicAsset("USDC", chartConstUSDCIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset(USDC): %v", err)
	}
	pyusd, err := canonical.NewClassicAsset("PYUSD", chartConstPYUSDIsser)
	if err != nil {
		t.Fatalf("NewClassicAsset(PYUSD): %v", err)
	}
	return New(Options{USDPeggedClassics: []canonical.Asset{usdc, pyusd}})
}

func chartConstNativeUSD(t *testing.T) canonical.Pair {
	t.Helper()
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("NewFiatAsset: %v", err)
	}
	p, err := canonical.NewPair(canonical.NativeAsset(), usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return p
}

func pairKeys(pairs []canonical.Pair) []string {
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.Base.String()+"/"+p.Quote.String())
	}
	return out
}

// TestChartFiatProxyPairs_ReachIsUnchanged pins the set: every base alias crossed with [Server.usdPegProxyQuotes] and the fiat's
// abstract backers, minus the literal pair and same-asset combinations. Built as the reference construction so dropping a form
// fails.
func TestChartFiatProxyPairs_ReachIsUnchanged(t *testing.T) {
	s := chartConstServer(t)
	pair := chartConstNativeUSD(t)

	var quotes []canonical.Asset
	quotes = append(quotes, s.usdPegProxyQuotes()...)
	backers := aggregate.FiatBackers(pair.Quote.Code)
	sort.Strings(backers)
	for _, code := range backers {
		a, err := canonical.NewCryptoAsset(code)
		if err != nil {
			t.Fatalf("NewCryptoAsset(%s): %v", code, err)
		}
		quotes = append(quotes, a)
	}
	want := map[string]struct{}{}
	literal := pair.Base.String() + "/" + pair.Quote.String()
	for _, b := range assetAliases(pair.Base) {
		for _, q := range quotes {
			if sameAsset(q, b) {
				continue
			}
			pp, err := canonical.NewPair(b, q)
			if err != nil {
				continue
			}
			k := pp.Base.String() + "/" + pp.Quote.String()
			if k == literal {
				continue
			}
			want[k] = struct{}{}
		}
	}

	got := map[string]struct{}{}
	for _, k := range pairKeys(s.chartFiatProxyPairs(pair)) {
		if _, dup := got[k]; dup {
			t.Errorf("%s enumerated twice", k)
		}
		got[k] = struct{}{}
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("%s dropped from the proxy walk — the split reorders, it must not narrow", k)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s added to the proxy walk", k)
		}
	}
}

// TestChartFiatProxyPairs_EveryEstablishedSpellingBeforeAnyHeldBack pins the order the per-bucket merge depends on: a thin
// pool's SAC form must not be reached before another family's classic form, or a few prints own a bucket the book can answer.
func TestChartFiatProxyPairs_EveryEstablishedSpellingBeforeAnyHeldBack(t *testing.T) {
	s := chartConstServer(t)
	pair := chartConstNativeUSD(t)

	established, heldBack := s.chartFiatProxyQuotes(pair.Quote)
	if len(heldBack) == 0 {
		t.Fatal("no held-back quote spellings — the fixture declares a SAC wrapper")
	}
	heldSet := map[string]struct{}{}
	for _, q := range heldBack {
		heldSet[q.String()] = struct{}{}
	}
	for _, q := range established {
		if _, bad := heldSet[q.String()]; bad {
			t.Errorf("%s is in both sets", q.String())
		}
	}

	walk := s.chartFiatProxyPairs(pair)
	lastEstablished, firstHeldBack := -1, -1
	for i, p := range walk {
		if _, held := heldSet[p.Quote.String()]; held {
			if firstHeldBack < 0 {
				firstHeldBack = i
			}
			continue
		}
		lastEstablished = i
	}
	if firstHeldBack < 0 {
		t.Fatalf("the walk enumerated no held-back spelling at all: %v", pairKeys(walk))
	}
	if firstHeldBack < lastEstablished {
		t.Errorf("a held-back spelling is enumerated at %d, before the last established one at %d: %v",
			firstHeldBack, lastEstablished, pairKeys(walk))
	}
}

// TestChartConstituents_CoverTheOHLCCombine pins that every market the fiat OHLC combine reads is also read by the chart's walk.
// The converse is deliberately not asserted: the chart derives through XLM where /v1/ohlc cannot, and closing that gap is not a
// regression.
func TestChartConstituents_CoverTheOHLCCombine(t *testing.T) {
	s := chartConstServer(t)
	pair := chartConstNativeUSD(t)

	chartSide := map[string]struct{}{}
	for _, k := range pairKeys(s.chartAliasPairs(pair)) {
		chartSide[k] = struct{}{}
	}
	for _, k := range pairKeys(s.chartFiatProxyPairs(pair)) {
		chartSide[k] = struct{}{}
	}

	missing := 0
	for _, sp := range s.usdPeggedConstituents(pair) {
		k := sp.Base.String() + "/" + sp.Quote.String()
		flipped := sp.Quote.String() + "/" + sp.Base.String()
		_, direct := chartSide[k]
		_, flip := chartSide[flipped]
		if !direct && !flip {
			missing++
			t.Errorf("/v1/ohlc reads %s and /v1/chart does not", k)
		}
	}
	if missing == 0 && len(chartSide) == 0 {
		t.Fatal("chart walk enumerated nothing — the assertion would hold vacuously")
	}
}
