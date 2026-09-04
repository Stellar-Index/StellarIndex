// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The classic↔SAC wrappers this file's fixtures need — the operator's
// declared USD peg (Circle's classic USDC and the SAC that wraps it) and
// a traded asset that also has both forms — plus a second USD-pegged
// classic with no wrapper. The C-strkeys are the pubnet contracts the
// deployed `[supply].sac_wrappers` declares; the PYUSD issuer is the
// official one.
const (
	pegAliasUSDCIssuer  = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	pegAliasUSDCClassic = "USDC-" + pegAliasUSDCIssuer
	pegAliasUSDCSAC     = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"

	pegAliasAquaIssuer  = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	pegAliasAquaClassic = "AQUA-" + pegAliasAquaIssuer
	pegAliasAquaSAC     = "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK"

	pegAliasPYUSDIssuer  = "GDQE7IXJ4HUHV6RQHIUPRJSEZE4DRS5WY577O2FY6YQ5LVWZ7JZTU2V5"
	pegAliasPYUSDClassic = "PYUSD-" + pegAliasPYUSDIssuer
)

// installPegAliasRegistry publishes the process AliasRegistry the served
// binary builds from `[supply].sac_wrappers`, carrying both wrappers, and
// resets to the XLM-only default on cleanup. NOT parallel: the registry is
// process-global (see canonical.InstallAliasRegistry).
func installPegAliasRegistry(t *testing.T) canonical.Asset {
	t.Helper()
	reg, err := canonical.NewAliasRegistry(map[string]string{
		pegAliasUSDCSAC: "USDC:" + pegAliasUSDCIssuer,
		pegAliasAquaSAC: "AQUA:" + pegAliasAquaIssuer,
	})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	return mustClassicAsset(t, "USDC", pegAliasUSDCIssuer)
}

func mustClassicAsset(t *testing.T, code, issuer string) canonical.Asset {
	t.Helper()
	a, err := canonical.NewClassicAsset(code, issuer)
	if err != nil {
		t.Fatalf("NewClassicAsset(%s): %v", code, err)
	}
	return a
}

// callIndex returns the position of key in calls, or -1.
func callIndex(calls []string, key string) int {
	for i, c := range calls {
		if c == key {
			return i
		}
	}
	return -1
}

type chartEnvelope struct {
	Data  v1.ChartSeries `json:"data"`
	Flags v1.Flags       `json:"flags"`
}

func getChart(t *testing.T, url string) chartEnvelope {
	t.Helper()
	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var env chartEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	return env
}

// TestChart_StablecoinFallback_ReachesPegSACTwin pins the USD-proxy walk
// on the PEG's own spellings, not just the classic one the operator typed
// into `[trades].usd_pegged_classic_assets`.
//
// A declared peg is an asset, not a spelling. Soroban AMMs (Aquarius,
// Phoenix, Soroswap) trade the SAC wrapper, so a pool's USD leg is stored
// quoted in the USDC SAC and never in USDC-GA5Z… — and the walk, bound to
// the classic form alone, read the one spelling the depth is not under.
// Measured on r1 2026-09-03: a Soroban-traded asset returned 0 chart
// points against fiat:USD while the identical window quoted in the USDC
// SAC returned 39.
//
// RED without the fix: 0 points — the only pair holding the series
// (SAC base × SAC peg) is never read.
func TestChart_StablecoinFallback_ReachesPegSACTwin(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &pairKeyedHistoryReader{byPair: map[string][]v1.HistoryPoint{
		// The Soroban pool's series: both legs in SAC form.
		pegAliasAquaSAC + "/" + pegAliasUSDCSAC: {
			{Bucket: t0, VWAP: "0.0041"},
			{Bucket: t0.Add(time.Hour), VWAP: "0.0042"},
		},
	}}
	srv := v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc}})
	ts := httpTestServer(t, srv)

	env := getChart(t, ts.URL+"/v1/chart?asset=AQUA:"+pegAliasAquaIssuer+
		"&quote=fiat:USD&timeframe=24h&granularity=1h")
	if len(env.Data.Points) != 2 {
		t.Fatalf("got %d points, want 2 — the USD proxy must reach the peg's SAC form", len(env.Data.Points))
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false; the series was served through the peg, not the requested quote")
	}
	if env.Data.AssetID != pegAliasAquaClassic {
		t.Errorf("asset_id = %q, want %q (echo the requested form)", env.Data.AssetID, pegAliasAquaClassic)
	}
	// SAC LAST is a money-safety ordering, not a style choice: the walk
	// takes the first form that answers, so classic depth must be read
	// before a thin Soroban pool. Held to ONE base spelling so the
	// comparison isolates the peg's alias order — two reads that differ
	// on both sides would order themselves by the base loop alone.
	classicAt := callIndex(reader.calls, pegAliasAquaClassic+"/"+pegAliasUSDCClassic)
	sacAt := callIndex(reader.calls, pegAliasAquaClassic+"/"+pegAliasUSDCSAC)
	if classicAt < 0 || sacAt < 0 {
		t.Fatalf("walk missed a combination: classic-peg at %d, SAC-peg at %d (calls=%v)", classicAt, sacAt, reader.calls)
	}
	if classicAt > sacAt {
		t.Errorf("classic peg read at %d, after its SAC form at %d — SAC must be last", classicAt, sacAt)
	}
}

