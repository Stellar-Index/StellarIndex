// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// /v1/chart withheld a directory-scam-flagged issuer's price SERIES
// while /v1/history/since-inception served the identical
// trajectory at 200 — the same pair, the same CAGG VWAP chain (the
// handler's own comment says so), differing only in the read closure.
// Gating one and not the other bought nothing: `?timeframe=all` and
// since-inception answer the same question.
//
// scam.go promises the RAW surfaces stay visible — /v1/history's trade
// rows, /v1/observations, /v1/ohlc — and that promise is kept. The
// distinction is raw trades versus an AGGREGATED price claim, not the
// route prefix: this endpoint is named for the raw family but serves
// bucketed VWAP.

// A flagged base must be withheld — and withheld BEFORE the read, so a
// flagged issuer's series never leaves the store.
func TestHistorySinceInception_WithholdsScamFlaggedIssuer(t *testing.T) {
	base := chartFlaggedBase(t)
	gate := &chartScamGate{withheld: map[string]bool{base.String(): true}}
	reader := &stubHistoryReader{points: []v1.HistoryPoint{}}
	srv := v1.New(v1.Options{History: reader, Scam: gate})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset="+base.String()+"&quote=native&granularity=1d")
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — a flagged issuer's VWAP series must be withheld here "+
			"exactly as it is on /v1/chart?timeframe=all, which reads the same chain. Body: %s",
			resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("body missing the price-withheld problem type: %s", body)
	}
	if len(gate.surfaces) == 0 || gate.surfaces[0] != "history_series" {
		t.Errorf("gate surfaces = %v, want the first to be \"history_series\" — the label is how an "+
			"operator attributes a withholding decision, and reusing \"chart\" would hide this "+
			"surface inside the other's counter", gate.surfaces)
	}
	if reader.lastCall.granularity != "" {
		t.Errorf("HistoryPoints was called (granularity=%q) despite the withholding — the gate must "+
			"short-circuit BEFORE the read", reader.lastCall.granularity)
	}
}

// Blast-radius guard: an UNFLAGGED pair must still be served in full.
func TestHistorySinceInception_ServesUnflaggedIssuer(t *testing.T) {
	gate := &chartScamGate{withheld: map[string]bool{}} // wired, flags nothing
	reader := &stubHistoryReader{points: []v1.HistoryPoint{}}
	srv := v1.New(v1.Options{History: reader, Scam: gate})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset=native&quote="+w2t2USDC+"&granularity=1d")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the gate must withhold only flagged bases. Body: %s",
			resp.StatusCode, body)
	}
	if reader.lastCall.granularity != "1d" {
		t.Errorf("HistoryPoints granularity = %q, want 1d — the read must still happen for an "+
			"unflagged pair", reader.lastCall.granularity)
	}
}

// A deployment with no gate wired must keep serving; every other gate
// in this package is nil-safe and this call site is not the exception.
func TestHistorySinceInception_NilGateServes(t *testing.T) {
	reader := &stubHistoryReader{points: []v1.HistoryPoint{}}
	srv := v1.New(v1.Options{History: reader}) // no Scam
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset=native&quote="+w2t2USDC+"&granularity=1d")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a nil scam gate broke since-inception: status = %d. Body: %s", resp.StatusCode, body)
	}
}

// The gate keys on the BASE, so a flagged asset cannot slip through by
// naming a different quote — the frontend triangulates through XLM.
func TestHistorySinceInception_GateKeysOnBaseNotQuote(t *testing.T) {
	base := chartFlaggedBase(t)
	gate := &chartScamGate{withheld: map[string]bool{base.String(): true}}
	reader := &stubHistoryReader{points: []v1.HistoryPoint{}}
	srv := v1.New(v1.Options{History: reader, Scam: gate})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset="+base.String()+"&quote="+w2t2USDC+"&granularity=1d")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("a different quote returned %d, want 404 — the gate keys on the BASE", resp.StatusCode)
	}
}

