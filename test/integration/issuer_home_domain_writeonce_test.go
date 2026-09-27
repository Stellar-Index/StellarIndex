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
