package timescale

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
	"time"
)

// fakeConn implements driver.Conn + driver.ExecerContext, recording every
// ExecContext query so the post-connect SET can be asserted.
type fakeConn struct {
	execs   []string
	execErr error
	closed  bool
}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not implemented") }
func (c *fakeConn) Close() error                        { c.closed = true; return nil }
func (c *fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("not implemented") }

func (c *fakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.execs = append(c.execs, query)
	if c.execErr != nil {
		return nil, c.execErr
	}
	return driver.RowsAffected(0), nil
}

// plainConn implements driver.Conn WITHOUT ExecerContext, to exercise the
// fail-closed guard.
type plainConn struct{ closed bool }

func (c *plainConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not implemented") }
func (c *plainConn) Close() error                        { c.closed = true; return nil }
func (c *plainConn) Begin() (driver.Tx, error)           { return nil, errors.New("not implemented") }

type fakeConnector struct {
	conn    driver.Conn
	connErr error
}

func (c *fakeConnector) Connect(context.Context) (driver.Conn, error) {
	if c.connErr != nil {
		return nil, c.connErr
	}
	return c.conn, nil
}
func (c *fakeConnector) Driver() driver.Driver { return nil }

// TestStatementTimeoutConnector_SetsTimeout pins R1 (audit-2026-07-16):
// every freshly-dialed serving-pool connection runs `SET statement_timeout`
// with the configured value (milliseconds) before it is handed out.
func TestStatementTimeoutConnector_SetsTimeout(t *testing.T) {
	fc := &fakeConn{}
	c := &statementTimeoutConnector{
		base:      &fakeConnector{conn: fc},
		timeoutMS: (30 * time.Second).Milliseconds(),
	}

	conn, err := c.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn != fc {
		t.Fatalf("Connect returned %v, want the wrapped conn", conn)
	}
	if len(fc.execs) != 1 || fc.execs[0] != "SET statement_timeout = 30000" {
		t.Fatalf("execs = %v, want exactly [SET statement_timeout = 30000]", fc.execs)
	}
}

// TestStatementTimeoutConnector_SetFailureClosesConn — a failed SET must
// fail the connection (not silently hand out an unbounded conn) AND close
// the underlying conn so it isn't leaked.
func TestStatementTimeoutConnector_SetFailureClosesConn(t *testing.T) {
	fc := &fakeConn{execErr: errors.New("boom")}
	c := &statementTimeoutConnector{
		base:      &fakeConnector{conn: fc},
		timeoutMS: 30000,
	}

	conn, err := c.Connect(context.Background())
	if err == nil {
		t.Fatal("Connect should error when SET statement_timeout fails")
	}
	if conn != nil {
		t.Errorf("Connect returned a non-nil conn on SET failure: %v", conn)
	}
	if !fc.closed {
		t.Error("underlying conn was not closed on SET failure (leak)")
	}
}

// TestStatementTimeoutConnector_RejectsNonExecerConn — if the driver conn
// can't run ExecContext, the timeout can't be enforced; fail closed rather
// than pretend it is in force.
func TestStatementTimeoutConnector_RejectsNonExecerConn(t *testing.T) {
	pc := &plainConn{}
	c := &statementTimeoutConnector{
		base:      &fakeConnector{conn: pc},
		timeoutMS: 30000,
	}

	if _, err := c.Connect(context.Background()); err == nil {
		t.Fatal("Connect should error when the driver conn lacks ExecerContext")
	}
	if !pc.closed {
		t.Error("non-execer conn was not closed")
	}
}

// TestStatementTimeoutConnector_PropagatesDialError — a dial failure passes
// through unwrapped-in-behaviour (no SET attempted).
func TestStatementTimeoutConnector_PropagatesDialError(t *testing.T) {
	c := &statementTimeoutConnector{
		base:      &fakeConnector{connErr: errors.New("dial refused")},
		timeoutMS: 30000,
	}
	if _, err := c.Connect(context.Background()); err == nil {
		t.Fatal("Connect should propagate the underlying dial error")
	}
}
