// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ─── the batch UPDATE must be prunable to the chunk it targets ─────
//
// `trades` is a hypertable. An UPDATE that names it and constrains `ts`
// only through the join (`t.ts = v.ts`) is planned against EVERY chunk:
// measured on production 2026-09-06, one batch's plan carried 260
// `Update on …_chunk` targets over an Append of 260 sequential scans
// (cost 10,040,409), TimescaleDB serviced the DML on the compressed ones
// by decompressing them wholesale, and a 23-row UPDATE ran 60 minutes,
// wrote ~270 GB of WAL and changed nothing. Bounding `t.ts` by the
// batch's own minimum and maximum is redundant against the join — it
// cannot exclude a row `t.ts = v.ts` would have matched — and it is what
// lets the planner drop the other 259 chunks.
//
// These tests hold the three properties that pruning depends on, at the
// only layer that can see them without a database: the statement the
// store builds and the values it binds.

// pruneRow builds one write-set row at ts.
func pruneRow(ts time.Time, ledger uint32) XLMBaseRestampRow {
	return XLMBaseRestampRow{
		Source:  "sdex",
		Ledger:  ledger,
		TxHash:  strings.Repeat("a", 64),
		OpIndex: 1,
		TS:      ts,
		Want:    "12.34000000",
	}
}

// TestApplyXLMBaseUSDVolumeRestamp_BatchIsPrunableToItsChunk pins the
// `ts` bounds: present in the statement, and bound to the batch's OWN
// extremes, so no row the join would match is excluded and no chunk
// outside the batch's span is planned.
func TestApplyXLMBaseUSDVolumeRestamp_BatchIsPrunableToItsChunk(t *testing.T) {
	ctx := context.Background()
	conn := newCaptureConn()
	store := &Store{db: sql.OpenDB(&captureConnector{conn: conn})}
	store.db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = store.db.Close() })

	// Deliberately NOT in ts order: the bounds are computed from the
	// batch, not read off its ends.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan := &XLMBaseRestampPlan{Rows: []XLMBaseRestampRow{
		pruneRow(base.Add(37*time.Minute), 60559500),
		pruneRow(base.Add(2*time.Minute), 60559447),
		pruneRow(base.Add(59*time.Minute), 60559611),
		pruneRow(base.Add(11*time.Minute), 60559470),
	}}
	wantLo, wantHi := base.Add(2*time.Minute), base.Add(59*time.Minute)

	if _, err := store.ApplyXLMBaseUSDVolumeRestamp(ctx, plan, 7, 0); err != nil {
		t.Fatalf("ApplyXLMBaseUSDVolumeRestamp: %v", err)
	}
	if len(conn.dml) != 1 {
		t.Fatalf("ran %d UPDATE(s), want 1: %q", len(conn.dml), conn.stmts)
	}
	got := conn.dml[0]

	if !strings.Contains(got.stmt, "t.ts      >= $2") || !strings.Contains(got.stmt, "t.ts      <= $3") {
		t.Fatalf("the batch UPDATE does not bound t.ts, so the planner cannot prune it to one chunk "+
			"(measured cost of the unbounded shape: 10,040,409 over 260 chunks). Statement:\n%s", got.stmt)
	}
	lo, ok := got.args[1].Value.(time.Time)
	if !ok {
		t.Fatalf("$2 = %T, want the batch's lower ts bound", got.args[1].Value)
	}
	hi, ok := got.args[2].Value.(time.Time)
	if !ok {
		t.Fatalf("$3 = %T, want the batch's upper ts bound", got.args[2].Value)
	}
	if !lo.Equal(wantLo) {
		t.Errorf("$2 = %s, want %s — the batch's earliest row", lo.Format(time.RFC3339), wantLo.Format(time.RFC3339))
	}
	if !hi.Equal(wantHi) {
		t.Errorf("$3 = %s, want %s — the batch's latest row", hi.Format(time.RFC3339), wantHi.Format(time.RFC3339))
	}
	// The bound must be INCLUSIVE at both ends, or the first and last row
	// of every batch would be dropped from the write set.
	for _, r := range plan.Rows {
		if r.TS.Before(lo) || r.TS.After(hi) {
			t.Errorf("row at %s falls outside the bound [%s, %s] the UPDATE would match",
				r.TS.Format(time.RFC3339), lo.Format(time.RFC3339), hi.Format(time.RFC3339))
		}
	}

	// `tx_hash` is char(64); binding it as text forces
	// `(t.tx_hash)::text = v.tx_hash`, which trades_pkey cannot serve.
	if strings.Contains(got.stmt, "::text, $") && !strings.Contains(got.stmt, "::bpchar") {
		t.Errorf("tx_hash is bound as text, so the join cannot ride trades_pkey. Statement:\n%s", got.stmt)
	}
	// Pruning only happens in a custom plan; a promoted generic plan
	// restores the 260-chunk shape.
	if got.planCache != "force_custom_plan" {
		t.Errorf("the UPDATE ran with plan_cache_mode = %q, want %q — a generic plan cannot prune on the bounds",
			got.planCache, "force_custom_plan")
	}
	// …and, like the decompression cap, it must not ride the pooled
	// connection out of the transaction.
	if v, ok := conn.session["plan_cache_mode"]; ok {
		t.Errorf("session-level plan_cache_mode = %q left on the pooled connection", v)
	}
}

