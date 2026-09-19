//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// issuers.home_domain must track the chain — RSEC-V1 / RLT-470, against a
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
	)
	seedIssuers(t, ctx, store, []seedIssuer{
		{g: anchor, homeDomain: "apay.io"},
		{g: drained, homeDomain: "lapsed-former.example"},
		{g: merged, homeDomain: "stellarbrunch.com"},
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
			t.Errorf("home_domain = %q, want ultracapital.xyz — the lake only returns accounts that "+
				"DECLARE a domain, so an empty value means we did not read it", got)
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

	t.Run("a merged account's empty reading leaves the stored domain alone", func(t *testing.T) {
		asOf := uint32(54564588)
		// validate() refuses a last_known_before_removal reading that carries
		// a domain, so an empty HomeDomain here is the only shape this branch
		// can take — and it must not wipe what the row already holds.
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
		if got := homeDomainOf(t, merged); got != "stellarbrunch.com" {
			t.Errorf("home_domain = %q, want stellarbrunch.com — an absent reading is not a retraction", got)
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
	})
}
