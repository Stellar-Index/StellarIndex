package chops

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	soroswap_router "github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// sorobanEraGenesis is the first pubnet ledger with Soroban — the lower
// bound for the global recognition scan.
const sorobanEraGenesis = 50_457_424

// computeCompleteness is the ADR-0033 Phase 6 computor: it derives the
// per-source completeness WATERMARK (substrate ∧ recognition ∧
// projection) and writes it to completeness_snapshots for the API +
// status page. Operator / cron-driven; compute-once / read-cheap, like
// the gap detector's source_coverage_snapshots.
//
// Per-source watermark = substrate continuity + hash chain (Claim 1) ∧
// projection reconciliation across ALL the source's tables (Claim 2b) ∧
// recognition for the source's own contracts (Claim 2a). Recognition
// gaps on a CONTRACT-PINNED source (oracles) cap that source; gaps on
// contracts no source owns go to a system-wide `recognition` snapshot
// (topic-based sources can't attribute an unhandled topic to themselves).
//
// Projection is bounded to the substrate∧recognition-verified region:
// no point re-deriving where an earlier claim already failed.
func computeCompleteness(args []string) error { //nolint:funlen,gocognit,gocyclo // linear computor; one block per claim.
	fs := flag.NewFlagSet("compute-completeness", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	toFlag := fs.Uint("to", 0, "Tip ledger (inclusive); 0 = resolve from the live ledgerstream cursor")
	only := fs.String("source", "", "Limit to one source (e.g. soroswap|blend|reflector-dex|sdex)")
	useCH := fs.Bool("ch", false, "Read all three claims from the certified ClickHouse lake (substrate + recognition + projection re-derive) instead of Postgres soroban_events — fast, off the serving DB (ADR-0033 + ADR-0034)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address (with -ch)")
	skipSubstrate := fs.Bool("skip-substrate", false, "Trust the prior substrate certification (substrate_ok=true) instead of re-scanning the hash-chain — fast per-source iteration once substrate is proven")
	skipRecognition := fs.Bool("skip-recognition", false, "Trust the prior recognition audit (recognition_ok=true) instead of re-scanning all topic shapes — the global DistinctTopicShapes scan is the load-heaviest step; skip it for gentle projection-only iteration once recognition is verified")
	fromLedger := fs.Uint("from", 0, "INCREMENTAL verify: only check [from, tip], trusting [genesis, from] as already verified (substrate + recognition + projection all scoped to [from, tip]); the watermark still extends to tip when the window is clean. 0 = full verify from each source's genesis. The completeness timer passes min(watermark) from the prior snapshots so each run re-checks only new ledgers — minutes, not hours.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return fmt.Errorf("-config is required")
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Minute)
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage open: %w", err)
	}
	defer func() { _ = store.Close() }()

	tip := uint32(*toFlag)
	if tip == 0 {
		cur, gerr := store.GetCursor(ctx, "ledgerstream", "")
		if gerr != nil {
			return fmt.Errorf("resolve tip from ledgerstream cursor: %w (pass -to to override)", gerr)
		}
		tip = cur.LastLedger
	}
	if tip == 0 {
		return fmt.Errorf("tip resolved to 0 — pass -to")
	}
	fmt.Fprintf(os.Stderr, "compute-completeness: tip=%d\n", tip)

	// sep41_transfers/sep41_supply are promoted into this catalogue by
	// buildReconciliationCatalogue itself (2026-07-11, post-full-history
	// re-derive) whenever [supply] watched_sep41_contracts is configured —
	// see its doc comment.
	catalogue, soroswapDec, err := buildReconciliationCatalogue(cfg)
	if err != nil {
		return fmt.Errorf("compute-completeness: reconciliation catalogue: %w", err)
	}
	// Fail CLOSED on an unknown -source before the per-source loop, which
	// would otherwise skip every source and report SUCCESS having verified
	// nothing (F7 fail-open).
	if verr := validateSourceFilter(*only, catalogue); verr != nil {
		return fmt.Errorf("compute-completeness: %w", verr)
	}
	if *only == "" || *only == "soroswap" {
		if serr := seedSoroswapForRecon(ctx, cfg, soroswapDec); serr != nil {
			fmt.Fprintf(os.Stderr, "compute-completeness: soroswap seed failed (%v) — soroswap projection may undercount\n", serr)
		}
	}

	// The CH lake event source for projection re-derive (ADR-0034) is built
	// PER SOURCE inside the loop below: the wide op_args_xdr column is read
	// only for sources whose decoder consumes events.Event.OpArgs (redstone).

	// Warm the factory-anchored gated registries (ADR-0035) so the
	// recognition dispatcher correctly recognizes real protocol children
	// (registered pools/vaults) and correctly flags FOREIGN emitters of
	// the same topic as gaps. Read-only (withHook=false) — the audit must
	// not mutate the registry. Depends on protocol_contracts being seeded
	// (`stellarindex-ops seed-protocol-contracts`); an empty table would
	// surface every real child shape as a false gap.
	gatedOpts, gerr := pipeline.GatedRegistryOptions(ctx, store, slog.Default(), ctx, false)
	if gerr != nil {
		return fmt.Errorf("gated registry warm: %w", gerr)
	}

	// ── Recognition (Claim 2a): one global scan, attributed per source ──
	//
	// FAIL CLOSED (C2-5 / RFC-8 detector-fail-open): a scan error must abort
	// the run. The prior code logged the error and continued with an empty
	// recGaps — indistinguishable from a clean "no gaps" scan — which the
	// per-source loop below reads as recognition_ok=true and (with substrate
	// ∧ projection clean) writes as lake_complete=true / complete=true: a
	// FALSE "complete" verdict on the public /v1/coverage. A transient CH
	// fault / query timeout / the DistinctTopicShapes memory-cap hit (the
	// load-heaviest step — see -skip-recognition) would launder into a
	// passing trust verdict. runRecognitionScan returns the error instead;
	// returning it here writes NO snapshot (the last real verdict stands) and
	// gives the cron a non-zero exit. A genuine INCOMPLETE (a scan that
	// SUCCEEDS and finds a real gap) still flows through as recGaps below,
	// keeping the existing recognition_ok=false / complete=false behavior.
	if *skipRecognition {
		fmt.Fprintln(os.Stderr, "compute-completeness: -skip-recognition — trusting prior recognition audit (no shape scan)")
	}
	recGaps, recErr := runRecognitionScan(*skipRecognition, func() ([]completeness.RecognitionGap, error) {
		if *useCH {
			// Recognition (Claim 2a) is a FULL-HISTORY property — "is every
			// topic shape a gated contract has EVER emitted recognized by some
			// decoder" — NOT an incremental one. It must NOT be scoped to the
			// incremental -from window: a low-volume source's rare wrong-topic
			// event (rozo emitted 393 payment_events over ~2 months, none in a
			// recent incremental window) would slip through and never flip
			// recognition_ok — the 2026-07-07 rozo blind spot (BACKLOG #89).
			// DistinctTopicShapes is bounded-memory at any lake size (windowed
			// per-partition narrow-column scan + batched exemplar fetch — the
			// single-query argMax-over-wide-XDR form died at any server memory
			// cap once P23/CAP-67 grew the distinct set), so always scan from
			// genesis regardless of -from — which correctly scopes only the
			// expensive row-by-row projection reconcile below.
			return computeRecognitionGapsCH(ctx, cfg, *chAddr, gatedOpts, uint32(sorobanEraGenesis), tip)
		}
		return computeRecognitionGaps(ctx, store, cfg, gatedOpts, tip)
	})
	if recErr != nil {
		return recErr
	}
	ownerOf := map[string]string{} // contract_id → source name (contract-pinned sources)
	for _, src := range catalogue {
		for _, c := range src.contractIDs {
			ownerOf[c] = src.name
		}
	}
	recBySource := map[string][]uint32{}
	var unattributed []completeness.RecognitionGap
	for _, g := range recGaps {
		if owner, ok := ownerOf[g.ContractID]; ok {
			recBySource[owner] = append(recBySource[owner], g.MinLedger)
		} else {
			unattributed = append(unattributed, g)
		}
	}

	// Substrate (Claim 1) is a property of the lake, not a source — compute the
	// earliest gap/break ONCE in -ch mode (over the whole Soroban-era range) and
	// reuse per source. The CH lake is the certified authoritative substrate.
	var chSubProblem uint32
	var chSubHas bool
	switch {
	case *useCH && *skipSubstrate:
		fmt.Fprintln(os.Stderr, "compute-completeness: -skip-substrate — trusting prior CH substrate certification (intact)")
	case *useCH:
		subFrom := uint32(2)
		if *fromLedger > 2 {
			subFrom = uint32(*fromLedger) //nolint:gosec // ledger seq fits uint32
		}
		p, has, d, serr := clickhouse.SubstrateProblem(ctx, *chAddr, subFrom, tip)
		if serr != nil {
			return fmt.Errorf("ch substrate: %w", serr)
		}
		chSubProblem, chSubHas = p, has
		if has {
			fmt.Fprintf(os.Stderr, "compute-completeness: CH substrate problem at %d (%s)\n", p, d)
		} else {
			fmt.Fprintf(os.Stderr, "compute-completeness: CH substrate intact [%d,tip] — contiguous + hash-chained\n", subFrom)
		}
	}

	// Retention boundary for trade-target sources: trades is right-sized to
	// ~90d (~1.55M ledgers), so projection for trade-protocols is verified
	// within [retentionStart, tip] (where the served tier keeps decoded rows);
	// the full-history coverage claim rests on the proven substrate. ~87d keeps
	// the window safely inside the retained range (no boundary undercount).
	var retentionStart uint32
	if *useCH && tip > 1_500_000 {
		retentionStart = tip - 1_500_000
	}

	// ── Per-source watermark ────────────────────────────────────────
	for _, src := range catalogue {
		if *only != "" && src.name != *only {
			continue
		}
		genesis := src.genesis
		var problems []uint32
		var detail []string

		// Claim 1: substrate continuity + hash chain over [genesis, tip].
		var substrateOK bool
		if *useCH {
			// Reuse the once-computed lake substrate; it's this source's
			// problem only if it falls at/after the source's genesis.
			substrateOK = !chSubHas || chSubProblem < genesis
			if !substrateOK {
				problems = append(problems, chSubProblem)
				detail = append(detail, fmt.Sprintf("substrate: lake gap/break at %d", chSubProblem))
			}
		} else {
			subGaps, err := store.FindLedgerIngestGaps(ctx, genesis, tip)
			if err != nil {
				return fmt.Errorf("%s: substrate gaps: %w", src.name, err)
			}
			breaks, err := store.VerifyLedgerHashChain(ctx, genesis, tip)
			if err != nil {
				return fmt.Errorf("%s: hash chain: %w", src.name, err)
			}
			substrateOK = len(subGaps) == 0 && len(breaks) == 0
			for _, g := range subGaps {
				problems = append(problems, uint32(g.Start))
			}
			for _, b := range breaks {
				problems = append(problems, b.LedgerSeq)
			}
			if !substrateOK {
				detail = append(detail, fmt.Sprintf("substrate: %d gap(s), %d chain break(s)", len(subGaps), len(breaks)))
			}
		}

		// Claim 2a: recognition gaps attributed to this source's contracts.
		recOK := true
		for _, l := range recBySource[src.name] {
			if l >= genesis {
				problems = append(problems, l)
				recOK = false
			}
		}
		if !recOK {
			detail = append(detail, "recognition: unhandled topic on this source's contract(s)")
		}

		// Substrate∧recognition watermark drives COVERAGE — coverage means "did
		// we capture every event" (substrate is the proof). Projection is a
		// fidelity claim on the served tier, evaluated separately so its keying
		// artifacts / retention-scoping don't corrupt the coverage signal.
		srW := completeness.ComputeWatermark(genesis, tip, problems)
		// Lake (archive) axis: substrate ∧ recognition only. `problems` at
		// this point holds no projection gaps in either branch below (the
		// CH branch never adds them here; the PG/legacy branch only
		// appends its projection gaps to `problems` AFTER this line), so
		// srW.Complete is genuinely decoupled from projection — the
		// ADR-0033/0034 two-axis verdict (decision brief
		// notes/DECISION-genesis-complete-verdict-2026-07-16.md, Option
		// B). lake_complete must NEVER be gated by the retention-scoped
		// projection reconcile.
		lakeComplete := srW.Complete
		projOK := false
		var w completeness.Watermark
		// Incremental: only reconcile [from, srW.Ledger], trusting [genesis, from]
		// as previously verified. projFrom = max(genesis, -from).
		projFrom := genesis
		if uint32(*fromLedger) > projFrom { //nolint:gosec // ledger seq fits uint32
			projFrom = uint32(*fromLedger) //nolint:gosec // ledger seq fits uint32
		}
		if *useCH {
			if srW.Ledger >= projFrom {
				streamer := clickhouse.ReconcileEventStreamer{Addr: *chAddr, NeedOpArgs: src.needsOpArgs}
				delta, pdetail, perr := reconcileProjectionAggregate(ctx, store, streamer, *chAddr, src, projFrom, srW.Ledger, retentionStart)
				if perr != nil {
					return fmt.Errorf("%s: projection: %w", src.name, perr)
				}
				projOK = delta == 0
				if !projOK {
					detail = append(detail, "projection: "+pdetail)
				}
			} else {
				detail = append(detail, "projection: not evaluated (earlier claim failed at genesis)")
			}
			// Coverage = substrate∧recognition (proven data capture). complete
			// additionally requires the served-tier projection to reconcile;
			// lakeComplete (set above, pre-projection) never does.
			w = combineWatermark(srW, projOK)
		} else {
			// Legacy Postgres path: strict per-ledger projection pins the watermark.
			if srW.Ledger >= genesis {
				pgaps, perr := reconcileSourceProjection(ctx, store, nil, src, genesis, srW.Ledger)
				if perr != nil {
					return fmt.Errorf("%s: projection: %w", src.name, perr)
				}
				projOK = len(pgaps) == 0
				problems = append(problems, pgaps...)
				if !projOK {
					detail = append(detail, fmt.Sprintf("projection: %d mismatched ledger(s) in [%d,%d]", len(pgaps), genesis, srW.Ledger))
				}
			} else {
				detail = append(detail, "projection: not evaluated (earlier claim failed at genesis)")
			}
			w = completeness.ComputeWatermark(genesis, tip, problems)
		}

		if len(detail) == 0 {
			detail = append(detail, "complete: substrate + recognition + projection verified to tip")
		}
		if err := store.UpsertCompletenessSnapshot(ctx, timescale.CompletenessSnapshot{
			Source: src.name, Genesis: genesis, Tip: tip,
			Watermark: w.Ledger, CoveragePct: w.CoveragePct, Complete: w.Complete,
			LakeComplete: lakeComplete,
			FirstProblem: w.FirstProblem,
			SubstrateOK:  substrateOK, RecognitionOK: recOK, ProjectionOK: projOK,
			Detail: strings.Join(detail, "; "),
		}); err != nil {
			return fmt.Errorf("%s: upsert snapshot: %w", src.name, err)
		}
		fmt.Fprintf(os.Stderr, "compute-completeness: %-14s watermark=%d coverage=%.4f complete=%v lake_complete=%v (%s)\n",
			src.name, w.Ledger, w.CoveragePct, w.Complete, lakeComplete, strings.Join(detail, "; "))
	}

	// ── System recognition snapshot (gaps on contracts no source owns) ──
	if *only == "" && !*skipRecognition {
		var earliest uint32
		for _, g := range unattributed {
			if earliest == 0 || g.MinLedger < earliest {
				earliest = g.MinLedger
			}
		}
		recW := completeness.ComputeWatermark(sorobanEraGenesis, tip, nilOrOne(earliest))
		detail := "no unrecognized event shapes on unowned contracts"
		if len(unattributed) > 0 {
			detail = fmt.Sprintf("%d unrecognized shape(s) on unowned contracts (earliest ledger %d) — run verify-recognition", len(unattributed), earliest)
		}
		if err := store.UpsertCompletenessSnapshot(ctx, timescale.CompletenessSnapshot{
			Source: "recognition", Genesis: sorobanEraGenesis, Tip: tip,
			Watermark: recW.Ledger, CoveragePct: recW.CoveragePct, Complete: recW.Complete,
			LakeComplete: recW.Complete, // no projection axis on this system snapshot
			FirstProblem: recW.FirstProblem, SubstrateOK: true, RecognitionOK: len(unattributed) == 0, ProjectionOK: true,
			Detail: detail,
		}); err != nil {
			return fmt.Errorf("upsert recognition snapshot: %w", err)
		}
		fmt.Fprintf(os.Stderr, "compute-completeness: recognition  unattributed=%d coverage=%.4f\n", len(unattributed), recW.CoveragePct)
	}

	return nil
}

