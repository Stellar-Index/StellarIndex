package v1_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/network"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// emissionsStore is the Postgres side of the emissions block: a coverage map and the groups it returns for any spans.
type emissionsStore struct {
	covs    map[string]timescale.LendingEmissionsUSDCoverage
	sums    []timescale.LendingEmissionsUSDBucketSum
	failSum bool
	spans   []timescale.LendingUSDSpan
}

func (*emissionsStore) CountRecentEventsBySource(context.Context) (map[string]int64, error) {
	return nil, nil
}

func (e *emissionsStore) LendingEmissionsUSDCoverages(context.Context) (map[string]timescale.LendingEmissionsUSDCoverage, error) {
	return e.covs, nil
}

func (e *emissionsStore) LendingEmissionsUSDBucketSums(_ context.Context, spans []timescale.LendingUSDSpan, _ int16, _ []string) ([]timescale.LendingEmissionsUSDBucketSum, error) {
	e.spans = spans
	if e.failSum {
		return nil, errors.New("pg down")
	}
	return e.sums, nil
}

var emisThrough = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

func emisCov(pool string, days int) timescale.LendingEmissionsUSDCoverage {
	return timescale.LendingEmissionsUSDCoverage{
		Pool: pool, Protocol: "blend", FloorDay: emisThrough.AddDate(0, 0, 1-days), ThroughDay: emisThrough,
		Unavailable: map[string]string{"backstop_claims": "backstop_claim_names_no_pool"}, ValuationRev: v1.LendingEmissionsValuationRevForTest,
	}
}

// emisBLND is BLND's SAC on the public network, the token the identity gate accepts for Blend.
var emisBLND, _ = xdrjson.SACContractID(blndAssetID, network.PublicNetworkPassphrase)

func emisGroup(pool, stream string, bucket int, amount, usd string) timescale.LendingEmissionsUSDBucketSum {
	token, decimals := emisBLND, 7
	g := timescale.LendingEmissionsUSDBucketSum{
		Pool: pool, Stream: stream, Bucket: bucket, Current: true, EmissionToken: &token, EmissionTokenDecimals: &decimals,
		Excluded: []string{}, Amount: amount, Rows: 1,
	}
	if usd != "" {
		g.AmountUSD = &usd
	}
	return g
}

func reasoned(g timescale.LendingEmissionsUSDBucketSum, reason string) timescale.LendingEmissionsUSDBucketSum {
	g.AmountUSD, g.USDReason, g.LowerBound = nil, &reason, true
	return g
}

// emissionsServer serves store with the writers' verdicts c and the public network, so the identity gate can pass.
func emissionsServer(t *testing.T, store v1.ProtocolStatsReader, c v1.CompletenessReader) *v1.Server {
	t.Helper()
	srv := newProtocolsServer(t, protocolsCase{clean: true, revenue: store, completeness: c}).srv
	srv.NetworkPassphrase = network.PublicNetworkPassphrase
	return srv
}

var emisVerified = verifiedHistory("blend", "blend_backstop", "blnt")

func blendEmissions(t *testing.T, store *emissionsStore) v1.LendingProtocolEmissions {
	t.Helper()
	stats, _ := getProtocols(t, emissionsServer(t, store, emisVerified))
	return stats[0].Emissions
}

func excludedAs(w v1.LendingProtocolEmissionsWindow, pool, reason string) *v1.LendingProtocolEmissionsExclusion {
	for i, e := range w.Excluded {
		if e.Reason == reason && strOr(e.Pool) == pool {
			return &w.Excluded[i]
		}
	}
	return nil
}