// TestChart_StablecoinFallback_EveryClassicPegBeforeAnySAC pins the
// ordering across the PEG dimension with the base held fixed: with two
// declared pegs, every peg's classic form is read before any peg's SAC
// form. A per-peg interleave ([pegA classic, pegA SAC, pegB classic])
// would read peg A's thin Soroban pool before peg B's deep classic book
// and re-price a series the classic-only walk already served from that
// book — the one outcome widening the walk must never produce.
//
// RED with a per-peg interleave: the thin pool (0.0041) is served in
// place of the deep book (0.0099).
func TestChart_StablecoinFallback_EveryClassicPegBeforeAnySAC(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	pyusd := mustClassicAsset(t, "PYUSD", pegAliasPYUSDIssuer)
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &pairKeyedHistoryReader{byPair: map[string][]v1.HistoryPoint{
		// Thin Soroban pool, quoted in the FIRST peg's SAC.
		pegAliasAquaClassic + "/" + pegAliasUSDCSAC: {
			{Bucket: t0, VWAP: "0.0041"},
		},
		// Deep classic book, quoted in the SECOND peg's classic form.
		pegAliasAquaClassic + "/" + pegAliasPYUSDClassic: {
			{Bucket: t0, VWAP: "0.0099"},
		},
	}}
	srv := v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc, pyusd}})
	ts := httpTestServer(t, srv)

	env := getChart(t, ts.URL+"/v1/chart?asset=AQUA:"+pegAliasAquaIssuer+
		"&quote=fiat:USD&timeframe=24h&granularity=1h")
	if len(env.Data.Points) != 1 {
		t.Fatalf("got %d points, want 1", len(env.Data.Points))
	}
	if got := env.Data.Points[0].P; got != "0.0099" {
		t.Errorf("served %s — the second peg's classic book must be read before the first peg's SAC pool (0.0041)", got)
	}
	deepAt := callIndex(reader.calls, pegAliasAquaClassic+"/"+pegAliasPYUSDClassic)
	thinAt := callIndex(reader.calls, pegAliasAquaClassic+"/"+pegAliasUSDCSAC)
	if deepAt < 0 {
		t.Fatalf("second peg's classic form never read (calls=%v)", reader.calls)
	}
	if thinAt >= 0 && thinAt < deepAt {
		t.Errorf("first peg's SAC read at %d, before the second peg's classic form at %d (calls=%v)", thinAt, deepAt, reader.calls)
	}
}

// TestChart_StablecoinFallback_SkipsPegSpellingOfTheBase pins the other
// half of widening the walk: a base and a proxy quote that are the SAME
// asset in two spellings must not be read at all. Their pair has no rows
// by construction, and every combination shares the handler's one 8s
// deadline.
func TestChart_StablecoinFallback_SkipsPegSpellingOfTheBase(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	reader := &pairKeyedHistoryReader{byPair: map[string][]v1.HistoryPoint{}}
	srv := v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc}})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/chart?asset=USDC:"+pegAliasUSDCIssuer+
		"&quote=fiat:USD&timeframe=24h&granularity=1h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	for _, call := range reader.calls {
		switch call {
		case pegAliasUSDCClassic + "/" + pegAliasUSDCSAC, pegAliasUSDCSAC + "/" + pegAliasUSDCClassic:
			t.Errorf("read %q — the two sides are one asset, never a market", call)
		}
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

