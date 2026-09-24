// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// openBucketCeiling matches a `bucket < now()` upper bound. A bucket's
// START is compared, so the in-progress bucket (start in (now-1min, now])
// passes it; the closed-bucket form (ADR-0015) is
// `bucket <= now() - INTERVAL '<bucket width>'`.
var openBucketCeiling = regexp.MustCompile(`(?i)\bbucket\s*<\s*now\(\)`)

// TestNoOpenBucketCeilingInReaders fails on any non-test source in this
// package that bounds a CAGG read with `bucket < now()`, which admits the
// in-progress bucket the moment real-time aggregation is enabled on the view.
func TestNoOpenBucketCeilingInReaders(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for i, line := range strings.Split(string(src), "\n") {
			if openBucketCeiling.MatchString(line) {
				t.Errorf("%s:%d: open-bucket ceiling %q; use `bucket <= now() - INTERVAL '1 minute'` (ADR-0015)",
					f, i+1, strings.TrimSpace(line))
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no package sources; the guard did not run")
	}
}

// TestTrailing24hVolumeReadersUseClosedCeiling pins the corrected bound on
// the trailing-24h USD-volume reads over prices_1m.
func TestTrailing24hVolumeReadersUseClosedCeiling(t *testing.T) {
	const closed = "bucket <= now() - INTERVAL '1 minute'"
	for name, q := range map[string]string{
		"refreshAssetVolumeUpsert": refreshAssetVolumeUpsert,
		"getAssetBySlugSQL":        getAssetBySlugSQL,
		"getNativeAssetSQL":        getNativeAssetSQL,
		"sorobanVolume24hUSDQuery": sorobanVolume24hUSDQuery,
	} {
		if !strings.Contains(q, closed) {
			t.Errorf("%s missing closed-bucket ceiling %q", name, closed)
		}
	}
}