// Every host today: the rollup is off, so no coverage and no rows. Every stream is null and not_valued, never zero.
func TestLendingProtocols_EmissionsNeverRunIsNull(t *testing.T) {
	for name, store := range map[string]v1.ProtocolStatsReader{"no reader": nil, "empty table": &emissionsStore{}} {
		t.Run(name, func(t *testing.T) {
			f := newProtocolsServer(t, protocolsCase{clean: true, revenue: store})
			stats, _ := getProtocols(t, f.srv)
			for _, st := range stats {
				w := st.Emissions.AllTime
				for _, a := range []v1.LendingProtocolEmissionAmounts{
					w.Streams.ReserveSchedule, w.Streams.PoolClaims,
					w.Streams.BackstopAllocBackstop, w.Streams.BackstopAllocPool, w.Streams.BackstopClaims,
				} {
					if a.AmountUSD != nil || a.Amount != nil || strOr(a.USDReason) != "not_valued" || !a.LowerBound {
						t.Fatalf("%s all_time stream %+v; want null, not_valued, a lower bound", st.Protocol, a)
					}
				}
				if len(w.Excluded) == 0 || w.FromDay != nil || st.Emissions.EmissionToken != nil {
					t.Fatalf("%s all_time %+v; want every pool named not_valued, no days", st.Protocol, w)
				}
				for _, e := range w.Excluded {
					if e.Reason != "not_valued" {
						t.Fatalf("%s: exclusion %+v; want not_valued", st.Protocol, e)
					}
				}
			}
		})
	}
}

// Covered pools sum exactly per window and stream; a covered day with no row is zero; a stream the pools do not serve
// is null and named; a stream's tokens are summed in base units beside the USD.
func TestLendingProtocols_EmissionsCoveredExact(t *testing.T) {
	f0 := newProtocolsServer(t, protocolsCase{clean: true})
	a, b := f0.activePool, f0.frozenPool
	store := &emissionsStore{
		covs: map[string]timescale.LendingEmissionsUSDCoverage{a: emisCov(a, 100), b: emisCov(b, 100)},
		sums: []timescale.LendingEmissionsUSDBucketSum{
			emisGroup(a, "pool_claims", 0, "100", "1.25"), emisGroup(a, "pool_claims", 3, "900", "10"),
			emisGroup(b, "pool_claims", 1, "0.5", "0.005"), emisGroup(a, "reserve_schedule", 1, "7.0000001", "2.5"),
		},
	}
	em := blendEmissions(t, store)
	pc := []v1.LendingProtocolEmissionAmounts{em.Day.Streams.PoolClaims, em.Week.Streams.PoolClaims, em.AllTime.Streams.PoolClaims}
	for i, want := range [][2]string{{"1.25", "100"}, {"1.26", "100.5"}, {"11.26", "1000.5"}} {
		if strOr(pc[i].AmountUSD) != want[0] || strOr(pc[i].Amount) != want[1] || pc[i].LowerBound || pc[i].AmountLowerBound {
			t.Fatalf("pool_claims window %d = %+v; want exact %s USD, %s units", i, pc[i], want[0], want[1])
		}
	}
	if s := em.Day.Streams.ReserveSchedule; strOr(s.AmountUSD) != "0.00" || strOr(s.Amount) != "0" || s.LowerBound {
		t.Fatalf("schedule 1d = %+v; want an exact zero: covered, nothing stored", s)
	}
	if s := em.Week.Streams.ReserveSchedule; strOr(s.AmountUSD) != "2.50" || strOr(s.Amount) != "7.0000001" {
		t.Fatalf("schedule 7d = %+v; want 2.50 and 7.0000001 units", s)
	}
	bc := em.AllTime.Streams.BackstopClaims
	if bc.AmountUSD != nil || strOr(bc.USDReason) != "stream_unavailable" || !bc.LowerBound || excludedAs(em.AllTime, a, "backstop_claim_names_no_pool") == nil {
		t.Fatalf("backstop_claims = %+v, excluded %+v; want null, stream_unavailable, named per pool", bc, em.AllTime.Excluded)
	}
	if strOr(em.EmissionToken) != emisBLND || em.EmissionTokenDecimals == nil || *em.EmissionTokenDecimals != 7 || em.Degraded {
		t.Fatalf("token %s degraded %v", strOr(em.EmissionToken), em.Degraded)
	}
	if strOr(em.AllTime.FromDay) != "2026-08-13" || strOr(em.Day.FromDay) != "2026-11-20" || strOr(em.Week.FromDay) != "2026-11-14" {
		t.Fatalf("from days %s/%s/%s", strOr(em.Day.FromDay), strOr(em.Week.FromDay), strOr(em.AllTime.FromDay))
	}
}

