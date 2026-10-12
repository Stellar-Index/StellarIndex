// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"
)

// Behavioural companions to TestCAGGPairReadsFoldBothDirections: once both directions are fetched, they must
// be folded into an exact volume-weighted union. With combineFixture, 0.5 means the flipped leg was dropped,
// 5 means it was served uninverted, and 0.4 means the fold ran.

// dirRows renders combineFixture as CAGG rows for one bucket, in the
// column order scanCombinedVwap1mRows scans.
func dirRows(b time.Time, base, quote string) [][]driver.Value {
	return [][]driver.Value{
		{b, base, "0.5", "100", int64(1), nil},
		{b, quote, "5", "10", int64(9), nil},
	}
}

// TestRecentClosedVWAP1mForPair_FoldsBothDirections covers the SEP-40 /v1/oracle/prices series.
// A reader filtering a single orientation would leave a flipped-only minute absent from the
// series and a two-sided minute reporting one leg's price.
func TestRecentClosedVWAP1mForPair_FoldsBothDirections(t *testing.T) {
	pair := testXLMUSDCPair(t)
	base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	var rows [][]driver.Value
	for i := 0; i < 3; i++ {
		b := base.Add(-time.Duration(i) * time.Minute) // newest first
		rows = append(rows, dirRows(b, pair.Base.String(), pair.Quote.String())...)
	}
	script := []scriptedResult{{
		cols: []string{"bucket", "base_asset", "vwap", "volume", "trade_count", "sources"},
		rows: rows,
	}}
	store, _ := newScriptedStore(t, script...)
	defer func() { _ = store.db.Close() }()

	got, err := store.RecentClosedVWAP1mForPair(context.Background(), pair, 3)
	if err != nil {
		t.Fatalf("RecentClosedVWAP1mForPair: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("limit=3 returned %d buckets, want 3 — `limit` is a BUCKET "+
			"limit, and a two-sided series must not return half as many "+
			"points just because each bucket costs two rows", len(got))
	}
	for i, r := range got {
		if r.VWAP != combineFixtureUnionVWAP {
			t.Errorf("bucket %d VWAP = %s, want the union %s (0.5 = flipped leg "+
				"dropped, 5 = flipped leg served uninverted)",
				i, r.VWAP, combineFixtureUnionVWAP)
		}
		if r.BaseAsset != pair.Base.String() || r.QuoteAsset != pair.Quote.String() {
			t.Errorf("bucket %d reported as %s/%s, want the REQUESTED orientation %s/%s",
				i, r.BaseAsset, r.QuoteAsset, pair.Base, pair.Quote)
		}
		if r.TradeCount != 10 {
			t.Errorf("bucket %d trade_count = %d, want 10 (the sum of both directions)",
				i, r.TradeCount)
		}
	}
}

// TestRecentClosedVWAP1mForPair_FlippedOnlyBucketIsServed: a market that traded ONLY in the stored
// (quote, base) orientation must still be served, or /v1/oracle/prices returns an empty array for an asset
// /v1/oracle/lastprice prices fine, and two endpoints on one SEP-40 surface disagree.
func TestRecentClosedVWAP1mForPair_FlippedOnlyBucketIsServed(t *testing.T) {
	pair := testXLMUSDCPair(t)
	b := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	script := []scriptedResult{{
		cols: []string{"bucket", "base_asset", "vwap", "volume", "trade_count", "sources"},
		rows: [][]driver.Value{
			// Only the flipped leg: 10 USDC traded for 40 XLM.
			{b, pair.Quote.String(), "4", "10", int64(3), nil},
		},
	}}
	store, _ := newScriptedStore(t, script...)
	defer func() { _ = store.db.Close() }()

	got, err := store.RecentClosedVWAP1mForPair(context.Background(), pair, 10)
	if err != nil {
		t.Fatalf("RecentClosedVWAP1mForPair: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("flipped-only bucket returned %d rows, want 1 — this is the "+
			"UNAUTH-DOS-9 scenario: the SEP-40 series went silently sparse", len(got))
	}
	// 10 USDC / 40 XLM = 0.25 USDC per XLM.
	if got[0].VWAP != "0.25" {
		t.Errorf("flipped-only bucket VWAP = %s, want 0.25 (the exact inverse); "+
			"4 would mean the stored flipped price was served uninverted, which "+
			"is a 16x error on a money surface", got[0].VWAP)
	}
}

