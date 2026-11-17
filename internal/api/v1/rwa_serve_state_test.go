package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type rwaServeEnvelope struct {
	AsOf  time.Time `json:"as_of"`
	Flags struct {
		Stale bool `json:"stale"`
	} `json:"flags"`
}

func getRWAEnvelope(t *testing.T, srv *v1.Server) (rwaServeEnvelope, http.Header) {
	t.Helper()
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/rwa/assets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env rwaServeEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env, resp.Header
}

// TestRWAAssets_AFailedMembershipReadIsDegraded: a membership whose only build failed is served as the unavailable view
// under flags.degraded and no-store, not as a clean answer.
func TestRWAAssets_AFailedMembershipReadIsDegraded(t *testing.T) {
	srv := v1.New(v1.Options{
		Listings:  rwaNothingListed,
		Sep1Cache: &stubSep1BoundReader{err: errors.New("sep1 scan: statement timeout")},
		Directory: &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{}},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            map[string][]timescale.AssetRow{},
		},
	})
	// flags.degraded is not serialised (json:"-"); on the wire it is no-store, and the body states the absence.
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/rwa/assets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data struct {
			Summary struct {
				Basis string `json:"basis"`
			} `json:"summary"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store on a degraded answer", cc)
	}
	if !strings.HasPrefix(env.Data.Summary.Basis, "Membership could not be established") {
		t.Errorf("summary.basis = %q, want the unavailable statement", env.Data.Summary.Basis)
	}
}

// failingLakeSupply is a token-supply reader whose bulk lake read never answers.
type failingLakeSupply struct{}

func (failingLakeSupply) TokenSupply(context.Context, string) (clickhouse.TokenSupply, error) {
	return clickhouse.TokenSupply{}, errors.New("lake sum exceeded its budget")
}

func (failingLakeSupply) NativeTotalCoins(context.Context) (int64, uint32, error) {
	return 0, 0, errors.New("lake sum exceeded its budget")
}

func (failingLakeSupply) TokenSupplyForContracts(context.Context, []string) (map[string]clickhouse.TokenSupply, error) {
	return nil, errors.New("lake sum exceeded its budget")
}

// failingTrustlineSum is the broad trustline-sum reader with every refresh failing.
type failingTrustlineSum struct {
	v1.ExplorerReader
}

func (failingTrustlineSum) ClassicCirculatingSupply(context.Context) (map[string]string, error) {
	return nil, errors.New("classic supply GROUP BY exceeded its budget")
}

// answeringLakeSupply answers the bulk lake read with no reading for any asset.
type answeringLakeSupply struct{ failingLakeSupply }

func (answeringLakeSupply) TokenSupplyForContracts(context.Context, []string) (map[string]clickhouse.TokenSupply, error) {
	return map[string]clickhouse.TokenSupply{}, nil
}

// TestRWAAssets_AnUnreadSupplyFlagsTheView: a member left without supply because the lake and the trustline sum both failed
// is an outage, so the view is served stale, no-store, and cached only for the degraded lifetime.
func TestRWAAssets_AnUnreadSupplyFlagsTheView(t *testing.T) {
	// Unpriced, so the market-cap fill skips it and only the RWA supply fill reads its supply.
	rows := map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, nil, 346312)}}
	build := func(lake v1.TokenSupplyReader, trustlines v1.ExplorerReader) *v1.Server {
		return v1.New(v1.Options{
			Listings:     rwaNothingListed,
			Sep1Cache:    &stubSep1BoundReader{bound: []timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")}},
			Directory:    &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")}},
			Oracle:       rwaOracle(t, rwaOracleRow(t, "redstone", "rwa:USTRY", "fiat:USD", "1074182", 6)),
			AssetsReader: &rwaListStub{stubAssetsReaderExt: &stubAssetsReaderExt{}, byIssuer: rows},
			TokenSupply:  lake,
			Explorer:     trustlines,
		})
	}
	// The trustline sum is left unwired in the control: its first read is always cold, which is itself unread.
	if env, _ := getRWAEnvelope(t, build(answeringLakeSupply{}, nil)); env.Flags.Stale {
		t.Fatal("control: the lake answered with no reading, yet the view was flagged stale")
	}
	srv := build(failingLakeSupply{}, failingTrustlineSum{})
	env, h := getRWAEnvelope(t, srv)
	if !env.Flags.Stale || !strings.Contains(h.Get("Cache-Control"), "no-store") {
		t.Errorf("flags = %+v, Cache-Control = %q; want stale and no-store when no supply reader answered", env.Flags, h.Get("Cache-Control"))
	}
	if life := srv.RWAAssetsViewLifetimeForTest(); life > v1.RWAAssetsDegradedTTL {
		t.Errorf("view cached for %s, want at most the degraded lifetime %s", life, v1.RWAAssetsDegradedTTL)
	}
}

// flakyListStub answers listing pages until fail is set.
type flakyListStub struct {
	*rwaListStub
	fail atomic.Bool
}

func (s *flakyListStub) ListAssetsExt(ctx context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	if s.fail.Load() {
		return nil, errors.New("catalogue read exceeded its budget")
	}
	return s.rwaListStub.ListAssetsExt(ctx, opts)
}

// TestRWAAssets_AViewPastItsMaximumAgeIsUnavailable: once the last view is past its hour and the rebuild fails, the route
// answers 503 rwa-assets-unavailable with Retry-After, not a 500 and not the expired totals.
func TestRWAAssets_AViewPastItsMaximumAgeIsUnavailable(t *testing.T) {
	rows := map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)}}
	list := &flakyListStub{rwaListStub: &rwaListStub{stubAssetsReaderExt: &stubAssetsReaderExt{}, byIssuer: rows, supply: rwaSupplyFor(rows)}}
	srv := v1.New(v1.Options{
		Listings:     rwaNothingListed,
		Sep1Cache:    &stubSep1BoundReader{bound: []timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")}},
		Directory:    &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")}},
		AssetsReader: list,
	})
	ts := httpTestServer(t, srv)
	if resp := mustGet(t, ts.URL+"/v1/rwa/assets"); resp.StatusCode != http.StatusOK {
		t.Fatalf("first build: status = %d, want 200", resp.StatusCode)
	}
	list.fail.Store(true)
	srv.AgeRWAAssetsViewForTest(59 * time.Minute)
	if resp := mustGet(t, ts.URL+"/v1/rwa/assets"); resp.StatusCode != http.StatusOK {
		t.Fatalf("inside the hour: status = %d, want the last view served", resp.StatusCode)
	}
	srv.AgeRWAAssetsViewForTest(2 * time.Minute)
	resp := mustGet(t, ts.URL+"/v1/rwa/assets")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("past the hour: status = %d, want 503", resp.StatusCode)
	}
	var p struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Type != "https://api.stellarindex.io/errors/rwa-assets-unavailable" || resp.Header.Get("Retry-After") == "" {
		t.Errorf("type = %q, Retry-After = %q; want rwa-assets-unavailable with a retry hint", p.Type, resp.Header.Get("Retry-After"))
	}
}

// TestRWAAssets_AMemberWithNoCatalogueRowMakesTheTotalsALowerBound: an admitted member the catalogue has not caught up with is
// absent from every total, so the summary, the reference total and its issuer's group must say lower bound and name the gap.
func TestRWAAssets_AMemberWithNoCatalogueRowMakesTheTotalsALowerBound(t *testing.T) {
	rows := map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)}}
	srv := v1.New(v1.Options{
		Listings: rwaNothingListed,
		Sep1Cache: &stubSep1BoundReader{bound: []timescale.Sep1BoundCurrency{
			rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
			rwaBound("TESOURO2", rwaGoodIssuer, "etherfuse.com", "bond"),
		}},
		Directory: &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")}},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{}, byIssuer: rows, supply: rwaSupplyFor(rows),
		},
	})
	ts := httpTestServer(t, srv)
	resp := mustGet(t, ts.URL+"/v1/rwa/assets")
	var env struct {
		Data struct {
			Assets  []struct{ Code string } `json:"assets"`
			Summary struct {
				MarketCapUSD *string `json:"market_cap_usd"`
				LowerBound   bool    `json:"lower_bound"`
				Basis        string  `json:"basis"`
				Reference    struct {
					LowerBound bool `json:"lower_bound"`
				} `json:"reference_valuation"`
			} `json:"summary"`
			ByIssuer []struct {
				Issuer              string `json:"issuer"`
				MarketCapLowerBound bool   `json:"market_cap_lower_bound"`
			} `json:"by_issuer"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	d := env.Data
	if len(d.Assets) != 1 || d.Summary.MarketCapUSD == nil {
		t.Fatalf("assets = %+v, market_cap_usd = %v; want USTRY served and valued", d.Assets, d.Summary.MarketCapUSD)
	}
	if !d.Summary.LowerBound || !d.Summary.Reference.LowerBound {
		t.Errorf("summary.lower_bound = %v, reference_valuation.lower_bound = %v; want both true with TESOURO2 excluded",
			d.Summary.LowerBound, d.Summary.Reference.LowerBound)
	}
	if !strings.Contains(d.Summary.Basis, "1 admitted classic member(s) have no asset catalogue row") {
		t.Errorf("summary.basis does not name the excluded member: %q", d.Summary.Basis)
	}
	if len(d.ByIssuer) != 1 || !d.ByIssuer[0].MarketCapLowerBound {
		t.Errorf("by_issuer = %+v, want the issuer's market cap marked a lower bound", d.ByIssuer)
	}
}

