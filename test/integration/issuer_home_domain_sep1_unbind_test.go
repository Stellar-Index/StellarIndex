//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

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
