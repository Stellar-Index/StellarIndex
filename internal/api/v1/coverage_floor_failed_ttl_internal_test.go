// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// erroringCoverageFloorReader answers every probe with a transient,
// non-context error — the "database answered with an error" shape.
type erroringCoverageFloorReader struct{}

func (erroringCoverageFloorReader) EarliestBucket(context.Context, canonical.Pair, string, time.Time, time.Time) (time.Time, bool, error) {
	return time.Time{}, false, errors.New("prices_1d unavailable")
}

func (erroringCoverageFloorReader) EarliestBucketAsStored(context.Context, canonical.Pair, string, time.Time, time.Time) (time.Time, bool, error) {
	return time.Time{}, false, errors.New("prices_1d unavailable")
}

func (erroringCoverageFloorReader) EarliestBucketLiteralQuote(context.Context, canonical.Pair, string, time.Time, time.Time) (time.Time, bool, error) {
	return time.Time{}, false, errors.New("prices_1d unavailable")
}

// TestCoverageFloor_FailedProbeUsesShortTTL is CA2-A02-harden-6: a
// transient store error must not poison the advisory
// coverage_from/outside_coverage annotation for the same thirty
// minutes a genuine floor answer earns. The failed entry's expiry must
// not be `now.Add(coverageFloorTTL)` — identical to a success — so
// this test fails if it is (expiry ~30m out) and passes once the
// failed branch uses its own short TTL.
func TestCoverageFloor_FailedProbeUsesShortTTL(t *testing.T) {
	pair, err := canonical.NewPair(mustParseAsset("crypto:XLM"), mustParseAsset("fiat:USD"))
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	s := &Server{
		Options:            Options{CoverageFloor: erroringCoverageFloorReader{}},
		logger:             slog.Default(),
		coverageFloorCache: &coverageFloorCache{entries: map[string]coverageFloorEntry{}},
	}

	before := time.Now().UTC()
	_, outcome := s.coverageFloor(context.Background(), pair, spanAliasedBothDirections)
	if outcome != coverageFloorFailed {
		t.Fatalf("outcome = %v, want coverageFloorFailed", outcome)
	}

	key := coverageFloorKey(pair, spanAliasedBothDirections)
	entry, ok := s.coverageFloorCache.lookup(key, before)
	if !ok {
		t.Fatal("failed probe was not memoised at all")
	}

	ttl := entry.expires.Sub(before)
	if ttl > coverageFloorFailedTTL+5*time.Second {
		t.Fatalf("failed-probe TTL = %s, want <= ~%s (coverageFloorFailedTTL), not the %s success TTL",
			ttl, coverageFloorFailedTTL, coverageFloorTTL)
	}
}
