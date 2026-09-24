package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// writeMinimalConfig drops a TOML config that has just enough fields
// for parseBackfillFlags + config.LoadWithEnv to succeed. The
// dispatcher / store / passphrase fields don't matter for these
// tests — we exit before opening any of them via -dry-run.
func writeMinimalConfig(t *testing.T, sources []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "stellarindex.toml")
	body := `
[region]
id   = "r1"
name = "TestRegion"

[stellar]
network = "pubnet"

[storage]
postgres_dsn = "postgres://x:y@localhost/test?sslmode=disable"
s3_endpoint  = "http://127.0.0.1:9000"
s3_bucket_archive = "galexie-archive"
s3_bucket_live    = "galexie-live"
s3_region    = "r1"

[ingestion]
enabled_sources = ["` + strings.Join(sources, `","`) + `"]
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBackfill_RejectsMissingFlags locks down the no-default-genesis
// guard. Operators have wiped the trades hypertable by typing
// `backfill -config PATH` without -from before — the flag is
// required so a fat-finger can't trigger a multi-day genesis replay.
func TestBackfill_RejectsMissingFlags(t *testing.T) {
	cfg := writeMinimalConfig(t, []string{"sdex"})
	cases := []struct {
		name       string
		args       []string
		wantSubstr string
	}{
		{"missing-config", []string{"-from", "100", "-to", "200"}, "-config required"},
		{"missing-from", []string{"-config", cfg, "-to", "200"}, "-from must be > 0"},
		{"to-equals-from", []string{"-config", cfg, "-from", "100", "-to", "100"}, "must be > -from"},
		{"to-below-from", []string{"-config", cfg, "-from", "100", "-to", "50"}, "must be > -from"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseBackfillFlags(tc.args)
			if err == nil {
				t.Fatalf("expected error containing %q; got nil", tc.wantSubstr)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

// TestBackfill_BackfillSafeGate is the load-bearing safety check.
// Every on-chain Soroban source defaults to BackfillSafe=false in
// the registry until its WASM-history audit lands. This test
// confirms the subcommand REFUSES to start when the operator's
// source list contains an unsafe source — and that the error
// message names the source so they know which audit to run.
// TestBackfill_AllSorobanSourcesPass confirms the gate accepts
// every audited on-chain Soroban source. As of 2026-04-29 all 8
// sources (soroswap, phoenix, aquarius, comet, reflector-{dex,cex,
// fx}, redstone, band) have completed their WASM-history audits
// (see docs/operations/wasm-audits/) and should pass cleanly.
//
// The gate-rejects-unsafe path is unit-tested in
// TestUnsafeBackfillSources_PureFunction below using a synthetic
// source name (which bypasses config validation) — that's where
// the regression coverage for "an unaudited source must be refused"
// lives. This test is the positive-side counterpart.
func TestBackfill_AllSorobanSourcesPass(t *testing.T) {
	// Use the DEX subset — oracle sources need oracle.* contract
	// IDs in config which writeMinimalConfig doesn't populate.
	// (Oracle sources also flipped 2026-04-29; the gate logic is
	// the same.)
	cfg := writeMinimalConfig(t, []string{"soroswap", "phoenix", "aquarius", "comet", "sdex"})
	args := []string{"-config", cfg, "-from", "21000000", "-to", "21001000", "-dry-run"}
	_, _, err := parseBackfillFlags(args)
	if err != nil {
		t.Fatalf("expected acceptance — every Soroban DEX source has been audited; got: %v", err)
	}
}

// TestBackfill_AllSafeSourcesAccepted confirms the gate doesn't
// false-positive: a list that's all BackfillSafe=true sources (sdex
// is the canonical pre-Soroban one + every off-chain CEX) reaches
// the dry-run output without complaint.
func TestBackfill_AllSafeSourcesAccepted(t *testing.T) {
	cfg := writeMinimalConfig(t, []string{"sdex"})
	args := []string{"-config", cfg, "-from", "21000000", "-to", "21001000", "-dry-run"}
	opts, _, err := parseBackfillFlags(args)
	if err != nil {
		t.Fatalf("expected acceptance for sdex-only list; got: %v", err)
	}
	if opts.from != 21000000 || opts.to != 21001000 {
		t.Errorf("opts.from/to mismatch: got [%d, %d]", opts.from, opts.to)
	}
	if !opts.dryRun {
		t.Error("opts.dryRun should be true")
	}
	if len(opts.sources) != 1 || opts.sources[0] != "sdex" {
		t.Errorf("opts.sources = %v, want [sdex]", opts.sources)
	}
}

// TestBackfill_SourceFlagOverridesConfig verifies the -source CSV
// overrides the config's enabled_sources, so an operator with
// `[soroswap, aquarius, sdex]` in the config can still backfill a
// subset (e.g. just sdex while the Soroban audits land).
func TestBackfill_SourceFlagOverridesConfig(t *testing.T) {
	// Every audited Soroban source is BackfillSafe=true now, so
	// this test verifies the override mechanism with a config that
	// has multiple safe sources and the operator narrows to one.
	cfg := writeMinimalConfig(t, []string{"soroswap", "aquarius", "sdex"})
	args := []string{"-config", cfg, "-from", "100", "-to", "200", "-source", "sdex", "-dry-run"}
	opts, _, err := parseBackfillFlags(args)
	if err != nil {
		t.Fatalf("override should let sdex-only through; got: %v", err)
	}
	if len(opts.sources) != 1 || opts.sources[0] != "sdex" {
		t.Errorf("opts.sources = %v, want [sdex]", opts.sources)
	}
}

// TestBackfill_BucketOverride verifies -bucket replaces the default
// cfg.Storage.S3BucketArchive. Useful for ad-hoc replays against a
// staging bucket without editing the live config.
func TestBackfill_BucketOverride(t *testing.T) {
	cfg := writeMinimalConfig(t, []string{"sdex"})
	args := []string{"-config", cfg, "-from", "100", "-to", "200", "-bucket", "scratch-bucket", "-dry-run"}
	opts, _, err := parseBackfillFlags(args)
	if err != nil {
		t.Fatal(err)
	}
	if opts.bucket != "scratch-bucket" {
		t.Errorf("opts.bucket = %q, want scratch-bucket", opts.bucket)
	}
}

// TestBackfillCursorSub_StableAcrossSourceOrder verifies the cursor
// key construction is deterministic regardless of how the operator
// types -source. Without sorting, `-source soroswap,sdex` and
// `-source sdex,soroswap` produce different cursor rows and a
// resume after a typo-driven re-order silently re-runs the whole
// range — exactly the failure mode -resume exists to prevent.
func TestBackfillCursorSub_StableAcrossSourceOrder(t *testing.T) {
	a := backfillOpts{from: 100, to: 200, sources: []string{"soroswap", "sdex", "binance"}}
	b := backfillOpts{from: 100, to: 200, sources: []string{"binance", "sdex", "soroswap"}}
	if backfillCursorSub(a) != backfillCursorSub(b) {
		t.Errorf("cursor sub depends on source-list order:\n  a=%q\n  b=%q",
			backfillCursorSub(a), backfillCursorSub(b))
	}
}

// TestBackfillCursorSub_DistinctRangesAndSources confirms that
// changing the range OR the source list produces a different
// cursor row — a different replay shouldn't share state with a
// previous one.
func TestBackfillCursorSub_DistinctRangesAndSources(t *testing.T) {
	base := backfillOpts{from: 100, to: 200, sources: []string{"sdex"}}

	cases := []struct {
		name string
		opts backfillOpts
	}{
		{"different-from", backfillOpts{from: 101, to: 200, sources: []string{"sdex"}}},
		{"different-to", backfillOpts{from: 100, to: 201, sources: []string{"sdex"}}},
		{"different-sources", backfillOpts{from: 100, to: 200, sources: []string{"binance"}}},
		{"extra-source", backfillOpts{from: 100, to: 200, sources: []string{"sdex", "binance"}}},
	}
	baseSub := backfillCursorSub(base)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := backfillCursorSub(tc.opts)
			if got == baseSub {
				t.Errorf("cursor sub collided with base: both = %q", got)
			}
		})
	}
}

// TestUnsafeBackfillSources_PureFunction is a unit-level check on
// the helper itself — proves we filter rather than fail-on-first.
// The error message in the gate test relies on getting the FULL
// unsafe list back, not just the first one.
func TestUnsafeBackfillSources_PureFunction(t *testing.T) {
	// As of 2026-04-29 every on-chain Soroban source has been
	// audited; use a synthetic typo'd source to exercise the
	// fail-closed Lookup-fallback path. Same shape as the gate
	// test above.
	got := unsafeBackfillSources([]string{"unknown-future-source", "binance", "sdex", "kraken", "comet"})
	want := []string{"unknown-future-source"}
	if len(got) != len(want) {
		t.Fatalf("unsafeBackfillSources returned %d entries, want %d: %v", len(got), len(want), got)
	}
	for i, s := range want {
		if got[i] != s {
			t.Errorf("entry %d: got %q, want %q", i, got[i], s)
		}
	}

	// Empty input → empty output (not nil-vs-empty drama in the
	// caller — caller does len() check).
	if out := unsafeBackfillSources(nil); len(out) != 0 {
		t.Errorf("empty input should return empty; got %v", out)
	}
}

// TestIsKnownSupplyObserverName — F-1243 wants `stellarindex-ops
// backfill accounts` to fail with a supply-observer-aware error
// rather than the generic "WASM-hash audit pending" message
// (which is misleading for these names — they're not Soroban
// sources at all). Pin the closed set so a future supply-observer
// addition that forgets to update this map will surface in a code
// review or a failing test rather than a misleading prod error.
func TestIsKnownSupplyObserverName(t *testing.T) {
	supplyNames := []string{
		"accounts",
		"trustlines",
		"claimable_balances",
		"sac_balances",
		"sep41_supply",
		"liquidity_pools",
	}
	for _, n := range supplyNames {
		if !isKnownSupplyObserverName(n) {
			t.Errorf("isKnownSupplyObserverName(%q) = false; want true", n)
		}
	}
	for _, n := range []string{"binance", "soroswap", "comet", "unknown-future-source"} {
		if isKnownSupplyObserverName(n) {
			t.Errorf("isKnownSupplyObserverName(%q) = true; want false (it's a price/oracle source, not a supply observer)", n)
		}
	}
}

