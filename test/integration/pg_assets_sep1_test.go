//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// rwa_curated_directory (migration 0161) census through real Timescale.
// The curated arm serves only contract rows, so the census must split the
// recognised population by address form or the classic rows it leaves
// out vanish from the response. The fixture includes a classic code that
// begins with C, which a `LIKE 'C%'` contract test would misfile.
func TestCuratedRWADirectory_CensusSplitsAddressForms(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const (
		curator        = "dune:stellar"
		pricedContract = "CBUBVYRKTQLMDRUBPP6SH4GO33KZCEEYBIWB5AWNGKODP4A6KPKM2VJ4"
		bareContract   = "CBGV2QFQBBGEQRUKUMCPO3SZOHDDYO6SCP5CH6TW7EALKVHCXTMWDDOF"
		pricedClassic  = "CETES-GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
		bareClassic    = "yUSDC-GCUG7ARUFEEUMSL56K7245YCPXPZPOXAY6TSRXZB2JZFBI4DOBVOTUSA"
		staleClassic   = "BENJI-GAXSPCTVGFIVYGHT7JLJZV57HCN5KUYDJ6DMPLNWUKL7A5A3HKCNW7JW"
	)
	priced := time.Now().UTC().Add(-time.Hour)
	entries := []timescale.CuratedRWAEntry{
		{Address: pricedContract, AssetCode: "VuMe", PriceUSD: "1.1174", PricedAt: priced},
		{Address: bareContract, AssetCode: "EUTBL"},
		{Address: pricedClassic, AssetCode: "CETES", PriceUSD: "0.057", PricedAt: priced},
		{Address: bareClassic, AssetCode: "yUSDC"},
		{Address: staleClassic, AssetCode: "BENJI", PriceUSD: "1", PricedAt: priced},
	}
	if _, _, err := store.ReplaceCuratedRWADirectory(ctx, curator, entries, "test"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE rwa_curated_directory SET synced_at = now() - INTERVAL '72 hours' WHERE address = $1`,
		staleClassic); err != nil {
		t.Fatalf("age row: %v", err)
	}

	rows, census, err := store.CuratedRWADirectoryByAddress(ctx, curator)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := timescale.CuratedRWACensus{
		Entries: 4, Contracts: 2, Classic: 2, Priced: 1, PricedClassic: 1, Stale: 1,
	}
	if census != want {
		t.Errorf("census = %+v, want %+v", census, want)
	}
	if why := census.Check(); why != "" {
		t.Errorf("census does not balance: %s", why)
	}
	if len(rows) != census.Entries {
		t.Errorf("read returned %d rows, census.Entries = %d", len(rows), census.Entries)
	}
}

// curated_rwa_published_series (migration 0162) through real Timescale:
// the sync's replace-per-series transaction and the reader that serves
// the curator's latest published total, its split and the whole series.
//
// What only a database can prove here:
//
//  1. REPLACE is per series and whole: a second run's total series
//     evicts every month the first run wrote, including months the
//     second run no longer prints, while a series the run did NOT name
//     is left exactly as it was.
//  2. The reader's recognition bound is in the SQL: rows whose
//     observed_at is past 48h are an absence, not a stale figure.
//  3. NUMERIC round-trip: the curator's printed decimal comes back as the
//     same literal (ADR-0003), and `date` comes back as the same day.
func TestCuratedRWAPublished_ReplacePerSeriesAndRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const curator = "dune:stellar"
	executed := time.Date(2026, 9, 17, 4, 58, 0, 0, time.UTC)
	// The split is a separate query execution on its own clock.
	splitExecuted := executed.Add(-30 * time.Hour)
	month := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	total := func(mo time.Time, v string) timescale.CuratedRWAPublishedRow {
		return timescale.CuratedRWAPublishedRow{
			Series: timescale.CuratedRWASeriesMonthlyTotal, MonthEnd: mo, ValueUSD: v,
			SourceQuery: 6961845, ExecutedAt: executed,
		}
	}
	split := func(mo time.Time, sub, v string) timescale.CuratedRWAPublishedRow {
		return timescale.CuratedRWAPublishedRow{
			Series: timescale.CuratedRWASeriesMonthlyBySubclass, MonthEnd: mo, Subclass: sub, ValueUSD: v,
			SourceQuery: 6961847, ExecutedAt: splitExecuted,
		}
	}

	// ── run 1: two months of total, a split for each ──
	n, err := store.ReplaceCuratedRWAPublished(ctx, curator, []timescale.CuratedRWAPublishedRow{
		total(month(2025, 7, 31), "3900000000.10"),
		total(month(2025, 8, 31), "4004795860.00"),
		split(month(2025, 7, 31), "US Treasuries", "3000000000.10"),
		split(month(2025, 7, 31), "Private Credit", "900000000.00"),
		split(month(2025, 8, 31), "US Treasuries", "3100000000.00"),
		split(month(2025, 8, 31), "Private Credit", "904795860.00"),
	})
	if err != nil {
		t.Fatalf("replace run 1: %v", err)
	}
	if n != 6 {
		t.Errorf("run 1 inserted %d rows, want 6", n)
	}

	got, err := store.LatestCuratedPublished(ctx, curator)
	if err != nil {
		t.Fatalf("read after run 1: %v", err)
	}
	if got == nil {
		t.Fatal("read after run 1 returned nil — nothing recognised")
	}
	if !got.MonthEnd.Equal(month(2025, 8, 31)) || got.TotalUSD != "4004795860.00" {
		t.Errorf("latest = %s %s, want 2025-08-31 4004795860.00 (the literal, not a float rendering)",
			got.MonthEnd.Format("2006-01-02"), got.TotalUSD)
	}
	if got.SourceQuery != 6961845 || got.SplitSourceQuery != 6961847 || !got.ExecutedAt.Equal(executed) {
		t.Errorf("provenance = query %d / %d at %v, want 6961845 / 6961847 at %v",
			got.SourceQuery, got.SplitSourceQuery, got.ExecutedAt, executed)
	}
	if !got.SplitExecutedAt.Equal(splitExecuted) {
		t.Errorf("split executed_at = %v, want the split query's own %v (not the total's %v)",
			got.SplitExecutedAt, splitExecuted, executed)
	}
	if got.ObservedAt.IsZero() || time.Since(got.ObservedAt) > time.Minute {
		t.Errorf("observed_at = %v, want this run's clock", got.ObservedAt)
	}
	if len(got.Series) != 2 || !got.Series[0].MonthEnd.Equal(month(2025, 7, 31)) || got.Series[1].ValueUSD != "4004795860.00" {
		t.Errorf("series = %+v, want two points oldest first", got.Series)
	}
	if len(got.BySubclass) != 2 || got.BySubclass[0].Subclass != "US Treasuries" || got.BySubclass[0].ValueUSD != "3100000000.00" ||
		got.BySubclass[1].Subclass != "Private Credit" || got.BySubclass[1].ValueUSD != "904795860.00" {
		t.Errorf("split = %+v, want the LATEST month's two rows largest first", got.BySubclass)
	}

	// ── run 2: the total series alone, one month dropped, one added ──
	// A replace is per series and whole: July must be gone from the
	// total series (the curator no longer prints it), September must be
	// present, and the split series — not named by this run — must be
	// exactly what run 1 wrote.
	if _, err := store.ReplaceCuratedRWAPublished(ctx, curator, []timescale.CuratedRWAPublishedRow{
		total(month(2025, 8, 31), "4004795860.00"),
		total(month(2025, 9, 30), "4100000000.00"),
	}); err != nil {
		t.Fatalf("replace run 2: %v", err)
	}
	got, err = store.LatestCuratedPublished(ctx, curator)
	if err != nil || got == nil {
		t.Fatalf("read after run 2: %v, %v", got, err)
	}
	if !got.MonthEnd.Equal(month(2025, 9, 30)) || got.TotalUSD != "4100000000.00" {
		t.Errorf("latest after run 2 = %s %s, want 2025-09-30 4100000000.00", got.MonthEnd.Format("2006-01-02"), got.TotalUSD)
	}
	if len(got.Series) != 2 || !got.Series[0].MonthEnd.Equal(month(2025, 8, 31)) {
		t.Errorf("series after run 2 = %+v, want Aug+Sep only (July evicted with its series)", got.Series)
	}
	// September has no split rows — the split series was not replaced —
	// so the latest month's split is empty, never July's or August's.
	if len(got.BySubclass) != 0 || !got.SplitExecutedAt.IsZero() {
		t.Errorf("split for a month the split series does not carry = %+v at %v, want none and no execution time",
			got.BySubclass, got.SplitExecutedAt)
	}
	var splitRows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM curated_rwa_published_series
		WHERE curator = $1 AND series = $2`, curator, timescale.CuratedRWASeriesMonthlyBySubclass).Scan(&splitRows); err != nil {
		t.Fatal(err)
	}
	if splitRows != 4 {
		t.Errorf("split series has %d rows after a total-only run, want run 1's 4 untouched", splitRows)
	}

	// ── another curator is invisible to this one ──
	if _, err := store.ReplaceCuratedRWAPublished(ctx, "other:curator", []timescale.CuratedRWAPublishedRow{
		total(month(2025, 10, 31), "1.00"),
	}); err != nil {
		t.Fatalf("replace other curator: %v", err)
	}
	got, err = store.LatestCuratedPublished(ctx, curator)
	if err != nil || got == nil || !got.MonthEnd.Equal(month(2025, 9, 30)) {
		t.Errorf("another curator's rows leaked into %s: %+v, %v", curator, got, err)
	}

	// ── the recognition bound: 49h-old rows are an absence ──
	if _, err := db.ExecContext(ctx, `UPDATE curated_rwa_published_series
		SET observed_at = now() - INTERVAL '49 hours' WHERE curator = $1`, curator); err != nil {
		t.Fatal(err)
	}
	got, err = store.LatestCuratedPublished(ctx, curator)
	if err != nil {
		t.Fatalf("read past the bound: %v", err)
	}
	if got != nil {
		t.Errorf("rows past the 48h recognition bound were served: %+v", got)
	}

	// ── a duplicate bucket in one run does not trip the primary key ──
	if _, err := store.ReplaceCuratedRWAPublished(ctx, curator, []timescale.CuratedRWAPublishedRow{
		total(month(2025, 9, 30), "1.00"),
		total(month(2025, 9, 30), "2.00"),
	}); err != nil {
		t.Fatalf("replace with a duplicate bucket: %v", err)
	}
	got, err = store.LatestCuratedPublished(ctx, curator)
	if err != nil || got == nil || got.TotalUSD != "2.00" {
		t.Errorf("duplicate bucket: got %+v, %v; want the LAST printed value 2.00", got, err)
	}
}

