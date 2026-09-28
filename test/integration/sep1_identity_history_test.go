//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// SEP-1 identity change detection and freshness against a real Postgres
// (GH #840, migration 0190). The history rows are written inside
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