// runRecognitionScan runs the selected recognition audit (Claim 2a) and
// returns its gaps, FAILING CLOSED on a scan error (C2-5 / RFC-8
// detector-fail-open). A scan ERROR (CH unreachable, query timeout, the
// DistinctTopicShapes memory-cap hit, a partial read) is wrapped and
// returned so compute-completeness aborts WITHOUT writing a verdict: an
// empty gap slice from a FAILED scan is indistinguishable from a clean
// scan, and proceeding would launder the failure into recognition_ok=true /
// lake_complete=true / complete=true for every source. A scan that SUCCEEDS
// and finds a real gap returns that gap (genuine INCOMPLETE is preserved);
// only errors fail closed. skip is the explicit -skip-recognition operator
// trust flag: no scan is run, and no gaps / no error are returned (trust the
// prior recognition_ok), which is distinct from a scan FAILURE.
//
// The scan is passed as a func value so the fail-closed contract is unit
// testable without a live ClickHouse / Postgres (compute_completeness_test).
func runRecognitionScan(skip bool, scan func() ([]completeness.RecognitionGap, error)) ([]completeness.RecognitionGap, error) {
	if skip {
		return nil, nil
	}
	gaps, err := scan()
	if err != nil {
		return nil, fmt.Errorf("recognition scan failed (failing closed — not writing completeness verdicts): %w", err)
	}
	return gaps, nil
}

