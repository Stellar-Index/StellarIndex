package v1_test

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type stubEmissionsHistoryLending struct {
	stubBLNTLendingReader
	hist   timescale.LendingEmissionsHistory
	isBLNT *bool
	q      *timescale.EmissionHistoryQuery
}

func (r *stubEmissionsHistoryLending) LendingEmissionsHistory(_ context.Context, _ string, isBLNT bool, q timescale.EmissionHistoryQuery) (timescale.LendingEmissionsHistory, error) {
	r.isBLNT, r.q = &isBLNT, &q
	return r.hist, nil
}

type fixedWatermark struct{ at time.Time }

func (f fixedWatermark) LakeWatermark(context.Context) (uint32, time.Time, error) {
	return 70_000_000, f.at, nil
}

const emisHistQuery = "?interval=1h&from=2026-11-01T00:00:00Z&to=2026-11-01T03:00:00Z"

var emisT0 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func emisHour(h float64) time.Time { return emisT0.Add(time.Duration(h * float64(time.Hour))) }

func verifiedHistory(sources ...string) *stubCompletenessReader {
	now := time.Now().UTC()
	c := &stubCompletenessReader{}
	for _, s := range sources {
		c.snaps = append(c.snaps, timescale.CompletenessSnapshot{
			Source: s, Genesis: 100, ProjectionOK: true, ProjectionVerifiedFrom: 100, ComputedAt: now, ProjectionEvidencedAt: now,
		})
	}
	return c
}

func getEmissionsHistory(t *testing.T, srv *v1.Server, pool, query string, want int) (v1.LendingEmissionsHistoryView, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/pools/"+pool+"/emissions/history"+query, nil))
	if rec.Code != want {
		t.Fatalf("status = %d, want %d: %s", rec.Code, want, rec.Body)
	}
	var body struct {
		Data v1.LendingEmissionsHistoryView
	}
	if want == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
	}
	return body.Data, rec.Body.Bytes()
}

// blendV2History: token 1 runs at eps 1 (10^-7 base units a second) from before the window, under a never-expiring u64 max,
// then eps 2 from 01:30 until it expires at 02:30. Claims: one
// naming only token 1, two naming several reserves; all three in the pool total.
func blendV2History(t *testing.T) timescale.LendingEmissionsHistory {
	return timescale.LendingEmissionsHistory{
		Schedule: []timescale.EmissionScheduleEvent{
			{TokenID: 1, At: emisHour(-1), EPS: mustAmount(t, "1"), Expiration: mustAmount(t, "18446744073709551615")},
			{TokenID: 1, At: emisHour(1.5), EPS: mustAmount(t, "2"), Expiration: mustAmount(t, "1793500200")}, // 02:30
		},
		Claims: []timescale.EmissionClaimBucket{
			{Start: emisHour(0), Scope: "pool", Claims: 3, Claimers: 2, Amount: mustAmount(t, "18446744073709551646")},
			{Start: emisHour(0), Scope: "several", Claims: 2, Claimers: 1, Amount: mustAmount(t, "18446744073709551641")},
			{Start: emisHour(0), Scope: "single", TokenID: u32(1), Claims: 1, Claimers: 1, Amount: mustAmount(t, "5")},
		},
		Alloc: []timescale.EmissionAllocBucket{
			{Start: emisHour(2), Events: 1, BackstopShare: mustAmount(t, "700"), PoolShare: mustAmount(t, "300")},
		},
	}
}

func blendV2Server(t *testing.T, lr *stubEmissionsHistoryLending, opts v1.Options) *v1.Server {
	opts.Explorer = &stubEmissionsExplorer{stubExplorerReader: &stubExplorerReader{}, state: emissionStreams(t, 1)}
	opts.Lending = lr
	return v1.New(opts)
}

