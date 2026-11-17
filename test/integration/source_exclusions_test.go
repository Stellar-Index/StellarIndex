//go:build integration

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/incident"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestSourceExclusions_InsertListLiftAndChecks pins the 0231 control table: rows round-trip, a lift stamps and keeps the
// row (a second lift is refused), the CHECKs reject malformed rows, and the append-only trigger refuses every change
// but a one-time lift.
func TestSourceExclusions_InsertListLiftAndChecks(t *testing.T) {
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

	from := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	wide, err := store.InsertSourceExclusion(ctx, timescale.SourceExclusion{
		Source: "binance", From: from, Reason: "venue halted", Actor: "ops",
	})
	if err != nil {
		t.Fatalf("insert source-wide: %v", err)
	}
	pair, err := store.InsertSourceExclusion(ctx, timescale.SourceExclusion{
		Source: "soroswap", Base: "crypto:BTC", Quote: "native", From: from, Until: from.Add(time.Hour),
		Reason: "pool drained", Actor: "ops",
	})
	if err != nil {
		t.Fatalf("insert pair: %v", err)
	}

	unlifted, err := store.UnliftedSourceExclusions(ctx)
	if err != nil || len(unlifted) != 2 {
		t.Fatalf("unlifted = %+v, %v; want 2 rows", unlifted, err)
	}
	if a := unlifted[0]; a.ID != wide || a.Base != "" || a.Quote != "" || !a.Until.IsZero() || !a.From.Equal(from) {
		t.Fatalf("source-wide row = %+v", a)
	}
	if p := unlifted[1]; p.ID != pair || p.Base != "crypto:BTC" || p.Quote != "native" || !p.Until.Equal(from.Add(time.Hour)) {
		t.Fatalf("pair row = %+v", p)
	}

	if err := store.LiftSourceExclusion(ctx, wide, "ops", "venue back"); err != nil {
		t.Fatalf("lift: %v", err)
	}
	if err := store.LiftSourceExclusion(ctx, wide, "ops", "again"); !errors.Is(err, timescale.ErrSourceExclusionLifted) {
		t.Fatalf("second lift err = %v, want ErrSourceExclusionLifted", err)
	}
	if err := store.LiftSourceExclusion(ctx, 999, "ops", "nope"); !errors.Is(err, timescale.ErrNotFound) {
		t.Fatalf("unknown-id lift err = %v, want ErrNotFound", err)
	}
	if unlifted, err = store.UnliftedSourceExclusions(ctx); err != nil || len(unlifted) != 1 || unlifted[0].ID != pair {
		t.Fatalf("unlifted after lift = %+v, %v; want only %d", unlifted, err, pair)
	}
	all, err := store.ListSourceExclusions(ctx, false)
	if err != nil || len(all) != 2 {
		t.Fatalf("all = %+v, %v; want both rows kept", all, err)
	}
	if l := all[0]; !l.Lifted() || l.LiftedBy != "ops" || l.LiftReason != "venue back" {
		t.Fatalf("lifted row = %+v", l)
	}

	bad := map[string]string{
		"pair half-set": `INSERT INTO source_exclusions (source, base_asset, from_ts, reason, actor)
			VALUES ('binance', 'native', now(), 'r', 'a')`,
		"pair blank leg": `INSERT INTO source_exclusions (source, base_asset, quote_asset, from_ts, reason, actor)
			VALUES ('binance', ' ', 'native', now(), 'r', 'a')`,
		"pair same asset": `INSERT INTO source_exclusions (source, base_asset, quote_asset, from_ts, reason, actor)
			VALUES ('binance', 'native', 'native', now(), 'r', 'a')`,
		"until not after from": `INSERT INTO source_exclusions (source, from_ts, until_ts, reason, actor)
			VALUES ('binance', '2026-11-17T09:00:00Z', '2026-11-17T09:00:00Z', 'r', 'a')`,
		"blank reason": `INSERT INTO source_exclusions (source, from_ts, reason, actor)
			VALUES ('binance', now(), '   ', 'a')`,
		"lift fields partial": `INSERT INTO source_exclusions (source, from_ts, reason, actor, lifted_at)
			VALUES ('binance', now(), 'r', 'a', now())`,
		"blank lifted_by": `INSERT INTO source_exclusions (source, from_ts, reason, actor, lifted_at, lifted_by, lift_reason)
			VALUES ('binance', now(), 'r', 'a', now(), '  ', 'back')`,
	}
	for name, q := range bad {
		if _, err := db.ExecContext(ctx, q); err == nil {
			t.Errorf("%s: insert accepted, want a CHECK violation", name)
		}
	}

	refused := map[string]string{
		"delete":                   fmt.Sprintf(`DELETE FROM source_exclusions WHERE id = %d`, pair),
		"truncate":                 `TRUNCATE source_exclusions`,
		"re-open a lifted row":     fmt.Sprintf(`UPDATE source_exclusions SET lifted_at = NULL, lifted_by = NULL, lift_reason = NULL WHERE id = %d`, wide),
		"re-stamp a lifted row":    fmt.Sprintf(`UPDATE source_exclusions SET lift_reason = 'rewritten' WHERE id = %d`, wide),
		"edit until_ts":            fmt.Sprintf(`UPDATE source_exclusions SET until_ts = from_ts + interval '1 day' WHERE id = %d`, pair),
		"lift while editing scope": fmt.Sprintf(`UPDATE source_exclusions SET lifted_at = now(), lifted_by = 'ops', lift_reason = 'x', source = 'binance' WHERE id = %d`, pair),
	}
	for name, q := range refused {
		if _, err := db.ExecContext(ctx, q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v, want the append-only trigger to refuse it", name, err)
		}
	}
	if all, err = store.ListSourceExclusions(ctx, false); err != nil || len(all) != 2 || !all[0].Lifted() ||
		all[0].LiftReason != "venue back" || all[1].Lifted() || !all[1].Until.Equal(from.Add(time.Hour)) || all[1].Source != "soroswap" {
		t.Fatalf("rows after refused changes = %+v, %v; want both unchanged", all, err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		`UPDATE source_exclusions SET lifted_at = now(), lifted_by = 'ops', lift_reason = 'pool refilled' WHERE id = %d`, pair)); err != nil {
		t.Fatalf("a plain lift UPDATE was refused: %v", err)
	}
}

