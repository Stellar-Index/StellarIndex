// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package dashboardauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// signInByLink sends a magic link to email and opens it in the browser that asked for it.
func (r *testRig) signInByLink(t *testing.T, email string) *httptest.ResponseRecorder {
	t.Helper()
	lw := r.postLogin(t, email)
	cb := httptest.NewRequest(http.MethodGet, "/v1/auth/callback?token="+url.QueryEscape(r.extractTokenFromSentEmail(t)), nil)
	cb.RemoteAddr = "203.0.113.5:55123"
	attachCookies(cb, lw)
	w := httptest.NewRecorder()
	r.h.HandleCallback(w, cb)
	return w
}

func (r *testRig) signInByCode(t *testing.T, email string) *httptest.ResponseRecorder {
	t.Helper()
	return r.postVerifyCode(t, email, r.loginAndCode(t, email))
}

// An email's first sign-in creates an account, so the registration switch and budget refuse it with a 503 and create
// nothing; an existing user's sign-in creates nothing and is untouched by both.
func TestEmailSignIn_FirstSignInObeysRegistrationLevers(t *testing.T) {
	routes := []struct {
		name   string
		signIn func(*testRig, *testing.T, string) *httptest.ResponseRecorder
		ok     int
	}{
		{"magic link", (*testRig).signInByLink, http.StatusSeeOther},
		{"verify code", (*testRig).signInByCode, http.StatusOK},
	}
	levers := []struct {
		name string
		pull func(*testRig) *fakeSignupBudget
	}{
		{"switched off", func(r *testRig) *fakeSignupBudget {
			r.h.cfg.SignupDisabled = true
			return nil
		}},
		{"budget spent", func(r *testRig) *fakeSignupBudget {
			b := &fakeSignupBudget{err: auth.ErrSignupRateLimited}
			r.h.cfg.SignupBudget = auth.NewRegistrationBudget(b)
			return b
		}},
	}
	for _, route := range routes {
		for _, lever := range levers {
			t.Run(route.name+"/"+lever.name+"/new email refused", func(t *testing.T) {
				r := newTestRig(t)
				budget := lever.pull(r)
				w := route.signIn(r, t, "newcomer@example.com")
				if w.Code != http.StatusServiceUnavailable || sessionCookieSet(w) {
					t.Fatalf("status = %d, session = %v; want 503 and none", w.Code, sessionCookieSet(w))
				}
				if _, err := r.users.GetUserByEmail(t.Context(), "newcomer@example.com"); err == nil || r.accountCount() != 0 {
					t.Fatalf("a refused first sign-in left a user (%v) or %d account(s)", err, r.accountCount())
				}
				if budget != nil && budget.calls != 1 {
					t.Errorf("budget spends = %d, want 1", budget.calls)
				}
			})
			t.Run(route.name+"/"+lever.name+"/existing user signs in", func(t *testing.T) {
				r := newTestRig(t)
				acct, _ := r.accounts.Create(t.Context(), platform.Account{
					Name: "Acme", Slug: "acme", BillingEmail: "ash@acme.com", Tier: platform.TierFree, Status: platform.AccountActive,
				})
				if _, err := r.users.CreateUser(t.Context(), platform.User{AccountID: acct.ID, Email: "ash@acme.com", Role: platform.RoleOwner}); err != nil {
					t.Fatalf("seed user: %v", err)
				}
				budget := lever.pull(r)
				w := route.signIn(r, t, "ash@acme.com")
				if w.Code != route.ok || !sessionCookieSet(w) || r.accountCount() != 1 {
					t.Fatalf("status = %d, session = %v, accounts = %d; want %d, a session and still 1", w.Code, sessionCookieSet(w), r.accountCount(), route.ok)
				}
				if budget != nil && budget.calls != 0 {
					t.Errorf("an existing user's sign-in spent %d budget unit(s)", budget.calls)
				}
			})
		}
	}
}

