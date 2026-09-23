//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestStorePoolsPinSessionTimeZoneUTC opens every Store constructor against
// a database whose default TimeZone follows a DST-observing host, with the
// DSN asking for yet another zone, and requires each pooled session to be
// UTC. The closed-bucket guards spell `bucket <= now() - INTERVAL '<grain>'`;
// in a non-UTC session '1 day' and '1 month' are calendar-local, so across a
// DST transition the guard disagrees with the UTC time_bucket grid by an hour
// and a daily bucket is served (or withheld) at the wrong instant.
func TestStorePoolsPinSessionTimeZoneUTC(t *testing.T) {
	ctx := context.Background()
	dsn := startTimescale(t, ctx)

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	var dbName string
	if err := admin.QueryRowContext(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx,
		fmt.Sprintf(`ALTER DATABASE %q SET timezone = 'America/New_York'`, dbName)); err != nil {
		t.Fatalf("set database default timezone: %v", err)
	}
	// The DSN's own setting, in a spelling other than pgx's lowercase key.
	dsnWithZone := dsn + "&TimeZone=America/Denver"
	t.Setenv("PGTZ", "America/Chicago")

	openers := map[string]func() (*timescale.Store, error){
		"Open": func() (*timescale.Store, error) { return timescale.Open(ctx, dsnWithZone) },
		"OpenServing": func() (*timescale.Store, error) {
			return timescale.OpenServing(ctx, dsnWithZone, 30*time.Second)
		},
		"OpenBackground": func() (*timescale.Store, error) {
			return timescale.OpenBackground(ctx, dsnWithZone, 30*time.Second)
		},
	}
	for name, open := range openers {
		t.Run(name, func(t *testing.T) {
			s, err := open()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			defer func() { _ = s.Close() }()
			assertSessionGuardArithmeticIsUTC(t, ctx, s.DB())
		})
	}
}

// assertSessionGuardArithmeticIsUTC checks the session zone and the
// calendar grains the closed-bucket guard subtracts, each evaluated at an
// instant where a session in any US zone would disagree with UTC.
func assertSessionGuardArithmeticIsUTC(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var zone string
	if err := db.QueryRowContext(ctx, `SHOW TimeZone`).Scan(&zone); err != nil {
		t.Fatal(err)
	}
	if zone != "UTC" {
		t.Errorf("session TimeZone = %q, want UTC", zone)
	}
	cases := []struct {
		now, grain string
		want       time.Time
	}{
		// 2026-03-08 is the US spring-forward day: 03:30Z on the 9th is
		// 23:30 EDT on the 8th, and a local '1 day' back lands on EST.
		{"2026-03-09T03:30:00Z", "1 day", time.Date(2026, 3, 8, 3, 30, 0, 0, time.UTC)},
		{"2026-03-09T03:30:00Z", "1 week", time.Date(2026, 3, 2, 3, 30, 0, 0, time.UTC)},
		// The first monthly bucket boundary after it: 00:30Z on 1 March is
		// still 28 February in New York.
		{"2026-03-01T00:30:00Z", "1 month", time.Date(2026, 2, 1, 0, 30, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		var got time.Time
		q := fmt.Sprintf(`SELECT $1::timestamptz - INTERVAL '%s'`, tc.grain)
		if err := db.QueryRowContext(ctx, q, tc.now).Scan(&got); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if !got.Equal(tc.want) {
			t.Errorf("%s - INTERVAL '%s' = %s, want %s (the UTC calendar step)",
				tc.now, tc.grain, got.UTC().Format(time.RFC3339), tc.want.Format(time.RFC3339))
		}
	}
}
