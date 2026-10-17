package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ─── The exact-tier restamp formula ───────────────────────────────

// TestExactTierUSDVolume_TracksTheWaterfall pins that [ExactTierUSDVolume] writes exactly what
// [tradeUSDVolume] writes for every exact-tier shape, or the restamp repairs rows to values verify-usd-volume
// would flag.
func TestExactTierUSDVolume_TracksTheWaterfall(t *testing.T) {
	spec := testQuoteSpec(t)
	usdcAsset, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	xlm, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatal(err)
	}
	usdx, err := canonical.NewClassicAsset("USDX", "GBUYUAI75XXWDZEKLY66CFYKQPET5JR4EENXZBUZ3YXZ7DS56Z4OKOFU")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		source      string
		base, quote canonical.Asset
		baseAmt     int64
		quoteAmt    int64
		want        string
	}{
		// CEX stamps 1e8: 1,250,000,000 → $12.5.
		{"cex USD quote (tier 1/2, 8 decimals)", "binance", xlm, usd, 10_000_000_000, 1_250_000_000, "12.50000000"},
		// SDEX classic 1e7: 1,250,000,000 stroops → $125.
		{"dex pegged quote (tier 2, 7 decimals)", "sdex", canonical.NativeAsset(), usdcAsset, 10_000_000_000, 1_250_000_000, "125.00000000"},
		// Tier 2b reads the BASE leg — the mis-valued class.
		{"dex pegged base (tier 2b)", "sdex", usdcAsset, canonical.NativeAsset(), 1_250_000_000, 10_000_000_000, "125.00000000"},
		// Both legs pegged: the quote wins, and the amounts differ so
		// reading the wrong leg yields a different number.
		{"dex both legs pegged (quote wins)", "sdex", usdcAsset, usdx, 1_250_000_000, 9_990_000_000, "999.00000000"},
		// Sub-unit dust: the render must keep all 8 places.
		{"dex dust", "sdex", canonical.NativeAsset(), usdcAsset, 1, 3, "0.00000030"},
		// A zero pegged base is exactly $0 on insert; the restamp must agree or it cannot repair the row.
		{"dex pegged base zero (tier 2b)", "sdex", usdcAsset, canonical.NativeAsset(), 0, 3, "0.00000000"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pair, perr := canonical.NewPair(tc.base, tc.quote)
			if perr != nil {
				t.Fatal(perr)
			}
			tr := canonical.Trade{
				Source:      tc.source,
				Pair:        pair,
				BaseAmount:  canonical.NewAmount(big.NewInt(tc.baseAmt)),
				QuoteAmount: canonical.NewAmount(big.NewInt(tc.quoteAmt)),
			}
			written := tradeUSDVolume(context.Background(), tr, spec, nil)
			if written == nil {
				t.Fatalf("tradeUSDVolume declined an exact-tier fixture")
			}
			tier, decimals, cerr := ClassifyUSDVolumeTier(tc.source, tc.base.String(), tc.quote.String(), spec)
			if cerr != nil {
				t.Fatalf("ClassifyUSDVolumeTier: %v", cerr)
			}
			if !tier.Exact() {
				t.Fatalf("tier = %q, want an exact tier for this fixture", tier)
			}
			got, ok := ExactTierUSDVolume(tier, decimals, tr.BaseAmount.String(), tr.QuoteAmount.String())
			if !ok {
				t.Fatalf("ExactTierUSDVolume declined tier %q/%d", tier, decimals)
			}
			if got != *written {
				t.Errorf("restamp formula = %q but the insert path wrote %q — the two have drifted", got, *written)
			}
			if got != tc.want {
				t.Errorf("restamp formula = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExactTierUSDVolume_DeclinesWhatTheWaterfallDeclines: the waterfall returns nil (usd_volume NULL) for a
// non-positive quote before probing any tier, and tier 2b declines a negative base. The restamp must
// decline the same rows; "repairing" them to a number would be a new defect.
func TestExactTierUSDVolume_DeclinesWhatTheWaterfallDeclines(t *testing.T) {
	cases := []struct {
		name        string
		tier        USDVolumeTier
		base, quote string
	}{
		{"zero quote on quote-pegged", TierQuotePegged, "100", "0"},
		{"zero quote on base-pegged (waterfall bails first)", TierBasePegged, "100", "0"},
		{"negative base", TierBasePegged, "-5", "100"},
		{"garbage amount", TierQuotePegged, "100", "1e5"},
		{"estimated tier is not restampable", TierEstimated, "100", "100"},
		{"unvaluable tier is not restampable", TierUnvaluable, "100", "100"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := ExactTierUSDVolume(tc.tier, 7, tc.base, tc.quote); ok {
				t.Errorf("ExactTierUSDVolume = %q, ok=true; want declined", got)
			}
		})
	}
}

// TestUSDVolumeRestampDecision is the differential: a correctly-stamped row is reported unchanged (whatever
// NUMERIC render Postgres chose), a wrong row changed with its identity as target, and a NULL row only if
// the operator opted into filling coverage.
func TestUSDVolumeRestampDecision(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := []struct {
		name        string
		stored      *string
		tier        USDVolumeTier
		base, quote string
		fillNull    bool
		wantValue   string
		wantChanged bool
		wantOK      bool
	}{
		{"correct row, canonical render", str("125.00000000"), TierBasePegged, "1250000000", "10000000000", false, "125.00000000", false, true},
		{"correct row, short NUMERIC render", str("125"), TierBasePegged, "1250000000", "10000000000", false, "125.00000000", false, true},
		{"correct row, long NUMERIC render", str("125.0000000000000000"), TierQuotePegged, "10000000000", "1250000000", false, "125.00000000", false, true},
		// The mis-valued class: a USDC-base row valued by the resolver's VWAP
		// (+0.7%) instead of the $1 peg identity.
		{"resolver-priced base_pegged row", str("125.87500000"), TierBasePegged, "1250000000", "10000000000", false, "125.00000000", true, true},
		// One unit at the render scale — the smallest possible defect.
		{"off by 1e-8", str("125.00000001"), TierBasePegged, "1250000000", "10000000000", false, "125.00000000", true, true},
		{"NULL row, coverage fill off", nil, TierBasePegged, "1250000000", "10000000000", false, "125.00000000", false, true},
		{"NULL row, coverage fill on", nil, TierBasePegged, "1250000000", "10000000000", true, "125.00000000", true, true},
		{"corrupt stored render is not rewritten", str("12x"), TierBasePegged, "1250000000", "10000000000", false, "125.00000000", false, false},
		// The pre-#2890 dust class: zero USDC moved, valued off the other leg.
		{"zero pegged base restamps to $0", str("0.00000006"), TierBasePegged, "0", "3", false, "0.00000000", true, true},
		{"non-restampable row", str("125"), TierBasePegged, "-1", "10000000000", false, "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed, ok := USDVolumeRestampDecision(tc.stored, tc.tier, 7, tc.base, tc.quote, tc.fillNull)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if got != tc.wantValue {
				t.Errorf("want value = %q, want %q", got, tc.wantValue)
			}
		})
	}
}

// TestExactTierRestampScope_MirrorsTheDecision: the SQL scope must carry the generation guard, skip rows via
// IS DISTINCT FROM, exclude NULLs unless FillNull, bind the Go-decided leg and 10^decimals per group,
// and refuse non-exact groups.
func TestExactTierRestampScope_MirrorsTheDecision(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	groups := []USDVolumeRestampGroup{
		{Source: "sdex", BaseAsset: "USDC-GA5Z", QuoteAsset: "native", Tier: TierBasePegged, Decimals: 7},
		{Source: "kraken", BaseAsset: "crypto:XLM", QuoteAsset: "fiat:USD", Tier: TierQuotePegged, Decimals: 8},
	}
	p := USDVolumeRestampParams{Groups: groups, From: from, To: from.Add(time.Hour), Generation: 1_755_000_000}

	groupRel, where, identity, args, err := exactTierRestampScope(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(groupRel, "(VALUES ") || !strings.HasSuffix(groupRel, ") AS g(source, base_asset, quote_asset, leg, denom)") {
		t.Errorf("group relation = %q", groupRel)
	}
	fromWhere := where
	for _, want := range []string{
		"t.derive_generation <= $3",
		"t.usd_volume IS DISTINCT FROM round(",
		"t.usd_volume IS NOT NULL",
		"t.quote_amount > 0",
		// A zero pegged base leg is valued at exactly $0, so the restamp must reach it.
		"END) >= 0",
	} {
		if !strings.Contains(fromWhere, want) {
			t.Errorf("scope lacks %q:\n%s", want, fromWhere)
		}
	}
	if identity != "round((CASE WHEN g.leg = 'base' THEN t.base_amount ELSE t.quote_amount END) / g.denom, 8)" {
		t.Errorf("identity expression = %q", identity)
	}
	// $1..$3 then 5 per group, leg + denominator decided in Go.
	if len(args) != 3+5*len(groups) {
		t.Fatalf("args = %d, want %d", len(args), 3+5*len(groups))
	}
	if args[2] != int64(1_755_000_000) {
		t.Errorf("generation arg = %v", args[2])
	}
	if args[6] != "base" || args[7] != "10000000" {
		t.Errorf("sdex group bound (leg, denom) = (%v, %v), want (base, 10000000)", args[6], args[7])
	}
	if args[11] != "quote" || args[12] != "100000000" {
		t.Errorf("kraken group bound (leg, denom) = (%v, %v), want (quote, 100000000)", args[11], args[12])
	}

	p.FillNull = true
	_, fromWhere, _, _, err = exactTierRestampScope(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fromWhere, "IS NOT NULL") {
		t.Errorf("FillNull scope still excludes NULL rows:\n%s", fromWhere)
	}

	p.Groups = append(p.Groups, USDVolumeRestampGroup{Source: "sdex", BaseAsset: "native", QuoteAsset: "AQUA-GB", Tier: TierEstimated})
	if _, _, _, _, err := exactTierRestampScope(p); err == nil {
		t.Error("an estimated-tier group was accepted — the restamp would overwrite resolver-priced rows with quote/10^0")
	}
	if _, _, _, _, err := exactTierRestampScope(USDVolumeRestampParams{Groups: groups, From: from, To: from}); err == nil {
		t.Error("an empty window was accepted")
	}
}

// ─── The restamp's decompression-cap GUC must not escape ───────────

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

// TestRestampExactTierUSDVolume_DecompressionCapNeverEscapesTheTransaction: the cap must be `SET LOCAL`,
// because a session SET would ride the pooled connection and uncap later DML. The test asserts the cap is
// lifted for the restamp's UPDATE and no later statement sees anything but the default.
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

	// 1. the cap WAS lifted for the restamp's own DML: the before-image
	// INSERT and the trades UPDATE.
	if len(conn.dml) != 2 {
		t.Fatalf("restamp ran %d DML statements, want 2 (before-image, update): %q", len(conn.dml), conn.stmts)
	}
	if !strings.HasPrefix(conn.dml[1].stmt, "UPDATE trades") {
		t.Fatalf("restamp DML = %q, want the trades UPDATE", conn.dml[1].stmt)
	}
	for _, d := range conn.dml {
		if d.cap != "0" {
			t.Errorf("restamp DML %.40q ran with %s = %q, want %q — the compressed-chunk DML would abort at the default cap",
				d.stmt, decompressionCapGUC, d.cap, "0")
		}
	}

	// 2. …and it did NOT ride the connection back into the pool: the next
	// DML to land on it runs under the DEFAULT cap again.
	if _, err := store.db.ExecContext(ctx, "UPDATE trades SET usd_volume = usd_volume"); err != nil {
		t.Fatalf("later DML: %v", err)
	}
	if len(conn.dml) != 3 {
		t.Fatalf("saw %d DML statements, want 3: %q", len(conn.dml), conn.stmts)
	}
	if got := conn.dml[2].cap; got != decompressionCapDefault {
		t.Errorf("later DML on the pooled connection ran with %s = %q, want the default %q — the restamp leaked the lifted cap into the pool (#312)",
			decompressionCapGUC, got, decompressionCapDefault)
	}
	// 3. nothing session-level was left behind at all.
	if v, ok := conn.session[decompressionCapGUC]; ok {
		t.Errorf("session-level %s = %q left on the pooled connection; the restamp must scope it to its transaction",
			decompressionCapGUC, v)
	}
}

// GUC-scoping driver fake: models SET vs SET LOCAL scoping and pgx's no-op ResetSession. It is stateful
// because the defect only shows on connection reuse, not in the SQL text.

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
	// isolation is the level the last transaction was opened at.
	isolation driver.IsolationLevel
	commits   int
	// rowsFor, when set, overrides gucConnDMLRows per statement.
	rowsFor func(stmt string) int64
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

func (c *gucConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.inTx {
		return nil, errors.New("gucConn: nested transaction")
	}
	c.inTx = true
	c.isolation = opts.Isolation
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
		if c.rowsFor != nil {
			return driver.RowsAffected(c.rowsFor(q)), nil
		}
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
func (t *gucTx) Commit() error   { t.c.commits++; t.c.endTx(); return nil }
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

// restampLogFixtureDay is an arbitrary historical window for the fakes.
var restampLogFixtureDay = time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)

func newRestampLogStore(t *testing.T) (*Store, *gucConn) {
	t.Helper()
	conn := newGUCConn()
	store := &Store{db: sql.OpenDB(&gucConnector{conn: conn})}
	store.db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = store.db.Close() })
	return store, conn
}

