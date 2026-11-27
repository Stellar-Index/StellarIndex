package v1_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/network"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// usdEmisLending serves daily (Daily reads, with hourly as Hours when asked) and hourly (1h reads) emission history,
// recording each query; failHours fails the hours as the store does, returning the whole days.
type usdEmisLending struct {
	stubBLNTLendingReader
	daily, hourly timescale.LendingEmissionsHistory
	failHours     bool
	qs            []timescale.EmissionHistoryQuery
}

func (r *usdEmisLending) LendingEmissionsHistory(_ context.Context, _ string, _ bool, q timescale.EmissionHistoryQuery) (timescale.LendingEmissionsHistory, error) {
	r.qs = append(r.qs, q)
	if !q.Daily {
		return r.hourly, nil
	}
	out := r.daily
	if q.Hours {
		if r.failHours {
			return out, fmt.Errorf("%w: connection reset", timescale.ErrEmissionHoursRead)
		}
		hours := r.hourly
		out.Hours = &hours
	}
	return out, nil
}

func emisSAC(t *testing.T, assetID string) string {
	t.Helper()
	sac, ok := xdrjson.SACContractID(assetID, network.PublicNetworkPassphrase)
	if !ok {
		t.Fatalf("no SAC for %s", assetID)
	}
	return sac
}

func usdPair(t *testing.T, assetID string) string {
	t.Helper()
	a, err := canonical.ParseAsset(assetID)
	if err != nil {
		t.Fatal(err)
	}
	return canonical.Pair{Base: a, Quote: canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}}.String()
}

// emisUSDServer is blendV2Server with the pool naming token as its emission token, BLND and BLNT priced at prices, and
// the verified catalogue and network wired so the identity gate can pass.
func emisUSDServer(t *testing.T, lr *usdEmisLending, token string, prices map[time.Time]string, opts v1.Options) (*v1.Server, *rangeRecordingHistory) {
	t.Helper()
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	st := emissionStreams(t, 1)
	st.EmissionToken = token
	hist := &rangeRecordingHistory{lockedHistoryReader: pairPrices(map[string]map[time.Time]string{
		usdPair(t, blndAssetID): prices, usdPair(t, blntAssetID): prices,
	})}
	opts.Explorer = emisHistExplorer{
		stubEmissionsExplorer: &stubEmissionsExplorer{stubExplorerReader: &stubExplorerReader{}, state: st},
		closed:                map[uint32]time.Time{emisAtTip: now},
	}
	opts.Lending, opts.History, opts.VerifiedCurrencies = lr, hist, cat
	opts.TokenDecimals = &lockstepDecStub{d: 7, found: true}
	opts.LakeWatermark = fixedWatermark{at: now}
	opts.Cursors = emisCursors{"blend": emisAtTip, "blend_backstop": emisAtTip, "blnt": emisAtTip}
	opts.LendingWriters = allEmisWriters
	if opts.CompletenessReader == nil {
		opts.CompletenessReader = verifiedHistory("blend", "blend_backstop", "blnt")
	}
	srv := v1.New(opts)
	srv.NetworkPassphrase = network.PublicNetworkPassphrase
	return srv, hist
}

func v2Lending() stubBLNTLendingReader {
	v2 := blend.PoolV2
	return stubBLNTLendingReader{stubLendingReader: stubLendingReader{version: &v2}}
}

func poolClaim(t *testing.T, h float64, amount string) timescale.EmissionClaimBucket {
	return timescale.EmissionClaimBucket{Start: emisHour(h), Scope: "pool", Claims: 1, Claimers: 1, Amount: mustAmount(t, amount)}
}

func usdIs(u v1.LendingEmissionUSD, amount string, priced, unpriced int, lowerBound bool) bool {
	return u.AmountUSD != nil && *u.AmountUSD == amount && u.PricedHours == priced && u.UnpricedHours == unpriced &&
		u.LowerBound == lowerBound && u.USDReason == nil
}

func usdNull(u v1.LendingEmissionUSD, reason string) bool {
	return u.AmountUSD == nil && u.USDReason != nil && *u.USDReason == reason
}