// combineWatermark applies the served-tier projection gate to the lake
// (substrate∧recognition) watermark srW: the returned watermark's
// Complete additionally requires projOK — this is the `complete`
// (served/combined) axis. It does NOT touch srW.Complete itself, which
// callers read separately as lake_complete — the two-axis verdict from
// notes/DECISION-genesis-complete-verdict-2026-07-16.md (Option B).
// Pure and deterministic, mirroring completeness.ComputeWatermark.
func combineWatermark(srW completeness.Watermark, projOK bool) completeness.Watermark {
	w := srW
	w.Complete = srW.Complete && projOK
	return w
}

// reconcileSourceProjection reconciles every table a source writes over
// [genesis, hi] and returns the union of mismatched ledgers. SDEX uses
// the LCM census; event sources re-derive (by kind) and project each
// table's kinds.
func reconcileSourceProjection(ctx context.Context, store *timescale.Store, chStreamer completeness.EventStreamer, src reconSource, genesis, hi uint32) ([]uint32, error) {
	var mismatched []uint32
	if src.census {
		expected, eerr := store.ClassicTradeEffectCountsByLedger(ctx, genesis, hi)
		if eerr != nil {
			return nil, eerr
		}
		for _, tgt := range src.targets {
			actual, aerr := store.CountRowsByLedger(ctx, tgt.table, "ledger", tgt.whereFilter, genesis, hi)
			if aerr != nil {
				return nil, aerr
			}
			for _, g := range completeness.ReconcileCounts(expected, actual) {
				mismatched = append(mismatched, g.Ledger)
			}
		}
		return mismatched, nil
	}

	// Re-derive expected outputs: from the CH lake (certified, off the serving
	// DB) when -ch, else from Postgres soroban_events.
	var byKind map[string]map[uint32]int
	var derr error
	if chStreamer != nil {
		byKind, derr = completeness.ReDeriveOutputCountsByKindFromEvents(ctx, chStreamer, src.dec, src.contractIDs, src.topic0Syms, genesis, hi)
	} else {
		byKind, derr = completeness.ReDeriveOutputCountsByKind(ctx, store, src.dec, src.contractIDs, src.topic0Syms, genesis, hi)
	}
	if derr != nil {
		return nil, derr
	}
	for _, tgt := range src.targets {
		expected := completeness.SumKinds(byKind, tgt.kinds...)
		actual, aerr := store.CountRowsByLedger(ctx, tgt.table, "ledger", tgt.whereFilter, genesis, hi)
		if aerr != nil {
			return nil, aerr
		}
		for _, g := range completeness.ReconcileCounts(expected, actual) {
			mismatched = append(mismatched, g.Ledger)
		}
	}
	return mismatched, nil
}

