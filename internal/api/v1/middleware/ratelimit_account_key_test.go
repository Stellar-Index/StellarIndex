package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// GH-1288 / GH-1147: the per-minute limit keyed on KeyID, so every key an
// account held got its own full bucket — a 25-key account ran at 25x its
// ceiling, and an operator's 100k/min comp became 2.5M/min. The monthly
// quota already counted per account; the rate limit must share that
// identity, so minting or rotating keys cannot multiply the ceiling.
func TestRateLimitBySubject_KeysOnOneAccountShareOneBucket(t *testing.T) {
	rdb, _ := newRLRedis(t)
	authBucket := ratelimit.New(rdb, 100, time.Minute)
	h := middleware.RateLimitBySubject(nil, authBucket, nil, nil)(okHandler())

	const perMin = 3
	serve := func(keyID string) int {
		r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
		sub := auth.Subject{
			Identifier:      auth.AccountIdentifier("comped-co"),
			Tier:            auth.TierAPIKey,
			KeyID:           keyID,
			RateLimitPerMin: perMin,
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), sub)))
		return w.Code
	}

	keys := []string{"kid_a", "kid_b", "kid_c", "kid_d", "kid_e"}
	allowed := 0
	for round := 0; round < 2; round++ {
		for _, k := range keys {
			if serve(k) == http.StatusOK {
				allowed++
			}
		}
	}
	if allowed != perMin {
		t.Fatalf("%d keys on one account were allowed %d requests in one window, want %d — the "+
			"per-minute ceiling must be the account's, not multiplied by its key count", len(keys), allowed, perMin)
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/price", nil)
	other := auth.Subject{Identifier: auth.AccountIdentifier("other-co"), Tier: auth.TierAPIKey, KeyID: "kid_z", RateLimitPerMin: perMin}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), other)))
	if w.Code != http.StatusOK {
		t.Fatalf("a different account got %d, want 200 — accounts must not share a bucket", w.Code)
	}
}