// TestLendingEmissionsHistoryUSD_ValuedAtTheHour values every stream of a Blend V2 pool hour by hour at BLND's own hourly
// price, carrying it within the age bound: the schedule (0.36 tokens an hour), the pool claims and both allocation shares.
// The price is keyed by BLND's code and issuer, reached from the pool's SAC; the streams are never added together.
func TestLendingEmissionsHistoryUSD_ValuedAtTheHour(t *testing.T) {
	lr := &usdEmisLending{stubBLNTLendingReader: v2Lending(), hourly: timescale.LendingEmissionsHistory{
		Schedule: []timescale.EmissionScheduleEvent{
			{TokenID: 1, At: emisHour(-1), EPS: mustAmount(t, "10000000000"), Expiration: mustAmount(t, "18446744073709551615")},
		},
		Claims: []timescale.EmissionClaimBucket{poolClaim(t, 0, "30000000"), poolClaim(t, 2, "20000000")},
		Alloc:  []timescale.EmissionAllocBucket{{Start: emisHour(2), Events: 1, BackstopShare: mustAmount(t, "7000000000"), PoolShare: mustAmount(t, "3000000000")}},
	}}
	srv, hist := emisUSDServer(t, lr, emisSAC(t, blndAssetID), map[time.Time]string{emisHour(0): "0.5", emisHour(2): "1"}, v1.Options{})
	got, raw := getEmissionsHistory(t, srv, mkCStrkey(t, 1), emisHistQuery, http.StatusOK)

	if got.PriceMaxAgeHours != 6 || len(lr.qs) != 1 {
		t.Fatalf("price_max_age_hours = %d, reads = %d; want 6 and one read at 1h", got.PriceMaxAgeHours, len(lr.qs))
	}
	line := got.Reserves.Streams[0]
	if !usdIs(line.USD, "0.72", 3, 0, false) || line.USD.PriceCarriedHours != 0 || len(line.Points) != 3 ||
		!usdIs(line.Points[1].USD, "0.18", 1, 0, false) || !usdIs(line.Points[2].USD, "0.36", 1, 0, false) {
		t.Errorf("schedule usd = %+v points %+v", line.USD, line.Points)
	}
	pc := got.PoolClaims
	if !usdIs(pc.USD, "3.50", 2, 0, false) || !usdIs(pc.Points[0].USD, "1.50", 1, 0, false) || !usdIs(pc.Points[1].USD, "2.00", 1, 0, false) {
		t.Errorf("pool claims usd = %+v points %+v", pc.USD, pc.Points)
	}
	ba := got.BackstopAlloc.USD
	if ba == nil || ba.BackstopEmissionsUSD == nil || *ba.BackstopEmissionsUSD != "700.00" || *ba.PoolEmissionsUSD != "300.00" || ba.LowerBound {
		t.Errorf("alloc usd = %+v", ba)
	}
	if len(hist.reads) == 0 {
		t.Fatal("no price read")
	}
	for _, r := range hist.reads {
		if !strings.HasPrefix(r.pair, blndAssetID+"/") || r.from.Before(emisHour(-6)) || r.to.After(emisHour(3)) {
			t.Errorf("price read %+v, want BLND's USD books inside [-6h, 3h)", r)
		}
	}
	for _, k := range []string{"totals_usd", "total_usd"} {
		if strings.Contains(string(raw), `"`+k+`"`) {
			t.Errorf("response has a cross-stream %s", k)
		}
	}
}

