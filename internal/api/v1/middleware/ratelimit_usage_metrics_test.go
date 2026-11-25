package middleware_test

import (
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// decisionDeltas runs fn and returns every (tier/result) decision series that moved, with its increase.
func decisionDeltas(fn func()) map[string]float64 {
	before := map[string]float64{}
	for _, tier := range obs.RateLimitTiers {
		for _, result := range obs.RateLimitResults {
			before[tier+"/"+result] = testutil.ToFloat64(obs.RateLimitDecisionsTotal.WithLabelValues(tier, result))
		}
	}
	fn()
	moved := map[string]float64{}
	for _, tier := range obs.RateLimitTiers {
		for _, result := range obs.RateLimitResults {
			if d := testutil.ToFloat64(obs.RateLimitDecisionsTotal.WithLabelValues(tier, result)) - before[tier+"/"+result]; d != 0 {
				moved[tier+"/"+result] = d
			}
		}
	}
	return moved
}

func utilisation(t *testing.T, tier string) (uint64, float64) {
	t.Helper()
	var pb dto.Metric
	if err := obs.RateLimitUtilisationRatio.WithLabelValues(tier).(prometheus.Metric).Write(&pb); err != nil {
		t.Fatalf("write histogram: %v", err)
	}
	return pb.GetHistogram().GetSampleCount(), pb.GetHistogram().GetSampleSum()
}

func assertOneDecision(t *testing.T, name string, moved map[string]float64, want string) {
	t.Helper()
	if len(moved) != 1 || moved[want] != 1 {
		t.Errorf("%s: decision series moved %v, want exactly %s +1", name, moved, want)
	}
}

// TestRateLimitDecisions_OneSeriesPerRequest: every outcome path counts the request exactly once, under its tier and result,
// including a refusal that comes from a re-price after the request was admitted.
func TestRateLimitDecisions_OneSeriesPerRequest(t *testing.T) {
	rdb, _ := newRLRedis(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	var worked int

	small := middleware.RateLimitBySubject(ratelimit.New(rdb, 1, time.Minute, frozenClock, ratelimit.WithKeyPrefix("dec-a:")), nil, nil, nil)(ok)
	assertOneDecision(t, "admitted", decisionDeltas(func() { serve(small, http.MethodGet, "/") }), "anonymous/allowed")
	assertOneDecision(t, "refused at admission", decisionDeltas(func() { serve(small, http.MethodGet, "/") }), "anonymous/limited")

	// Cost 6 of 10: the first request spends 6; the second is admitted (7) and refused by its own re-price (12).
	reprice := middleware.RateLimitBySubject(ratelimit.New(rdb, 10, time.Minute, frozenClock, ratelimit.WithKeyPrefix("dec-b:")), nil, nil, nil)(chargingHandler(6, &worked))
	serve(reprice, http.MethodGet, "/")
	assertOneDecision(t, "refused at re-price", decisionDeltas(func() { serve(reprice, http.MethodGet, "/") }), "anonymous/limited")

	clock := newManualClock()
	down := middleware.RateLimitBySubject(ratelimit.New(newDownRedis(t), 100, time.Minute, ratelimit.WithClock(clock.now)), nil, nil, nil)(ok)
	assertOneDecision(t, "failed open", decisionDeltas(func() { serve(down, http.MethodGet, "/") }), "anonymous/fail_open")
	clock.advance(ratelimit.DefaultDwellTime + time.Second)
	var code int
	assertOneDecision(t, "failed closed", decisionDeltas(func() { code = serve(down, http.MethodGet, "/").Code }), "anonymous/unavailable")
	if code != http.StatusServiceUnavailable {
		t.Errorf("failed-closed status = %d, want 503", code)
	}
}

// TestRateLimitDecisions_TierIsBoundedAndNeverAnIdentity: the tier label is the subject's auth tier (custom for a per-key ceiling),
// and no series created by the limiter carries a client identity: label names are fixed and every value is from the closed sets.
func TestRateLimitDecisions_TierIsBoundedAndNeverAnIdentity(t *testing.T) {
	rdb, _ := newRLRedis(t)
	anon := ratelimit.New(rdb, 100, time.Minute, frozenClock, ratelimit.WithKeyPrefix("tier-anon:"))
	authB := ratelimit.New(rdb, 100, time.Minute, frozenClock, ratelimit.WithKeyPrefix("tier-auth:"))
	h := middleware.RateLimitBySubject(anon, authB, nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))

	const account = "GSECRETACCOUNTIDENTITY"
	cases := []struct {
		name    string
		subject *auth.Subject
		want    string
	}{
		{"no subject", nil, "anonymous"},
		{"api key", &auth.Subject{Identifier: account, Tier: auth.TierAPIKey, KeyID: "kid-1"}, "apikey"},
		{"per-key ceiling", &auth.Subject{Identifier: account, Tier: auth.TierAPIKey, KeyID: "kid-2", RateLimitPerMin: 50}, obs.RateLimitTierCustom},
		{"operator", &auth.Subject{Identifier: account, Tier: auth.TierOperator}, "operator"},
		{"sep10", ptr(auth.NewSEP10Session(account, time.Unix(1_700_000_000, 0))), "sep10"},
		{"unknown tier", &auth.Subject{Identifier: account, Tier: auth.Tier("gold")}, obs.RateLimitTierOther},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "203.0.113.9:4000"
		r.Header.Set("User-Agent", "probe-agent")
		if tc.subject != nil {
			r = r.WithContext(auth.WithSubject(r.Context(), *tc.subject))
		}
		assertOneDecision(t, tc.name, decisionDeltas(func() { h.ServeHTTP(httptest.NewRecorder(), r) }), tc.want+"/allowed")
	}

	for _, c := range []prometheus.Collector{obs.RateLimitDecisionsTotal, obs.RateLimitUtilisationRatio} {
		ch := make(chan prometheus.Metric, 256)
		go func() { c.Collect(ch); close(ch) }()
		for m := range ch {
			var pb dto.Metric
			if err := m.Write(&pb); err != nil {
				t.Fatal(err)
			}
			for _, lp := range pb.GetLabel() {
				switch lp.GetName() {
				case "tier":
					if !slices.Contains(obs.RateLimitTiers, lp.GetValue()) {
						t.Errorf("tier label %q is outside the closed set", lp.GetValue())
					}
				case "result":
					if !slices.Contains(obs.RateLimitResults, lp.GetValue()) {
						t.Errorf("result label %q is outside the closed set", lp.GetValue())
					}
				default:
					t.Errorf("unexpected label %q=%q: usage metrics carry tier and result only", lp.GetName(), lp.GetValue())
				}
			}
		}
	}
}

func ptr[T any](v T) *T { return &v }

// TestRateLimitUtilisation_ObservesBudgetSpentAfterEveryCharge: one observation per allowed request, equal to the window count
// after all its charges (base, re-price, cold fill) over the limit; a refused request is counted, not observed.
func TestRateLimitUtilisation_ObservesBudgetSpentAfterEveryCharge(t *testing.T) {
	rdb, _ := newRLRedis(t)
	b := ratelimit.New(rdb, 20, time.Minute, frozenClock, ratelimit.WithKeyPrefix("util:"))
	h := middleware.RateLimitBySubject(b, nil, nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cost, _ := strconv.Atoi(r.URL.Query().Get("cost")); cost > 0 && !middleware.ChargeRateLimit(w, r, cost) {
			return
		}
		if r.URL.Query().Has("cold") {
			if err := ratelimit.ChargeColdFill(r.Context()); err != nil {
				middleware.WriteRateLimitRefusal(w, r, err)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	}))

	beforeN, beforeSum := utilisation(t, "anonymous")
	steps := []struct {
		target string
		code   int
	}{
		{"/", http.StatusOK},                   // 1 token: 1/20
		{"/?cold", http.StatusOK},              // 1 + ColdFillCost: 7/20
		{"/?cost=10", http.StatusOK},           // re-priced to 10 in total: 17/20
		{"/?cold", http.StatusTooManyRequests}, // 18, then 23 > 20: refused
	}
	moved := decisionDeltas(func() {
		for i, s := range steps {
			if got := serve(h, http.MethodGet, s.target).Code; got != s.code {
				t.Fatalf("step %d (%s): status %d, want %d", i+1, s.target, got, s.code)
			}
		}
	})
	if moved["anonymous/allowed"] != 3 || moved["anonymous/limited"] != 1 || len(moved) != 2 {
		t.Errorf("decisions moved %v, want anonymous/allowed +3 and anonymous/limited +1", moved)
	}
	n, sum := utilisation(t, "anonymous")
	if n-beforeN != 3 {
		t.Errorf("observations = %d, want 3 (one per allowed request)", n-beforeN)
	}
	if want := (1.0 + 7 + 17) / 20; math.Abs(sum-beforeSum-want) > 1e-9 {
		t.Errorf("observed sum = %v, want %v (1/20 + 7/20 + 17/20)", sum-beforeSum, want)
	}
}

// TestRateLimitUsage_SEP10PerIPChargeIsRecorded: a SEP-10 request is recorded by its per-IP charge as well as its account's,
// so a per-IP refusal is counted and an allowed request observes the larger of the two budget shares.
func TestRateLimitUsage_SEP10PerIPChargeIsRecorded(t *testing.T) {
	rdb, _ := newRLRedis(t)
	authB := ratelimit.New(rdb, 10, time.Minute, frozenClock, ratelimit.WithKeyPrefix("sep10-usage:"))
	h := middleware.RateLimitBySubject(nil, authB, nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	sep10 := func(remote, acct string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		return r.WithContext(auth.WithSubject(r.Context(), auth.NewSEP10Session(acct, time.Time{})))
	}

	// Allowed: the IP has spent 2 of 10 across two accounts, the second account only 1, so the per-IP share binds.
	h.ServeHTTP(httptest.NewRecorder(), sep10("198.51.100.20:1", "GACCOUNTONE"))
	beforeN, beforeSum := utilisation(t, "sep10")
	assertOneDecision(t, "per-IP binds", decisionDeltas(func() {
		h.ServeHTTP(httptest.NewRecorder(), sep10("198.51.100.20:1", "GACCOUNTTWO"))
	}), "sep10/allowed")
	if n, sum := utilisation(t, "sep10"); n-beforeN != 1 || math.Abs(sum-beforeSum-0.2) > 1e-9 {
		t.Errorf("observed %d sample(s) summing %v, want 1 at 0.2 (per-IP 2/10, not the account's 1/10)", n-beforeN, sum-beforeSum)
	}

	// Refused at the per-IP charge, before the account is billed: counted as limited, not observed.
	for i := range 10 {
		h.ServeHTTP(httptest.NewRecorder(), sep10("198.51.100.21:1", "GFILLER"+strconv.Itoa(i)))
	}
	beforeN, _ = utilisation(t, "sep10")
	var code int
	assertOneDecision(t, "per-IP refusal", decisionDeltas(func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, sep10("198.51.100.21:1", "GFRESHACCOUNT"))
		code = w.Code
	}), "sep10/limited")
	if code != http.StatusTooManyRequests {
		t.Errorf("per-IP refusal status = %d, want 429", code)
	}
	if n, _ := utilisation(t, "sep10"); n != beforeN {
		t.Errorf("a refused request was observed (%d new samples)", n-beforeN)
	}
}