// reconcileProjectionAggregate is the CH-backed projection check
// (ADR-0033 Claim 2b). Since the CS-084 fix it compares STRICT
// PER-LEDGER counts by default (via projectionDelta →
// completeness.ReconcileCounts): the totals compare it originally
// used let a real drop in ledger L net against a phantom overcount
// elsewhere in the scope and report complete=true. Sources whose
// served `ledger` keying can differ from the re-derive's event
// ledger (the oracle sources — legacy backfill vintages keyed
// oracle_updates.ledger by the ORACLE TIMESTAMP's ledger) opt out
// via reconSource.aggregateReconcile, keep the totals compare, and
// accept the documented netting residual. Returns Σ|per-ledger Δ|
// across targets (0 = clean); the name keeps its historical
// "Aggregate" for grep continuity with older run logs.
//
// Scope: a source with a `trades` target is scoped to [retentionStart, hi] —
// trades is right-sized to ~90d, so its decoded rows >retention don't exist in
// the served tier (the raw events ARE captured: substrate proves that). We
// verify the served tier is faithful within what it retains; the full-history
// coverage claim rests on substrate. Pure entity/oracle sources verify the
// whole [genesis, hi].
func reconcileProjectionAggregate(ctx context.Context, store *timescale.Store, chStreamer completeness.EventStreamer, chAddr string, src reconSource, genesis, hi, retentionStart uint32) (int, string, error) { //nolint:gocognit,gocyclo // three linear reconcile branches (callDec / census / event re-derive) over the target list + the factory preseed; clearer unsplit, the retention floor is already extracted.
	lo := genesis
	if hasTradesTarget(src) && retentionStart > genesis {
		lo = retentionStart
	}
	var totalDelta int
	var details []string

	if src.callDec != nil {
		// Event-less ContractCall source (band, soroswap-router): re-derive the
		// census from the lake's InvokeContract ops (no soroban_events landing
		// zone) and reconcile against the served tier (oracle_updates /
		// soroswap_router_swaps) by the SAME decoder the live dispatcher routes.
		// retentionFloor scopes to where served data begins (these tables are
		// full-history, so it's just the first-call ledger, not a 90d boundary).
		flo, ferr := retentionFloor(ctx, store, src, lo, hi)
		if ferr != nil {
			return 0, "", ferr
		}
		expected, eerr := reDeriveContractCallCensus(ctx, chAddr, src.callContract, src.callDec, flo, hi)
		if eerr != nil {
			return 0, "", eerr
		}
		for _, tgt := range src.targets {
			actual, aerr := store.CountRowsByLedger(ctx, tgt.table, "ledger", tgt.whereFilter, flo, hi)
			if aerr != nil {
				return 0, "", aerr
			}
			if d, detail := projectionDelta(src, tgt.table, expected, actual, flo, hi); d != 0 {
				totalDelta += d
				details = append(details, detail)
			}
		}
		return totalDelta, strings.Join(details, "; "), nil
	}

	if src.census {
		// Floor at the ACTUAL retained boundary (drop_chunks can retain less than
		// retentionStart; census>0 vs served=0 below the oldest chunk is a
		// retention artifact, not a gap — see retentionFloor).
		var ferr error
		lo, ferr = retentionFloor(ctx, store, src, lo, hi)
		if ferr != nil {
			return 0, "", ferr
		}
		// Re-derive the census by running the SDEX decoder over the certified CH
		// operations and counting its trade output — the SAME decode the indexer
		// applies to live ops. This matches served by identical logic, so the
		// only residual is ops the served tier dropped (real coverage gaps), not
		// the over-count claimAtomCount carries (it counts claims the decoder
		// later drops as malformed-asset). Independent SOURCE (the lake's full op
		// set, substrate-proven) vs the live-ingested ops — catches drops, never
		// passes by construction.
		expected, eerr := reDeriveSDEXCensusViaDecoder(ctx, chAddr, lo, hi)
		if eerr != nil {
			return 0, "", eerr
		}
		for _, tgt := range src.targets {
			actual, aerr := store.CountRowsByLedger(ctx, tgt.table, "ledger", tgt.whereFilter, lo, hi)
			if aerr != nil {
				return 0, "", aerr
			}
			if d, detail := projectionDelta(src, tgt.table, expected, actual, lo, hi); d != 0 {
				totalDelta += d
				details = append(details, detail)
			}
		}
		return totalDelta, strings.Join(details, "; "), nil
	}

	// Factory-anchored sources (ADR-0035): seed the gate registry from the
	// factory's creation events [genesis, lo) before the re-derive, so children
	// deployed before this window aren't dropped — exactly as
	// verify-reconciliation does (verify_reconciliation.go). Without this the
	// daily verdict's child gate was only the static protocol_contracts seed and
	// went STALE as new pools deployed: blend reported complete=false
	// (expected=0) on windows whose activity was on pools missing from the seed,
	// while the live decoder (which self-seeds from deploy events) captured them.
	// Adding it here makes the watchdog self-maintaining. (Reads the Postgres
	// soroban_events landing zone for the rare, indexed creation events; a
	// CH-native preseed for full -ch purity is a follow-up.)
	if len(src.factories) > 0 {
		if perr := preseedFactoryChildren(ctx, store, src, lo); perr != nil {
			return 0, "", fmt.Errorf("%s preseed: %w", src.name, perr)
		}
	}

	byKind, derr := completeness.ReDeriveOutputCountsByKindFromEvents(ctx, chStreamer, src.dec, src.contractIDs, src.topic0Syms, lo, hi)
	if derr != nil {
		return 0, "", derr
	}
	for _, tgt := range src.targets {
		expected := completeness.SumKinds(byKind, tgt.kinds...)
		actual, aerr := store.CountRowsByLedger(ctx, tgt.table, "ledger", tgt.whereFilter, lo, hi)
		if aerr != nil {
			return 0, "", aerr
		}
		if d, detail := projectionDelta(src, tgt.table, expected, actual, lo, hi); d != 0 {
			totalDelta += d
			details = append(details, detail)
		}
	}
	return totalDelta, strings.Join(details, "; "), nil
}