// Invariant: one client IP creates at most passkeySignupMaxPerIP accounts per window through email-proven first sign-ins, and
// a refused one spends no unit of the all-caller email-proven budget, so a catch-all domain on one address cannot drain it.
// Existing users on the same address still sign in.
func TestEmailSignIn_FirstSignInPerIPCapSpendsNoBudget(t *testing.T) {
	r := newTestRig(t)
	budget := &fakeSignupBudget{}
	r.h.cfg.SignupBudget = auth.NewRegistrationBudget(budget)
	acct, _ := r.accounts.Create(t.Context(), platform.Account{
		Name: "Acme", Slug: "acme", BillingEmail: "ash@acme.com", Tier: platform.TierFree, Status: platform.AccountActive,
	})
	if _, err := r.users.CreateUser(t.Context(), platform.User{AccountID: acct.ID, Email: "ash@acme.com", Role: platform.RoleOwner}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	for i := range passkeySignupMaxPerIP {
		if w := r.signInByCode(t, "a"+strconv.Itoa(i)+"@catch-all.example"); w.Code != http.StatusOK {
			t.Fatalf("sign-up %d inside the cap: status = %d, want 200", i, w.Code)
		}
	}
	accounts := r.accountCount()
	w := r.signInByCode(t, "over-cap@catch-all.example")
	if w.Code != http.StatusTooManyRequests || sessionCookieSet(w) || r.accountCount() != accounts {
		t.Fatalf("sign-up past the per-IP cap: status = %d, session = %v, accounts +%d; want 429, none, none",
			w.Code, sessionCookieSet(w), r.accountCount()-accounts)
	}
	if !strings.Contains(w.Body.String(), "has been used; request a new one") {
		t.Errorf("429 detail does not say the spent link or code must be re-requested: %s", w.Body.String())
	}
	if budget.calls != passkeySignupMaxPerIP {
		t.Errorf("budget spends = %d, want %d (the refused sign-up spends none)", budget.calls, passkeySignupMaxPerIP)
	}
	if w := r.signInByCode(t, "ash@acme.com"); w.Code != http.StatusOK || !sessionCookieSet(w) {
		t.Errorf("existing user's sign-in from the capped address: status = %d, want 200 and a session", w.Code)
	}
}

// Invariant: each sign-up route has its own per-IP cap, so free passkey begin-signups from an address cannot refuse an
// email-proven first sign-in from it.
func TestEmailSignIn_PasskeyBeginsDoNotExhaustEmailPerIPCap(t *testing.T) {
	rig := newPasskeyRig(t)
	for i := range passkeySignupMaxPerIP + 1 {
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/passkey/begin-signup", nil)
		req.RemoteAddr = "203.0.113.9:1"
		w := httptest.NewRecorder()
		rig.h.HandlePasskeyBeginSignup(w, req)
		want := http.StatusOK
		if i == passkeySignupMaxPerIP {
			want = http.StatusTooManyRequests
		}
		if w.Code != want {
			t.Fatalf("begin %d: status = %d, want %d", i+1, w.Code, want)
		}
	}
	if w := rig.signInByCode(t, "first@example.com"); w.Code != http.StatusOK || !sessionCookieSet(w) {
		t.Fatalf("email first sign-in after passkey begins exhausted their cap: status = %d, want 200 and a session", w.Code)
	}
}

// registerKeyStub mints the key /v1/register issues; register calls nothing else on it.
type registerKeyStub struct{ platform.APIKeyStore }

func (registerKeyStub) Create(_ context.Context, k platform.APIKey, _ int) (platform.APIKey, error) {
	return k, nil
}

// Invariant: one unit of the hourly budget is one account created, on every account-creating route, and each created
// account counts as registrations_total{created}. Proof-free routes (/v1/register, passkey sign-up) share one counter; an
// email-proven first sign-in has its own, so draining the proof-free routes cannot refuse it. Real Redis-backed budget.
func TestRegistrationBudget_ProofFreeRoutesCannotDrainEmailSignUp(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	budget := auth.NewRegistrationBudget(auth.NewRedisSignupIPThrottle(rdb, auth.SignupIPThrottleOptions{Max: 2}))

	rig, _, _ := newLiveClockPasskeyRig(t)
	rig.h.cfg.SignupBudget = budget
	var anonymous middleware.Middleware = func(next http.Handler) http.Handler { return next }
	ts := httptest.NewServer(v1.New(v1.Options{
		Auth:             anonymous,
		RegisterAccounts: rig.accounts,
		APIKeyBudgets:    v1.APIKeyBudgetStores{Platform: registerKeyStub{}},
		RegisterBudget:   budget,
	}).Handler())
	t.Cleanup(ts.Close)
	register := func() int {
		resp, err := http.Post(ts.URL+"/v1/register", "application/json", nil) //nolint:noctx // test server
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	created := func() float64 {
		return testutil.ToFloat64(obs.RegistrationsTotal.WithLabelValues(obs.RegistrationCreated, obs.RegistrationPoolProofFree)) +
			testutil.ToFloat64(obs.RegistrationsTotal.WithLabelValues(obs.RegistrationCreated, obs.RegistrationPoolEmailProven))
	}
	exhausted := func(pool string) float64 {
		return testutil.ToFloat64(obs.RegistrationsTotal.WithLabelValues(obs.RegistrationBudgetExhausted, pool))
	}
	proofFreeExhaustedBefore, emailExhaustedBefore := exhausted(obs.RegistrationPoolProofFree), exhausted(obs.RegistrationPoolEmailProven)

	before, createdBefore := rig.accountCount(), created()
	if c := register(); c != http.StatusOK {
		t.Fatalf("register: status = %d, want 200", c)
	}
	if w := passkeySignupOnce(t, rig, "EXAMPLE-SIGNUP-CRED-PLACEHOLDER1"); w.Code != http.StatusOK {
		t.Fatalf("passkey sign-up: status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if c := register(); c != http.StatusServiceUnavailable {
		t.Fatalf("register past the proof-free budget: status = %d, want 503", c)
	}
	if w := passkeySignupOnce(t, rig, "EXAMPLE-SIGNUP-CRED-PLACEHOLDER2"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("passkey sign-up past the proof-free budget: status = %d, want 503", w.Code)
	}

	if w := rig.signInByCode(t, "second@example.com"); w.Code != http.StatusOK {
		t.Fatalf("first code sign-in with the proof-free budget spent: status = %d, want 200", w.Code)
	}
	if w := rig.signInByLink(t, "third@example.com"); w.Code != http.StatusSeeOther {
		t.Fatalf("first link sign-in with the proof-free budget spent: status = %d, want 303", w.Code)
	}
	if w := rig.signInByCode(t, "fourth@example.com"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("first code sign-in past the email budget: status = %d, want 503", w.Code)
	}
	// Each refusal names the counter that filled, so the alert shows which sign-up path is turned away.
	if got := exhausted(obs.RegistrationPoolProofFree) - proofFreeExhaustedBefore; got != 2 {
		t.Errorf("budget_exhausted{pool=proof_free} grew by %v, want 2 (register + passkey)", got)
	}
	if got := exhausted(obs.RegistrationPoolEmailProven) - emailExhaustedBefore; got != 1 {
		t.Errorf("budget_exhausted{pool=email_proven} grew by %v, want 1", got)
	}
	if got := rig.accountCount() - before; got != 4 {
		t.Errorf("%d account(s) created, want 4 (2 proof-free + 2 email-proven)", got)
	}
	// /v1/register counts its own creation; the dashboard routes must count theirs.
	if got := created() - createdBefore; got != 4 {
		t.Errorf("registrations_total{created} grew by %v, want 4", got)
	}
	if w := rig.signInByCode(t, rig.user.Email); w.Code != http.StatusOK {
		t.Errorf("existing user's sign-in with both budgets spent: status = %d, want 200", w.Code)
	}
}