// TestClosedVWAP1mAtOrBefore_FoldsBothDirections — /v1/assets/{id}'s
// change_24h_pct anchor. This is the reader UNAUTH-DOS-9 itself missed.
func TestClosedVWAP1mAtOrBefore_FoldsBothDirections(t *testing.T) {
	pair := testXLMUSDCPair(t)
	b := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	script := []scriptedResult{{
		cols: []string{"bucket", "base_asset", "vwap", "volume", "trade_count", "sources"},
		rows: dirRows(b, pair.Base.String(), pair.Quote.String()),
	}}
	store, _ := newScriptedStore(t, script...)
	defer func() { _ = store.db.Close() }()

	got, err := store.ClosedVWAP1mAtOrBefore(context.Background(), pair, b.Add(time.Hour))
	if err != nil {
		t.Fatalf("ClosedVWAP1mAtOrBefore: %v", err)
	}
	if got.VWAP != combineFixtureUnionVWAP {
		t.Errorf("anchor VWAP = %s, want the union %s — an anchor taken from one "+
			"leg makes change_24h_pct wrong by whatever the other leg traded",
			got.VWAP, combineFixtureUnionVWAP)
	}
	if !got.Bucket.Equal(b) {
		t.Errorf("anchor bucket = %s, want %s", got.Bucket, b)
	}
}

// TestClosedVWAP1mAtOrBefore_TrimsTheOlderBucket: the query caps at LIMIT 2 as a bucket holds at most two
// rows; when the newest qualifying bucket has one, the second row is from an older bucket and must not leak
// into the anchor.
func TestClosedVWAP1mAtOrBefore_TrimsTheOlderBucket(t *testing.T) {
	pair := testXLMUSDCPair(t)
	newest := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	older := newest.Add(-time.Minute)

	script := []scriptedResult{{
		cols: []string{"bucket", "base_asset", "vwap", "volume", "trade_count", "sources"},
		rows: [][]driver.Value{
			{newest, pair.Base.String(), "0.5", "100", int64(1), nil},
			{older, pair.Base.String(), "9.9", "100", int64(1), nil},
		},
	}}
	store, _ := newScriptedStore(t, script...)
	defer func() { _ = store.db.Close() }()

	got, err := store.ClosedVWAP1mAtOrBefore(context.Background(), pair, newest.Add(time.Hour))
	if err != nil {
		t.Fatalf("ClosedVWAP1mAtOrBefore: %v", err)
	}
	if !got.Bucket.Equal(newest) {
		t.Fatalf("anchor bucket = %s, want the NEWEST %s", got.Bucket, newest)
	}
	if got.VWAP != "0.5" {
		t.Errorf("anchor VWAP = %s, want 0.5 — 9.9 means the older bucket's row "+
			"was folded into the newest bucket's answer", got.VWAP)
	}
}

// TestClosedVWAP1mAtOrBefore_NoRowsIsErrNoRows — the sentinel the
// caller branches on must survive the rewrite from QueryRow to Query.
func TestClosedVWAP1mAtOrBefore_NoRowsIsErrNoRows(t *testing.T) {
	pair := testXLMUSDCPair(t)
	script := []scriptedResult{{
		cols: []string{"bucket", "base_asset", "vwap", "volume", "trade_count", "sources"},
		rows: nil,
	}}
	store, _ := newScriptedStore(t, script...)
	defer func() { _ = store.db.Close() }()

	_, err := store.ClosedVWAP1mAtOrBefore(context.Background(), pair, time.Now())
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("empty result returned %v, want sql.ErrNoRows — the "+
			"change_24h_pct path branches on that sentinel", err)
	}
}

// TestTimedVWAPs1mForChangeSummary_FoldsBothDirections is the third instance of the class. It
// also asserts the ASC ordering and the bucket-END timestamp, since the worker keys points by
// when the bucket CLOSED.
func TestTimedVWAPs1mForChangeSummary_FoldsBothDirections(t *testing.T) {
	pair := testXLMUSDCPair(t)
	start := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

	var rows [][]driver.Value
	for i := 0; i < 2; i++ {
		b := start.Add(time.Duration(i) * time.Minute) // oldest first
		rows = append(rows, dirRows(b, pair.Base.String(), pair.Quote.String())...)
	}
	script := []scriptedResult{{
		cols: []string{"bucket", "base_asset", "vwap", "volume", "trade_count", "sources"},
		rows: rows,
	}}
	store, _ := newScriptedStore(t, script...)
	defer func() { _ = store.db.Close() }()

	got, err := store.TimedVWAPs1mForChangeSummary(
		context.Background(), pair, start, start.Add(time.Hour))
	if err != nil {
		t.Fatalf("TimedVWAPs1mForChangeSummary: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("returned %d points, want 2 (one per bucket, not one per row)", len(got))
	}
	for i, p := range got {
		wantAt := start.Add(time.Duration(i)*time.Minute + time.Minute)
		if !p.At.Equal(wantAt) {
			t.Errorf("point %d At = %s, want the bucket END %s", i, p.At, wantAt)
		}
		if p.Value != combineFixtureUnionVWAP {
			t.Errorf("point %d value = %s, want the union %s", i, p.Value, combineFixtureUnionVWAP)
		}
	}
	if !got[0].At.Before(got[1].At) {
		t.Error("points are not oldest-first; the change-summary worker assumes ASC")
	}
}
