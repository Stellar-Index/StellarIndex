package chops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/ops/ingest"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/projector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	sushiswap_v3 "github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/wasmaudit"
)

// tradeOf extracts the canonical.Trade from a trade-shaped event so the rebuild
// can batch-insert trades (the bulk of the projected output). Mirrors
// pipeline.tradeFromEvent for the projected trade sources.
func tradeOf(ev consumer.Event) (canonical.Trade, bool) {
	switch e := ev.(type) {
	case aquarius.TradeEvent:
		return e.Trade, true
	case soroswap.TradeEvent:
		return e.Trade, true
	case phoenix.TradeEvent:
		return e.Trade, true
	case comet.TradeEvent:
		return e.Trade, true
	case sushiswap_v3.TradeEvent:
		return e.Trade, true
	case sdex.TradeEvent:
		return e.Trade, true
	}
	return canonical.Trade{}, false
}

// seedSoroswapFromPG seeds the soroswap decoder's pair registry from the
// persisted soroswap_pairs table (fast, no RPC). Mirrors the seeding half of
// pipeline.SoroswapPersistenceOptions.
func seedSoroswapFromPG(ctx context.Context, store *timescale.Store, dec *soroswap.Decoder) (int, error) {
	rows, err := store.LoadSoroswapPairRegistry(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		t0, err := canonical.NewSorobanAsset(r.Token0Strkey)
		if err != nil {
			continue
		}
		t1, err := canonical.NewSorobanAsset(r.Token1Strkey)
		if err != nil {
			continue
		}
		dec.SeedPair(r.PairStrkey, t0, t1)
		n++
	}
	return n, nil
}

// projectorCursorReader reports the live projector's cursor for one source: (lastLedger, true,
// nil) when a cursor row exists, and (0, false, nil) when the projector never ran for it. It is
// a seam so the guard below is testable with a scripted cursor.
type projectorCursorReader func(source string) (uint32, bool, error)

// checkCHRebuildLiveOverlap is the one-writer guard (checkLiveCursorGuard, ADR-0048 D3) for
// ch-rebuild's multi-source shape. `-write` rows carry a positive derive_generation and win ON
// CONFLICT, so a run to the tip would be a second writer inside the projector's range. Per source
// a run is allowed only when the live cursor is at or above -to; a missing cursor counts as 0 and
// -allow-live-overlap overrides. It arms only under -write, so the read-only report still works.
func checkCHRebuildLiveOverlap(projectedSources []string, to uint32, allowOverlap bool, read projectorCursorReader) error {
	if allowOverlap || len(projectedSources) == 0 {
		return nil
	}
	var offenders []string
	for _, name := range projectedSources {
		last, have, err := read(name)
		if err != nil {
			return fmt.Errorf("read live projector cursor for %s: %w", name, err)
		}
		if liveCursorCovers(have, last, to) {
			continue
		}
		cur := "none (projector has never run for this source)"
		if have {
			cur = fmt.Sprintf("%d", last)
		}
		offenders = append(offenders, fmt.Sprintf("%s (live cursor %s)", name, cur))
	}
	if len(offenders) == 0 {
		return nil
	}
	return fmt.Errorf("ch-rebuild: refusing to -write — the live projector's cursor is below the requested top %d for: %s. "+
		"ADR-0048 D3's one-writer contract: a projected source has exactly one writer, and this pass must only fill HISTORY BEHIND a live-current source — never a range the live tail might still walk into concurrently. "+
		"Wait for the live projector to pass ledger %d, lower -to, drop those sources from -sources, or pass -allow-live-overlap if you have independently verified this is safe",
		to, strings.Join(offenders, ", "), to)
}