// TestLendingEmissionsHistory_BlendV2 pins the scheduled tokens per bucket (seeded by the update before the window, clamped
// at expiration), the claim split, the backstop allocation, and the Blend backstop claim null.
func TestLendingEmissionsHistory_BlendV2(t *testing.T) {
	lr := &stubEmissionsHistoryLending{hist: blendV2History(t)}
	srv := blendV2Server(t, lr, v1.Options{CompletenessReader: verifiedHistory("blend", "blend_backstop")})
	got, _ := getEmissionsHistory(t, srv, mkCStrkey(t, 1), emisHistQuery, http.StatusOK)

	if lr.q == nil || !lr.q.Schedule || !lr.q.BackstopAlloc || lr.q.BackstopClaim || *lr.isBLNT || lr.q.Daily ||
		!lr.q.From.Equal(emisT0) || !lr.q.To.Equal(emisHour(3)) {
		t.Fatalf("query = %+v", lr.q)
	}
	rs := got.Reserves
	if !rs.Available || rs.LowerBound || rs.Approximate || len(rs.Streams) != 1 {
		t.Fatalf("reserves = %+v", rs)
	}
	s := rs.Streams[0]
	if s.TokenID != 1 || s.Side != "supply" || s.Asset == nil || *s.Asset != mkCStrkey(t, 3) || s.Tokens != "0.00126" ||
		len(s.Points) != 3 || s.Points[0].Tokens != "0.00036" || s.Points[1].Tokens != "0.00054" || s.Points[2].Tokens != "0.00036" {
		t.Fatalf("stream = %+v", s)
	}
	pc := got.PoolClaims
	if !pc.Available || pc.LowerBound || pc.Totals.Claims != 3 || pc.Totals.Amount != "18446744073709551646" || len(pc.Points) != 1 {
		t.Fatalf("pool claims = %+v", pc)
	}
	br := pc.ByReserve
	if !br.LowerBound || len(br.Reserves) != 1 || br.Reserves[0].TokenID != 1 || br.Reserves[0].Totals.Amount != "5" ||
		br.Reserves[0].Side != "supply" {
		t.Fatalf("by reserve = %+v", br)
	}
	if len(got.Excluded) != 1 || got.Excluded[0].Stream != "pool_claims_by_reserve" ||
		got.Excluded[0].Reason != "claim_names_several_reserves" || *got.Excluded[0].Count != 2 ||
		*got.Excluded[0].Amount != "18446744073709551641" || !got.LowerBound {
		t.Fatalf("excluded = %+v", got.Excluded)
	}
	ba := got.BackstopAlloc
	if !ba.Available || ba.LowerBound || ba.TierSplitReason != nil || ba.Totals.BackstopEmissions != "700" || ba.Totals.PoolEmissions != "300" {
		t.Fatalf("alloc = %+v", ba)
	}
	if bc := got.BackstopClaims; bc.Available || bc.UnavailableReason == nil || *bc.UnavailableReason != "backstop_claim_names_no_pool" {
		t.Fatalf("backstop claims = %+v", bc)
	}
}

// TestLendingEmissionsHistory_BlendV1 serves claims only: the V1 schedule and backstop are null with their reasons, and
// neither the schedule nor the allocation is read.
func TestLendingEmissionsHistory_BlendV1(t *testing.T) {
	v1Pool := blend.PoolV1
	lr := &stubEmissionsHistoryLending{
		stubBLNTLendingReader: stubBLNTLendingReader{stubLendingReader: stubLendingReader{version: &v1Pool}},
		hist:                  timescale.LendingEmissionsHistory{Claims: blendV2History(t).Claims},
	}
	srv := blendV2Server(t, lr, v1.Options{CompletenessReader: verifiedHistory("blend", "blend_backstop")})
	got, _ := getEmissionsHistory(t, srv, mkCStrkey(t, 1), emisHistQuery, http.StatusOK)
	if lr.q.Schedule || lr.q.BackstopAlloc || lr.q.BackstopClaim {
		t.Fatalf("query = %+v", lr.q)
	}
	if got.Reserves.Available || *got.Reserves.UnavailableReason != "v1_emission_schedule_unverified" || got.Reserves.LowerBound ||
		got.BackstopAlloc.Available || *got.BackstopAlloc.UnavailableReason != "v1_backstop_unaudited" ||
		*got.BackstopClaims.UnavailableReason != "backstop_claim_names_no_pool" || got.EmissionToken != nil {
		t.Fatalf("v1 view = %+v", got)
	}
	if !got.PoolClaims.Available || got.PoolClaims.Totals.Claims != 3 || got.PoolClaims.ByReserve.Reserves[0].Asset != nil {
		t.Fatalf("v1 claims = %+v", got.PoolClaims)
	}
}