// TestLendingEmissionsHistoryUSD_DailyRoundsOnce: a 1d bucket is valued from an hourly read of the same streams and bounds,
// each hour at its own price, and rounded once: three hours of $0.004 serve "0.01", not three rounded "0.00".
func TestLendingEmissionsHistoryUSD_DailyRoundsOnce(t *testing.T) {
	day := timescale.EmissionClaimBucket{Start: emisT0, Scope: "pool", Claims: 3, Claimers: 3, Amount: mustAmount(t, "120000")}
	lr := &usdEmisLending{
		stubBLNTLendingReader: v2Lending(),
		daily:                 timescale.LendingEmissionsHistory{Claims: []timescale.EmissionClaimBucket{day}},
		hourly: timescale.LendingEmissionsHistory{Claims: []timescale.EmissionClaimBucket{
			poolClaim(t, 0, "40000"), poolClaim(t, 1, "40000"), poolClaim(t, 2, "40000"),
		}},
	}
	prices := map[time.Time]string{emisHour(0): "1", emisHour(1): "1", emisHour(2): "1"}
	srv, _ := emisUSDServer(t, lr, emisSAC(t, blndAssetID), prices, v1.Options{})
	got, _ := getEmissionsHistory(t, srv, mkCStrkey(t, 1), "?interval=1d&from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z", http.StatusOK)

	if len(lr.qs) != 1 || !lr.qs[0].Daily || !lr.qs[0].Hours {
		t.Fatalf("reads = %+v, want one daily read asking for its hours in the same snapshot", lr.qs)
	}
	pc := got.PoolClaims
	if pc.Totals.Amount != "120000" || !usdIs(pc.USD, "0.01", 3, 0, false) || !usdIs(pc.Points[0].USD, "0.01", 3, 0, false) {
		t.Errorf("pool claims = %+v usd %+v point %+v", pc.Totals, pc.USD, pc.Points[0].USD)
	}

	// Hours that do not add up to their day leave the rest unvalued: one unpriced hour, a lower bound.
	lr.hourly.Claims = lr.hourly.Claims[:2]
	lr.qs = nil
	got, _ = getEmissionsHistory(t, srv, mkCStrkey(t, 1), "?interval=1d&from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z", http.StatusOK)
	if u := got.PoolClaims.USD; !usdIs(u, "0.01", 2, 1, true) {
		t.Errorf("short hours: usd = %+v, want 0.01 over 2 priced and 1 unpriced, a lower bound", u)
	}
}

// TestLendingEmissionsHistoryUSD_StaleAndUnpriced: an hour more than six hours past the last price is not valued at it.
// Its bucket is null with no_price_within_bound, never "0.00", and the stream total is the priced part flagged a lower bound.
func TestLendingEmissionsHistoryUSD_StaleAndUnpriced(t *testing.T) {
	lr := &usdEmisLending{stubBLNTLendingReader: v2Lending(), hourly: timescale.LendingEmissionsHistory{
		Claims: []timescale.EmissionClaimBucket{poolClaim(t, 0, "10000000"), poolClaim(t, 6, "10000000"), poolClaim(t, 8, "10000000")},
	}}
	srv, _ := emisUSDServer(t, lr, emisSAC(t, blndAssetID), map[time.Time]string{emisHour(0): "2"}, v1.Options{})
	got, _ := getEmissionsHistory(t, srv, mkCStrkey(t, 1), "?interval=1h&from=2026-11-01T00:00:00Z&to=2026-11-01T10:00:00Z", http.StatusOK)

	pc := got.PoolClaims
	if !usdIs(pc.USD, "4.00", 2, 1, true) || pc.USD.PriceCarriedHours != 1 {
		t.Errorf("pool usd = %+v, want 4.00 over 2 priced (one carried 6 h) and 1 unpriced, a lower bound", pc.USD)
	}
	if len(pc.Points) != 3 || !usdIs(pc.Points[1].USD, "2.00", 1, 0, false) || !usdNull(pc.Points[2].USD, "no_price_within_bound") ||
		pc.Points[2].USD.UnpricedHours != 1 {
		t.Errorf("points = %+v, want the 08:00 bucket null with no_price_within_bound", pc.Points)
	}
}

// TestLendingEmissionsHistoryUSD_IdentityGate: USD is served only for the protocol's verified token, by code and issuer.
// A Blend V1 pool's token is not read, so its claims are null (emission_token_unread); a Blend pool naming BLNT, or a
// BLND minted by another issuer, is emission_token_unverified. None of them reads a price or the hourly history.
func TestLendingEmissionsHistoryUSD_IdentityGate(t *testing.T) {
	prices := map[time.Time]string{emisHour(0): "1"}
	claims := timescale.LendingEmissionsHistory{Claims: []timescale.EmissionClaimBucket{poolClaim(t, 0, "10000000")}}
	v1Pool := blend.PoolV1
	for _, tc := range []struct {
		name, token, reason string
		reader              stubBLNTLendingReader
	}{
		{"blend v1", emisSAC(t, blndAssetID), "emission_token_unread", stubBLNTLendingReader{stubLendingReader: stubLendingReader{version: &v1Pool}}},
		{"blnt token on blend", emisSAC(t, blntAssetID), "emission_token_unverified", v2Lending()},
		{"blnd by code alone", emisSAC(t, "BLND-GAP2ACGH3O6TRFAERGM4USJ6XWU3U2BFIK224JPV6NI3CUNEEEGTBTFB"), "emission_token_unverified", v2Lending()},
	} {
		lr := &usdEmisLending{stubBLNTLendingReader: tc.reader, daily: claims, hourly: claims}
		srv, hist := emisUSDServer(t, lr, tc.token, prices, v1.Options{})
		got, _ := getEmissionsHistory(t, srv, mkCStrkey(t, 1), "?interval=1d&from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z", http.StatusOK)
		if !usdNull(got.PoolClaims.USD, tc.reason) || !usdNull(got.PoolClaims.Points[0].USD, tc.reason) {
			t.Errorf("%s: usd = %+v, want null %s", tc.name, got.PoolClaims.USD, tc.reason)
		}
		if len(hist.reads) != 0 || len(lr.qs) != 1 || lr.qs[0].Hours {
			t.Errorf("%s: %d price reads and reads %+v, want none and one without hours", tc.name, len(hist.reads), lr.qs)
		}
	}
}