// retentionFloor raises lo to the actual oldest retained ledger of the source's
// served table(s). trades is drop_chunks-managed (~90d) and can retain LESS
// than retentionStart: tip-1.5M is ~100d at the current ledger rate, ~10d / 150k
// ledgers below the oldest retained chunk (min served ≈ 2026-03-12). Counting
// census>0 vs served=0 for those retention-dropped ledgers is a false gap, so we
// scope the reconcile to where served data actually begins.
func retentionFloor(ctx context.Context, store *timescale.Store, src reconSource, lo, hi uint32) (uint32, error) {
	for _, tgt := range src.targets {
		minL, ok, err := store.MinLedger(ctx, tgt.table, "ledger", tgt.whereFilter, lo, hi)
		if err != nil {
			return 0, err
		}
		if ok && minL > lo {
			lo = minL
		}
	}
	return lo, nil
}

// reDeriveSDEXCensusViaDecoder re-derives the expected SDEX trade count per
// ledger by running the SDEX decoder over the certified CH operations and
// counting the DISTINCT, Validate-passing trades it emits — mirroring exactly
// what InsertTrade lands in the served tier (the Validate gate AND the served
// PK's ON CONFLICT DO NOTHING de-dup). This is the honest projection oracle:
// census == served by identical write logic, so the residual is exactly the
// ops the served tier dropped (real coverage gaps) — not a methodology
// artifact (one-side-zero fills or op_index fanout collisions, both of which
// the served can't hold but the CH substrate retains). Read-only; windowed
// 100k so the operations⋈results join stays under the CH memory cap.
//
// distinct-PK accumulation is natural fan-out; splitting hurts the read.
//
//nolint:gocognit // windowed stream → per-op decode → per-trade Validate +
func reDeriveSDEXCensusViaDecoder(ctx context.Context, chAddr string, from, to uint32) (map[uint32]int, error) {
	out := make(map[uint32]int)
	dec := sdex.NewDecoder()
	// sdexPK is the served trades primary key minus its per-ledger constants:
	// source is always "sdex" and ts is the ledger close-time (constant within
	// a ledger), so per-ledger de-dup reduces to (tx_hash, op_index). Counting
	// DISTINCT keys mirrors the served ON CONFLICT (source,ledger,tx_hash,
	// op_index,ts) DO NOTHING: the op_index fanout stride (1024) collides for
	// rare >1024-claim ops, which the served de-dups — so the census must too,
	// or it reads systematically high (the fixed-across-tips residual).
	type sdexPK struct {
		tx string
		op uint32
	}
	// 25k (was 100k): halves twice the per-window join input after the
	// 2026-07-05 OOM series — combined with the reader's grace_hash
	// spill this bounds memory regardless of history growth.
	const window = 25_000
	for lo := from; lo <= to; lo += window {
		hi := lo + window - 1
		if hi > to {
			hi = to
		}
		seen := make(map[uint32]map[sdexPK]struct{})
		if err := clickhouse.StreamSDEXOps(ctx, chAddr, lo, hi, func(op clickhouse.SDEXOp) error {
			// SDEX Decode soft-fails per claim (never a non-nil error).
			outs, _ := dec.Decode(dispatcher.OpContext{
				Ledger:   op.Ledger,
				ClosedAt: op.ClosedAt,
				TxHash:   op.TxHash,
				TxSource: op.Source,
				OpIndex:  int(op.OpIndex),
				Op:       op.Op,
				OpResult: op.OpResult,
			})
			// Mirror the served write exactly with two filters: (1)
			// canonical.Trade.Validate() (BaseAmount>0 ∧ QuoteAmount>0) — the
			// decoder emits one-side-zero fills for raw completeness but
			// InsertTrade rejects them; (2) PK de-dup — fanout collisions on
			// >1024-claim ops are coalesced by ON CONFLICT. Both leave the raw
			// claim in the CH substrate; the served projection holds the
			// distinct, valid subset, so this counts that subset.
			for _, ev := range outs {
				te, ok := ev.(sdex.TradeEvent)
				if !ok || te.Trade.Validate() != nil {
					continue
				}
				s := seen[te.Trade.Ledger]
				if s == nil {
					s = make(map[sdexPK]struct{})
					seen[te.Trade.Ledger] = s
				}
				s[sdexPK{tx: te.Trade.TxHash, op: te.Trade.OpIndex}] = struct{}{}
			}
			return nil
		}); err != nil {
			return nil, err
		}
		for ledger, s := range seen {
			out[ledger] += len(s)
		}
		if hi == to {
			break
		}
	}
	return out, nil
}

