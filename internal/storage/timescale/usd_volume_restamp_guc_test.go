package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"
)

// ─── W5.3: the restamp's decompression-cap GUC must not escape ─────

const (
	// decompressionCapGUC is lifted for the restamp's bulk UPDATE over
	// COMPRESSED chunks; decompressionCapDefault is TimescaleDB's
	// out-of-the-box value, i.e. what every OTHER statement must see.
	decompressionCapGUC     = "timescaledb.max_tuples_decompressed_per_dml_transaction"
	decompressionCapDefault = "100000"
	// gucConnDMLRows is what the fake reports for a DML, so the test can
	// also prove the row count survives the COMMIT.
	gucConnDMLRows = 7
)

// TestRestampExactTierUSDVolume_DecompressionCapNeverEscapesTheTransaction
// pins the GUC hygiene of [Store.RestampExactTierUSDVolume] (#312).
//
// The hazard is pooling, not SQL: `database/sql`'s Conn.Close returns the
// connection TO THE POOL, and pgx v5's stdlib adapter resets nothing on
// reuse (its default ResetSession only pings, and discards a conn left
// mid-transaction — pgx v5 stdlib/sql.go). A session-level
// `SET timescaledb.max_tuples_decompressed_per_dml_transaction = 0` would
// therefore ride that pooled connection for the process lifetime and
// silently uncap every later DML that landed on it. `SET LOCAL` inside the
// write transaction is unwound by Postgres at COMMIT/ROLLBACK.
//
// [gucConn] models exactly those two semantics — transaction-local vs
// session GUC scoping, and pgx's no-op session reset — so the pool
// reuse the hazard needs is observable without a database. The test
// asserts BOTH halves: the cap is still lifted for the restamp's own
// UPDATE (a "fix" that drops the SET would abort a real day's DML — one
// day needed 265k tuples on 2026-07-30), and no statement after it sees
// anything but the default.
func TestRestampExactTierUSDVolume_DecompressionCapNeverEscapesTheTransaction(t *testing.T) {
	ctx := context.Background()
	conn := newGUCConn()
	store := &Store{db: sql.OpenDB(&gucConnector{conn: conn})}
	// ONE connection: the conn the restamp borrows IS the conn the next
	// statement lands on, which makes the pool-reuse hazard deterministic.
	store.db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = store.db.Close() })

	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	p := USDVolumeRestampParams{
		Groups: []USDVolumeRestampGroup{{
			Source:     "sdex",
			BaseAsset:  "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			QuoteAsset: "native",
			Tier:       TierBasePegged,
			Decimals:   7,
		}},
		From:       day,
		To:         day.Add(time.Hour),
		Generation: 1_756_400_000,
	}
	n, err := store.RestampExactTierUSDVolume(ctx, p)
	if err != nil {
		t.Fatalf("RestampExactTierUSDVolume: %v", err)
	}
	if n != gucConnDMLRows {
		t.Errorf("rows affected = %d, want %d (the count must survive the COMMIT)", n, gucConnDMLRows)
	}

	// 1. the cap WAS lifted for the restamp's own UPDATE.
	if len(conn.dml) != 1 {
		t.Fatalf("restamp ran %d DML statements, want 1: %q", len(conn.dml), conn.stmts)
	}
	if !strings.HasPrefix(conn.dml[0].stmt, "UPDATE trades") {
		t.Fatalf("restamp DML = %q, want the trades UPDATE", conn.dml[0].stmt)
	}
	if conn.dml[0].cap != "0" {
		t.Errorf("restamp UPDATE ran with %s = %q, want %q — the compressed-chunk DML would abort at the default cap",
			decompressionCapGUC, conn.dml[0].cap, "0")
	}

	// 2. …and it did NOT ride the connection back into the pool: the next
	// DML to land on it runs under the DEFAULT cap again.
	if _, err := store.db.ExecContext(ctx, "UPDATE trades SET usd_volume = usd_volume"); err != nil {
		t.Fatalf("later DML: %v", err)
	}
	if len(conn.dml) != 2 {
		t.Fatalf("saw %d DML statements, want 2: %q", len(conn.dml), conn.stmts)
	}
	if got := conn.dml[1].cap; got != decompressionCapDefault {
		t.Errorf("later DML on the pooled connection ran with %s = %q, want the default %q — the restamp leaked the lifted cap into the pool (#312)",
			decompressionCapGUC, got, decompressionCapDefault)
	}
	// 3. nothing session-level was left behind at all.
	if v, ok := conn.session[decompressionCapGUC]; ok {
		t.Errorf("session-level %s = %q left on the pooled connection; the restamp must scope it to its transaction",
			decompressionCapGUC, v)
	}
}