// The RAW trade surface stays visible — the promise scam.go, substance.go
// and the withheld problem's own escape-hatch guidance all make. If this
// ever 404s, the gate was pushed down into the shared reader and our own
// error message became a lie.
func TestHistorySinceInception_RawTradesSurfaceStaysVisible(t *testing.T) {
	base := chartFlaggedBase(t)
	gate := &chartScamGate{withheld: map[string]bool{base.String(): true}}
	srv := v1.New(v1.Options{History: &stubHistoryReader{}, Scam: gate})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history?asset="+base.String()+"&quote=native")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound && strings.Contains(string(body), "price-withheld") {
		t.Errorf("/v1/history (RAW trades) was withheld — scam.go promises the raw surfaces stay "+
			"visible; only the aggregated price claim is gated. Body: %s", body)
	}
}

// TestHistorySinceInception_AssetAliasFirstHit — the series is keyed
// under crypto:XLM/<USDC>; a ?asset=native query must resolve it via the
// alias loop instead of returning an empty points array.
func TestHistorySinceInception_AssetAliasFirstHit(t *testing.T) {
	usdc, err := canonical.ParseAsset(w2t2USDC)
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	cryptoXLM, _ := canonical.ParseAsset("crypto:XLM")
	cryptoPair, _ := canonical.NewPair(cryptoXLM, usdc)

	reader := &stubHistoryReader{
		pointsByPair: map[string][]v1.HistoryPoint{
			cryptoPair.String(): {{
				Bucket: time.Unix(1_772_000_000, 0).UTC(),
				VWAP:   "0.1600000000",
			}},
		},
	}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset=native&quote="+usdc.String()+"&granularity=1h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"0.1600000000"`) {
		t.Errorf("body missing crypto:XLM-keyed history point 0.16 (alias loop should surface it): %s", body)
	}
}

// /v1/history/since-inception shares the chart's read chain, so it serves the
// merged series.
func TestHistorySinceInception_HoledSeriesReachesTheProxyWalk(t *testing.T) {
	ts := holedServer(t, holedFlagshipStore())

	env := getSinceInception(t, ts)
	if got := len(env.Data.Points); got != 40 {
		t.Fatalf("points = %d, want 40 — since-inception must serve the pool's buckets too", got)
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false; 25 buckets came through the peg's SAC form")
	}
	if env.Data.Discontinuous {
		t.Error("discontinuous = true on a merged, contiguous since-inception series")
	}
}

// since-inception carries the same gap signal rather than asserting a
// continuity it does not have.
func TestHistorySinceInception_DeclaresItsOwnHole(t *testing.T) {
	env := getSinceInception(t, holedServer(t, holedCEXOnlyStore()))
	requireHoleDeclared(t, len(env.Data.Points), env.Data.Discontinuous, env.Data.GapStartsAt, env.Data.GapEndsAt)
}

// TestHistorySinceInception_ChargesByGranularity: the client's
// granularity selects how many buckets the unbounded read returns (up to
// 50,000 at 1m against ~3,650 at the 1d default), so it must select the
// price too. Each case is one request into a fresh 100-token window.
func TestHistorySinceInception_ChargesByGranularity(t *testing.T) {
	cases := []struct {
		name, query   string
		wantStatus    int
		wantRemaining int
	}{
		{"default grain", "?asset=native", 200, 99},
		{"1d", "?asset=native&granularity=1d", 200, 99},
		{"1w", "?asset=native&granularity=1w", 200, 99},
		{"1mo", "?asset=native&granularity=1mo", 200, 99},
		{"4h", "?asset=native&granularity=4h", 200, 94},
		{"1h", "?asset=native&granularity=1h", 200, 87},
		{"15m", "?asset=native&granularity=15m", 200, 87},
		{"1m", "?asset=native&granularity=1m", 200, 87},
		{"rejected before any read", "?granularity=1m", 400, 99},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := newSinceInceptionLimitedServer(t, 100)
			resp := mustGet(t, ts.URL+sinceInceptionProbe+tc.query)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			var got int
			if resp.StatusCode == http.StatusOK {
				got = remainingBeforeProbe(t, resp, ts.URL+sinceInceptionProbe)
			} else {
				n, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
				if err != nil {
					t.Fatalf("X-RateLimit-Remaining: %v", err)
				}
				got = n
			}
			if got != tc.wantRemaining {
				t.Fatalf("remaining after request = %d, want %d", got, tc.wantRemaining)
			}
		})
	}
}

// TestHistorySinceInception_DeniedGrainDoesNoRead: the surcharge lands
// before the read. After a default request (1 token) the 13-token budget
// has 12 left; a 1m request pays its base token and cannot find the 12
// more, and the store never sees the 1m read.
func TestHistorySinceInception_DeniedGrainDoesNoRead(t *testing.T) {
	ts, reader := newSinceInceptionLimitedServer(t, 13)
	if resp := mustGet(t, ts.URL+sinceInceptionProbe+"?asset=native"); resp.StatusCode != http.StatusOK {
		t.Fatalf("default request status = %d, want 200", resp.StatusCode)
	}
	resp := mustGet(t, ts.URL+sinceInceptionProbe+"?asset=native&granularity=1m")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("1m request status = %d, want 429", resp.StatusCode)
	}
	if n := reader.oneMinuteReads.Load(); n != 0 {
		t.Fatalf("store served %d 1m read(s) for a denied request", n)
	}
}

// TestHistorySinceInception_BadGranularity400 confirms unknown
// granularity surfaces as 400 — the reader returns
// ErrUnknownGranularity, the handler maps to 400 problem+json.
func TestHistorySinceInception_BadGranularity400(t *testing.T) {
	reader := &stubHistoryReader{pointsErr: v1.ErrUnknownGranularity}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset=native&granularity=2h")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for unknown granularity", resp.StatusCode)
	}
}

// TestHistorySinceInception_HappyPath verifies the wire shape:
// envelope wraps a HistorySeries; points are an array of
// {t, p, v_usd?}; the granularity defaults to 1d when omitted.
func TestHistorySinceInception_HappyPath(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	v := "1234.56"
	reader := &stubHistoryReader{
		points: []v1.HistoryPoint{
			{Bucket: t0, VWAP: "0.123", VolumeUSD: &v},
			{Bucket: t0.Add(24 * time.Hour), VWAP: "0.124"},
		},
	}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.HistorySeries `json:"data"`
	}
	mustDecode(t, resp, &env)

	if env.Data.AssetID != "native" {
		t.Errorf("asset_id = %q, want native", env.Data.AssetID)
	}
	if env.Data.Quote != "fiat:USD" {
		t.Errorf("quote = %q, want fiat:USD", env.Data.Quote)
	}
	if env.Data.Granularity != "1d" {
		t.Errorf("granularity default = %q, want 1d", env.Data.Granularity)
	}
	if env.Data.PriceType != "vwap" {
		t.Errorf("price_type = %q, want vwap", env.Data.PriceType)
	}
	if len(env.Data.Points) != 2 {
		t.Fatalf("got %d points, want 2", len(env.Data.Points))
	}
	if env.Data.Points[0].P != "0.123" || env.Data.Points[1].P != "0.124" {
		t.Errorf("points prices: %+v", env.Data.Points)
	}
	// VolumeUSD is *string so empty is null in JSON; second point omits it.
	if env.Data.Points[0].VUSD == nil || *env.Data.Points[0].VUSD != "1234.56" {
		t.Errorf("point[0].v_usd = %+v, want pointer to '1234.56'", env.Data.Points[0].VUSD)
	}
	if env.Data.Points[1].VUSD != nil {
		t.Errorf("point[1].v_usd = %+v, want nil (omitted)", env.Data.Points[1].VUSD)
	}

	if reader.lastCall.granularity != "1d" {
		t.Errorf("reader saw granularity=%q, want default-resolved 1d", reader.lastCall.granularity)
	}
}

// TestHistorySinceInception_StablecoinFallback pins that, when
// the literal X/fiat:USD CAGG read returns
// empty (because no on-chain trades quote in fiat:USD), the handler
// retries against each operator-declared classic USD-peg and serves
// the first non-empty result. Without this, since-inception
// XLM/fiat:USD returns empty even when XLM/<USDC-classic> has data.
func TestHistorySinceInception_StablecoinFallback(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	usdcG := "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

	reader := &stubHistoryReader{
		pointsByPair: map[string][]v1.HistoryPoint{
			// Literal pair: empty (the case where the bug bit).
			"native/fiat:USD": nil,
			// Proxied pair: has data.
			"native/" + usdcG: {
				{Bucket: t0, VWAP: "0.123"},
				{Bucket: t0.Add(24 * time.Hour), VWAP: "0.124"},
			},
		},
	}
	classicUSDC, _ := canonical.ParseAsset(usdcG)
	srv := v1.New(v1.Options{
		History:           reader,
		USDPeggedClassics: []canonical.Asset{classicUSDC},
	})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.HistorySeries `json:"data"`
	}
	mustDecode(t, resp, &env)
	if len(env.Data.Points) != 2 {
		t.Fatalf("got %d points, want 2 (fallback should have served XLM/<USDC> data)", len(env.Data.Points))
	}
	// Wire shape still reports the requested quote, not the proxy peg —
	// the user asked for fiat:USD and the snapshot is interpreted as
	// USD via the implicit peg ≈ $1 assumption.
	if env.Data.Quote != "fiat:USD" {
		t.Errorf("quote = %q, want fiat:USD (unchanged by fallback)", env.Data.Quote)
	}
}

// TestHistorySinceInception_GranularityForwarded confirms a
// non-default granularity reaches the reader unchanged. Catches a
// regression where the handler accidentally rewrites the param.
func TestHistorySinceInception_GranularityForwarded(t *testing.T) {
	reader := &stubHistoryReader{points: []v1.HistoryPoint{}}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset=native&granularity=15m")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if reader.lastCall.granularity != "15m" {
		t.Errorf("reader saw granularity=%q, want 15m", reader.lastCall.granularity)
	}
}

// TestHistorySinceInception_RowCapTruncated pins that since-inception
// has no window to compare against the 50k-bucket cap up front, so a
// pair whose grid exceeds it must be flagged AFTER the read — an
// oldest-first cap-hit is otherwise indistinguishable from a series
// that legitimately ends at the last returned bucket.
func TestHistorySinceInception_RowCapTruncated(t *testing.T) {
	const historyMaxPoints = 50_000 // internal/api/v1/history.go
	t0 := time.Unix(1_000_000_000, 0).UTC()
	points := make([]v1.HistoryPoint, historyMaxPoints)
	for i := range points {
		points[i] = v1.HistoryPoint{Bucket: t0.Add(time.Duration(i) * time.Hour), VWAP: "1.0"}
	}
	reader := &stubHistoryReader{points: points}
	srv := v1.New(v1.Options{History: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.HistorySeries `json:"data"`
	}
	mustDecode(t, resp, &env)

	if !env.Data.RowCapTruncated {
		t.Error("row_cap_truncated = false, want true (read returned exactly historyMaxPoints rows)")
	}
	wantEnds := points[len(points)-1].Bucket
	if env.Data.DataEndsAt == nil {
		t.Fatal("data_ends_at = nil, want the last served bucket")
	}
	if !time.Time(*env.Data.DataEndsAt).Equal(wantEnds) {
		t.Errorf("data_ends_at = %v, want %v", time.Time(*env.Data.DataEndsAt), wantEnds)
	}
}

// TestHistorySinceInception_NonstandardDecimals_NormalizesPrice pins that
// /v1/history/since-inception serves the corrected CAGG VWAP, like the
// /v1/chart series it shares a read chain with: `p` = raw ratio × K
// (10^(9−7) = 100), `v_usd` untouched.
func TestHistorySinceInception_NonstandardDecimals_NormalizesPrice(t *testing.T) {
	vusd := "1234.56"
	reader := &stubHistoryReader{points: []v1.HistoryPoint{{
		Bucket: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), VWAP: "41.32", VolumeUSD: &vusd,
	}}}
	srv := v1.New(v1.Options{
		History:             reader,
		NonstandardDecimals: nonstandardDecimalsCacheWith(t, flaggedAsset, 9),
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset="+flaggedAsset+"&quote="+classicUSDC)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"p":"4132.0000000000"`) {
		t.Errorf("since-inception must serve the corrected price 4132.0000000000 (raw 41.32 × 100): %s", body)
	}
	if !strings.Contains(body, `"v_usd":"1234.56"`) {
		t.Errorf("since-inception v_usd must be untouched (already USD-anchored): %s", body)
	}
}