// contractCallRowID projects a ContractCall-source event onto the identity the
// served tier stores it under — its table PK minus the per-ledger constants
// (source + ledger_close_time, both fixed within a ledger). The auth tree
// surfaces the SAME authorized call at multiple CallPaths for multi-entry
// (co-signed) / nested-auth txs (see dispatcher.extractInvokeContractCallTrees:
// "Duplicate calls across entries are accepted… dispatch-side dedup is the
// consumer's concern via the CallPath identifier"). The served ON CONFLICT
// dedups on these columns, so the census counts DISTINCT identities to match —
// mirroring reDeriveSDEXCensusViaDecoder.
//   - soroswap-router → soroswap_router_swaps PK (…, tx_hash, op_index, call_sig):
//     callSig (migration 0056) is the per-call discriminator — distinct swaps in
//     one op get distinct identities (all stored); identical auth-tree dups share
//     a callSig and dedup. ts unused.
//   - band → oracle_updates PK (…, tx_hash, op_index, ts): op_index is fanned per
//     feed, so (tx, op, ts) is already unique per update. callSig unused.
type contractCallRowID struct {
	tx      string
	op      uint32
	ts      int64
	callSig string
}

// contractCallRowIdentity returns the served-row identity for a ContractCall
// source event. ok=false for an event type not routed through such a source
// (defensive; never expected here).
func contractCallRowIdentity(ev consumer.Event) (contractCallRowID, bool) {
	switch e := ev.(type) {
	case soroswap_router.Event:
		s := e.Swap
		return contractCallRowID{tx: s.TxHash, op: uint32(s.OpIndex), callSig: s.CallSig()}, true //nolint:gosec // op_index is a small non-negative op position
	case band.UpdateEvent:
		u := e.Update
		return contractCallRowID{tx: u.TxHash, op: u.OpIndex, ts: u.Timestamp.Unix()}, true
	}
	return contractCallRowID{}, false
}