func exactRestampLogParams() USDVolumeRestampParams {
	return USDVolumeRestampParams{
		Groups: []USDVolumeRestampGroup{{
			Source:     "sdex",
			BaseAsset:  "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			QuoteAsset: "native",
			Tier:       TierBasePegged,
			Decimals:   7,
		}},
		From:       restampLogFixtureDay,
		To:         restampLogFixtureDay.Add(time.Hour),
		Generation: 1_756_400_000,
	}
}

func planRestampLogFixture() *XLMBaseRestampPlan {
	return &XLMBaseRestampPlan{Rows: []XLMBaseRestampRow{{
		Source: "sdex", Ledger: 58_000_000, OpIndex: 1, TS: restampLogFixtureDay.Add(time.Minute),
		TxHash: strings.Repeat("ab", 32), Want: "12.50000000",
	}}}
}

// TestUSDVolumeRestamp_LogsBeforeImageInTheSameTransaction pins that every in-place trades.usd_volume rewrite
// first copies the rows' prior usd_volume/derive_generation into usd_volume_restamp_log, from the same
// FROM/WHERE as the UPDATE, inside one REPEATABLE READ transaction, so a bad run is reversible.
func TestUSDVolumeRestamp_LogsBeforeImageInTheSameTransaction(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(*Store) (int64, error){
		"exact tier": func(s *Store) (int64, error) { return s.RestampExactTierUSDVolume(ctx, exactRestampLogParams()) },
		"plan tiers": func(s *Store) (int64, error) {
			return s.ApplyUSDVolumeRestampPlan(ctx, planRestampLogFixture(), 1_756_400_000, 0)
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			store, conn := newRestampLogStore(t)
			if _, err := run(store); err != nil {
				t.Fatalf("restamp: %v", err)
			}
			if len(conn.dml) != 2 {
				t.Fatalf("restamp ran %d DML statement(s), want 2 (before-image, then update): %q", len(conn.dml), conn.stmts)
			}
			logStmt, update := conn.dml[0].stmt, conn.dml[1].stmt
			if !strings.HasPrefix(strings.TrimSpace(logStmt), "INSERT INTO usd_volume_restamp_log") {
				t.Fatalf("first DML = %q, want the before-image INSERT INTO usd_volume_restamp_log", logStmt)
			}
			if !strings.Contains(logStmt, "t.usd_volume, t.derive_generation") {
				t.Errorf("before-image does not copy the row's prior usd_volume and derive_generation: %q", logStmt)
			}
			if !strings.HasPrefix(update, "UPDATE trades t SET usd_volume") {
				t.Fatalf("second DML = %q, want the trades UPDATE", update)
			}
			_, logScope, ok1 := strings.Cut(logStmt, "FROM trades t, ")
			_, updScope, ok2 := strings.Cut(update, " FROM ")
			if !ok1 || !ok2 || logScope != updScope {
				t.Errorf("before-image and update select different rows:\n log: %q\n upd: %q", logScope, updScope)
			}
			if conn.isolation != driver.IsolationLevel(sql.LevelRepeatableRead) {
				t.Errorf("restamp transaction isolation = %v, want REPEATABLE READ (one snapshot for log + update)", sql.IsolationLevel(conn.isolation))
			}
			if conn.commits != 1 {
				t.Errorf("commits = %d, want 1", conn.commits)
			}
		})
	}
}