// TestLendingEmissionsHistory_BLNT: the schedule is flagged approximate (stored eps drifts by dust without an event) but
// not a lower bound; the allocation has no tier split; backstop claims are per tier with each amount's unit named.
func TestLendingEmissionsHistory_BLNT(t *testing.T) {
	unknown := blend.PoolVersionUnknown
	h := blendV2History(t)
	h.BackstopClaims = []timescale.EmissionBackstopClaimBucket{
		{Start: emisHour(1), Tier: "FirstLoss", Claims: 2, Claimers: 2, BLNT: mustAmount(t, "10"), LP: mustAmount(t, "4"), Shares: mustAmount(t, "18446744073709551616")},
		{Start: emisHour(2), Tier: "FirstLoss", Claims: 1, Claimers: 1, BLNT: mustAmount(t, "1"), LP: mustAmount(t, "1"), Shares: mustAmount(t, "1")},
	}
	lr := &stubEmissionsHistoryLending{
		stubBLNTLendingReader: stubBLNTLendingReader{stubLendingReader: stubLendingReader{version: &unknown}, blntAssets: []string{mkCStrkey(t, 3)}},
		hist:                  h,
	}
	srv := blendV2Server(t, lr, v1.Options{CompletenessReader: verifiedHistory("blnt")})
	got, _ := getEmissionsHistory(t, srv, blnt.MainnetFixedPool, emisHistQuery, http.StatusOK)
	if !*lr.isBLNT || !lr.q.BackstopClaim || got.Protocol != "blnt" {
		t.Fatalf("query = %+v view %+v", lr.q, got)
	}
	if !got.Reserves.Approximate || got.Reserves.ApproximateReason == nil || *got.Reserves.ApproximateReason != "stored_eps_rewritten_without_event" ||
		got.Reserves.LowerBound {
		t.Fatalf("reserves = %+v", got.Reserves)
	}
	if got.BackstopAlloc.TierSplitAvailable || got.BackstopAlloc.TierSplitReason == nil || *got.BackstopAlloc.TierSplitReason != "tier_split_not_on_chain" {
		t.Fatalf("alloc = %+v", got.BackstopAlloc)
	}
	bc := got.BackstopClaims
	if !bc.Available || bc.LowerBound || bc.Units.LPAmount != "backstop_lp_token_base_units" || len(bc.Tiers) != 1 ||
		bc.Tiers[0].Totals.Claims != 3 || bc.Tiers[0].Totals.Shares != "18446744073709551617" || bc.Tiers[0].Totals.BLNTAmount != "11" ||
		bc.Tiers[0].Totals.LPAmount != "5" || len(bc.Tiers[0].Points) != 2 {
		t.Fatalf("backstop claims = %+v", bc)
	}
}

// TestLendingEmissionsHistory_LowerBoundRules: each exclusion alone bounds the stream it names and no other.
func TestLendingEmissionsHistory_LowerBoundRules(t *testing.T) {
	pool := mkCStrkey(t, 1)
	t.Run("history_unverified", func(t *testing.T) {
		lr := &stubEmissionsHistoryLending{hist: timescale.LendingEmissionsHistory{}}
		got, _ := getEmissionsHistory(t, blendV2Server(t, lr, v1.Options{CompletenessReader: verifiedHistory("blend_backstop")}), pool, emisHistQuery, http.StatusOK)
		if !got.Reserves.LowerBound || !got.PoolClaims.LowerBound || !got.PoolClaims.ByReserve.LowerBound || got.BackstopAlloc.LowerBound ||
			len(got.Excluded) != 2 || got.Excluded[0].Reason != "history_unverified" {
			t.Fatalf("view = %+v", got)
		}
	})
	t.Run("amount_unread", func(t *testing.T) {
		lr := &stubEmissionsHistoryLending{hist: timescale.LendingEmissionsHistory{Claims: []timescale.EmissionClaimBucket{
			{Start: emisT0, Scope: "pool", Claims: 2, Amount: mustAmount(t, "5"), AmountUnread: 1},
		}}}
		got, _ := getEmissionsHistory(t, blendV2Server(t, lr, v1.Options{CompletenessReader: verifiedHistory("blend", "blend_backstop")}), pool, emisHistQuery, http.StatusOK)
		if !got.PoolClaims.LowerBound || got.Reserves.LowerBound || got.BackstopAlloc.LowerBound || len(got.Excluded) != 1 ||
			got.Excluded[0].Reason != "amount_unread" || *got.Excluded[0].Count != 1 {
			t.Fatalf("view = %+v", got)
		}
	})
	t.Run("claim_names_no_reserve", func(t *testing.T) {
		lr := &stubEmissionsHistoryLending{hist: timescale.LendingEmissionsHistory{Claims: []timescale.EmissionClaimBucket{
			{Start: emisT0, Scope: "pool", Claims: 1, Amount: mustAmount(t, "9")},
			{Start: emisT0, Scope: "none", Claims: 1, Amount: mustAmount(t, "9")},
		}}}
		got, _ := getEmissionsHistory(t, blendV2Server(t, lr, v1.Options{CompletenessReader: verifiedHistory("blend", "blend_backstop")}), pool, emisHistQuery, http.StatusOK)
		if got.PoolClaims.LowerBound || !got.PoolClaims.ByReserve.LowerBound || got.PoolClaims.Totals.Amount != "9" ||
			len(got.Excluded) != 1 || got.Excluded[0].Reason != "claim_names_no_reserve" {
			t.Fatalf("view = %+v", got)
		}
	})
	t.Run("window_past_lake_tip", func(t *testing.T) {
		lr := &stubEmissionsHistoryLending{hist: blendV2History(t)}
		srv := blendV2Server(t, lr, v1.Options{
			CompletenessReader: verifiedHistory("blend", "blend_backstop"), LakeWatermark: fixedWatermark{at: emisHour(1.5)},
		})
		got, _ := getEmissionsHistory(t, srv, pool, emisHistQuery, http.StatusOK)
		s := got.Reserves.Streams[0]
		if !lr.q.To.Equal(emisHour(1.5)) || time.Time(got.ServedThrough) != emisHour(1.5) || !got.Reserves.LowerBound ||
			!got.PoolClaims.LowerBound || s.Tokens != "0.00054" || len(s.Points) != 2 || !s.Points[1].Partial || s.Points[0].Partial {
			t.Fatalf("view = %+v stream %+v", got, s)
		}
	})
}