// TestCheckBackfillSources_TailoredErrors — F-1243 wants the
// supply-observer error message to differ from the WASM-audit one
// so operators don't waste time auditing decoders for sources
// that aren't Soroban sources at all.
func TestCheckBackfillSources_TailoredErrors(t *testing.T) {
	t.Run("supply observer surfaces the supply-specific message", func(t *testing.T) {
		err := checkBackfillSources([]string{"accounts"}, 100, 200)
		if err == nil {
			t.Fatal("expected error for supply-observer backfill request")
		}
		if !strings.Contains(err.Error(), "supply observers") {
			t.Errorf("error %q should mention 'supply observers'", err.Error())
		}
		if strings.Contains(err.Error(), "WASM-hash audit") {
			t.Errorf("supply-observer error should NOT reference the WASM-audit gate; got %q", err.Error())
		}
	})
	t.Run("genuine audit-pending soroban source surfaces the wasm-audit message", func(t *testing.T) {
		err := checkBackfillSources([]string{"unknown-future-source"}, 100, 200)
		if err == nil {
			t.Fatal("expected error for unknown source")
		}
		if !strings.Contains(err.Error(), "WASM-hash audit") {
			t.Errorf("audit-pending error should mention 'WASM-hash audit'; got %q", err.Error())
		}
	})
	t.Run("clean source list returns nil", func(t *testing.T) {
		if err := checkBackfillSources([]string{"binance", "soroswap"}, 100, 200); err != nil {
			t.Errorf("clean source list should pass; got %v", err)
		}
	})
}