// TestUSDVolumeRestamp_RefusesToCommitAnUnloggedRewrite: when the UPDATE
// rewrites a row the before-image did not capture, nothing commits.
func TestUSDVolumeRestamp_RefusesToCommitAnUnloggedRewrite(t *testing.T) {
	store, conn := newRestampLogStore(t)
	conn.rowsFor = func(stmt string) int64 {
		if strings.HasPrefix(stmt, "UPDATE") {
			return 3
		}
		return 2
	}
	n, err := store.RestampExactTierUSDVolume(context.Background(), exactRestampLogParams())
	if err == nil || !strings.Contains(err.Error(), "before-image holds 2 row(s) but the update rewrote 3") {
		t.Fatalf("restamp = %d, %v; want the before-image/update count mismatch", n, err)
	}
	if n != 0 || conn.commits != 0 {
		t.Errorf("mismatched restamp returned %d and committed %d time(s); want 0, 0", n, conn.commits)
	}
}

// TestTradesUSDVolumeRewrites_GoThroughTheBeforeImage fails when a new
// `UPDATE trades … SET usd_volume` appears anywhere but
// [usdVolumeRestampWrite.statements], which pairs it with its log.
func TestTradesUSDVolumeRewrites_GoThroughTheBeforeImage(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	updateTrades := regexp.MustCompile(`(?i)UPDATE\s+trades\b`)
	setListEnd := regexp.MustCompile(`(?i)\b(FROM|WHERE)\b|;`)
	var sites []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		text := string(src)
		for _, loc := range updateTrades.FindAllStringIndex(text, -1) {
			rest := text[loc[1]:]
			if end := setListEnd.FindStringIndex(rest); end != nil {
				rest = rest[:end[0]]
			}
			if strings.Contains(rest, "usd_volume") {
				sites = append(sites, f)
			}
		}
	}
	if len(sites) != 1 || sites[0] != "usd_volume_restamp.go" {
		t.Errorf("in-place trades.usd_volume rewrites found in %q; want exactly one, in usd_volume_restamp.go's usdVolumeRestampWrite.statements — route new ones through Store.restampTradesUSDVolume so the before-image is logged", sites)
	}
}