// TestLendingEmissionsHistory_LayersNeverAdded: scheduled, allocated and claimed describe the same tokens at different
// stages, so no served amount may be the sum of two layers' amounts.
func TestLendingEmissionsHistory_LayersNeverAdded(t *testing.T) {
	lr := &stubEmissionsHistoryLending{hist: blendV2History(t)}
	srv := blendV2Server(t, lr, v1.Options{CompletenessReader: verifiedHistory("blend", "blend_backstop")})
	_, raw := getEmissionsHistory(t, srv, mkCStrkey(t, 1), emisHistQuery, http.StatusOK)
	var doc struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	layers := map[string][]*big.Rat{}
	for _, k := range []string{"reserves", "pool_claims", "backstop_alloc"} {
		layers[k] = decimalStrings(t, doc.Data[k])
	}
	all := decimalStrings(t, raw)
	for a, as := range layers {
		for b, bs := range layers {
			if a >= b {
				continue
			}
			for _, x := range as {
				for _, y := range bs {
					sum := new(big.Rat).Add(x, y)
					for _, v := range all {
						if x.Sign() != 0 && y.Sign() != 0 && v.Cmp(sum) == 0 {
							t.Fatalf("%s %s + %s %s is served as %s", a, x.RatString(), b, y.RatString(), v.RatString())
						}
					}
				}
			}
		}
	}
}

// decimalStrings collects every JSON string that parses as a decimal.
func decimalStrings(t *testing.T, raw json.RawMessage) []*big.Rat {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	var out []*big.Rat
	var walk func(any)
	walk = func(n any) {
		switch x := n.(type) {
		case map[string]any:
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		case string:
			if r, ok := new(big.Rat).SetString(x); ok && !strings.ContainsAny(x, "-:TZ") {
				out = append(out, r)
			}
		}
	}
	walk(v)
	return out
}

func TestLendingEmissionsHistory_Validation(t *testing.T) {
	lr := &stubEmissionsHistoryLending{}
	srv := blendV2Server(t, lr, v1.Options{})
	pool := mkCStrkey(t, 1)
	getEmissionsHistory(t, srv, "not-a-pool", emisHistQuery, http.StatusBadRequest)
	getEmissionsHistory(t, srv, pool, "?interval=5m", http.StatusBadRequest)
	getEmissionsHistory(t, srv, pool, "?interval=1h&from=2026-01-01T00:00:00Z&to=2026-11-01T00:00:00Z", http.StatusBadRequest)
	unknown := blend.PoolVersionUnknown
	lr = &stubEmissionsHistoryLending{stubBLNTLendingReader: stubBLNTLendingReader{stubLendingReader: stubLendingReader{version: &unknown}}}
	getEmissionsHistory(t, blendV2Server(t, lr, v1.Options{}), pool, emisHistQuery, http.StatusNotFound)
	if lr.q != nil {
		t.Fatal("an unknown pool reached the event read")
	}
	// A lending reader without the event history is a 503, never empty streams.
	srv = v1.New(v1.Options{Lending: &stubEmissionClaimReader{}})
	getEmissionsHistory(t, srv, pool, emisHistQuery, http.StatusServiceUnavailable)
}
