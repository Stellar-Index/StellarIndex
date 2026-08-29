// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

// A database/sql driver that replays a scripted result per statement, in
// order, and records BOTH the SQL text and the bound arguments of every
// statement it was asked to run.
//
// It is the exec-aware, argument-recording sibling of [cannedConn] (see
// vwap_direction_combine_test.go), which covers query-only reads and does
// not surface the args. Store methods whose contract lives in the
// PLACEHOLDER BINDING — "$1 is the kind, $2 is the limit", "the ON CONFLICT
// guard gets this store's derive_generation", "the window bound is
// since.UTC()" — cannot be pinned without them, and the write paths
// (INSERT/DELETE) need ExecContext at all. New no-database store tests
// should use this one; folding cannedConn into it is a follow-up.
//
// Faithfulness to production: [scriptedConn.CheckNamedValue] accepts every
// Go value unconverted, exactly as pgx v5's stdlib driver does (pgx encodes
// Go values itself rather than going through database/sql's default
// converter), so a []string arg reaches the driver as a []string here and
// as a Postgres array in production — the reason internal/pgarray is a SCAN
// adapter only.

// scriptedResult is one statement's canned outcome: either rows (for a
// query) or a row count (for an exec), or an error.
type scriptedResult struct {
	cols         []string
	rows         [][]driver.Value
	rowsAffected int64
	err          error
}

// recordedStmt is one statement the store actually issued.
type recordedStmt struct {
	sql  string
	args []driver.Value
}

// arg returns the 1-based placeholder ($n) value, failing the test when
// the statement bound fewer arguments than that.
func (s recordedStmt) arg(t *testing.T, n int) driver.Value {
	t.Helper()
	if n < 1 || n > len(s.args) {
		t.Fatalf("statement bound %d args, wanted $%d\nSQL: %s", len(s.args), n, s.sql)
	}
	return s.args[n-1]
}

type scriptedConn struct {
	script []scriptedResult
	n      int
	stmts  []recordedStmt
}

func (c *scriptedConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("not implemented")
}
func (c *scriptedConn) Close() error              { return nil }
func (c *scriptedConn) Begin() (driver.Tx, error) { return nil, errors.New("not implemented") }

// CheckNamedValue passes every value through unconverted — what pgx's
// stdlib driver does.
func (c *scriptedConn) CheckNamedValue(*driver.NamedValue) error { return nil }

func (c *scriptedConn) next(q string, args []driver.NamedValue) (scriptedResult, error) {
	vals := make([]driver.Value, len(args))
	for _, a := range args {
		if a.Ordinal < 1 || a.Ordinal > len(vals) {
			return scriptedResult{}, fmt.Errorf("scriptedConn: arg ordinal %d out of range", a.Ordinal)
		}
		vals[a.Ordinal-1] = a.Value
	}
	c.stmts = append(c.stmts, recordedStmt{sql: q, args: vals})
	if c.n >= len(c.script) {
		return scriptedResult{}, errors.New("scriptedConn: unexpected extra statement: " + q)
	}
	res := c.script[c.n]
	c.n++
	return res, res.err
}

func (c *scriptedConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	res, err := c.next(q, args)
	if err != nil {
		return nil, err
	}
	return &scriptedRows{res: res}, nil
}

func (c *scriptedConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	res, err := c.next(q, args)
	if err != nil {
		return nil, err
	}
	return scriptedExecResult{affected: res.rowsAffected}, nil
}

type scriptedRows struct {
	res scriptedResult
	i   int
}

func (r *scriptedRows) Columns() []string { return r.res.cols }
func (r *scriptedRows) Close() error      { return nil }
func (r *scriptedRows) Next(dest []driver.Value) error {
	if r.i >= len(r.res.rows) {
		return io.EOF
	}
	copy(dest, r.res.rows[r.i])
	r.i++
	return nil
}

type scriptedExecResult struct{ affected int64 }

func (r scriptedExecResult) LastInsertId() (int64, error) {
	return 0, errors.New("scriptedExecResult: no LastInsertId")
}
func (r scriptedExecResult) RowsAffected() (int64, error) { return r.affected, nil }

type scriptedDriver struct{}

func (scriptedDriver) Open(string) (driver.Conn, error) { return nil, errors.New("not implemented") }

type scriptedConnector struct{ conn *scriptedConn }

func (c *scriptedConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (c *scriptedConnector) Driver() driver.Driver                        { return scriptedDriver{} }

// newScriptedStore returns a Store backed by the scripted connection,
// registering the close on t.
func newScriptedStore(t *testing.T, script ...scriptedResult) (*Store, *scriptedConn) {
	t.Helper()
	conn := &scriptedConn{script: script}
	store := &Store{db: sql.OpenDB(&scriptedConnector{conn: conn})}
	t.Cleanup(func() { _ = store.db.Close() })
	return store, conn
}

// only returns the single statement the store was expected to issue.
func (c *scriptedConn) only(t *testing.T) recordedStmt {
	t.Helper()
	if len(c.stmts) != 1 {
		t.Fatalf("store issued %d statements, want exactly 1: %+v", len(c.stmts), c.stmts)
	}
	return c.stmts[0]
}

// wantTime asserts a bound argument is the given instant, in UTC — every
// timestamptz bound in this package is normalised with .UTC() before it
// reaches the driver so the wire value can never carry a local offset.
func wantTime(t *testing.T, got driver.Value, want time.Time) {
	t.Helper()
	ts, ok := got.(time.Time)
	if !ok {
		t.Fatalf("bound arg is %T, want time.Time", got)
	}
	if !ts.Equal(want) {
		t.Errorf("bound time = %s, want %s", ts, want)
	}
	if ts.Location() != time.UTC {
		t.Errorf("bound time location = %s, want UTC", ts.Location())
	}
}
