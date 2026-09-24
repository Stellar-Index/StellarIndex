package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

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

// TestUSDVolumeRestamp_LogsBeforeImageInTheSameTransaction pins that every
// in-place trades.usd_volume rewrite — the exact tier and the plan-based
// tiers — first copies the rows' prior usd_volume/derive_generation into
// usd_volume_restamp_log, from the same FROM/WHERE as the UPDATE, inside
// one REPEATABLE READ transaction, so a bad run is reversible.
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