// TestHistorySinceInception_StablecoinFallback_ReachesPegSACTwin is the
// since-inception half of the same defect, and it was blind on BOTH sides:
// the fallback proxied the LITERAL base against the CLASSIC peg only, so
// the combination Soroban depth is stored under — SAC base quoted in the
// peg's SAC — was never read even though the literal-spelling walk above
// it already crosses the base aliases.
//
// RED without the fix: 0 points.
func TestHistorySinceInception_StablecoinFallback_ReachesPegSACTwin(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &stubHistoryReader{pointsByPair: map[string][]v1.HistoryPoint{
		pegAliasAquaSAC + "/" + pegAliasUSDCSAC: {
			{Bucket: t0, VWAP: "0.0041"},
		},
	}}
	srv := v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset=AQUA:"+pegAliasAquaIssuer+
		"&quote=fiat:USD&granularity=1d")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var env struct {
		Data  v1.HistorySeries `json:"data"`
		Flags v1.Flags         `json:"flags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Points) != 1 {
		t.Fatalf("got %d points, want 1 — the USD proxy must reach the peg's SAC form", len(env.Data.Points))
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false; the series was served through the peg, not the requested quote")
	}
	if env.Data.AssetID != pegAliasAquaClassic {
		t.Errorf("asset_id = %q, want %q (echo the requested form)", env.Data.AssetID, pegAliasAquaClassic)
	}
}

// TestHistorySinceInception_DeclaredPeg_FiatUSD_CrossesThroughXLM is the
// since-inception half: the same fixture, the same request under the
// classic id, points from inception rather than a window.
//
// RED without the cross: 0 points.
func TestHistorySinceInception_DeclaredPeg_FiatUSD_CrossesThroughXLM(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &stubHistoryReader{pointsByPair: map[string][]v1.HistoryPoint{
		pegAliasUSDCSAC + "/" + canonical.XLMSacContractID: {
			{Bucket: t0, VWAP: "5"},
			{Bucket: t0.Add(24 * time.Hour), VWAP: "5.05"},
		},
		"crypto:XLM/fiat:USD": {
			{Bucket: t0, VWAP: "0.2"},
			{Bucket: t0.Add(24 * time.Hour), VWAP: "0.198"},
		},
	}}
	srv := v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/history/since-inception?asset=USDC:"+pegAliasUSDCIssuer+
		"&quote=fiat:USD&granularity=1d")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var env struct {
		Data  v1.HistorySeries `json:"data"`
		Flags v1.Flags         `json:"flags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Points) != 2 {
		t.Fatalf("got %d points, want 2 — the declared peg's USD history must be derived through XLM", len(env.Data.Points))
	}
	if got := env.Data.Points[0].P; got != "1.0000000000" {
		t.Errorf("points[0].p = %s, want 1.0000000000 (5 × 0.2)", got)
	}
	if got := env.Data.Points[1].P; got != "0.9999000000" {
		t.Errorf("points[1].p = %s, want 0.9999000000 (5.05 × 0.198)", got)
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false; a series composed through XLM is derived, not traded")
	}
	if env.Data.AssetID != pegAliasUSDCClassic || env.Data.Quote != "fiat:USD" {
		t.Errorf("asset_id/quote = %q/%q, want %q/fiat:USD (echo the request)", env.Data.AssetID, env.Data.Quote, pegAliasUSDCClassic)
	}
}
