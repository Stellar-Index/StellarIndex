package customerwebhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// nopStore is a do-nothing DeliveryStore so New() can be exercised in
// internal (white-box) tests that only need to read back the applied
// Options defaults.
type nopStore struct{}

func (nopStore) ListPendingDeliveries(context.Context, int) ([]platform.WebhookDelivery, error) {
	return nil, nil
}

func (nopStore) GetWebhook(context.Context, uuid.UUID) (platform.CustomerWebhook, error) {
	return platform.CustomerWebhook{}, platform.ErrNotFound
}

func (nopStore) MarkDelivered(context.Context, uuid.UUID, int) error { return nil }

func (nopStore) MarkAttemptFailed(context.Context, uuid.UUID, string, int, time.Time) error {
	return nil
}

// TestBatchLimit_WorstCaseSerialUnderLease is the MEDIUM double-delivery
// guard (F-1270 hardening). The worker drains a batch SERIALLY, and the
// store leases each claimed row for storeLeaseDuration (5m). If the
// worst-case serial batch time (BatchLimit × per-request Timeout) can
// exceed that lease, a second worker re-claims the un-processed tail and
// DOUBLE-delivers. This pins the default BatchLimit × Timeout comfortably
// under the 5-minute lease; a regression that bumps BatchLimit back to
// 100 (100 × 10s = 1000s ≫ 300s) fails here.
func TestBatchLimit_WorstCaseSerialUnderLease(t *testing.T) {
	w := New(nopStore{}, Options{})

	if w.opts.BatchLimit != defaultBatchLimit {
		t.Errorf("default BatchLimit = %d, want %d", w.opts.BatchLimit, defaultBatchLimit)
	}
	worst := time.Duration(w.opts.BatchLimit) * w.opts.HTTPClient.Timeout
	if worst >= storeLeaseDuration {
		t.Fatalf("worst-case serial batch %s (BatchLimit=%d × Timeout=%s) must stay under the %s store lease; "+
			"a batch that outlives its lease is re-claimed by a second worker and double-delivered",
			worst, w.opts.BatchLimit, w.opts.HTTPClient.Timeout, storeLeaseDuration)
	}
}

// TestScheduleRetry_ExponentialBackoff pins the retry schedule (30s, 1m,
// 2m, 4m, … doubling, capped at 1h) and the INFO overflow clamp. Every
// delay is now full-jittered into [cap/2, cap], so we assert band
// membership rather than an exact value. A regression that dropped the
// cap, mis-shifted the exponent, or let `base << n` wrap int64 for a
// large attempt number would push a delay out of band and fail here.
func TestScheduleRetry_ExponentialBackoff(t *testing.T) {
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	w := &Worker{opts: Options{Clock: func() time.Time { return base }}}

	const hour = time.Hour
	cases := []struct {
		attempt int
		cap     time.Duration // deterministic (pre-jitter) backoff ceiling
	}{
		{1, 30 * time.Second},
		{2, 1 * time.Minute},
		{3, 2 * time.Minute},
		{4, 4 * time.Minute},
		{5, 8 * time.Minute},
		{6, 16 * time.Minute},
		{7, 32 * time.Minute}, // last rung below the cap
		{8, hour},             // 64m clamped down to the 1h ceiling
		{28, hour},            // large-but-non-overflowing shift → still clamped
		{30, hour},            // INFO: shift clamp prevents int64 wrap → still 1h
		{100, hour},           // arbitrarily large attempt → clamp holds, never wraps
	}
	for _, tc := range cases {
		lo, hi := tc.cap/2, tc.cap
		// Sample repeatedly: a bug that ignored the attempt number or
		// wrapped to a negative delay would have to land in-band every
		// time to escape detection.
		for i := 0; i < 32; i++ {
			got := w.scheduleRetry(tc.attempt).Sub(base)
			if got < lo || got > hi {
				t.Fatalf("scheduleRetry(%d) delay = %s, want within jitter band [%s, %s]",
					tc.attempt, got, lo, hi)
			}
		}
	}
}

// TestScheduleRetry_Jitter is the LOW thundering-herd guard: the retry
// delay is jittered, so two calls at the SAME attempt land in the
// full-jitter band [delay/2, delay] AND vary across samples. Without
// jitter, scheduleRetry(8) returns exactly 1h every time — the
// distinct-value assertion below would then see one value and fail,
// which is what makes this test non-vacuous.
func TestScheduleRetry_Jitter(t *testing.T) {
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	w := &Worker{opts: Options{Clock: func() time.Time { return base }}}

	const attempt = 8 // 1h rung → widest band [30m, 1h]
	lo, hi := 30*time.Minute, time.Hour

	seen := map[time.Duration]struct{}{}
	const samples = 64
	for i := 0; i < samples; i++ {
		got := w.scheduleRetry(attempt).Sub(base)
		if got < lo || got > hi {
			t.Fatalf("jittered delay %s outside band [%s, %s]", got, lo, hi)
		}
		seen[got] = struct{}{}
	}
	if len(seen) < 2 {
		t.Errorf("expected jittered delays to vary across %d samples, got %d distinct value(s) — "+
			"jitter absent (thundering-herd risk)", samples, len(seen))
	}
}

// TestSignHMACSHA256_BindsTimestamp is the CS-055 replay-defence
// guard: the signature MUST cover "<unix_ts>." + body, not the body
// alone (a body-only signature made a captured delivery replayable
// forever). We recompute the expected HMAC and assert the exact
// value, then assert that a body-only signature differs — proving
// the timestamp is genuinely mixed in.
func TestSignHMACSHA256_BindsTimestamp(t *testing.T) {
	secret := []byte("whsec_test_key")
	ts := int64(1_751_720_400)
	payload := []byte(`{"event":"key.mint","id":"abc"}`)

	got := signHMACSHA256(secret, ts, payload)

	// Independent recomputation of the documented scheme.
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte{'.'})
	mac.Write(payload)
	want := hex.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Fatalf("signature = %s, want %s", got, want)
	}

	// Body-only HMAC (the pre-CS-055 shape) must NOT match — this is
	// the whole point of the timestamped construction.
	bodyOnly := hmac.New(sha256.New, secret)
	bodyOnly.Write(payload)
	if got == hex.EncodeToString(bodyOnly.Sum(nil)) {
		t.Error("signature equals body-only HMAC; timestamp is not bound (CS-055 regression)")
	}

	// Different timestamp / secret → different signature; determinism holds.
	if got == signHMACSHA256(secret, ts+1, payload) {
		t.Error("changing the timestamp did not change the signature")
	}
	if got == signHMACSHA256([]byte("other"), ts, payload) {
		t.Error("changing the secret did not change the signature")
	}
	if got != signHMACSHA256(secret, ts, payload) {
		t.Error("signature is not deterministic for identical inputs")
	}
}
