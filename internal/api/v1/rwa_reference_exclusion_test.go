package v1_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type rwaAssetsEnvelope struct {
	Data  v1.RWAAssetsView `json:"data"`
	Flags struct {
		Stale               bool   `json:"stale"`
		FillsWithheldReason string `json:"fills_withheld_reason"`
	} `json:"flags"`
}

func getRWAAssetsAt(t *testing.T, url string) (rwaAssetsEnvelope, string) {
	t.Helper()
	resp := mustGet(t, url+"/v1/rwa/assets")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env rwaAssetsEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data.Assets) != 1 {
		t.Fatalf("assets = %d, want 1", len(env.Data.Assets))
	}
	return env, resp.Header.Get("Cache-Control")
}

// TestRWAAssets_ASourceExcludedReferenceIsWithheldAndFlagged: an operator exclusion over the oracle that values a member
// refuses its reference, valuation and premium on the next request, even with the view already cached, marks the envelope
// and serves it no-store; an unreadable set refuses it as an outage. An empty set leaves the published figures alone.
func TestRWAAssets_ASourceExcludedReferenceIsWithheldAndFlagged(t *testing.T) {
	gate := &swapGate{g: assetExclGate(exclTable{})}
	srv := v1.New(v1.Options{
		Listings:  rwaNothingListed,
		Sep1Cache: &stubSep1BoundReader{bound: []timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")}},
		Directory: &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")}},
		Oracle:    rwaOracle(t, rwaOracleRow(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8)),
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.02033610"), 346312)}},
			supply:              rwaSupplyFor(map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.02033610"), 346312)}}),
		},
		Exclusions: gate,
	})
	url := httpTestServer(t, srv).URL

	env, cc := getRWAAssetsAt(t, url)
	a := env.Data.Assets[0]
	if a.Reference == nil || a.Premium.Status != v1.RWAPremiumPublished || a.ReferenceValuation.ValueUSD == nil {
		t.Fatalf("empty set: reference = %+v, premium = %q; want the oracle figure published", a.Reference, a.Premium.Status)
	}
	if env.Flags.FillsWithheldReason != "" || strings.Contains(cc, "no-store") {
		t.Errorf("empty set: fills_withheld_reason = %q, Cache-Control = %q; want neither", env.Flags.FillsWithheldReason, cc)
	}

	gate.set(assetExclGate(exclTable{rows: []timescale.SourceExclusion{{
		ID: 1, Source: "redstone", Base: "rwa:USTRY", Quote: "fiat:USD", From: time.Now().Add(-time.Hour),
	}}}))
	env, cc = getRWAAssetsAt(t, url)
	a = env.Data.Assets[0]
	if a.Reference != nil || a.ReferenceValuation.ValueUSD != nil || a.Premium.Pct != nil {
		t.Errorf("excluded: reference = %+v, valuation = %v, premium = %v; want none served", a.Reference,
			a.ReferenceValuation.ValueUSD, a.Premium.Pct)
	}
	if a.ReferenceValuation.Status != v1.RWAPremiumReferenceSourceExcluded || a.Premium.Status != v1.RWAPremiumReferenceSourceExcluded {
		t.Errorf("excluded: statuses = %q/%q, want %q", a.ReferenceValuation.Status, a.Premium.Status, v1.RWAPremiumReferenceSourceExcluded)
	}
	if env.Flags.FillsWithheldReason != "source_excluded" || !strings.Contains(cc, "no-store") {
		t.Errorf("excluded: fills_withheld_reason = %q, Cache-Control = %q; want source_excluded, no-store",
			env.Flags.FillsWithheldReason, cc)
	}
	if rv := env.Data.Summary.ReferenceValuation; rv.ValueUSD != nil || !rv.LowerBound {
		t.Errorf("excluded: summary reference total = %v lower_bound = %v; want no total, a lower bound", rv.ValueUSD, rv.LowerBound)
	}
	if !env.Data.Funnel.Balanced {
		t.Errorf("excluded: funnel unbalanced: %s", env.Data.Funnel.Imbalance)
	}

	gate.set(assetExclGate(exclTable{err: errors.New("exclusions table unreachable")}))
	env, cc = getRWAAssetsAt(t, url)
	a = env.Data.Assets[0]
	if a.Reference != nil || a.ReferenceValuation.Status != v1.RWAPremiumReferenceUnavailable {
		t.Errorf("unread: reference = %+v, status = %q; want withheld as %q", a.Reference, a.ReferenceValuation.Status,
			v1.RWAPremiumReferenceUnavailable)
	}
	if !env.Flags.Stale || !strings.Contains(cc, "no-store") {
		t.Errorf("unread: stale = %v, Cache-Control = %q; want stale, no-store", env.Flags.Stale, cc)
	}
}
