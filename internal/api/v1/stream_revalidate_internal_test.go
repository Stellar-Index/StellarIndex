package v1

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// revocableKeys is an API-key validator whose single key can be revoked
// mid-test, the way a dashboard revocation lands in the key store.
type revocableKeys struct {
	revoked atomic.Bool
	// down, when set, is the outage every lookup fails with.
	down atomic.Pointer[error]
}

func (k *revocableKeys) Lookup(_ context.Context, key string) (auth.Subject, error) {
	if down := k.down.Load(); down != nil {
		return auth.Subject{}, *down
	}
	if key != "stream-test-key" || k.revoked.Load() {
		return auth.Subject{}, auth.ErrUnauthorized
	}
	return auth.Subject{Identifier: "acct-1", Tier: auth.TierAPIKey, MonthlyQuota: 1000}, nil
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

// TestStreamRevalidate_RevokedKeyAndExhaustedQuotaFailTheCheck pins containment: every stream endpoint gets a
// Revalidate hook from streamOptions replaying the real Auth and MonthlyQuota gates, so a key revoked or quota
// exhausted after admission fails the next heartbeat check.
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

// downCounter is a month-to-date reader whose store can go down.
type downCounter struct {
	down *atomic.Bool
	used *atomic.Int64
}

func (c downCounter) MonthToDate(context.Context, string) (int64, error) {
	if c.down.Load() {
		return 0, errors.New("redis: connection refused")
	}
	return c.used.Load(), nil
}

// An outage is not a revocation. While the account-status store is down the Auth gate answers 503, and while the
// usage counter is down the real MonthlyQuota gate answers a 429 it marks as unavailable. Ending the stream on either
// writes ":revoked", after which the client does not reconnect; the check must pass and be made again next heartbeat.
func TestStreamRevalidate_AnOutageDoesNotEndTheStream(t *testing.T) {
	keys := &revocableKeys{}
	var counterDown atomic.Bool
	var used atomic.Int64
	clock := time.Unix(1_700_000_000, 0)
	s := New(Options{
		Auth: middleware.Auth(middleware.AuthOptions{Mode: middleware.AuthModeAPIKey, APIKey: keys}),
		MonthlyQuota: middleware.MonthlyQuota(downCounter{&counterDown, &used}, nil,
			middleware.WithMonthlyQuotaDwellTime(time.Minute),
			middleware.WithMonthlyQuotaClock(func() time.Time { clock = clock.Add(2 * time.Minute); return clock })),
	})
	revalidate := s.streamOptions().Revalidate

	for name, outage := range map[string]error{
		"account-status store": auth.ErrAccountStatusUnavailable,
		"key store":            fmt.Errorf("auth: apikey redis get: %w: %w", auth.ErrKeyStoreUnavailable, errors.New("dial tcp: connection refused")),
	} {
		keys.down.Store(&outage)
		if !revalidate(streamRequest()) {
			t.Errorf("Revalidate = false while the %s is down: an unread credential is not a revoked key", name)
		}
	}
	keys.down.Store(nil)

	counterDown.Store(true)
	revalidate(streamRequest()) // the first failed read starts the dwell clock and fails open
	if !revalidate(streamRequest()) {
		t.Error("Revalidate = false while the usage counter is down past its dwell window: quota-unavailable is not quota-exhausted")
	}
	counterDown.Store(false)

	// The real gate's exhausted answer is still a refusal: the outage mark must never reach it.
	used.Store(1000)
	if revalidate(streamRequest()) {
		t.Error("Revalidate = true with the monthly quota used up")
	}
	used.Store(0)

	keys.revoked.Store(true)
	if revalidate(streamRequest()) {
		t.Error("Revalidate = true for a revoked key once the stores are back")
	}
}

// TestStreamRevalidate_GateNamesExistInStack: the revalidator selects gates by middlewareStack entry name, so a
// renamed entry would silently drop out of the heartbeat check. Every selected name must exist in the full stack.
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

// streamResumeHandlers maps each spec path that may declare a
// Last-Event-ID parameter to the handler source that must read it.
var streamResumeHandlers = map[string]string{
	"/price/stream":     "price_stream.go",
	"/price/tip/stream": "price_tip_stream.go",
}