// TestLendingEmissionsHistoryUSD_BLNT values a BLNT pool at BLNT's price; a backstop tier's usd values blnt_amount only.
func TestLendingEmissionsHistoryUSD_BLNT(t *testing.T) {
	unknown := blend.PoolVersionUnknown
	lr := &usdEmisLending{
		stubBLNTLendingReader: stubBLNTLendingReader{stubLendingReader: stubLendingReader{version: &unknown}, blntAssets: []string{mkCStrkey(t, 3)}},
		hourly: timescale.LendingEmissionsHistory{
			BackstopClaims: []timescale.EmissionBackstopClaimBucket{
				{Start: emisHour(1), Tier: "FirstLoss", Claims: 1, Claimers: 1, BLNT: mustAmount(t, "50000000"), LP: mustAmount(t, "999999999999"), Shares: mustAmount(t, "7")},
			},
			Schedule: []timescale.EmissionScheduleEvent{
				{TokenID: 1, At: emisHour(1), EPS: mustAmount(t, "10000000000"), Expiration: mustAmount(t, "1793498400")}, // 02:00
			},
		},
	}
	srv, hist := emisUSDServer(t, lr, emisSAC(t, blntAssetID), map[time.Time]string{emisHour(1): "0.01"}, v1.Options{})
	got, _ := getEmissionsHistory(t, srv, blnt.MainnetFixedPool, emisHistQuery, http.StatusOK)
	tiers := got.BackstopClaims.Tiers
	if len(tiers) != 1 || !usdIs(tiers[0].USD, "0.05", 1, 0, false) || !usdIs(tiers[0].Points[0].USD, "0.05", 1, 0, false) {
		t.Errorf("tiers = %+v", tiers)
	}
	// BLNT's schedule is rebuilt to dust: its usd says so, and is not a lower bound for it.
	if l := got.Reserves.Streams[0]; !l.USD.Approximate || l.USD.LowerBound || !l.Points[0].USD.Approximate || l.USD.AmountUSD == nil {
		t.Errorf("schedule usd = %+v, want approximate, not a lower bound", l.USD)
	}
	for _, r := range hist.reads {
		if !strings.HasPrefix(r.pair, blntAssetID+"/") {
			t.Errorf("price read %s, want BLNT's USD books only", r.pair)
		}
	}
}

// TestLendingEmissionsHistoryUSD_ThinWithheld: a thin BLND market is withheld as /revenue withholds it: every figure
// carrying tokens is null with withheld, never "0.00", and a stream with no tokens is still "0.00".
func TestLendingEmissionsHistoryUSD_ThinWithheld(t *testing.T) {
	lr := &usdEmisLending{stubBLNTLendingReader: v2Lending(), hourly: timescale.LendingEmissionsHistory{
		Claims: []timescale.EmissionClaimBucket{poolClaim(t, 1, "10000000")},
	}}
	thin := pricingguard.NewSubstanceGate(thinStore{thin: map[string]bool{blndAssetID: true}}, pricingguard.SubstanceGateOptions{})
	srv, _ := emisUSDServer(t, lr, emisSAC(t, blndAssetID), map[time.Time]string{emisHour(1): "1"}, v1.Options{Substance: thin})
	got, _ := getEmissionsHistory(t, srv, mkCStrkey(t, 1), emisHistQuery, http.StatusOK)
	if u := got.PoolClaims.USD; !usdNull(u, "withheld") || u.UnpricedHours != 1 || !u.LowerBound {
		t.Errorf("pool claims usd = %+v, want null withheld over 1 unpriced hour", u)
	}
	if !usdNull(got.PoolClaims.Points[0].USD, "withheld") {
		t.Errorf("point usd = %+v, want null withheld", got.PoolClaims.Points[0].USD)
	}
	if a := got.BackstopAlloc.USD; a == nil || a.BackstopEmissionsUSD == nil || *a.BackstopEmissionsUSD != "0.00" || a.USDReason != nil {
		t.Errorf("alloc usd = %+v, want 0.00: no tokens are worth nothing whatever the price", a)
	}
}

