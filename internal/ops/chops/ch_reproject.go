package chops

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// chReproject re-derives a range from the lake and compares per-ledger counts to the served Postgres protocol tables (ADR-0034). Soroswap
// is re-derived without RPC pair-seeding, so its undercount for earlier pairs is expected and flagged.
func chReproject(args []string) error { //nolint:gocognit,gocyclo,funlen // linear: build cats, two re-derives, compare; splitting reduces clarity.
	fs := flag.NewFlagSet("ch-reproject", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	from := fs.Uint("from", 0, "first ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "last ledger sequence (inclusive, required)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	maxList := fs.Int("max-list", 20, "max mismatched ledgers to print per kind")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *from == 0 || *to == 0 || *to < *from {
		return fmt.Errorf("-config, -from, -to are required; -to must be >= -from")
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage open: %w", err)
	}
	defer func() { _ = store.Close() }()

	lo, hi := uint32(*from), uint32(*to)

	cat, _, err := buildReconciliationCatalogue(cfg)
	if err != nil {
		return fmt.Errorf("ch-reproject: reconciliation catalogue: %w", err)
	}
	// Re-derive on the live gate (curated set plus protocol_contracts), not the bare seed: an
	// unwarmed re-derive drops admitted contracts and reports a false CH-under-served delta,
	// which invites the destructive ch-rebuild -write. Must precede the preseed below.
	if cat, err = warmCatalogueGates(ctx, store, slog.Default(), cat); err != nil {
		return fmt.Errorf("ch-reproject: %w", err)
	}

	// Factory-anchored sources (ADR-0035): seed each gate registry from creation events in
	// [genesis, lo) before re-deriving. Without it blend re-derives 0 rows above its factory
	// deploys, a bogus delta here and a silently empty arm in ch-rebuild -write.
	for _, src := range cat {
		if len(src.factories) == 0 {
			continue
		}
		pblind, perr := preseedFactoryChildren(ctx, clickhouse.ReconcileEventStreamer{Addr: *chAddr}, src, lo)
		if perr != nil {
			return fmt.Errorf("%s: preseed factory children: %w", src.name, perr)
		}
		// A writer must not rebuild over a registry missing a child whose
		// creation event its decoder could not evaluate.
		if pblind.Any() {
			return fmt.Errorf("%s: preseed factory children: %s", src.name, pblind.Detail())
		}
	}

	// ─── ClickHouse side: one pass, every event-based decoder per event ──
	fmt.Fprintf(os.Stderr, "ch-reproject: re-deriving [%d,%d] from ClickHouse %s\n", lo, hi, *chAddr)
	// Bucket per SOURCE then kind: the 3 reflector variants all emit the same EventKind ("reflector.update") but route to
	// oracle_updates under distinct source filters, so a kind-only bucket would merge them. Per-source keying keeps each
	// variant's output (and its contractIDs prefilter) separate.
	chBySrc := make(map[string]map[string]map[uint32]int)
	chStart := time.Now()
	accumulate := func(ev events.Event) error {
		for _, src := range cat {
			if src.dec == nil { // census-only (sdex) — op-based, not in contract_events
				continue
			}
			// Oracle decoders match by TOPIC, not contract — restrict each to
			// its own contract (mirrors the PG path's contractIDs prefilter).
			if len(src.contractIDs) > 0 && !containsStr(src.contractIDs, ev.ContractID) {
				continue
			}
			if !src.dec.Matches(ev) {
				continue
			}
			outs, derr := src.dec.Decode(ev)
			if derr != nil {
				continue // soft-fail, mirroring the projector + the PG re-derive
			}
			bk := chBySrc[src.name]
			if bk == nil {
				bk = make(map[string]map[uint32]int)
				chBySrc[src.name] = bk
			}
			for _, out := range outs {
				k := out.EventKind()
				if bk[k] == nil {
					bk[k] = make(map[uint32]int)
				}
				bk[k][ev.Ledger]++
			}
		}
		return nil
	}
	// Window the stream per 1M-ledger partition: one unbounded read over the Soroban era outruns
	// the 1h ReadTimeout. The cat decoders persist across windows, so correlation state carries
	// over each boundary and output matches one continuous pass.
	var cherr error
	for wlo := lo; ; {
		whi := hi
		if aligned := (uint64(wlo)/1_000_000+1)*1_000_000 - 1; aligned < uint64(hi) {
			whi = uint32(aligned)
		}
		if cherr = clickhouse.StreamContractEvents(ctx, *chAddr, wlo, whi, nil, accumulate); cherr != nil {
			break
		}
		if whi >= hi {
			break
		}
		wlo = whi + 1
	}
	if cherr != nil {
		return fmt.Errorf("ch-reproject: clickhouse stream: %w", cherr)
	}
	fmt.Fprintf(os.Stderr, "ch-reproject: ClickHouse pass done in %s\n", time.Since(chStart).Round(time.Second))

	// ─── SDEX side: op-based re-derivation ───────────────────────────────
	// SDEX trades are op-derived, not event-derived. Count the storable projection, not raw
	// decoder output: a one-side-zero fill is never stored and would print a permanent CH > served.
	sdexStart := time.Now()
	sdexByLedger, sdexBlind, serr := reDeriveSDEXCensusViaDecoder(ctx, *chAddr, lo, hi)
	if serr != nil {
		return fmt.Errorf("ch-reproject: sdex op stream: %w", serr)
	}
	fmt.Fprintf(os.Stderr, "ch-reproject: SDEX op pass done in %s\n", time.Since(sdexStart).Round(time.Second))

	// ─── compare CH re-derive vs the served protocol tables, per target ──
	fmt.Printf("\n=== ch-reproject: rebuild-from-ClickHouse vs served tables ===\n")
	fmt.Printf("range: %d..%d  (CH > served ⇒ lake recovers silently-dropped rows; CH < served ⇒ CH-side gap)\n\n", lo, hi)
	fmt.Printf("%-34s %12s %12s  %s\n", "source/table", "CH-rederive", "served", "verdict")

	var diverged []string
	for _, src := range cat {
		if src.dec == nil {
			// sdex: op-based re-derivation (sdexByLedger, above). One target
			// (trades WHERE source='sdex'); CH > served = storable fills the
			// served tier is missing.
			if src.name == "sdex" {
				if sdexBlind.Any() {
					diverged = append(diverged, "sdex (undecodable)")
					fmt.Printf("%-34s %s\n", "sdex (undecodable)", sdexBlind.Detail())
				}
				for _, tgt := range src.targets {
					actual, aerr := store.CountRowsByLedger(ctx, tgt.table, "ledger", tgt.countFilter(), lo, hi)
					if aerr != nil {
						return fmt.Errorf("ch-reproject: %s/%s served counts: %w", src.name, tgt.table, aerr)
					}
					gaps := completeness.ReconcileCounts(actual, sdexByLedger)
					label := src.name + "/" + tgt.table
					chTotal, servedTotal := sumCounts(sdexByLedger), sumCounts(actual)
					if len(gaps) == 0 {
						fmt.Printf("%-34s %12d %12d  OK\n", label, chTotal, servedTotal)
						continue
					}
					diverged = append(diverged, label)
					fmt.Printf("%-34s %12d %12d  %d ledger(s) differ\n", label, chTotal, servedTotal, len(gaps))
					for i, g := range gaps {
						if i >= *maxList {
							fmt.Printf("    … %d more (raise -max-list)\n", len(gaps)-*maxList)
							break
						}
						fmt.Printf("    ledger=%d served=%d CH=%d (delta %+d)\n", g.Ledger, g.Expected, g.Actual, g.Actual-g.Expected)
					}
				}
			}
			continue
		}
		for _, tgt := range src.targets {
			expected := completeness.SumKinds(chBySrc[src.name], tgt.kinds...) // CH-re-derived per ledger, this source only
			actual, aerr := store.CountRowsByLedger(ctx, tgt.table, "ledger", tgt.countFilter(), lo, hi)
			if aerr != nil {
				return fmt.Errorf("ch-reproject: %s/%s served counts: %w", src.name, tgt.table, aerr)
			}
			gaps := completeness.ReconcileCounts(actual, expected) // actual=served baseline, "actual"=CH
			label := src.name + "/" + tgt.table
			chTotal, servedTotal := sumCounts(expected), sumCounts(actual)
			if len(gaps) == 0 {
				fmt.Printf("%-34s %12d %12d  OK\n", label, chTotal, servedTotal)
				continue
			}
			diverged = append(diverged, label)
			note := ""
			if src.name == "soroswap" {
				note = " (soroswap unseeded — undercount expected)"
			}
			fmt.Printf("%-34s %12d %12d  %d ledger(s) differ%s\n", label, chTotal, servedTotal, len(gaps), note)
			for i, g := range gaps {
				if i >= *maxList {
					fmt.Printf("    … %d more (raise -max-list)\n", len(gaps)-*maxList)
					break
				}
				fmt.Printf("    ledger=%d served=%d CH=%d (delta %+d)\n", g.Ledger, g.Expected, g.Actual, g.Actual-g.Expected)
			}
		}
	}

	return chReprojectVerdict(diverged)
}

// errChReprojectDiverged marks a range whose lake re-derivation disagrees
// with the served tables.
var errChReprojectDiverged = errors.New("ch-reproject: CH re-derivation diverges from the served tables")

// chReprojectVerdict maps compared targets to the exit status: a caller scripting $? must not read a divergence as a
// match, the contract ch-gate keeps.
func chReprojectVerdict(diverged []string) error {
	if len(diverged) > 0 {
		fmt.Printf("\nch-reproject: differences found — review above (CH>served = recovered loss; CH<served = CH gap)\n")
		return fmt.Errorf("%w: %d target(s): %s", errChReprojectDiverged, len(diverged), strings.Join(diverged, ", "))
	}
	fmt.Printf("\n✅ ch-reproject: CH re-derivation matches the served tables exactly\n")
	return nil
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