// planBackfillChunks tests — the parallelism plan must satisfy two
// invariants: the union of chunks covers [from, to] exactly with no
// gaps and no overlaps; chunk count equals the requested parallel
// (clamped to range size when the operator asks for more workers
// than ledgers).
func TestPlanBackfillChunks(t *testing.T) {
	cases := []struct {
		name     string
		from     uint32
		to       uint32
		n        int
		wantLen  int
		wantLast uint32 // last chunk's `to` should equal `to`
	}{
		{"sequential n=1", 100, 200, 1, 1, 200},
		{"even split n=4", 100, 199, 4, 4, 199},
		{"uneven split n=3 absorbs remainder in last", 100, 200, 3, 3, 200},
		{"n=0 treated as 1 (defensive)", 100, 200, 0, 1, 200},
		{"workers > range — degrades", 100, 102, 8, 3, 102},
		{"single ledger range", 500, 500, 4, 1, 500},
		{"adjacent to uint32 max", 4_294_967_290, 4_294_967_295, 2, 2, 4_294_967_295},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planBackfillChunks(tc.from, tc.to, tc.n)
			if len(got) != tc.wantLen {
				t.Fatalf("len = %d, want %d (chunks: %v)", len(got), tc.wantLen, got)
			}
			if got[0].from != tc.from {
				t.Errorf("first chunk.from = %d, want %d", got[0].from, tc.from)
			}
			if got[len(got)-1].to != tc.wantLast {
				t.Errorf("last chunk.to = %d, want %d", got[len(got)-1].to, tc.wantLast)
			}
			// Coverage invariant: chunks are contiguous + non-overlapping.
			for i := 1; i < len(got); i++ {
				if got[i].from != got[i-1].to+1 {
					t.Errorf("chunk %d.from = %d, want %d (no gap, no overlap with chunk %d)",
						i, got[i].from, got[i-1].to+1, i-1)
				}
			}
		})
	}
}