// TestLendingEmissionsHistoryUSD_ExcludedSourceDoesNotValueHour: an operator exclusion over an hour's BLND price leaves
// that hour unvalued, counted, and the stream a lower bound.
func TestLendingEmissionsHistoryUSD_ExcludedSourceDoesNotValueHour(t *testing.T) {
	get := func(excl v1.PriceExclusionGate) (v1.LendingEmissionUSD, v1.Flags) {
		t.Helper()
		lr := &usdEmisLending{stubBLNTLendingReader: v2Lending(), hourly: timescale.LendingEmissionsHistory{
			Claims: []timescale.EmissionClaimBucket{poolClaim(t, 1, "10000000"), poolClaim(t, 10, "10000000")},
		}}
		srv, _ := emisUSDServer(t, lr, emisSAC(t, blndAssetID), map[time.Time]string{emisHour(1): "0.1", emisHour(10): "0.3"}, v1.Options{Exclusions: excl})
		got, raw := getEmissionsHistory(t, srv, mkCStrkey(t, 1), "?interval=1h&from=2026-11-01T00:00:00Z&to=2026-11-01T12:00:00Z", http.StatusOK)
		return got.PoolClaims.USD, emisFlags(t, raw)
	}
	if u, f := get(nil); !usdIs(u, "0.40", 2, 0, false) || f.FillsWithheldReason != "" {
		t.Fatalf("control: usd = %+v flags %+v, want 0.40 complete, nothing withheld", u, f)
	}
	u, flags := get(assetExclGate(exclTable{rows: []timescale.SourceExclusion{{
		ID: 1, Source: "soroswap", Base: blndAssetID, Quote: "fiat:USD", From: emisHour(10), Until: emisHour(11),
	}}}))
	if !usdIs(u, "0.10", 1, 1, true) {
		t.Errorf("excluded: usd = %+v, want 0.10 with the excluded hour unpriced, a lower bound", u)
	}
	if flags.FillsWithheldReason != v1.PriceWithheldSourceExcluded {
		t.Errorf("flags = %+v; the walk's withholding must reach the envelope", flags)
	}
}

func emisFlags(t *testing.T, raw []byte) v1.Flags {
	t.Helper()
	var env struct{ Flags v1.Flags }
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	return env.Flags
}

const emisDayQuery = "?interval=1d&from=2026-11-01T00:00:00Z&to=2026-11-02T00:00:00Z"

// TestLendingEmissionsHistoryUSD_HoursNeverExceedTheDay: a day whose hours carry more than it (or none of it) values
// nothing, reads_disagree, rather than serving more USD than its tokens support; a day missing some hours stays a lower
// bound over the hours it has.
func TestLendingEmissionsHistoryUSD_HoursNeverExceedTheDay(t *testing.T) {
	prices := map[time.Time]string{emisHour(0): "1", emisHour(1): "1", emisHour(2): "1"}
	day := timescale.EmissionClaimBucket{Start: emisT0, Scope: "pool", Claims: 1, Claimers: 1, Amount: mustAmount(t, "10000000")}
	for _, tc := range []struct {
		name  string
		hours []timescale.EmissionClaimBucket
	}{
		{"hours past the day", []timescale.EmissionClaimBucket{poolClaim(t, 0, "10000000"), poolClaim(t, 1, "10000000"), poolClaim(t, 2, "10000000")}},
		{"no hours", nil},
	} {
		lr := &usdEmisLending{
			stubBLNTLendingReader: v2Lending(),
			daily:                 timescale.LendingEmissionsHistory{Claims: []timescale.EmissionClaimBucket{day}}, hourly: timescale.LendingEmissionsHistory{Claims: tc.hours},
		}
		srv, _ := emisUSDServer(t, lr, emisSAC(t, blndAssetID), prices, v1.Options{})
		got, _ := getEmissionsHistory(t, srv, mkCStrkey(t, 1), emisDayQuery, http.StatusOK)
		if pc := got.PoolClaims; !usdNull(pc.USD, "reads_disagree") || !usdNull(pc.Points[0].USD, "reads_disagree") {
			t.Errorf("%s: usd = %+v point %+v, want null reads_disagree", tc.name, pc.USD, pc.Points[0].USD)
		}
	}
}

