// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package dashboardauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
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

// registerKeyStub mints the key /v1/register issues; register calls nothing else on it.
type registerKeyStub struct{ platform.APIKeyStore }

func (registerKeyStub) Create(_ context.Context, k platform.APIKey, _ int) (platform.APIKey, error) {
	return k, nil
}

// Invariant: one unit of the hourly budget is one account, on every account-creating route, all drawing from ONE
// counter. Run against the real Redis-backed budget: three creations across three routes spend it, and then every
// route refuses a fourth while an existing user still signs in.
func TestRegistrationBudget_OneCounterAcrossEveryRoute(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	budget := auth.NewRegistrationBudget(auth.NewRedisSignupIPThrottle(rdb, auth.SignupIPThrottleOptions{Max: 3}))

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

	before := rig.accountCount()
	if c := register(); c != http.StatusOK {
		t.Fatalf("register 1: status = %d, want 200", c)
	}
	if w := passkeySignupOnce(t, rig, "EXAMPLE-SIGNUP-CRED-PLACEHOLDER1"); w.Code != http.StatusOK {
		t.Fatalf("passkey sign-up 2: status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if w := rig.signInByCode(t, "second@example.com"); w.Code != http.StatusOK {
		t.Fatalf("first code sign-in 3: status = %d, want 200", w.Code)
	}
	if got := rig.accountCount() - before; got != 3 {
		t.Fatalf("%d account(s) created, want 3", got)
	}

	if c := register(); c != http.StatusServiceUnavailable {
		t.Errorf("register past the budget: status = %d, want 503", c)
	}
	if w := passkeySignupOnce(t, rig, "EXAMPLE-SIGNUP-CRED-PLACEHOLDER2"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("passkey sign-up past the budget: status = %d, want 503", w.Code)
	}
	if w := rig.signInByCode(t, "third@example.com"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("first code sign-in past the budget: status = %d, want 503", w.Code)
	}
	if w := rig.signInByLink(t, "fourth@example.com"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("first link sign-in past the budget: status = %d, want 503", w.Code)
	}
	if got := rig.accountCount() - before; got != 3 {
		t.Errorf("%d account(s) created against a budget of 3", got)
	}
	if w := rig.signInByCode(t, rig.user.Email); w.Code != http.StatusOK {
		t.Errorf("existing user's sign-in with the budget spent: status = %d, want 200", w.Code)
	}
}
