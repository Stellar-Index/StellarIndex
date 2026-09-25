//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

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
		if err := store.SetIssuerSep1Payload(ctx, dead, []byte(sep1BondPayload(dead))); err != nil {
			t.Fatalf("SetIssuerSep1Payload: %v", err)
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
		if err := store.SetIssuerSep1Payload(ctx, dead, []byte(sep1BondPayload(dead))); err != nil {
			t.Fatalf("SetIssuerSep1Payload: %v", err)
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