// TestLendingEmissionsHistoryUSD_UnreadAmounts: a bucket none of whose claim amounts were read is null (amount_unread),
// never "0.00"; one missing some is valued over the read ones as a lower bound, and so is the stream.
func TestLendingEmissionsHistoryUSD_UnreadAmounts(t *testing.T) {
	none := poolClaim(t, 1, "0")
	none.AmountUnread = 1
	some := poolClaim(t, 2, "10000000")
	some.Claims, some.AmountUnread = 2, 1
	alloc := timescale.EmissionAllocBucket{Start: emisHour(2), Events: 1, BackstopShare: mustAmount(t, "0"), PoolShare: mustAmount(t, "0"), Unread: 1}
	lr := &usdEmisLending{stubBLNTLendingReader: v2Lending(), hourly: timescale.LendingEmissionsHistory{
		Claims: []timescale.EmissionClaimBucket{none, some}, Alloc: []timescale.EmissionAllocBucket{alloc},
	}}
	srv, _ := emisUSDServer(t, lr, emisSAC(t, blndAssetID), map[time.Time]string{emisHour(1): "1", emisHour(2): "1"}, v1.Options{})
	got, _ := getEmissionsHistory(t, srv, mkCStrkey(t, 1), emisHistQuery, http.StatusOK)
	pc := got.PoolClaims
	if !usdNull(pc.Points[0].USD, "amount_unread") || !usdIs(pc.Points[1].USD, "1.00", 1, 0, true) || !usdIs(pc.USD, "1.00", 1, 0, true) {
		t.Errorf("points %+v / %+v total %+v, want null amount_unread, then 1.00 and 1.00 lower bounds", pc.Points[0].USD, pc.Points[1].USD, pc.USD)
	}
	if a := got.BackstopAlloc.Points[0].USD; a.BackstopEmissionsUSD != nil || a.USDReason == nil || *a.USDReason != "amount_unread" {
		t.Errorf("alloc point usd = %+v, want null amount_unread", a)
	}
}

// TestLendingEmissionsHistoryUSD_ByReserveFromItsOwnHours: each single-reserve line is valued from its own reserve's
// hours, at each hour's price, not from the pool's.
func TestLendingEmissionsHistoryUSD_ByReserveFromItsOwnHours(t *testing.T) {
	single := func(id uint32, start time.Time, amt string) timescale.EmissionClaimBucket {
		return timescale.EmissionClaimBucket{Start: start, Scope: "single", TokenID: u32(id), Claims: 1, Claimers: 1, Amount: mustAmount(t, amt)}
	}
	lr := &usdEmisLending{
		stubBLNTLendingReader: v2Lending(),
		daily: timescale.LendingEmissionsHistory{Claims: []timescale.EmissionClaimBucket{
			{Start: emisT0, Scope: "pool", Claims: 3, Claimers: 2, Amount: mustAmount(t, "40000000")},
			single(1, emisT0, "30000000"), single(3, emisT0, "10000000"),
		}},
		hourly: timescale.LendingEmissionsHistory{Claims: []timescale.EmissionClaimBucket{
			poolClaim(t, 0, "10000000"), single(1, emisHour(0), "10000000"),
			poolClaim(t, 5, "30000000"), single(1, emisHour(5), "20000000"), single(3, emisHour(5), "10000000"),
		}},
	}
	srv, _ := emisUSDServer(t, lr, emisSAC(t, blndAssetID), map[time.Time]string{emisHour(0): "1", emisHour(5): "3"}, v1.Options{})
	got, _ := getEmissionsHistory(t, srv, mkCStrkey(t, 1), emisDayQuery, http.StatusOK)
	want := map[uint32]string{1: "7.00", 3: "3.00"}
	for _, l := range got.PoolClaims.ByReserve.Reserves {
		if l.USD.AmountUSD == nil || *l.USD.AmountUSD != want[l.TokenID] || *l.Points[0].USD.AmountUSD != want[l.TokenID] {
			t.Errorf("reserve %d usd = %+v, want %s", l.TokenID, l.USD, want[l.TokenID])
		}
	}
	if u := got.PoolClaims.USD; !usdIs(u, "10.00", 2, 0, false) {
		t.Errorf("pool usd = %+v, want 10.00", u)
	}
}