// TestParseBackfillFlags_Parallel — exercise the flag's
// validation without the full integration plumbing.
func TestParseBackfillFlags_Parallel(t *testing.T) {
	cfgPath := writeMinimalConfig(t, []string{"sdex"})
	t.Run("default is 1", func(t *testing.T) {
		opts, _, err := parseBackfillFlags([]string{"-config", cfgPath, "-from", "100", "-to", "200", "-dry-run"})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if opts.parallel != 1 {
			t.Errorf("parallel = %d, want 1", opts.parallel)
		}
	})
	t.Run("explicit 8 accepted", func(t *testing.T) {
		opts, _, err := parseBackfillFlags([]string{"-config", cfgPath, "-from", "100", "-to", "200", "-parallel", "8", "-dry-run"})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if opts.parallel != 8 {
			t.Errorf("parallel = %d, want 8", opts.parallel)
		}
	})
	t.Run("zero rejected", func(t *testing.T) {
		_, _, err := parseBackfillFlags([]string{"-config", cfgPath, "-from", "100", "-to", "200", "-parallel", "0", "-dry-run"})
		if err == nil {
			t.Fatal("expected error for parallel=0")
		}
	})
	t.Run("negative rejected", func(t *testing.T) {
		_, _, err := parseBackfillFlags([]string{"-config", cfgPath, "-from", "100", "-to", "200", "-parallel", "-3", "-dry-run"})
		if err == nil {
			t.Fatal("expected error for parallel=-3")
		}
	})
}

// fakeCAGGRefresher is a DB-free caggRefresher: canned
// LedgerRangeToTimeRange + per-view RefreshContinuousAggregate
// results, so refreshCAGGsForChunk's failure-aggregation logic
// (DAT-09 / REL-08) is exercisable without live Postgres.
type fakeCAGGRefresher struct {
	tsFrom, tsTo time.Time
	rangeErr     error
	// oracleFrom/oracleTo is the chunk's oracle_updates span; zero means
	// the chunk wrote no oracle rows.
	oracleFrom, oracleTo time.Time
	armed                bool
	failViews            map[string]error // view name -> error to return

	refreshedViews []string                // every view refreshed, forced or not, in call order
	forced         map[string]bool         // views refreshed with force => true
	windows        map[string][2]time.Time // view -> the window it was refreshed over
}

func (f *fakeCAGGRefresher) LedgerRangeToTimeRange(_ context.Context, _, _ uint32) (time.Time, time.Time, error) {
	return f.tsFrom, f.tsTo, f.rangeErr
}

func (f *fakeCAGGRefresher) LedgerRangeToOracleTimeRange(_ context.Context, _, _ uint32) (time.Time, time.Time, error) {
	if f.oracleFrom.IsZero() {
		return time.Time{}, time.Time{}, timescale.ErrNotFound
	}
	return f.oracleFrom, f.oracleTo, nil
}

func (f *fakeCAGGRefresher) Prices1mRetentionArmed(context.Context) (bool, error) {
	return f.armed, nil
}

