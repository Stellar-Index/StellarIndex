package v1

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestCachedRWAAssetsView_AgeIsBoundedAndMarked: an expired view is served stale AND degraded, and once its rebuilds
// have failed past rwaAssetsViewMaxStale it is no longer served at all.
func TestCachedRWAAssetsView_AgeIsBoundedAndMarked(t *testing.T) {
	done := make(chan struct{})
	close(done)
	for _, tc := range []struct {
		name    string
		age     time.Duration
		served  bool
		wantErr bool
	}{
		{"expired, inside the bound", 20 * time.Minute, true, false},
		{"expired, past the bound", rwaAssetsViewMaxStale + time.Minute, false, true},
	} {
		built := time.Now().Add(-tc.age)
		s := &Server{
			rwaViewCache:   &rwaAssetsBuild{builtAt: built, observedAt: built},
			rwaViewExpires: built.Add(rwaAssetsViewTTL),
			rwaViewFlight:  done,
			rwaViewErr:     errors.New("listing page: statement timeout"),
		}
		b, err := s.cachedRWAAssetsView(context.Background())
		if (err != nil) != tc.wantErr {
			t.Fatalf("%s: err = %v, want error %v", tc.name, err, tc.wantErr)
		}
		if tc.served && (!b.flags.Stale || !b.flags.Degraded || !b.observedAt.Equal(built)) {
			t.Errorf("%s: build = %+v, want stale, degraded and dated at its build", tc.name, b)
		}
	}
}