// TestPersistIssuerAuthFlagsRefusesAnOlderReading — auth_flags_as_of_ledger
// only moves forward. A labelled reading older than the one on record must
// not overwrite it: two drain runs landing out of order would otherwise
// reinstate a home_domain the account had already moved away from.
func TestPersistIssuerAuthFlagsRefusesAnOlderReading(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const issuer = "GAEVD52W5E4Q2KTVQXC76ZSZYBEXXR3GGQZAIEP6BW3JMBTUBCHRY6UM"
	seedIssuers(t, ctx, store, []seedIssuer{{g: issuer, homeDomain: ""}})
	asOf := func(v uint32) *uint32 { return &v }

	persist := func(t *testing.T, f timescale.IssuerAuthFlags) int {
		t.Helper()
		f.GStrkey = issuer
		n, err := store.PersistIssuerAuthFlags(ctx, []timescale.IssuerAuthFlags{f})
		if err != nil {
			t.Fatalf("PersistIssuerAuthFlags: %v", err)
		}
		return n
	}
	expect := func(t *testing.T, wantDomain string, wantLedger uint32, wantRequired bool) {
		t.Helper()
		got, err := store.GetIssuer(ctx, issuer)
		if err != nil {
			t.Fatalf("GetIssuer: %v", err)
		}
		if got.HomeDomain != wantDomain {
			t.Errorf("home_domain = %q, want %q", got.HomeDomain, wantDomain)
		}
		if got.AuthFlagsAsOfLedger == nil || *got.AuthFlagsAsOfLedger != wantLedger {
			t.Errorf("auth_flags_as_of_ledger = %v, want %d", got.AuthFlagsAsOfLedger, wantLedger)
		}
		if got.AuthRequired == nil || *got.AuthRequired != wantRequired {
			t.Errorf("auth_required = %v, want %v", got.AuthRequired, wantRequired)
		}
	}

	if n := persist(t, timescale.IssuerAuthFlags{
		Required: true, HomeDomain: "current.example",
		Source: timescale.AuthFlagsSourceLive, AsOfLedger: asOf(64228661),
	}); n != 1 {
		t.Fatalf("first reading changed %d row(s), want 1", n)
	}

	t.Run("an older live reading is refused", func(t *testing.T) {
		if n := persist(t, timescale.IssuerAuthFlags{
			HomeDomain: "lapsed.example",
			Source:     timescale.AuthFlagsSourceLive, AsOfLedger: asOf(64100000),
		}); n != 0 {
			t.Errorf("older reading changed %d row(s), want 0", n)
		}
		expect(t, "current.example", 64228661, true)
	})

	t.Run("an older last-known reading is refused", func(t *testing.T) {
		if n := persist(t, timescale.IssuerAuthFlags{
			Source: timescale.AuthFlagsSourceLastKnownBeforeRemoval, AsOfLedger: asOf(64200000),
		}); n != 0 {
			t.Errorf("older last-known reading changed %d row(s), want 0", n)
		}
		expect(t, "current.example", 64228661, true)
	})

	t.Run("the same ledger re-applies", func(t *testing.T) {
		if n := persist(t, timescale.IssuerAuthFlags{
			Required: true, HomeDomain: "current.example",
			Source: timescale.AuthFlagsSourceLive, AsOfLedger: asOf(64228661),
		}); n != 1 {
			t.Errorf("same-ledger reading changed %d row(s), want 1", n)
		}
		expect(t, "current.example", 64228661, true)
	})

	t.Run("a newer reading wins", func(t *testing.T) {
		if n := persist(t, timescale.IssuerAuthFlags{
			HomeDomain: "moved.example",
			Source:     timescale.AuthFlagsSourceLive, AsOfLedger: asOf(64300000),
		}); n != 1 {
			t.Errorf("newer reading changed %d row(s), want 1", n)
		}
		expect(t, "moved.example", 64300000, false)
	})
}

// Auth-flag provenance round-trip against a real Postgres.
//
// The write half of the fix (PersistIssuerAuthFlags stamping
// auth_flags_source + auth_flags_as_of_ledger), the queue that keeps it from
// becoming a one-way latch (IssuerGStrkeysNeedingRecheck), and the read half
// (GetIssuer) are one loop. Testing either end alone would miss the two
// things that only appear when they close: migration 0153's CHECKs firing on
// what the drain actually writes, and a recovered reading's home_domain.
func TestIssuerAuthFlagsProvenanceRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Real r1 residue issuers. mergedA was merged away at
	// 54,564,588 declaring `stellarbrunch.com`; mergedB at 56,082,413 with
	// flags 0xA declaring `xcrypto.exchange`; live is unresolved on r1 but
	// has a current AccountEntry at 64,228,661.
	const (
		mergedA = "GA2PQOJ26IP24ECRXEZ4BE6BEIB4HNDWSA2E6JVPFIP6KO6BKOEAZ6XW"
		mergedB = "GA2P3HKTQFBZBWOWPLESMV5AEHJLWJKNAYV5HLXOPBRWUEHFR64FQTKN"
		live    = "GAEVD52W5E4Q2KTVQXC76ZSZYBEXXR3GGQZAIEP6BW3JMBTUBCHRY6UM"
	)
	seedIssuers(t, ctx, store, []seedIssuer{
		{g: mergedA, homeDomain: ""},
		{g: mergedB, homeDomain: ""},
		{g: live, homeDomain: ""},
	})

	asOf := func(v uint32) *uint32 { return &v }

	t.Run("persist stamps provenance and keeps a dead account's domain out", func(t *testing.T) {
		n, err := store.PersistIssuerAuthFlags(ctx, []timescale.IssuerAuthFlags{
			{
				GStrkey: mergedA, Source: timescale.AuthFlagsSourceLastKnownBeforeRemoval,
				AsOfLedger: asOf(54564588),
			},
			{
				GStrkey: mergedB, Revocable: true, Clawback: true,
				Source: timescale.AuthFlagsSourceLastKnownBeforeRemoval, AsOfLedger: asOf(56082413),
			},
			{
				GStrkey: live, Source: timescale.AuthFlagsSourceLive,
				AsOfLedger: asOf(64228661), HomeDomain: "congress-card.org",
			},
		})
		if err != nil {
			t.Fatalf("PersistIssuerAuthFlags: %v", err)
		}
		if n != 3 {
			t.Fatalf("changed %d row(s), want 3", n)
		}

		got, err := store.GetIssuer(ctx, mergedB)
		if err != nil {
			t.Fatalf("GetIssuer(%s): %v", mergedB, err)
		}
		if got.AuthFlagsSource != timescale.AuthFlagsSourceLastKnownBeforeRemoval {
			t.Errorf("auth_flags_source = %q, want %q", got.AuthFlagsSource, timescale.AuthFlagsSourceLastKnownBeforeRemoval)
		}
		if got.AuthFlagsAsOfLedger == nil || *got.AuthFlagsAsOfLedger != 56082413 {
			t.Errorf("auth_flags_as_of_ledger = %v, want 56082413", got.AuthFlagsAsOfLedger)
		}
		if got.AuthRevocable == nil || !*got.AuthRevocable || got.AuthClawback == nil || !*got.AuthClawback {
			t.Errorf("recovered flags rev=%v claw=%v, want both true (mask 0xA)", got.AuthRevocable, got.AuthClawback)
		}
		// The recovered reading must leave no identity behind. The reader
		// blanks the domain and validate() refuses one, so the row's
		// home_domain is untouched — and it was NULL.
		if got.HomeDomain != "" {
			t.Errorf("home_domain = %q, want empty — a merged account's self-declared identity must not be persisted", got.HomeDomain)
		}

		if l, err := store.GetIssuer(ctx, live); err != nil {
			t.Fatalf("GetIssuer(%s): %v", live, err)
		} else {
			if l.AuthFlagsSource != timescale.AuthFlagsSourceLive {
				t.Errorf("live issuer source = %q, want %q", l.AuthFlagsSource, timescale.AuthFlagsSourceLive)
			}
			if l.HomeDomain != "congress-card.org" {
				t.Errorf("live issuer home_domain = %q, want it carried — a live account's domain is still checkable", l.HomeDomain)
			}
		}
	})

	t.Run("re-check queue holds exactly the last-known rows", func(t *testing.T) {
		got, err := store.IssuerGStrkeysNeedingRecheck(ctx, 0)
		if err != nil {
			t.Fatalf("IssuerGStrkeysNeedingRecheck: %v", err)
		}
		// Ordered by primary key, so repeated bounded runs make forward
		// progress instead of re-walking the same head. '3' (0x33) sorts
		// before 'Q' (0x51), so mergedB leads.
		want := []string{mergedB, mergedA}
		if len(got) != len(want) {
			t.Fatalf("re-check queue = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("re-check queue = %v, want %v (PK order, live rows excluded)", got, want)
			}
		}

		// And the PRIMARY queue must no longer see them: they have
		// auth_required set, which is precisely why the second queue has to
		// exist.
		primary, err := store.IssuerGStrkeysNeedingFlags(ctx, 0)
		if err != nil {
			t.Fatalf("IssuerGStrkeysNeedingFlags: %v", err)
		}
		if len(primary) != 0 {
			t.Errorf("primary queue = %v, want empty — filled rows leave it for good", primary)
		}
	})

	t.Run("a re-created issuer flips back to live", func(t *testing.T) {
		if _, err := store.PersistIssuerAuthFlags(ctx, []timescale.IssuerAuthFlags{{
			GStrkey: mergedA, Required: true,
			Source: timescale.AuthFlagsSourceLive, AsOfLedger: asOf(64228661),
		}}); err != nil {
			t.Fatalf("PersistIssuerAuthFlags: %v", err)
		}
		got, err := store.GetIssuer(ctx, mergedA)
		if err != nil {
			t.Fatalf("GetIssuer: %v", err)
		}
		if got.AuthFlagsSource != timescale.AuthFlagsSourceLive {
			t.Errorf("source = %q, want %q", got.AuthFlagsSource, timescale.AuthFlagsSourceLive)
		}
		if got.AuthFlagsAsOfLedger == nil || *got.AuthFlagsAsOfLedger != 64228661 {
			t.Errorf("as-of = %v, want 64228661 — the as-of ledger must move WITH the source, never lag it",
				got.AuthFlagsAsOfLedger)
		}
		if got.AuthRequired == nil || !*got.AuthRequired {
			t.Errorf("auth_required = %v, want the live true", got.AuthRequired)
		}
		queue, err := store.IssuerGStrkeysNeedingRecheck(ctx, 0)
		if err != nil {
			t.Fatalf("IssuerGStrkeysNeedingRecheck: %v", err)
		}
		if len(queue) != 1 || queue[0] != mergedB {
			t.Errorf("re-check queue = %v, want just [%s] — the revived row must leave it", queue, mergedB)
		}
	})

	t.Run("an unlabelled write leaves the persisted provenance alone", func(t *testing.T) {
		// The old released binary writes auth_* without touching the two
		// provenance columns (migrations rule 9). Nulling them on such a
		// write would be a regression, not a no-op.
		if _, err := store.PersistIssuerAuthFlags(ctx, []timescale.IssuerAuthFlags{
			{GStrkey: mergedB, Revocable: true, Clawback: true},
		}); err != nil {
			t.Fatalf("PersistIssuerAuthFlags: %v", err)
		}
		got, err := store.GetIssuer(ctx, mergedB)
		if err != nil {
			t.Fatalf("GetIssuer: %v", err)
		}
		if got.AuthFlagsSource != timescale.AuthFlagsSourceLastKnownBeforeRemoval {
			t.Errorf("source = %q, want it preserved as %q", got.AuthFlagsSource, timescale.AuthFlagsSourceLastKnownBeforeRemoval)
		}
		if got.AuthFlagsAsOfLedger == nil || *got.AuthFlagsAsOfLedger != 56082413 {
			t.Errorf("as-of = %v, want the preserved 56082413", got.AuthFlagsAsOfLedger)
		}
	})

	t.Run("the guards refuse what migration 0153 would reject", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			row  timescale.IssuerAuthFlags
		}{
			{"historical reading with no as-of ledger", timescale.IssuerAuthFlags{
				GStrkey: mergedB, Source: timescale.AuthFlagsSourceLastKnownBeforeRemoval,
			}},
			{"source outside the enum", timescale.IssuerAuthFlags{
				GStrkey: mergedB, Source: "guessed",
			}},
			{"dead account's home_domain", timescale.IssuerAuthFlags{
				GStrkey: mergedB, Source: timescale.AuthFlagsSourceLastKnownBeforeRemoval,
				AsOfLedger: asOf(56082413), HomeDomain: "stellarbrunch.com",
			}},
		} {
			if _, err := store.PersistIssuerAuthFlags(ctx, []timescale.IssuerAuthFlags{tc.row}); err == nil {
				t.Errorf("%s: PersistIssuerAuthFlags = nil, want a refusal", tc.name)
			}
		}
		// …and nothing partial was written.
		got, err := store.GetIssuer(ctx, mergedB)
		if err != nil {
			t.Fatalf("GetIssuer: %v", err)
		}
		if got.AuthFlagsSource != timescale.AuthFlagsSourceLastKnownBeforeRemoval || got.HomeDomain != "" {
			t.Errorf("after the refusals: source=%q home_domain=%q, want %q and empty",
				got.AuthFlagsSource, got.HomeDomain, timescale.AuthFlagsSourceLastKnownBeforeRemoval)
		}
	})
}

