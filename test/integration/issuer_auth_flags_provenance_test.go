//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Auth-flag provenance round-trip against a real Postgres (#374).
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

	// Real r1 residue issuers (2026-09-03). mergedA was merged away at
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