// A pool covered for only part of a window is summed for what it has and named for the days it lacks: the window is a
// lower bound, never an exact total.
func TestLendingProtocols_EmissionsPartlyCoveredIsLowerBound(t *testing.T) {
	f0 := newProtocolsServer(t, protocolsCase{clean: true})
	a, b := f0.activePool, f0.frozenPool
	store := &emissionsStore{
		covs: map[string]timescale.LendingEmissionsUSDCoverage{a: emisCov(a, 3), b: emisCov(b, 100)},
		sums: []timescale.LendingEmissionsUSDBucketSum{emisGroup(a, "pool_claims", 1, "40", "4")},
	}
	em := blendEmissions(t, store)
	w := em.Week
	e := excludedAs(w, a, "days_not_valued")
	if e == nil || e.Count == nil || *e.Count != 4 || e.Stream != nil {
		t.Fatalf("7d excluded %+v; want %s named days_not_valued for 4 days", w.Excluded, a)
	}
	if pc := w.Streams.PoolClaims; strOr(pc.AmountUSD) != "4.00" || !pc.LowerBound || !pc.AmountLowerBound {
		t.Fatalf("7d pool_claims = %+v; want 4.00 as a lower bound", pc)
	}
	if d := em.Day.Streams.PoolClaims; d.LowerBound || strOr(d.AmountUSD) != "0.00" {
		t.Fatalf("1d pool_claims = %+v; want exact: both pools cover the day", d)
	}
}

// Coverage an older valuation revision wrote is not coverage: the pool is not_valued and its rows are not summed.
func TestLendingProtocols_EmissionsOldRevisionNotCovered(t *testing.T) {
	f0 := newProtocolsServer(t, protocolsCase{clean: true})
	a, b := f0.activePool, f0.frozenPool
	old := emisCov(a, 100)
	old.ValuationRev = v1.LendingEmissionsValuationRevForTest - 1
	store := &emissionsStore{
		covs: map[string]timescale.LendingEmissionsUSDCoverage{a: old, b: emisCov(b, 100)},
		sums: []timescale.LendingEmissionsUSDBucketSum{emisGroup(a, "pool_claims", 0, "100", "1.25"), emisGroup(b, "pool_claims", 0, "10", "1")},
	}
	em := blendEmissions(t, store)
	pc := em.Day.Streams.PoolClaims
	if excludedAs(em.Day, a, "not_valued") == nil || strOr(pc.AmountUSD) != "1.00" || !pc.LowerBound {
		t.Fatalf("1d = %+v excluded %+v; want %s not_valued, only the current pool's 1.00 as a lower bound", pc, em.Day.Excluded, a)
	}
	for _, sp := range store.spans {
		if sp.Pool == a {
			t.Fatalf("spans %+v read the old-revision pool", store.spans)
		}
	}
}

