package customerwebhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"
	"time"
)

// TestScheduleRetry_ExponentialBackoff pins the retry schedule
// (30s, 1m, 2m, 4m, … doubling, capped at 1h). A regression that
// dropped the cap or mis-shifted the exponent would either hammer a
// flapping endpoint or push a delivery's next attempt years out.
func TestScheduleRetry_ExponentialBackoff(t *testing.T) {
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	w := &Worker{opts: Options{Clock: func() time.Time { return base }}}

	const hour = time.Hour
	cases := []struct {
		attempt int
		want    time.Duration
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
		{30, hour},            // shift overflows int64 to <=0 → cap guard catches it
	}
	for _, tc := range cases {
		got := w.scheduleRetry(tc.attempt).Sub(base)
		if got != tc.want {
			t.Errorf("scheduleRetry(%d) delay = %s, want %s", tc.attempt, got, tc.want)
		}
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