func (f *fakeCAGGRefresher) RefreshContinuousAggregate(_ context.Context, name string, from, to time.Time) error {
	f.refreshedViews = append(f.refreshedViews, name)
	if f.windows == nil {
		f.windows = map[string][2]time.Time{}
	}
	f.windows[name] = [2]time.Time{from, to}
	return f.failViews[name]
}

func (f *fakeCAGGRefresher) RefreshContinuousAggregateForced(ctx context.Context, name string, from, to time.Time) error {
	if f.forced == nil {
		f.forced = map[string]bool{}
	}
	f.forced[name] = true
	return f.RefreshContinuousAggregate(ctx, name, from, to)
}

// independentTradesView is a trades aggregate no other view is built on,
// so failing it must not stop any other refresh.
func independentTradesView(t *testing.T) string {
	t.Helper()
	for _, c := range timescale.TradesCAGGs {
		if c.Name != "prices_1m" && !slices.Contains(timescale.CAGGsOnPrices1m, c.Name) {
			return c.Name
		}
	}
	t.Fatal("timescale.TradesCAGGs has no view independent of prices_1m")
	return ""
}

func viewNames(specs ...[]timescale.CAGGSpec) []string {
	var out []string
	for _, s := range specs {
		for _, c := range s {
			out = append(out, c.Name)
		}
	}
	return out
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRefreshCAGGsForChunk_ViewFailurePropagates is the DAT-09 / REL-08
// regression: a single failing CAGG view must make the WHOLE function
// return a non-nil error (previously it logged and returned nil),
// so the caller does not advance the durable cursor past an
// unmaterialised chunk. Every OTHER view must still be attempted —
// one wedged view must not block refreshing the rest.
func TestRefreshCAGGsForChunk_ViewFailurePropagates(t *testing.T) {
	// Fail a view nothing else is built on — its identity is not what
	// this test is about (a failed prices_1m has its own test, since its
	// dependants are skipped) — and confirm every view was still attempted.
	failing := independentTradesView(t)
	fake := &fakeCAGGRefresher{
		tsFrom:    time.Now().Add(-time.Hour),
		tsTo:      time.Now(),
		failViews: map[string]error{failing: fmt.Errorf("view refresh failed: statement timeout")},
	}
	err := refreshCAGGsForChunk(context.Background(), discardLogger(), fake, chunkRange{from: 100, to: 200})
	if err == nil {
		t.Fatal("expected a non-nil error when a CAGG view failed to refresh, got nil")
	}
	if !strings.Contains(err.Error(), failing) {
		t.Errorf("error should name the failing view %q, got: %v", failing, err)
	}
	if len(fake.refreshedViews) != len(timescale.TradesCAGGs) {
		t.Errorf("expected every configured CAGG view attempted despite one failure, got %d/%d: %v",
			len(fake.refreshedViews), len(timescale.TradesCAGGs), fake.refreshedViews)
	}
}

// TestRefreshCAGGsForChunk_AllSucceedIsNil: the happy path must still
// return nil (regression guard against over-correcting into
// always-error).
func TestRefreshCAGGsForChunk_AllSucceedIsNil(t *testing.T) {
	fake := &fakeCAGGRefresher{tsFrom: time.Now().Add(-time.Hour), tsTo: time.Now()}
	err := refreshCAGGsForChunk(context.Background(), discardLogger(), fake, chunkRange{from: 100, to: 200})
	if err != nil {
		t.Fatalf("expected nil error when every view refreshed cleanly, got %v", err)
	}
	if len(fake.refreshedViews) != len(timescale.TradesCAGGs) {
		t.Errorf("expected every configured CAGG view attempted, got %d/%d", len(fake.refreshedViews), len(timescale.TradesCAGGs))
	}
}

// TestRefreshCAGGsForChunk_RefreshesEveryAggregateTheChunkFeeds is the
// GH-687 regression. A chunk that wrote trades and oracle rows must
// refresh every aggregate rooted on either table — not only the seven
// prices_* rungs — because none of their policies reach a historical
// range: twap_1h 4 h, twap_1d and dex_volume_by_pair_1d 7 d,
// oracle_prices_1d 7 d, against readers serving a year or a lifetime.
// twap_* must follow a FORCED prices_1m whose window covers theirs, and
// each root's views must be refreshed over that root's own time span.
func TestRefreshCAGGsForChunk_RefreshesEveryAggregateTheChunkFeeds(t *testing.T) {
	tradesFrom := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	oracleFrom := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	fake := &fakeCAGGRefresher{
		tsFrom: tradesFrom, tsTo: tradesFrom.Add(7 * 24 * time.Hour),
		oracleFrom: oracleFrom, oracleTo: oracleFrom.Add(7 * 24 * time.Hour),
	}
	if err := refreshCAGGsForChunk(context.Background(), discardLogger(), fake, chunkRange{from: 100, to: 200}); err != nil {
		t.Fatalf("refreshCAGGsForChunk: %v", err)
	}
	want := viewNames(timescale.TradesCAGGs, timescale.OracleCAGGs)
	got := slices.Clone(fake.refreshedViews)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("refreshed %v, want every trades and oracle aggregate exactly once: %v", fake.refreshedViews, want)
	}
	minute := slices.Index(fake.refreshedViews, "prices_1m")
	for _, v := range timescale.CAGGsOnPrices1m {
		if i := slices.Index(fake.refreshedViews, v); i < minute {
			t.Errorf("%s refreshed at %d, before prices_1m (%d) which it is built on", v, i, minute)
		}
		if !fake.forced["prices_1m"] || !fake.forced[v] {
			t.Errorf("prices_1m and %s must both be forced; forced = %v", v, fake.forced)
		}
		if w, m := fake.windows[v], fake.windows["prices_1m"]; w[0].Before(m[0]) || w[1].After(m[1]) {
			t.Errorf("%s window %v is not inside prices_1m's forced window %v", v, w, m)
		}
	}
	for _, c := range timescale.OracleCAGGs {
		if w := fake.windows[c.Name]; w[0].After(fake.oracleFrom) || w[1].Before(fake.oracleTo) || !w[1].Before(tradesFrom) {
			t.Errorf("%s refreshed over %v, want the oracle rows' span [%s, %s], not the trades'",
				c.Name, w, fake.oracleFrom, fake.oracleTo)
		}
	}
}