// A row stored with a usd_reason is named with its reason and its rows, its tokens still summed: never 0.00 USD. A row
// of another valuation revision is left out entirely; a partly priced row is summed and named for its hours.
func TestLendingProtocols_EmissionsReasonedRowsNamed(t *testing.T) {
	f0 := newProtocolsServer(t, protocolsCase{clean: true})
	a, b := f0.activePool, f0.frozenPool
	stale := emisGroup(a, "backstop_alloc_pool", 0, "8", "8")
	stale.Current = false
	partial := emisGroup(b, "backstop_alloc_backstop", 0, "6", "3")
	partial.LowerBound, partial.HoursUnvalued = true, 5
	store := &emissionsStore{
		covs: map[string]timescale.LendingEmissionsUSDCoverage{a: emisCov(a, 100), b: emisCov(b, 100)},
		sums: []timescale.LendingEmissionsUSDBucketSum{
			reasoned(emisGroup(a, "pool_claims", 0, "50", ""), "no_price_within_bound"),
			reasoned(emisGroup(a, "reserve_schedule", 0, "5", ""), "reads_disagree"), emisGroup(b, "reserve_schedule", 0, "2", "2"),
			stale, partial,
		},
	}
	em := blendEmissions(t, store)
	s := em.Day.Streams
	if pc := s.PoolClaims; pc.AmountUSD != nil || strOr(pc.USDReason) != "all_excluded" || !pc.LowerBound || strOr(pc.Amount) != "50" || pc.AmountLowerBound {
		t.Fatalf("pool_claims = %+v; want USD null all_excluded (never 0.00), 50 units exact", pc)
	}
	if e := excludedAs(em.Day, a, "no_price_within_bound"); e == nil || strOr(e.Stream) != "pool_claims" || e.Count == nil || *e.Count != 1 {
		t.Fatalf("excluded %+v; want the reasoned pool_claims row named", em.Day.Excluded)
	}
	if rs := s.ReserveSchedule; strOr(rs.AmountUSD) != "2.00" || !rs.LowerBound || strOr(rs.Amount) != "7" || excludedAs(em.Day, a, "reads_disagree") == nil {
		t.Fatalf("reserve_schedule = %+v; want 2.00 as a lower bound naming reads_disagree, 7 units", rs)
	}
	if ap := s.BackstopAllocPool; ap.AmountUSD != nil || strOr(ap.USDReason) != "all_excluded" || ap.Amount != nil || excludedAs(em.Day, a, "valuation_stale") == nil {
		t.Fatalf("backstop_alloc_pool = %+v; want the stale row left out, named valuation_stale", ap)
	}
	e := excludedAs(em.Day, b, "price_missing")
	if ab := s.BackstopAllocBackstop; strOr(ab.AmountUSD) != "3.00" || !ab.LowerBound || ab.AmountLowerBound || e == nil || e.Count == nil || *e.Count != 5 {
		t.Fatalf("backstop_alloc_backstop = %+v excluded %+v; want 3.00, a USD lower bound naming 5 unpriced hours", ab, em.Day.Excluded)
	}
}

// A failed sums read names every pool and degrades the block, and the response is not cached as good.
func TestLendingProtocols_EmissionsReadFailureDegrades(t *testing.T) {
	f0 := newProtocolsServer(t, protocolsCase{clean: true})
	a := f0.activePool
	store := &emissionsStore{covs: map[string]timescale.LendingEmissionsUSDCoverage{a: emisCov(a, 100)}, failSum: true}
	stats, resp := getProtocols(t, emissionsServer(t, store, emisVerified))
	assertProtocolsNoStore(t, resp, true)
	em := stats[0].Emissions
	pc := em.AllTime.Streams.PoolClaims
	if !em.Degraded || pc.AmountUSD != nil || strOr(pc.USDReason) != "not_valued" || excludedAs(em.AllTime, a, "emissions_read_failed") == nil {
		t.Fatalf("failed read served %+v excluded %+v degraded %v", pc, em.AllTime.Excluded, em.Degraded)
	}
}

// A covered day with no stored row is an exact zero only while the stream's writer is verified back to its first ledger
// now; unverified, stale (whatever it was when the day was stored) or unread, the stream is a lower bound naming why.
func TestLendingProtocols_EmissionsHistoryVerdictBoundsEmptyDays(t *testing.T) {
	f0 := newProtocolsServer(t, protocolsCase{clean: true})
	a, b := f0.activePool, f0.frozenPool
	store := &emissionsStore{covs: map[string]timescale.LendingEmissionsUSDCoverage{a: emisCov(a, 100), b: emisCov(b, 100)}}
	stale := verifiedHistory("blend", "blend_backstop")
	stale.snaps[0].ComputedAt = time.Now().Add(-30 * 24 * time.Hour)
	for name, c := range map[string]struct {
		reader         v1.CompletenessReader
		reason         string
		degraded, cold bool
	}{
		"verified":   {reader: emisVerified},
		"unverified": {reason: "history_unverified"},
		"stale":      {reader: stale, reason: "history_verdict_stale"},
		"unread":     {reader: &stubCompletenessReader{err: errors.New("pg down")}, reason: "history_verdict_unread", degraded: true},
	} {
		t.Run(name, func(t *testing.T) {
			stats, resp := getProtocols(t, emissionsServer(t, store, c.reader))
			em := stats[0].Emissions
			pc := em.Day.Streams.PoolClaims
			if strOr(pc.AmountUSD) != "0.00" || pc.LowerBound != (c.reason != "") || pc.AmountLowerBound != (c.reason != "") || em.Degraded != c.degraded {
				t.Fatalf("1d pool_claims = %+v degraded %v; want 0.00, lower bound %v", pc, em.Degraded, c.reason != "")
			}
			if c.reason == "" {
				return
			}
			named := false
			for _, e := range em.Day.Excluded {
				named = named || (e.Pool == nil && strOr(e.Stream) == "pool_claims" && e.Reason == c.reason)
			}
			if !named {
				t.Fatalf("1d excluded %+v; want pool_claims named %s", em.Day.Excluded, c.reason)
			}
			assertProtocolsNoStore(t, resp, c.degraded)
		})
	}
}

