package main

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/cmd/stellarindex-api/internal/wiring"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/metadata"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type fakeObservationReader struct {
	rows       map[string]timescale.AccountObservation
	err        error
	batchCalls *int
}

func (f fakeObservationReader) LatestAccountObservationAtOrBefore(_ context.Context, accountID string, _ uint32) (timescale.AccountObservation, error) {
	if f.err != nil {
		return timescale.AccountObservation{}, f.err
	}
	row, ok := f.rows[accountID]
	if !ok {
		return timescale.AccountObservation{}, timescale.ErrNotFound
	}
	return row, nil
}

func (f fakeObservationReader) LatestAccountObservationsAtOrBefore(ctx context.Context, accountIDs []string, asOfLedger uint32) (map[string]timescale.AccountObservation, error) {
	if f.batchCalls != nil {
		*f.batchCalls++
	}
	out := make(map[string]timescale.AccountObservation, len(accountIDs))
	for _, id := range accountIDs {
		row, err := f.LatestAccountObservationAtOrBefore(ctx, id, asOfLedger)
		if errors.Is(err, timescale.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[id] = row
	}
	return out, nil
}

// TestHomeDomainLookup_ObservedAbsenceBeatsStatic drives the production
// composition (store adapter → ChainedHomeDomainLookup → assetToDetail):
// an issuer whose latest account_observations row has no home_domain —
// cleared by SetOptions (stored NULL) or merged (is_removal) — must serve
// no home_domain, never the operator's static entry.
func TestHomeDomainLookup_ObservedAbsenceBeatsStatic(t *testing.T) {
	const (
		cleared   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		merged    = "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
		live      = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		unwatched = "GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55"
	)
	onChain := "aqua.network"
	stale := "stale.example.com"
	reader := fakeObservationReader{rows: map[string]timescale.AccountObservation{
		cleared: {AccountID: cleared, Ledger: 10, Balance: big.NewInt(1)},
		merged:  {AccountID: merged, Ledger: 11, Balance: big.NewInt(0), IsRemoval: true, HomeDomain: &stale},
		live:    {AccountID: live, Ledger: 12, Balance: big.NewInt(1), HomeDomain: &onChain},
	}}
	static := func(string) (string, bool) { return "operator.example.com", true }
	lookup := metadata.ChainedHomeDomainLookup(
		metadata.NewLCMHomeDomainResolver(metadataStoreLookup{s: reader}), static, nil)

	cases := []struct {
		issuer string
		want   string // "" = HomeDomain must be nil
	}{
		{cleared, ""},
		{merged, ""},
		{live, onChain},
		{unwatched, "operator.example.com"},
	}
	for _, tc := range cases {
		d := wiring.AssetToDetail(context.Background(), canonical.Asset{Type: canonical.AssetClassic, Code: "TST", Issuer: tc.issuer}, lookup)
		got := ""
		if d.HomeDomain != nil {
			got = *d.HomeDomain
		}
		if got != tc.want {
			t.Errorf("issuer %s: home_domain=%q, want %q", tc.issuer, got, tc.want)
		}
	}
}

// TestAssetsToDetails_OneBatchReadPerPage drives the production listing
// wiring (newHomeDomainLookups → assetsToDetails): a page of N assets makes
// one observation read, and each row gets the same home_domain the
// per-issuer chain would give it.
func TestAssetsToDetails_OneBatchReadPerPage(t *testing.T) {
	const (
		cleared   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		live      = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		unwatched = "GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55"
	)
	onChain := "aqua.network"
	batchCalls := 0
	reader := fakeObservationReader{batchCalls: &batchCalls, rows: map[string]timescale.AccountObservation{
		cleared: {AccountID: cleared, Ledger: 10, Balance: big.NewInt(1)},
		live:    {AccountID: live, Ledger: 12, Balance: big.NewInt(1), HomeDomain: &onChain},
	}}
	static := func(string) (string, bool) { return "operator.example.com", true }
	hdl := newHomeDomainLookups(metadata.NewLCMHomeDomainResolver(metadataStoreLookup{s: reader}), static, nil)

	assets := []canonical.Asset{
		{Type: canonical.AssetClassic, Code: "A", Issuer: cleared},
		{Type: canonical.AssetClassic, Code: "B", Issuer: live},
		{Type: canonical.AssetClassic, Code: "C", Issuer: live},
		{Type: canonical.AssetClassic, Code: "D", Issuer: unwatched},
		{Type: canonical.AssetNative},
	}
	want := []string{"", onChain, onChain, "operator.example.com", ""}
	got := wiring.AssetsToDetails(context.Background(), assets, hdl.listing)
	if batchCalls != 1 {
		t.Fatalf("observation batch reads=%d for %d rows, want 1", batchCalls, len(assets))
	}
	for i, d := range got {
		hd := ""
		if d.HomeDomain != nil {
			hd = *d.HomeDomain
		}
		if hd != want[i] {
			t.Errorf("row %d (%s): home_domain=%q, want %q", i, assets[i].String(), hd, want[i])
		}
	}
}

func TestMetadataStoreLookup_StorageErrorPropagates(t *testing.T) {
	boom := errors.New("conn reset")
	_, err := metadataStoreLookup{s: fakeObservationReader{err: boom}}.HomeDomainAtOrBefore(context.Background(), "GA", 1)
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v, want %v", err, boom)
	}
}