// TestRefreshCAGGsForChunk_OracleOnlyChunkRefreshesOracleViews: a chunk
// of an oracle-only backfill writes no trades, and that must not skip
// the oracle aggregates.
func TestRefreshCAGGsForChunk_OracleOnlyChunkRefreshesOracleViews(t *testing.T) {
	oracleFrom := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	fake := &fakeCAGGRefresher{rangeErr: timescale.ErrNotFound, oracleFrom: oracleFrom, oracleTo: oracleFrom.Add(time.Hour)}
	if err := refreshCAGGsForChunk(context.Background(), discardLogger(), fake, chunkRange{from: 100, to: 200}); err != nil {
		t.Fatalf("refreshCAGGsForChunk: %v", err)
	}
	if want := viewNames(timescale.OracleCAGGs); !slices.Equal(fake.refreshedViews, want) {
		t.Errorf("refreshed %v, want exactly the oracle aggregates %v", fake.refreshedViews, want)
	}
}

// TestRefreshCAGGsForChunk_RefusesTwapsWhilePrices1mRetentionIsArmed:
// with migration 0156's retention armed, a twap refresh could read minute
// rows the policy drops, so it is refused and the chunk fails — while
// every other view is still refreshed.
func TestRefreshCAGGsForChunk_RefusesTwapsWhilePrices1mRetentionIsArmed(t *testing.T) {
	fake := &fakeCAGGRefresher{tsFrom: time.Now().Add(-time.Hour), tsTo: time.Now(), armed: true}
	err := refreshCAGGsForChunk(context.Background(), discardLogger(), fake, chunkRange{from: 100, to: 200})
	if err == nil {
		t.Fatal("an armed prices_1m retention must fail the chunk's twap refresh, got nil")
	}
	for _, v := range timescale.CAGGsOnPrices1m {
		if !strings.Contains(err.Error(), v) || slices.Contains(fake.refreshedViews, v) {
			t.Errorf("%s must be refused and named, got err %v, refreshed %v", v, err, fake.refreshedViews)
		}
	}
	if want := len(timescale.TradesCAGGs) - len(timescale.CAGGsOnPrices1m); len(fake.refreshedViews) != want {
		t.Errorf("refreshed %d views %v, want the other %d", len(fake.refreshedViews), fake.refreshedViews, want)
	}
}

