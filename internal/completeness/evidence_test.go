package completeness

import (
	"testing"
	"time"
)

func TestProjectionEvidenceExpired(t *testing.T) {
	now := time.Date(2026, 10, 2, 6, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		evidencedAt time.Time
		maxAge      time.Duration
		want        bool
	}{
		{"fresh", now.Add(-time.Hour), MaxProjectionCarryAge, false},
		{"exactly at the bound", now.Add(-MaxProjectionCarryAge), MaxProjectionCarryAge, false},
		{"past the bound", now.Add(-MaxProjectionCarryAge - time.Second), MaxProjectionCarryAge, true},
		{"unknown evidence is never fresh", time.Time{}, MaxProjectionCarryAge, true},
		{"bound disabled", time.Time{}, 0, false},
	} {
		if got := ProjectionEvidenceExpired(tc.evidencedAt, now, tc.maxAge); got != tc.want {
			t.Errorf("%s: ProjectionEvidenceExpired = %v, want %v", tc.name, got, tc.want)
		}
	}
}
