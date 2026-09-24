package v1_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

type fakeOpenBucketReader struct {
	views []string
	err   error
}

func (f fakeOpenBucketReader) OpenBucketCAGGs(context.Context) ([]string, error) {
	return f.views, f.err
}

// TestClosedBucketChecker_Ping pins ADR-0015 at runtime: a CAGG that serves
// its open bucket (e.g. an out-of-band materialized_only = false on
// prices_1m) must fail readiness, and so must an unreadable catalog.
func TestClosedBucketChecker_Ping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		reader     fakeOpenBucketReader
		wantSubstr string
	}{
		{name: "every CAGG closed-bucket", reader: fakeOpenBucketReader{}},
		{
			name:       "prices_1m flipped to real-time",
			reader:     fakeOpenBucketReader{views: []string{"oracle_prices_1d", "prices_1m"}},
			wantSubstr: "oracle_prices_1d, prices_1m",
		},
		{
			name:       "catalog unreadable fails closed",
			reader:     fakeOpenBucketReader{err: errors.New("boom")},
			wantSubstr: "boom",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := v1.NewClosedBucketChecker(tc.reader)
			if !c.Critical() {
				t.Fatal("closed-bucket checker must be Critical: an open-bucket CAGG serves two prices for one pair")
			}
			err := c.Ping(context.Background())
			if tc.wantSubstr == "" {
				if err != nil {
					t.Fatalf("Ping = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("Ping = %v, want error containing %q", err, tc.wantSubstr)
			}
		})
	}
}