// TestApplyXLMBaseUSDVolumeRestamp_BatchStaysUnderTheParameterCeiling
// pins the split: `-chunk-batch` defaults to 20,000 rows, the statement
// binds 6 placeholders per row plus 3, and the extended query protocol
// carries at most 65,535. An unsplit batch aborts the run mid-walk.
func TestApplyXLMBaseUSDVolumeRestamp_BatchStaysUnderTheParameterCeiling(t *testing.T) {
	ctx := context.Background()
	conn := newCaptureConn()
	store := &Store{db: sql.OpenDB(&captureConnector{conn: conn})}
	store.db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = store.db.Close() })

	const rows = 20_000
	base := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	plan := &XLMBaseRestampPlan{Rows: make([]XLMBaseRestampRow, 0, rows)}
	for i := range rows {
		plan.Rows = append(plan.Rows, pruneRow(base.Add(time.Duration(i)*time.Millisecond), uint32(60559447+i))) //nolint:gosec // test fixture
	}

	if _, err := store.ApplyXLMBaseUSDVolumeRestamp(ctx, plan, 7, rows); err != nil {
		t.Fatalf("ApplyXLMBaseUSDVolumeRestamp: %v", err)
	}
	if len(conn.dml) < 2 {
		t.Fatalf("a %d-row batch ran in %d statement(s); it must be split to stay under the protocol ceiling", rows, len(conn.dml))
	}
	var seen int
	for i, d := range conn.dml {
		if len(d.args) > 65535 {
			t.Errorf("statement %d bound %d parameters, over the protocol's 65,535 — pgx refuses this Exec", i, len(d.args))
		}
		seen += (len(d.args) - 3) / 6
	}
	if seen != rows {
		t.Errorf("the split statements carry %d row(s), want %d — the batch must be divided, not truncated", seen, rows)
	}
}

// -----------------------------------------------------------------
// Statement-capturing driver fake. It records each DML's text, its bound
// arguments and the transaction-local GUCs in force when it ran, which
// is everything these properties are stated in. Same shape as the GUC
// fake in usd_volume_restamp_guc_test.go, kept separate so neither test
// constrains the other's fake.
// -----------------------------------------------------------------

type captureDML struct {
	stmt      string
	args      []driver.NamedValue
	planCache string
}

type captureConn struct {
	session map[string]string
	local   map[string]string
	inTx    bool
	stmts   []string
	dml     []captureDML
}

func newCaptureConn() *captureConn { return &captureConn{session: map[string]string{}} }

func (c *captureConn) effective(name string) string {
	if v, ok := c.local[name]; ok {
		return v
	}
	return c.session[name]
}

func (c *captureConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("captureConn: Prepare not implemented")
}
func (c *captureConn) Close() error { return nil }

func (c *captureConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *captureConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	if c.inTx {
		return nil, errors.New("captureConn: nested transaction")
	}
	c.inTx = true
	c.local = map[string]string{}
	return &captureTx{c: c}, nil
}

func (c *captureConn) ResetSession(context.Context) error { return nil }

func (c *captureConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.stmts = append(c.stmts, q)
	switch {
	case strings.HasPrefix(q, "SET LOCAL "):
		name, val, err := parseGUCSet(strings.TrimPrefix(q, "SET LOCAL "))
		if err != nil {
			return nil, err
		}
		if !c.inTx {
			return driver.RowsAffected(0), nil
		}
		c.local[name] = val
	case strings.HasPrefix(q, "SET "):
		name, val, err := parseGUCSet(strings.TrimPrefix(q, "SET "))
		if err != nil {
			return nil, err
		}
		c.session[name] = val
	default:
		c.dml = append(c.dml, captureDML{stmt: q, args: args, planCache: c.effective("plan_cache_mode")})
		return driver.RowsAffected(int64(len(args))), nil
	}
	return driver.RowsAffected(0), nil
}

type captureTx struct{ c *captureConn }

func (t *captureTx) Commit() error   { t.c.endTx(); return nil }
func (t *captureTx) Rollback() error { t.c.endTx(); return nil }

func (c *captureConn) endTx() {
	c.inTx = false
	c.local = nil
}

type captureConnector struct{ conn *captureConn }

func (c *captureConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (c *captureConnector) Driver() driver.Driver                        { return captureDriver{} }

type captureDriver struct{}

func (captureDriver) Open(string) (driver.Conn, error) {
	return nil, fmt.Errorf("captureDriver: Open not implemented")
}
