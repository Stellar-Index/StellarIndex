package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type stubEmissionsExplorer struct {
	*stubExplorerReader
	state  clickhouse.LendingEmissionsState
	tiered *bool
}

func (e *stubEmissionsExplorer) LendingEmissions(_ context.Context, _ string, tiered bool) (clickhouse.LendingEmissionsState, error) {
	e.tiered = &tiered
	return e.state, nil
}

type stubEmissionClaimReader struct {
	stubBLNTLendingReader
	claims timescale.LendingEmissionClaims
	isBLNT *bool
}

func (r *stubEmissionClaimReader) LendingEmissionClaims(_ context.Context, _ string, isBLNT bool, _ time.Time) (timescale.LendingEmissionClaims, error) {
	r.isBLNT = &isBLNT
	return r.claims, nil
}

func mustAmount(t *testing.T, s string) canonical.Amount {
	t.Helper()
	a, err := canonical.FromString(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func getEmissions(t *testing.T, srv *v1.Server, pool string, want int) v1.LendingEmissionsView {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/pools/"+pool+"/emissions", nil))
	if rec.Code != want {
		t.Fatalf("status = %d, want %d: %s", rec.Code, want, rec.Body)
	}
	var body struct{ Data v1.LendingEmissionsView }
	if want == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
	}
	return body.Data
}

func emissionStreams(t *testing.T, future uint64) clickhouse.LendingEmissionsState {
	asset := mkCStrkey(t, 3)
	return clickhouse.LendingEmissionsState{
		Instance: &clickhouse.PoolInstance{Backstop: blnt.MainnetBackstop}, EmissionToken: mkCStrkey(t, 9),
		ReserveList: []string{asset},
		Reserves: []clickhouse.ReserveEmission{
			{TokenID: 0, Asset: asset, Side: clickhouse.EmissionSideBorrow, Data: clickhouse.EmissionData{EPS: 1, Expiration: 1_000, LastTime: 900}},
			{TokenID: 1, Asset: asset, Side: clickhouse.EmissionSideSupply, Data: clickhouse.EmissionData{EPS: 18_000_000_000_000_000_000 / 2, Expiration: future, LastTime: 900}},
		},
	}
}

// TestLendingPoolEmissions_BlendV2 serves V2 streams with live judged against now, exact eps, and pool claims with the
// Blend backstop claims flagged as unattributable.
func TestLendingPoolEmissions_BlendV2(t *testing.T) {
	future := uint64(time.Now().Add(time.Hour).Unix()) //nolint:gosec // test clock
	st := emissionStreams(t, future)
	st.Backstop = []clickhouse.BackstopEmission{{Data: clickhouse.EmissionData{EPS: 5, Expiration: future, LastTime: 1}}}
	ex := &stubEmissionsExplorer{stubExplorerReader: &stubExplorerReader{}, state: st}
	lr := &stubEmissionClaimReader{claims: timescale.LendingEmissionClaims{Pool: timescale.LendingClaimTotals{
		Claims: 3, Claims30d: 1, Claimers: 2, Claimers30d: 1,
		Amount: mustAmount(t, "18446744073709551621"), Amount30d: mustAmount(t, "5"),
	}}}
	pool := mkCStrkey(t, 1)
	got := getEmissions(t, v1.New(v1.Options{Explorer: ex, Lending: lr}), pool, http.StatusOK)

	if ex.tiered == nil || *ex.tiered || lr.isBLNT == nil || *lr.isBLNT || got.Protocol != "blend" || !got.StorageAvailable ||
		got.EPSDecimals != 7 || got.EmissionToken == nil || *got.EmissionToken != mkCStrkey(t, 9) {
		t.Fatalf("view = %+v", got)
	}
	if len(got.Reserves) != 2 || got.Reserves[0].Live || got.Reserves[0].Side != "borrow" ||
		!got.Reserves[1].Live || got.Reserves[1].EPS != "9000000000000000000" || got.Reserves[1].Side != "supply" {
		t.Fatalf("reserves = %+v", got.Reserves)
	}
	if len(got.Backstop) != 1 || got.Backstop[0].Tier != nil || !got.Backstop[0].Live {
		t.Fatalf("backstop = %+v", got.Backstop)
	}
	c := got.Claims
	if !c.Available || c.Pool == nil || c.Pool.Amount != "18446744073709551621" || c.Pool.Claims != 3 || c.Backstop != nil ||
		c.BackstopUnavailableReason == nil {
		t.Fatalf("claims = %+v", c)
	}
}

// TestLendingPoolEmissions_ClaimsLowerBoundUnlessVerifiedFromGenesis: claim totals sum projected events, so they are served as
// a lower bound naming why unless the protocol's projection is verified back to its first ledger, as account earnings do.
func TestLendingPoolEmissions_ClaimsLowerBoundUnlessVerifiedFromGenesis(t *testing.T) {
	ex := &stubEmissionsExplorer{stubExplorerReader: &stubExplorerReader{}, state: emissionStreams(t, 1)}
	lr := &stubEmissionClaimReader{claims: timescale.LendingEmissionClaims{Pool: timescale.LendingClaimTotals{
		Claims: 1, Amount: mustAmount(t, "1200000000"), Amount30d: mustAmount(t, "0"),
	}}}
	pool := mkCStrkey(t, 1)
	now := time.Now().UTC()
	floorAfterGenesis := &stubCompletenessReader{snaps: []timescale.CompletenessSnapshot{
		{Source: "blend", Genesis: 100, ProjectionOK: true, ProjectionVerifiedFrom: 5000, ComputedAt: now, ProjectionEvidencedAt: now},
	}}
	cases := []struct {
		name   string
		reader *stubCompletenessReader
		reason string
	}{
		{name: "verified from genesis", reader: verifiedLendingHistory()},
		{name: "projection floor after genesis", reader: floorAfterGenesis, reason: "history_unverified"},
		{name: "verdict read fails", reader: &stubCompletenessReader{err: errors.New("pg down")}, reason: "history_verdict_unread"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := getEmissions(t, v1.New(v1.Options{Explorer: ex, Lending: lr, CompletenessReader: tc.reader}), pool, http.StatusOK).Claims
			want := tc.reason
			if want == "" {
				want = "null"
			}
			if c.Pool == nil || c.Pool.Amount != "1200000000" || c.LowerBound != (tc.reason != "") || strOrNull(c.LowerBoundReason) != want {
				t.Fatalf("claims = %+v reason %s", c, strOrNull(c.LowerBoundReason))
			}
		})
	}
}