// TestRefreshCAGGsForChunk_FailedPrices1mSkipsItsDependants: twap_*
// recomputed over a minute range whose forced refresh failed would be
// built from stale or dropped minute rows, so they are not attempted.
func TestRefreshCAGGsForChunk_FailedPrices1mSkipsItsDependants(t *testing.T) {
	fake := &fakeCAGGRefresher{
		tsFrom: time.Now().Add(-time.Hour), tsTo: time.Now(),
		failViews: map[string]error{"prices_1m": fmt.Errorf("statement timeout")},
	}
	err := refreshCAGGsForChunk(context.Background(), discardLogger(), fake, chunkRange{from: 100, to: 200})
	if err == nil {
		t.Fatal("a failed prices_1m refresh must fail the chunk, got nil")
	}
	for _, v := range timescale.CAGGsOnPrices1m {
		if slices.Contains(fake.refreshedViews, v) || !strings.Contains(err.Error(), v) {
			t.Errorf("%s must be skipped and named after prices_1m failed; err %v, refreshed %v", v, err, fake.refreshedViews)
		}
	}
}

// TestRefreshCAGGsForChunk_NoTradesIsNil: an empty chunk (no trades
// inserted — ErrNotFound from LedgerRangeToTimeRange) is a legitimate
// skip, not a failure.
func TestRefreshCAGGsForChunk_NoTradesIsNil(t *testing.T) {
	fake := &fakeCAGGRefresher{rangeErr: timescale.ErrNotFound}
	err := refreshCAGGsForChunk(context.Background(), discardLogger(), fake, chunkRange{from: 100, to: 200})
	if err != nil {
		t.Fatalf("expected nil error when the chunk had no trades, got %v", err)
	}
	if len(fake.refreshedViews) != 0 {
		t.Errorf("expected no views attempted when there's no ts range to refresh, got %v", fake.refreshedViews)
	}
}

// serialisationProbeRefresher is a caggRefresher that reports whether
// two callers were ever inside RefreshContinuousAggregate at the same
// time. It stands in for TimescaleDB's actual behaviour on that
// overlap — an immediate 55P03 to the loser, which the real store
// retries for a fixed ~3s budget and then surfaces as a hard error,
// fatal to the chunk per DAT-09 / REL-08.
type serialisationProbeRefresher struct {
	tsFrom, tsTo time.Time

	mu       sync.Mutex
	inFlight int
	maxSeen  int
	calls    int
}

func (p *serialisationProbeRefresher) LedgerRangeToTimeRange(_ context.Context, _, _ uint32) (time.Time, time.Time, error) {
	return p.tsFrom, p.tsTo, nil
}

func (p *serialisationProbeRefresher) LedgerRangeToOracleTimeRange(_ context.Context, _, _ uint32) (time.Time, time.Time, error) {
	return p.tsFrom, p.tsTo, nil
}

func (p *serialisationProbeRefresher) Prices1mRetentionArmed(context.Context) (bool, error) {
	return false, nil
}

func (p *serialisationProbeRefresher) RefreshContinuousAggregateForced(ctx context.Context, name string, from, to time.Time) error {
	return p.RefreshContinuousAggregate(ctx, name, from, to)
}

func (p *serialisationProbeRefresher) RefreshContinuousAggregate(_ context.Context, _ string, _, _ time.Time) error {
	p.mu.Lock()
	p.inFlight++
	p.calls++
	if p.inFlight > p.maxSeen {
		p.maxSeen = p.inFlight
	}
	p.mu.Unlock()

	// Long enough that unsynchronised workers overlap reliably; the
	// real prices_1m refresh is minutes, not microseconds.
	time.Sleep(2 * time.Millisecond)

	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()
	return nil
}