// A FILLED issuer row must still be re-offered to the chain, against a real
// Postgres.
//
// Making `issuers.home_domain` overwritable was necessary and not sufficient.
// Nothing scheduled ever re-read a row that already had one: the drain's
// primary queue is `auth_required IS NULL`, so a row leaves it the moment it
// is filled, and its re-check queue covers only `last_known_before_removal`
// rows. `issuer-enrich`, the job whose whole purpose is to sync the column, is
// a manual one-shot with no timer. So an anchor that moved domain with
// SetOptions and let the old name lapse kept the lapsed name until an operator
// ran a backfill by hand — while the hourly SEP-1 refresh kept fetching it,
// and whoever registered it next could serve a stellar.toml listing the
// anchor's issuer account back and inherit its verified org identity.
//
// The queue is SQL, so none of this is checkable in Go. The last subtest
// closes the loop the correction exists to open: the SEP-1 refresh's own
// candidate query must go on to fetch the NEW domain.
func TestIssuersNeedingChainRecheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Ordered by primary key: lapsed < merged < unlabelled < unresolved.
	const (
		// Filled and labelled `live`, holding a domain the account no
		// longer declares. The population the finding is about.
		lapsedIssuer = "GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55"
		// Filled from a merged account's pre-image; the OTHER queue's job.
		mergedIssuer = "GAZPQOJ26IP24ECRXEZ4BE6BEIB4HNDWSA2E6JVPFIP6KO6BKOEAZ6XW"
		// Filled before migration 0153, so its provenance is NULL.
		unlabelledIssuer = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		// Never resolved at all; the PRIMARY queue's job, not this one.
		unresolvedIssuer = "GDM4RQUQQUVSKQA7S6EM7XBZP3FCGH4Q7CL6TABQ7B2BEJ5ERARM2M5M"
	)
	seedIssuers(t, ctx, store, []seedIssuer{
		{g: lapsedIssuer, homeDomain: "lapsed-former.example"},
		{g: mergedIssuer, homeDomain: "stellarbrunch.com"},
		{g: unlabelledIssuer, homeDomain: "aqua.network"},
		{g: unresolvedIssuer, homeDomain: "velo.org"},
	})

	asOf := uint32(64100000)
	removalLedger := uint32(54564588)
	seed := []timescale.IssuerAuthFlags{
		{
			GStrkey: lapsedIssuer, Required: true,
			HomeDomain: "lapsed-former.example",
			Source:     timescale.AuthFlagsSourceLive, AsOfLedger: &asOf,
		},
		{
			GStrkey: mergedIssuer, Revocable: true,
			Source: timescale.AuthFlagsSourceLastKnownBeforeRemoval, AsOfLedger: &removalLedger,
		},
		// Source "" leaves auth_flags_source NULL — the pre-0153 shape.
		{GStrkey: unlabelledIssuer, Clawback: true},
	}
	if _, err := store.PersistIssuerAuthFlags(ctx, seed); err != nil {
		t.Fatalf("seed persisted flags: %v", err)
	}

	homeDomainOfRow := func(t *testing.T, g string) string {
		t.Helper()
		var hd string
		if err := store.DB().QueryRowContext(ctx,
			`SELECT COALESCE(home_domain, '') FROM issuers WHERE g_strkey = $1`, g,
		).Scan(&hd); err != nil {
			t.Fatalf("read home_domain(%s): %v", g, err)
		}
		return hd
	}

	t.Run("the two one-shot queues cannot see a filled live row", func(t *testing.T) {
		// This is the gap, stated against the real statements rather than
		// asserted in prose: the row is in NEITHER of them.
		primary, err := store.IssuerGStrkeysNeedingFlags(ctx, 0)
		if err != nil {
			t.Fatalf("IssuerGStrkeysNeedingFlags: %v", err)
		}
		if slices.Contains(primary, lapsedIssuer) {
			t.Errorf("the primary queue offers %s; it is `auth_required IS NULL` and the row is filled", lapsedIssuer)
		}
		lastKnown, err := store.IssuerGStrkeysNeedingRecheck(ctx, 0)
		if err != nil {
			t.Fatalf("IssuerGStrkeysNeedingRecheck: %v", err)
		}
		if slices.Contains(lastKnown, lapsedIssuer) {
			t.Errorf("the last-known queue offers %s; it covers only merged accounts", lapsedIssuer)
		}
	})

	t.Run("the chain re-check queue offers exactly the filled non-merged rows", func(t *testing.T) {
		got, err := store.IssuersNeedingChainRecheck(ctx, 0)
		if err != nil {
			t.Fatalf("IssuersNeedingChainRecheck: %v", err)
		}
		want := []string{lapsedIssuer, unlabelledIssuer}
		if len(got) != len(want) {
			t.Fatalf("queue = %v, want exactly %v", strkeysOf(got), want)
		}
		for i, w := range want {
			if got[i].GStrkey != w {
				t.Fatalf("queue = %v, want %v in primary-key order", strkeysOf(got), want)
			}
		}

		// The VALUES on record have to come back, not just the keys: the
		// pass writes only the rows the chain disagrees with, and it cannot
		// tell agreement from disagreement without them.
		rec := got[0]
		if rec.HomeDomain != "lapsed-former.example" {
			t.Errorf("home_domain on record = %q, want lapsed-former.example", rec.HomeDomain)
		}
		if rec.Required == nil || !*rec.Required {
			t.Errorf("auth_required on record = %v, want true", rec.Required)
		}
		if rec.Revocable == nil || *rec.Revocable {
			t.Errorf("auth_revocable on record = %v, want false", rec.Revocable)
		}
		if rec.Source != timescale.AuthFlagsSourceLive {
			t.Errorf("source on record = %q, want %q", rec.Source, timescale.AuthFlagsSourceLive)
		}
		if rec.AsOfLedger == nil || *rec.AsOfLedger != asOf {
			t.Errorf("as-of on record = %v, want %d", rec.AsOfLedger, asOf)
		}

		// A pre-0153 row's provenance is UNKNOWN, and must not read as a
		// claim that the reading is current.
		if got[1].Source != "" {
			t.Errorf("unlabelled row's source = %q, want empty", got[1].Source)
		}
	})

	t.Run("a positive limit bounds the queue", func(t *testing.T) {
		got, err := store.IssuersNeedingChainRecheck(ctx, 1)
		if err != nil {
			t.Fatalf("IssuersNeedingChainRecheck: %v", err)
		}
		if len(got) != 1 || got[0].GStrkey != lapsedIssuer {
			t.Errorf("bounded queue = %v, want just %s — an unbounded pass would defeat the knob",
				strkeysOf(got), lapsedIssuer)
		}
	})

	t.Run("the chain's answer then corrects the row", func(t *testing.T) {
		fresh := uint32(64228661)
		n, err := store.PersistIssuerAuthFlags(ctx, []timescale.IssuerAuthFlags{{
			GStrkey: lapsedIssuer, Required: true,
			HomeDomain: "ultracapital.xyz",
			Source:     timescale.AuthFlagsSourceLive, AsOfLedger: &fresh,
		}})
		if err != nil {
			t.Fatalf("PersistIssuerAuthFlags: %v", err)
		}
		if n != 1 {
			t.Fatalf("changed %d row(s), want 1", n)
		}
		if got := homeDomainOfRow(t, lapsedIssuer); got != "ultracapital.xyz" {
			t.Errorf("home_domain = %q, want ultracapital.xyz — on-chain remediation has to reach a row "+
				"the drain already filled", got)
		}
	})

	t.Run("the corrected row leaves the queue unchanged in shape", func(t *testing.T) {
		// Still offered — a row is re-read every run, not retired once
		// corrected — and now carrying what the chain last said.
		got, err := store.IssuersNeedingChainRecheck(ctx, 0)
		if err != nil {
			t.Fatalf("IssuersNeedingChainRecheck: %v", err)
		}
		if len(got) != 2 || got[0].GStrkey != lapsedIssuer {
			t.Fatalf("queue = %v, want the corrected row still offered", strkeysOf(got))
		}
		if got[0].HomeDomain != "ultracapital.xyz" {
			t.Errorf("home_domain on record = %q, want ultracapital.xyz", got[0].HomeDomain)
		}
		if got[0].AsOfLedger == nil || *got[0].AsOfLedger != 64228661 {
			t.Errorf("as-of on record = %v, want 64228661", got[0].AsOfLedger)
		}
	})

	t.Run("the SEP-1 refresh queue then aims at the corrected domain", func(t *testing.T) {
		// The point of correcting the column: the job that fetches
		// attacker-authored documents must stop aiming at the lapsed name.
		candidates, err := store.IssuersNeedingSep1Refresh(ctx, 0, 100)
		if err != nil {
			t.Fatalf("IssuersNeedingSep1Refresh: %v", err)
		}
		got := map[string]string{}
		for _, c := range candidates {
			got[c.GStrkey] = c.HomeDomain
		}
		if got[lapsedIssuer] != "ultracapital.xyz" {
			t.Errorf("sep1 queue offers %s at %q, want ultracapital.xyz", lapsedIssuer, got[lapsedIssuer])
		}
	})

	t.Run("a merged row stays out of the queue after its own re-check", func(t *testing.T) {
		// The two queues partition the filled rows; if this one ever picked
		// up last-known rows they would be read from the lake twice a night.
		// A merged row leaves it once its persisted reading has cleared the
		// domain it held while live, which the seed persist above already did.
		got, err := store.IssuersNeedingChainRecheck(ctx, 0)
		if err != nil {
			t.Fatalf("IssuersNeedingChainRecheck: %v", err)
		}
		for _, rec := range got {
			if rec.GStrkey == mergedIssuer {
				t.Errorf("queue offers the merged row %s; the last-known queue already carries it", mergedIssuer)
			}
		}
		if got := homeDomainOfRow(t, mergedIssuer); got != "" {
			t.Errorf("merged row's home_domain = %q, want empty — a merged account's identity is not kept", got)
		}
	})
}