// TestChart_DeclaredPeg_FiatUSD_CrossesThroughXLM pins the numeraire's
// own dollar series. The declared USD peg has no USD-quoted buckets under
// any spelling — every USD series on chain is served by rewriting the
// quote to THIS asset — and the proxy walk cannot proxy the peg through
// itself, so USDC, the largest asset on the deployment, charted as an
// empty series beside a 24h volume in the tens of millions. Its dollar
// depth is the USDC/XLM book and the USDC-SAC/XLM-SAC pools; crossed
// with XLM's CEX-quoted dollar series bucket by bucket, that is the
// peg's actual traded dollar price.
//
// The fixture stores classic USDC's trades under its SAC identity
// (the Soroban pool: USDC-SAC quoted in the XLM SAC) and XLM's dollar
// series under `crypto:XLM`; the request names the classic id. Three
// pool buckets against two XLM buckets prove the join emits only buckets
// present on both legs. Prices are exact products rendered at 10
// digits: 5 × 0.2 and 5.05 × 0.198.
//
// RED without the cross: 0 points.
func TestChart_DeclaredPeg_FiatUSD_CrossesThroughXLM(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	t0 := time.Unix(1_770_000_000, 0).UTC()
	vol0, vol1 := "12345.67", "890.12"
	reader := &pairKeyedHistoryReader{byPair: map[string][]v1.HistoryPoint{
		pegAliasUSDCSAC + "/" + canonical.XLMSacContractID: {
			{Bucket: t0, VWAP: "5", VolumeUSD: &vol0},
			{Bucket: t0.Add(time.Hour), VWAP: "5.05", VolumeUSD: &vol1},
			{Bucket: t0.Add(2 * time.Hour), VWAP: "5.1"},
		},
		"crypto:XLM/fiat:USD": {
			{Bucket: t0, VWAP: "0.2"},
			{Bucket: t0.Add(time.Hour), VWAP: "0.198"},
		},
	}}
	srv := v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc}})
	ts := httpTestServer(t, srv)

	env := getChart(t, ts.URL+"/v1/chart?asset=USDC:"+pegAliasUSDCIssuer+
		"&quote=fiat:USD&timeframe=24h&granularity=1h")
	if len(env.Data.Points) != 2 {
		t.Fatalf("got %d points, want 2 — the declared peg's USD series must be derived through XLM (calls=%v)", len(env.Data.Points), reader.calls)
	}
	if got := env.Data.Points[0].P; got != "1.0000000000" {
		t.Errorf("points[0].p = %s, want 1.0000000000 (5 × 0.2)", got)
	}
	if got := env.Data.Points[1].P; got != "0.9999000000" {
		t.Errorf("points[1].p = %s, want 0.9999000000 (5.05 × 0.198)", got)
	}
	if got := env.Data.Points[0].VUSD; got == nil || *got != vol0 {
		t.Errorf("points[0].v_usd = %v, want %s (the asset leg's own USD volume)", got, vol0)
	}
	if !env.Data.Points[1].T.Equal(t0.Add(time.Hour)) {
		t.Errorf("points[1].t = %s, want %s", env.Data.Points[1].T, t0.Add(time.Hour))
	}
	if !env.Flags.Triangulated {
		t.Error("flags.triangulated = false; a series composed through XLM is derived, not traded")
	}
	if env.Data.AssetID != pegAliasUSDCClassic || env.Data.Quote != "fiat:USD" {
		t.Errorf("asset_id/quote = %q/%q, want %q/fiat:USD (echo the request)", env.Data.AssetID, env.Data.Quote, pegAliasUSDCClassic)
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

// TestChart_FiatFallback_XLMCrossRunsLast pins where the cross sits in
// the chain: after every directly observed market. A pool quoted in the
// peg's SAC is a traded series and must be served as-is, and once it has
// answered the XLM legs are not read at all — a derived value never
// displaces an observed one, and never costs a read it cannot use.
//
// RED with the cross ahead of the proxy walk: the derived 0.0040000000
// is served in place of the pool's 0.0041.
func TestChart_FiatFallback_XLMCrossRunsLast(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &pairKeyedHistoryReader{byPair: map[string][]v1.HistoryPoint{
		pegAliasAquaSAC + "/" + pegAliasUSDCSAC: {
			{Bucket: t0, VWAP: "0.0041"},
		},
		pegAliasAquaClassic + "/native": {
			{Bucket: t0, VWAP: "0.02"},
		},
		"crypto:XLM/fiat:USD": {
			{Bucket: t0, VWAP: "0.2"},
		},
	}}
	srv := v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc}})
	ts := httpTestServer(t, srv)

	env := getChart(t, ts.URL+"/v1/chart?asset=AQUA:"+pegAliasAquaIssuer+
		"&quote=fiat:USD&timeframe=24h&granularity=1h")
	if len(env.Data.Points) != 1 {
		t.Fatalf("got %d points, want 1", len(env.Data.Points))
	}
	if got := env.Data.Points[0].P; got != "0.0041" {
		t.Errorf("served %s — the observed pool (0.0041) must win over the XLM-derived value", got)
	}
	if at := callIndex(reader.calls, pegAliasAquaClassic+"/native"); at >= 0 {
		t.Errorf("XLM leg read at %d although a proxy already answered (calls=%v)", at, reader.calls)
	}
}
