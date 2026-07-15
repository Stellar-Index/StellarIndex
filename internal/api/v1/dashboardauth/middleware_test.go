package dashboardauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// TestMiddleware_NilNowDoesNotPanic is a regression for the live
// production bug where main.go built the auth Config without a Now
// func and passed it raw to Middleware. NewHandlers defaulted Now on
// its own copy, but the resolver Middleware kept the nil — so
// resolveSession's cfg.Now() nil-derefed on every authenticated
// request. The magic-link cookie resolved fine; /v1/account/me then
// 500'd, making login look broken. Middleware must default Now (and
// Logger) so a valid session resolves without panicking.
func TestMiddleware_NilNowDoesNotPanic(t *testing.T) {
	accounts := newFakeAccountStore()
	users := newFakeUserStore()

	acct, err := accounts.Create(context.Background(), platform.Account{
		Name:   "tester",
		Slug:   "tester",
		Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	user, err := users.CreateUser(context.Background(), platform.User{
		AccountID: acct.ID,
		Email:     "tester@example.com",
		Role:      platform.RoleOwner,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	sess, err := users.CreateSession(context.Background(), platform.Session{
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Config with Now AND Logger left nil — the exact shape that
	// panicked in production.
	cfg := &Config{
		Accounts: accounts,
		Users:    users,
		Tokens:   newFakeTokenStore(nil),
	}

	var resolved bool
	h := Middleware(cfg)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, resolved = SessionFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/account/me", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: sess.ID.String()})
	rec := httptest.NewRecorder()

	// The bug manifested as a panic recovered upstream into a 500;
	// here an unrecovered panic fails the test directly.
	h.ServeHTTP(rec, req)

	if !resolved {
		t.Fatal("expected the valid session cookie to resolve, got anonymous")
	}
	if cfg.Now == nil {
		t.Fatal("Middleware should have defaulted a nil cfg.Now")
	}
}
