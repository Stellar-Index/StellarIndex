// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
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
	reg, err := canonical.NewAliasRegistry(canonical.PubnetPassphrase, map[string]string{
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

// legFailingHistoryReader serves pairKeyedHistoryReader's fixture but
// returns failFor's error for the pairs it names, so one leg of the
// through-XLM cross can fail while every other read succeeds.
type legFailingHistoryReader struct {
	*pairKeyedHistoryReader
	failFor func(base, quote string) error
}

func (r *legFailingHistoryReader) HistoryPointsInRange(ctx context.Context, p canonical.Pair, g string, from, to time.Time, limit int) ([]v1.HistoryPoint, error) {
	if err := r.failFor(p.Base.String(), p.Quote.String()); err != nil {
		return nil, err
	}
	return r.pairKeyedHistoryReader.HistoryPointsInRange(ctx, p, g, from, to, limit)
}

func isXLMSpelling(id string) bool {
	return id == "native" || id == "crypto:XLM" || id == canonical.XLMSacContractID
}

func isUSDCSpelling(id string) bool {
	return id == pegAliasUSDCClassic || id == pegAliasUSDCSAC
}

// throughXLMLegChartURL serves the declared-peg cross fixture of
// TestChart_DeclaredPeg_FiatUSD_CrossesThroughXLM — the asset leg under
// USDC-SAC/XLM-SAC and, when withPivot, the pivot under crypto:XLM —
// behind a reader that fails the reads failFor names.
func throughXLMLegChartURL(t *testing.T, withPivot bool, failFor func(base, quote string) error) string {
	t.Helper()
	usdc := installPegAliasRegistry(t)
	t0 := time.Unix(1_770_000_000, 0).UTC()
	byPair := map[string][]v1.HistoryPoint{
		pegAliasUSDCSAC + "/" + canonical.XLMSacContractID: {{Bucket: t0, VWAP: "5"}},
	}
	if withPivot {
		byPair["crypto:XLM/fiat:USD"] = []v1.HistoryPoint{{Bucket: t0, VWAP: "0.2"}}
	}
	reader := &legFailingHistoryReader{
		pairKeyedHistoryReader: &pairKeyedHistoryReader{byPair: byPair},
		failFor:                failFor,
	}
	srv := v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc}})
	ts := httpTestServer(t, srv)
	return ts.URL + "/v1/chart?asset=USDC:" + pegAliasUSDCIssuer +
		"&quote=fiat:USD&timeframe=24h&granularity=1h"
}
