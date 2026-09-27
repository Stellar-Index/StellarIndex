package v1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// revocableKeys is an API-key validator whose single key can be revoked
// mid-test, the way a dashboard revocation lands in the key store.
type revocableKeys struct{ revoked atomic.Bool }

func (k *revocableKeys) Lookup(_ context.Context, key string) (auth.Subject, error) {
	if key != "stream-test-key" || k.revoked.Load() {
		return auth.Subject{}, auth.ErrUnauthorized
	}
	return auth.Subject{Identifier: "acct-1", Tier: auth.TierAPIKey}, nil
}

// flagGate answers 429 once its flag is set and counts every pass.
func flagGate(deny *atomic.Bool, passes *atomic.Int32) middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			passes.Add(1)
			if deny != nil && deny.Load() {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func streamRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/ledger/stream", nil)
	r.Header.Set("Authorization", "Bearer stream-test-key")
	return r
}

// TestStreamRevalidate_RevokedKeyAndExhaustedQuotaFailTheCheck pins the
// containment property: every stream endpoint gets a Revalidate hook
// from streamOptions, and it replays the real Auth and MonthlyQuota
// gates, so a key revoked or a quota exhausted after the stream was
// admitted fails the next heartbeat's check.
func TestStreamRevalidate_RevokedKeyAndExhaustedQuotaFailTheCheck(t *testing.T) {
	keys := &revocableKeys{}
	var quotaExhausted atomic.Bool
	var quotaPasses, rateLimitPasses atomic.Int32
	s := New(Options{
		Auth:         middleware.Auth(middleware.AuthOptions{Mode: middleware.AuthModeAPIKey, APIKey: keys}),
		MonthlyQuota: flagGate(&quotaExhausted, &quotaPasses),
		RateLimit:    flagGate(nil, &rateLimitPasses),
	})
	revalidate := s.streamOptions().Revalidate
	if revalidate == nil {
		t.Fatal("streamOptions().Revalidate is nil with Auth configured — open streams are never re-checked")
	}

	if !revalidate(streamRequest()) {
		t.Fatal("Revalidate = false for a valid key within quota")
	}
	if quotaPasses.Load() != 1 {
		t.Errorf("MonthlyQuota ran %d times, want 1 — the quota gate is not re-checked", quotaPasses.Load())
	}
	if rateLimitPasses.Load() != 0 {
		t.Errorf("RateLimit ran %d times during revalidation, want 0 — a heartbeat must not spend a token", rateLimitPasses.Load())
	}

	quotaExhausted.Store(true)
	if revalidate(streamRequest()) {
		t.Error("Revalidate = true after the monthly quota was exhausted")
	}
	quotaExhausted.Store(false)

	keys.revoked.Store(true)
	if revalidate(streamRequest()) {
		t.Error("Revalidate = true after the API key was revoked")
	}
}

// TestStreamRevalidate_GateNamesExistInStack — the revalidator selects
// gates by middlewareStack entry name, so a renamed entry would silently
// drop out of the per-heartbeat check. Every selected name must be one
// the fully-configured stack actually emits.
func TestStreamRevalidate_GateNamesExistInStack(t *testing.T) {
	pass := flagGate(nil, new(atomic.Int32))
	s := New(Options{Auth: pass, KeyPolicy: pass, RequireEmailVerified: pass, MonthlyQuota: pass})
	inStack := map[string]bool{}
	for _, e := range s.middlewareStack() {
		inStack[e.name] = true
	}
	for name := range streamRevalidationGates {
		if !inStack[name] {
			t.Errorf("streamRevalidationGates names %q, which middlewareStack never emits", name)
		}
	}
}

// TestStreamRevalidate_NilWithoutGates — an auth-less deployment has
// nothing to re-check, so no per-heartbeat work is scheduled.
func TestStreamRevalidate_NilWithoutGates(t *testing.T) {
	if New(Options{}).streamOptions().Revalidate != nil {
		t.Error("Revalidate wired with no gate middleware configured")
	}
}