// Stored USD in a token that is not the protocol token's verified SAC is refused at read time, as /emissions/history
// refuses it, while its tokens are still counted; a token walk that cannot be read is price_read_failed and degraded.
func TestLendingProtocols_EmissionsReadTimeRefusals(t *testing.T) {
	f0 := newProtocolsServer(t, protocolsCase{clean: true})
	a, b := f0.activePool, f0.frozenPool
	foreign := emisGroup(a, "pool_claims", 0, "10", "5")
	other := mkCStrkey(t, 9)
	foreign.EmissionToken = &other
	walked := emisGroup(b, "reserve_schedule", 0, "3", "3")
	walked.LastValuedDay = emisThrough // this fixture has no price history, so the BLND day walk fails
	store := &emissionsStore{
		covs: map[string]timescale.LendingEmissionsUSDCoverage{a: emisCov(a, 100), b: emisCov(b, 100)},
		sums: []timescale.LendingEmissionsUSDBucketSum{foreign, walked},
	}
	em := blendEmissions(t, store)
	pc, rs := em.Day.Streams.PoolClaims, em.Day.Streams.ReserveSchedule
	if pc.AmountUSD != nil || strOr(pc.USDReason) != "all_excluded" || excludedAs(em.Day, a, "emission_token_unverified") == nil {
		t.Fatalf("pool_claims = %+v excluded %+v; want the foreign token's USD refused", pc, em.Day.Excluded)
	}
	if rs.AmountUSD != nil || excludedAs(em.Day, b, "price_read_failed") == nil || !em.Degraded {
		t.Fatalf("reserve_schedule = %+v excluded %+v degraded %v; want price_read_failed", rs, em.Day.Excluded, em.Degraded)
	}
	if pc.Amount != nil || strOr(pc.AmountReason) != "emission_token_mixed" || em.EmissionToken != nil {
		t.Fatalf("pool_claims amount %s (%s), token %s; want null: two tokens", strOr(pc.Amount), strOr(pc.AmountReason), strOr(em.EmissionToken))
	}
}

// The block never sums base units it cannot name one token for: a row with no token, or no decimals, nulls every amount
// with its reason while the USD is summed as usual.
func TestLendingProtocols_EmissionsAmountNeedsOneToken(t *testing.T) {
	f0 := newProtocolsServer(t, protocolsCase{clean: true})
	a, b := f0.activePool, f0.frozenPool
	noDecimals := emisGroup(b, "pool_claims", 0, "4", "1")
	noDecimals.EmissionTokenDecimals = nil
	noToken := emisGroup(b, "pool_claims", 0, "4", "1")
	noToken.EmissionToken = nil
	for reason, g := range map[string]timescale.LendingEmissionsUSDBucketSum{"emission_token_decimals_unknown": noDecimals, "emission_token_not_stored": noToken} {
		t.Run(reason, func(t *testing.T) {
			store := &emissionsStore{
				covs: map[string]timescale.LendingEmissionsUSDCoverage{a: emisCov(a, 100), b: emisCov(b, 100)},
				sums: []timescale.LendingEmissionsUSDBucketSum{emisGroup(a, "reserve_schedule", 0, "2", "2"), g},
			}
			em := blendEmissions(t, store)
			rs := em.Day.Streams.ReserveSchedule
			if rs.Amount != nil || strOr(rs.AmountReason) != reason || strOr(rs.AmountUSD) != "2.00" || em.EmissionToken != nil {
				t.Fatalf("reserve_schedule = %+v token %s; want amount null (%s), USD 2.00", rs, strOr(em.EmissionToken), reason)
			}
		})
	}
}