func strkeysOf(recs []timescale.IssuerAuthFlagsOnRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.GStrkey)
	}
	return out
}

// A SEP-1 payload is bound to the home_domain it was fetched from, against a
// real Postgres.
//
// The payload records no source domain and every reader pairs it with the
// CURRENT column, so a writer that moves home_domain and leaves the payload
// behind serves the old domain's org identity, logo and RWA attestations as
// the new domain's. Both writers are single SQL statements, so only an
// executing test can pin that they unbind it — and that a write which does
// NOT move the domain leaves the payload byte-identical.
func TestIssuerHomeDomainChangeUnbindsSep1Payload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		enriched = "GBFXOHVAS7DXHZPMPZL4HDPPMGSSJBWDGEOXSYHMPTSJKDFHPPFXFZ2K"
		drained  = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		steady   = "GA2PQOJ26IP24ECRXEZ4BE6BEIB4HNDWSA2E6JVPFIP6KO6BKOEAZ6XW"
	)
	seedIssuers(t, ctx, store, []seedIssuer{
		{g: enriched, homeDomain: "lapsed.example"},
		{g: drained, homeDomain: "lapsed.example"},
		{g: steady, homeDomain: "steady.example"},
	})
	// Each row holds a verified payload from its current domain and one
	// failed attempt on the ladder, so every column the reset owns is set.
	attackerPayload := func(g string) []byte {
		return []byte(`{"OrgName":"Attacker Org","OrgVerified":true,"Currencies":[{"Code":"TOK","Issuer":"` + g + `","AnchorAssetType":"fund"}]}`)
	}
	for g, domain := range map[string]string{enriched: "lapsed.example", drained: "lapsed.example", steady: "steady.example"} {
		if stored, err := store.SetIssuerSep1Payload(ctx, g, domain, attackerPayload(g)); err != nil || !stored {
			t.Fatalf("SetIssuerSep1Payload(%s) = (%v, %v), want (true, nil)", g, stored, err)
		}
		if _, err := store.MarkIssuerSep1Failed(ctx, g); err != nil {
			t.Fatalf("MarkIssuerSep1Failed(%s): %v", g, err)
		}
	}

	type sep1State struct {
		payload, resolved, fetched, next sql.NullString
		failures                         sql.NullInt64
	}
	stateOf := func(t *testing.T, g string) sep1State {
		t.Helper()
		var s sep1State
		if err := store.DB().QueryRowContext(ctx, `
			SELECT sep1_payload::text, sep1_resolved_at::text, sep1_payload_fetched_at::text,
			       sep1_next_attempt_after::text, sep1_consecutive_failures
			  FROM issuers WHERE g_strkey = $1`, g,
		).Scan(&s.payload, &s.resolved, &s.fetched, &s.next, &s.failures); err != nil {
			t.Fatalf("read sep1 state(%s): %v", g, err)
		}
		return s
	}
	assertUnbound := func(t *testing.T, g, wantDomain string) {
		t.Helper()
		s := stateOf(t, g)
		if s.payload.Valid || s.resolved.Valid || s.fetched.Valid || s.next.Valid {
			t.Errorf("%s: payload=%v resolved_at=%v fetched_at=%v next_attempt_after=%v, want all NULL — "+
				"the old domain's payload is still bound to the row", g, s.payload.Valid, s.resolved.Valid,
				s.fetched.Valid, s.next.Valid)
		}
		if !s.failures.Valid || s.failures.Int64 != 0 {
			t.Errorf("%s: sep1_consecutive_failures = %v, want 0", g, s.failures)
		}
		row, err := store.GetIssuer(ctx, g)
		if err != nil {
			t.Fatalf("GetIssuer(%s): %v", g, err)
		}
		if row.HomeDomain != wantDomain || row.OrgVerified || row.OrgName != "" {
			t.Errorf("%s: served home_domain=%q org_name=%q org_verified=%v, want %q with no org identity "+
				"until the new domain is fetched", g, row.HomeDomain, row.OrgName, row.OrgVerified, wantDomain)
		}
	}
	before := stateOf(t, steady)

	t.Run("the enrich job's domain move unbinds the payload", func(t *testing.T) {
		changed, err := store.SyncIssuerHomeDomain(ctx, enriched, "new-home.example")
		if err != nil || !changed {
			t.Fatalf("SyncIssuerHomeDomain = (%v, %v), want (true, nil)", changed, err)
		}
		assertUnbound(t, enriched, "new-home.example")
	})

	t.Run("the auth-flags drain's domain move unbinds the payload", func(t *testing.T) {
		asOf := uint32(64228661)
		if _, err := store.PersistIssuerAuthFlags(ctx, []timescale.IssuerAuthFlags{{
			GStrkey: drained, Required: true,
			Source: timescale.AuthFlagsSourceLive, AsOfLedger: &asOf,
			HomeDomain: "new-home.example",
		}}); err != nil {
			t.Fatalf("PersistIssuerAuthFlags: %v", err)
		}
		assertUnbound(t, drained, "new-home.example")
	})

	// The refresh reads a candidate, fetches for up to 30s, then writes. A move
	// that lands inside that window has already unbound the row; storing the
	// old domain's toml afterwards would bind it to the new one.
	t.Run("a payload fetched from the old domain does not land after a move", func(t *testing.T) {
		stored, err := store.SetIssuerSep1Payload(ctx, enriched, "lapsed.example", attackerPayload(enriched))
		if err != nil || stored {
			t.Fatalf("SetIssuerSep1Payload(fetchedFrom=lapsed.example) after the move = (%v, %v), want (false, nil)", stored, err)
		}
		assertUnbound(t, enriched, "new-home.example")
	})

	t.Run("a write that keeps the domain leaves the payload byte-identical", func(t *testing.T) {
		asOf := uint32(64228662)
		if _, err := store.PersistIssuerAuthFlags(ctx, []timescale.IssuerAuthFlags{{
			GStrkey: steady, Revocable: true,
			Source: timescale.AuthFlagsSourceLive, AsOfLedger: &asOf,
			HomeDomain: "steady.example",
		}}); err != nil {
			t.Fatalf("PersistIssuerAuthFlags: %v", err)
		}
		if changed, err := store.SyncIssuerHomeDomain(ctx, steady, "steady.example"); err != nil || changed {
			t.Fatalf("SyncIssuerHomeDomain on an agreeing row = (%v, %v), want (false, nil)", changed, err)
		}
		if after := stateOf(t, steady); after != before {
			t.Errorf("sep1 state moved on a write that kept the domain:\n before %+v\n after  %+v", before, after)
		}
	})

	t.Run("the moved rows head the refresh queue under their new domain", func(t *testing.T) {
		candidates, err := store.IssuersNeedingSep1Refresh(ctx, 24*time.Hour, 100)
		if err != nil {
			t.Fatalf("IssuersNeedingSep1Refresh: %v", err)
		}
		got := map[string]string{}
		for _, c := range candidates {
			got[c.GStrkey] = c.HomeDomain
		}
		for _, g := range []string{enriched, drained} {
			if got[g] != "new-home.example" {
				t.Errorf("sep1 queue offers %s at %q, want new-home.example now, not after the old domain's deferral", g, got[g])
			}
		}
		if _, ok := got[steady]; ok {
			t.Errorf("sep1 queue offers %s, whose domain did not move and whose deferral still holds", steady)
		}
	})
}

