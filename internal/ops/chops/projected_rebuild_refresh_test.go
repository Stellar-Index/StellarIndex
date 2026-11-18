package chops

import (
	"bytes"
	"context"
	"errors"
	"flag"
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
		r := projectedRebuildRefresh("/etc/si.toml", source, write, refresh, 100, 200)
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
