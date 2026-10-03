//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// moneyColumnExceptions are the migrated columns whose DDL carries a
// `-- lint-money:ok <reason>` marker; the test requires both halves to agree.
var moneyColumnExceptions = map[string]bool{
	"public.sdex_offer_events.price_n":   true,
	"public.sdex_offer_events.price_d":   true,
	"public.defindex_fees.fee_index":     true,
	"public.sushiswap_v3_pools.fee_pips": true,
}

// TestMoneyColumnsAreNumeric is the runtime half of the ADR-0003 money
// gate. scripts/ci/lint-migrations.sh greps single-line `name type` DDL,
// so a CAGG/view column typed by its expression (`sum(x)::double precision
// AS volume_usd`), a type on the next line or a float DOMAIN all pass it.
// The catalog sees the resolved type of every table, view, materialized
// view and foreign table column whatever the syntax, so this asserts no
// money-named column in the fully migrated schema is an integer, float or
// money type, nor a domain or array over one.
func TestMoneyColumnsAreNumeric(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	_, thisFile, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(thisFile), "..", "..")
	namePattern := lintMoneyNamePattern(t, repo)
	markedColumns := lintMoneyMarkedColumns(t, repo)

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	flagged := nonNumericMoneyColumns(ctx, t, db, namePattern)
	for _, col := range sortedKeys(flagged) {
		if !moneyColumnExceptions[col] {
			t.Errorf("%s is %s; a monetary column must be NUMERIC (ADR-0003) — cast the expression to numeric, or mark the DDL `-- lint-money:ok <reason>` and add it to moneyColumnExceptions",
				col, flagged[col])
		}
	}
	for _, col := range sortedKeys(moneyColumnExceptions) {
		if _, ok := flagged[col]; !ok {
			t.Errorf("moneyColumnExceptions entry %s is no longer a non-NUMERIC money column — remove it", col)
		}
		if !markedColumns[col[strings.LastIndex(col, ".")+1:]] {
			t.Errorf("moneyColumnExceptions entry %s has no `-- lint-money:ok <reason>` marker in migrations/*.up.sql", col)
		}
	}

	// The shapes the catalog query must resolve, so a blind spot in it
	// fails here rather than passing the migrated schema vacuously.
	probe := map[string]string{
		"money_gate_probe.probe_t.amounts":         "",
		"money_gate_probe.probe_t.fee_d":           "",
		"money_gate_probe.probe_t.reserves":        "",
		"money_gate_probe.probe_m.price_usd":       "",
		"money_gate_probe.probe_v.volume_usd":      "",
		"money_gate_probe.probe_ok.price_numeric":  "numeric",
		"money_gate_probe.probe_ok.amount_numeric": "numeric[]",
	}
	for _, stmt := range []string{
		`CREATE SCHEMA money_gate_probe`,
		`CREATE DOMAIN money_gate_probe.f8 AS double precision`,
		`CREATE DOMAIN money_gate_probe.f8s AS money_gate_probe.f8[]`,
		`CREATE TABLE money_gate_probe.probe_t (amounts double precision[], fee_d money_gate_probe.f8, reserves money_gate_probe.f8s)`,
		`CREATE MATERIALIZED VIEW money_gate_probe.probe_m AS SELECT 1.0::float8 AS price_usd`,
		`CREATE VIEW money_gate_probe.probe_v AS SELECT 1::bigint AS volume_usd`,
		`CREATE TABLE money_gate_probe.probe_ok (price_numeric numeric, amount_numeric numeric[])`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("probe %q: %v", stmt, err)
		}
	}
	probed := nonNumericMoneyColumns(ctx, t, db, namePattern)
	for _, col := range sortedKeys(probe) {
		_, got := probed[col]
		if want := probe[col] == ""; got != want {
			t.Errorf("catalog probe %s: flagged=%v, want %v", col, got, want)
		}
	}
}

// nonNumericMoneyColumns maps each money-named column whose type resolves,
// through domains and array elements, to an integer, float or money type
// to its declared type.
func nonNumericMoneyColumns(ctx context.Context, t *testing.T, db *sql.DB, namePattern string) map[string]string {
	t.Helper()
	// pg_catalog rather than information_schema: the latter omits
	// materialized views and reports every array as data_type ARRAY.
	rows, err := db.QueryContext(ctx, `
		WITH RECURSIVE resolved(col, declared, typ) AS (
			SELECT n.nspname || '.' || c.relname || '.' || a.attname,
			       format_type(a.atttypid, a.atttypmod), a.atttypid
			  FROM pg_attribute a
			  JOIN pg_class c ON c.oid = a.attrelid
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f')
			   AND a.attnum > 0 AND NOT a.attisdropped
			   AND n.nspname !~ '^(pg_|information_schema$|_?timescaledb)'
			   AND a.attname ~* $1
			UNION ALL
			SELECT r.col, r.declared,
			       CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE t.typelem END
			  FROM resolved r
			  JOIN pg_type t ON t.oid = r.typ
			 WHERE t.typtype = 'd' OR (t.typcategory = 'A' AND t.typelem <> 0)
		)
		SELECT DISTINCT col, declared
		  FROM resolved
		 WHERE typ = ANY ('{int2,int4,int8,float4,float8,money}'::regtype[])`,
		"^("+namePattern+")$")
	if err != nil {
		t.Fatalf("query money columns: %v", err)
	}
	defer rows.Close()

	flagged := map[string]string{}
	for rows.Next() {
		var col, declared string
		if err := rows.Scan(&col, &declared); err != nil {
			t.Fatalf("scan: %v", err)
		}
		flagged[col] = declared
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return flagged
}

// lintMoneyNamePattern reads the stem regex from lint-migrations.sh so the
// DDL grep and this runtime check cannot disagree on what is money.
func lintMoneyNamePattern(t *testing.T, repo string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(repo, "scripts", "ci", "lint-migrations.sh"))
	if err != nil {
		t.Fatalf("read lint-migrations.sh: %v", err)
	}
	m := regexp.MustCompile(`(?m)^name='([^']+)'$`).FindSubmatch(src)
	if m == nil {
		t.Fatal("lint-migrations.sh: no `name='...'` money-stem pattern line")
	}
	return string(m[1])
}

// lintMoneyMarkedColumns returns the leading column name of every
// migrations/*.up.sql line carrying a `-- lint-money:ok` marker.
func lintMoneyMarkedColumns(t *testing.T, repo string) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repo, "migrations", "*.up.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	leading := regexp.MustCompile(`^\s*"?([A-Za-z0-9_]+)"?\s`)
	marked := map[string]bool{}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, "lint-money:ok") {
				continue
			}
			if m := leading.FindStringSubmatch(line); m != nil {
				marked[strings.ToLower(m[1])] = true
			}
		}
	}
	return marked
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