// issuers.home_domain must track the chain, against a
// real Postgres.
//
// Both writers of the column refused a row that already held a value: the
// enrich job would only write into a NULL-or-empty column, and the
// auth-flags drain COALESCEd the stored value ahead of the one it had just
// decoded. Each cited a SEP-1 resolver that is better sourced than the AccountEntry
// — a resolver that never writes this column; it READS it to pick its fetch
// target. So the column was write-once, and an anchor that moved domain
// on-chain and let the old name lapse could never take its identity back:
// the hourly SEP-1 refresh kept fetching the lapsed name, and whoever
// registered it next could serve a stellar.toml listing the anchor's issuer
// back and inherit its verified org identity.
//
// Both statements are SQL — one a WHERE clause, one a COALESCE — so neither
// can be tested in Go. The last subtest closes the loop the fix opens:
// after the correction, the SEP-1 refresh's own candidate query hands out
// the NEW domain, which is the whole point of correcting the column.
func TestIssuerHomeDomainTracksTheChain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The founding case: the ex-apay ETH issuer, whose on-chain domain moved
	// to ultracapital.xyz when Ultra Stellar acquired apay.io's wrapped
	// assets. `drained` stands in for a row the auth-flags drain has filled.
	const (
		anchor  = "GBFXOHVAS7DXHZPMPZL4HDPPMGSSJBWDGEOXSYHMPTSJKDFHPPFXFZ2K"
		drained = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		merged  = "GA2PQOJ26IP24ECRXEZ4BE6BEIB4HNDWSA2E6JVPFIP6KO6BKOEAZ6XW"
		// Accounts that ran SetOptions(home_domain="") and let the old name lapse.
		clearedOnChain = "GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55"
		clearedEnrich  = "GAEVD52W5E4Q2KTVQXC76ZSZYBEXXR3GGQZAIEP6BW3JMBTUBCHRY6UM"
	)
	seedIssuers(t, ctx, store, []seedIssuer{
		{g: anchor, homeDomain: "apay.io"},
		{g: drained, homeDomain: "lapsed-former.example"},
		{g: merged, homeDomain: "stellarbrunch.com"},
		{g: clearedOnChain, homeDomain: "lapsed-cleared.example"},
		{g: clearedEnrich, homeDomain: "lapsed-cleared.example"},
	})

	homeDomainOf := func(t *testing.T, g string) string {
		t.Helper()
		var hd string
		if err := store.DB().QueryRowContext(ctx,
			`SELECT COALESCE(home_domain, '') FROM issuers WHERE g_strkey = $1`, g,
		).Scan(&hd); err != nil {
			t.Fatalf("read home_domain(%s): %v", g, err)
		}
		return hd
	}

	t.Run("the enrich job overwrites a stale domain", func(t *testing.T) {
		changed, err := store.SyncIssuerHomeDomain(ctx, anchor, "ultracapital.xyz")
		if err != nil {
			t.Fatalf("SyncIssuerHomeDomain: %v", err)
		}
		if !changed {
			t.Error("SyncIssuerHomeDomain reported no change; want the row corrected")
		}
		if got := homeDomainOf(t, anchor); got != "ultracapital.xyz" {
			t.Errorf("home_domain = %q, want ultracapital.xyz — on-chain remediation must reach the column", got)
		}
	})

	t.Run("a re-run over an agreeing row changes nothing", func(t *testing.T) {
		changed, err := store.SyncIssuerHomeDomain(ctx, anchor, "ultracapital.xyz")
		if err != nil {
			t.Fatalf("SyncIssuerHomeDomain: %v", err)
		}
		if changed {
			t.Error("SyncIssuerHomeDomain reported a change on an agreeing row; the run's count would lie")
		}
	})

	t.Run("an empty domain never blanks a row", func(t *testing.T) {
		changed, err := store.SyncIssuerHomeDomain(ctx, anchor, "")
		if err != nil {
			t.Fatalf("SyncIssuerHomeDomain: %v", err)
		}
		if changed {
			t.Error("SyncIssuerHomeDomain reported a change for an empty domain")
		}
		if got := homeDomainOf(t, anchor); got != "ultracapital.xyz" {
			t.Errorf("home_domain = %q, want ultracapital.xyz — an empty value here means we did not "+
				"read it; a declared-none reading goes through ClearIssuerHomeDomain", got)
		}
	})

	t.Run("the auth-flags drain overwrites a stale domain", func(t *testing.T) {
		asOf := uint32(64228661)
		n, err := store.PersistIssuerAuthFlags(ctx, []timescale.IssuerAuthFlags{{
			GStrkey: drained, Required: true,
			Source: timescale.AuthFlagsSourceLive, AsOfLedger: &asOf,
			HomeDomain: "current-anchor.example",
		}})
		if err != nil {
			t.Fatalf("PersistIssuerAuthFlags: %v", err)
		}
		if n != 1 {
			t.Fatalf("changed %d row(s), want 1", n)
		}
		if got := homeDomainOf(t, drained); got != "current-anchor.example" {
			t.Errorf("home_domain = %q, want current-anchor.example — the live AccountEntry the drain "+
				"decoded outranks an older copy of the same field", got)
		}
	})

	t.Run("a merged account's reading clears the stored domain", func(t *testing.T) {
		asOf := uint32(54564588)
		// validate() refuses to WRITE a merged account's self-declared
		// domain because it can no longer be verified; keeping one stored
		// while the account was live is the same impersonation surface, and
		// the SEP-1 refresh would keep fetching it once it lapses.
		n, err := store.PersistIssuerAuthFlags(ctx, []timescale.IssuerAuthFlags{{
			GStrkey: merged, Revocable: true,
			Source: timescale.AuthFlagsSourceLastKnownBeforeRemoval, AsOfLedger: &asOf,
		}})
		if err != nil {
			t.Fatalf("PersistIssuerAuthFlags: %v", err)
		}
		if n != 1 {
			t.Fatalf("changed %d row(s), want 1", n)
		}
		if got := homeDomainOf(t, merged); got != "" {
			t.Errorf("home_domain = %q, want empty — a merged account's identity is not kept", got)
		}
	})

	t.Run("a live reading that declares no domain clears the stored one", func(t *testing.T) {
		asOf := uint32(64228700)
		if _, err := store.PersistIssuerAuthFlags(ctx, []timescale.IssuerAuthFlags{{
			GStrkey: clearedOnChain, Required: true,
			Source: timescale.AuthFlagsSourceLive, AsOfLedger: &asOf,
		}}); err != nil {
			t.Fatalf("PersistIssuerAuthFlags: %v", err)
		}
		if got := homeDomainOf(t, clearedOnChain); got != "" {
			t.Errorf("home_domain = %q, want empty — the live AccountEntry declares none", got)
		}
	})

	t.Run("the enrich job clears a domain the account no longer declares", func(t *testing.T) {
		changed, err := store.ClearIssuerHomeDomain(ctx, clearedEnrich)
		if err != nil || !changed {
			t.Fatalf("ClearIssuerHomeDomain = (%v, %v), want (true, nil)", changed, err)
		}
		if got := homeDomainOf(t, clearedEnrich); got != "" {
			t.Errorf("home_domain = %q, want empty", got)
		}
		if changed, err := store.ClearIssuerHomeDomain(ctx, clearedEnrich); err != nil || changed {
			t.Errorf("second ClearIssuerHomeDomain = (%v, %v), want (false, nil) on an agreeing row", changed, err)
		}
	})

	t.Run("an unlabelled empty reading still leaves the stored domain alone", func(t *testing.T) {
		if _, err := store.PersistIssuerAuthFlags(ctx, []timescale.IssuerAuthFlags{{
			GStrkey: anchor, Required: true,
		}}); err != nil {
			t.Fatalf("PersistIssuerAuthFlags: %v", err)
		}
		if got := homeDomainOf(t, anchor); got != "ultracapital.xyz" {
			t.Errorf("home_domain = %q, want ultracapital.xyz — nothing says what produced an unlabelled reading", got)
		}
	})

	t.Run("the SEP-1 refresh queue then hands out the corrected domain", func(t *testing.T) {
		// This is the loop the fix exists to close: correcting the column is
		// only worth anything if the job that fetches attacker-authored
		// documents stops aiming at the lapsed name.
		candidates, err := store.IssuersNeedingSep1Refresh(ctx, 0, 100)
		if err != nil {
			t.Fatalf("IssuersNeedingSep1Refresh: %v", err)
		}
		got := map[string]string{}
		for _, c := range candidates {
			got[c.GStrkey] = c.HomeDomain
		}
		if got[anchor] != "ultracapital.xyz" {
			t.Errorf("sep1 queue offers %s at %q, want ultracapital.xyz — the refresh would still "+
				"fetch the lapsed domain", anchor, got[anchor])
		}
		if got[drained] != "current-anchor.example" {
			t.Errorf("sep1 queue offers %s at %q, want current-anchor.example", drained, got[drained])
		}
		for _, g := range []string{merged, clearedOnChain, clearedEnrich} {
			if d, ok := got[g]; ok {
				t.Errorf("sep1 queue still offers %s at %q — a cleared or merged identity must stop being fetched", g, d)
			}
		}
	})

	t.Run("the chain re-check queue offers a merged row until its domain is cleared", func(t *testing.T) {
		const staleMerged = "GA2P3HKTQFBZBWOWPLESMV5AEHJLWJKNAYV5HLXOPBRWUEHFR64FQTKN"
		seedIssuers(t, ctx, store, []seedIssuer{{g: staleMerged, homeDomain: "xcrypto.exchange"}})
		// The shape rows written before the persist cleared a merged identity
		// are left in: labelled last-known, still holding the live domain.
		if _, err := store.DB().ExecContext(ctx, `
			UPDATE issuers SET auth_required = false, auth_revocable = true, auth_immutable = false,
			       auth_clawback = true, auth_flags_source = $2, auth_flags_as_of_ledger = 56082413
			 WHERE g_strkey = $1`, staleMerged, timescale.AuthFlagsSourceLastKnownBeforeRemoval); err != nil {
			t.Fatalf("seed stale merged row: %v", err)
		}
		offered := func() bool {
			recs, err := store.IssuersNeedingChainRecheck(ctx, 0)
			if err != nil {
				t.Fatalf("IssuersNeedingChainRecheck: %v", err)
			}
			for _, r := range recs {
				if r.GStrkey == staleMerged {
					return true
				}
			}
			return false
		}
		if !offered() {
			t.Fatalf("%s is not offered — a merged row holding a domain would keep it for good", staleMerged)
		}
		asOf := uint32(56082413)
		if _, err := store.PersistIssuerAuthFlags(ctx, []timescale.IssuerAuthFlags{{
			GStrkey: staleMerged, Revocable: true, Clawback: true,
			Source: timescale.AuthFlagsSourceLastKnownBeforeRemoval, AsOfLedger: &asOf,
		}}); err != nil {
			t.Fatalf("PersistIssuerAuthFlags: %v", err)
		}
		if offered() {
			t.Errorf("%s is still offered after its domain was cleared — the queues no longer partition", staleMerged)
		}
	})
}

// A flagged issuer's asset named by an unregistered SAC contract id has
// no issuer to look up; the scam gate recognises it through
// DirectoryScamFlaggedClassicAssets. This pins that query against real
// Timescale: only classic assets of scam-tagged G-accounts are listed,
// and the gate built over the real store withholds their SAC spelling.
func TestScamGate_SACIndexOverRealDirectory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	c.InstallAliasRegistry(nil)
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		flaggedIssuer = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
		cleanIssuer   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	)
	flaggedContract := "C" + dirAddress("FLAGGED")[1:] // only G-accounts issue classic assets
	mustReplaceDirectory(t, ctx, store, dirUpstreamSource, []timescale.DirectoryEntry{
		dirEntry(flaggedIssuer, "Rio Issuer", "issuer", "Malicious"),
		dirEntry(cleanIssuer, "Clean Issuer", "issuer"),
		dirEntry(flaggedContract, "Flagged Contract", "unsafe"),
	})

	now := time.Now().UTC()
	for _, a := range []struct{ code, issuer string }{
		{"RIO", flaggedIssuer},
		{"RIOX", flaggedIssuer},
		{"USDC", cleanIssuer},
	} {
		if _, err := store.DB().ExecContext(ctx, `
			INSERT INTO classic_assets
			    (asset_id, code, issuer_g_strkey, slug,
			     first_seen_at, first_seen_ledger, last_seen_at, last_seen_ledger,
			     observation_count)
			VALUES ($1, $2, $3, $4, $5, 1, $5, 100, 1)`,
			a.code+"-"+a.issuer, a.code, a.issuer, "sacidx-"+a.code, now); err != nil {
			t.Fatalf("insert classic asset %s: %v", a.code, err)
		}
	}

	listed, err := store.DirectoryScamFlaggedClassicAssets(ctx)
	if err != nil {
		t.Fatalf("DirectoryScamFlaggedClassicAssets: %v", err)
	}
	got := make([]string, 0, len(listed))
	for _, a := range listed {
		if a.Type != c.AssetClassic {
			t.Errorf("listed %v with type %q, want classic", a, a.Type)
		}
		got = append(got, a.Code+"-"+a.Issuer)
	}
	slices.Sort(got)
	want := []string{"RIO-" + flaggedIssuer, "RIOX-" + flaggedIssuer}
	if !slices.Equal(got, want) {
		t.Fatalf("flagged classic assets = %v, want %v (mixed-case tag must match; clean issuer excluded)", got, want)
	}

	gate := pricingguard.NewScamGate(store, pricingguard.ScamGateOptions{})
	sacOf := func(code, issuer string) c.Asset {
		classic, err := c.NewClassicAsset(code, issuer)
		if err != nil {
			t.Fatalf("classic %s: %v", code, err)
		}
		cid, err := classic.SacContractID()
		if err != nil {
			t.Fatalf("derive SAC %s: %v", code, err)
		}
		sac, err := c.NewSorobanAsset(cid)
		if err != nil {
			t.Fatalf("soroban asset %s: %v", cid, err)
		}
		return sac
	}
	if !gate.WithheldPair(ctx, sacOf("RIOX", flaggedIssuer), c.NativeAsset(), "price_read") {
		t.Error("flagged issuer's unregistered SAC was served over the real store")
	}
	if gate.WithheldPair(ctx, sacOf("USDC", cleanIssuer), c.NativeAsset(), "price_read") {
		t.Error("clean issuer's SAC was withheld over the real store")
	}
}

