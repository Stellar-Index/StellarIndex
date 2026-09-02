//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestCatalogCorrectionsAndScaffoldDrop pins what migrations 0151 and 0152
// actually put in (and take out of) the DATABASE — which is the only place
// this class of defect lives.
//
// Why it needs a real database. A `COMMENT ON` string is not repo state: it
// is a row in pg_description written once, by whichever migration issued it.
// Editing the original migration's text therefore changes what a FRESH
// database gets and changes NOTHING about an applied one, so a file-level
// assertion would pass while every deployed environment still served the
// wrong string through `\d+`. The same is true in reverse for 0152: the
// defect was six tables that EXIST in the catalog with no writer, and only
// information_schema can answer whether they still do.
//
// Every assertion below is on the CORRECTED VALUE, not on
// defined/non-empty:
//
//   - trades.usd_volume must no longer claim the aggregator fills it
//     post-insert (it is valued at INSERT; NULL means "no route" and never
//     becomes a value on its own — a consumer that polls waits forever).
//   - aquarius_rewards_events must name twelve kinds, matching its own
//     event_kind CHECK, and must name config_rewards — the one the old
//     "11 kinds" list omitted.
//   - oracle_updates.contract_id must not name coinmarketcap or the retired
//     chainlink-http spelling.
//   - aquarius_protocol_fee.recipient must point at the `token` column
//     (migration 0139) instead of telling the reader to join a trade.
//   - soroban_events.topics_xdr must not offer the "ClickHouse-lake
//     re-project" recovery, which does not exist for that column.
//   - customer_webhooks.secret_hash must carry the MISNAMED warning: the
//     column holds the raw HMAC signing key, not a hash of one.
//   - the six unwired scaffold tables must be GONE, as must the four
//     orphaned Stripe columns and the two partial indexes over them.
//
// Runs under `-tags=integration` only, against the TimescaleDB version r1
// runs.
func TestCatalogCorrectionsAndScaffoldDrop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// ─── 0151: stored comments say the true thing ───────────────

	colComment := func(table, column string) string {
		t.Helper()
		var c sql.NullString
		err := db.QueryRowContext(ctx, `
			SELECT col_description(c.oid, a.attnum)
			  FROM pg_class c
			  JOIN pg_attribute a ON a.attrelid = c.oid
			 WHERE c.relname = $1 AND a.attname = $2`, table, column).Scan(&c)
		if err != nil {
			t.Fatalf("read comment on %s.%s: %v", table, column, err)
		}
		if !c.Valid {
			// Errorf, not Fatalf: one missing comment must not hide the
			// state of the other eleven. A test that stops at the first
			// failure reports one regression when there are several.
			t.Errorf("%s.%s has NO catalog comment — migration 0151 should have set one", table, column)
			return ""
		}
		return c.String
	}

	tableComment := func(table string) string {
		t.Helper()
		var c sql.NullString
		err := db.QueryRowContext(ctx,
			`SELECT obj_description($1::regclass, 'pg_class')`, table).Scan(&c)
		if err != nil {
			t.Fatalf("read comment on table %s: %v", table, err)
		}
		if !c.Valid {
			t.Errorf("table %s has NO catalog comment — migration 0151 should have set one", table)
			return ""
		}
		return c.String
	}

	type commentCase struct {
		what     string
		got      string
		mustHave []string
		mustNot  []string
		why      string
	}

	cases := []commentCase{
		{
			what:     "trades.usd_volume",
			got:      colComment("trades", "usd_volume"),
			mustHave: []string{"Valued AT INSERT", "no route"},
			mustNot:  []string{"Derived by the aggregator post-insert"},
			why: "no aggregator pass has ever valued this column; NULL means the tiered " +
				"USD router found no route, so a consumer waiting for the value to appear waits forever",
		},
		{
			what:     "oracle_updates.contract_id",
			got:      colComment("oracle_updates", "contract_id"),
			mustHave: []string{"chainlink", "coingecko", "ecb"},
			mustNot:  []string{"coinmarketcap", "chainlink-http"},
			why: "measured on r1 the off-chain (NULL contract_id) sources are chainlink, " +
				"coingecko and ecb; no coinmarketcap ingest writes here and `chainlink-http` is retired",
		},
		{
			what:     "aquarius_protocol_fee.recipient",
			got:      colComment("aquarius_protocol_fee", "recipient"),
			mustHave: []string{"`token` column", "0139"},
			mustNot:  []string{"token is positional"},
			why: "0139 added the token column from topic[1]; joining a trade instead " +
				"mis-attributes an op that sweeps two tokens to the same recipient",
		},
		{
			what:     "soroban_events.topics_xdr",
			got:      colComment("soroban_events", "topics_xdr"),
			mustHave: []string{"NOT repaired by projector-replay"},
			mustNot:  []string{"Recover pre-0114 rows via a ClickHouse-lake re-project"},
			why: "the projector reads this table and writes the per-source tables; " +
				"nothing writes topics_xdr back here, so the recovery it named cannot restore the column",
		},
		{
			what:     "customer_webhooks.secret_hash",
			got:      colComment("customer_webhooks", "secret_hash"),
			mustHave: []string{"MISNAMED", "RAW HMAC-SHA-256"},
			why: "the column stores the shared signing key, not a hash of it — `\\d+` is " +
				"where an operator forms the belief that it is already hashed",
		},
		{
			what:     "aquarius_rewards_events",
			got:      tableComment("aquarius_rewards_events"),
			mustHave: []string{"12 kinds", "config_rewards"},
			mustNot:  []string{"11 kinds"},
			why:      "the event_kind CHECK admits twelve values; the old list named eleven",
		},
		{
			what:     "decoder_stats_5m",
			got:      tableComment("decoder_stats_5m"),
			mustHave: []string{"statsflush", "WRITE-ONLY"},
			mustNot:  []string{"so /v1/diagnostics/decoders can query"},
			why: "the writer is the indexer's statsflush worker, not the aggregator, and " +
				"the /v1/diagnostics/decoders route it cited is not in the OpenAPI spec",
		},
		{
			what:    "completeness_snapshots",
			got:     tableComment("completeness_snapshots"),
			mustNot: []string{"notes/DECISION-genesis-complete-verdict"},
			why:     "notes/ is gitignored, so that pointer resolves to nothing for any reader",
		},
		{
			what:    "change_summary_5m",
			got:     tableComment("change_summary_5m"),
			mustNot: []string{"showcase-site-data-inventory.md"},
			why:     "the doc was renamed to explorer-data-inventory.md",
		},
		{
			what:    "soroswap_skim_events",
			got:     tableComment("soroswap_skim_events"),
			mustNot: []string{"docs/discovery/dexes-amms"},
			why:     "the docs/discovery/ tree was removed from the repo",
		},
	}

	for _, tc := range cases {
		if tc.got == "" {
			continue // already reported as missing by the reader above
		}
		for _, want := range tc.mustHave {
			if !strings.Contains(tc.got, want) {
				t.Errorf("catalog comment on %s does not contain %q (%s).\n  got: %s",
					tc.what, want, tc.why, tc.got)
			}
		}
		for _, bad := range tc.mustNot {
			if strings.Contains(tc.got, bad) {
				t.Errorf("catalog comment on %s STILL contains %q (%s).\n  got: %s",
					tc.what, bad, tc.why, tc.got)
			}
		}
	}

	// The corrected aquarius_rewards_events comment must enumerate exactly
	// the values its own CHECK admits — the "12 kinds" claim is only worth
	// anything if the list behind it is the real set.
	assertRewardsKindsMatchCheck(t, db, ctx, tableComment("aquarius_rewards_events"))

	// ─── 0152: the unwired scaffolds are gone ───────────────────

	for _, gone := range []string{
		"wasm_versions", "contract_wasm_history", "tvl_observations",
		"anchors", "classic_asset_stats_5m", "aggregator_exposures",
	} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM information_schema.tables
			  WHERE table_schema = 'public' AND table_name = $1`, gone).Scan(&n); err != nil {
			t.Fatalf("look up table %s: %v", gone, err)
		}
		if n != 0 {
			t.Errorf("table %q still exists after migration 0152. It has no Go writer or "+
				"reader in any released binary, so a `\\dt` listing it is a claim that a "+
				"capability exists when the data is merely absent (#358).", gone)
		}
	}

	for _, col := range []string{
		"dead_lettered_at", "dead_letter_reason", "dead_letter_resolved_at", "claimed_at",
	} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM information_schema.columns
			  WHERE table_name = 'stripe_event_log' AND column_name = $1`, col).Scan(&n); err != nil {
			t.Fatalf("look up stripe_event_log.%s: %v", col, err)
		}
		if n != 0 {
			t.Errorf("stripe_event_log.%s still exists after migration 0152 — its writer was "+
				"deleted in d2185560, so the column describes a reconciliation protocol no "+
				"code implements (#357 F8).", col)
		}
	}

	for _, idx := range []string{
		"stripe_event_log_open_dead_letters_idx", "stripe_event_log_claimed_idx",
	} {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_indexes WHERE indexname = $1`, idx).Scan(&n); err != nil {
			t.Fatalf("look up index %s: %v", idx, err)
		}
		if n != 0 {
			t.Errorf("index %q still exists after migration 0152 (its columns are gone)", idx)
		}
	}

	// stripe_event_log itself SURVIVES — 0027 creates it and 0152 must not
	// break 0027's rollback symmetry — but it is tombstoned so the next
	// reader does not go looking for a worker that was deleted.
	assertTableExists(t, db, ctx, "stripe_event_log")
	if c := tableComment("stripe_event_log"); !strings.Contains(c, "INERT") ||
		!strings.Contains(c, "d2185560") {
		t.Errorf("stripe_event_log's tombstone comment does not record that its writers "+
			"were deleted in d2185560.\n  got: %s", c)
	}
}

// assertRewardsKindsMatchCheck proves the corrected "12 kinds" comment lists
// exactly the values the event_kind CHECK constraint admits — so a future
// widening of the CHECK that forgets the comment fails here rather than
// re-creating the #357 F4 drift.
func assertRewardsKindsMatchCheck(t *testing.T, db *sql.DB, ctx context.Context, comment string) {
	t.Helper()

	var def string
	err := db.QueryRowContext(ctx, `
		SELECT pg_get_constraintdef(con.oid)
		  FROM pg_constraint con
		  JOIN pg_class c ON c.oid = con.conrelid
		 WHERE c.relname = 'aquarius_rewards_events'
		   AND con.contype = 'c'
		   AND pg_get_constraintdef(con.oid) LIKE '%event_kind%'`).Scan(&def)
	if err != nil {
		t.Fatalf("read aquarius_rewards_events event_kind CHECK: %v", err)
	}

	if comment == "" {
		return // the missing comment is already an error; nothing to compare
	}

	kinds := extractQuotedLiterals(def)
	if len(kinds) == 0 {
		t.Fatalf("parsed 0 kinds out of the event_kind CHECK %q — the parse has gone "+
			"vacuous; fix it, do not delete the assertion", def)
	}
	t.Logf("event_kind CHECK admits %d kinds", len(kinds))

	for _, k := range kinds {
		if !strings.Contains(comment, k) {
			t.Errorf("aquarius_rewards_events' catalog comment omits event kind %q, which "+
				"its own CHECK admits. The comment is what an operator reads through "+
				"`\\d+`; a kind missing from it is a kind they will not know exists (#357 F4).\n"+
				"  comment: %s", k, comment)
		}
	}
}

// extractQuotedLiterals pulls the 'single-quoted' literals out of a
// constraint definition, de-duplicated, preserving order.
func extractQuotedLiterals(s string) []string {
	var out []string
	seen := map[string]bool{}
	for {
		i := strings.Index(s, "'")
		if i < 0 {
			return out
		}
		s = s[i+1:]
		j := strings.Index(s, "'")
		if j < 0 {
			return out
		}
		lit := s[:j]
		s = s[j+1:]
		if lit != "" && !seen[lit] {
			seen[lit] = true
			out = append(out, lit)
		}
	}
}
