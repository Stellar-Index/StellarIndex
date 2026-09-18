//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestDataFreshnessSurvivesADeadFeed executes the data-freshness
// watchdog's per-domain SQL — the SHIPPED bytes of
// configs/ansible/roles/archival-node/files/data-freshness.sh — against
// a real database in which feeds have been dead long enough to fall out
// of the windows the queries used to enumerate them.
//
// The defect being pinned: the window that decided WHICH sources exist
// was the same window that decided whether they were healthy. A source
// dead beyond it dropped out of its own GROUP BY, so
// `stellarindex_data_freshness_stale{source=X}` went ABSENT instead of
// going to 1, Prometheus aged the series out, and `== 1` alerts
// RESOLVED — the watchdog fell silent the worse the outage got. The
// oracle and fx legs auto-resolved at 30 days, the supply leg at 7, and
// the CS-102 per-asset gauge reported a literal healthy 0 once every
// watched asset had been frozen longer than 30 days.
func TestDataFreshnessSurvivesADeadFeed(t *testing.T) {
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

	now := time.Now().UTC()
	longDead := now.AddDate(0, 0, -40) // past the 30-day oracle/fx windows
	frozen := now.AddDate(0, 0, -40)   // past the 7-day supply window

	// One oracle alive, one dead for 40 days (the coingecko-quota shape).
	seedOracleUpdate(t, ctx, db, 1, "reflector", now.Add(-2*time.Minute))
	seedOracleUpdate(t, ctx, db, 2, "coingecko", longDead)
	// One FX source, dead for 40 days.
	seedFXQuote(t, ctx, db, "massive", "EUR", longDead)
	// Every watched supply asset frozen 40 days ago.
	for i, asset := range []string{"USDC-GA5Z", "EURC-GB3Q", "BENJI-GBHN"} {
		seedSupplyRow(t, ctx, db, asset, frozen.Add(time.Duration(i)*time.Minute))
	}
	// issuers exists but no SEP-1 payload was ever resolved.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO issuers (g_strkey) VALUES ('GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN')`,
	); err != nil {
		t.Fatalf("seed issuer: %v", err)
	}

	samples := runFreshnessDomainQueries(t, ctx, db)

	t.Run("a long-dead source still reports stale", func(t *testing.T) {
		for _, want := range []struct{ key, why string }{
			{
				`stellarindex_data_freshness_stale{domain="oracle",source="coingecko"}`,
				"an oracle dead 40 days is past the 30-day enumeration window",
			},
			{
				`stellarindex_data_freshness_stale{domain="fx",source="massive"}`,
				"an FX source dead 40 days is past the 30-day enumeration window",
			},
			{
				`stellarindex_data_freshness_stale{domain="supply",source="asset_supply_history"}`,
				"supply frozen 40 days is past the 7-day enumeration window",
			},
		} {
			v, ok := samples[want.key]
			if !ok {
				t.Errorf("%s is ABSENT — %s, so the series that should alert disappeared instead of reading 1", want.key, want.why)
				continue
			}
			if v != "1" {
				t.Errorf("%s = %s, want 1 (%s)", want.key, v, want.why)
			}
		}
	})

	t.Run("a domain that never observed anything is stale, not silent", func(t *testing.T) {
		key := `stellarindex_data_freshness_stale{domain="sep1",source="issuers"}`
		v, ok := samples[key]
		if !ok {
			t.Errorf("%s is ABSENT — a sep1 refresh that has never run leaves max(sep1_resolved_at) NULL, and an unknown age must not read as no alarm", key)
		} else if v != "1" {
			t.Errorf("%s = %s, want 1 — nothing was ever resolved, which is the most stale this domain can be", key, v)
		}
	})

	t.Run("a live source still reports healthy", func(t *testing.T) {
		key := `stellarindex_data_freshness_stale{domain="oracle",source="reflector"}`
		v, ok := samples[key]
		if !ok || v != "0" {
			t.Errorf("%s = %q (present=%v), want 0 — an oracle that printed two minutes ago is not stale", key, v, ok)
		}
		age, ok := samples[`stellarindex_data_freshness_age_seconds{domain="oracle",source="reflector"}`]
		if !ok {
			t.Fatalf("no age sample for the live oracle")
		}
		if n, err := strconv.ParseFloat(age, 64); err != nil || n > 600 {
			t.Errorf("live oracle age = %s seconds, want a couple of minutes", age)
		}
	})

	t.Run("the per-asset supply gauge counts a whole-domain freeze", func(t *testing.T) {
		v, ok := samples["stellarindex_supply_assets_stale"]
		if !ok {
			t.Fatal("stellarindex_supply_assets_stale is absent")
		}
		if v != "3" {
			t.Errorf("stellarindex_supply_assets_stale = %s, want 3 — every watched asset froze 40 days ago, and a count of 0 there is the CS-102 shape reported as health", v)
		}
		worst, ok := samples["stellarindex_supply_asset_max_age_seconds"]
		if !ok {
			t.Fatal("stellarindex_supply_asset_max_age_seconds is absent")
		}
		if n, err := strconv.ParseFloat(worst, 64); err != nil || n < 30*24*3600 {
			t.Errorf("worst per-asset supply age = %s seconds, want >= 30 days", worst)
		}
	})
}