// TestLendingEmissionsHistoryUSD_TotalsTakeTheStreamLowerBound: a stream bounded by an exclusion (here an unverified
// projection) bounds its USD totals too, for the pool claims and the backstop allocation alike.
func TestLendingEmissionsHistoryUSD_TotalsTakeTheStreamLowerBound(t *testing.T) {
	lr := &usdEmisLending{stubBLNTLendingReader: v2Lending(), hourly: timescale.LendingEmissionsHistory{
		Claims: []timescale.EmissionClaimBucket{poolClaim(t, 1, "10000000")},
		Alloc:  []timescale.EmissionAllocBucket{{Start: emisHour(1), Events: 1, BackstopShare: mustAmount(t, "10000000"), PoolShare: mustAmount(t, "10000000")}},
	}}
	srv, _ := emisUSDServer(t, lr, emisSAC(t, blndAssetID), map[time.Time]string{emisHour(1): "1"}, v1.Options{CompletenessReader: verifiedHistory("other")})
	got, _ := getEmissionsHistory(t, srv, mkCStrkey(t, 1), emisHistQuery, http.StatusOK)
	if !got.PoolClaims.LowerBound || !usdIs(got.PoolClaims.USD, "1.00", 1, 0, true) {
		t.Errorf("pool claims lb %v usd %+v, want 1.00 a lower bound", got.PoolClaims.LowerBound, got.PoolClaims.USD)
	}
	if a := got.BackstopAlloc.USD; !got.BackstopAlloc.LowerBound || a == nil || !a.LowerBound || *a.PoolEmissionsUSD != "1.00" {
		t.Errorf("alloc lb %v usd %+v, want 1.00 a lower bound", got.BackstopAlloc.LowerBound, a)
	}
}

// TestLendingEmissionsHistoryUSD_HourlyReadFails: when only the hours of a 1d read fail, the days are still served in
// tokens; the rows' usd is null (hourly_read_failed), the schedule is still valued, and the response is not cacheable.
func TestLendingEmissionsHistoryUSD_HourlyReadFails(t *testing.T) {
	day := timescale.EmissionClaimBucket{Start: emisT0, Scope: "pool", Claims: 1, Claimers: 1, Amount: mustAmount(t, "10000000")}
	lr := &usdEmisLending{stubBLNTLendingReader: v2Lending(), failHours: true, daily: timescale.LendingEmissionsHistory{
		Claims: []timescale.EmissionClaimBucket{day},
		Schedule: []timescale.EmissionScheduleEvent{
			{TokenID: 1, At: emisHour(-1), EPS: mustAmount(t, "10000000000"), Expiration: mustAmount(t, "1793502000")}, // 03:00
		},
	}}
	srv, _ := emisUSDServer(t, lr, emisSAC(t, blndAssetID), map[time.Time]string{emisHour(0): "1", emisHour(1): "1", emisHour(2): "1"}, v1.Options{})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/pools/"+mkCStrkey(t, 1)+"/emissions/history"+emisDayQuery, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var env struct {
		Data v1.LendingEmissionsHistoryView
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	got := env.Data
	if got.PoolClaims.Totals.Amount != "10000000" || !usdNull(got.PoolClaims.USD, "hourly_read_failed") {
		t.Errorf("pool claims %+v usd %+v, want tokens served and usd null hourly_read_failed", got.PoolClaims.Totals, got.PoolClaims.USD)
	}
	if u := got.Reserves.Streams[0].USD; !usdIs(u, "1.08", 3, 0, false) {
		t.Errorf("schedule usd = %+v, want 1.08 valued without the hourly read", u)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store for a degraded response", cc)
	}
}

