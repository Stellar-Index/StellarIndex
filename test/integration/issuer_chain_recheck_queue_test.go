//go:build integration

package integration_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A FILLED issuer row must still be re-offered to the chain — RSEC-V1 /
// RLT-470, against a real Postgres.
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
		got, err := store.IssuersNeedingChainRecheck(ctx, 0)
		if err != nil {
			t.Fatalf("IssuersNeedingChainRecheck: %v", err)
		}
		for _, rec := range got {
			if rec.GStrkey == mergedIssuer {
				t.Errorf("queue offers the merged row %s; the last-known queue already carries it", mergedIssuer)
			}
		}
		if got := homeDomainOfRow(t, mergedIssuer); got != "stellarbrunch.com" {
			t.Errorf("merged row's home_domain = %q, want it untouched", got)
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