// TestSourceExclude_CommandLeavesTradesUntouched drives the real command's create and lift with -write and checks a
// seeded trade row is byte-identical afterwards: the control plane must never write trades.
func TestSourceExclude_CommandLeavesTradesUntouched(t *testing.T) {
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
	insertSampleTrade(t, db, ctx)
	cfgPath := filepath.Join(t.TempDir(), "stellarindex.toml")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf("[storage]\npostgres_dsn = %q\n", dsn)), 0o600); err != nil {
		t.Fatal(err)
	}

	snapshot := func() (int, string) {
		t.Helper()
		var n int
		var rows string
		if err := db.QueryRowContext(ctx,
			`SELECT count(*), COALESCE(string_agg(to_jsonb(t)::text, '|' ORDER BY to_jsonb(t)::text), '') FROM trades t`,
		).Scan(&n, &rows); err != nil {
			t.Fatalf("snapshot trades: %v", err)
		}
		return n, rows
	}
	beforeN, before := snapshot()
	if beforeN != 1 {
		t.Fatalf("seeded trades = %d, want 1", beforeN)
	}

	run := func(args ...string) string {
		t.Helper()
		out, err := captureStdout(t, func() error {
			return incident.SourceExclude(append([]string{"-config", cfgPath, "-actor", "ops", "-write"}, args...))
		})
		if err != nil {
			t.Fatalf("source-exclude %v: %v\n%s", args, err, out)
		}
		return out
	}
	run("-source", "sdex", "-base", "native", "-quote", "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
		"-from", time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), "-reason", "bad fills")
	unlifted, err := store.UnliftedSourceExclusions(ctx)
	if err != nil || len(unlifted) != 1 {
		t.Fatalf("unlifted after create = %+v, %v; want 1 row", unlifted, err)
	}
	run("-lift", "-id", fmt.Sprint(unlifted[0].ID), "-reason", "fills fixed")
	if unlifted, err = store.UnliftedSourceExclusions(ctx); err != nil || len(unlifted) != 0 {
		t.Fatalf("unlifted after lift = %+v, %v; want none", unlifted, err)
	}

	var audits int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM audit_log WHERE action IN ('source.exclude', 'source.exclude.recorded', 'source.exclude.lift')`,
	).Scan(&audits); err != nil || audits != 3 {
		t.Fatalf("source-exclude audit rows = %d, %v; want create, recorded and lift", audits, err)
	}
	if afterN, after := snapshot(); afterN != beforeN || after != before {
		t.Fatalf("trades changed by source-exclude:\nbefore (%d) %s\nafter  (%d) %s", beforeN, before, afterN, after)
	}
}