// BenchmarkEmissionsHistoryUSD_MaxWindow serves the widest 1d window with 12 and 40 schedule lines priced every hour.
func BenchmarkEmissionsHistoryUSD_MaxWindow(b *testing.B) {
	t := &testing.T{}
	to := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	from := to.Add(-1998 * 24 * time.Hour)
	prices := map[time.Time]string{}
	for h := from; h.Before(to); h = h.Add(time.Hour) {
		prices[h] = fmt.Sprintf("0.%07d", 4000+h.Hour())
	}
	q := "?interval=1d&from=" + from.Format(time.RFC3339) + "&to=" + to.Format(time.RFC3339)
	for _, n := range []int{12, 40} {
		var ev []timescale.EmissionScheduleEvent
		for i := range n {
			ev = append(ev, timescale.EmissionScheduleEvent{TokenID: uint32(i), At: from.Add(-time.Hour), EPS: mustAmount(t, "10000000000"), Expiration: mustAmount(t, "18446744073709551615")}) //nolint:gosec // i < 40
		}
		lr := &usdEmisLending{stubBLNTLendingReader: v2Lending(), daily: timescale.LendingEmissionsHistory{Schedule: ev}}
		srv, _ := emisUSDServer(t, lr, emisSAC(t, blndAssetID), prices, v1.Options{})
		b.Run(fmt.Sprintf("lines=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				rec := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/pools/"+mkCStrkey(t, 1)+"/emissions/history"+q, nil))
				if rec.Code != http.StatusOK {
					b.Fatalf("status %d", rec.Code)
				}
			}
		})
	}
}

// TestLendingEmissionsHistoryUSD_MaxRangeReads pins the read count at the widest 1d window with amounts at both ends: two
// history reads (daily, then hourly) and one price walk whose reads stay inside the data's span.
func TestLendingEmissionsHistoryUSD_MaxRangeReads(t *testing.T) {
	to := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	from := to.Add(-1998 * 24 * time.Hour)
	first, last := from.Add(time.Hour), to.Add(-time.Hour)
	daily := timescale.LendingEmissionsHistory{Claims: []timescale.EmissionClaimBucket{
		{Start: from, Scope: "pool", Claims: 1, Claimers: 1, Amount: mustAmount(t, "10000000")},
		{Start: last.Truncate(24 * time.Hour), Scope: "pool", Claims: 1, Claimers: 1, Amount: mustAmount(t, "10000000")},
	}}
	hourly := timescale.LendingEmissionsHistory{Claims: []timescale.EmissionClaimBucket{
		{Start: first, Scope: "pool", Claims: 1, Claimers: 1, Amount: mustAmount(t, "10000000")},
		{Start: last, Scope: "pool", Claims: 1, Claimers: 1, Amount: mustAmount(t, "10000000")},
	}}
	lr := &usdEmisLending{stubBLNTLendingReader: v2Lending(), daily: daily, hourly: hourly}
	srv, hist := emisUSDServer(t, lr, emisSAC(t, blndAssetID), map[time.Time]string{first: "1", last: "1"}, v1.Options{})
	got, _ := getEmissionsHistory(t, srv, mkCStrkey(t, 1), "?interval=1d&from="+from.Format(time.RFC3339)+"&to="+to.Format(time.RFC3339), http.StatusOK)
	if !usdIs(got.PoolClaims.USD, "2.00", 2, 0, false) {
		t.Errorf("usd = %+v", got.PoolClaims.USD)
	}
	if len(lr.qs) != 1 || !lr.qs[0].Hours {
		t.Errorf("history reads = %+v, want one, with its hours", lr.qs)
	}
	for _, r := range hist.reads {
		if r.from.Before(first.Add(-6*time.Hour)) || r.to.After(last.Add(time.Hour)) {
			t.Errorf("price read [%s, %s) outside the data's span", r.from, r.to)
		}
	}
	seen := map[string]bool{}
	for _, r := range hist.reads {
		if seen[r.pair] {
			t.Errorf("pair %s read twice: one walk per request reads each book once", r.pair)
		}
		seen[r.pair] = true
	}
	t.Logf("max 1d range: %d history reads, %d price reads", len(lr.qs), len(hist.reads))
}