// accountStateExplorer is a v1.ExplorerReader whose only live method is
// the issuer account-state read the asset-detail backfill makes.
type accountStateExplorer struct {
	v1.ExplorerReader
	states map[string]clickhouse.AccountState
}

func (e accountStateExplorer) AccountStateCached(_ context.Context, account string) (clickhouse.AccountState, bool, error) {
	return e.states[account], false, nil
}

type lookupAssetReader struct {
	lookup func(ctx context.Context, issuer string) (string, bool)
}

func (r lookupAssetReader) GetAsset(ctx context.Context, a canonical.Asset) (v1.AssetDetail, error) {
	return wiring.AssetToDetail(ctx, a, r.lookup), nil
}

func (r lookupAssetReader) ListAssets(context.Context, string, int) ([]v1.AssetDetail, string, error) {
	return nil, "", nil
}

// TestAssetDetail_LiveOnChainHomeDomainBeatsStaticMap drives the production
// wiring (newHomeDomainLookups → assetToDetail → v1 backfill) with a
// populated [metadata.issuer_home_domains] map. Precedence on
// /v1/assets/{id}: observation → live ClickHouse account state → static
// map → curated knownIssuers; an observed clear suppresses the static map.
func TestAssetDetail_LiveOnChainHomeDomainBeatsStaticMap(t *testing.T) {
	const (
		usdcIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN" // knownIssuers: circle.com
		chainLive  = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		cleared    = "GBYBVWOOVC4EJVRIF4HMWG5B7POLCS7JRPY5KYR3BCLEK24IJQOGUARD" // not curated
		observed   = "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
	)
	onChain := "observed.example"
	obs := fakeObservationReader{rows: map[string]timescale.AccountObservation{
		cleared:  {AccountID: cleared, Ledger: 10, Balance: big.NewInt(1)},
		observed: {AccountID: observed, Ledger: 11, Balance: big.NewInt(1), HomeDomain: &onChain},
	}}
	static := func(string) (string, bool) { return "operator-static.example", true }
	hdl := newHomeDomainLookups(metadata.NewLCMHomeDomainResolver(metadataStoreLookup{s: obs}), static, nil)
	explorer := accountStateExplorer{states: map[string]clickhouse.AccountState{
		chainLive: {Exists: true, HomeDomain: "live-onchain.example"},
		observed:  {Exists: true, HomeDomain: "live-onchain.example"},
	}}
	srv := v1.New(v1.Options{
		Assets:           lookupAssetReader{lookup: hdl.detail},
		Explorer:         explorer,
		StaticHomeDomain: hdl.static,
	})

	cases := []struct {
		name, issuer, want string // want "" = home_domain absent
	}{
		{"live chain beats static", chainLive, "live-onchain.example"},
		{"observation is served as-is", observed, onChain},
		{"static beats curated map when chain is silent", usdcIssuer, "operator-static.example"},
		{"observed clear suppresses static", cleared, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/assets/TST-"+tc.issuer, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var env struct {
				Data v1.AssetDetail `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got := ""
			if env.Data.HomeDomain != nil {
				got = *env.Data.HomeDomain
			}
			if got != tc.want {
				t.Errorf("home_domain=%q, want %q", got, tc.want)
			}
		})
	}
}
