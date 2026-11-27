package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stellar/go-stellar-sdk/network"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// windowEmisLending serves emission events by window as the store does: hourly rows filtered to [From, To) (the backstop
// streams to BackstopTo), summed per UTC day at 1d with the hours in the same read; skew moves a day's pool-claims total
// off its hours, so the day's reads disagree.
type windowEmisLending struct {
	stubBLNTLendingReader
	schedule []timescale.EmissionScheduleEvent
	claims   []timescale.EmissionClaimBucket
	alloc    []timescale.EmissionAllocBucket
	tiers    []timescale.EmissionBackstopClaimBucket
	skew     map[time.Time]int64
	failHrs  bool // the hourly read fails, as the store reports it, with the whole days
}

func inWindow[T any](rows []T, start func(T) time.Time, from, to time.Time) []T {
	out := []T{}
	for _, r := range rows {
		if s := start(r); !s.Before(from) && s.Before(to) {
			out = append(out, r)
		}
	}
	return out
}

func dayOf(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

func (r *windowEmisLending) LendingEmissionsHistory(_ context.Context, _ string, _ bool, q timescale.EmissionHistoryQuery) (timescale.LendingEmissionsHistory, error) {
	bto := q.BackstopTo
	if bto.IsZero() {
		bto = q.To
	}
	var h timescale.LendingEmissionsHistory
	if q.Schedule {
		last := map[uint32]timescale.EmissionScheduleEvent{}
		for _, e := range r.schedule {
			if !e.At.After(q.From) {
				last[e.TokenID] = e
			}
		}
		for _, e := range r.schedule {
			if l, ok := last[e.TokenID]; (ok && l.At.Equal(e.At)) || (e.At.After(q.From) && e.At.Before(q.To)) {
				h.Schedule = append(h.Schedule, e)
			}
		}
	}
	h.Claims = inWindow(r.claims, func(b timescale.EmissionClaimBucket) time.Time { return b.Start }, q.From, q.To)
	if q.BackstopAlloc {
		h.Alloc = inWindow(r.alloc, func(b timescale.EmissionAllocBucket) time.Time { return b.Start }, q.From, bto)
	}
	if q.BackstopClaim {
		h.BackstopClaims = inWindow(r.tiers, func(b timescale.EmissionBackstopClaimBucket) time.Time { return b.Start }, q.From, bto)
	}
	if !q.Daily {
		return h, nil
	}
	hours := h
	hours.Schedule = nil
	d := timescale.LendingEmissionsHistory{Schedule: h.Schedule}
	add := func(a, b canonical.Amount) canonical.Amount {
		return canonical.NewAmount(new(big.Int).Add(a.BigInt(), b.BigInt()))
	}
	for _, c := range h.Claims {
		c.Start = dayOf(c.Start)
		if i := slices.IndexFunc(d.Claims, func(o timescale.EmissionClaimBucket) bool {
			return o.Start.Equal(c.Start) && o.Scope == c.Scope && reflect.DeepEqual(o.TokenID, c.TokenID)
		}); i >= 0 {
			o := &d.Claims[i]
			o.Claims, o.Claimers, o.AmountUnread, o.Amount = o.Claims+c.Claims, o.Claimers+c.Claimers, o.AmountUnread+c.AmountUnread, add(o.Amount, c.Amount)
			continue
		}
		d.Claims = append(d.Claims, c)
	}
	for i := range d.Claims {
		if s := r.skew[d.Claims[i].Start]; s != 0 && d.Claims[i].Scope == timescale.EmissionClaimScopePool {
			d.Claims[i].Amount = add(d.Claims[i].Amount, canonical.NewAmount(big.NewInt(s)))
		}
	}
	for _, a := range h.Alloc {
		a.Start = dayOf(a.Start)
		if n := len(d.Alloc); n > 0 && d.Alloc[n-1].Start.Equal(a.Start) {
			o := &d.Alloc[n-1]
			o.Events, o.Unread, o.BackstopShare, o.PoolShare = o.Events+a.Events, o.Unread+a.Unread, add(o.BackstopShare, a.BackstopShare), add(o.PoolShare, a.PoolShare)
			continue
		}
		d.Alloc = append(d.Alloc, a)
	}
	for _, c := range h.BackstopClaims {
		c.Start = dayOf(c.Start)
		d.BackstopClaims = append(d.BackstopClaims, c) // one claim per tier-day in these fixtures
	}
	if q.Hours && r.failHrs {
		return d, fmt.Errorf("%w: connection reset", timescale.ErrEmissionHoursRead)
	}
	if q.Hours {
		d.Hours = &hours
	}
	return d, nil
}

// memEmisStore is the rollup's Postgres side in memory; failReplace fails the next row writes.
type memEmisStore struct {
	pools       map[string]time.Time
	rows        map[string]timescale.LendingEmissionsUSDDay // by pool, day, stream, line
	covs        map[string]timescale.LendingEmissionsUSDCoverage
	failReplace int
	since       []time.Time
}

func newMemEmisStore(pools map[string]time.Time) *memEmisStore {
	return &memEmisStore{pools: pools, rows: map[string]timescale.LendingEmissionsUSDDay{}, covs: map[string]timescale.LendingEmissionsUSDCoverage{}}
}

func memEmisKey(pool string, day time.Time, stream, line string) string {
	return pool + "|" + day.Format(time.DateOnly) + "|" + stream + "|" + line
}

func (m *memEmisStore) HoldInstanceLock(context.Context, string, func(), *slog.Logger) (*timescale.InstanceLock, error) {
	return nil, nil
}

func (m *memEmisStore) LendingEmissionPools(context.Context) (map[string]time.Time, error) {
	return m.pools, nil
}

func (m *memEmisStore) LendingEmissionsUSDDays(_ context.Context, pool string, from, through time.Time) ([]timescale.LendingEmissionsUSDDay, error) {
	var out []timescale.LendingEmissionsUSDDay
	for _, d := range m.rows {
		if d.Pool == pool && !d.Day.Before(from) && !d.Day.After(through) {
			out = append(out, d)
		}
	}
	return out, nil
}

func (m *memEmisStore) ReplaceLendingEmissionsUSDDays(_ context.Context, pool string, days []time.Time, rows []timescale.LendingEmissionsUSDDay) error {
	if len(days) == 0 {
		return nil
	}
	if m.failReplace > 0 {
		m.failReplace--
		return errors.New("store failed")
	}
	for k, d := range m.rows {
		if d.Pool == pool && slices.ContainsFunc(days, d.Day.Equal) {
			delete(m.rows, k)
		}
	}
	for _, d := range rows {
		m.rows[memEmisKey(d.Pool, d.Day, d.Stream, d.Line)] = d
	}
	return nil
}

func (m *memEmisStore) LendingEmissionsUSDStaleRevDays(context.Context, int16, int) (map[string][]time.Time, error) {
	return nil, nil
}

func (m *memEmisStore) LendingEmissionsUSDCoverages(context.Context) (map[string]timescale.LendingEmissionsUSDCoverage, error) {
	out := map[string]timescale.LendingEmissionsUSDCoverage{}
	for k, c := range m.covs {
		out[k] = c
	}
	return out, nil
}

func (m *memEmisStore) UpsertLendingEmissionsUSDCoverage(_ context.Context, c timescale.LendingEmissionsUSDCoverage) error {
	m.covs[c.Pool] = c
	return nil
}

func (m *memEmisStore) SourceExclusionWindowsChangedSince(_ context.Context, since time.Time) ([][2]time.Time, error) {
	m.since = append(m.since, since)
	return nil, nil
}

func (m *memEmisStore) day(pool string, day time.Time) []timescale.LendingEmissionsUSDDay {
	out, _ := m.LendingEmissionsUSDDays(context.Background(), pool, day, day)
	return out
}

func (m *memEmisStore) row(t *testing.T, pool string, day time.Time, stream, line string) timescale.LendingEmissionsUSDDay {
	t.Helper()
	d, ok := m.rows[memEmisKey(pool, day, stream, line)]
	if !ok {
		t.Fatalf("no stored %s/%s row on %s", stream, line, day.Format(time.DateOnly))
	}
	return d
}

// emisRollupServer is emisUSDServer over a windowed reader.
func emisRollupServer(t *testing.T, lr *windowEmisLending, token string, prices map[time.Time]string, opts v1.Options) *v1.Server {
	t.Helper()
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	st := emissionStreams(t, 1)
	st.EmissionToken = token
	opts.Explorer = emisHistExplorer{
		stubEmissionsExplorer: &stubEmissionsExplorer{stubExplorerReader: &stubExplorerReader{}, state: st},
		closed:                map[uint32]time.Time{emisAtTip: now},
	}
	opts.Lending, opts.VerifiedCurrencies = lr, cat
	opts.History = &rangeRecordingHistory{lockedHistoryReader: pairPrices(map[string]map[time.Time]string{
		usdPair(t, blndAssetID): prices, usdPair(t, blntAssetID): prices,
	})}
	opts.TokenDecimals = &lockstepDecStub{d: 7, found: true}
	opts.LakeWatermark = fixedWatermark{at: now}
	opts.Cursors = emisCursors{"blend": emisAtTip, "blend_backstop": emisAtTip, "blnt": emisAtTip}
	opts.LendingWriters = allEmisWriters
	opts.CompletenessReader = verifiedHistory("blend", "blend_backstop", "blnt")
	srv := v1.New(opts)
	srv.NetworkPassphrase = network.PublicNetworkPassphrase
	return srv
}

func emisDay(i int) time.Time { return emisT0.Add(time.Duration(i) * 24 * time.Hour) }

func emisAt(day int, hour float64) time.Time {
	return emisDay(day).Add(time.Duration(hour * float64(time.Hour)))
}

func claimAt(t *testing.T, day int, hour float64, scope string, id *uint32, claims, unread int64, amount string) timescale.EmissionClaimBucket {
	return timescale.EmissionClaimBucket{
		Start: emisAt(day, hour), Scope: scope, TokenID: id, Claims: claims, Claimers: claims,
		Amount: mustAmount(t, amount), AmountUnread: unread,
	}
}

func rollOnce(t *testing.T, srv *v1.Server, store *memEmisStore) v1.LendingRevenueRollupStats {
	t.Helper()
	r := srv.NewLendingEmissionsRollup(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	st, err := r.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// servedDay is one stream line's figures as /emissions/history?interval=1d serves them for a day, re-derived here from
// the served body: the allocation's two shares each as their own line, with the shares' shared coverage.
type servedDay struct {
	tokens   string
	usd      map[string]any
	streamLB bool
}

func usdJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func servedLines(t *testing.T, v v1.LendingEmissionsHistoryView, day time.Time) map[string]servedDay {
	t.Helper()
	out := map[string]servedDay{}
	at := func(w v1.WireTime) bool { return time.Time(w).Equal(day) }
	for _, l := range v.Reserves.Streams {
		for _, p := range l.Points {
			if at(p.T) {
				out["reserve_schedule|"+strconv.Itoa(int(l.TokenID))] = servedDay{p.Tokens, usdJSON(t, p.USD), v.Reserves.LowerBound}
			}
		}
	}
	for _, p := range v.PoolClaims.Points {
		if at(p.T) {
			out["pool_claims|"] = servedDay{p.Amount, usdJSON(t, p.USD), v.PoolClaims.LowerBound}
		}
	}
	for _, l := range v.PoolClaims.ByReserve.Reserves {
		for _, p := range l.Points {
			if at(p.T) {
				out["pool_claims_reserve|"+strconv.Itoa(int(l.TokenID))] = servedDay{p.Amount, usdJSON(t, p.USD), v.PoolClaims.ByReserve.LowerBound}
			}
		}
	}
	for _, p := range v.BackstopAlloc.Points {
		if !at(p.T) {
			continue
		}
		for _, leg := range []struct{ stream, tokens, key string }{
			{"backstop_alloc_backstop", p.BackstopEmissions, "backstop_emissions_usd"}, {"backstop_alloc_pool", p.PoolEmissions, "pool_emissions_usd"},
		} {
			m := usdJSON(t, p.USD)
			m["amount_usd"] = m[leg.key]
			delete(m, "backstop_emissions_usd")
			delete(m, "pool_emissions_usd")
			out[leg.stream+"|"] = servedDay{leg.tokens, m, v.BackstopAlloc.LowerBound}
		}
	}
	for _, tier := range v.BackstopClaims.Tiers {
		for _, p := range tier.Points {
			if at(p.T) {
				out["backstop_claims|"+tier.Tier] = servedDay{p.BLNTAmount, usdJSON(t, p.USD), v.BackstopClaims.LowerBound}
			}
		}
	}
	return out
}

// assertStoredEqualsServed: the rows stored for day are exactly the lines /emissions/history?interval=1d serves for it,
// each with the served tokens, the served USD once its exact value is rounded, and the stream's lower bound.
func assertStoredEqualsServed(t *testing.T, srv *v1.Server, store *memEmisStore, pool string, day time.Time) {
	t.Helper()
	q := fmt.Sprintf("?interval=1d&from=%s&to=%s", day.Format(time.RFC3339), day.Add(24*time.Hour).Format(time.RFC3339))
	view, _ := getEmissionsHistory(t, srv, pool, q, http.StatusOK)
	want := servedLines(t, view, day)
	rows := store.day(pool, day)
	if len(rows) != len(want) {
		t.Errorf("%s: %d rows stored, %d lines served: %+v vs %+v", day.Format(time.DateOnly), len(rows), len(want), rows, want)
	}
	for _, d := range rows {
		w, ok := want[d.Stream+"|"+d.Line]
		if !ok {
			t.Errorf("%s: stored %s/%s is not served", day.Format(time.DateOnly), d.Stream, d.Line)
			continue
		}
		got, err := v1.EmissionUSDFromRowForTest(d)
		if err != nil {
			t.Fatal(err)
		}
		if g := usdJSON(t, got); d.Amount != w.tokens || !reflect.DeepEqual(g, w.usd) || d.StreamLowerBound != w.streamLB {
			t.Errorf("%s %s/%s: stored tokens %s usd %v stream lower bound %v; served %s %v %v", day.Format(time.DateOnly),
				d.Stream, d.Line, d.Amount, g, d.StreamLowerBound, w.tokens, w.usd, w.streamLB)
		}
	}
}

func blendV2Rollup(t *testing.T, opts v1.Options) (*windowEmisLending, map[time.Time]string) {
	lr := &windowEmisLending{
		stubBLNTLendingReader: v2Lending(),
		schedule: []timescale.EmissionScheduleEvent{{
			TokenID: 1, At: emisAt(-1, 0), EPS: mustAmount(t, "10000000000"),
			Expiration: mustAmount(t, strconv.FormatInt(emisDay(3).Unix(), 10)),
		}},
		claims: []timescale.EmissionClaimBucket{
			// Day 0: priced claims; one names one reserve, one names several.
			claimAt(t, 0, 1, "pool", nil, 1, 0, "30000000"), claimAt(t, 0, 1, "single", u32(1), 1, 0, "30000000"),
			claimAt(t, 0, 5, "pool", nil, 1, 0, "10000000"), claimAt(t, 0, 5, "several", nil, 1, 0, "10000000"),
			// Day 1: one hour priced by a carried price, one past the six-hour bound.
			claimAt(t, 1, 3, "pool", nil, 1, 0, "10000000"), claimAt(t, 1, 10, "pool", nil, 1, 0, "10000000"),
			// Day 2: an operator exclusion over the 04:00 price.
			claimAt(t, 2, 4, "pool", nil, 1, 0, "10000000"), claimAt(t, 2, 8, "pool", nil, 1, 0, "10000000"),
			// Day 3: one of two claims' amounts unread.
			claimAt(t, 3, 2, "pool", nil, 2, 1, "10000000"),
			// Day 4: the day's total disagrees with its hours.
			claimAt(t, 4, 6, "pool", nil, 1, 0, "10000000"),
		},
		alloc: []timescale.EmissionAllocBucket{
			{Start: emisAt(0, 2), Events: 1, BackstopShare: mustAmount(t, "7000000000"), PoolShare: mustAmount(t, "3000000000")},
			{Start: emisAt(3, 2), Events: 1, Unread: 1, BackstopShare: mustAmount(t, "0"), PoolShare: mustAmount(t, "0")},
		},
		skew: map[time.Time]int64{emisDay(4): -1},
	}
	_ = opts
	prices := map[time.Time]string{
		emisAt(0, 0): "0.5", emisAt(0, 1): "1", emisAt(0, 2): "1", emisAt(0, 5): "2", emisAt(1, 0): "1",
		emisAt(2, 4): "1", emisAt(2, 8): "3", emisAt(3, 2): "1", emisAt(4, 6): "1",
	}
	return lr, prices
}

func blendV2RollupServer(t *testing.T) (*v1.Server, *memEmisStore, string) {
	lr, prices := blendV2Rollup(t, v1.Options{})
	excl := assetExclGate(exclTable{rows: []timescale.SourceExclusion{{
		ID: 1, Source: "soroswap", Base: blndAssetID, Quote: "fiat:USD", From: emisAt(2, 4), Until: emisAt(2, 5),
	}}})
	srv := emisRollupServer(t, lr, emisSAC(t, blndAssetID), prices, v1.Options{Exclusions: excl})
	pool := mkCStrkey(t, 1)
	return srv, newMemEmisStore(map[string]time.Time{pool: emisAt(0, 1)}), pool
}

// TestLendingEmissionsRollup_StoredEqualsServed: for every day of a Blend V2 pool (priced, carried, stale, excluded hour,
// unread amounts, reads that disagree, single- and several-reserve claims, both allocation shares) the stored rows are
// what /emissions/history?interval=1d serves for that day: the same lines, tokens, USD to the cent once rounded, and
// lower bounds. Coverage runs through the last whole served day.
func TestLendingEmissionsRollup_StoredEqualsServed(t *testing.T) {
	srv, store, pool := blendV2RollupServer(t)
	st := rollOnce(t, srv, store)
	through := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	if c := store.covs[pool]; !c.FloorDay.Equal(emisDay(0)) || !c.ThroughDay.Equal(through) || c.Protocol != "blend" {
		t.Fatalf("coverage %+v, want days %s..%s", c, emisDay(0).Format(time.DateOnly), through.Format(time.DateOnly))
	}
	if st.DaysChecked != int(through.Sub(emisDay(0))/(24*time.Hour))+1 || st.Written != len(store.rows) {
		t.Errorf("stats %+v for %d rows", st, len(store.rows))
	}
	for i := range 6 {
		assertStoredEqualsServed(t, srv, store, pool, emisDay(i))
	}

	// The facts a stored row must keep, each named.
	if d := store.row(t, pool, emisDay(1), "pool_claims", ""); d.AmountUSD == nil || !d.LowerBound || d.HoursUnvalued != 1 || d.HoursCarried != 1 {
		t.Errorf("stale day: %+v, want the priced part, a lower bound over one unvalued and one carried hour", d)
	}
	if d := store.row(t, pool, emisDay(3), "backstop_alloc_pool", ""); d.AmountUSD != nil || d.USDReason == nil || *d.USDReason != "amount_unread" {
		t.Errorf("unread allocation: %+v, want null usd with amount_unread, never 0", d)
	}
	if d := store.row(t, pool, emisDay(4), "pool_claims", ""); d.AmountUSD != nil || d.USDReason == nil || *d.USDReason != "reads_disagree" {
		t.Errorf("disagreeing day: %+v, want null usd with reads_disagree", d)
	}
	if d := store.row(t, pool, emisDay(0), "pool_claims_reserve", "1"); !d.StreamLowerBound || !slices.Contains(d.Excluded, "claim_names_several_reserves") {
		t.Errorf("by-reserve line: %+v, want the several-reserve exclusion and its lower bound", d)
	}
	pc, ba := store.row(t, pool, emisDay(0), "pool_claims", ""), store.row(t, pool, emisDay(0), "backstop_alloc_backstop", "")
	if pc.Amount != "40000000" || ba.Amount != "7000000000" || *pc.AmountUSD != "5" || *ba.AmountUSD != "700" {
		t.Errorf("day 0: pool claims %s ($%v), backstop share %s ($%v); each its own layer", pc.Amount, *pc.AmountUSD, ba.Amount, *ba.AmountUSD)
	}

	// A re-run over unchanged sources writes nothing.
	if st := rollOnce(t, srv, store); st.Written != 0 {
		t.Errorf("re-run wrote %d rows", st.Written)
	}
}

// TestLendingEmissionsRollup_RoundsOnlyOnRead: the stored USD is the exact sum; only the served figure is rounded.
func TestLendingEmissionsRollup_RoundsOnlyOnRead(t *testing.T) {
	lr := &windowEmisLending{stubBLNTLendingReader: v2Lending(), claims: []timescale.EmissionClaimBucket{
		claimAt(t, 0, 0, "pool", nil, 1, 0, "40000"), claimAt(t, 0, 1, "pool", nil, 1, 0, "40000"), claimAt(t, 0, 2, "pool", nil, 1, 0, "40000"),
	}}
	srv := emisRollupServer(t, lr, emisSAC(t, blndAssetID), map[time.Time]string{emisAt(0, 0): "1", emisAt(0, 1): "1", emisAt(0, 2): "1"}, v1.Options{})
	pool := mkCStrkey(t, 1)
	store := newMemEmisStore(map[string]time.Time{pool: emisAt(0, 0)})
	rollOnce(t, srv, store)
	d := store.row(t, pool, emisDay(0), "pool_claims", "")
	u, err := v1.EmissionUSDFromRowForTest(d)
	if err != nil {
		t.Fatal(err)
	}
	if *d.AmountUSD != "0.012" || *u.AmountUSD != "0.01" {
		t.Errorf("stored %s served %s, want exact 0.012 rounded once to 0.01", *d.AmountUSD, *u.AmountUSD)
	}
	assertStoredEqualsServed(t, srv, store, pool, emisDay(0))
}

// TestLendingEmissionsRollup_V1BLNTAndIdentity: a Blend V1 pool stores claims only, unpriced (its token is unread), with
// its unavailable streams on the coverage; a BLNT pool's schedule keeps its approximate marker and its tiers their own
// lines; a Blend pool naming BLNT is refused by the identity gate, stored unpriced and never valued.
func TestLendingEmissionsRollup_V1BLNTAndIdentity(t *testing.T) {
	v1Pool := blend.PoolV1
	claims := []timescale.EmissionClaimBucket{claimAt(t, 0, 1, "pool", nil, 1, 0, "10000000")}
	prices := map[time.Time]string{emisAt(0, 1): "1"}

	lr := &windowEmisLending{stubBLNTLendingReader: stubBLNTLendingReader{stubLendingReader: stubLendingReader{version: &v1Pool}}, claims: claims}
	srv := emisRollupServer(t, lr, emisSAC(t, blndAssetID), prices, v1.Options{})
	pool := mkCStrkey(t, 1)
	store := newMemEmisStore(map[string]time.Time{pool: emisAt(0, 1)})
	if st := rollOnce(t, srv, store); st.Refused == 0 {
		t.Errorf("v1: stats %+v, want the unread token counted refused", st)
	}
	if d := store.row(t, pool, emisDay(0), "pool_claims", ""); d.AmountUSD != nil || *d.USDReason != "emission_token_unread" || d.EmissionToken != nil {
		t.Errorf("v1 claims: %+v, want null usd, emission_token_unread", d)
	}
	if u := store.covs[pool].Unavailable; u["reserves"] != "v1_emission_schedule_unverified" || u["backstop_alloc"] != "v1_backstop_unaudited" {
		t.Errorf("v1 coverage unavailable %v", u)
	}
	assertStoredEqualsServed(t, srv, store, pool, emisDay(0))

	unknown := blend.PoolVersionUnknown
	lr = &windowEmisLending{
		stubBLNTLendingReader: stubBLNTLendingReader{stubLendingReader: stubLendingReader{version: &unknown}, blntAssets: []string{mkCStrkey(t, 3)}},
		schedule: []timescale.EmissionScheduleEvent{{
			TokenID: 1, At: emisAt(0, 1), EPS: mustAmount(t, "10000000000"),
			Expiration: mustAmount(t, strconv.FormatInt(emisAt(0, 2).Unix(), 10)),
		}},
		tiers: []timescale.EmissionBackstopClaimBucket{{
			Start: emisAt(0, 1), Tier: "FirstLoss", Claims: 1, Claimers: 1,
			BLNT: mustAmount(t, "50000000"), LP: mustAmount(t, "999999999999"), Shares: mustAmount(t, "7"),
		}},
	}
	srv = emisRollupServer(t, lr, emisSAC(t, blntAssetID), prices, v1.Options{})
	store = newMemEmisStore(map[string]time.Time{blnt.MainnetFixedPool: emisAt(0, 1)})
	rollOnce(t, srv, store)
	if d := store.row(t, blnt.MainnetFixedPool, emisDay(0), "reserve_schedule", "1"); !d.Approximate || d.ApproximateReason == nil ||
		*d.ApproximateReason != "stored_eps_rewritten_without_event" || d.LowerBound || d.AmountUSD == nil {
		t.Errorf("blnt schedule: %+v, want approximate (not a lower bound) and priced", d)
	}
	if d := store.row(t, blnt.MainnetFixedPool, emisDay(0), "backstop_claims", "FirstLoss"); d.Amount != "50000000" || *d.AmountUSD != "5" {
		t.Errorf("blnt tier: %+v, want blnt_amount valued alone", d)
	}
	assertStoredEqualsServed(t, srv, store, blnt.MainnetFixedPool, emisDay(0))

	lr = &windowEmisLending{stubBLNTLendingReader: v2Lending(), claims: claims}
	srv = emisRollupServer(t, lr, emisSAC(t, blntAssetID), prices, v1.Options{})
	store = newMemEmisStore(map[string]time.Time{pool: emisAt(0, 1)})
	if st := rollOnce(t, srv, store); st.Refused == 0 {
		t.Errorf("identity: stats %+v, want the refusal counted", st)
	}
	if d := store.row(t, pool, emisDay(0), "pool_claims", ""); d.AmountUSD != nil || *d.USDReason != "emission_token_unverified" {
		t.Errorf("blnt token on a blend pool: %+v, want null usd, emission_token_unverified", d)
	}
	assertStoredEqualsServed(t, srv, store, pool, emisDay(0))
}

// TestLendingEmissionsRollup_FailedStoreKeepsCoverageAndState: a cycle whose row write fails stores no coverage and
// commits no planning state (the next cycle reads exclusions from the same point); the next successful cycle stores the
// rows, then the coverage, and moves the exclusion read on.
func TestLendingEmissionsRollup_FailedStoreKeepsCoverageAndState(t *testing.T) {
	srv, store, pool := blendV2RollupServer(t)
	r := srv.NewLendingEmissionsRollup(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	store.failReplace = 1
	if _, err := r.Cycle(context.Background()); err == nil {
		t.Fatal("cycle with a failing store returned no error")
	}
	if len(store.covs) != 0 || len(store.rows) != 0 {
		t.Fatalf("after a failed write: coverage %+v, %d rows; want neither", store.covs, len(store.rows))
	}
	if _, err := r.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.since) != 2 || !store.since[1].Equal(store.since[0]) {
		t.Fatalf("exclusion reads since %v, want the failed cycle's point re-read", store.since)
	}
	if _, ok := store.covs[pool]; !ok || len(store.rows) == 0 {
		t.Fatalf("after a good cycle: coverage %+v, %d rows", store.covs, len(store.rows))
	}
	if _, err := r.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.since[2].After(store.since[1]) {
		t.Fatalf("exclusion reads since %v, want the read moved on after a good cycle", store.since)
	}
}

// TestLendingEmissionsRollup_CutReadIsRetriedNotStored: a day whose hourly read failed is served in tokens with its USD
// withheld, but the rollup stores none of it: coverage stops before it and the next cycle values it whole.
func TestLendingEmissionsRollup_CutReadIsRetriedNotStored(t *testing.T) {
	lr, prices := blendV2Rollup(t, v1.Options{})
	lr.failHrs = true
	srv := emisRollupServer(t, lr, emisSAC(t, blndAssetID), prices, v1.Options{})
	pool := mkCStrkey(t, 1)
	store := newMemEmisStore(map[string]time.Time{pool: emisAt(0, 1)})
	if st := rollOnce(t, srv, store); st.Degraded == 0 || len(store.rows) != 0 || !store.covs[pool].ThroughDay.Equal(emisDay(-1)) {
		t.Fatalf("hours failing: stats %+v, %d rows, coverage %+v; want nothing stored before day 0", st, len(store.rows), store.covs[pool])
	}
	lr.failHrs = false
	rollOnce(t, srv, store)
	if d := store.row(t, pool, emisDay(0), "pool_claims", ""); d.AmountUSD == nil || *d.AmountUSD != "5" {
		t.Fatalf("retried day 0: %+v", d)
	}
	assertStoredEqualsServed(t, srv, store, pool, emisDay(0))
}

// The emissions rollup's started and last-success gauges are the ones its stalled alert reads.
func TestLendingEmissionsRollup_SetsStartedAndSuccessGauges(t *testing.T) {
	srv, store, _ := blendV2RollupServer(t)
	r := srv.NewLendingEmissionsRollup(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	before := float64(time.Now().Unix())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx)
	if got := testutil.ToFloat64(obs.LendingEmissionsRollupStartedUnix); got < before {
		t.Fatalf("started gauge %v, want at least %v", got, before)
	}
	r.RunCycleForTest(context.Background())
	if got := testutil.ToFloat64(obs.LendingEmissionsRollupLastSuccessUnix); got < before {
		t.Fatalf("last-success gauge %v, want at least %v", got, before)
	}
}