// rwaDatedListStub serves listing pages as a cache would after its upstream refresh kept failing: old and stale.
type rwaDatedListStub struct {
	*rwaListStub
	at time.Time
}

func (s *rwaDatedListStub) ListAssetsExtAt(
	ctx context.Context, opts timescale.ListAssetsOptions,
) ([]timescale.AssetRow, time.Time, bool, error) {
	rows, err := s.ListAssetsExt(ctx, opts)
	return rows, s.at, true, err
}

// TestRWAAssets_AsOfIsTheOldestListingRead: the money figures are as old as the listing rows behind them, so as_of says so
// and a stale page marks the response stale, rather than stamping two-hour-old prices with the request time.
func TestRWAAssets_AsOfIsTheOldestListingRead(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	rows := map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)}}
	srv := v1.New(v1.Options{
		Listings:  rwaNothingListed,
		Sep1Cache: &stubSep1BoundReader{bound: []timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")}},
		Directory: &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")}},
		AssetsReader: &rwaDatedListStub{
			rwaListStub: &rwaListStub{stubAssetsReaderExt: &stubAssetsReaderExt{}, byIssuer: rows, supply: rwaSupplyFor(rows)},
			at:          old,
		},
	})
	env, h := getRWAEnvelope(t, srv)
	if !env.AsOf.Equal(old) {
		t.Errorf("as_of = %s, want the listing read at %s", env.AsOf, old)
	}
	if !env.Flags.Stale || !strings.Contains(h.Get("Cache-Control"), "no-store") {
		t.Errorf("flags = %+v, Cache-Control = %q, want stale and no-store for a page served past its refresh",
			env.Flags, h.Get("Cache-Control"))
	}
}
