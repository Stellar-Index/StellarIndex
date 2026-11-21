package pricingguard

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type fakeExclusions struct {
	rows  []timescale.SourceExclusion
	err   error
	reads int
}

func (f *fakeExclusions) UnliftedSourceExclusions(context.Context) ([]timescale.SourceExclusion, error) {
	f.reads++
	return f.rows, f.err
}

var exclBucket = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func exclPair(t *testing.T) (canonical.Asset, canonical.Asset) {
	t.Helper()
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	return canonical.NativeAsset(), usd
}

func exclGate(f *fakeExclusions, now *time.Time) *SourceExclusionGate {
	g := NewSourceExclusionGate(f, SourceExclusionGateOptions{})
	g.now = func() time.Time { return *now }
	return g
}

func TestSourceExclusionGate_Verdicts(t *testing.T) {
	base, quote := exclPair(t)
	row := timescale.Vwap1mRow{Bucket: exclBucket, VWAP: "0.1", Sources: []string{"sdex", "kraken"}}
	open := timescale.SourceExclusion{Source: "kraken", From: exclBucket.Add(-time.Hour)}
	cases := []struct {
		name string
		rows []timescale.SourceExclusion
		want Withholding
	}{
		{"none", nil, NotWithheld},
		{"contributing source excluded", []timescale.SourceExclusion{open}, WithheldSourceExcluded},
		{"other source excluded", []timescale.SourceExclusion{{Source: "binance", From: open.From}}, NotWithheld},
		{"window ends before the bucket", []timescale.SourceExclusion{{Source: "kraken", From: open.From, Until: exclBucket}}, NotWithheld},
		{"window starts inside the bucket", []timescale.SourceExclusion{{Source: "kraken", From: exclBucket.Add(30 * time.Second)}}, WithheldSourceExcluded},
		{"window starts at the bucket close", []timescale.SourceExclusion{{Source: "kraken", From: exclBucket.Add(time.Minute)}}, NotWithheld},
		{"lifted", []timescale.SourceExclusion{{Source: "kraken", From: open.From, LiftedAt: exclBucket}}, NotWithheld},
		{"pair scope, alias form, reversed", []timescale.SourceExclusion{{Source: "kraken", Base: "fiat:USD", Quote: "crypto:XLM", From: open.From}}, WithheldSourceExcluded},
		{"pair scope, other pair", []timescale.SourceExclusion{{Source: "kraken", Base: "crypto:BTC", Quote: "fiat:USD", From: open.From}}, NotWithheld},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := exclBucket
			g := exclGate(&fakeExclusions{rows: tc.rows}, &now)
			if got := g.BucketWithholding(context.Background(), base, quote, row); got != tc.want {
				t.Fatalf("BucketWithholding = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSourceExclusionGate_NilWithholdsNothing(t *testing.T) {
	base, quote := exclPair(t)
	var g *SourceExclusionGate
	rows := []timescale.Vwap1mRow{{Bucket: exclBucket, Sources: []string{"kraken"}}}
	if got := g.BucketWithholding(context.Background(), base, quote, rows[0]); got != NotWithheld {
		t.Fatalf("nil gate = %q", got)
	}
	if got, w := g.CleanBuckets(context.Background(), base, quote, rows); w != NotWithheld || len(got) != 1 {
		t.Fatalf("nil gate CleanBuckets = %v, %q", got, w)
	}
}

// A kill switch that cannot be read must not read as "nothing excluded"; the last good read covers a brief outage only.
func TestSourceExclusionGate_UnreadableFailsClosed(t *testing.T) {
	base, quote := exclPair(t)
	row := timescale.Vwap1mRow{Bucket: exclBucket, Sources: []string{"sdex"}}
	now := exclBucket
	f := &fakeExclusions{err: errors.New("db down")}
	g := exclGate(f, &now)
	if got := g.BucketWithholding(context.Background(), base, quote, row); got != WithheldExclusionsUnread {
		t.Fatalf("never read: %q, want %q", got, WithheldExclusionsUnread)
	}

	f.err = nil
	if got := g.BucketWithholding(context.Background(), base, quote, row); got != NotWithheld {
		t.Fatalf("readable: %q", got)
	}
	f.err = errors.New("db down")
	now = now.Add(exclusionMaxStale - time.Second)
	if got := g.BucketWithholding(context.Background(), base, quote, row); got != NotWithheld {
		t.Fatalf("within the stale bound: %q, want the last good read", got)
	}
	now = now.Add(2 * time.Second)
	if got := g.BucketWithholding(context.Background(), base, quote, row); got != WithheldExclusionsUnread {
		t.Fatalf("past the stale bound: %q, want %q", got, WithheldExclusionsUnread)
	}
	if got, w := g.CleanBuckets(context.Background(), base, quote, []timescale.Vwap1mRow{row}); w != WithheldExclusionsUnread || got != nil {
		t.Fatalf("CleanBuckets unread = %v, %q", got, w)
	}
}

func TestSourceExclusionGate_CachesWithinTTL(t *testing.T) {
	base, quote := exclPair(t)
	row := timescale.Vwap1mRow{Bucket: exclBucket, Sources: []string{"kraken"}}
	now := exclBucket
	f := &fakeExclusions{}
	g := exclGate(f, &now)
	g.BucketWithholding(context.Background(), base, quote, row)
	f.rows = []timescale.SourceExclusion{{Source: "kraken", From: exclBucket.Add(-time.Hour)}}
	now = now.Add(exclusionCacheTTL - time.Second)
	if got := g.BucketWithholding(context.Background(), base, quote, row); got != NotWithheld || f.reads != 1 {
		t.Fatalf("within TTL: %q after %d reads, want the cached set and one read", got, f.reads)
	}
	now = now.Add(2 * time.Second)
	if got := g.BucketWithholding(context.Background(), base, quote, row); got != WithheldSourceExcluded {
		t.Fatalf("after TTL: %q, want the new exclusion applied", got)
	}
}

func TestSourceExclusionGate_CleanBucketsDropsOnlyCovered(t *testing.T) {
	base, quote := exclPair(t)
	now := exclBucket
	g := exclGate(&fakeExclusions{rows: []timescale.SourceExclusion{{Source: "kraken", From: exclBucket.Add(-2 * time.Minute)}}}, &now)
	rows := []timescale.Vwap1mRow{
		{Bucket: exclBucket, VWAP: "9", Sources: []string{"sdex", "kraken"}},
		{Bucket: exclBucket.Add(-time.Minute), VWAP: "1", Sources: []string{"sdex"}},
		{Bucket: exclBucket.Add(-3 * time.Minute), VWAP: "2", Sources: []string{"kraken"}},
	}
	got, w := g.CleanBuckets(context.Background(), base, quote, rows)
	if w != NotWithheld || len(got) != 2 || got[0].VWAP != "1" || got[1].VWAP != "2" {
		t.Fatalf("CleanBuckets = %+v, %q; want the sdex-only and pre-window buckets", got, w)
	}
}