// reDeriveContractCallCensus re-derives the expected row count per ledger for an
// event-less ContractCall source (band, soroswap-router). With no soroban_events
// landing zone, this IS the projection oracle. It counts DISTINCT served-PK
// identities (contractCallRowID) — not raw events — so the auth-tree duplicates
// the live path also dedups (via ON CONFLICT) don't read as a coverage gap,
// while genuinely-distinct swaps that share (tx, op) are kept apart by call_sig
// (mirrors reDeriveSDEXCensusViaDecoder's distinct-PK count). Built on
// forEachContractCallEvent so it decodes byte-identically to the ch-rebuild
// WRITE path; the write persists the same raw events and ON CONFLICT collapses
// them to this exact set, so a written-row re-verify reaches Δ=0.
func reDeriveContractCallCensus(ctx context.Context, chAddr, contractStrkey string, dec dispatcher.ContractCallDecoder, from, to uint32) (map[uint32]int, error) {
	seen := make(map[uint32]map[contractCallRowID]struct{})
	err := forEachContractCallEvent(ctx, chAddr, contractStrkey, dec, from, to, func(ledger uint32, ev consumer.Event) error {
		id, ok := contractCallRowIdentity(ev)
		if !ok {
			return fmt.Errorf("reDeriveContractCallCensus: unexpected event type %T (no served-row identity)", ev)
		}
		ids := seen[ledger]
		if ids == nil {
			ids = make(map[contractCallRowID]struct{})
			seen[ledger] = ids
		}
		ids[id] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make(map[uint32]int, len(seen))
	for ledger, ids := range seen {
		out[ledger] = len(ids)
	}
	return out, nil
}

// forEachContractCallEvent streams the lake's InvokeContract ops that touch
// contractStrkey, extracts each op's auth-tree calls
// (dispatcher.ExtractContractCallTree — byte-identical to what the live
// dispatcher feeds its ContractCallDecoders), runs dec over the matching ones,
// and invokes fn once per decoded event with its event ledger. It is the single
// decode path shared by the projection census (reDeriveContractCallCensus, which
// counts) and the ch-rebuild writer (which buffers + persists), so the WRITE
// path produces EXACTLY what the census expects. contractStrkey is decoded to
// its 32-byte ID for the body_xdr substring filter; windowed so the
// successful-tx IN-set stays bounded.
//
// linear pipeline; splitting hurts the read (mirrors reDeriveSDEXCensusViaDecoder).
//
//nolint:gocognit // windowed stream → per-op call-tree → Matches/Decode is a
func forEachContractCallEvent(ctx context.Context, chAddr, contractStrkey string, dec dispatcher.ContractCallDecoder, from, to uint32, fn func(ledger uint32, ev consumer.Event) error) error {
	raw, err := strkey.Decode(strkey.VersionByteContract, contractStrkey)
	if err != nil {
		return fmt.Errorf("decode contract strkey %s: %w", contractStrkey, err)
	}
	contractHex := hex.EncodeToString(raw)
	const window = 250_000
	for lo := from; lo <= to; lo += window {
		hi := lo + window - 1
		if hi > to {
			hi = to
		}
		if err := clickhouse.StreamContractCallOps(ctx, chAddr, contractHex, lo, hi, func(op clickhouse.ContractCallOp) error {
			for _, call := range dispatcher.ExtractContractCallTree(op.Op) {
				if !dec.Matches(call.ContractID, call.FunctionName) {
					continue
				}
				evs, derr := dec.Decode(dispatcher.ContractCallContext{
					Ledger:            op.Ledger,
					ClosedAt:          op.ClosedAt,
					TxHash:            op.TxHash,
					TxSource:          op.Source,
					OpSource:          op.Source,
					OpIndex:           int(op.OpIndex),
					ContractID:        call.ContractID,
					FunctionName:      call.FunctionName,
					Args:              call.Args,
					CallPath:          call.CallPath,
					CallPathContracts: call.CallPathContracts,
				})
				if derr != nil {
					continue // malformed call: skip per the decoder contract
				}
				for _, ev := range evs {
					if ferr := fn(op.Ledger, ev); ferr != nil {
						return ferr
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if hi == to {
			break
		}
	}
	return nil
}

func hasTradesTarget(src reconSource) bool {
	for _, t := range src.targets {
		if t.table == "trades" {
			return true
		}
	}
	return src.census // sdex census also writes trades
}

func absDiff(a, b int) int {
	if a > b {
		return a - b
	}
	return b - a
}

// projectionDelta compares one target's re-derived expected counts
// against its served counts, both keyed by ledger.
//
// Default is STRICT PER-LEDGER via completeness.ReconcileCounts —
// CS-084: comparing window totals lets a real drop in ledger L net
// against a phantom overcount elsewhere in the window and report
// complete=true; the per-ledger maps were already computed on both
// sides, only the comparison used to collapse them. Sources with a
// non-empty aggregateReconcile keep the totals compare for the
// keying reason their catalogue entry documents, and accept that
// netting residual.
//
// Returns Σ|per-ledger Δ| (0 = clean) and a human detail string.
func projectionDelta(src reconSource, table string, expected, actual map[uint32]int, lo, hi uint32) (int, string) {
	if src.aggregateReconcile != "" {
		e, a := sumCounts(expected), sumCounts(actual)
		if d := absDiff(e, a); d != 0 {
			return d, fmt.Sprintf("%s: expected=%d served=%d Δ=%d [%d,%d] (aggregate compare — %s)",
				table, e, a, d, lo, hi, src.aggregateReconcile)
		}
		return 0, ""
	}
	gaps := completeness.ReconcileCounts(expected, actual)
	if len(gaps) == 0 {
		return 0, ""
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].Ledger < gaps[j].Ledger })
	delta := 0
	for _, g := range gaps {
		delta += absDiff(g.Expected, g.Actual)
	}
	first := gaps[0]
	return delta, fmt.Sprintf("%s: %d mismatched ledger(s), Σ|Δ|=%d, first: ledger=%d expected=%d served=%d [%d,%d]",
		table, len(gaps), delta, first.Ledger, first.Expected, first.Actual, lo, hi)
}

// computeRecognitionGapsCH is the CH-backed recognition audit: distinct
// (contract, topic) shapes from the certified lake (excluding the CAP-67
// classic-token firehose — sep41 isn't enabled, so it's out of protocol scope)
// run through the dispatcher's Recognize(). Fast + off the serving DB vs the
// Postgres soroban_events scan in computeRecognitionGaps.
func computeRecognitionGapsCH(ctx context.Context, cfg config.Config, chAddr string, gated map[string][]contractid.Option, from, tip uint32) ([]completeness.RecognitionGap, error) {
	disp, err := pipeline.BuildDispatcher(cfg.Ingestion.EnabledSources, cfg.Oracle, gated)
	if err != nil {
		return nil, fmt.Errorf("build dispatcher: %w", err)
	}
	shapes, err := clickhouse.DistinctTopicShapes(ctx, chAddr, from, tip, clickhouse.ClassicTokenTopic0Syms)
	if err != nil {
		return nil, err
	}
	var gaps []completeness.RecognitionGap
	for _, s := range shapes {
		if _, ok := disp.Recognize(s.Event()); ok {
			continue
		}
		gaps = append(gaps, completeness.RecognitionGap{
			ContractID: s.ContractID,
			Topic0Sym:  s.Topic0Sym,
			Count:      int64(s.Count),
			MinLedger:  s.MinLedger,
			MaxLedger:  s.MaxLedger,
			Reason:     "no decoder matches",
		})
	}
	return gaps, nil
}

// computeRecognitionGaps runs the global recognition audit over the
// Soroban era and returns every unrecognized event shape.
func computeRecognitionGaps(ctx context.Context, store *timescale.Store, cfg config.Config, gated map[string][]contractid.Option, tip uint32) ([]completeness.RecognitionGap, error) {
	disp, err := pipeline.BuildDispatcher(cfg.Ingestion.EnabledSources, cfg.Oracle, gated)
	if err != nil {
		return nil, fmt.Errorf("build dispatcher: %w", err)
	}
	samples, err := store.DistinctSorobanTopicSamples(ctx, sorobanEraGenesis, tip)
	if err != nil {
		return nil, err
	}
	return completeness.AuditRecognition(samples, disp), nil
}

func nilOrOne(v uint32) []uint32 {
	if v == 0 {
		return nil
	}
	return []uint32{v}
}
