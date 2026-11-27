package v1_test

import (
	"context"
	"net/http"
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

// usdEmisLending serves daily (Daily reads) and hourly (every other read) emission history, recording each query.
type usdEmisLending struct {
	stubBLNTLendingReader
	daily, hourly timescale.LendingEmissionsHistory
	qs            []timescale.EmissionHistoryQuery
}

func (r *usdEmisLending) LendingEmissionsHistory(_ context.Context, _ string, _ bool, q timescale.EmissionHistoryQuery) (timescale.LendingEmissionsHistory, error) {
	r.qs = append(r.qs, q)
	if q.Daily {
		return r.daily, nil
	}
	return r.hourly, nil
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
	opts.CompletenessReader = verifiedHistory("blend", "blend_backstop", "blnt")
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

	if len(lr.qs) != 2 || !lr.qs[0].Daily || lr.qs[1].Daily || lr.qs[1].Schedule || !lr.qs[1].From.Equal(lr.qs[0].From) ||
		!lr.qs[1].To.Equal(lr.qs[0].To) || !lr.qs[1].BackstopTo.Equal(lr.qs[0].BackstopTo) {
		t.Fatalf("reads = %+v, want the daily read then an hourly one over the same bounds without the schedule", lr.qs)
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
		if len(hist.reads) != 0 || len(lr.qs) != 1 {
			t.Errorf("%s: %d price reads and %d history reads, want none and one", tc.name, len(hist.reads), len(lr.qs))
		}
	}
}

// TestLendingEmissionsHistoryUSD_BLNT values a BLNT pool at BLNT's price; a backstop tier's usd values blnt_amount only.
func TestLendingEmissionsHistoryUSD_BLNT(t *testing.T) {
	unknown := blend.PoolVersionUnknown
	lr := &usdEmisLending{
		stubBLNTLendingReader: stubBLNTLendingReader{stubLendingReader: stubLendingReader{version: &unknown}, blntAssets: []string{mkCStrkey(t, 3)}},
		hourly: timescale.LendingEmissionsHistory{BackstopClaims: []timescale.EmissionBackstopClaimBucket{
			{Start: emisHour(1), Tier: "FirstLoss", Claims: 1, Claimers: 1, BLNT: mustAmount(t, "50000000"), LP: mustAmount(t, "999999999999"), Shares: mustAmount(t, "7")},
		}},
	}
	srv, hist := emisUSDServer(t, lr, emisSAC(t, blntAssetID), map[time.Time]string{emisHour(1): "0.01"}, v1.Options{})
	got, _ := getEmissionsHistory(t, srv, blnt.MainnetFixedPool, emisHistQuery, http.StatusOK)
	tiers := got.BackstopClaims.Tiers
	if len(tiers) != 1 || !usdIs(tiers[0].USD, "0.05", 1, 0, false) || !usdIs(tiers[0].Points[0].USD, "0.05", 1, 0, false) {
		t.Errorf("tiers = %+v", tiers)
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
	get := func(excl v1.PriceExclusionGate) v1.LendingEmissionUSD {
		t.Helper()
		lr := &usdEmisLending{stubBLNTLendingReader: v2Lending(), hourly: timescale.LendingEmissionsHistory{
			Claims: []timescale.EmissionClaimBucket{poolClaim(t, 1, "10000000"), poolClaim(t, 10, "10000000")},
		}}
		srv, _ := emisUSDServer(t, lr, emisSAC(t, blndAssetID), map[time.Time]string{emisHour(1): "0.1", emisHour(10): "0.3"}, v1.Options{Exclusions: excl})
		got, _ := getEmissionsHistory(t, srv, mkCStrkey(t, 1), "?interval=1h&from=2026-11-01T00:00:00Z&to=2026-11-01T12:00:00Z", http.StatusOK)
		return got.PoolClaims.USD
	}
	if u := get(nil); !usdIs(u, "0.40", 2, 0, false) {
		t.Fatalf("control: usd = %+v, want 0.40 complete", u)
	}
	u := get(assetExclGate(exclTable{rows: []timescale.SourceExclusion{{
		ID: 1, Source: "soroswap", Base: blndAssetID, Quote: "fiat:USD", From: emisHour(10), Until: emisHour(11),
	}}}))
	if !usdIs(u, "0.10", 1, 1, true) {
		t.Errorf("excluded: usd = %+v, want 0.10 with the excluded hour unpriced, a lower bound", u)
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
	if len(lr.qs) != 2 {
		t.Errorf("history reads = %d, want 2", len(lr.qs))
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
