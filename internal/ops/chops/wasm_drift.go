// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/wasmaudit"
)

// wasmDriftLake is the ClickHouse surface wasm-drift reads: the creation-event
// stream the factory walk uses, and the contract → current wasm hash hop.
type wasmDriftLake interface {
	completeness.EventStreamer
	ContractWasmHash(ctx context.Context, contractID string) (string, error)
}

type chWasmDriftLake struct {
	clickhouse.ReconcileEventStreamer
	*clickhouse.ExplorerReader
}

// Per-contract verdicts. Only wasmDriftDrift counts toward the exit code.
const (
	wasmDriftAudited    = "audited"
	wasmDriftDrift      = "drift"
	wasmDriftSAC        = "sac"
	wasmDriftUnresolved = "unresolved"
)

type wasmDriftRow struct {
	source, contract, wasmHash, verdict string
}

type wasmDriftReport struct {
	rows      []wasmDriftRow
	checked   map[string]int // per audited source
	unaudited []string       // gated sources with no manifest entry
}

func (r wasmDriftReport) drifting() int {
	n := 0
	for _, row := range r.rows {
		if row.verdict == wasmDriftDrift {
			n++
		}
	}
	return n
}

// wasmDrift is the stellarindex-ops `wasm-drift` subcommand: every contract of
// a gated source that has an audit log must be running a hash that log
// string-checked. Contracts upgrade in place, so a decoder audited against
// one build can silently misread the next.
//
// Usage: wasm-drift [-config PATH] [-ch-addr H:P] [-source NAME] [-textfile PATH].
// Read-only; touches ClickHouse only. Exit code = drifting contracts (capped at 255).
func wasmDrift(args []string) error {
	fs := flag.NewFlagSet("wasm-drift", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/stellarindex.toml", "path to stellarindex.toml — used only to resolve the default -ch-addr")
	chAddr := fs.String("ch-addr", "", "ClickHouse native address (default: cfg.storage.clickhouse_addr from -config, falling back to "+defaultCHAddr+")")
	only := fs.String("source", "", "check one gated source only (default: every gated source)")
	textfile := fs.String("textfile", "", "node_exporter textfile path, e.g. /var/lib/node_exporter/textfile_collector/wasm_drift.prom; empty = none")
	if err := fs.Parse(args); err != nil {
		return err
	}

	manifest, err := wasmaudit.Load()
	if err != nil {
		return err
	}
	sources, err := wasmDriftSources(*only)
	if err != nil {
		return err
	}

	addr := resolveCHAddr(*chAddr, *cfgPath)
	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	tip, err := clickhouse.MaxLedger(ctx, addr)
	if err != nil {
		return fmt.Errorf("wasm-drift: CH max ledger: %w", err)
	}
	reader, err := clickhouse.NewExplorerReader(ctx, addr)
	if err != nil {
		return fmt.Errorf("wasm-drift: open ClickHouse: %w", err)
	}
	defer func() { _ = reader.Close() }()

	lake := chWasmDriftLake{ReconcileEventStreamer: clickhouse.ReconcileEventStreamer{Addr: addr}, ExplorerReader: reader}
	rep, err := runWasmDrift(ctx, lake, manifest, sources, tip)
	if err != nil {
		return err
	}
	printWasmDriftReport(rep, tip)

	if *textfile != "" {
		if err := writeAtomic(*textfile, renderWasmDriftProm(rep, time.Now())); err != nil {
			return fmt.Errorf("wasm-drift: write textfile: %w", err)
		}
	}
	return wasmDriftExit(rep.drifting())
}

// wasmDriftExit maps the drifting-contract count to the process exit code.
func wasmDriftExit(drifting int) error {
	if drifting == 0 {
		return nil
	}
	return &opsutil.ExitCodeError{Code: min(drifting, 255)}
}

// wasmDriftSources resolves -source against the gated registry, sorted.
func wasmDriftSources(only string) ([]string, error) {
	names := pipeline.GatedSourceNames()
	sort.Strings(names)
	if only == "" {
		return names, nil
	}
	for _, n := range names {
		if n == only {
			return []string{only}, nil
		}
	}
	return nil, fmt.Errorf("wasm-drift: -source %q is not a gated source (one of: %s)", only, strings.Join(names, ", "))
}

// runWasmDrift checks every contract of every audited source in sources
// against the manifest. A source with no manifest entry has nothing to
// drift from, so it is reported unaudited instead of flagging every contract.
func runWasmDrift(ctx context.Context, lake wasmDriftLake, manifest map[string]wasmaudit.Entry, sources []string, tip uint32) (wasmDriftReport, error) {
	audited := make(map[string]bool)
	for _, e := range manifest {
		for _, src := range e.Sources {
			audited[src] = true
		}
	}
	rep := wasmDriftReport{checked: make(map[string]int)}
	for _, source := range sources {
		if !audited[source] {
			rep.unaudited = append(rep.unaudited, source)
			continue
		}
		contracts, err := wasmaudit.ContractSet(ctx, wasmaudit.Deps{Events: lake}, source, tip)
		if err != nil {
			return rep, fmt.Errorf("wasm-drift: %s contract set: %w", source, err)
		}
		for _, c := range contracts {
			row, err := wasmDriftVerdict(ctx, lake, manifest, source, c)
			if err != nil {
				return rep, err
			}
			rep.rows = append(rep.rows, row)
		}
		rep.checked[source] = len(contracts)
	}
	return rep, nil
}

func wasmDriftVerdict(ctx context.Context, lake wasmDriftLake, manifest map[string]wasmaudit.Entry, source, contract string) (wasmDriftRow, error) {
	row := wasmDriftRow{source: source, contract: contract}
	h, err := lake.ContractWasmHash(ctx, contract)
	switch {
	case errors.Is(err, clickhouse.ErrContractIsSAC):
		row.verdict = wasmDriftSAC
	case errors.Is(err, clickhouse.ErrContractWasmUnresolved):
		row.verdict = wasmDriftUnresolved
	case err != nil:
		return row, fmt.Errorf("wasm-drift: %s %s: resolve wasm hash: %w", source, contract, err)
	default:
		row.wasmHash = h
		// A hash audited for another source is still drift here: the
		// contract is running code this source's decoder was never checked against.
		if manifest[h].Covers(source) {
			row.verdict = wasmDriftAudited
		} else {
			row.verdict = wasmDriftDrift
		}
	}
	return row, nil
}

func printWasmDriftReport(rep wasmDriftReport, tip uint32) {
	fmt.Printf("\n=== wasm-drift: gated contracts vs audited WASM (lake tip %d) ===\n", tip)
	for _, r := range rep.rows {
		if r.verdict == wasmDriftAudited {
			continue
		}
		fmt.Printf("  %-12s %-14s %s %s\n", strings.ToUpper(r.verdict), r.source, r.contract, r.wasmHash)
	}
	for _, s := range rep.unaudited {
		fmt.Printf("  UNAUDITED    %-14s no audit log in audited_wasm.json — contracts not checked\n", s)
	}
	verdict := "PASSED"
	if rep.drifting() > 0 {
		verdict = "FAILED"
	}
	fmt.Printf("wasm-drift: summary drifting=%d contracts_checked=%d unaudited_sources=%d  (%s)\n",
		rep.drifting(), len(rep.rows), len(rep.unaudited), verdict)
}

// renderWasmDriftProm renders wasm_drift.prom. Pure.
func renderWasmDriftProm(rep wasmDriftReport, now time.Time) string {
	var b strings.Builder
	b.WriteString("# HELP stellarindex_wasm_drift 1 per gated contract running a WASM hash its source's audit log has not string-checked.\n")
	b.WriteString("# TYPE stellarindex_wasm_drift gauge\n")
	unverifiable := make(map[[2]string]int)
	for _, r := range rep.rows {
		switch r.verdict {
		case wasmDriftDrift:
			fmt.Fprintf(&b, "stellarindex_wasm_drift{source=%q,contract=%q,wasm_hash=%q} 1\n", r.source, r.contract, r.wasmHash)
		case wasmDriftSAC, wasmDriftUnresolved:
			unverifiable[[2]string{r.source, r.verdict}]++
		}
	}
	b.WriteString("# HELP stellarindex_wasm_drift_contracts_checked Gated contracts wasm-drift resolved per audited source on its last run.\n")
	b.WriteString("# TYPE stellarindex_wasm_drift_contracts_checked gauge\n")
	checked := make([]string, 0, len(rep.checked))
	for s := range rep.checked {
		checked = append(checked, s)
	}
	sort.Strings(checked)
	for _, s := range checked {
		fmt.Fprintf(&b, "stellarindex_wasm_drift_contracts_checked{source=%q} %d\n", s, rep.checked[s])
	}
	b.WriteString("# HELP stellarindex_wasm_drift_contracts_unverifiable Gated contracts with no WASM to compare: a stellar asset contract (sac) or no instance entry in the lake (unresolved).\n")
	b.WriteString("# TYPE stellarindex_wasm_drift_contracts_unverifiable gauge\n")
	keys := make([][2]string, 0, len(unverifiable))
	for k := range unverifiable {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, k := range keys {
		fmt.Fprintf(&b, "stellarindex_wasm_drift_contracts_unverifiable{source=%q,reason=%q} %d\n", k[0], k[1], unverifiable[k])
	}
	b.WriteString("# HELP stellarindex_wasm_drift_source_unaudited 1 per gated source with no audit log in the manifest; its contracts are not checked.\n")
	b.WriteString("# TYPE stellarindex_wasm_drift_source_unaudited gauge\n")
	for _, s := range rep.unaudited {
		fmt.Fprintf(&b, "stellarindex_wasm_drift_source_unaudited{source=%q} 1\n", s)
	}
	b.WriteString("# HELP stellarindex_wasm_drift_last_run_unix When wasm-drift last completed a run.\n")
	b.WriteString("# TYPE stellarindex_wasm_drift_last_run_unix gauge\n")
	fmt.Fprintf(&b, "stellarindex_wasm_drift_last_run_unix %d\n", now.Unix())
	return b.String()
}
