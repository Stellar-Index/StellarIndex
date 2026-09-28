//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

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