// The batch UPDATE must be prunable to the chunk it targets: bounding `t.ts` by the batch min and max lets
// the planner drop other chunks, avoiding wholesale decompression of compressed ones. These pin the statement
// and binds.

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

// TestApplyXLMBaseUSDVolumeRestamp_BatchIsPrunableToItsChunk pins that the statement carries `ts` bounds
// bound to the batch's own extremes, so no matching row is excluded and no chunk outside its span is planned.
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
	updates := captureUpdates(conn.dml)
	if len(updates) != 1 {
		t.Fatalf("ran %d UPDATE(s), want 1: %q", len(updates), conn.stmts)
	}
	got := updates[0]

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

// TestApplyXLMBaseUSDVolumeRestamp_BatchStaysUnderTheParameterCeiling pins the split: `-chunk-batch` defaults
// to 20,000 rows at xlmBaseRestampArgsPerRow placeholders per row plus 3, and the extended protocol carries
// at most 65,535, so an unsplit batch aborts the run mid-walk.
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
	updates := captureUpdates(conn.dml)
	if len(updates) < 2 {
		t.Fatalf("a %d-row batch ran in %d UPDATE(s); it must be split to stay under the protocol ceiling", rows, len(updates))
	}
	for i, d := range conn.dml {
		if len(d.args) > 65535 {
			t.Errorf("statement %d bound %d parameters, over the protocol's 65,535 — pgx refuses this Exec", i, len(d.args))
		}
	}
	var seen int
	for _, d := range updates {
		seen += (len(d.args) - 3) / xlmBaseRestampArgsPerRow
	}
	if seen != rows {
		t.Errorf("the split statements carry %d row(s), want %d — the batch must be divided, not truncated", seen, rows)
	}
}

// Statement-capturing driver fake: records each DML's text, bound arguments and the transaction-local GUCs
// in force. Kept separate from the GUC fake earlier in this file so neither test constrains the
// other's fake.

type captureDML struct {
	stmt      string
	args      []driver.NamedValue
	planCache string
}

// captureUpdates drops the before-image INSERTs, leaving the UPDATEs.
func captureUpdates(dml []captureDML) []captureDML {
	var out []captureDML
	for _, d := range dml {
		if strings.HasPrefix(d.stmt, "UPDATE") {
			out = append(out, d)
		}
	}
	return out
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
