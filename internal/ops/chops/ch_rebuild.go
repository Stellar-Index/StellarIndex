package chops

import (
	"context"
	"errors"
	"flag"
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

// projectorCursorReader reports the live projector's cursor position for
// one source: (lastLedger, true, nil) when a cursor row exists, and
// (0, false, nil) when the projector has never run for that source. The
// seam exists so the guard below is unit-testable with a scripted cursor
// (mirroring projected_rebuild's checkLiveCursorGuard tests) instead of a
// live ingestion_cursors table.
type projectorCursorReader func(source string) (uint32, bool, error)

// checkCHRebuildLiveOverlap is projected-rebuild's ADR-0048 D3 one-writer
// contract (see checkLiveCursorGuard in projected_rebuild.go), ported to
// ch-rebuild's multi-source shape.
//
// WHY IT HAS TO BE HERE TOO: `ch-rebuild -write` re-derives the SAME
// projected domains the live projector owns (ADR-0031/0032 give each of
// them exactly one writer), through the same decoders and the same
// pipeline.HandleEvent sink — and it stamps a positive derive_generation,
// so its rows WIN the ON CONFLICT guard over the live projector's gen-0
// values. Run with `-to <tip>` it is therefore a second writer inside the
// live projector's committed range, re-stamping rows the projector may
// still be writing. Row content is identical on the same binary, so this
// is an invariant-7 violation by construction rather than data loss — but
// the sibling bulk path (projected-rebuild) already refuses it, and a
// guard on one of two tools is not a guard. The runbooks that prescribe
// `ch-rebuild ... -to <tip>` (see docs/architecture/ingest-pipeline.md's
// replay decision rule) cannot make it safe; this refusal can.
//
// Semantics are byte-for-byte projected-rebuild's, per source: allowed
// only when the live projector's cursor for that source is AT OR ABOVE
// the requested top, i.e. this pass fills history strictly BEHIND a
// live-current source. No cursor row at all is treated as "cursor at 0"
// (a never-run projector WILL walk this range once it starts), and
// -allow-live-overlap is the operator's explicit "I verified this is
// safe" override. Non-projected passes (-sdex, -contract-calls, band /
// soroswap-router) are not affected: nothing else writes those domains.
//
// DELIBERATE DIVERGENCE from projected-rebuild: that command applies the
// guard to dry-runs too; this one arms it only under -write. ch-rebuild's
// default mode writes nothing at all (it is the count/compare report that
// ch-reproject shares), so a dry run cannot race any writer, and refusing
// it would break the read-only reporting every operator procedure starts
// with.
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

// projectedSourcesInRun returns the names of the sources this invocation
// would WRITE that the live projector also writes — resolved from
// projector.BuildRegistry (the same registry.go the indexer's projector
// goroutine builds from) rather than a list kept here, so a source added
// to the projector is covered by the guard on its first run.
//
// A per-name BuildRegistry error means the name HAS a projector entry
// whose config is incomplete; that is still the projector's domain, so it
// counts as projected (fail-closed) rather than aborting the run — which
// is why this returns no error of its own.
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

// reDerivedSourcesInRun returns the name of every source this invocation
// would DECODE — the subject of the BackfillSafe gate. It mirrors, pass
// by pass, the selection each pass in chRebuild applies: an event source
// needs a decoder, a ContractCall source needs -contract-calls, sdex
// needs -sdex, the sep41 pair needs -sep41; all respect -sources.
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

// checkCHRebuildBackfillSafe refuses a -write run that would decode a
// source whose decoder has not been audited against every WASM
// generation that ran over its history (finding F050).
//
// ch-rebuild runs the CURRENT decoders over a HISTORICAL lake range and
// — because it stamps a positive derive_generation — its rows WIN over
// what is stored. That is `backfill`'s old-WASM-generation hazard with a
// stronger writer, yet `backfill` was the only command that asked
// external.BackfillSafe. The question goes through
// [external.ReplayBackfillSafe], which resolves the projector-namespace
// names (blend_backstop, the sep41 pair) that have no registry row of
// their own; a name nobody registered is refused, which also turns a
// -sources typo from a silent rebuild-of-nothing into an error.
//
// Armed under -write only, the same deliberate divergence
// checkCHRebuildLiveOverlap documents: the default mode writes nothing,
// and the dry-run count/compare report is precisely how an unaudited
// decoder gets evaluated against history. No override flag, matching
// `backfill` — the way through is the audit plus the registry flip.
func checkCHRebuildBackfillSafe(sources []string) error {
	unsafeSources := external.UnsafeReplaySources(sources)
	if len(unsafeSources) == 0 {
		return nil
	}
	return fmt.Errorf("ch-rebuild: refusing to -write — sources not BackfillSafe (per-WASM-hash audit pending, or not a known source): %v. "+
		"This pass decodes history with the CURRENT decoders and its rows overwrite the stored ones; Soroban contracts upgrade in place, "+
		"so an unaudited old WASM generation decodes to silently wrong rows. Restrict -sources to audited sources (a run with no -sources "+
		"selects the whole catalogue), or run stellarindex-ops wasm-history over each source's contracts, record the audit under "+
		"docs/operations/wasm-audits/, and flip BackfillSafe=true in internal/sources/external/registry.go in the same PR. "+
		"The default dry-run is not gated",
		unsafeSources)
}

// chRebuildPreflightPrefix opens the single stdout line a successful
// `ch-rebuild -write -preflight` prints. It is a CONTRACT with
// scripts/ops/ch-rebuild-projected.sh, which selects the line by this
// prefix and reads the `rederive=` list off its end — change the two
// together (TestChRebuildProjectedScript_ParsesTheRealPreflightLine feeds
// this function's output to the shipped script).
const chRebuildPreflightPrefix = "ch-rebuild: preflight ok"

// checkCHRebuildPreflightFlags refuses -preflight without -write. The
// guards a preflight exists to run are all -write guards (a dry run writes
// nothing and is deliberately not guarded), so a bare -preflight would run
// none of them and still answer "ok" — a pass over nothing, handed to a
// caller that is about to delete rows on the strength of it.
func checkCHRebuildPreflightFlags(preflight, write bool) error {
	if preflight && !write {
		return fmt.Errorf("-preflight checks the guards of a -write run and a dry run has none; pass it together with -write")
	}
	return nil
}

// chRebuildDirtyRecordedPrefix opens the single stdout line a successful
// `ch-rebuild -write -record-dirty-window` prints. Like
// chRebuildPreflightPrefix it is a CONTRACT with
// scripts/ops/ch-rebuild-projected.sh, which logs the line verbatim so the
// operator can see WHICH sources the completeness verifier now owes a
// re-reconcile for.
const chRebuildDirtyRecordedPrefix = "ch-rebuild: recorded projection dirty window"

// checkCHRebuildRecordDirtyFlags keeps the three steps around a clean-slate
// window separable. -preflight is the ASK that precedes the DELETE, -write
// is the re-derive, and -record-dirty-window is the RECORD that outlives a
// re-derive which did not happen — an invocation combining them would
// either answer "ok, delete" while declaring the range already emptied, or
// file an obligation for a re-derive that is about to run in the same
// process (the routine case #408 measured and refused).
func checkCHRebuildRecordDirtyFlags(recordDirty, preflight, write bool) error {
	if recordDirty && write {
		return fmt.Errorf("-record-dirty-window records that a range was emptied and NOT re-derived; it is not a mode of -write. Run the re-derive and, if it fails, the record, as separate invocations")
	}
	if recordDirty && preflight {
		return fmt.Errorf("-record-dirty-window and -preflight are different steps (ask before the DELETE vs. record an emptied window); pass one")
	}
	return nil
}

// chRebuildRecordDirtySources resolves the sources a
// `-record-dirty-window` invocation files an obligation for: the ones it
// NAMED, each of which must carry a reconciliation-catalogue entry.
//
// Named rather than re-derivable, deliberately. The caller is telling this
// process which sources' rows it already DELETED, and whether the current
// config could re-derive them is a different question — a source whose
// gate has since closed is the one whose hole will persist longest. But a
// name outside the catalogue is refused: compute-completeness looks a
// window up by the source it is verifying, so a row under a name no
// verdict ever reads is a silent no-op dressed as an obligation.
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

// chRebuildSourceUniverse returns every source name this binary knows as
// a ch-rebuild source, in catalogue order: the reconciliation catalogue
// (which carries the event, ContractCall and sdex entries) plus the two
// SEP-41 sources, which buildReconciliationCatalogue only promotes when a
// watched set is configured but which -sources may legitimately name
// either way.
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

// checkCHRebuildSources refuses a -sources value naming something this
// binary does not know as a ch-rebuild source (K015).
//
// srcFilter is a bare split of the flag and enabled() is a membership test
// against it, so a name nobody recognises — `-sources sdx` for `sdex` —
// makes enabled() false for EVERY real source: the run streams the range,
// decodes nothing, prints its DRY-RUN banner and its count report, and
// exits 0 having re-derived nothing. That is the DO-NOTHING half of the
// trap the write gate's own doc names, reported as success, and it is the
// mode operators run FIRST: -write already refuses an unregistered name
// through checkCHRebuildBackfillSafe, the default dry run did not.
//
// Only UNKNOWN names are refused, never known-but-inert ones. A named
// source whose pass this invocation did not request (-sdex / -sep41 /
// -contract-calls), or whose decoder this config does not build, still
// narrows legitimately — and scripts/ops/ch-rebuild-projected.sh DEPENDS
// on that narrowing: it asks -preflight for its whole source set and
// deletes only the subset the verdict names back.
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

// recordCHRebuildDirtyWindows records [lo,hi] as a pending projection dirty
// window for every source in sources, so the next compute-completeness
// re-reconciles the range instead of carrying its prior clean claim over
// it (F075).
//
// One row PER SOURCE, under the catalogue names the reconcile keys on: the
// table is keyed by source and compute-completeness looks a window up by
// the source it is verifying, so a single record — or one under a name no
// catalogue entry carries — silently no-ops. An empty set is therefore an
// error, not a quiet success: the caller asked to record an obligation and
// none was written.
//
// The obligation is discharged ONLY by a clean completeness verdict whose
// scope covered the window (compute-completeness clears it in the same
// transaction that stores the verdict — finding F072). Nothing in the
// rebuild path clears it, because a re-derive is the CAUSE of the
// dirtiness, never evidence against it.
func recordCHRebuildDirtyWindows(ctx context.Context, store projectionDirtyWindowRecorder, w io.Writer, lo, hi uint32, sources []string) error {
	if len(sources) == 0 {
		return fmt.Errorf("ch-rebuild: -record-dirty-window [%d,%d]: this invocation would re-derive no source, so no obligation was recorded — name the sources whose rows were deleted in -sources", lo, hi)
	}
	for _, name := range sources {
		if err := store.RecordProjectionDirtyWindow(ctx, timescale.ProjectionDirtyWindow{
			Source: name,
			From:   lo,
			To:     hi,
			Reason: timescale.CHRebuildEmptiedReason(lo, hi),
		}); err != nil {
			return fmt.Errorf("ch-rebuild: -record-dirty-window (%s): %w", name, err)
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

// chRebuild is the ADR-0034 Phase-4 write path: it re-derives a ledger range's
// protocol output from the ClickHouse Tier-1 lake using the EXISTING decoders
// and WRITES it to the Postgres served tier via the production sink
// (pipeline.HandleEvent — idempotent ON CONFLICT). It is the write-enabled
// sibling of ch-reproject (which only counts + compares).
//
// Three passes, mirroring the dataflow split:
//   - Event-based sources (soroswap / aquarius / phoenix / comet / blend /
//     cctp / rozo / defindex / reflector / redstone): one StreamContractEvents
//     pass, every Matches-gated decoder per event. This is where the
//     event_index-collision recovery lands (CH > served: aquarius +61%,
//     defindex/cctp/blend_emissions 0→N).
//   - SDEX (op-based, NOT in contract_events): a StreamSDEXOps pass feeding the
//     SDEX OpDecoder. Gated behind -sdex because it decodes ~15.5 B trade ops
//     across all history and the loss it recovers (passive-offer + one-side-zero
//     fills) is ~0.004 % and pricing-immaterial (the aggregator skips zero legs;
//     served pricing is CEX+SDEX-dominated). The fixed live indexer captures
//     these forward; a full historical SDEX rebuild is opt-in.
//   - Event-less ContractCall sources (band / soroswap-router): a
//     StreamContractCallOps pass (body_xdr contract-byte filter) feeding each
//     source's ContractCallDecoder. Gated behind -contract-calls. These emit no
//     Soroban events, so the projector can't rebuild them — this pass is the
//     lake-replay successor to the superseded backfill-router MinIO walk
//     (still registered as `stellarindex-ops backfill-router`; this pass is
//     the preferred lake-path replacement, not a drop-in removal).
//   - SEP-41 watched-contract sources (sep41_transfers / sep41_supply): a
//     dedicated StreamContractEventsFiltered pass gated behind -sep41. They
//     CANNOT ride the main event pass — their topics ARE the CAP-67 firehose
//     it excludes — so this pass prefilters on contract_id IN (the watched
//     set), turning the 447M-row firehose scan into an indexed one. See the
//     -sep41 flag help for the operator truncate+re-derive contract. For a
//     SCOPED dropped-rows recovery (a decoder bug that lost a few rows from
//     otherwise-clean data), -contracts <csv> narrows the contract_id prefilter
//     to just the affected contracts and -sep41-supply-only narrows the read to
//     the supply topics (mint/burn/clawback), so the additive ON CONFLICT write
//     recovers the missing rows without a full re-derive or a truncate
//     (docs/operations/sep41-mint-recovery.md).
//
// Defaults to DRY-RUN (count only). Pass -write to persist. For a clean-slate
// rebuild (ADR-0034 "rebuild, not repair") the operator truncates the target
// tables first; the writes are idempotent either way (recover-into-existing or
// repopulate-after-truncate). Window [from,to] per partition for the full run
// so the streamed result set + the successful-tx IN-set stay bounded.
func chRebuild(args []string) error { //nolint:gocognit,gocyclo,funlen // linear: seed, event pass, optional op pass, report; splitting hurts clarity.
	fs := flag.NewFlagSet("ch-rebuild", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	from := fs.Uint("from", 0, "first ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "last ledger sequence (inclusive, required)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	only := fs.String("sources", "", "comma-separated source names to rebuild (default: all event-based)")
	includeSDEX := fs.Bool("sdex", false, "also re-derive SDEX trades from operations (expensive: ~15.5B op decodes all-history)")
	sdexGaps := fs.Bool("sdex-gaps", false, "with -sdex: re-derive ONLY the served gaps in [from,to] in one pass (each gap is an empty range → pure insert, no ON CONFLICT walk) — efficient drop-backlog recovery vs re-scanning the whole range")
	sdexReconcile := fs.Bool("sdex-reconcile", false, "with -sdex: re-derive ONLY ledgers where the distinct Validate-passing census exceeds the served count (PARTIAL-drop ledgers the empty-gap pass misses); recovers the served-tier projection to exact parity with the lake")
	contractCalls := fs.Bool("contract-calls", false, "also re-derive the event-less ContractCall sources (band, soroswap-router) from the lake's InvokeContract ops — filtered on the contract's bytes in body_xdr (no contract_id column) — and run their ContractCallDecoders. These have NO soroban_events landing zone, so neither the event pass nor the projector can rebuild them; this is the ADR-0034 lake-replay successor to the superseded backfill-router MinIO walk (still registered; see 'stellarindex-ops backfill-router -h'). Respects -sources.")
	includeSEP41 := fs.Bool("sep41", false, "also re-derive the SEP-41 watched-contract sources (sep41_transfers, sep41_supply) from the lake via a contract_id-prefiltered event pass (their topics are the CAP-67 firehose the main pass excludes). FULL re-derive contract: for a whole-history rebuild run this as part of the truncate+re-derive procedure — TRUNCATE sep41_transfers + sep41_supply_events FIRST (historical rows predate the migration-0057 event_index PK, so multiple same-op events sit COLLAPSED on disk; the idempotent ON CONFLICT writes cannot un-collapse them — recover-into-existing is accepted only if you accept that residue). ROLLUP: when the SUPPLY source is re-derived, -write AUTO-RESETS the sep41_supply_rollup fold checkpoint after the events land (a FULL re-derive resets every watched contract's fold columns) so the aggregator worker re-folds from zero instead of double-counting the re-derived history (the KALE 2× served-value bug, incident 2026-07-06); the seeded migration-0088 genesis baseline is PRESERVED, so no manual TRUNCATE sep41_supply_rollup + re-seed is needed. After a full-history -write re-derive the two sources become eligible for the ADR-0033 projection reconcile — DONE (2026-07-11, windows 50.0M-63.42M, rc=0): buildReconciliationCatalogue now promotes them into the default catalogue unconditionally whenever [supply] watched_sep41_contracts is configured (no further code change needed), so verify-reconciliation/ch-reproject/compute-completeness all see them. Because that promotion is config-gated, not re-derive-state-gated, a FUTURE full truncate+re-derive will show the two sources as reconcile-red for the DURATION of the rebuild (truncated table vs. lake expectation) exactly like any other source mid-rebuild — expected, not a regression. For a SCOPED dropped-rows recovery (a decoder bug that lost a handful of rows from post-0057-clean data), use -contracts to narrow to the affected contracts instead — no truncate needed, the additive ON CONFLICT write only ADDS the missing rows (docs/operations/sep41-mint-recovery.md). Requires [supply] watched_sep41_contracts. Respects -sources.")
	contractsCSV := fs.String("contracts", "", "comma-separated contract C-strkeys to SCOPE the read to (default: no scope). For -sep41 this REPLACES [supply] watched_sep41_contracts as the contract_id READ prefilter, so a scoped recovery does an indexed scan of ONLY these contracts' events (far cheaper than all watched contracts) and idempotently ADDS their missing rows — the leanest way to recover dropped rows without a full re-derive. With -sep41 -write on the SUPPLY source, ONLY these contracts' sep41_supply_rollup fold rows are reset afterwards (genesis baseline preserved), so the worker re-folds their recovered below-checkpoint rows — a scoped recovery is safe by default, no manual rollup surgery. Must be a SUBSET of the watched set: the sep41 decoders still gate Matches() on the full watched set, so a contract outside it is read but decoded to nothing (a warning is printed). For the general event pass it is an extra decode-time contract gate. See docs/operations/sep41-mint-recovery.md.")
	sep41SupplyOnly := fs.Bool("sep41-supply-only", false, "with -sep41 -sources sep41_supply: narrow the CH read to the supply-affecting topics (mint/burn/clawback) via the topic_0_sym prefilter, skipping the transfer firehose at the SQL layer — so recovering a high-transfer-volume contract's few mints does not re-read millions of transfer events. Invalid unless sep41_transfers is disabled (via -sources sep41_supply): the topic prefilter would otherwise silently drop transfer recovery.")
	write := fs.Bool("write", false, "actually write to Postgres (default: dry-run, count only)")
	bulkTrades := fs.Bool("bulk-trades", false, "with -write: land trade rows through the BULK backfill writer (timescale.Store.BulkBackfillTrades) instead of the per-batch upsert. Opt-in and BACKFILL-ONLY. It proves - per source, scoped by ledger AND ts - that the target range holds no stored rows, then resolves usd_volume for the whole buffer through a worker pool and streams the rows in over parallel binary COPY connections. Rows are identical to the upsert path's (same storability gate, same intra-batch PK dedupe, same tradeUSDVolume waterfall, same derive_generation, same source_entry_counts / registry / sentinel side effects); the difference is that a latency-bound workload stops being serial. If the range is NOT empty - or a COPY hits a unique violation because something wrote underneath it - the buffer is handed to the ordinary generation-guarded upsert instead and the run says so. Worth it for a historical re-derive below the source's floor; pointless (and it will just fall back) for a recovery into populated ledgers.")
	allowLiveOverlap := fs.Bool("allow-live-overlap", false, "DANGEROUS: bypass the live-cursor guard and -write a range the live projector's cursor for a PROJECTED source is still inside. Only pass this if you have independently verified the live projector will not process this range concurrently — see the ADR-0048 D3 one-writer contract on checkCHRebuildLiveOverlap.")
	preflight := fs.Bool("preflight", false, "with -write: run the refusals this exact -write invocation would hit BEFORE it reads the lake — the BackfillSafe gate (both legs), the live-cursor one-writer guard, and the buffered-range ceiling — then print one line naming the sources it would re-derive (`"+chRebuildPreflightPrefix+" [from,to] rederive=a,b,c`) on stdout and exit 0 WITHOUT reading ClickHouse or writing a row. A refusal exits non-zero exactly as the real run would. It exists for a caller that must do something destructive before the re-derive (scripts/ops/ch-rebuild-projected.sh DELETEs the window first): ask here, and delete only what this prints. Runtime failures (a lake stream error, a failed write) are by nature not covered.")
	recordDirty := fs.Bool("record-dirty-window", false, "record [from,to] as a pending ADR-0033 projection dirty window for every source named in -sources (each must be a reconciliation-catalogue source; the list is REQUIRED), then exit 0 WITHOUT reading ClickHouse or writing a row. The next compute-completeness re-reconciles that range instead of carrying its prior clean projection claim over it, and clears the obligation only with the verdict that discharges it. This is the RECORD, not a re-derive, so it takes neither -write nor -preflight. It exists for the operator (and scripts/ops/ch-rebuild-projected.sh's TELL THE VERDICT line) after a clean-slate DELETE whose re-derive did not complete: the window is then EMPTY and /v1/coverage must stop certifying it complete (F075). An ordinary -write run deliberately records nothing — see the -write warning and #408.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *from == 0 || *to == 0 || *to < *from {
		return fmt.Errorf("-config, -from, -to are required; -to must be >= -from")
	}
	if err := checkCHRebuildPreflightFlags(*preflight, *write); err != nil {
		return err
	}
	if err := checkCHRebuildRecordDirtyFlags(*recordDirty, *preflight, *write); err != nil {
		return err
	}
	// -contracts scopes both passes to a contract subset; the sep41 pass pushes
	// it into the CH read as the contract_id prefilter (narrows the scan), the
	// general event pass applies it as a decode-time gate.
	contractsOverride := parseCSVList(*contractsCSV)
	if *sep41SupplyOnly && !*includeSEP41 {
		return fmt.Errorf("-sep41-supply-only requires -sep41")
	}
	// BackfillSafe gate, first leg (F050): sources the operator NAMED.
	// Asked before the config load so the refusal needs no reachable
	// database; the default-all case is asked again once the catalogue
	// exists — see checkCHRebuildBackfillSafe.
	if *write {
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
	// Re-derive path (INV-3 / migration 0109): stamp a positive
	// derive_generation so this rebuild's corrected values UPDATE the
	// stored rows in place — via both BatchInsertTrades/InsertTrade below
	// and pipeline.HandleEvent (which draws the generation from this same
	// store) — and win over the live gen-0 values.
	store.SetDeriveGeneration(time.Now().Unix())

	// usd_volume resolution — MANDATORY on this path, not optional.
	// The generation stamped above means every trade this rebuild writes
	// WINS the upsert, and InsertTrade/BatchInsertTrades assign
	// `usd_volume = EXCLUDED.usd_volume` unconditionally. Without the
	// resolvers installed, tradeUSDVolume returns nil for every DEX trade
	// and every FX-priced CEX trade, so the rebuild would overwrite
	// correct stored values with NULL across its whole ledger range.
	// This wiring was absent entirely until 2026-07-22.
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
	// -record-dirty-window returns HERE: as soon as there is a catalogue to
	// check the named sources against, and before the gate warm-up, the
	// factory preseed and any lake read.
	//
	// It writes no served row — it files an obligation ON the completeness
	// verifier — so nothing downstream may be able to refuse it. That is
	// the point in the case it exists for: a window
	// ch-rebuild-projected.sh emptied for a source whose decoder gate has
	// since closed, or whose preseed now errors, is exactly the hole
	// /v1/coverage must stop certifying clean (F075), and a later
	// placement would let a refusal swallow the record.
	if *recordDirty {
		named, rerr := chRebuildRecordDirtySources(cat, parseCSVList(*only))
		if rerr != nil {
			return rerr
		}
		return recordCHRebuildDirtyWindows(ctx, store, os.Stdout, lo, hi, named)
	}
	// A -sources name nobody recognises selects nothing and exits 0 —
	// refuse it here, as early as the catalogue exists and before the
	// gate warm-up reads Postgres (K015).
	if serr := checkCHRebuildSources(cat, parseCSVList(*only)); serr != nil {
		return serr
	}
	// Re-derive on the gate the live indexer runs with — curated set ∪
	// protocol_contracts — not on the bare in-code seed (RLT-430): a
	// contract an operator admitted through protocol_contracts was decoded
	// live, and `-write` after a truncate rebuilt its table without it.
	// Read-only (no upsert hook). Must precede the preseed below, which
	// seeds into the decoders this rebuilds.
	if cat, cerr = warmCatalogueGates(ctx, store, logger, cat); cerr != nil {
		return fmt.Errorf("ch-rebuild: %w", cerr)
	}

	// Factory-anchored sources (ADR-0035): seed each gate registry from
	// the factory's creation events in [genesis, lo) BEFORE the
	// re-derive, exactly as verify-reconciliation and
	// compute-completeness already do. Without it a source whose
	// decoder carries no in-code curated set — blend is the only one —
	// re-derives 0 rows for any window above its factory deploys, which
	// reads as a bogus delta here and as a silently-empty arm in
	// ch-rebuild -write (cold audit 2026-08-03). Read-only, idempotent,
	// and a no-op for the 20+ non-factory sources.
	//
	// Caveat carried from the sibling call sites: preseedFactoryChildren
	// walks the Postgres soroban_events landing zone, which is
	// decommission-pending (#803); a CH-native preseed is the durable fix
	// for all four callers.
	for _, src := range cat {
		if len(src.factories) == 0 {
			continue
		}
		pblind, perr := preseedFactoryChildren(ctx, store, src, lo)
		if perr != nil {
			return fmt.Errorf("%s: preseed factory children: %w", src.name, perr)
		}
		// A writer must not rebuild over a registry missing a child whose
		// creation event its decoder could not evaluate.
		if pblind.Any() {
			return fmt.Errorf("%s: preseed factory children: %s", src.name, pblind.Detail())
		}
	}
	// ch-rebuild manages sep41_transfers/sep41_supply itself via the
	// dedicated -sep41 pass below (its own contract-prefiltered CH read,
	// its own decoder instances, its own written-count bookkeeping) rather
	// than through buildReconciliationCatalogue's generic per-source
	// re-derive. buildReconciliationCatalogue ALSO promotes these two
	// (2026-07-11) whenever the watched set is configured — which -sep41
	// requires — so drop them here unconditionally to restore the
	// pre-promotion invariant this whole function is written against: cat
	// carries no sep41 entries until the -sep41 pass folds its OWN
	// sep41Cat in below (after the read, so the main event pass's
	// hasEventSource / -sources filtering above never spuriously trips on
	// a source whose topics that pass's CH stream excludes anyway).
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
	// Seed soroswap pairs from the PG registry (NOT the RPC factory seed —
	// per-window invocations would each pay a ~200s RPC round-trip + depend on
	// an external endpoint). The live indexer persists every new_pair to
	// soroswap_pairs, so the registry covers all historical pairs; pairs created
	// within a window are also self-discovered from in-range new_pair events.
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
	if *write {
		// BackfillSafe gate, second leg (F050): everything this run would
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
	if *write {
		mode = "WRITE"
		// The ADR-0033 completeness verdict does NOT learn about this
		// rewrite. projector-replay records a projection dirty window so
		// the next compute-completeness re-reconciles the rewound range;
		// ch-rebuild records nothing, so the nightly verdict CARRIES ITS
		// PRIOR CLEAN CLAIM over the range this run just changed
		// (wave-D CV-1).
		//
		// Bounded in practice, which is why this is a warning and not a
		// refusal: the reconcile is COUNT-based and a correct ch-rebuild
		// converges served→lake using the same decoders the reconcile's
		// expected side uses, so a wrong verdict needs a SECOND
		// independent defect (a partial write, pre-0057 key residue, a
		// decoder blind spot) stacked on this operator action. The one
		// documented production -write — the 2026-08-04 usd_volume
		// restamp — was value-only and count-neutral.
		//
		// Recording the window automatically is the right fix and is NOT
		// done here on purpose. ch-rebuild is multi-source, so it needs
		// one window PER ENABLED CATALOGUE SOURCE (a single record, or
		// one under a non-catalogue name, silently no-ops), and a
		// measured hazard blocks the naive port: ONE source's dirty
		// window (aquarius [51M,tip]) already blew the -pass 120-minute
		// deadline and needed a bespoke identity-gated prefilter, while
		// compute-completeness runs under TimeoutStartSec=180min.
		// Recording windows for the 8 sources ch-rebuild-projected.sh
		// drives over [50M,62.894M] would force the next nightly to
		// re-reconcile ~12.9M ledgers across 8 un-prefiltered sources —
		// a likely timeout that takes out EVERY source's verdict, which
		// is worse than the stale claim it fixes. It needs a bounded
		// per-window re-reconcile and a re-measured pass wall-clock
		// first.
		//
		// ONE case is carved out of that trade-off and IS recorded, via
		// the separate -record-dirty-window mode: a window
		// ch-rebuild-projected.sh DELETEd whose re-derive did not
		// complete (F075). There the served tier is not "rewritten and
		// probably fine", it is EMPTY, so a carried clean claim is
		// certainly false rather than second-order — and the cost lands
		// only in an incident the operator is already handling, for the
		// deleted sources alone, instead of on every routine window.
		logger.Warn("ch-rebuild -write does NOT record a projection dirty window — " +
			"the next completeness verdict will carry its prior clean claim over this range " +
			"(a window left EMPTIED by a failed clean-slate re-derive is the exception: " +
			"ch-rebuild-projected.sh records that one with -record-dirty-window). " +
			"Note the [from,to] window and source set on the change record, and re-check " +
			"the affected sources' reconcile before trusting the next /v1/coverage verdict.")
	}
	// Buffer-pass range guard (2026-07-05): every decode pass
	// buffers a whole invocation's decoded events in this process. A
	// 12.9M-ledger -sep41 run ballooned until the kernel killed it
	// silently — and the memory pressure swapped galexie's captive
	// core into an invalid-local-state wedge (11h lake stall). The
	// tool's docs always said "window your invocation"; docs aren't
	// guards. 2M ledgers ≈ a comfortable single-window ceiling.
	const maxBufferedRange = 2_000_000
	if chRebuildBuffers(cat, sep41Cat, passes, enabled) && *to-*from > maxBufferedRange {
		return fmt.Errorf("ch-rebuild: range [%d,%d] spans %d ledgers — every decode pass (event, sep41, sdex, contract-call) buffers in-process; window invocations to <=%d ledgers (loop externally, resume per window)",
			*from, *to, *to-*from, maxBufferedRange)
	}
	// -preflight stops HERE: past the last refusal, before the first lake
	// read. Everything above is read-only against Postgres, so a caller
	// that deletes on the strength of this answer has deleted nothing the
	// run below would then refuse to rewrite (RLT-381).
	if *preflight {
		return reportCHRebuildPreflight(os.Stdout, lo, hi, reDerivedSourcesInRun(cat, sep41Cat, passes, enabled))
	}

	fmt.Fprintf(os.Stderr, "ch-rebuild: [%d,%d] mode=%s sources=%q sdex=%v contract-calls=%v sep41=%v ch=%s\n",
		lo, hi, mode, *only, *includeSDEX, *contractCalls, *includeSEP41, *chAddr)

	// Buffer decoded events during the CH stream, then write to Postgres AFTER
	// the stream closes. Holding the CH FINAL stream open across slow per-row PG
	// writes trips the client read timeout mid-stream; decoupling read from
	// write keeps the CH connection short-lived. Windows are partition-aligned
	// (1M) so a window's decoded set stays bounded in memory.
	var buf []consumer.Event

	// ─── Event-based pass (read → buffer) ────────────────────────────────
	// Skip entirely unless an enabled source actually has an event decoder:
	// StreamContractEvents scans the whole firehose-excluded contract_events
	// range regardless of how many decoders fire, so running it for a
	// ContractCall-only invocation (e.g. -sources soroswap-router) is a pure
	// waste — a multi-million-ledger CH scan whose every row is skipped.
	hasEventSource := false
	for _, src := range cat {
		if src.dec != nil && enabled(src.name) {
			hasEventSource = true
			break
		}
	}
	if hasEventSource {
		evStart := time.Now()
		// Exclude the CAP-67 classic-token firehose — none of the projected DEX/
		// lending sources consume it, and it's 99.99% of contract_events. Use
		// FirehoseExcludeSyms (NOT ClassicTokenTopic0Syms): set_admin must be
		// RETAINED because Blend/Comet emit a pool set_admin sharing that topic —
		// excluding it wholesale dropped blend_admin's set_admin rows from the
		// re-derive (matches the projector's firehoseExcludeSyms).
		cherr := clickhouse.StreamContractEvents(ctx, *chAddr, lo, hi, clickhouse.FirehoseExcludeSyms, func(ev events.Event) error {
			if !contractAllowed(contractsOverride, ev.ContractID) {
				return nil // -contracts scope: skip events outside the subset
			}
			for _, src := range cat {
				if src.dec == nil || !enabled(src.name) {
					continue
				}
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
				buf = append(buf, outs...)
			}
			return nil
		})
		if cherr != nil {
			return fmt.Errorf("ch-rebuild: event stream: %w", cherr)
		}
		fmt.Fprintf(os.Stderr, "ch-rebuild: event read done in %s (%d events buffered)\n",
			time.Since(evStart).Round(time.Second), len(buf))
	} else {
		fmt.Fprintln(os.Stderr, "ch-rebuild: event pass skipped (no enabled event-decoder source)")
	}

	// ─── SEP-41 pass (opt-in; read → buffer) ─────────────────────────────
	// Cannot ride the main event pass: the sep41 topics ARE the CAP-67
	// classic-token firehose it excludes via FirehoseExcludeSyms. Stream a
	// contract_id-prefiltered read instead (the watched set is the ADR-0033
	// contractIDs prefilter), which is an indexed scan of only the watched
	// contracts' events. FINAL because the dry-run COUNTS: un-merged
	// duplicate ReplacingMergeTree parts would inflate the report (the
	// -write path alone wouldn't care — ON CONFLICT absorbs duplicates).
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
				gaps, gerr := store.FindPerSourceLedgerGaps(ctx, tgt, int64(lo), int64(hi), 1)
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
			// Re-derive ONLY the PARTIAL-drop ledgers: those where the distinct,
			// Validate-passing census exceeds the served count. The empty-gap
			// pass (-sdex-gaps) misses these because served>0. Per 100k window:
			// decode + buffer valid trade events by ledger (+ track the distinct
			// served-PK census), compare to the per-ledger served count, and
			// queue every event for any ledger that's short. Writing the full
			// set is fine — ON CONFLICT no-ops the rows already present and
			// inserts only the dropped ones.
			type pk struct {
				tx string
				op uint32
			}
			// 25k (was 100k): the sdex reconcile joins OOM'd at 100k even
			// under grace_hash (2026-07-05 heal run) — and the wedged-CH
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
						if !ok || te.Trade.Validate() != nil {
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
				served, serr := store.CountRowsByLedger(ctx, "trades", "ledger", "source='sdex'", wlo, whi)
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
	// Event-less ContractCall sources (band, soroswap-router) emit no Soroban
	// events, so neither the event pass above nor the ADR-0032 projector can
	// rebuild them — there's no landing-zone signal. Re-derive from the lake's
	// InvokeContract ops (filtered on the contract's bytes in body_xdr) and run
	// each source's ContractCallDecoder, byte-identical to the live dispatcher's
	// routing AND to the projection census (forEachContractCallEvent is shared
	// with reDeriveContractCallCensus), so the written rows reconcile to the
	// census Δ=0. This is the ADR-0034 lake-replay replacement for the
	// superseded backfill-router MinIO walk (still registered as
	// `stellarindex-ops backfill-router`; it under-produced: it pre-dated the
	// auth-tree-roots extraction, so it missed router calls nested inside
	// aggregator contracts).
	if *contractCalls {
		ccStart := time.Now()
		for _, src := range cat {
			if src.callDec == nil || !enabled(src.name) {
				continue
			}
			before := len(buf)
			// The blind spots are the WRITER's side of the C4-059
			// symmetry: a call this stream cannot decode is a row the
			// rebuild does not write, exactly as the census cannot expect
			// it. Reported so a rebuild states what it dropped rather than
			// leaving the census to discover it later.
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
	// drainAndWrite batches the trade/sep41 streams (per-row fallback on batch
	// failure) and counts an event in written[] ONLY after its insert is
	// confirmed; any row whose insert fails is tallied in failed[] instead
	// (RA-1), so the completion report and exit code below cannot claim a
	// partially-failed recovery as complete.
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
	written, failed := drainAndWrite(ctx, logger, w, buf, *write)

	if *write {
		// ─── reset the SEP-41 supply rollup fold checkpoint ──────────────
		// A -sep41 -write run rewrites sep41_supply_events history BELOW the
		// aggregator's incremental sep41_supply_rollup checkpoint. The rollup
		// worker only folds `ledger > last_ledger`, so without a reset it either
		// DOUBLE-counts a full re-derive (served supply 2×, the KALE bug) or
		// never folds a scoped recovery's below-checkpoint rows (served
		// undercount). Reset the fold columns HERE — after the events are fully
		// written, so the worker re-folds from zero over the complete corrected
		// set (resetting before the drain would let a concurrent worker advance
		// the checkpoint mid-write and miss below-checkpoint rows). The reset
		// PRESERVES the seeded genesis baseline (migration 0088). Gated on the
		// supply source actually being re-derived: a transfers-only run
		// (-sources sep41_transfers) leaves sep41_supply_events untouched, so
		// there is nothing to re-fold.
		if reset, resetContracts := sep41RollupResetPlan(*includeSEP41, *write, enabled(sep41supply.SourceName), contractsOverride); reset {
			n, rerr := store.ResetSEP41SupplyRollupFold(ctx, resetContracts)
			if rerr != nil {
				return fmt.Errorf("ch-rebuild: sep41 rollup reset: %w", rerr)
			}
			scope := "FULL — all watched contracts"
			if len(resetContracts) > 0 {
				scope = fmt.Sprintf("SCOPED — %d contract(s)", len(resetContracts))
			}
			fmt.Fprintf(os.Stderr, "ch-rebuild: reset %d sep41_supply_rollup fold row(s) [%s]; the aggregator worker will re-fold from zero (genesis baseline preserved)\n", n, scope)
		}
	}

	// ─── report ──────────────────────────────────────────────────────────
	fmt.Printf("\n=== ch-rebuild [%d,%d] %s ===\n", lo, hi, mode)
	fmt.Printf("%-16s %14s %14s\n", "source", "written", "failed")
	var total, totalFailed int
	for _, src := range cat {
		n, okW := written[src.name]
		f, okF := failed[src.name]
		if !okW && !okF {
			continue
		}
		fmt.Printf("%-16s %14d %14d\n", src.name, n, f)
		total += n
		totalFailed += f
	}
	fmt.Printf("%-16s %14d %14d\n", "TOTAL", total, totalFailed)
	if !*write {
		fmt.Printf("\n(dry-run — re-run with -write to persist to Postgres)\n")
	}
	// RA-1: a partially-failed write must NOT present as complete (exit 0). If
	// any batch/per-row/HandleEvent insert failed, the corresponding rows were
	// excluded from written[] above and counted in failed[]; return a non-nil
	// error so the process exits non-zero and the operator re-runs to recover.
	if totalFailed > 0 {
		return fmt.Errorf("ch-rebuild: %d event(s) failed to write (rows missing) — see the 'failed' column and re-run to recover", totalFailed)
	}
	return nil
}

// eventWriter abstracts the Postgres write operations drainAndWrite performs.
// chRebuild wires it to the concrete *timescale.Store + pipeline.HandleEvent;
// tests inject fakes that fail selected inserts to exercise the RA-1 counting.
type eventWriter struct {
	batchTrades func(context.Context, []canonical.Trade) error
	insertTrade func(context.Context, canonical.Trade) error
	// bulkTrades, when non-nil (ch-rebuild -bulk-trades), replaces the
	// per-batch upsert as the PRIMARY trade writer and raises the trade batch
	// size to bulkTradeBatchN. It is the backfill-only COPY writer; see
	// timescale.Store.BulkBackfillTrades. The per-row insertTrade fallback
	// below is unchanged and still catches whatever it cannot land.
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

// writeTradeBatch lands one trade batch through whichever primary writer is
// wired: the bulk backfill COPY writer when ch-rebuild was given -bulk-trades,
// otherwise the ordinary generation-guarded batch upsert. Returning an error
// drops the caller into the unchanged per-row fallback.
//
// A bulk call that REFUSED its own precondition is not an error - the rows
// landed, through the upsert - but it is reported, because a run that thinks
// it took the fast path and did not is exactly the kind of silent revert that
// makes a throughput measurement lie.
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

// drainAndWrite persists the buffered events to Postgres and returns per-source
// written / failed tallies.
//
// Trade and sep41 events are batched (one multi-row INSERT per batch) with a
// per-row fallback on batch failure; everything else (protocol entities) goes
// per-row via HandleEvent. The primary CopyMerge* path and the per-row Insert*
// fallback share the identical INV-3 generation-guarded corrective-upsert
// semantics (both bind s.deriveGeneration and merge DO UPDATE ... WHERE
// derive_generation <= EXCLUDED), so a batch error dropping into the fallback
// cannot change the write outcome (TV-3).
//
// RA-1: an event is counted in written[source] ONLY after its insert is
// confirmed. A row whose batch AND per-row insert both fail — or whose
// HandleEvent returns an error — is tallied in failed[source] and never
// inflates written[]. In dry-run (write=false) every event is counted as
// "would write" and nothing is persisted.
func drainAndWrite(ctx context.Context, logger *slog.Logger, w eventWriter, buf []consumer.Event, write bool) (written, failed map[string]int) { //nolint:gocognit,gocyclo,funlen // linear: three symmetric batch/flush closures + a per-event dispatch; splitting the flush closures apart hurts clarity.
	written = map[string]int{}
	failed = map[string]int{}

	// The bulk writer wants ONE large buffer, not 1000-row slices: its
	// emptiness proof is one round trip per source per call, and its COPY
	// partitions only pay for themselves above a few thousand rows. The upsert
	// writer keeps the 1000-row batch it was tuned for (13 params/row against
	// Postgres's 65535 parameter ceiling).
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
			for _, s := range batchSrc {
				written[s]++
			}
		}
		batch = batch[:0]
		batchSrc = batchSrc[:0]
	}
	// sep41 batches (2026-07-05): the full-history re-derive buffers tens of
	// millions of sep41 events per window; per-row HandleEvent capped writes at
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
		if !write {
			written[ev.Source()]++ // dry-run: count what WOULD be written
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
			batch = append(batch, t)
			batchSrc = append(batchSrc, ev.Source())
			if len(batch) >= tradeBatchN {
				flush()
			}
			continue // counted in flush once the insert is confirmed
		}
		// Protocol-entity path: HandleEvent performs the insert. RA-1: capture
		// its error and count only on success (was `_ = pipeline.HandleEvent`,
		// which let a failed write inflate the completion report).
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

// contractAllowed reports whether an event's contract passes the -contracts
// scope gate: true when no override is set, or the contract is in the subset.
// The general event pass applies this per event; the sep41 pass instead pushes
// the same subset into the CH read as the contract_id prefilter (which narrows
// the scan, not just the decode).
func contractAllowed(override []string, contractID string) bool {
	return len(override) == 0 || containsStr(override, contractID)
}

// sep41RollupResetPlan decides whether a `ch-rebuild -sep41 -write` run must
// reset the sep41_supply_rollup fold checkpoint, and for which contracts, so
// the aggregator's rollup worker re-folds the re-derived history correctly
// instead of double-counting it (full re-derive) or never folding the recovered
// below-checkpoint rows (scoped recovery). Incident 2026-07-06.
//
// The reset applies only when the SEP-41 SUPPLY source is actually being
// re-derived — a dry-run (no -write), a non-sep41 run, or a transfers-only run
// (`-sources sep41_transfers`) leaves sep41_supply_events untouched, so there is
// nothing to re-fold. When it does apply, the returned contract set is exactly
// the CH read scope: nil for a FULL re-derive (reset every rollup row), or the
// `-contracts` override for a SCOPED recovery (reset only those rows).
func sep41RollupResetPlan(includeSEP41, write, supplyEnabled bool, contractsOverride []string) (reset bool, contracts []string) {
	if !includeSEP41 || !write || !supplyEnabled {
		return false, nil
	}
	return true, contractsOverride
}

// dropReconSources returns cat with every entry whose name is in names
// removed, preserving order. Used to keep ch-rebuild's own -sep41
// handling (a dedicated read pass + its own decoder instances) the sole
// source of sep41_transfers/sep41_supply entries in cat, even though
// buildReconciliationCatalogue may hand back a catalogue that already
// promoted them.
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

// chRebuildBuffers reports whether any pass of this invocation appends to
// the in-process buffer. Every pass does, so it keys on the same selection
// as the BackfillSafe gate rather than on event decoders alone.
func chRebuildBuffers(cat, sep41Cat []reconSource, passes chRebuildPasses, enabled func(string) bool) bool {
	return passes.sep41 || len(reDerivedSourcesInRun(cat, sep41Cat, passes, enabled)) > 0
}