// projectedSourcesInRun returns the sources this run would write that the live projector also
// writes. It resolves them from projector.BuildRegistry, not a local list, so a newly projected
// source is guarded on its first run. A per-name BuildRegistry error (a projector entry with
// incomplete config) is still the projector's domain, so it counts as projected (fail-closed).
func projectedSourcesInRun(cfg config.Config, cat, sep41Cat []reconSource, includeSEP41 bool, enabled func(string) bool) []string {
	candidates := make([]string, 0, len(cat)+len(sep41Cat))
	for _, src := range cat {
		if src.dec != nil && enabled(src.name) {
			candidates = append(candidates, src.name)
		}
	}
	if includeSEP41 {
		for _, src := range sep41Cat {
			if enabled(src.name) {
				candidates = append(candidates, src.name)
			}
		}
	}
	out := make([]string, 0, len(candidates))
	for _, name := range candidates {
		reg, err := projector.BuildRegistry([]string{name}, cfg.Oracle, cfg.Supply.WatchedSEP41Contracts, nil)
		if err != nil {
			out = append(out, name) // see the fail-closed note above
			continue
		}
		// BuildRegistry ALWAYS appends the sep41 sources when contracts are
		// watched, so "the registry is non-empty" is not the question —
		// "did it resolve THIS name" is.
		for _, s := range reg.Sources {
			if strings.EqualFold(s.Name, name) {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// chRebuildPasses records which of ch-rebuild's opt-in passes an
// invocation engaged (the event pass is always on).
type chRebuildPasses struct {
	sep41, contractCalls, sdex bool
}

// reDerivedSourcesInRun returns every source this run would decode, the subject of the
// BackfillSafe gate. It mirrors each chRebuild pass's selection: event sources need a decoder,
// ContractCall sources -contract-calls, sdex -sdex, the sep41 pair -sep41; all respect -sources.
func reDerivedSourcesInRun(cat, sep41Cat []reconSource, passes chRebuildPasses, enabled func(string) bool) []string {
	var out []string
	for _, src := range cat {
		if !enabled(src.name) {
			continue
		}
		switch {
		case src.dec != nil:
			out = append(out, src.name)
		case src.callDec != nil && passes.contractCalls:
			out = append(out, src.name)
		case src.name == sdex.SourceName && passes.sdex:
			out = append(out, src.name)
		}
	}
	if passes.sep41 {
		for _, src := range sep41Cat {
			if enabled(src.name) {
				out = append(out, src.name)
			}
		}
	}
	return out
}

// gateCHRebuildLake is a -write run's per-WASM lake gate over [lo, hi].
// chAddr is the lake the run reads (-ch-addr), not the config's.
func gateCHRebuildLake(ctx context.Context, cfg config.Config, chAddr string, store *timescale.Store, sources []string, lo, hi uint32) error {
	return wasmaudit.GateReplay(ctx, chAddr, cfg.Oracle, store.LoadProtocolContracts, sources, lo, hi)
}

// checkCHRebuildBackfillSafe refuses a -write run over a source whose decoder was not audited
// against every WASM generation in its history: the current decoder over an old range with
// winning rows is backfill's hazard with a stronger writer. An unregistered name is refused
// (external.ReplayBackfillSafe). It arms under -write only, so a dry run can still evaluate an
// unaudited decoder; there is no override.
func checkCHRebuildBackfillSafe(sources []string) error {
	unsafeSources := external.UnsafeReplaySources(sources)
	if len(unsafeSources) == 0 {
		return nil
	}
	return fmt.Errorf("ch-rebuild: refusing to -write — sources not BackfillSafe (per-WASM-hash audit pending, or not a known source): %v. "+
		"This pass decodes history with the CURRENT decoders and its rows overwrite the stored ones; Soroban contracts upgrade in place, "+
		"so an unaudited old WASM generation decodes to silently wrong rows. Restrict -sources to audited sources (a run with no -sources "+
		"selects the whole catalogue), or run stellarindex-ops wasm-history over each source's contracts, then audit each WASM hash under docs/operations/wasm-audits/, add it to internal/wasmaudit/audited_wasm.json and set Backfill: BackfillPerWASM in internal/sources/external/registry.go in the same PR. "+
		"The default dry-run is not gated",
		unsafeSources)
}

// chRebuildPreflightPrefix opens the one stdout line a successful `ch-rebuild -write -preflight`
// prints. It is a contract with scripts/ops/ch-rebuild-projected.sh, which selects the line by
// this prefix and reads the `rederive=` list off its end; change the two together
// (TestChRebuildProjectedScript_ParsesTheRealPreflightLine).
const chRebuildPreflightPrefix = "ch-rebuild: preflight ok"

// checkCHRebuildPreflightFlags refuses -preflight without -write. Every guard a preflight runs is
// a -write guard, so a bare -preflight would run none and still answer "ok" to a caller about to
// delete rows on that answer.
func checkCHRebuildPreflightFlags(preflight, write bool) error {
	if preflight && !write {
		return fmt.Errorf("-preflight checks the guards of a -write run and a dry run has none; pass it together with -write")
	}
	return nil
}

// chRebuildDirtyRecordedPrefix opens the one stdout line a successful `ch-rebuild -write
// -record-dirty-window` prints. Like chRebuildPreflightPrefix it is a contract with
// scripts/ops/ch-rebuild-projected.sh, which logs it so the operator sees which sources the
// completeness verifier owes a re-reconcile.
const chRebuildDirtyRecordedPrefix = "ch-rebuild: recorded projection dirty window"

// checkCHRebuildRecordDirtyFlags keeps the three steps around a clean-slate window separable:
// -preflight asks before the delete, -write re-derives, and -record-dirty-window records an
// obligation that outlives a re-derive which did not happen. Combining them would answer "ok,
// delete" for an already emptied range, or double-file against a re-derive that records its own.
func checkCHRebuildRecordDirtyFlags(recordDirty, preflight, write bool) error {
	if recordDirty && write {
		return fmt.Errorf("-record-dirty-window records that a range was emptied and NOT re-derived; it is not a mode of -write. Run the re-derive and, if it fails, the record, as separate invocations")
	}
	if recordDirty && preflight {
		return fmt.Errorf("-record-dirty-window and -preflight are different steps (ask before the DELETE vs. record an emptied window); pass one")
	}
	return nil
}

// chRebuildRecordDirtySources resolves the sources a `-record-dirty-window` invocation files an
// obligation for: the ones named, each of which must be in the reconciliation catalogue.
// Sources are named, not inferred as re-derivable: the caller knows which rows it deleted, and a
// source whose gate has since closed leaves the longest-lived hole. An unknown name is refused
// because compute-completeness looks windows up by source, so a row under it would never be read.
func chRebuildRecordDirtySources(cat []reconSource, named []string) ([]string, error) {
	if len(named) == 0 {
		return nil, fmt.Errorf("-record-dirty-window needs -sources: it records the obligation for the sources whose rows were deleted, and recording one for every catalogue source would force a full re-reconcile of ranges nothing touched")
	}
	known := make(map[string]bool, len(cat))
	for _, src := range cat {
		known[src.name] = true
	}
	for _, name := range named {
		if !known[name] {
			return nil, fmt.Errorf("-record-dirty-window: %q is not a reconciliation-catalogue source, so a window recorded under it would never be read by a completeness verdict; name the catalogue sources whose rows were deleted", name)
		}
	}
	return named, nil
}

// chRebuildSourceUniverse returns every ch-rebuild source name in catalogue order: the
// reconciliation catalogue plus the two SEP-41 sources, which the catalogue promotes only when a
// watched set is configured but which -sources may name either way.
func chRebuildSourceUniverse(cat []reconSource) []string {
	out := make([]string, 0, len(cat)+2)
	seen := map[string]bool{}
	for _, src := range cat {
		if !seen[src.name] {
			seen[src.name] = true
			out = append(out, src.name)
		}
	}
	for _, name := range []string{sep41transfers.SourceName, sep41supply.SourceName} {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// checkCHRebuildSources refuses a -sources value naming something unknown as a ch-rebuild source;
// otherwise a typo makes enabled() false for every real source and the dry run exits 0 having
// re-derived nothing. Only unknown names are refused: a known source whose pass was not requested
// or whose decoder this config does not build still narrows legitimately, and
// scripts/ops/ch-rebuild-projected.sh depends on that.
func checkCHRebuildSources(cat []reconSource, named []string) error {
	if len(named) == 0 {
		return nil
	}
	known := chRebuildSourceUniverse(cat)
	inUniverse := make(map[string]bool, len(known))
	for _, name := range known {
		inUniverse[name] = true
	}
	var unknown []string
	for _, name := range named {
		if !inUniverse[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	return fmt.Errorf("ch-rebuild: -sources names %v, which this binary does not know as a re-derivable source — "+
		"the filter is a plain name match, so the run would re-derive NOTHING and still exit 0. Known sources: %s",
		unknown, strings.Join(known, ", "))
}

// projectionDirtyWindowRecorder is the slice of timescale.Store
// [recordCHRebuildDirtyWindows] needs.
type projectionDirtyWindowRecorder interface {
	RecordProjectionDirtyWindow(ctx context.Context, w timescale.ProjectionDirtyWindow) error
}

// recordCHRebuildDirtyWindows records [lo,hi] as a pending dirty window for every source, stamped
// with reason(lo, hi), so the next compute-completeness re-reconciles the range instead of
// carrying a prior clean claim over it. It writes one row per source under its catalogue name
// (any other name silently no-ops) and errors on an empty set. Only a clean verdict clears it.
func recordCHRebuildDirtyWindows(ctx context.Context, store projectionDirtyWindowRecorder, w io.Writer, lo, hi uint32, sources []string, reason func(from, to uint32) string) error {
	if len(sources) == 0 {
		return fmt.Errorf("ch-rebuild: record dirty window [%d,%d]: this invocation would re-derive no source, so no obligation was recorded — name the sources whose rows were deleted in -sources", lo, hi)
	}
	for _, name := range sources {
		if err := store.RecordProjectionDirtyWindow(ctx, timescale.ProjectionDirtyWindow{
			Source: name,
			From:   lo,
			To:     hi,
			Reason: reason(lo, hi),
		}); err != nil {
			return fmt.Errorf("ch-rebuild: record dirty window (%s): %w", name, err)
		}
	}
	_, err := fmt.Fprintf(w, "%s [%d,%d] sources=%s\n", chRebuildDirtyRecordedPrefix, lo, hi, strings.Join(sources, ","))
	return err
}

// reportCHRebuildPreflight prints the preflight verdict: the range and the
// sources this invocation would re-derive, comma-separated, in catalogue
// order. An empty list is a legal answer (nothing named resolves to a
// decoder under this config) and prints `rederive=` with nothing after it.
func reportCHRebuildPreflight(w io.Writer, lo, hi uint32, rederive []string) error {
	_, err := fmt.Fprintf(w, "%s [%d,%d] rederive=%s\n", chRebuildPreflightPrefix, lo, hi, strings.Join(rederive, ","))
	return err
}

// chRebuild is the write path (ADR-0034): it re-derives a ledger range's protocol output from the
// ClickHouse lake with the existing decoders and writes it to Postgres through the production
// sink (pipeline.HandleEvent, idempotent ON CONFLICT). ch-reproject is its count-only sibling.
//
// Passes: event sources (one StreamContractEvents pass), -sdex (StreamSDEXOps), -contract-calls
// (StreamContractCallOps; the projector cannot rebuild these) and -sep41, whose CAP-67 firehose
// topics the main pass excludes, so it prefilters on contract_id
// (docs/operations/sep41-mint-recovery.md). Dry-run by default; -write persists.
func chRebuild(args []string) error { //nolint:gocognit,gocyclo,funlen // linear: seed, event pass, optional op pass, report; splitting hurts clarity.
	fs, gate := opsutil.NewMutatingFlagSet("ch-rebuild")
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	from := fs.Uint("from", 0, "first ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "last ledger sequence (inclusive, required)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	only := fs.String("sources", "", "comma-separated source names to rebuild (default: all event-based)")
	includeSDEX := fs.Bool("sdex", false, "also re-derive SDEX trades from operations (expensive: ~15.5B op decodes all-history)")
	sdexGaps := fs.Bool("sdex-gaps", false, "with -sdex: re-derive ONLY the served gaps in [from,to] in one pass (each gap is an empty range → pure insert, no ON CONFLICT walk) — efficient drop-backlog recovery vs re-scanning the whole range")
	sdexReconcile := fs.Bool("sdex-reconcile", false, "with -sdex: re-derive ONLY ledgers where the distinct Validate-passing census exceeds the served count (PARTIAL-drop ledgers the empty-gap pass misses); recovers the served-tier projection to exact parity with the lake")
	contractCalls := fs.Bool("contract-calls", false, "also re-derive the event-less ContractCall sources (band, soroswap-router) from the lake's InvokeContract ops — filtered on the contract's bytes in body_xdr (no contract_id column) — and run their ContractCallDecoders. These have NO soroban_events landing zone, so neither the event pass nor the projector can rebuild them. Respects -sources.")
	includeSEP41 := fs.Bool("sep41", false, "also re-derive the SEP-41 watched-contract sources (sep41_transfers, sep41_supply) from the lake via a contract_id-prefiltered event pass (their topics are the CAP-67 firehose the main pass excludes). FULL re-derive contract: for a whole-history rebuild run this as part of the truncate+re-derive procedure — TRUNCATE sep41_transfers + sep41_supply_events FIRST (historical rows predate the migration-0057 event_index PK, so multiple same-op events sit COLLAPSED on disk; the idempotent ON CONFLICT writes cannot un-collapse them — recover-into-existing is accepted only if you accept that residue). ROLLUP: when the SUPPLY source is re-derived, -write AUTO-RESETS the sep41_supply_rollup fold checkpoint after the events land (a FULL re-derive resets every watched contract's fold columns) and re-folds it from zero in place instead of double-counting the re-derived history (the KALE 2× served-value bug, incident 2026-07-06); the seeded migration-0088 genesis baseline is PRESERVED, so no manual TRUNCATE sep41_supply_rollup + re-seed is needed. After a full-history -write re-derive the two sources become eligible for the ADR-0033 projection reconcile — DONE (2026-07-11, windows 50.0M-63.42M, rc=0): buildReconciliationCatalogue now promotes them into the default catalogue unconditionally whenever [supply] watched_sep41_contracts is configured (no further code change needed), so verify-reconciliation/ch-reproject/compute-completeness all see them. Because that promotion is config-gated, not re-derive-state-gated, a FUTURE full truncate+re-derive will show the two sources as reconcile-red for the DURATION of the rebuild (truncated table vs. lake expectation) exactly like any other source mid-rebuild — expected, not a regression. For a SCOPED dropped-rows recovery (a decoder bug that lost a handful of rows from post-0057-clean data), use -contracts to narrow to the affected contracts instead — no truncate needed, the additive ON CONFLICT write only ADDS the missing rows (docs/operations/sep41-mint-recovery.md). Requires [supply] watched_sep41_contracts. Respects -sources.")
	contractsCSV := fs.String("contracts", "", "comma-separated contract C-strkeys to SCOPE the read to (default: no scope). For -sep41 this REPLACES [supply] watched_sep41_contracts as the contract_id READ prefilter, so a scoped recovery does an indexed scan of ONLY these contracts' events (far cheaper than all watched contracts) and idempotently ADDS their missing rows — the leanest way to recover dropped rows without a full re-derive. With -sep41 -write on the SUPPLY source, ONLY these contracts' sep41_supply_rollup fold rows are reset afterwards (genesis baseline preserved) and re-folded from zero in place over their recovered below-checkpoint rows — a scoped recovery is safe by default, no manual rollup surgery. Must be a SUBSET of the watched set: the sep41 decoders still gate Matches() on the full watched set, so a contract outside it is read but decoded to nothing (a warning is printed). For the general event pass it is an extra decode-time contract gate. See docs/operations/sep41-mint-recovery.md.")
	sep41SupplyOnly := fs.Bool("sep41-supply-only", false, "with -sep41 -sources sep41_supply: narrow the CH read to the supply-affecting topics (mint/burn/clawback) via the topic_0_sym prefilter, skipping the transfer firehose at the SQL layer — so recovering a high-transfer-volume contract's few mints does not re-read millions of transfer events. Invalid unless sep41_transfers is disabled (via -sources sep41_supply): the topic prefilter would otherwise silently drop transfer recovery.")
	bulkTrades := fs.Bool("bulk-trades", false, "with -write: land trade rows through the BULK backfill writer (timescale.Store.BulkBackfillTrades) instead of the per-batch upsert. Opt-in and BACKFILL-ONLY. It proves - per source, scoped by ledger AND ts - that the target range holds no stored rows, then resolves usd_volume for the whole buffer through a worker pool and streams the rows in over parallel binary COPY connections. Rows are identical to the upsert path's (same storability gate, same intra-batch PK dedupe, same tradeUSDVolume waterfall, same derive_generation, same source_entry_counts / registry / sentinel side effects); the difference is that a latency-bound workload stops being serial. If the range is NOT empty - or a COPY hits a unique violation because something wrote underneath it - the buffer is handed to the ordinary generation-guarded upsert instead and the run says so. Worth it for a historical re-derive below the source's floor; pointless (and it will just fall back) for a recovery into populated ledgers.")
	allowLiveOverlap := fs.Bool("allow-live-overlap", false, "DANGEROUS: bypass the live-cursor guard and -write a range the live projector's cursor for a PROJECTED source is still inside. Only pass this if you have independently verified the live projector will not process this range concurrently — see the ADR-0048 D3 one-writer contract on checkCHRebuildLiveOverlap.")
	preflight := fs.Bool("preflight", false, "with -write: run the refusals this exact -write invocation would hit BEFORE it reads the lake — the BackfillSafe gate (both legs), the live-cursor one-writer guard, and the buffered-range ceiling — then print one line naming the sources it would re-derive (`"+chRebuildPreflightPrefix+" [from,to] rederive=a,b,c`) on stdout and exit 0 WITHOUT reading the lake or writing a row (the per-WASM replay gate does read the ClickHouse instance index). A refusal exits non-zero exactly as the real run would. It exists for a caller that must do something destructive before the re-derive (scripts/ops/ch-rebuild-projected.sh DELETEs the window first): ask here, and delete only what this prints. Runtime failures (a lake stream error, a failed write) are by nature not covered.")
	requireRows := fs.String("require-rows", "", "comma-separated sources that must write at least one row: the run fails when one of them re-derives none. scripts/ops/ch-rebuild-projected.sh passes every source whose window its DELETE found occupied, so an emptied window whose re-derive came back empty (an empty gate registry, a lake gap) exits non-zero and stays dirty instead of being marked done. A source absent here may legitimately be quiet over the range.")
	recordDirty := fs.Bool("record-dirty-window", false, "record [from,to] as a pending ADR-0033 projection dirty window for every source named in -sources (each must be a reconciliation-catalogue source; the list is REQUIRED), then exit 0 WITHOUT reading ClickHouse or writing a row. The next compute-completeness re-reconciles that range instead of carrying its prior clean projection claim over it, and clears the obligation only with the verdict that discharges it. This is the RECORD, not a re-derive, so it takes neither -write nor -preflight. It exists for the operator (and scripts/ops/ch-rebuild-projected.sh's TELL THE VERDICT line) after a clean-slate DELETE whose re-derive did not complete: the window is then EMPTY and /v1/coverage must stop certifying it complete (F075). An ordinary -write run records its own rewritten window before it writes; this mode is for a window left EMPTIED.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	write := gate.Enabled()
	if *cfgPath == "" || *from == 0 || *to == 0 || *to < *from {
		return fmt.Errorf("-config, -from, -to are required; -to must be >= -from")
	}
	if err := checkCHRebuildPreflightFlags(*preflight, write); err != nil {
		return err
	}
	if err := checkCHRebuildRecordDirtyFlags(*recordDirty, *preflight, write); err != nil {
		return err
	}
	// -contracts scopes both passes to a contract subset; the sep41 pass pushes
	// it into the CH read as the contract_id prefilter (narrows the scan), the
	// general event pass applies it as a decode-time gate.
	contractsOverride := parseCSVList(*contractsCSV)
	if *sep41SupplyOnly && !*includeSEP41 {
		return fmt.Errorf("-sep41-supply-only requires -sep41")
	}
	// BackfillSafe gate, first leg: sources the operator NAMED.
	// Asked before the config load so the refusal needs no reachable
	// database; the default-all case is asked again once the catalogue
	// exists — see checkCHRebuildBackfillSafe.
	if write {
		if gerr := checkCHRebuildBackfillSafe(parseCSVList(*only)); gerr != nil {
			return gerr
		}
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
	// Stamp a positive derive_generation so corrected values update stored rows in place, through
	// both the trade inserts below and pipeline.HandleEvent, and win over live gen-0 values.
	store.SetDeriveGeneration(time.Now().Unix())

	// usd_volume resolution is mandatory here: every trade written wins the upsert, which assigns
	// `usd_volume = EXCLUDED.usd_volume` unconditionally. Without the resolvers tradeUSDVolume
	// returns nil for DEX and FX-priced CEX trades, overwriting correct values with NULL.
	if err := timescale.InstallUSDVolumeResolution(
		store,
		cfg.Trades.USDPeggedClassicAssets,
		cfg.Supply.SACWrappers,
	); err != nil {
		return err
	}

	// Warn-level logger: HandleEvent debug-logs per event, which would flood at
	// rebuild volume. Errors + warns still surface.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	lo, hi := uint32(*from), uint32(*to)

	cat, soroswapDec, cerr := buildReconciliationCatalogue(cfg)
	if cerr != nil {
		return fmt.Errorf("ch-rebuild: reconciliation catalogue: %w", cerr)
	}
	// -record-dirty-window returns here, before the gate warm-up, factory preseed and any lake
	// read. It writes no served row, only an obligation on the completeness verifier, so nothing
	// downstream may refuse it: a window emptied for a source whose gate has since closed or
	// whose preseed now errors is the hole /v1/coverage must stop certifying clean.
	if *recordDirty {
		named, rerr := chRebuildRecordDirtySources(cat, parseCSVList(*only))
		if rerr != nil {
			return rerr
		}
		return recordCHRebuildDirtyWindows(ctx, store, os.Stdout, lo, hi, named, timescale.CHRebuildEmptiedReason)
	}
	// A -sources name nobody recognises selects nothing and exits 0 —
	// refuse it here, as early as the catalogue exists and before the
	// gate warm-up reads Postgres.
	if serr := checkCHRebuildSources(cat, parseCSVList(*only)); serr != nil {
		return serr
	}
	// Re-derive on the live indexer's gate (curated set plus protocol_contracts), not the bare
	// seed, or `-write` after a truncate would rebuild a table without operator-admitted
	// contracts. Read-only. Must precede the preseed below, which seeds the decoders this rebuilds.
	if cat, cerr = warmCatalogueGates(ctx, store, logger, cat); cerr != nil {
		return fmt.Errorf("ch-rebuild: %w", cerr)
	}

	// The dedicated -sep41 pass below owns sep41_transfers and sep41_supply, with its own prefiltered
	// read, decoders and bookkeeping. The catalogue also promotes them when a watched set is
	// configured, so drop them here: cat carries no sep41 entries until that pass folds in its own
	// sep41Cat, which keeps the main pass's hasEventSource and -sources filtering honest.
	cat = dropReconSources(cat, sep41transfers.SourceName, sep41supply.SourceName)
	// -sep41 opt-in: the sep41 sources stay OUT of the default catalogue
	// (and out of every counting consumer) until the operator truncate+
	// re-derive this flag exists for has run — see buildSEP41ReconSources.
	var sep41Cat []reconSource
	if *includeSEP41 {
		var serr error
		if sep41Cat, serr = buildSEP41ReconSources(cfg); serr != nil {
			return fmt.Errorf("ch-rebuild: -sep41: %w", serr)
		}
	}
	// Seed soroswap pairs from the PG registry, not an RPC factory seed that every windowed run
	// would pay for. The indexer persists every new_pair, and in-range new_pair events are
	// self-discovered.
	if n, serr := seedSoroswapFromPG(ctx, store, soroswapDec); serr != nil {
		fmt.Fprintf(os.Stderr, "ch-rebuild: soroswap PG seed failed (%v) — soroswap may undercount\n", serr)
	} else {
		fmt.Fprintf(os.Stderr, "ch-rebuild: seeded %d soroswap pairs from PG\n", n)
	}

	srcFilter := map[string]bool{}
	for _, s := range strings.Split(*only, ",") {
		if s = strings.TrimSpace(s); s != "" {
			srcFilter[s] = true
		}
	}
	enabled := func(name string) bool { return len(srcFilter) == 0 || srcFilter[name] }

	// ─── ADR-0048 D3 one-writer contract (ported from projected-rebuild) ──
	// A -write pass over a PROJECTED source is a second writer of a domain
	// ADR-0031/0032 gives to the projector alone. Refuse the overlap the
	// same way projected-rebuild does — see checkCHRebuildLiveOverlap.
	passes := chRebuildPasses{sep41: *includeSEP41, contractCalls: *contractCalls, sdex: *includeSDEX}
	if write {
		// BackfillSafe gate, second leg: everything this run would
		// decode, which with no -sources is the whole catalogue.
		if gerr := checkCHRebuildBackfillSafe(reDerivedSourcesInRun(cat, sep41Cat, passes, enabled)); gerr != nil {
			return gerr
		}
		projected := projectedSourcesInRun(cfg, cat, sep41Cat, *includeSEP41, enabled)
		if gerr := checkCHRebuildLiveOverlap(projected, hi, *allowLiveOverlap, func(source string) (uint32, bool, error) {
			c, cerr := store.GetCursor(ctx, "projector", source)
			switch {
			case errors.Is(cerr, timescale.ErrNotFound):
				return 0, false, nil
			case cerr != nil:
				return 0, false, cerr
			}
			return c.LastLedger, true, nil
		}); gerr != nil {
			return gerr
		}
	}

	mode := "DRY-RUN (count only)"
	if write {
		mode = "WRITE"
	}
	// Buffer-pass range guard: every decode pass buffers a whole invocation's decoded events in this
	// process, and an oversized run is killed silently while starving co-located services of
	// memory. A docs note is not a guard; 2M ledgers is a comfortable single-window ceiling.
	const maxBufferedRange = 2_000_000
	if chRebuildBuffers(cat, sep41Cat, passes, enabled) && *to-*from > maxBufferedRange {
		return fmt.Errorf("ch-rebuild: range [%d,%d] spans %d ledgers — every decode pass (event, sep41, sdex, contract-call) buffers in-process; window invocations to <=%d ledgers (loop externally, resume per window)",
			*from, *to, *to-*from, maxBufferedRange)
	}
	// -preflight stops HERE: past the last refusal, before the first lake
	// read. Everything above is read-only against Postgres, so a caller
	// that deletes on the strength of this answer has deleted nothing the
	// run below would then refuse to rewrite.
	if *preflight {
		return reportCHRebuildPreflight(os.Stdout, lo, hi, reDerivedSourcesInRun(cat, sep41Cat, passes, enabled))
	}
	// The per-WASM lake gate opens ClickHouse, so it runs after every cheap
	// refusal and the -preflight stop, and before the first lake read or write.
	if write {
		if gerr := gateCHRebuildLake(ctx, cfg, *chAddr, store, reDerivedSourcesInRun(cat, sep41Cat, passes, enabled), lo, hi); gerr != nil {
			return gerr
		}
	}
	gate.Banner()
	// Factory-anchored sources (ADR-0035): seed each gate registry from the factory's creation
	// events in [genesis, lo) before the re-derive, as verify-reconciliation does. Without it a
	// source with no in-code curated set (blend) re-derives 0 rows above its factory deploys.
	// Read-only and idempotent. It reads the lake, so it sits below the -preflight stop.
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

	// A -write rewrites served rows below the completeness watermark, so the next verdict must
	// re-reconcile [lo,hi], as with projector-replay. Record it before the first write and refuse
	// without it: a run failing partway has touched a subset the record already covers.
	if write {
		if rederived := reDerivedSourcesInRun(cat, sep41Cat, passes, enabled); len(rederived) > 0 {
			if rerr := recordCHRebuildDirtyWindows(ctx, store, os.Stderr, lo, hi, rederived, timescale.CHRebuildWriteReason); rerr != nil {
				return fmt.Errorf("%w (refusing to write without it — the completeness verdict would carry a stale claim over the rewritten range)", rerr)
			}
		}
	}

	fmt.Fprintf(os.Stderr, "ch-rebuild: [%d,%d] mode=%s sources=%q sdex=%v contract-calls=%v sep41=%v ch=%s\n",
		lo, hi, mode, *only, *includeSDEX, *contractCalls, *includeSEP41, *chAddr)

	// Buffer decoded events during the CH stream and write to Postgres after it closes, since a
	// FINAL stream held open across slow per-row writes trips the client read timeout. Windows are
	// partition-aligned (1M) to bound memory.
	var buf []consumer.Event

	// ─── Event-based pass (read → buffer) ────────────────────────────────
	// Skip unless an enabled source has an event decoder: the scan covers the whole range
	// regardless of how many decoders fire, so a ContractCall-only run would scan for nothing.
	hasEventSource := false
	for _, src := range cat {
		if src.dec != nil && enabled(src.name) {
			hasEventSource = true
			break
		}
	}
	if hasEventSource {
		evStart := time.Now()
		var everr error
		if buf, everr = runCHRebuildEventPass(ctx, chRebuildLakeReads(*chAddr), lo, hi, cat, enabled, contractsOverride); everr != nil {
			return everr
		}
		fmt.Fprintf(os.Stderr, "ch-rebuild: event read done in %s (%d events buffered)\n",
			time.Since(evStart).Round(time.Second), len(buf))
	} else {
		fmt.Fprintln(os.Stderr, "ch-rebuild: event pass skipped (no enabled event-decoder source)")
	}

	// ─── SEP-41 pass (opt-in; read → buffer) ─────────────────────────────
	// The sep41 topics are the CAP-67 firehose the main pass excludes (FirehoseExcludeSyms), so
	// stream a contract_id-prefiltered read of the watched set instead. FINAL because the dry run
	// counts: unmerged ReplacingMergeTree duplicates would inflate it (ON CONFLICT absorbs them on write).
	if *includeSEP41 {
		anySEP41 := false
		for _, src := range sep41Cat {
			if enabled(src.name) {
				anySEP41 = true
				break
			}
		}
		if anySEP41 {
			// Contract prefilter: default is the whole watched set; -contracts
			// narrows the CH read to a subset (the scoped dropped-rows recovery).
			sep41Contracts := cfg.Supply.WatchedSEP41Contracts
			if len(contractsOverride) > 0 {
				sep41Contracts = contractsOverride
				// The sep41 decoders gate Matches() on the FULL watched set, so a
				// -contracts entry outside it is read but decoded to nothing — a
				// likely typo that would leave the dominant-burn alerts firing.
				for _, c := range contractsOverride {
					if !containsStr(cfg.Supply.WatchedSEP41Contracts, c) {
						fmt.Fprintf(os.Stderr, "ch-rebuild: WARNING -contracts %s is not in [supply] watched_sep41_contracts — the sep41 decoder will not match it (recovers nothing)\n", c)
					}
				}
			}
			// Topic prefilter: -sep41-supply-only reads ONLY the supply-affecting
			// topics (mint/burn/clawback), skipping the transfer firehose at the
			// SQL layer. Invalid with sep41_transfers enabled — it would silently
			// drop transfer recovery — so require -sources sep41_supply.
			var sep41Topic0 []string
			if *sep41SupplyOnly {
				if enabled(sep41transfers.SourceName) {
					return fmt.Errorf("-sep41-supply-only excludes the transfer topic firehose but sep41_transfers is enabled (it would silently recover nothing); restrict with -sources sep41_supply")
				}
				sep41Topic0 = []string{sep41supply.SymbolMint, sep41supply.SymbolBurn, sep41supply.SymbolClawback}
			}
			sepStart := time.Now()
			before := len(buf)
			seperr := clickhouse.StreamContractEventsFiltered(ctx, *chAddr, lo, hi,
				sep41Contracts, sep41Topic0, nil,
				true,  // FINAL: the dry-run COUNTS (see above)
				false, // no op_args_xdr: the sep41 decoders decode from topics+data, never OpArgs
				false, // no state-write keys: sep41 decoders never read them
				func(ev events.Event) error {
					for _, src := range sep41Cat {
						if !enabled(src.name) || !src.dec.Matches(ev) {
							continue
						}
						outs, derr := src.dec.Decode(ev)
						if derr != nil {
							continue // soft-fail, mirroring the live dispatcher path
						}
						buf = append(buf, outs...)
					}
					return nil
				})
			if seperr != nil {
				return fmt.Errorf("ch-rebuild: sep41 event stream: %w", seperr)
			}
			fmt.Fprintf(os.Stderr, "ch-rebuild: sep41 read done in %s (%d events buffered)\n",
				time.Since(sepStart).Round(time.Second), len(buf)-before)
		}
		// Fold into the catalogue AFTER the read passes so the report loop
		// covers them; the main event pass above must never see them (its
		// stream excludes their topics — they'd silently count zero).
		cat = append(cat, sep41Cat...)
	}

	// ─── SDEX op-based pass (opt-in; read → buffer) ──────────────────────
	if *includeSDEX && enabled("sdex") {
		sdexDec := sdex.NewDecoder()
		sStart := time.Now()
		decodeRange := func(rlo, rhi uint32) error {
			// SDEX Decode never returns a non-nil error (soft-fails per claim).
			return clickhouse.StreamSDEXOps(ctx, *chAddr, rlo, rhi, func(op clickhouse.SDEXOp) error {
				outs, _ := sdexDec.Decode(dispatcher.OpContext{
					Ledger:   op.Ledger,
					ClosedAt: op.ClosedAt,
					TxHash:   op.TxHash,
					TxSource: op.Source,
					OpIndex:  int(op.OpIndex),
					Op:       op.Op,
					OpResult: op.OpResult,
				})
				buf = append(buf, outs...)
				return nil
			})
		}
		if *sdexGaps {
			// Re-derive ONLY the served gaps in one pass: each gap is an empty
			// ledger range, so the decode + write is a pure insert (no ON CONFLICT
			// walk over the 121M served rows that makes a full-range pass slow).
			// This clears the dual-sink drop backlog cheaply and safely.
			targets, terr := ingest.ResolveFindDataGapsTargets("sdex")
			if terr != nil {
				return fmt.Errorf("ch-rebuild: sdex gap targets: %w", terr)
			}
			var ng int
			for _, tgt := range targets {
				gaps, gerr := store.FindPerSourceLedgerGaps(ctx, tgt, int64(lo), int64(hi), 1, 0)
				if gerr != nil {
					return fmt.Errorf("ch-rebuild: find sdex gaps: %w", gerr)
				}
				for _, g := range gaps {
					if derr := decodeRange(uint32(g.Start), uint32(g.End)); derr != nil { //nolint:gosec // ledger seq fits uint32
						return fmt.Errorf("ch-rebuild: sdex gap [%d,%d]: %w", g.Start, g.End, derr)
					}
					ng++
				}
			}
			fmt.Fprintf(os.Stderr, "ch-rebuild: SDEX gap-only read done (%d gaps) in %s\n", ng, time.Since(sStart).Round(time.Second))
		} else if *sdexReconcile {
			// Re-derive only partial-drop ledgers, where the distinct Validate-passing census exceeds
			// the served count; the empty-gap pass misses these because served > 0. Per 100k window,
			// queue every event of a short ledger. Writing the full set is fine, as ON CONFLICT
			// no-ops existing rows.
			type pk struct {
				tx string
				op uint32
			}
			// 25k: the sdex reconcile joins OOM'd at 100k even
			// under grace_hash — and a wedged-CH
			// bad_alloc followed the same heavy sequence. Match
			// compute_completeness's window.
			const rwin = 25_000
			var nShort int
			for wlo := lo; wlo <= hi; wlo += rwin {
				whi := wlo + rwin - 1
				if whi > hi {
					whi = hi
				}
				byLedger := make(map[uint32][]consumer.Event)
				seen := make(map[uint32]map[pk]struct{})
				if derr := clickhouse.StreamSDEXOps(ctx, *chAddr, wlo, whi, func(op clickhouse.SDEXOp) error {
					outs, _ := sdexDec.Decode(dispatcher.OpContext{
						Ledger:   op.Ledger,
						ClosedAt: op.ClosedAt,
						TxHash:   op.TxHash,
						TxSource: op.Source,
						OpIndex:  int(op.OpIndex),
						Op:       op.Op,
						OpResult: op.OpResult,
					})
					for _, ev := range outs {
						te, ok := ev.(sdex.TradeEvent)
						// Zero-leg fills stay out of census AND served count until
						// a full-history walk lands them; see sdexPriceableFilter.
						if !ok || te.Trade.Validate() != nil || timescale.IsOneSideZeroFill(te.Trade) {
							continue
						}
						byLedger[te.Trade.Ledger] = append(byLedger[te.Trade.Ledger], ev)
						s := seen[te.Trade.Ledger]
						if s == nil {
							s = make(map[pk]struct{})
							seen[te.Trade.Ledger] = s
						}
						s[pk{tx: te.Trade.TxHash, op: te.Trade.OpIndex}] = struct{}{}
					}
					return nil
				}); derr != nil {
					return fmt.Errorf("ch-rebuild: sdex reconcile stream [%d,%d]: %w", wlo, whi, derr)
				}
				served, serr := store.CountRowsByLedger(ctx, "trades", "ledger", "source='sdex' AND "+sdexPriceableFilter, wlo, whi)
				if serr != nil {
					return fmt.Errorf("ch-rebuild: sdex reconcile served counts [%d,%d]: %w", wlo, whi, serr)
				}
				for ledger, evs := range byLedger {
					if len(seen[ledger]) > served[ledger] {
						buf = append(buf, evs...)
						nShort++
					}
				}
			}
			fmt.Fprintf(os.Stderr, "ch-rebuild: SDEX reconcile read done (%d short ledgers) in %s\n", nShort, time.Since(sStart).Round(time.Second))
		} else if derr := decodeRange(lo, hi); derr != nil {
			return fmt.Errorf("ch-rebuild: sdex op stream: %w", derr)
		} else {
			fmt.Fprintf(os.Stderr, "ch-rebuild: SDEX read done in %s\n", time.Since(sStart).Round(time.Second))
		}
	}

	// ─── ContractCall pass (opt-in; read → buffer) ───────────────────────
	// Event-less ContractCall sources emit no Soroban events, so neither the event pass nor the
	// projector can rebuild them. forEachContractCallEvent is shared with the census, so written
	// rows reconcile to it exactly.
	if *contractCalls {
		ccStart := time.Now()
		for _, src := range cat {
			if src.callDec == nil || !enabled(src.name) {
				continue
			}
			before := len(buf)
			// Blind spots are the writer's side of the decode-blind symmetry: a call this stream
			// cannot decode is a row not written, as the census cannot expect it. Reported so the
			// rebuild states what it dropped.
			ccBlind, cerr := forEachContractCallEvent(ctx, *chAddr, src.callContract, src.callDec, lo, hi, func(_ uint32, ev consumer.Event) error {
				buf = append(buf, ev)
				return nil
			})
			if cerr != nil {
				return fmt.Errorf("ch-rebuild: contract-call stream %s: %w", src.name, cerr)
			}
			if ccBlind.Any() {
				fmt.Fprintf(os.Stderr, "ch-rebuild: contract-call %s — %s\n", src.name, ccBlind.Detail())
			}
			fmt.Fprintf(os.Stderr, "ch-rebuild: contract-call %s read done (%d events)\n", src.name, len(buf)-before)
		}
		fmt.Fprintf(os.Stderr, "ch-rebuild: contract-call read done in %s\n", time.Since(ccStart).Round(time.Second))
	}

	// ─── write the buffered events to Postgres ───────────────────────────
	// drainAndWrite counts an event in written[] only after its insert is confirmed and tallies
	// failures in failed[], so the report and exit code cannot call a partial recovery complete.
	w := eventWriter{
		batchTrades: store.BatchInsertTrades,
		insertTrade: store.InsertTrade,
		copyXfer:    store.CopyMergeSEP41Transfers,
		insertXfer:  store.InsertSEP41Transfer,
		copySup:     store.CopyMergeSEP41SupplyEvents,
		insertSup:   store.InsertSEP41SupplyEvent,
		handle: func(ctx context.Context, ev consumer.Event) error {
			return pipeline.HandleEvent(ctx, logger, store, ev)
		},
	}
	if *bulkTrades {
		// Opt-in, and only ever set here: the live indexer never builds an
		// eventWriter, so nothing outside this flag can reach the bulk writer.
		w.bulkTrades = func(ctx context.Context, batch []canonical.Trade) (timescale.BulkBackfillResult, error) {
			return store.BulkBackfillTrades(ctx, batch, timescale.BulkBackfillOptions{})
		}
	}
	written, failed := drainAndWrite(ctx, logger, w, buf, write)

	if write {
		// ─── reset the SEP-41 supply rollup fold checkpoint ──────────────
		// A -sep41 -write rewrites sep41_supply_events below the rollup checkpoint and the worker
		// folds only `ledger > last_ledger`, so without a reset a full re-derive double-counts
		// supply and a scoped one never folds. Reset after the drain, so a concurrent worker cannot
		// advance the checkpoint mid-write. It preserves the genesis baseline.
		if reset, resetContracts := sep41RollupResetPlan(*includeSEP41, write, enabled(sep41supply.SourceName), contractsOverride); reset {
			n, rerr := store.ResetSEP41SupplyRollupFold(ctx, resetContracts)
			if rerr != nil {
				return fmt.Errorf("ch-rebuild: sep41 rollup reset: %w", rerr)
			}
			scope := "FULL — all watched contracts"
			if len(resetContracts) > 0 {
				scope = fmt.Sprintf("SCOPED — %d contract(s)", len(resetContracts))
			}
			fmt.Fprintf(os.Stderr, "ch-rebuild: reset %d sep41_supply_rollup fold row(s) [%s], each re-folded from zero in place (genesis baseline preserved)\n", n, scope)
		}
	}

	// The stale-row sweep keys on this run's own generation, so it runs only
	// here and in projected-rebuild, never under projector-replay (gen 0).
	serr := sweepCHRebuildOracle(ctx, os.Stderr, store, cat, enabled, *contractCalls, failed, lo, hi, store.DeriveGeneration(), write)

	// ─── report ──────────────────────────────────────────────────────────
	fmt.Printf("\n=== ch-rebuild [%d,%d] %s ===\n", lo, hi, mode)
	rerr := reportCHRebuildCounts(os.Stdout, cat, reDerivedSourcesInRun(cat, sep41Cat, passes, enabled), parseCSVList(*requireRows), written, failed)
	if !write {
		fmt.Printf("\n(dry-run — re-run with -write to persist to Postgres)\n")
	}
	return errors.Join(rerr, serr)
}

// reportCHRebuildCounts prints a row per source that wrote or failed anything, and per re-derived
// source even at zero, so an empty re-derive of a cleared window shows as a row, not absence. It
// returns the error gating the exit code: a failed write, or a required source that wrote no row.
func reportCHRebuildCounts(w io.Writer, cat []reconSource, reDerived, required []string, written, failed map[string]int) error {
	_, _ = fmt.Fprintf(w, "%-16s %14s %14s\n", "source", "written", "failed")
	var total, totalFailed int
	var empty []string
	for _, src := range cat {
		n, okW := written[src.name]
		f, okF := failed[src.name]
		if !okW && !okF {
			if !containsStr(reDerived, src.name) {
				continue
			}
			empty = append(empty, src.name)
		}
		_, _ = fmt.Fprintf(w, "%-16s %14d %14d\n", src.name, n, f)
		total += n
		totalFailed += f
	}
	_, _ = fmt.Fprintf(w, "%-16s %14d %14d\n", "TOTAL", total, totalFailed)
	if len(empty) > 0 {
		_, _ = fmt.Fprintf(w, "\nre-derived NO rows for: %s — confirm the range had no activity for them before treating it as rebuilt\n", strings.Join(empty, ","))
	}
	// A partially-failed write must NOT present as complete (exit 0);
	// failed rows were excluded from written[] and the operator re-runs.
	if totalFailed > 0 {
		return fmt.Errorf("ch-rebuild: %d event(s) failed to write (rows missing) — see the 'failed' column and re-run to recover", totalFailed)
	}
	var missing []string
	for _, name := range required {
		if written[name] == 0 {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("ch-rebuild: wrote no row for %s, which -require-rows says held rows in this range (rows missing) — the range is not rebuilt", strings.Join(missing, ","))
	}
	return nil
}

// eventWriter abstracts the Postgres write operations drainAndWrite performs.
// chRebuild wires it to the concrete *timescale.Store + pipeline.HandleEvent;
// tests inject fakes that fail selected inserts to exercise the written/failed counting.
type eventWriter struct {
	batchTrades func(context.Context, []canonical.Trade) error
	insertTrade func(context.Context, canonical.Trade) error
	// bulkTrades, when non-nil (-bulk-trades), replaces the per-batch upsert as the primary trade
	// writer and raises the batch size to bulkTradeBatchN (see timescale.Store.BulkBackfillTrades).
	// The per-row insertTrade fallback still catches whatever it cannot land.
	bulkTrades func(context.Context, []canonical.Trade) (timescale.BulkBackfillResult, error)
	copyXfer   func(context.Context, []timescale.SEP41TransferRow) error
	insertXfer func(context.Context, timescale.SEP41TransferRow) error
	copySup    func(context.Context, []timescale.SEP41SupplyEvent) error
	insertSup  func(context.Context, timescale.SEP41SupplyEvent) error
	handle     func(context.Context, consumer.Event) error
}

// Trade batch sizes for drainAndWrite's flush. The upsert path is capped by
// Postgres's 65535 bind parameters (13 per row); the bulk path has no
// placeholder ceiling at all - COPY streams - so it is sized for amortising
// the emptiness proof and filling the parallel writers instead.
const (
	upsertTradeBatchN = 1000
	bulkTradeBatchN   = 100_000
)

// writeTradeBatch lands one trade batch through the bulk COPY writer under -bulk-trades, otherwise
// the generation-guarded batch upsert. An error drops the caller into the per-row fallback. A bulk
// call that refused its own precondition is not an error (the rows landed via upsert) but is
// reported, since a silent revert off the fast path makes throughput measurements lie.
func writeTradeBatch(ctx context.Context, logger *slog.Logger, w eventWriter, batch []canonical.Trade) error {
	if w.bulkTrades == nil {
		return w.batchTrades(ctx, batch)
	}
	res, err := w.bulkTrades(ctx, batch)
	if err != nil {
		return err
	}
	if res.Path != timescale.BulkBackfillPathCopy {
		logger.Warn("bulk trade write fell back to the upsert path",
			"n", len(batch), "reason", res.FallbackReason)
	}
	return nil
}

// tallyTrade counts a trade the batch writer accepted: a Validate-passing row
// (including a one-side-zero SDEX fill) is stored; any other row is a decoder
// bug the store's filter drops, so it must fail the run.
func tallyTrade(t canonical.Trade, src string, written, failed map[string]int) {
	if t.Validate() == nil {
		written[src]++
		return
	}
	failed[src]++
}

// drainAndWrite persists buffered events to Postgres and returns per-source written and failed
// tallies. Batches fall back per-row through the same generation-guarded upsert, so the outcome
// is unchanged; an event counts as written only once its insert is confirmed. Dry run predicts the split.
func drainAndWrite(ctx context.Context, logger *slog.Logger, w eventWriter, buf []consumer.Event, write bool) (written, failed map[string]int) { //nolint:gocognit,gocyclo,funlen // linear: three symmetric batch/flush closures + a per-event dispatch; splitting the flush closures apart hurts clarity.
	written = map[string]int{}
	failed = map[string]int{}

	// The bulk writer wants one large buffer: its emptiness proof costs a round trip per call and
	// COPY only pays above a few thousand rows. The upsert writer keeps 1000 rows (13 params/row
	// against Postgres's 65535 parameter ceiling).
	tradeBatchN := upsertTradeBatchN
	if w.bulkTrades != nil {
		tradeBatchN = bulkTradeBatchN
	}
	batch := make([]canonical.Trade, 0, tradeBatchN)
	batchSrc := make([]string, 0, tradeBatchN)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := writeTradeBatch(ctx, logger, w, batch); err != nil {
			logger.Warn("batch trade insert failed; per-row fallback", "n", len(batch), "err", err)
			for i, t := range batch {
				if ierr := w.insertTrade(ctx, t); ierr != nil {
					logger.Error("per-row trade insert failed", "source", batchSrc[i], "err", ierr)
					failed[batchSrc[i]]++
				} else {
					written[batchSrc[i]]++
				}
			}
		} else {
			// The writer succeeds while silently dropping Validate-failing rows
			// (filterStorableTrades); those never landed.
			for i, s := range batchSrc {
				tallyTrade(batch[i], s, written, failed)
			}
		}
		batch = batch[:0]
		batchSrc = batchSrc[:0]
	}
	// sep41 batches: the full-history re-derive buffers tens of
	// millions of sep41 events per window; per-row HandleEvent caps writes at
	// ~520/s. Same batching pattern as trades, same per-row fallback.
	const sepBatchN = 50_000
	xferBatch := make([]timescale.SEP41TransferRow, 0, sepBatchN)
	xferSrc := make([]string, 0, sepBatchN)
	flushXfer := func() {
		if len(xferBatch) == 0 {
			return
		}
		if err := w.copyXfer(ctx, xferBatch); err != nil {
			logger.Warn("sep41_transfers batch failed; per-row fallback", "n", len(xferBatch), "err", err)
			for i, r := range xferBatch {
				if ierr := w.insertXfer(ctx, r); ierr != nil {
					logger.Error("per-row sep41 transfer insert failed", "source", xferSrc[i], "err", ierr)
					failed[xferSrc[i]]++
				} else {
					written[xferSrc[i]]++
				}
			}
		} else {
			for _, s := range xferSrc {
				written[s]++
			}
		}
		xferBatch = xferBatch[:0]
		xferSrc = xferSrc[:0]
	}
	supBatch := make([]timescale.SEP41SupplyEvent, 0, sepBatchN)
	supSrc := make([]string, 0, sepBatchN)
	flushSup := func() {
		if len(supBatch) == 0 {
			return
		}
		if err := w.copySup(ctx, supBatch); err != nil {
			logger.Warn("sep41_supply batch failed; per-row fallback", "n", len(supBatch), "err", err)
			for i, r := range supBatch {
				if ierr := w.insertSup(ctx, r); ierr != nil {
					logger.Error("per-row sep41 supply insert failed", "source", supSrc[i], "err", ierr)
					failed[supSrc[i]]++
				} else {
					written[supSrc[i]]++
				}
			}
		} else {
			for _, s := range supSrc {
				written[s]++
			}
		}
		supBatch = supBatch[:0]
		supSrc = supSrc[:0]
	}

	wStart := time.Now()
	fmt.Fprintf(os.Stderr, "ch-rebuild: drain start (%d events)\n", len(buf))
	for i, ev := range buf {
		if i > 0 && i%1_000_000 == 0 {
			fmt.Fprintf(os.Stderr, "ch-rebuild: drain %dM/%dM in %s\n", i/1_000_000, len(buf)/1_000_000, time.Since(wStart).Round(time.Second))
		}
		if !write { // dry-run: count what WOULD be written
			if t, ok := tradeOf(ev); ok {
				tallyTrade(t, ev.Source(), written, failed)
			} else {
				written[ev.Source()]++
			}
			continue
		}
		switch e := ev.(type) {
		case sep41transfers.Event:
			xferBatch = append(xferBatch, pipeline.SEP41TransferRowOf(e))
			xferSrc = append(xferSrc, ev.Source())
			if len(xferBatch) >= sepBatchN {
				flushXfer()
			}
			continue // counted in flushXfer once the insert is confirmed
		case sep41supply.Event:
			supBatch = append(supBatch, pipeline.SEP41SupplyRowOf(e))
			supSrc = append(supSrc, ev.Source())
			if len(supBatch) >= sepBatchN {
				flushSup()
			}
			continue // counted in flushSup once the insert is confirmed
		}
		if t, ok := tradeOf(ev); ok {
			// Cut only on a ledger boundary: the bulk writer's emptiness probe spans the batch's
			// ledger extent, so a ledger split across batches makes the second refuse the COPY. This
			// helps only a ledger-ordered buffer, which -sdex-reconcile's is not.
			if len(batch) >= tradeBatchN && batch[len(batch)-1].Ledger != t.Ledger {
				flush()
			}
			batch = append(batch, t)
			batchSrc = append(batchSrc, ev.Source())
			continue // counted in flush once the insert is confirmed
		}
		// Protocol-entity path: HandleEvent performs the insert. Capture
		// its error and count only on success, so a failed write cannot
		// inflate the completion report.
		if herr := w.handle(ctx, ev); herr != nil {
			logger.Error("HandleEvent insert failed", "source", ev.Source(), "err", herr)
			failed[ev.Source()]++
		} else {
			written[ev.Source()]++
		}
	}
	if write {
		flush()
		flushXfer()
		flushSup()
		var wrote int
		for _, n := range written {
			wrote += n
		}
		fmt.Fprintf(os.Stderr, "ch-rebuild: wrote %d events in %s\n", wrote, time.Since(wStart).Round(time.Second))
	}
	return written, failed
}

// parseCSVList splits a comma-separated flag value into a trimmed,
// order-preserving, de-duplicated slice (empty entries dropped). Used for the
// -contracts scope filter (mirrors the inline -sources split).
func parseCSVList(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// contractAllowed reports whether a contract passes the -contracts scope gate. The sep41 pass
// instead pushes the subset into the CH read as the contract_id prefilter.
func contractAllowed(override []string, contractID string) bool {
	return len(override) == 0 || containsStr(override, contractID)
}

// sep41RollupResetPlan decides whether a -sep41 -write run must reset the sep41_supply_rollup
// fold checkpoint, avoiding double-counted or unfolded below-checkpoint rows. It applies only
// when the supply source is re-derived; the contracts are nil for a full reset or the -contracts override.
func sep41RollupResetPlan(includeSEP41, write, supplyEnabled bool, contractsOverride []string) (reset bool, contracts []string) {
	if !includeSEP41 || !write || !supplyEnabled {
		return false, nil
	}
	return true, contractsOverride
}

// dropReconSources returns cat without the named entries, in order, so the dedicated -sep41 pass
// stays the sole source of sep41 entries even if the catalogue already promoted them.
func dropReconSources(cat []reconSource, names ...string) []reconSource {
	drop := make(map[string]bool, len(names))
	for _, n := range names {
		drop[n] = true
	}
	out := make([]reconSource, 0, len(cat))
	for _, src := range cat {
		if drop[src.name] {
			continue
		}
		out = append(out, src)
	}
	return out
}

// chRebuildEventReads is the pair of lake reads the event pass issues; tests
// substitute fakes for ClickHouse.
type chRebuildEventReads struct {
	firehose func(ctx context.Context, from, to uint32, excludeTopic0 []string, fn func(events.Event) error) error
	scoped   func(ctx context.Context, from, to uint32, contractIDs []string, withOpArgs bool, fn func(events.Event) error) error
}

func chRebuildLakeReads(addr string) chRebuildEventReads {
	return chRebuildEventReads{
		firehose: func(ctx context.Context, from, to uint32, excludeTopic0 []string, fn func(events.Event) error) error {
			return clickhouse.StreamContractEvents(ctx, addr, from, to, excludeTopic0, fn)
		},
		scoped: func(ctx context.Context, from, to uint32, contractIDs []string, withOpArgs bool, fn func(events.Event) error) error {
			// FINAL: the dry-run counts, so un-merged duplicate parts must collapse.
			return clickhouse.StreamContractEventsFiltered(ctx, addr, from, to, contractIDs, nil, nil, true, withOpArgs, false, fn)
		},
	}
}

// runCHRebuildEventPass decodes [lo,hi] through every enabled event-decoder
// source and returns the decoded outputs in read order.
func runCHRebuildEventPass(ctx context.Context, reads chRebuildEventReads, lo, hi uint32, cat []reconSource, enabled func(string) bool, contractsOverride []string) ([]consumer.Event, error) {
	firehoseSrcs, scopedSrcs, err := splitCHRebuildEventSources(cat, enabled)
	if err != nil {
		return nil, err
	}
	var buf []consumer.Event
	if len(firehoseSrcs) > 0 {
		// Exclude the CAP-67 classic-token firehose (99.99% of contract_events);
		// sources whose decoders consume those topics take the scoped read below.
		// FirehoseExcludeSyms, not ClassicTokenTopic0Syms: Blend and Comet emit a
		// pool set_admin sharing the token topic.
		if err := reads.firehose(ctx, lo, hi, clickhouse.FirehoseExcludeSyms, decodeCHRebuildEvents(firehoseSrcs, contractsOverride, &buf)); err != nil {
			return nil, fmt.Errorf("ch-rebuild: event stream: %w", err)
		}
		buf = append(buf, drainCHRebuildSources(firehoseSrcs)...)
	}
	if len(scopedSrcs) > 0 {
		var ids []string
		withOpArgs := false
		for _, src := range scopedSrcs {
			ids = append(ids, src.contractIDs...)
			withOpArgs = withOpArgs || src.needsOpArgs
		}
		if err := reads.scoped(ctx, lo, hi, ids, withOpArgs, decodeCHRebuildEvents(scopedSrcs, contractsOverride, &buf)); err != nil {
			return nil, fmt.Errorf("ch-rebuild: contract-scoped event stream: %w", err)
		}
		buf = append(buf, drainCHRebuildSources(scopedSrcs)...)
	}
	return buf, nil
}

// drainCHRebuildSources flushes correlation groups still open once a stream
// has ended; no later event exists to emit them.
func drainCHRebuildSources(srcs []reconSource) []consumer.Event {
	var out []consumer.Event
	for _, src := range srcs {
		out = append(out, dispatcher.Drain(src.dec)...)
	}
	return out
}

// splitCHRebuildEventSources partitions the enabled event-decoder sources into
// those read from the topic-excluded firehose and those read by contract id.
func splitCHRebuildEventSources(cat []reconSource, enabled func(string) bool) (firehose, scoped []reconSource, err error) {
	for _, src := range cat {
		if src.dec == nil || !enabled(src.name) {
			continue
		}
		if !src.firehoseTopics {
			firehose = append(firehose, src)
			continue
		}
		// An unscoped read without the exclusion would stream the whole CAP-67 firehose.
		if len(src.contractIDs) == 0 {
			return nil, nil, fmt.Errorf("ch-rebuild: %s consumes firehose topics but has no contract prefilter", src.name)
		}
		scoped = append(scoped, src)
	}
	return firehose, scoped, nil
}

// decodeCHRebuildEvents returns the stream callback that runs each event
// through srcs' decoders and appends the outputs to buf.
func decodeCHRebuildEvents(srcs []reconSource, contractsOverride []string, buf *[]consumer.Event) func(events.Event) error {
	return func(ev events.Event) error {
		if !contractAllowed(contractsOverride, ev.ContractID) {
			return nil // -contracts scope: skip events outside the subset
		}
		for _, src := range srcs {
			if len(src.contractIDs) > 0 && !containsStr(src.contractIDs, ev.ContractID) {
				continue
			}
			if !src.dec.Matches(ev) {
				continue
			}
			outs, derr := src.dec.Decode(ev)
			if derr != nil {
				continue // soft-fail, mirroring the projector + live path
			}
			*buf = append(*buf, outs...)
		}
		return nil
	}
}

// chRebuildBuffers reports whether any pass of this invocation appends to
// the in-process buffer. Every pass does, so it keys on the same selection
// as the BackfillSafe gate rather than on event decoders alone.
func chRebuildBuffers(cat, sep41Cat []reconSource, passes chRebuildPasses, enabled func(string) bool) bool {
	return passes.sep41 || len(reDerivedSourcesInRun(cat, sep41Cat, passes, enabled)) > 0
}