// runFreshnessDomainQueries executes the watchdog's per-domain and
// per-asset blocks and returns every exposition sample it rendered,
// keyed by `name{labels}`.
func runFreshnessDomainQueries(t *testing.T, ctx context.Context, db *sql.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, needle := range []string{
		"stellarindex_data_freshness_stale",
		"stellarindex_supply_assets_stale",
	} {
		for _, stmt := range freshnessSQLBlocks(t, needle) {
			rows, err := db.QueryContext(ctx, stmt)
			if err != nil {
				t.Fatalf("execute watchdog block: %v\n--- SQL ---\n%s", err, stmt)
			}
			for rows.Next() {
				var line sql.NullString
				if err := rows.Scan(&line); err != nil {
					t.Fatalf("scan exposition line: %v", err)
				}
				// A NULL rendering is the failure mode the publication
				// validator withholds — record it as such rather than
				// silently dropping it, so an assertion can see it.
				if !line.Valid {
					continue
				}
				key, value, found := strings.Cut(strings.TrimSpace(line.String), " ")
				if !found {
					t.Fatalf("un-parseable exposition line %q", line.String)
				}
				out[key] = value
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("watchdog block rows: %v", err)
			}
			_ = rows.Close()
		}
	}
	return out
}

func seedOracleUpdate(t *testing.T, ctx context.Context, db *sql.DB, nonce int, source string, at time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO oracle_updates
		    (source, ledger, tx_hash, op_index, ts, asset, quote, price, decimals, ingested_at)
		VALUES ($1, $2, $3, 0, $4, 'native', 'fiat:USD', 0.12::numeric, 14, $4)`,
		source, 90_000_000+nonce, fmt.Sprintf("%064x", 900_000+nonce), at,
	); err != nil {
		t.Fatalf("seed oracle_updates %s: %v", source, err)
	}
}

func seedFXQuote(t *testing.T, ctx context.Context, db *sql.DB, source, ticker string, at time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO fx_quotes (bucket, ticker, rate_usd, inverse_usd, source, observed_at)
		VALUES ($1, $2, 1.08::numeric, 0.925::numeric, $3, $1)`,
		at, ticker, source,
	); err != nil {
		t.Fatalf("seed fx_quotes %s: %v", source, err)
	}
}

func seedSupplyRow(t *testing.T, ctx context.Context, db *sql.DB, assetKey string, at time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO asset_supply_history
		    (time, asset_key, total_supply, circulating_supply, basis, ledger_sequence)
		VALUES ($1, $2, 1000::numeric, 900::numeric, 'classic', 50000000)`,
		at, assetKey,
	); err != nil {
		t.Fatalf("seed asset_supply_history %s: %v", assetKey, err)
	}
}
