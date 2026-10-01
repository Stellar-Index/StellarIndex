// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package scale

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCandleTxHash_GranularityDistinguishesSameClose(t *testing.T) {
	const closeTs = int64(1_745_006_399)
	seen := map[string]time.Duration{}
	for _, g := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 4 * time.Hour, 24 * time.Hour} {
		h, err := CandleTxHash("XLMUSD", closeTs, g)
		if err != nil {
			t.Fatalf("%v: %v", g, err)
		}
		if len(h) != 64 {
			t.Fatalf("%v: len %d, want 64", g, len(h))
		}
		if prev, dup := seen[h]; dup {
			t.Fatalf("%v and %v candles closing at %d share tx_hash %s", prev, g, closeTs, h)
		}
		seen[h] = g
	}
}

// Rows already written at the default granularity must keep their
// identity, or a re-run inserts a second copy of every candle.
func TestCandleTxHash_DefaultGranularityKeepsLegacyIdentity(t *testing.T) {
	const closeTs = int64(1_745_006_399)
	want := SyntheticTxHash(fmt.Sprintf("XLMUSD-BF-%020d", closeTs))
	got, err := CandleTxHash("XLMUSD", closeTs, LegacyCandleGranularity)
	if err != nil || got != want {
		t.Fatalf("1h identity = (%s, %v), want legacy %s", got, err, want)
	}
}

// A symbol whose legacy seed would truncate is refused at every
// granularity, and a non-positive granularity is never an identity.
func TestCandleTxHash_Refusals(t *testing.T) {
	for _, g := range []time.Duration{time.Minute, time.Hour} {
		if _, err := CandleTxHash("RENDERUSD", 0, g); !errors.Is(err, ErrSyntheticSeedTooLong) {
			t.Errorf("9-byte symbol at %v: err = %v, want ErrSyntheticSeedTooLong", g, err)
		}
	}
	if _, err := CandleTxHash("XLMUSD", 0, 0); err == nil {
		t.Error("zero granularity accepted")
	}
}

// Every CEX candle backfill must derive identity through CandleTxHash;
// a private "-BF-" seed would drop granularity again.
func TestCandleTxHash_NoPrivateCandleSeeds(t *testing.T) {
	files, err := filepath.Glob("../*/*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == filepath.Join("..", "scale", "scale.go") {
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		if strings.Contains(string(body), "-BF-") {
			t.Errorf("%s formats a private -BF- candle seed; use scale.CandleTxHash", f)
		}
	}
	if checked == 0 {
		t.Fatal("scanned no source files; glob is wrong")
	}
}

func TestCandleClosed(t *testing.T) {
	to := time.Unix(1_745_010_000, 0)
	later := to.Add(time.Hour)
	for _, c := range []struct {
		name string
		end  time.Time
		now  time.Time
		want bool
	}{
		{"ends before to and now", to.Add(-time.Hour), later, true},
		{"ends exactly at to", to, later, true},
		{"ends past to", to.Add(time.Second), later, false},
		{"ends exactly at now", to.Add(-time.Minute), to.Add(-time.Minute), true},
		{"still open at now", to.Add(-time.Minute), to.Add(-2 * time.Minute), false},
	} {
		if got := CandleClosed(c.end, to, c.now); got != c.want {
			t.Errorf("%s: CandleClosed = %v, want %v", c.name, got, c.want)
		}
	}
}