// TestLendingPoolEmissions_BLNT reads the tiered layout and serves per-tier backstop claims as shares.
func TestLendingPoolEmissions_BLNT(t *testing.T) {
	unknown := blend.PoolVersionUnknown
	future := uint64(time.Now().Add(time.Hour).Unix()) //nolint:gosec // test clock
	st := emissionStreams(t, future)
	st.Backstop = []clickhouse.BackstopEmission{{Tier: "FirstLoss", Data: clickhouse.EmissionData{EPS: 5, Expiration: future}}}
	ex := &stubEmissionsExplorer{stubExplorerReader: &stubExplorerReader{}, state: st}
	lr := &stubEmissionClaimReader{
		stubBLNTLendingReader: stubBLNTLendingReader{stubLendingReader: stubLendingReader{version: &unknown}, blntAssets: []string{mkCStrkey(t, 3)}},
		claims: timescale.LendingEmissionClaims{Backstop: []timescale.BLNTBackstopClaimTotals{{
			Tier: "FirstLoss", LendingClaimTotals: timescale.LendingClaimTotals{Claims: 1, Amount: mustAmount(t, "10")},
			LP: mustAmount(t, "4"), Shares: mustAmount(t, "18446744073709551616"),
		}}},
	}
	got := getEmissions(t, v1.New(v1.Options{Explorer: ex, Lending: lr}), blnt.MainnetFixedPool, http.StatusOK)
	if ex.tiered == nil || !*ex.tiered || lr.isBLNT == nil || !*lr.isBLNT || got.Protocol != "blnt" {
		t.Fatalf("view = %+v", got)
	}
	if len(got.Backstop) != 1 || got.Backstop[0].Tier == nil || *got.Backstop[0].Tier != "FirstLoss" {
		t.Fatalf("backstop = %+v", got.Backstop)
	}
	c := got.Claims
	if len(c.Backstop) != 1 || c.Backstop[0].SharesMinted != "18446744073709551616" || c.Backstop[0].LPTokens != "4" ||
		c.Backstop[0].Amount != "10" || c.BackstopUnavailableReason != nil {
		t.Fatalf("claims = %+v", c)
	}
}

func TestLendingPoolEmissions_UnknownPoolIs404(t *testing.T) {
	unknown := blend.PoolVersionUnknown
	ex := &stubEmissionsExplorer{stubExplorerReader: &stubExplorerReader{}}
	lr := &stubEmissionClaimReader{stubBLNTLendingReader: stubBLNTLendingReader{stubLendingReader: stubLendingReader{version: &unknown}}}
	getEmissions(t, v1.New(v1.Options{Explorer: ex, Lending: lr}), mkCStrkey(t, 1), http.StatusNotFound)
	if ex.tiered != nil || lr.isBLNT != nil {
		t.Fatal("an unknown pool reached the storage or claim reads")
	}
}

// TestLendingPoolEmissions_MissingStorageIsFlagged: a V1 pool's storage layout is unverified and an unreadable instance is
// unavailable; both still serve claims and flag the storage part rather than an empty "no emissions".
func TestLendingPoolEmissions_MissingStorageIsFlagged(t *testing.T) {
	v1Pool := blend.PoolV1
	ex := &stubEmissionsExplorer{stubExplorerReader: &stubExplorerReader{}}
	lr := &stubEmissionClaimReader{stubBLNTLendingReader: stubBLNTLendingReader{stubLendingReader: stubLendingReader{version: &v1Pool}}}
	got := getEmissions(t, v1.New(v1.Options{Explorer: ex, Lending: lr}), mkCStrkey(t, 1), http.StatusOK)
	if ex.tiered != nil || got.StorageAvailable || got.StorageUnavailableReason == nil ||
		*got.StorageUnavailableReason != "blend_v1_emission_storage_unverified" || !got.Claims.Available {
		t.Fatalf("v1 view = %+v", got)
	}

	lr = &stubEmissionClaimReader{}
	got = getEmissions(t, v1.New(v1.Options{Explorer: ex, Lending: lr}), mkCStrkey(t, 1), http.StatusOK)
	if got.StorageAvailable || got.StorageUnavailableReason == nil || *got.StorageUnavailableReason != "pool_instance_unavailable" ||
		len(got.Reserves) != 0 || got.EmissionToken != nil {
		t.Fatalf("no-instance view = %+v", got)
	}
}
