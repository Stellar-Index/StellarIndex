package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// countingList counts listing reads, one batch per view build, and while slow holds each read until release closes.
type countingList struct {
	*rwaListStub
	reads   atomic.Int64
	slow    atomic.Bool
	release chan struct{}
}

func (l *countingList) ListAssetsExt(ctx context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	l.reads.Add(1)
	if l.slow.Load() {
		select {
		case <-l.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return l.rwaListStub.ListAssetsExt(ctx, opts)
}

// hookGate runs hook once, inside the first exclusion-set read after it is armed.
type hookGate struct {
	*swapGate
	hook atomic.Pointer[func()]
}

func (g *hookGate) Snapshot(ctx context.Context) (pricingguard.ExclusionSet, pricingguard.Withholding) {
	if f := g.hook.Swap(nil); f != nil {
		(*f)()
	}
	return g.swapGate.Snapshot(ctx)
}

func ustryExcluded() v1.PriceExclusionGate {
	return assetExclGate(exclTable{rows: []timescale.SourceExclusion{{
		ID: 1, Source: "redstone", Base: "rwa:USTRY", Quote: "fiat:USD", From: time.Now().Add(-time.Hour),
	}}})
}

func newRWAAssetsCacheServer(t *testing.T, gate v1.PriceExclusionGate) (*v1.Server, *countingList) {
	t.Helper()
	rows := map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.02033610"), 346312)}}
	list := &countingList{
		rwaListStub: &rwaListStub{stubAssetsReaderExt: &stubAssetsReaderExt{}, byIssuer: rows, supply: rwaSupplyFor(rows)},
		release:     make(chan struct{}),
	}
	t.Cleanup(func() { close(list.release) })
	srv := v1.New(v1.Options{
		Listings:     rwaNothingListed,
		Sep1Cache:    &stubSep1BoundReader{bound: []timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")}},
		Directory:    &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")}},
		Oracle:       rwaOracle(t, rwaOracleRow(t, "redstone", "rwa:USTRY", "fiat:USD", "107403800", 8)),
		AssetsReader: list,
		Exclusions:   gate,
	})
	return srv, list
}

// TestRWAAssets_AVerdictRebuildPastTheDeadlineIsRefusedAsSuch: a request whose deadline passes while the view it refused is
// rebuilt gets the same retryable 503 as a rebuild that still disagrees, never a 500 or a generic timeout.
func TestRWAAssets_AVerdictRebuildPastTheDeadlineIsRefusedAsSuch(t *testing.T) {
	gate := &swapGate{g: assetExclGate(exclTable{})}
	srv, list := newRWAAssetsCacheServer(t, gate)
	url := httpTestServer(t, srv).URL
	getRWAAssetsAt(t, url)

	gate.set(ustryExcluded())
	list.slow.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/rwa/assets", nil).WithContext(ctx))

	var problem struct {
		Type   string `json:"type"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&problem); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("status = %d Retry-After = %q Cache-Control = %q, want 503, Retry-After, no-store", rec.Code,
			rec.Header().Get("Retry-After"), rec.Header().Get("Cache-Control"))
	}
	if !strings.HasSuffix(problem.Type, "/rwa-assets-unavailable") || !strings.Contains(problem.Detail, "source exclusions changed") {
		t.Errorf("type = %q detail = %q, want the exclusion-change refusal", problem.Type, problem.Detail)
	}
}

// TestRWAAssets_ALiftedExclusionRepublishesTheReference: once the exclusion over a member's oracle is lifted, the next request
// publishes its reference again with neither the withheld flag nor no-store, though the cached view was built under it.
func TestRWAAssets_ALiftedExclusionRepublishesTheReference(t *testing.T) {
	gate := &swapGate{g: ustryExcluded()}
	srv, _ := newRWAAssetsCacheServer(t, gate)
	url := httpTestServer(t, srv).URL

	env, cc := getRWAAssetsAt(t, url)
	if a := env.Data.Assets[0]; a.Reference != nil || env.Flags.FillsWithheldReason != "source_excluded" || !strings.Contains(cc, "no-store") {
		t.Fatalf("excluded: reference = %+v, fills_withheld_reason = %q, Cache-Control = %q; want withheld, source_excluded, no-store",
			a.Reference, env.Flags.FillsWithheldReason, cc)
	}

	gate.set(assetExclGate(exclTable{}))
	env, cc = getRWAAssetsAt(t, url)
	a := env.Data.Assets[0]
	if a.Reference == nil || a.ReferenceValuation.ValueUSD == nil || a.Premium.Status != v1.RWAPremiumPublished {
		t.Errorf("lifted: reference = %+v, valuation = %v, premium = %q; want the oracle figure published", a.Reference,
			a.ReferenceValuation.ValueUSD, a.Premium.Status)
	}
	if env.Flags.FillsWithheldReason != "" || env.Flags.Stale || strings.Contains(cc, "no-store") {
		t.Errorf("lifted: fills_withheld_reason = %q, stale = %v, Cache-Control = %q; want none", env.Flags.FillsWithheldReason,
			env.Flags.Stale, cc)
	}
}

// TestRWAAssets_AVerdictMismatchServesAnAgreeingViewThatLanded: a request that refused the cached view serves the view another
// request's rebuild has since cached, when that one agrees with the set, rather than building a second.
func TestRWAAssets_AVerdictMismatchServesAnAgreeingViewThatLanded(t *testing.T) {
	gate := &hookGate{swapGate: &swapGate{g: assetExclGate(exclTable{})}}
	srv, list := newRWAAssetsCacheServer(t, gate)
	url := httpTestServer(t, srv).URL
	getRWAAssetsAt(t, url)

	gate.set(ustryExcluded())
	before := list.reads.Load()
	getRWAAssetsAt(t, url)
	perBuild := list.reads.Load() - before
	if perBuild == 0 {
		t.Fatal("an exclusion change caused no rebuild; the test cannot count builds")
	}

	gate.set(assetExclGate(exclTable{}))
	nested := func() { getRWAAssetsAt(t, url) }
	gate.hook.Store(&nested)
	before = list.reads.Load()
	env, _ := getRWAAssetsAt(t, url)
	if got := list.reads.Load() - before; got != perBuild {
		t.Errorf("listing reads = %d, want %d: one rebuild between the two requests, not one each", got, perBuild)
	}
	if a := env.Data.Assets[0]; a.Reference == nil || a.Premium.Status != v1.RWAPremiumPublished {
		t.Errorf("reference = %+v, premium = %q; want the oracle figure published", a.Reference, a.Premium.Status)
	}
}
