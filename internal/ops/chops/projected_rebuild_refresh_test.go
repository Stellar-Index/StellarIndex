package chops

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A projected-rebuild -write rewrites served trade and oracle rows behind every refresh policy's lookback, so it
// refreshes the aggregates over exactly the rebuilt range unless switched off, and a failure names the follow-up.
func TestProjectedRebuildRefresh(t *testing.T) {
	var ranges [][2]uint32
	var out bytes.Buffer
	call := func(source string, write, refresh bool, runErr error) error {
		out.Reset()
		r := projectedRebuildRefresh("/etc/si.toml", source, write, refresh, 100, 200, nil)
		return refreshAfterRebuild(context.Background(), &out, r, func(context.Context) error {
			ranges = append(ranges, [2]uint32{r.lo, r.hi})
			return runErr
		})
	}

	if err := call("soroswap", true, true, nil); err != nil || len(ranges) != 1 || ranges[0] != [2]uint32{100, 200} {
		t.Fatalf("a trade source -write: err=%v refreshed %v, want one refresh over [100,200]", err, ranges)
	}
	if err := call("band", true, true, nil); err != nil || len(ranges) != 2 {
		t.Fatalf("an oracle source -write: err=%v, refreshes=%d, want 2", err, len(ranges))
	}
	if err := call("sep41_supply", true, true, nil); err != nil || len(ranges) != 2 || out.Len() != 0 {
		t.Fatalf("sep41_supply feeds no aggregate: err=%v refreshes=%d out=%q", err, len(ranges), out.String())
	}
	if err := call("soroswap", true, false, nil); err != nil || len(ranges) != 2 ||
		!strings.Contains(out.String(), "projected-rebuild: FOLLOW-UP OWED") || !strings.Contains(out.String(), wantRefreshFollowUp) {
		t.Fatalf("-refresh-caggs=false: err=%v refreshes=%d out=%q, want no refresh and the owed command", err, len(ranges), out.String())
	}
	if err := call("soroswap", false, true, nil); err != nil || len(ranges) != 2 || !strings.Contains(out.String(), "would then refresh") {
		t.Fatalf("dry run: err=%v refreshes=%d out=%q, want the preview only", err, len(ranges), out.String())
	}
	err := call("soroswap", true, true, errors.New("lock timeout"))
	if err == nil || !strings.Contains(err.Error(), wantRefreshFollowUp) {
		t.Fatalf("failed refresh: err=%v, want an error naming %q", err, wantRefreshFollowUp)
	}
}

// An -only-kinds run that keeps no trade kind writes no row an aggregate is built on: it skips the refresh and says so
// rather than printing a follow-up as owed. One that keeps the trade kind refreshes as an unfiltered run does.
func TestProjectedRebuildRefresh_OnlyKinds(t *testing.T) {
	refreshes := 0
	var out bytes.Buffer
	call := func(source string, write, refresh bool, only kindFilter) error {
		out.Reset()
		r := projectedRebuildRefresh("/etc/si.toml", source, write, refresh, 100, 200, only)
		return refreshAfterRebuild(context.Background(), &out, r, func(context.Context) error { refreshes++; return nil })
	}

	if err := call("aquarius", true, true, kindFilter{"aquarius.liquidity"}); err != nil || refreshes != 0 ||
		!strings.Contains(out.String(), "aggregate refresh SKIPPED, nothing owed") {
		t.Fatalf("liquidity-only -write: err=%v refreshes=%d out=%q, want no refresh and the stated skip", err, refreshes, out.String())
	}
	if err := call("aquarius", true, false, kindFilter{"aquarius.liquidity"}); err != nil || strings.Contains(out.String(), "OWED") {
		t.Fatalf("liquidity-only -refresh-caggs=false: err=%v out=%q, nothing is owed", err, out.String())
	}
	if err := call("aquarius", true, true, kindFilter{"aquarius.liquidity", "aquarius.trade"}); err != nil || refreshes != 1 {
		t.Fatalf("filter keeping aquarius.trade: err=%v refreshes=%d, want the refresh", err, refreshes)
	}
	if err := call("band", true, true, kindFilter{"band.update"}); err != nil || refreshes != 2 {
		t.Fatalf("oracle source filter: err=%v refreshes=%d, want the refresh (oracle_updates feeds the aggregates)", err, refreshes)
	}
}

// opsTradeWriteMarkers are the calls through which an ops command writes served trade or oracle rows, or rewinds the
// projector so it re-writes them.
var opsTradeWriteMarkers = []string{"pipeline.HandleEvent(", ".InsertTrade(", ".BatchInsertTrades(", ".InsertOracle", "RewindCursor("}

// opsTradeWriterRefreshExempt names the writer files that register no default-on -refresh-caggs themselves, and why.
var opsTradeWriterRefreshExempt = map[string]string{
	"ingest/backfill_external.go":  "a writer seam of the backfill verb, whose -refresh-caggs (backfill.go) refreshes every chunk it writes",
	"ingest/backfill_coingecko.go": "refreshes the oracle aggregates over its window after every -write, with no switch to turn that off",
}

// Every ops command that rewrites trade or oracle rows behind the refresh policies' lookback refreshes the aggregates
// over what it wrote by default; a new writer either registers -refresh-caggs defaulting to true or is exempt with a reason.
func TestOpsTradeWriters_RefreshTheAggregatesByDefault(t *testing.T) {
	writers := 0
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		s := string(src)
		if !slices.ContainsFunc(opsTradeWriteMarkers, func(m string) bool { return strings.Contains(s, m) }) {
			return nil
		}
		writers++
		rel := filepath.ToSlash(strings.TrimPrefix(path, "../"))
		if _, ok := opsTradeWriterRefreshExempt[rel]; ok {
			return nil
		}
		if !strings.Contains(s, `"refresh-caggs", true`) {
			t.Errorf("%s writes trade or oracle rows but registers no -refresh-caggs defaulting to true: refresh the aggregates over "+
				"the written range (refreshAfterRebuild / ingest.RefreshCAGGsForRange) or add it to opsTradeWriterRefreshExempt with a reason", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if writers < 4 {
		t.Fatalf("found %d trade writers under internal/ops, want at least 4 — the scan is not seeing the tree", writers)
	}
}

// The refresh is on unless switched off, for every rebuild command that rewrites trade or oracle rows.
func TestRebuildCommands_RefreshCAGGsByDefault(t *testing.T) {
	for verb, run := range map[string]func([]string) error{"ch-rebuild": chRebuild, "projected-rebuild": projectedRebuild} {
		help := captureStderr(t, func() {
			if err := run([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
				t.Errorf("%s -h: %v", verb, err)
			}
		})
		i := strings.Index(help, "  -refresh-caggs\n")
		block := ""
		if i >= 0 {
			block, _, _ = strings.Cut(help[i+3:], "\n  -")
		}
		if !strings.Contains(block, "(default true)") {
			t.Errorf("%s has no -refresh-caggs flag defaulting to true", verb)
		}
	}
}