// The SEP-1 attestation age bound against a real Postgres (migration 0178).
//
// The bound lives in BoundSep1Currencies's SQL and in which writer stamps
// sep1_payload_fetched_at, and the backfill is an UPDATE in the migration;
// none of the three exists in Go to unit-test. The failure being pinned: a
// domain that goes dark keeps its last payload, every failed attempt stamps
// sep1_resolved_at fresh, and the RWA scan kept admitting from it forever.
func TestSep1AttestationAge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 177)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		healthy   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		streaking = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		dead      = "GA2PQOJ26IP24ECRXEZ4BE6BEIB4HNDWSA2E6JVPFIP6KO6BKOEAZ6XW"
	)
	seedIssuers(t, ctx, store, []seedIssuer{
		{g: healthy, homeDomain: "centre.io"},
		{g: streaking, homeDomain: "litemint.store"},
		{g: dead, homeDomain: "coinonstellar.com"},
	})

	// Pre-0178 state: a payload last touched by a success (no streak), and
	// one whose domain has been failing since it was fetched.
	_, err = store.DB().ExecContext(ctx, `
        UPDATE issuers SET sep1_payload = $2::jsonb,
                           sep1_resolved_at = NOW() - INTERVAL '2 days',
                           sep1_consecutive_failures = 0
         WHERE g_strkey = $1`, healthy, sep1BondPayload(healthy))
	if err != nil {
		t.Fatalf("seed healthy payload: %v", err)
	}
	_, err = store.DB().ExecContext(ctx, `
        UPDATE issuers SET sep1_payload = $2::jsonb,
                           sep1_resolved_at = NOW(),
                           sep1_consecutive_failures = 4
         WHERE g_strkey = $1`, streaking, sep1BondPayload(streaking))
	if err != nil {
		t.Fatalf("seed streaking payload: %v", err)
	}

	applyMigrationsUpTo(t, dsn, 178)

	t.Run("backfill stamps only what a success last touched", func(t *testing.T) {
		fetched, resolved := sep1PayloadTimes(t, ctx, store, healthy)
		if !fetched.Valid || !fetched.Time.Equal(resolved.Time) {
			t.Errorf("healthy fetched_at = %v, want its sep1_resolved_at %v", fetched, resolved)
		}
		if fetched, _ := sep1PayloadTimes(t, ctx, store, streaking); fetched.Valid {
			t.Errorf("streaking fetched_at = %v, want NULL — a failure last stamped sep1_resolved_at, "+
				"so it is not the payload's fetch time", fetched.Time)
		}
	})

	t.Run("a failed attempt does not renew the payload's age", func(t *testing.T) {
		if stored, err := store.SetIssuerSep1Payload(ctx, dead, "coinonstellar.com", []byte(sep1BondPayload(dead))); err != nil || !stored {
			t.Fatalf("SetIssuerSep1Payload = (%v, %v), want (true, nil)", stored, err)
		}
		if got := boundIssuers(t, ctx, store); !got[dead] {
			t.Fatalf("a payload fetched just now is not admitted: %v", got)
		}
		_, err := store.DB().ExecContext(ctx, `
            UPDATE issuers SET sep1_payload_fetched_at = NOW() - $2::interval
             WHERE g_strkey = $1`, dead, "31 days")
		if err != nil {
			t.Fatalf("age payload: %v", err)
		}
		if _, err := store.MarkIssuerSep1Failed(ctx, dead); err != nil {
			t.Fatalf("MarkIssuerSep1Failed: %v", err)
		}
		fetched, resolved := sep1PayloadTimes(t, ctx, store, dead)
		if time.Since(fetched.Time) < 30*24*time.Hour {
			t.Errorf("fetched_at = %v after a failure, want it left 31 days old", fetched.Time)
		}
		if time.Since(resolved.Time) > time.Hour {
			t.Errorf("sep1_resolved_at = %v, want the failure's fresh stamp", resolved.Time)
		}
	})

	t.Run("only fresh payloads are read, and the rest are counted", func(t *testing.T) {
		got, census := boundScan(t, ctx, store)
		if !got[healthy] {
			t.Errorf("healthy (fetched 2 days ago) not admitted: %v", got)
		}
		if got[dead] {
			t.Error("dead (payload 31 days old, attempt just failed) still admitted — a dark domain attests forever")
		}
		if got[streaking] {
			t.Error("streaking (fetch time unknown) admitted — an unrecorded age must read as stale")
		}
		if census.IssuersWithPayload != 3 || census.IssuersPayloadStale != 2 {
			t.Errorf("census = %+v, want 3 payloads walked and 2 counted stale", census)
		}
		if why := census.Check(); why != "" {
			t.Errorf("census does not balance: %s", why)
		}
	})

	t.Run("a success restores it", func(t *testing.T) {
		if stored, err := store.SetIssuerSep1Payload(ctx, dead, "coinonstellar.com", []byte(sep1BondPayload(dead))); err != nil || !stored {
			t.Fatalf("SetIssuerSep1Payload = (%v, %v), want (true, nil)", stored, err)
		}
		if got := boundIssuers(t, ctx, store); !got[dead] {
			t.Errorf("a recovered domain is still refused: %v", got)
		}
	})
}

func sep1BondPayload(issuer string) string {
	return `{"OrgName":"Test Org","Currencies":[{"Code":"BOND","Issuer":"` + issuer +
		`","AnchorAssetType":"bond","AnchorAsset":"US Treasury Notes"}]}`
}

func sep1PayloadTimes(t *testing.T, ctx context.Context, store *timescale.Store, gStrkey string) (fetched, resolved sql.NullTime) {
	t.Helper()
	err := store.DB().QueryRowContext(ctx,
		`SELECT sep1_payload_fetched_at, sep1_resolved_at FROM issuers WHERE g_strkey = $1`, gStrkey,
	).Scan(&fetched, &resolved)
	if err != nil {
		t.Fatalf("read sep1 times for %s: %v", gStrkey, err)
	}
	return fetched, resolved
}

func boundScan(t *testing.T, ctx context.Context, store *timescale.Store) (map[string]bool, timescale.Sep1BoundCensus) {
	t.Helper()
	bound, census, err := store.BoundSep1Currencies(ctx, nil)
	if err != nil {
		t.Fatalf("BoundSep1Currencies: %v", err)
	}
	out := make(map[string]bool, len(bound))
	for _, b := range bound {
		out[b.Issuer] = true
	}
	return out, census
}

func boundIssuers(t *testing.T, ctx context.Context, store *timescale.Store) map[string]bool {
	t.Helper()
	got, _ := boundScan(t, ctx, store)
	return got
}

