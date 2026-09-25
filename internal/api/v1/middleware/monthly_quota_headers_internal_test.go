package middleware

import (
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMonthlyQuotaHeaders_FormatEveryInt64 pins the quota headers to
// strconv's decimal form across the int64 range. The hand-rolled
// formatter they used rendered math.MinInt64 as "-": negating it
// overflows back to itself, so its digit loop never ran.
func TestMonthlyQuotaHeaders_FormatEveryInt64(t *testing.T) {
	cases := []struct {
		quota, used  int64
		wantQ, wantU string
	}{
		{0, 0, "0", "0"},
		{1_000_000, 999_999, "1000000", "999999"},
		{math.MaxInt64, -1, "9223372036854775807", "-1"},
		{math.MinInt64, math.MinInt64, "-9223372036854775808", "-9223372036854775808"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		writeMonthlyQuotaDenied(rec, httptest.NewRequest(http.MethodGet, "/v1/assets", nil), tc.quota, tc.used)
		if got := rec.Header().Get("X-StellarIndex-Monthly-Quota"); got != tc.wantQ {
			t.Errorf("quota %d: X-StellarIndex-Monthly-Quota = %q, want %q", tc.quota, got, tc.wantQ)
		}
		if got := rec.Header().Get("X-StellarIndex-Monthly-Used"); got != tc.wantU {
			t.Errorf("used %d: X-StellarIndex-Monthly-Used = %q, want %q", tc.used, got, tc.wantU)
		}
	}
}
