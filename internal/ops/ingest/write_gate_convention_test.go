// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// mutatingIngestSubcommands is every stellarindex-ops subcommand this
// package owns that WRITES to a datastore. Each must carry the shared
// fail-closed write gate (opsutil.NewMutatingFlagSet / RegisterWriteGate),
// because a preview-by-default is the only thing standing between a
// mistyped range and an applied one.
//
// Six of these — census-backfill, backfill-router, tag-routed-via,
// tag-signer, seed-soroswap-pairs, seed-protocol-contracts — declared
// NEITHER -write nor -dry-run and wrote unconditionally (K015). The gate
// was an opt-in helper, so nothing noticed; this list is what makes the
// convention a property instead of a habit. Add a mutating subcommand,
// add it here.
//
// Two mutating subcommands are deliberately ABSENT and must be added by
// whoever lands their flip: `backfill` and (in internal/ops/chops)
// `ch-backfill` still default to WRITE with a -dry-run opt-out. Flipping
// them is a caller-visible change — scripts/ops/ch-live-catchup.sh,
// ch-full-backfill.sh, phaseD-*.sh, ordinal-rederive-chunks.sh and
// restore-drill.sh all invoke ch-backfill with no mode flag and would
// silently become previews — so it lands with those callers, not here.
var mutatingIngestSubcommands = []string{
	// Gated as part of K015.
	"census-backfill",
	"backfill-router",
	"tag-routed-via",
	"tag-signer",
	"seed-soroswap-pairs",
	"seed-protocol-contracts",
	// Already gated; listed so a regression in either direction is loud.
	"asset-registry-backfill",
	"backfill-chainlink",
	"backfill-external",
	"curated-rwa-sync",
	"directory-sync",
	"issuer-enrich",
	"issuer-flags",
	"listing-sync",
	"projector-replay",
	"reap-cursors",
	"resume-stalled",
	"sep1-refresh",
}

// TestMutatingSubcommandsRegisterTheSharedWriteGate drives each
// subcommand through the real dispatch entry point with -h and reads the
// flag set it declares off its own usage output.
func TestMutatingSubcommandsRegisterTheSharedWriteGate(t *testing.T) {
	for _, verb := range mutatingIngestSubcommands {
		usage := subcommandUsage(t, verb)
		writeLine := usageFlagLine(usage, "write")
		dryRunLine := usageFlagLine(usage, "dry-run")
		switch {
		case writeLine == "":
			t.Errorf("%s writes to a datastore but declares no -write flag (K015): "+
				"with no preview, a mistyped range is applied on the first run", verb)
		case !strings.Contains(writeLine, "fail-closed DRY RUN"):
			t.Errorf("%s declares a -write flag that is not the shared opsutil gate — "+
				"one CLI, one convention, one description. Got:\n%s", verb, writeLine)
		case strings.Contains(writeLine, "(default true)"):
			t.Errorf("%s defaults -write to TRUE — the gate is fail-OPEN, which is the "+
				"default-WRITE shape it exists to remove. Got:\n%s", verb, writeLine)
		}
		switch {
		case dryRunLine == "":
			t.Errorf("%s declares no -dry-run alias — scripts and runbooks that already "+
				"pass it must keep working", verb)
		case !strings.Contains(dryRunLine, "the DEFAULT"):
			t.Errorf("%s declares a -dry-run flag that is not the shared opsutil alias "+
				"(so dry run is probably NOT the default). Got:\n%s", verb, dryRunLine)
		}
	}
}

// subcommandUsage runs `<verb> -h` through the package's real dispatch
// function and returns what it printed. flag.ContinueOnError makes -h
// print the defaults and return flag.ErrHelp from Parse, before the
// handler touches a config file or a database.
func subcommandUsage(t *testing.T, verb string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	orig := os.Stderr
	os.Stderr = w
	runErr := Run([]string{verb, "-h"})
	os.Stderr = orig
	_ = w.Close()
	usage := <-done
	_ = r.Close()

	if !errors.Is(runErr, flag.ErrHelp) {
		t.Fatalf("%s -h returned %v, want flag.ErrHelp — the handler did work before parsing, "+
			"so this test is not reading its declared flags", verb, runErr)
	}
	if usage == "" {
		t.Fatalf("%s -h printed nothing — this test is asserting nothing", verb)
	}
	return usage
}

// usageFlagLine returns the usage block for one flag: its `  -name` line
// plus the indented description beneath it, or "" when the flag is not
// declared.
func usageFlagLine(usage, name string) string {
	lines := strings.Split(usage, "\n")
	for i, line := range lines {
		if strings.TrimRight(line, " ") != "  -"+name {
			continue
		}
		block := line
		for _, next := range lines[i+1:] {
			if !strings.HasPrefix(next, "    	") && !strings.HasPrefix(next, "\t") {
				break
			}
			block += "\n" + next
		}
		return block
	}
	return ""
}

// TestPreviewPathsNeverReachTheStore is the behavioural half: the gate is
// only worth its flag if the preview branch actually writes nothing. Each
// of these is handed a NIL *timescale.Store, so any call through to the
// datastore panics on the nil dereference instead of quietly passing.
func TestPreviewPathsNeverReachTheStore(t *testing.T) {
	ctx := context.Background()
	var store *timescale.Store // deliberately nil — see the doc above

	t.Run("tag-signer", func(t *testing.T) {
		tags := []timescale.SignerTag{
			{Ledger: 1, TxHash: "a", Signer: "GA"},
			{Ledger: 2, TxHash: "b", Signer: "GB"},
		}
		got, err := tagSignerWindow(ctx, store, false, time.Unix(0, 0), time.Unix(1, 0), tags)
		if err != nil {
			t.Fatalf("preview returned %v, want nil", err)
		}
		if got != int64(len(tags)) {
			t.Errorf("preview reported %d trades, want the %d candidate tags it would apply", got, len(tags))
		}
	})

	t.Run("census-backfill", func(t *testing.T) {
		if err := upsertCensusRow(ctx, store, false, timescale.LedgerIngestRow{LedgerSeq: 42}); err != nil {
			t.Errorf("preview returned %v, want nil", err)
		}
	})

	t.Run("backfill-router", func(t *testing.T) {
		if err := insertRouterSwap(ctx, store, false, timescale.SoroswapRouterSwap{Ledger: 42}); err != nil {
			t.Errorf("preview returned %v, want nil", err)
		}
	})

	t.Run("seed-protocol-contracts", func(t *testing.T) {
		// A curated-set source: its whole write is the in-code set, so
		// the preview must report the set size and upsert none of it.
		var curated string
		var want int
		for _, name := range pipeline.GatedSourceNames() {
			meta, ok := pipeline.GatedMetaFor(name)
			if ok && len(meta.Factories) == 0 && len(meta.CuratedSet) > 0 {
				curated, want = name, len(meta.CuratedSet)
				break
			}
		}
		if curated == "" {
			t.Skip("no curated-only gated source in the registry")
		}
		got, err := seedOneGatedSource(ctx, store, false, curated, 100)
		if err != nil {
			t.Fatalf("preview of %s returned %v, want nil", curated, err)
		}
		if got != want {
			t.Errorf("preview of %s reported %d contracts, want the %d it would upsert", curated, got, want)
		}
	})
}