// SEP-1 identity change detection and freshness against a real Postgres
// (migration 0190). The history rows are written inside
// SetIssuerSep1Payload's transaction and the freshness verdict is SQL in
// data-freshness.sh; neither exists in Go to unit-test.
func TestSep1IdentityHistoryAndFreshness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	const g = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	seedIssuers(t, ctx, store, []seedIssuer{{g: g, homeDomain: "anchor.example"}})

	write := func(payload string) {
		t.Helper()
		if stored, err := store.SetIssuerSep1Payload(ctx, g, "anchor.example", []byte(payload)); err != nil || !stored {
			t.Fatalf("SetIssuerSep1Payload = (%v, %v), want (true, nil)", stored, err)
		}
	}
	historyRows := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM issuer_identity_history WHERE g_strkey = $1`, g).Scan(&n); err != nil {
			t.Fatalf("count history: %v", err)
		}
		return n
	}

	original := `{"OrgName":"Anchor","Documentation":{"ORG_URL":"https://anchor.example"},
		"Currencies":[{"Code":"USDX","Issuer":"` + g + `","Image":"https://anchor.example/usdx.png"}]}`
	write(original)
	if n := historyRows(); n != 0 {
		t.Fatalf("first fetch wrote %d history rows, want 0 — nothing was established yet", n)
	}
	write(original)
	if n := historyRows(); n != 0 {
		t.Fatalf("an unchanged refresh wrote %d history rows, want 0", n)
	}

	write(`{"OrgName":"Anchor","Documentation":{"ORG_URL":"https://evil.example"},
		"Currencies":[{"Code":"USDX","Issuer":"` + g + `","Image":"https://evil.example/usdx.png"}]}`)
	rows, err := db.QueryContext(ctx, `
        SELECT field, COALESCE(currency, ''), COALESCE(old_value, ''), COALESCE(new_value, '')
          FROM issuer_identity_history WHERE g_strkey = $1 ORDER BY field`, g)
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	defer rows.Close()
	var got [][4]string
	for rows.Next() {
		var r [4]string
		if err := rows.Scan(&r[0], &r[1], &r[2], &r[3]); err != nil {
			t.Fatalf("scan history: %v", err)
		}
		got = append(got, r)
	}
	want := [][4]string{
		{"currency_image", "USDX-" + g, "https://anchor.example/usdx.png", "https://evil.example/usdx.png"},
		{"org_url", "", "https://anchor.example", "https://evil.example"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("identity history = %v, want %v — the previous identity must be recoverable", got, want)
	}

	// A domain that last served a payload 40 days ago and whose every
	// attempt since failed: failures stamp sep1_resolved_at, so only the
	// payload's own fetch time can show the staleness.
	if _, err := db.ExecContext(ctx, `
        UPDATE issuers SET sep1_payload_fetched_at = NOW() - INTERVAL '40 days',
                           sep1_resolved_at = NOW(),
                           sep1_consecutive_failures = 6
         WHERE g_strkey = $1`, g); err != nil {
		t.Fatalf("age payload: %v", err)
	}
	samples := runFreshnessDomainQueries(t, ctx, db)
	key := `stellarindex_data_freshness_stale{domain="sep1",source="issuers"}`
	if v := samples[key]; v != "1" {
		t.Errorf("%s = %q, want 1 — no payload fetched in 40 days, yet a failing refresh read as fresh", key, v)
	}
}

// AllSep1Images against a real Postgres.
//
// Every claim here needs the database. The scan is a LATERAL over
// jsonb_array_elements, which raises 22023 on anything that is not an
// array — and one such row fails the whole statement, blanking the logo
// map for every issuer. None of that exists in Go to unit-test, so the
// only honest test is one that EXECUTES the query against the shapes an
// attacker-authored stellar.toml can actually put in the column.
//
// What this pins is the OUTCOME: the right rows, and no error, across the
// whole hostile set in one pass. It does NOT distinguish the query's CASE
// guard from a plain WHERE guard — measured on this image the planner
// pushes either below the lateral. The CASE is there because that is a
// property of the plan rather than of the query; see allSep1ImagesQuery.
//
// It also re-runs the brand hijack through the
// real SQL. The provenance rule lives in Go (timescale.sep1ImageFrom, unit
// tested), but "the rule is applied to what the query actually returns" is
// a claim about the two together.
//
// Production opens the store with timescale.Open and wires it into
// v1.New(v1.Options{Sep1Cache: store}) (cmd/stellarindex-api/main.go); this
// test uses that same constructor.
func TestAllSep1ImagesProjection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		circle   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		attacker = "GAS4V4XZ3JHFTGKHCTMTWIIVFHGLBUMSQMGZM4RIWYPQHNXBAOGZBKHR"
		junk     = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		noToml   = "GD6VWBXI6NY18X5RJVSCQBRQOFB5PVRAPKQ6IMDG5UAXNHMLRPQ7LTKO"
	)

	// Every payload shape reachable from a hostile or broken stellar.toml,
	// in ONE row set — the query must survive all of them in a single pass,
	// which is the property a per-shape unit test cannot establish.
	seedIssuers(t, ctx, store, []seedIssuer{
		{g: circle, homeDomain: "centre.io"},
		{g: attacker, homeDomain: "attacker.example"},
		{g: junk, homeDomain: "junk.example"},
		{g: noToml, homeDomain: "nothing.example"},
	})

	setPayload := func(g, payload string) {
		t.Helper()
		if _, uerr := store.DB().ExecContext(ctx,
			`UPDATE issuers SET sep1_payload = $2::jsonb WHERE g_strkey = $1`, g, payload,
		); uerr != nil {
			t.Fatalf("set payload for %s: %v", g, uerr)
		}
	}

	setPayload(circle, `{"OrgName":"Circle","Currencies":[
		{"Code":"USDC","Issuer":"`+circle+`","Image":"https://circle.com/usdc.svg"}
	]}`)

	// The hijack: the attacker's own TOML declares BOTH its own asset and
	// Circle's. Only its own may survive.
	setPayload(attacker, `{"OrgName":"Totally Circle","Currencies":[
		{"Code":"SCAM","Issuer":"`+attacker+`","Image":"https://attacker.example/scam.png"},
		{"Code":"USDC","Issuer":"`+circle+`","Image":"https://attacker.example/usdc.png"}
	]}`)

	// Shapes that must yield nothing AND must not error the whole scan.
	// A single 22023 here blanks the logo map for every issuer.
	setPayload(junk, `{"Currencies":[1,"two",null,true,
		{"Code":"NOIMG","Issuer":"`+junk+`"},
		{"Code":"EMPTY","Issuer":"`+junk+`","Image":""},
		{"Code":"NULLIMG","Issuer":"`+junk+`","Image":null},
		{"Code":"OBJIMG","Issuer":"`+junk+`","Image":{"nested":1}},
		{"Issuer":"`+junk+`","Image":"https://junk.example/nocode.png"},
		{"Code":"NOISS","Image":"https://junk.example/noissuer.png"},
		{"Code":"LOWER","issuer":"`+junk+`","image":"https://junk.example/lower.png"}
	]}`)

	for name, payload := range map[string]string{
		"no Currencies key":   `{"OrgName":"x"}`,
		"Currencies null":     `{"Currencies":null}`,
		"Currencies object":   `{"Currencies":{"Code":"X","Image":"https://e/x.png"}}`,
		"Currencies string":   `{"Currencies":"nope"}`,
		"Currencies empty":    `{"Currencies":[]}`,
		"payload is array":    `[1,2,3]`,
		"payload is scalar":   `"just a string"`,
		"payload is a numbr":  `42`,
		"payload is null-ish": `{"Currencies":[{"Code":null,"Issuer":null,"Image":null}]}`,
	} {
		setPayload(noToml, payload)
		got, gerr := store.AllSep1Images(ctx)
		if gerr != nil {
			t.Fatalf("%s: AllSep1Images errored — one issuer's payload blanked the whole map: %v", name, gerr)
		}
		// The two healthy issuers must be unaffected by the junk beside them.
		if len(got) != 2 {
			t.Errorf("%s: got %d images %+v, want 2 (Circle's USDC + the attacker's own SCAM)", name, len(got), got)
		}
	}

	// Back to a payload of its own, then assert the whole result exactly.
	setPayload(noToml, `{"Currencies":[]}`)
	got, err := store.AllSep1Images(ctx)
	if err != nil {
		t.Fatalf("AllSep1Images: %v", err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].Code < got[j].Code })

	want := []timescale.Sep1Image{
		{Code: "SCAM", Issuer: attacker, Image: "https://attacker.example/scam.png"},
		{Code: "USDC", Issuer: circle, Image: "https://circle.com/usdc.svg"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d images %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("image %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, img := range got {
		if img.Code == "USDC" && img.Issuer == circle && img.Image != "https://circle.com/usdc.svg" {
			t.Errorf("USDC's logo was served from a hostile TOML: %q — brand hijack", img.Image)
		}
	}
}

// TestSep1PayloadOutlivedDomain pins the one predicate every SEP-1 identity
// read shares: a held payload stops vouching for an issuer once it is past
// Sep1AttestationMaxAge, or of unrecorded age, AND its domain is failing now.
// Neither condition alone may downgrade it.
func TestSep1PayloadOutlivedDomain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	old := time.Now().Add(-timescale.Sep1AttestationMaxAge - time.Hour)
	fresh := time.Now().Add(-time.Hour)
	cases := []struct {
		name      string
		g         string
		fetchedAt *time.Time
		failures  int
		outlived  bool
	}{
		{"old payload, domain failing", "GA2P3HKTQFBZBWOWPLESMV5AEHJLWJKNAYV5HLXOPBRWUEHFR64FQTKN", &old, 5, true},
		{"unrecorded age, domain failing", "GA2PQOJ26IP24ECRXEZ4BE6BEIB4HNDWSA2E6JVPFIP6KO6BKOEAZ6XW", nil, 1, true},
		{"old payload, domain not failing", "GA2PZMWITS45LSQF7KWN7SH2YREIUHLC4SNNIYX5D2LTNEML74CANJMO", &old, 0, false},
		{"fresh payload, domain failing", "GA2QXW7YFAIR35LGKM2TDCQQZFR33XJCWF4N6SMRLOKX3HL76JKKPA62", &fresh, 3, false},
	}

	for i, c := range cases {
		seedIssuers(t, ctx, store, []seedIssuer{{g: c.g, homeDomain: "anchor.example"}})
		seedClassicAssets(t, ctx, store, []seedAsset{{
			assetID: "USDX-" + c.g, code: "USDX", issuer: c.g, slug: "usdx-" + c.g[:8], obs: int64(100 - i),
		}})
		payload := `{"OrgName":"Anchor","OrgVerified":true,
			"Currencies":[{"Code":"USDX","Issuer":"` + c.g + `","Image":"https://anchor.example/usdx.png"}]}`
		if _, err := store.DB().ExecContext(ctx, `
            UPDATE issuers
               SET sep1_payload = $2::jsonb,
                   sep1_payload_fetched_at = $3,
                   sep1_consecutive_failures = $4
             WHERE g_strkey = $1`, c.g, payload, c.fetchedAt, c.failures); err != nil {
			t.Fatalf("%s: seed payload: %v", c.name, err)
		}
	}

	images, err := store.AllSep1Images(ctx)
	if err != nil {
		t.Fatalf("AllSep1Images: %v", err)
	}
	imaged := map[string]bool{}
	for _, img := range images {
		imaged[img.Issuer] = true
	}
	listed, err := store.ListIssuers(ctx, 10)
	if err != nil {
		t.Fatalf("ListIssuers: %v", err)
	}
	listVerified := map[string]bool{}
	for _, r := range listed {
		listVerified[r.GStrkey] = r.OrgVerified
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sep, err := store.GetIssuerSep1Cached(ctx, c.g)
			if err != nil || sep == nil {
				t.Fatalf("GetIssuerSep1Cached = (%v, %v)", sep, err)
			}
			if sep.OutlivedDomain != c.outlived {
				t.Errorf("GetIssuerSep1Cached OutlivedDomain = %v, want %v", sep.OutlivedDomain, c.outlived)
			}
			row, err := store.GetIssuer(ctx, c.g)
			if err != nil {
				t.Fatalf("GetIssuer: %v", err)
			}
			if row.OrgVerified == c.outlived {
				t.Errorf("GetIssuer org_verified = %v, want %v", row.OrgVerified, !c.outlived)
			}
			if listVerified[c.g] == c.outlived {
				t.Errorf("ListIssuers org_verified = %v, want %v", listVerified[c.g], !c.outlived)
			}
			if imaged[c.g] == c.outlived {
				t.Errorf("AllSep1Images served image = %v, want %v", imaged[c.g], !c.outlived)
			}
		})
	}
}

// SEP-1 retry-ladder round-trip against a real Postgres (migration 0159).
//
// Every claim here needs the database. The ladder is computed inside the
// UPDATE (`$2::interval * POWER(2, …)` clamped by `$3::interval`) so that the
// new failure count and the deferral derived from it cannot disagree, and the
// queue's new arm is a column predicate — neither exists in Go to unit-test.
// An untyped bind parameter beside an interval operator also compiles and
// reviews perfectly while raising 42883 on every call at runtime, so the only
// honest test of this SQL is one that EXECUTES it.
//
// Production opens the store with timescale.Open (internal/ops/ingest/
// sep1_refresh.go, `store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)`)
// and this test uses that same constructor — not OpenServing/OpenBackground,
// which wrap it with a statement timeout the cron does not set.
func TestSep1RetryBackoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Real r1 residue. coinonstellar.com is NXDOMAIN;
	// centre.io is the Circle toml the USDC issuer resolves through;
	// litemint.store fronts a large family of parked subdomains.
	const (
		dead      = "GA2PQOJ26IP24ECRXEZ4BE6BEIB4HNDWSA2E6JVPFIP6KO6BKOEAZ6XW"
		healthy   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		recovered = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	)
	seedIssuers(t, ctx, store, []seedIssuer{
		{g: dead, homeDomain: "coinonstellar.com"},
		{g: healthy, homeDomain: "centre.io"},
		{g: recovered, homeDomain: "litemint.store"},
	})

	t.Run("the ladder doubles and then caps", func(t *testing.T) {
		// 1d, 2d, 4d, 8d, 16d, then the 30d cap — NOT 32d. The first step
		// is deliberately the cadence a healthy domain already gets, so a
		// domain's first failure costs it nothing.
		want := []time.Duration{
			24 * time.Hour,
			2 * 24 * time.Hour,
			4 * 24 * time.Hour,
			8 * 24 * time.Hour,
			16 * 24 * time.Hour,
			30 * 24 * time.Hour,
			30 * 24 * time.Hour,
		}
		for i, wantDefer := range want {
			streak, ferr := store.MarkIssuerSep1Failed(ctx, dead)
			if ferr != nil {
				t.Fatalf("MarkIssuerSep1Failed #%d: %v", i+1, ferr)
			}
			if streak != i+1 {
				t.Fatalf("failure #%d returned streak %d, want %d", i+1, streak, i+1)
			}
			gotDefer := sep1Deferral(t, ctx, store, dead)
			if gotDefer != wantDefer {
				t.Errorf("failure #%d deferred by %v, want %v", i+1, gotDefer, wantDefer)
			}
		}
	})

	t.Run("a repeatedly-failing domain is not selected while a healthy one is", func(t *testing.T) {
		// This is the defect. Both rows are equally stale — 40 days since
		// the last attempt, well past `-older-than 24h` — and before the
		// ladder both were selected, with the dead one AHEAD of the healthy
		// one whenever its sep1_resolved_at was the older of the two. The
		// only thing that may keep it out now is its own deferral.
		ageSep1ResolvedAt(t, ctx, store, dead, 40*24*time.Hour)
		ageSep1ResolvedAt(t, ctx, store, healthy, 40*24*time.Hour)
		ageSep1ResolvedAt(t, ctx, store, recovered, 40*24*time.Hour)

		got := candidateSet(t, ctx, store, 24*time.Hour, 100)
		if got[dead] {
			t.Errorf("the 7x-failed domain is still a candidate — the deferral is not being applied")
		}
		if !got[healthy] {
			t.Errorf("the never-failed domain is NOT a candidate; want it selected every cadence")
		}
	})

	t.Run("a success clears the ladder and restores the fast cadence", func(t *testing.T) {
		// Three failures first: enough to earn a 4-day deferral, so the
		// recovery has something real to undo.
		for i := 0; i < 3; i++ {
			if _, ferr := store.MarkIssuerSep1Failed(ctx, recovered); ferr != nil {
				t.Fatalf("MarkIssuerSep1Failed: %v", ferr)
			}
		}
		ageSep1ResolvedAt(t, ctx, store, recovered, 40*24*time.Hour)
		if candidateSet(t, ctx, store, 24*time.Hour, 100)[recovered] {
			t.Fatalf("a 3x-failed domain is still a candidate; the rest of this case proves nothing")
		}

		if stored, err := store.SetIssuerSep1Payload(ctx, recovered, "litemint.store",
			[]byte(`{"OrgName":"Litemint","OrgVerified":false}`)); err != nil || !stored {
			t.Fatalf("SetIssuerSep1Payload = (%v, %v), want (true, nil)", stored, err)
		}
		failures, next := sep1Ladder(t, ctx, store, recovered)
		if !failures.Valid || failures.Int64 != 0 {
			t.Errorf("after a success sep1_consecutive_failures = %v, want 0", failures)
		}
		if next.Valid {
			t.Errorf("after a success sep1_next_attempt_after = %v, want NULL — a recovering domain "+
				"must not serve out the deferral it earned while it was dead", next.Time)
		}

		ageSep1ResolvedAt(t, ctx, store, recovered, 40*24*time.Hour)
		if !candidateSet(t, ctx, store, 24*time.Hour, 100)[recovered] {
			t.Errorf("a recovered domain is not back in the queue")
		}
	})

	t.Run("a payload the database refuses is the document's fault", func(t *testing.T) {
		// jsonb cannot hold U+0000 (22P05), and any account can publish a
		// toml that decodes to one. The refresh keeps that ladder step, so
		// the store must tell it apart from an outage on our side.
		_, err := store.SetIssuerSep1Payload(ctx, dead, "coinonstellar.com", []byte(`{"OrgName":"a\u0000b"}`))
		if !errors.Is(err, timescale.ErrSep1PayloadRejected) {
			t.Errorf("SetIssuerSep1Payload(NUL) err = %v; want ErrSep1PayloadRejected", err)
		}
	})

	t.Run("candidates report whether the domain has served a payload", func(t *testing.T) {
		// The systemic verdict counts only reached domains; a never-reached
		// one reading as reached would re-arm the testnet false alarm.
		ageSep1ResolvedAt(t, ctx, store, healthy, 40*24*time.Hour)
		ageSep1ResolvedAt(t, ctx, store, recovered, 40*24*time.Hour)
		got, err := store.IssuersNeedingSep1Refresh(ctx, 24*time.Hour, 100)
		if err != nil {
			t.Fatalf("IssuersNeedingSep1Refresh: %v", err)
		}
		reached := map[string]bool{}
		for _, c := range got {
			reached[c.GStrkey] = c.Reached
		}
		if r, ok := reached[recovered]; !ok || !r {
			t.Errorf("queue: recovered (holds a payload) Reached = %v (present %v), want true", r, ok)
		}
		if r, ok := reached[healthy]; !ok || r {
			t.Errorf("queue: healthy (no payload yet) Reached = %v (present %v), want false", r, ok)
		}
		for g, want := range map[string]bool{recovered: true, dead: false} {
			c, cerr := store.IssuerSep1CandidateByStrkey(ctx, g)
			if cerr != nil || c.Reached != want {
				t.Errorf("IssuerSep1CandidateByStrkey(%s) = (Reached %v, %v), want Reached %v", g, c.Reached, cerr, want)
			}
		}
	})

	t.Run("unwinding a run takes back exactly one step", func(t *testing.T) {
		// The systemic-outage escape hatch. Everything the run failed gets
		// its ladder step back and its deferral lifted, so a night when our
		// DNS was broken leaves no trace on the schedule.
		if _, ferr := store.MarkIssuerSep1Failed(ctx, healthy); ferr != nil {
			t.Fatalf("MarkIssuerSep1Failed: %v", ferr)
		}
		if u, err := store.IssuerSep1Unreachable(ctx, healthy); err != nil || !u {
			t.Fatalf("IssuerSep1Unreachable after a failure = (%v, %v), want (true, nil)", u, err)
		}
		_, failing := boundScan(t, ctx, store)
		ageSep1ResolvedAt(t, ctx, store, healthy, 40*24*time.Hour)
		if candidateSet(t, ctx, store, 24*time.Hour, 100)[healthy] {
			t.Fatalf("a once-failed domain is still a candidate; the rest of this case proves nothing")
		}

		n, uerr := store.UnwindIssuerSep1Backoff(ctx, []string{healthy})
		if uerr != nil {
			t.Fatalf("UnwindIssuerSep1Backoff: %v", uerr)
		}
		if n != 1 {
			t.Errorf("unwound %d row(s), want 1", n)
		}
		failures, next := sep1Ladder(t, ctx, store, healthy)
		if !failures.Valid || failures.Int64 != 0 {
			t.Errorf("sep1_consecutive_failures = %v, want the pre-run 0", failures)
		}
		if next.Valid {
			t.Errorf("sep1_next_attempt_after = %v, want NULL", next.Time)
		}
		if !candidateSet(t, ctx, store, 24*time.Hour, 100)[healthy] {
			t.Errorf("an unwound domain is not back in the queue")
		}
		// sep1_resolved_at keeps its stamp through the unwind, so it cannot
		// be what tells an outage on our side from the issuer's failure.
		if u, err := store.IssuerSep1Unreachable(ctx, healthy); err != nil || u {
			t.Errorf("IssuerSep1Unreachable after the unwind = (%v, %v), want (false, nil): "+
				"the only failure was ours", u, err)
		}
		if _, unwound := boundScan(t, ctx, store); failing.IssuersFetchedWithoutPayload-unwound.IssuersFetchedWithoutPayload != 1 {
			t.Errorf("census served-nothing count %d -> %d across the unwind, want it to drop by exactly 1",
				failing.IssuersFetchedWithoutPayload, unwound.IssuersFetchedWithoutPayload)
		}

		// Unwinding a row that never failed must floor at zero rather than
		// trip issuers_sep1_consecutive_failures_check.
		if _, uerr = store.UnwindIssuerSep1Backoff(ctx, []string{healthy}); uerr != nil {
			t.Errorf("unwinding an already-zero row: %v, want it to floor at 0", uerr)
		}
		// And an empty batch must not build `= ANY('{}')` for nothing.
		if n, uerr = store.UnwindIssuerSep1Backoff(ctx, nil); uerr != nil || n != 0 {
			t.Errorf("UnwindIssuerSep1Backoff(nil) = %d, %v; want 0, nil", n, uerr)
		}
	})

	t.Run("an over-range limit clamps to the ceiling, not to 100", func(t *testing.T) {
		// Resetting ANY out-of-range limit to 100 would give an operator
		// raising LIMIT past the cap silently a fraction of the intended
		// budget. It reads as "the job is slow", never as "your setting was
		// rejected" — precisely the shape of bug this ladder exists to
		// remove elsewhere.
		const extra = 150
		rows := make([]seedIssuer, 0, extra)
		for i := 0; i < extra; i++ {
			rows = append(rows, seedIssuer{
				g:          fmt.Sprintf("GBULK%051d", i),
				homeDomain: fmt.Sprintf("bulk-%03d.example", i),
			})
		}
		seedIssuers(t, ctx, store, rows)

		got, qerr := store.IssuersNeedingSep1Refresh(ctx, 0, 999_999)
		if qerr != nil {
			t.Fatalf("IssuersNeedingSep1Refresh: %v", qerr)
		}
		if len(got) <= 100 {
			t.Errorf("an over-range limit returned %d row(s) out of %d eligible — it is still being "+
				"snapped down to the old 100 default", len(got), extra)
		}
	})
}

// sep1Ladder reads the two migration-0159 columns straight from the row.
func sep1Ladder(t *testing.T, ctx context.Context, store *timescale.Store, gStrkey string) (sql.NullInt64, sql.NullTime) {
	t.Helper()
	var (
		failures sql.NullInt64
		next     sql.NullTime
	)
	err := store.DB().QueryRowContext(ctx,
		`SELECT sep1_consecutive_failures, sep1_next_attempt_after FROM issuers WHERE g_strkey = $1`,
		gStrkey,
	).Scan(&failures, &next)
	if err != nil {
		t.Fatalf("read ladder for %s: %v", gStrkey, err)
	}
	return failures, next
}

// sep1Deferral returns how far past the attempt the next one was pushed.
// Both columns are set to NOW() in the same statement, so their difference
// is exactly the interval the ladder produced — no clock skew to tolerate.
func sep1Deferral(t *testing.T, ctx context.Context, store *timescale.Store, gStrkey string) time.Duration {
	t.Helper()
	var secs float64
	err := store.DB().QueryRowContext(ctx, `
        SELECT extract(epoch FROM sep1_next_attempt_after - sep1_resolved_at)
          FROM issuers WHERE g_strkey = $1`, gStrkey).Scan(&secs)
	if err != nil {
		t.Fatalf("read deferral for %s: %v", gStrkey, err)
	}
	return time.Duration(secs) * time.Second
}

// ageSep1ResolvedAt backdates the last-attempt stamp so the `-older-than`
// arm of the queue is satisfied and the DEFERRAL arm is the only thing left
// deciding. Without this every row the test just wrote is < 24h old and
// nothing would be selected for reasons that have nothing to do with 0159.
func ageSep1ResolvedAt(t *testing.T, ctx context.Context, store *timescale.Store, gStrkey string, age time.Duration) {
	t.Helper()
	_, err := store.DB().ExecContext(ctx,
		`UPDATE issuers SET sep1_resolved_at = NOW() - $2::interval WHERE g_strkey = $1`,
		gStrkey, fmt.Sprintf("%d seconds", int64(age/time.Second)))
	if err != nil {
		t.Fatalf("backdate %s: %v", gStrkey, err)
	}
}

func candidateSet(t *testing.T, ctx context.Context, store *timescale.Store, staleness time.Duration, limit int) map[string]bool {
	t.Helper()
	got, err := store.IssuersNeedingSep1Refresh(ctx, staleness, limit)
	if err != nil {
		t.Fatalf("IssuersNeedingSep1Refresh: %v", err)
	}
	out := make(map[string]bool, len(got))
	for _, c := range got {
		out[c.GStrkey] = true
	}
	return out
}