// -----------------------------------------------------------------
// GUC-scoping driver fake — models the two behaviours the hazard needs:
// Postgres's SET vs SET LOCAL scoping (LOCAL is unwound by COMMIT /
// ROLLBACK; a plain SET outlives the transaction) and pgx v5 stdlib's
// no-op ResetSession (a pooled connection keeps its session state). Same
// "testable without a database" discipline as the canned driver in
// vwap_direction_combine_test.go, but this one has to be STATEFUL: the
// defect is invisible in the SQL text and only shows up on reuse.
// -----------------------------------------------------------------

type gucDML struct {
	stmt string
	// cap is the effective decompressionCapGUC when the statement ran.
	cap string
}

type gucConn struct {
	session map[string]string
	local   map[string]string
	inTx    bool
	stmts   []string
	dml     []gucDML
}

func newGUCConn() *gucConn { return &gucConn{session: map[string]string{}} }

// effective resolves a GUC the way Postgres does: the transaction-local
// value if one was set in this transaction, else the session value, else
// the built-in default.
func (c *gucConn) effective(name string) string {
	if v, ok := c.local[name]; ok {
		return v
	}
	if v, ok := c.session[name]; ok {
		return v
	}
	if name == decompressionCapGUC {
		return decompressionCapDefault
	}
	return ""
}

func (c *gucConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("gucConn: Prepare not implemented")
}
func (c *gucConn) Close() error { return nil }

func (c *gucConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *gucConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	if c.inTx {
		return nil, errors.New("gucConn: nested transaction")
	}
	c.inTx = true
	c.local = map[string]string{}
	return &gucTx{c: c}, nil
}

// ResetSession mirrors pgx v5 stdlib's DEFAULT: a no-op. It issues no
// DISCARD ALL / RESET ALL, so session GUCs survive into the pool — the
// whole reason a session SET here is a leak.
func (c *gucConn) ResetSession(context.Context) error { return nil }

func (c *gucConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.stmts = append(c.stmts, q)
	switch {
	case strings.HasPrefix(q, "SET LOCAL "):
		name, val, err := parseGUCSet(strings.TrimPrefix(q, "SET LOCAL "))
		if err != nil {
			return nil, err
		}
		if !c.inTx {
			// Postgres WARNs and IGNORES a SET LOCAL outside a
			// transaction block — model that, so a "fix" that sets it
			// on a bare connection shows up as an unlifted cap.
			return driver.RowsAffected(0), nil
		}
		c.local[name] = val
	case strings.HasPrefix(q, "SET "):
		name, val, err := parseGUCSet(strings.TrimPrefix(q, "SET "))
		if err != nil {
			return nil, err
		}
		c.session[name] = val
	case strings.HasPrefix(q, "RESET "):
		name := strings.TrimSpace(strings.TrimPrefix(q, "RESET "))
		delete(c.session, name)
		delete(c.local, name)
	default:
		c.dml = append(c.dml, gucDML{stmt: q, cap: c.effective(decompressionCapGUC)})
		return driver.RowsAffected(gucConnDMLRows), nil
	}
	return driver.RowsAffected(0), nil
}

func parseGUCSet(rest string) (name, val string, err error) {
	norm := strings.Replace(rest, " TO ", " = ", 1)
	name, val, ok := strings.Cut(norm, "=")
	if !ok {
		return "", "", errors.New("gucConn: unparseable SET: " + rest)
	}
	return strings.TrimSpace(name), strings.Trim(strings.TrimSpace(val), "'"), nil
}

type gucTx struct{ c *gucConn }

// Commit and Rollback both discard the transaction-local overlay — the
// Postgres semantics that make SET LOCAL safe on a pooled connection.
func (t *gucTx) Commit() error   { t.c.endTx(); return nil }
func (t *gucTx) Rollback() error { t.c.endTx(); return nil }

func (c *gucConn) endTx() {
	c.inTx = false
	c.local = nil
}

type gucConnector struct{ conn *gucConn }

func (c *gucConnector) Connect(context.Context) (driver.Conn, error) { return c.conn, nil }
func (c *gucConnector) Driver() driver.Driver                        { return gucDriver{} }

type gucDriver struct{}

func (gucDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("gucDriver: Open not implemented")
}