// TestRefreshCAGGsForChunk_SerialisesAcrossParallelWorkers is the
// -parallel hazard: every worker `-parallel N` starts lives in ONE
// process and walks the same CAGG list, and TimescaleDB answers a
// second refresh of the same view with an immediate 55P03 rather than
// a wait. With prices_1m leading the list, the losing worker's bounded
// retry can expire against a refresh whose cost is the chunk's trade
// count — and a refresh error is fatal to the chunk, so the cursor
// never checkpoints and the documented weekly loop halts.
//
// The guard is behavioural, not structural: run the workers and assert
// that no two were ever inside a refresh together.
func TestRefreshCAGGsForChunk_SerialisesAcrossParallelWorkers(t *testing.T) {
	const workers = 4
	probe := &serialisationProbeRefresher{
		tsFrom: time.Now().Add(-time.Hour),
		tsTo:   time.Now(),
	}
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			chunk := chunkRange{from: uint32(100 + i*100), to: uint32(200 + i*100)}
			if err := refreshCAGGsForChunk(context.Background(), discardLogger(), probe, chunk); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("worker returned an error: %v", err)
	}

	probe.mu.Lock()
	maxSeen, calls := probe.maxSeen, probe.calls
	probe.mu.Unlock()

	views := len(timescale.TradesCAGGs) + len(timescale.OracleCAGGs)
	if want := workers * views; calls != want {
		t.Errorf("got %d refresh calls, want %d (%d workers x %d views)",
			calls, want, workers, views)
	}
	if maxSeen != 1 {
		t.Errorf("%d refreshes were in flight at once — every -parallel worker is in this process, "+
			"and TimescaleDB rejects the loser of a same-view race with 55P03 instead of blocking it. "+
			"The refresh loop must hold caggRefreshMu so the retry budget is never spent on a "+
			"collision this process created", maxSeen)
	}
}

// TestRefreshCAGGsForChunk_TimeoutIsFatalAndNamed is the W8-19 caller
// half: a per-CALL bound firing on one view surfaces as a chunk
// failure that names that view (the cursor must not advance), every
// other view is still attempted, and the typed error is reachable on
// the chain so the log line can carry the window and the bound.
func TestRefreshCAGGsForChunk_TimeoutIsFatalAndNamed(t *testing.T) {
	timedOut := independentTradesView(t)
	tsFrom, tsTo := time.Now().Add(-time.Hour), time.Now()
	fake := &fakeCAGGRefresher{
		tsFrom: tsFrom,
		tsTo:   tsTo,
		failViews: map[string]error{timedOut: &timescale.CAGGRefreshTimeoutError{
			View: timedOut, From: tsFrom, To: tsTo, Timeout: 10 * time.Minute,
			Err: fmt.Errorf("canceling statement due to statement timeout (SQLSTATE 57014)"),
		}},
	}
	err := refreshCAGGsForChunk(context.Background(), discardLogger(), fake, chunkRange{from: 100, to: 200})
	if err == nil {
		t.Fatal("a timed-out CAGG refresh must fail the chunk, got nil")
	}
	if !strings.Contains(err.Error(), timedOut) {
		t.Errorf("error should name the timed-out view %q, got: %v", timedOut, err)
	}
	if len(fake.refreshedViews) != len(timescale.TradesCAGGs) {
		t.Errorf("every other view must still be attempted after a timeout, got %d/%d: %v",
			len(fake.refreshedViews), len(timescale.TradesCAGGs), fake.refreshedViews)
	}
}

// TestParseBackfillFlags_WriteGate pins the fail-closed mode contract
// (#868): backfill used to WRITE unless -dry-run was passed. Omitting both
// flags now refuses, -dry-run previews, and only -write applies.
func TestParseBackfillFlags_WriteGate(t *testing.T) {
	cfgPath := writeMinimalConfig(t, []string{"sdex"})
	base := []string{"-config", cfgPath, "-from", "100", "-to", "200"}

	if _, _, err := parseBackfillFlags(base); !errors.Is(err, opsutil.ErrWriteModeUnstated) {
		t.Fatalf("no mode flag: err = %v, want opsutil.ErrWriteModeUnstated — "+
			"a backfill that names neither -write nor -dry-run must refuse, not write", err)
	}
	for _, tc := range []struct {
		name    string
		extra   []string
		wantDry bool
	}{
		{"dry-run previews", []string{"-dry-run"}, true},
		{"write applies", []string{"-write"}, false},
		{"write wins over the dry-run alias", []string{"-dry-run", "-write"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, _, err := parseBackfillFlags(append(append([]string{}, base...), tc.extra...))
			if err != nil {
				t.Fatalf("parse %v: %v", tc.extra, err)
			}
			if opts.dryRun != tc.wantDry {
				t.Errorf("%v: opts.dryRun = %v, want %v", tc.extra, opts.dryRun, tc.wantDry)
			}
		})
	}
}
