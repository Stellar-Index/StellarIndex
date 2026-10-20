package chops

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"golang.org/x/sync/errgroup"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sourcenet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	soroswap_router "github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// sorobanEraFloor is the network's first Soroban ledger, the lower bound of the recognition scan
// and event census. A test net starts at 1, so the pubnet boundary would invert the range.
func sorobanEraFloor(cfg config.Config) uint32 {
	if cfg.Stellar.SorobanGenesisLedger == 0 {
		return clickhouse.SorobanGenesisLedger
	}
	return cfg.Stellar.SorobanGenesisLedger
}

// sourceSubstrateOK is the per-source Claim-1 verdict: OK iff no problem or the problem ledger is below genesis. Relies on
// SubstrateProblem returning the range tip for an empty or truncated lake.
func sourceSubstrateOK(problem uint32, hasProblem bool, genesis uint32) bool {
	return !hasProblem || problem < genesis
}

// substrateScan is the memoized CH substrate-check result for one scanned
// floor (see substrateForGenesis).
type substrateScan struct {
	problem uint32
	has     bool
	detail  string
}

// substrateScanner is the seam over clickhouse.SubstrateProblem that keeps substrateForGenesis
// testable without a live lake.
type substrateScanner func(ctx context.Context, from, to uint32) (problem uint32, hasProblem bool, detail string, err error)

// substrateForGenesis scans the CH substrate once per distinct floor, over [max(floor,genesis), tip]: a shared scan would let a hole below
// a high-genesis source hide a later one in its range. floor > tip (-skip-substrate) returns zero without scanning.
func substrateForGenesis(ctx context.Context, scan substrateScanner, cache map[uint32]substrateScan, genesis, floor, tip uint32) (substrateScan, error) {
	if floor > tip {
		return substrateScan{}, nil
	}
	scanFrom := floor
	if genesis > scanFrom {
		scanFrom = genesis
	}
	if cached, ok := cache[scanFrom]; ok {
		return cached, nil
	}
	p, has, d, err := scan(ctx, scanFrom, tip)
	if err != nil {
		return substrateScan{}, err
	}
	res := substrateScan{problem: p, has: has, detail: d}
	cache[scanFrom] = res
	return res, nil
}

// computeCompleteness derives per-source completeness watermarks (substrate, recognition, projection; ADR-0033) into
// completeness_snapshots. Exit status says whether the pass ran, not the verdict; a per-source error keeps that source's prior verdict.
func computeCompleteness(args []string) error { //nolint:funlen,gocognit,gocyclo // linear computor; one block per claim.
	fs, gate := opsutil.NewMutatingFlagSet("compute-completeness")
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	toFlag := fs.Uint("to", 0, "Tip ledger (inclusive); 0 = resolve from the live ledgerstream cursor. A frozen cursor is refused either way unless -allow-frozen-cursor is set")
	allowFrozenCursor := fs.Bool("allow-frozen-cursor", false, "Stamp a verdict even though the ledgerstream cursor is provably behind the network (operator override; requires -to)")
	only := fs.String("source", "", "Limit to one source (e.g. soroswap|blend|reflector-dex|sdex)")
	useCH := fs.Bool("ch", false, "Required: read all three claims from the certified ClickHouse lake (substrate + recognition + projection re-derive), off the serving DB (ADR-0033 + ADR-0034). A run without it fails")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	skipSubstrate := fs.Bool("skip-substrate", false, "Skip the hash-chain re-scan and CARRY the prior substrate verdict — fast per-source iteration once substrate is proven. This run scans nothing, so it can only CONFIRM a prior clean verdict that already reached this run's tip; a FAILING or short prior verdict publishes substrate_ok=false with the unverified band named in the detail (C4-057). It no longer asserts substrate_ok=true unconditionally.")
	skipRecognition := fs.Bool("skip-recognition", false, "Trust the prior recognition audit (recognition_ok=true) instead of re-scanning all topic shapes — the global DistinctTopicShapes scan is the load-heaviest step; skip it for gentle projection-only iteration once recognition is verified")
	fromLedger := fs.Uint("from", 0, "INCREMENTAL verify: only check [from, tip], trusting [genesis, from] as already verified (substrate + recognition + projection all scoped to [from, tip]); the watermark still extends to tip when the window is clean. 0 = full verify from each source's genesis. The completeness timer passes min(watermark) from the prior snapshots so each run re-checks only new ledgers — minutes, not hours. An incremental run can only CONFIRM or DOWNGRADE the served `complete` axis, never upgrade it: a range it did not reconcile is carried from the prior verdict, and a FAILING prior verdict is cleared only by a full run (INV-5 — see projectionClaim).")
	pass := fs.Bool("pass", false, "WHOLE-PASS mode: prove recognition + substrate ONCE at full range for the ENTIRE source catalogue, then reconcile EACH source's projection incrementally from its own prior watermark (projectionFloor). One process replaces the per-source, per-chunk driver that re-ran the load-heaviest global recognition scan (DistinctTopicShapes, ~9 min over full history at max_threads=2) on EVERY 25k chunk — the nightly timeout that froze the alphabetical tail's verdicts. Because substrate is proven at FULL tip, its write advances the tip and is never blocked by the CS-083 guard (the low-tip substrate flap). Every catalogue source gets a verdict — a never-seeded source, and a source whose prior projection verdict was FAILING, both reconcile from genesis (a red source has no verified ground to resume from, so the pass re-verifies it rather than carrying the failure forward forever). Requires -ch; incompatible with -source / -from / -skip-substrate / -skip-recognition (the per-chunk knobs it replaces).")
	maxCarryAge := fs.Duration("max-carry-age", completeness.MaxProjectionCarryAge, "With -pass: a source whose clean projection claim was last reconciled over its whole served range longer ago than this (or never, on record) re-verifies from genesis instead of carrying the claim forward again — oldest evidence first, at most 3 sources per pass, never the SDEX census or a reproofOutlastsPass source such as sep41_transfers (their full re-verify outlasts the pass; compute-completeness-sdex.timer re-proves sdex weekly with -source sdex). 0 = carry without bound.")
	sourceTimeout := fs.Duration("source-timeout", 45*time.Minute, "Deadline for ONE source's evaluation; a source that exceeds it gets no verdict (its prior stands) and the pass moves on, so one slow source cannot starve the rest. 0 disables; ignored with -source, where -timeout is the budget (the weekly SDEX re-proof needs hours).")
	timeout := fs.Duration("timeout", 120*time.Minute, "Deadline for the whole run; sources not reached by it keep their prior verdict and are named in the error. Keep it below the systemd unit's TimeoutStartSec.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return fmt.Errorf("-config is required")
	}
	if *timeout <= 0 {
		return fmt.Errorf("-timeout must be > 0, got %s", *timeout)
	}
	// Explicit rather than implied so a caller still written for the removed
	// Postgres soroban_events path fails here instead of silently verifying less.
	if !*useCH {
		return fmt.Errorf("compute-completeness: -ch is required (every claim reads the certified ClickHouse lake; the Postgres soroban_events path is removed)")
	}
	// Fail CLOSED on -pass combined with the knobs it replaces (validatePassFlags): a partial pass
	// over a subset of sources, or one carrying a stale verdict, would look complete.
	if perr := validatePassFlags(*pass, *only, *fromLedger, *skipSubstrate, *skipRecognition); perr != nil {
		return fmt.Errorf("compute-completeness: %w", perr)
	}
	// The completeness timers read exit 0 as "verdicts published".
	if err := gate.RequireStatedMode(); err != nil {
		return err
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}
	write := gate.Banner()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage open: %w", err)
	}
	defer func() { _ = store.Close() }()

	cur, gerr := store.GetCursor(ctx, "ledgerstream", "")
	tip, err := verdictTipFromCursorRead(uint32(*toFlag), cur, gerr, time.Now(), *allowFrozenCursor) //nolint:gosec // ledger seq fits uint32
	if err != nil {
		return fmt.Errorf("compute-completeness: %w", err)
	}
	if tip == 0 {
		return fmt.Errorf("tip resolved to 0 — pass -to")
	}
	fmt.Fprintf(os.Stderr, "compute-completeness: tip=%d\n", tip)
	sorobanFloor := sorobanEraFloor(cfg)
	if *pass {
		fmt.Fprintln(os.Stderr, "compute-completeness: -pass — recognition + substrate proven once at full range; projection reconciled per source from its own watermark")
	}

	// buildReconciliationCatalogue promotes sep41_transfers/sep41_supply into this catalogue itself
	// whenever [supply] watched_sep41_contracts is configured.
	catalogue, soroswapDec, err := buildReconciliationCatalogue(cfg)
	if err != nil {
		return fmt.Errorf("compute-completeness: reconciliation catalogue: %w", err)
	}
	// Fail CLOSED on an unknown -source: the loop would skip every source and report success
	// having verified nothing.
	if verr := validateSourceFilter(*only, catalogue); verr != nil {
		return fmt.Errorf("compute-completeness: %w", verr)
	}

	// Warm the factory-anchored gated registries (ADR-0035) so recognition accepts real protocol children and flags foreign
	// emitters of the same topic. Read-only: the audit must not mutate the registry. An unseeded protocol_contracts table would
	// surface every child as a false gap. Warmed before any catalogue reader because the same options gate the projection side.
	gatedOpts, gerr := pipeline.GatedRegistryOptions(ctx, store, slog.Default(), ctx, false)
	if gerr != nil {
		return fmt.Errorf("gated registry warm: %w", gerr)
	}
	// Re-derive the expected side on the live indexer's gate (curated set plus protocol_contracts),
	// not the bare in-code seed; otherwise a contract admitted via protocol_contracts yields served
	// rows no expected side explains, published as a false projection mismatch. Must precede the
	// ownerOf attribution and every per-source re-derive, which seed into the decoders rebuilt here.
	if catalogue, err = applyGatedOptions(catalogue, gatedOpts); err != nil {
		return fmt.Errorf("compute-completeness: %w", err)
	}

	if *only == "" || *only == "soroswap" {
		// Fail closed: a partial seed would publish a false projection red for soroswap. A disabled
		// seed (no factory configured) is not a failure; see seedSoroswapForRecon.
		if serr := seedSoroswapForRecon(ctx, cfg, *chAddr, soroswapDec); serr != nil {
			return fmt.Errorf("compute-completeness: soroswap pair seed: %w", serr)
		}
	}

	// The CH event source for projection re-derive is built PER SOURCE in the loop: the wide
	// op_args_xdr column is read only for decoders that consume events.Event.OpArgs (redstone).

	// ── Recognition (Claim 2a): one global scan, attributed per source ──
	//
	// Fail closed: a scan error aborts the run, because an empty recGaps would read as a clean
	// scan and publish a false "complete" on /v1/coverage. No snapshot is written, so the last
	// real verdict stands.
	if *skipRecognition {
		fmt.Fprintln(os.Stderr, "compute-completeness: -skip-recognition — trusting prior recognition audit (no shape scan)")
	}
	// Seed the census's soroswap decoder from the same pair registry that feeds ownerOf. With an
	// empty pairTokens map Matches() rejects every real pair event, creating false "unhandled
	// topic" gaps and clamping soroswap's projection watermark. Matches() needs only key presence,
	// so empty PairTokens suffice. Fail closed on a load error; skipped under -skip-recognition.
	var soroswapOpts []soroswap.DecoderOption
	if !*skipRecognition {
		pairs, perr := store.LoadSoroswapPairRegistry(ctx)
		if perr != nil {
			return fmt.Errorf("load soroswap pair registry for recognition census: %w", perr)
		}
		seed := make(map[string]soroswap.PairTokens, len(pairs))
		for _, p := range pairs {
			seed[p.PairStrkey] = soroswap.PairTokens{}
		}
		soroswapOpts = append(soroswapOpts, soroswap.WithSeededPairTokensDecoder(seed))
	}
	recGaps, recErr := runRecognitionScan(*skipRecognition, func() ([]completeness.RecognitionGap, error) {
		// Recognition (Claim 2a) is a full-history property, so it is never scoped to the incremental -from window: a
		// low-volume source's rare wrong-topic event would slip through. DistinctTopicShapes is bounded-memory at any lake
		// size, so always scan from genesis; -from scopes only the row-by-row projection reconcile below.
		return computeRecognitionGapsCH(ctx, cfg, *chAddr, gatedOpts, sorobanFloor, tip, soroswapOpts...)
	})
	if recErr != nil {
		return recErr
	}
	ownerOf := contractOwners(catalogue)
	// Topic-matched sources (soroswap, aquarius, phoenix, comet, defindex, blend) have no static ownerOf entries; without the
	// protocol_contracts and soroswap_pairs registries a dropped topic on their pools would go unattributed and their
	// recognition axis could never fail. Fail closed on a registry read error. Only done when the scan found gaps.
	if len(recGaps) > 0 {
		if oerr := loadRegistryOwners(ctx, store, ownerOf); oerr != nil {
			return fmt.Errorf("recognition owner attribution (failing closed — an incomplete owner map cannot fail recognition for the topic-matched sources): %w", oerr)
		}
	}
	recBySource, unattributed := attributeRecognitionGaps(ownerOf, recGaps)

	// Substrate (Claim 1) is scoped per source: each gets its own scan floored at its genesis,
	// memoised by floor (see substrateForGenesis). The CH lake is the authoritative substrate.
	chSubstrateScanner := substrateScanner(func(ctx context.Context, from, to uint32) (uint32, bool, string, error) {
		return clickhouse.SubstrateProblem(ctx, *chAddr, from, to)
	})
	chSubstrateCache := make(map[uint32]substrateScan)
	// subScanFrom is the floor this run's substrate scan reached; above tip means no scan at all
	// (-skip-substrate). substrateClaim needs it to tell "proven from genesis" from "trusted below
	// a floor".
	subScanFrom := tip + 1
	if *skipSubstrate {
		fmt.Fprintln(os.Stderr, "compute-completeness: -skip-substrate — no substrate scan; the prior verdict is carried, not re-proven")
	} else {
		subScanFrom = uint32(2)
		if *fromLedger > 2 {
			subScanFrom = uint32(*fromLedger) //nolint:gosec // ledger seq fits uint32
		}
	}
	// The event table is censused against ledgers over the whole Soroban era every run, including
	// -skip-substrate and -from runs, whose carried prefix is where a dropped partition hides
	// (eventCensusLoss).
	evCensus, err := clickhouse.EventCensusShortfalls(ctx, *chAddr, sorobanFloor, tip)
	if err != nil {
		return fmt.Errorf("contract_events census (failing closed — cannot certify the event table recognition and projection read): %w", err)
	}

	// Prior verdicts. An incremental run (-from) reconciles only a suffix, so it may confirm or downgrade the served `complete`
	// axis but never upgrade it; the prior verdict is the only evidence for the untouched prefix. Read them before the loop
	// overwrites them and fail closed on error (see projectionClaim).
	priorSnaps, err := store.ListCompletenessSnapshots(ctx)
	if err != nil {
		return fmt.Errorf("prior completeness verdicts (failing closed — an incremental run cannot gate its claim without them): %w", err)
	}
	priorProj, priorSub, priorRec, priorWatermark := buildPriorVerdicts(priorSnaps)
	if *pass { // projectionFloor honours an expired carry only in -pass
		if refloored := expireStaleCarries(priorProj, catalogue, time.Now(), *maxCarryAge); len(refloored) > 0 {
			fmt.Fprintf(os.Stderr, "compute-completeness: re-proving expired projection evidence from genesis this pass: %s\n", strings.Join(refloored, ", "))
		}
	}

	// Durable per-target projection floors. They are the only record of how far back each target
	// was ever verified, so a run that cannot read them cannot tell whether the served tier lost
	// its oldest rows. Fail closed, like the prior verdicts.
	targetFloors, err := store.CompletenessTargetFloors(ctx)
	if err != nil {
		return fmt.Errorf("durable projection floors (failing closed — cannot distinguish served-tier loss from never-projected without them): %w", err)
	}

	// Pending replay-rewind dirty windows. A projector-replay that rewound below a source's watermark rewrote rows the carried
	// projection claim still certifies, so the carry is invalid until that range is re-reconciled. Fail closed: a run blind to
	// them would republish carried claims over rewritten ranges.
	dirtyWindows, err := store.ProjectionDirtyWindows(ctx)
	if err != nil {
		return fmt.Errorf("pending replay-rewind dirty windows (failing closed — carrying a projection claim over a rewound range is the invalidation this record exists to prevent): %w", err)
	}

	// ── System recognition snapshot (gaps on contracts no source owns) ──
	//
	// Refreshed on every run, whatever the -source filter, and before the per-source loop so a
	// pass that runs out of time still publishes it. An upsert error never stops the loop.
	var recSnapErr error
	if !*skipRecognition && !write {
		fmt.Fprintf(os.Stderr, "compute-completeness: recognition  unattributed=%d (dry-run: not stored)\n", len(unattributed))
	} else if !*skipRecognition {
		// The census feeds /v1/coverage's recognition axis; the contract count shows how
		// concentrated the unattributed shapes are.
		census := completeness.RecognitionCensus{Shapes: len(unattributed)}
		contracts := make(map[string]struct{}, len(unattributed))
		for _, g := range unattributed {
			if census.EarliestLedger == 0 || g.MinLedger < census.EarliestLedger {
				census.EarliestLedger = g.MinLedger
			}
			contracts[g.ContractID] = struct{}{}
		}
		census.Contracts = len(contracts)
		recW := completeness.ComputeWatermark(sorobanFloor, tip, nilOrOne(census.EarliestLedger))
		detail := completeness.FormatRecognitionDetail(census)
		if err := store.UpsertCompletenessSnapshot(ctx, timescale.CompletenessSnapshot{
			Source: completeness.SystemRecognitionSource, Genesis: sorobanFloor, Tip: tip,
			Watermark: recW.Ledger, CoveragePct: recW.CoveragePct, Complete: recW.Complete,
			LakeComplete: recW.Complete, // no projection axis on this system snapshot
			FirstProblem: recW.FirstProblem, SubstrateOK: true, RecognitionOK: census.Shapes == 0, ProjectionOK: true,
			Detail: detail,
		}); err != nil {
			recSnapErr = fmt.Errorf("upsert recognition snapshot: %w", err)
		} else {
			fmt.Fprintf(os.Stderr, "compute-completeness: recognition  unattributed=%d contracts=%d coverage=%.4f\n",
				census.Shapes, census.Contracts, recW.CoveragePct)
		}
	}

	// ── Per-source watermark ────────────────────────────────────────
	evalSource := func(ctx context.Context, src reconSource) error {
		genesis := src.genesis
		var problems []uint32
		var detail []string

		// Claim 1: substrate continuity + hash chain over [genesis, tip].
		var substrateOK, eventsShort bool
		// Scan this source's own [genesis,tip] (memoised per floor). The problem is the source's
		// only if it falls at or after its genesis; SubstrateProblem reports coverage failures
		// at correct ledgers so a high-genesis source cannot read one as "below my genesis".
		srcScanFrom := subScanFrom
		if genesis > srcScanFrom {
			srcScanFrom = genesis
		}
		srcSub, serr := substrateForGenesis(ctx, chSubstrateScanner, chSubstrateCache, genesis, subScanFrom, tip)
		if serr != nil {
			return fmt.Errorf("%s: ch substrate [%d,%d]: %w", src.name, srcScanFrom, tip, serr)
		}
		switch {
		case srcSub.has:
			fmt.Fprintf(os.Stderr, "compute-completeness: CH substrate problem for %s at %d (%s)\n", src.name, srcSub.problem, srcSub.detail)
		case subScanFrom <= tip:
			fmt.Fprintf(os.Stderr, "compute-completeness: CH substrate intact [%d,tip] for %s — contiguous + hash-chained\n", srcScanFrom, src.name)
		}
		scanClean := sourceSubstrateOK(srcSub.problem, srcSub.has, genesis)
		if !scanClean {
			problems = append(problems, srcSub.problem)
		}
		// Publish only on the range actually scanned; the scan floor and the claim differ.
		var subDetail string
		substrateOK, subDetail = substrateClaim(genesis, tip, srcScanFrom, scanClean, srcSub.problem, priorSub[src.name])
		detail = append(detail, subDetail)
		// substrateClaim can refuse a clean suffix scan (no, failing or stale prior) while
		// problems holds no ledger. Feed the unproven-prefix floor in so the numeric coverage
		// watermark never reads 1.0 while substrate_ok is false (see lakeCoverageProblem).
		if p := lakeCoverageProblem(genesis, scanClean, substrateOK, priorSub[src.name]); p != 0 {
			problems = append(problems, p)
		}
		// Substrate twin of detectFloorLoss: a carried clean prefix hides a dropped partition below
		// subScanFrom, so probe the fixed src.genesis with a cheap 1-ledger scan; an absent genesis
		// fails substrate_ok. Only needed when a prefix is carried, since a deep scan re-reads it.
		if subScanFrom > genesis {
			fp, fh, _, ferr := clickhouse.SubstrateProblem(ctx, *chAddr, genesis, genesis)
			if ferr != nil {
				return fmt.Errorf("%s: substrate bottom-edge probe: %w", src.name, ferr)
			}
			if p, lost, d := substrateFloorLoss(genesis, subScanFrom, fp, fh); lost {
				substrateOK = false
				problems = append(problems, p)
				detail = append(detail, d)
			}
		}
		if p, short, d := eventCensusLoss(src, genesis, evCensus); short {
			substrateOK, eventsShort = false, true
			problems = append(problems, p)
			detail = append(detail, d)
		}

		// Claim 2a: recognition gaps attributed to this source's contracts (static IDs or the
		// registry members folded into ownerOf).
		recOK, recProblems := sourceRecognitionOK(genesis, tip, recBySource[src.name], *skipRecognition, priorRec[src.name])
		problems = append(problems, recProblems...)
		recDetail := recognitionClaim(recOK, *skipRecognition)
		if eventsShort {
			recOK, recDetail = false, "recognition: not certifiable — the contract_events it scans are short of what stellar.ledgers records (see substrate)"
		}
		detail = append(detail, recDetail)

		// The substrate and recognition watermark drives coverage ("did we capture every event").
		// Projection is a served-tier fidelity claim, evaluated separately so its keying and
		// retention scoping cannot corrupt the coverage signal.
		srW := completeness.ComputeWatermark(genesis, tip, problems)
		// Lake axis (ADR-0033/0034): substrate and recognition only, so lake_complete is never
		// gated by the retention-scoped projection reconcile. It is also gated by substrateOK, the
		// claim about the range scanned; the `&&` is a belt-and-suspenders guard.
		lakeComplete := srW.Complete && substrateOK
		projOK := false
		// The projection axis's own floor; without it a consumer would read the lake's
		// genesis_ledger as the served-tier floor. 0 means not evaluated.
		var projVerifiedFrom uint32
		// Set only when the CH reconcile found a failure, so a lower-tip run still records it.
		var projFound bool
		// What this run itself proved, as opposed to what it carried.
		var (
			projReconciledFrom uint32
			evidencedNow       bool
			evidencedAt        time.Time
		)
		// Incremental: reconcile only [projFrom, srW.Ledger], with projFrom from projectionFloor.
		// A pending replay-rewind window overrides that floor, because the rewritten range must be
		// re-reconciled before any claim covers it; -from cannot skip past it.
		dirtyWin, hasDirty := dirtyWindows[src.name]
		projFrom, deferDirty := projectionPlan(src, *pass, priorProj[src.name], priorWatermark[src.name], *fromLedger, srW.Ledger, dirtyWin, hasDirty)
		switch {
		case deferDirty:
			// Detail is written by the window disposition below.
		case srW.Ledger >= projFrom:
			streamer := clickhouse.ReconcileEventStreamer{Addr: *chAddr, NeedOpArgs: src.needsOpArgs, NeedStateWriteKeys: src.needsStateWriteKeys, SymbolTopic0Only: src.symbolTopic0}
			scopes, servedMins, servedFrom, runFrom, serr := projectionScopes(ctx, store, src, genesis, projFrom, srW.Ledger)
			if serr != nil {
				return fmt.Errorf("%s: served floor: %w", src.name, serr)
			}
			projVerifiedFrom = servedFrom
			// Bottom-edge truncation moves the reconcile's own floor, so compare against the
			// durable floor first and treat loss as a hard failure; the surviving rows would
			// otherwise reconcile perfectly and read complete.
			floorLoss := detectFloorLoss(src, servedMins, targetFloors)
			delta, blind, pdetail, perr := reconcileProjectionAggregate(ctx, store, streamer, *chAddr, src, scopes)
			if perr != nil {
				return fmt.Errorf("%s: projection: %w", src.name, perr)
			}
			projFound = projectionFoundProblem(delta, blind, floorLoss)
			// Record verified ground only after a clean, non-blind reconcile. Recording after
			// detected loss would adopt the post-loss floor and erase the evidence; a blind
			// re-derive has a zero delta only because both sides dropped the same rows.
			if write && delta == 0 && len(floorLoss) == 0 && !blind.Any() {
				if ferr := recordFloors(ctx, store, src, scopes, servedMins); ferr != nil {
					return fmt.Errorf("%s: record projection floors: %w", src.name, ferr)
				}
			}
			// A run may claim only the range it reconciled (ADR-0033). The clean predicate is
			// delta == 0 && !blind.Any(): undecodable ledgers add 0 to delta while proving
			// nothing, so a bare delta == 0 would certify exactly what the check cannot see.
			var claimDetail string
			projOK, claimDetail = projectionClaim(servedFrom, runFrom, srW.Ledger, delta == 0 && !blind.Any(), pdetail, priorProj[src.name], newClaimScope(src, scopes, servedMins, targetFloors))
			detail = append(detail, claimDetail)
			if len(floorLoss) > 0 {
				projOK = false
				detail = append(detail, floorLoss...)
			}
			if vacuous, d := projectionWithoutEvidence(projOK, len(src.targets), servedMins, genesis, srW.Ledger); vacuous {
				projOK, projVerifiedFrom = false, 0
				detail = append(detail, d)
			}
			projReconciledFrom = runFrom
			evidencedNow, evidencedAt = projectionEvidence(projOK, servedFrom, runFrom, priorProj[src.name])
			if projOK && !evidencedNow {
				detail = append(detail, carriedEvidenceDetail(evidencedAt))
			}
		default:
			detail = append(detail, "projection: not evaluated (earlier claim failed at genesis)")
		}
		// Coverage is substrate and recognition; complete also requires the served-tier projection
		// to reconcile, which lakeComplete never does. The served axis cannot be stronger than
		// the lake axis, hence lakeComplete gates it.
		w := servedAxisVerdict(srW, lakeComplete && projOK, deferDirty, dirtyWin)

		// Only a run whose clean projection verdict covered the whole window may delete it; any
		// other run leaves it pending so later runs keep extending their floor over it.
		var dirtyCleared bool
		if hasDirty {
			dirtyCleared = dirtyWindowSatisfied(dirtyWin, projOK, projFrom, genesis, srW.Ledger)
			if dirtyCleared {
				detail = append(detail, fmt.Sprintf(
					"projection: replay-rewind window [%d,%d] re-verified clean this run — clearing it",
					dirtyWin.From, dirtyWin.To))
			} else if deferDirty {
				detail = append(detail, fmt.Sprintf(
					"projection: not evaluated — dirty window [%d,%d] PENDING this source's dedicated weekly compute-completeness timer, which re-proves it from genesis; complete withheld and watermark held at %d until then",
					dirtyWin.From, dirtyWin.To, w.Ledger))
			} else {
				detail = append(detail, fmt.Sprintf(
					"projection: replay-rewind window [%d,%d] PENDING re-verification — the reconcile floor stays extended over it until a clean run covers it",
					dirtyWin.From, dirtyWin.To))
			}
		}

		if len(detail) == 0 {
			detail = append(detail, "complete: substrate + recognition + projection verified to tip")
		}
		snap := timescale.CompletenessSnapshot{
			Source: src.name, Genesis: genesis, Tip: tip,
			Watermark: w.Ledger, CoveragePct: w.CoveragePct, Complete: w.Complete,
			LakeComplete:             lakeComplete,
			FirstProblem:             w.FirstProblem,
			FoundProblem:             projFound,
			ProjectionVerifiedFrom:   projVerifiedFrom,
			ProjectionReconciledFrom: projReconciledFrom,
			ProjectionEvidencedNow:   evidencedNow,
			ProjectionEvidencedAt:    evidencedAt,
			SubstrateOK:              substrateOK, RecognitionOK: recOK, ProjectionOK: projOK,
			Detail: strings.Join(detail, "; "),
		}
		if !write {
			fmt.Fprintf(os.Stderr, "compute-completeness: %-14s watermark=%d coverage=%.4f complete=%v lake_complete=%v (%s) (dry-run: not stored)\n",
				src.name, w.Ledger, w.CoveragePct, w.Complete, lakeComplete, snap.Detail)
			return nil
		}
		pub, pubErr := publishSourceVerdict(ctx, store, snap, dirtyWin, dirtyCleared)
		if pubErr != nil {
			return fmt.Errorf("%s: publish verdict: %w", src.name, pubErr)
		}
		fmt.Fprintf(os.Stderr, "compute-completeness: %-14s watermark=%d coverage=%.4f complete=%v lake_complete=%v (%s)%s\n",
			src.name, w.Ledger, w.CoveragePct, w.Complete, lakeComplete, strings.Join(detail, "; "),
			verdictNotStoredNote(pub, tip, hasDirty))
		return nil
	}
	if *pass {
		catalogue = orderForPass(catalogue, priorProj, priorWatermark)
	}
	srcErr := evaluateEachSource(ctx, catalogue, *only, *sourceTimeout, evalSource)
	if recSnapErr != nil {
		return errors.Join(srcErr, recSnapErr)
	}

	// On a non-pubnet network, stale rows for pubnet-only sources would keep the verdict red by
	// construction, so clear them. Only names sourcenet classifies as not applicable are passed.
	if na := sourcenet.NotApplicableOn(cfg.Stellar.Network); write && len(na) > 0 {
		names := make([]string, 0, len(na))
		for _, e := range na {
			names = append(names, e.Source)
		}
		if n, derr := store.DeleteCompletenessSnapshots(ctx, names); derr != nil {
			return errors.Join(srcErr, fmt.Errorf("compute-completeness: clear not-applicable snapshots: %w", derr))
		} else if n > 0 {
			fmt.Fprintf(os.Stderr, "compute-completeness: network=%s — cleared %d stale snapshot row(s) for pubnet-only sources\n", cfg.Stellar.Network, n)
		}
	}

	return srcErr
}

// evaluateEachSource runs eval per selected source and joins failures so one error cannot withhold the others' verdicts (an errored source
// keeps its prior). perSource > 0 bounds each eval; a done ctx names the skipped sources.
func evaluateEachSource(ctx context.Context, catalogue []reconSource, only string, perSource time.Duration, eval func(context.Context, reconSource) error) error {
	var errs []error
	for i, src := range catalogue {
		if only != "" && src.name != only {
			continue
		}
		if cerr := ctx.Err(); cerr != nil {
			var skipped []string
			for _, s := range catalogue[i:] {
				if only == "" || s.name == only {
					skipped = append(skipped, s.name)
				}
			}
			errs = append(errs, fmt.Errorf("not evaluated, prior verdicts stand (%s): %w", strings.Join(skipped, ", "), cerr))
			break
		}
		sctx, scancel := ctx, context.CancelFunc(func() {})
		if perSource > 0 && only == "" { // with -source the run's -timeout is that source's budget
			sctx, scancel = context.WithTimeout(ctx, perSource)
		}
		err := eval(sctx, src)
		scancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "compute-completeness: %s: NO VERDICT this run, prior verdict stands: %v\n", src.name, err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// orderForPass runs from-genesis re-verifies after incremental sources so a long one cannot starve the cheap ones; the census (SDEX)
// source goes last, after any reproofOutlastsPass source.
func orderForPass(catalogue []reconSource, prior map[string]priorProjection, priorWatermark map[string]uint32) []reconSource {
	ordered := make([]reconSource, 0, len(catalogue))
	var fromGenesis, heavyFromGenesis, censusFromGenesis []reconSource
	for _, src := range catalogue {
		switch {
		case sourceProjectionFloor(src, true, prior[src.name], priorWatermark[src.name], 0) > src.genesis:
			ordered = append(ordered, src)
		case src.census:
			censusFromGenesis = append(censusFromGenesis, src)
		case src.outlastsPass():
			heavyFromGenesis = append(heavyFromGenesis, src)
		default:
			fromGenesis = append(fromGenesis, src)
		}
	}
	return append(append(append(ordered, fromGenesis...), heavyFromGenesis...), censusFromGenesis...)
}

// projectionFloor is the reconcile floor: -pass resumes from the prior watermark, or genesis if none, the prior failed (watermark sits at
// tip) or the carry expired; otherwise max(genesis, -from).
func projectionFloor(genesis uint32, pass bool, prior priorProjection, priorWatermark uint32, fromLedger uint) uint32 {
	if pass {
		if !prior.known || !prior.ok || prior.evidenceExpired {
			return genesis
		}
		if priorWatermark > genesis {
			return priorWatermark
		}
		return genesis
	}
	if uint32(fromLedger) > genesis { //nolint:gosec // ledger seq fits uint32
		return uint32(fromLedger) //nolint:gosec // ledger seq fits uint32
	}
	return genesis
}

// maxTipCursorLagLedgers is how far the network may run past the ledgerstream cursor before the
// cursor stops being a usable verdict tip: ~1h at [completeness.SlowestLedgerClose].
const maxTipCursorLagLedgers uint32 = 600

// tipFromLiveCursor resolves the verdict tip from the live cursor, refusing one the network has left behind (ADR-0033): a frozen ingest
// must not stamp a fresh computed_at on a stale tip.
func tipFromLiveCursor(cur timescale.Cursor, now time.Time) (uint32, error) {
	floor := completeness.NetworkTipLowerBound(cur.LastLedger, cur.UpdatedAt, now)
	if lag := floor - cur.LastLedger; lag > maxTipCursorLagLedgers {
		return 0, fmt.Errorf("ledgerstream cursor is frozen at ledger %d (last advanced %s ago; the network has closed at least %d ledgers since) — it is not the network tip, so no verdict is stamped against it (-allow-frozen-cursor with -to overrides)",
			cur.LastLedger, now.Sub(cur.UpdatedAt).Round(time.Second), lag)
	}
	return cur.LastLedger, nil
}

// verdictTipFromCursorRead applies the cursor read: with an explicit -to a missing cursor row
// means no live ingest to guard; any other read error, or a missing row without -to, fails closed.
func verdictTipFromCursorRead(to uint32, cur timescale.Cursor, readErr error, now time.Time, allowFrozen bool) (uint32, error) {
	if readErr != nil {
		if to != 0 && errors.Is(readErr, timescale.ErrNotFound) {
			return to, nil
		}
		return 0, fmt.Errorf("read ledgerstream cursor: %w", readErr)
	}
	return resolveVerdictTip(to, cur, now, allowFrozen)
}

// resolveVerdictTip picks the verdict tip; the frozen-cursor guard also covers an explicit -to, which the nightly wrapper passes.
func resolveVerdictTip(to uint32, cur timescale.Cursor, now time.Time, allowFrozen bool) (uint32, error) {
	live, err := tipFromLiveCursor(cur, now)
	switch {
	case err != nil && (!allowFrozen || to == 0):
		return 0, err
	case to != 0:
		return to, nil
	}
	return live, nil
}

// sourceProjectionFloor is projectionFloor for one source; in -pass, a genesis-claimed source whose clean prior covered only above genesis
// re-verifies from genesis (projectionClaim refuses that carry).
func sourceProjectionFloor(src reconSource, pass bool, prior priorProjection, priorWatermark uint32, fromLedger uint) uint32 {
	if pass && src.servedWindowReason == "" && prior.verifiedFrom > src.genesis {
		return src.genesis
	}
	return projectionFloor(src.genesis, pass, prior, priorWatermark, fromLedger)
}

// validatePassFlags rejects -pass with -source, -from, -skip-substrate or -skip-recognition: each defeats the single full-range proof or
// carries a prior verdict, so a partial pass would look complete.
func validatePassFlags(pass bool, source string, fromLedger uint, skipSubstrate, skipRecognition bool) error {
	if !pass {
		return nil
	}
	switch {
	case source != "":
		return fmt.Errorf("-pass computes EVERY source in one process and is incompatible with -source %q", source)
	case fromLedger != 0:
		return fmt.Errorf("-pass proves substrate + recognition at full range and reconciles each source from its own watermark; it is incompatible with -from %d", fromLedger)
	case skipSubstrate:
		return fmt.Errorf("-pass proves substrate fresh at full tip and is incompatible with -skip-substrate")
	case skipRecognition:
		return fmt.Errorf("-pass proves recognition fresh and is incompatible with -skip-recognition")
	}
	return nil
}

// runRecognitionScan fails closed on a scan error: an empty gap slice from a failed scan looks clean and would publish recognition_ok and
// complete for every source. scan is a func so the contract is testable.
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

// loadRegistryOwners folds the protocol_contracts and soroswap-pair registries into ownerOf so topic-matched sources fail recognition on
// their own pools; fails closed on a read error. Pure fold: mergeRegistryOwners.
func loadRegistryOwners(ctx context.Context, store *timescale.Store, ownerOf map[string][]string) error {
	gatedChildren := make(map[string][]string)
	for _, source := range pipeline.GatedSourceNames() {
		ids, err := store.LoadProtocolContracts(ctx, source)
		if err != nil {
			return fmt.Errorf("protocol_contracts %s: %w", source, err)
		}
		gatedChildren[source] = ids
	}
	pairs, err := store.LoadSoroswapPairRegistry(ctx)
	if err != nil {
		return fmt.Errorf("soroswap pair registry: %w", err)
	}
	pairIDs := make([]string, 0, len(pairs))
	for _, p := range pairs {
		pairIDs = append(pairIDs, p.PairStrkey)
	}
	mergeRegistryOwners(ownerOf, gatedChildren, pairIDs)
	return nil
}

// mergeRegistryOwners adds registry owners to ownerOf, except for contracts already pinned by a source's static contractIDs (curated
// sources keep exact attribution). The "soroswap" literal mirrors the catalogue name.
func mergeRegistryOwners(ownerOf map[string][]string, gatedChildren map[string][]string, soroswapPairs []string) {
	pinned := make(map[string]bool, len(ownerOf))
	for c := range ownerOf {
		pinned[c] = true
	}
	for source, ids := range gatedChildren {
		for _, c := range ids {
			if !pinned[c] {
				addOwner(ownerOf, c, source)
			}
		}
	}
	for _, p := range soroswapPairs {
		if !pinned[p] {
			addOwner(ownerOf, p, "soroswap")
		}
	}
}

// contractOwners maps each static contract ID to every source pinning it; sources may share a set (sep41_transfers, sep41_supply), so a
// single-owner map would leave one unable to fail recognition.
func contractOwners(catalogue []reconSource) map[string][]string {
	ownerOf := map[string][]string{}
	for _, src := range catalogue {
		for _, c := range src.contractIDs {
			addOwner(ownerOf, c, src.name)
		}
	}
	return ownerOf
}

func addOwner(ownerOf map[string][]string, contract, source string) {
	if !slices.Contains(ownerOf[contract], source) {
		ownerOf[contract] = append(ownerOf[contract], source)
	}
}

// attributeRecognitionGaps splits the global scan's gaps via ownerOf into per-source gaps (capping that source's recognition axis) and the
// unattributed rest for the system `recognition` snapshot. Pure.
func attributeRecognitionGaps(ownerOf map[string][]string, gaps []completeness.RecognitionGap) (map[string][]uint32, []completeness.RecognitionGap) {
	recBySource := map[string][]uint32{}
	var unattributed []completeness.RecognitionGap
	for _, g := range gaps {
		owners := ownerOf[g.ContractID]
		if len(owners) == 0 {
			unattributed = append(unattributed, g)
			continue
		}
		// A gap on a shared contract caps every source that owns it: none
		// of them recognised the shape, and no one of them may read clean.
		for _, owner := range owners {
			recBySource[owner] = append(recBySource[owner], g.MinLedger)
		}
	}
	return recBySource, unattributed
}

// sourceRecognitionOK is the per-source Claim-2a verdict: fails on a gap at or after genesis. Under -skip-recognition it carries the prior
// verdict only if known, clean and covering this run's tip, else fails closed.
func sourceRecognitionOK(genesis, hi uint32, attributed []uint32, skipRecognition bool, prior priorProjection) (bool, []uint32) {
	ok := true
	var problems []uint32
	for _, l := range attributed {
		if l >= genesis {
			problems = append(problems, l)
			ok = false
		}
	}
	if !skipRecognition || !ok {
		return ok, problems
	}
	switch {
	case !prior.known:
		return false, append(problems, genesis)
	case !prior.ok:
		return false, append(problems, genesis)
	case hi > prior.tip:
		return false, append(problems, prior.tip+1)
	default:
		return true, nil
	}
}

// recognitionClaim states what this run knows about the recognition axis (mirrors substrateClaim/projectionClaim): recOK alone cannot tell
// a clean scan from -skip-recognition trusting the prior audit.
func recognitionClaim(recOK, skipRecognition bool) string {
	switch {
	case !recOK && skipRecognition:
		return "recognition: -skip-recognition and no clean prior recognition verdict covers this run's tip — refusing to carry recognition_ok=true without evidence (re-run without -skip-recognition)"
	case !recOK:
		return "recognition: unhandled topic on this source's contract(s)"
	case skipRecognition:
		return "recognition: carried from the prior audit (-skip-recognition), not re-proven this run"
	default:
		return "recognition: verified — every on-chain event shape recognized by a decoder"
	}
}

// projectionFoundProblem reports whether the CH aggregate reconcile FOUND a failure; it localises none to a ledger, so the snapshot
// carries this instead of FirstProblem. Unevaluated or scope-only failures are not a find.
func projectionFoundProblem(delta int, blind completeness.BlindSpots, floorLoss []string) bool {
	return delta != 0 || blind.Any() || len(floorLoss) > 0
}

// combineWatermark gates the lake watermark srW on projOK to give the served `complete` axis. It
// leaves srW.Complete, read separately as lake_complete, untouched (ADR-0033/0034).
func combineWatermark(srW completeness.Watermark, projOK bool) completeness.Watermark {
	w := srW
	w.Complete = srW.Complete && projOK
	return w
}

// projectionScope is the ledger range a Claim-2b reconcile covered for one target, so a run
// never publishes completeness over ledgers it did not reconcile (ADR-0033).
type projectionScope struct {
	From uint32 // inclusive
	To   uint32 // inclusive
}

// empty reports a degenerate scope, which reconcileTarget skips uncounted.
func (sc projectionScope) empty() bool { return sc.From > sc.To }

// targetScope derives one target's reconcile range from its own MIN(ledger) over [genesis, hi]; an empty target floors at genesis (fail
// closed). runFrom only raises it (projectionClaim handles that); reconciling below served rows invents gaps.
func targetScope(servedMin uint32, haveServedRows bool, genesis, runFrom, hi uint32) projectionScope {
	lo := servedMin
	if !haveServedRows || lo < genesis {
		lo = genesis
	}
	if runFrom > lo {
		lo = runFrom
	}
	if lo > hi {
		lo = hi + 1 // degenerate/empty scope; reconcileProjectionAggregate skips it
	}
	return projectionScope{From: lo, To: hi}
}

// projectionScopes resolves each target's reconcile scope (parallel to src.targets), servedFrom (bottom of the served axis's claim) and
// runFrom (this run's start); servedFrom < runFrom is what projectionClaim gates. One indexed MIN(ledger) per target.
func projectionScopes(ctx context.Context, store *timescale.Store, src reconSource, genesis, runFrom, hi uint32) (scopes []projectionScope, servedMins []servedFloor, servedFromOut, runFromOut uint32, err error) {
	if len(src.targets) == 0 {
		return nil, nil, genesis, genesis, nil
	}
	servedMins = make([]servedFloor, len(src.targets))
	for i, tgt := range src.targets {
		minL, ok, err := store.MinLedger(ctx, tgt.table, "ledger", tgt.whereFilter, genesis, hi)
		if err != nil {
			return nil, nil, 0, 0, fmt.Errorf("%s min served ledger: %w", tgt.table, err)
		}
		servedMins[i] = servedFloor{min: minL, present: ok}
	}
	scopes, servedFrom, runLo := scopesFromServed(src, servedMins, genesis, runFrom, hi)
	return scopes, servedMins, servedFrom, runLo, nil
}

// scopesFromServed is the pure half of projectionScopes. A source without a servedWindowReason scopes every target from genesis so a
// never-projected prefix reconciles as expected>0 vs served=0.
func scopesFromServed(src reconSource, servedMins []servedFloor, genesis, runFrom, hi uint32) (scopes []projectionScope, servedFrom, runFromOut uint32) {
	scopes = make([]projectionScope, len(servedMins))
	servedFrom, runLo := hi, hi
	for i, sm := range servedMins {
		if src.servedWindowReason == "" {
			sm = servedFloor{min: genesis, present: true}
		}
		if served := targetScope(sm.min, sm.present, genesis, 0, hi).From; served < servedFrom {
			servedFrom = served
		}
		scopes[i] = targetScope(sm.min, sm.present, genesis, runFrom, hi)
		if scopes[i].From < runLo {
			runLo = scopes[i].From
		}
	}
	return scopes, servedFrom, runLo
}

// servedFloor is one target's live MIN(ledger) over the reconcile window and whether it held anything; compared with the durable floor,
// which targetScope cannot do (it derives its floor from this value).
type servedFloor struct {
	min     uint32
	present bool
}

// detectFloorLoss returns a detail line per target whose oldest rows are missing versus the durable floor, which the served tier cannot
// rewrite. A target with no recorded floor is skipped (zero would flag all); an empty one that had a floor is reported.
func detectFloorLoss(src reconSource, servedMins []servedFloor, floors map[string]timescale.CompletenessTargetFloor) []string {
	var out []string
	for i, sm := range servedMins {
		if i >= len(src.targets) {
			break
		}
		tgt := src.targets[i]
		prior, ok := floors[timescale.TargetFloorKey(src.name, tgt.table, tgt.whereFilter)]
		if !ok {
			continue // no floor recorded yet — nothing to compare against
		}
		if !sm.present {
			out = append(out, fmt.Sprintf(
				"projection: %s holds NO rows but was previously verified from ledger %d — "+
					"the served tier lost everything below the recorded floor",
				tgt.table, prior.VerifiedFrom))
			continue
		}
		if sm.min > prior.VerifiedFrom {
			out = append(out, fmt.Sprintf(
				"projection: %s now begins at ledger %d but was previously verified from %d — "+
					"%d ledgers of served rows below the recorded floor are GONE (not a scope "+
					"change: no reconcile target has a retention policy)",
				tgt.table, sm.min, prior.VerifiedFrom, sm.min-prior.VerifiedFrom))
		}
	}
	return out
}

// floorsToRecord is the pure half of recordFloors: live MIN(ledger) per target, only when the scope reached the bottom edge (a clipped
// scan would bank a floor and disarm detectFloorLoss). Empty targets are skipped: LEAST() could never undo a recorded genesis.
func floorsToRecord(src reconSource, scopes []projectionScope, servedMins []servedFloor) []timescale.CompletenessTargetFloor {
	out := make([]timescale.CompletenessTargetFloor, 0, len(src.targets))
	for i, tgt := range src.targets {
		if i >= len(scopes) || i >= len(servedMins) {
			break
		}
		sm, from := servedMins[i], scopes[i].From //nolint:gosec // G602 false positive: the check above bounds i
		if !sm.present {
			continue
		}
		if from > sm.min {
			// Clipped by an incremental floor: nothing below the clip was verified.
			continue
		}
		// The floor is the target's live bottom edge, not a genesis-floored
		// scope start: detectFloorLoss reads MIN(ledger) > floor as loss.
		out = append(out, timescale.CompletenessTargetFloor{
			Source:       src.name,
			Table:        tgt.table,
			Filter:       tgt.whereFilter,
			VerifiedFrom: sm.min,
		})
	}
	return out
}

// recordFloors persists what this run verified, per floorsToRecord. Call it only after a clean
// reconcile, or a mismatch would enshrine an unverified range.
func recordFloors(ctx context.Context, store *timescale.Store, src reconSource, scopes []projectionScope, servedMins []servedFloor) error {
	for _, floor := range floorsToRecord(src, scopes, servedMins) {
		if err := store.UpsertCompletenessTargetFloor(ctx, floor); err != nil {
			return err
		}
	}
	return nil
}

// scopeUnion is the range the expected side must be re-derived over to serve
// every target's scope in one lake pass. Assumes a non-empty slice.
func scopeUnion(scopes []projectionScope) (uint32, uint32) {
	lo, hi := scopes[0].From, scopes[0].To
	for _, s := range scopes[1:] {
		if s.From < lo {
			lo = s.From
		}
		if s.To > hi {
			hi = s.To
		}
	}
	return lo, hi
}

// clipCounts returns a new map without the per-ledger counts outside sc. The expected side is
// derived once over the union of target scopes, so each target must clip to its own.
func clipCounts(m map[uint32]int, sc projectionScope) map[uint32]int {
	out := make(map[uint32]int, len(m))
	for ledger, n := range m {
		if ledger < sc.From || ledger > sc.To {
			continue
		}
		out[ledger] = n
	}
	return out
}

// priorProjection is what the last published verdict knows about one axis; known=false means no
// verdict was ever written.
type priorProjection struct {
	known bool
	ok    bool
	tip   uint32
	// evidencedAt is when the prior claim was last reconciled over the whole
	// served range (zero = unknown); a carry inherits it unchanged.
	evidencedAt time.Time
	// evidenceExpired: evidencedAt is older than -max-carry-age, so -pass
	// re-proves the source instead of carrying it again (projectionFloor).
	evidenceExpired bool
	// verifiedFrom is the prior verdict's projection_verified_from; 0 = not
	// recorded. A carry may only cover ground at or above it.
	verifiedFrom uint32
}

// maxEvidenceRefloorsPerPass caps how many expired carries one -pass re-proves from genesis, so
// expiry is staggered across nights.
const maxEvidenceRefloorsPerPass = 3

// expireStaleCarries marks, oldest first, at most maxEvidenceRefloorsPerPass sources whose clean claim was last fully reconciled over
// maxAge ago (or never), returning their names. Sources that outlast the pass are never marked (a deadline-cut re-floor repeats nightly);
// maxAge <= 0 disables.
func expireStaleCarries(prior map[string]priorProjection, catalogue []reconSource, now time.Time, maxAge time.Duration) []string {
	var expired []string
	for _, src := range catalogue {
		p := prior[src.name]
		if !src.outlastsPass() && p.known && p.ok && completeness.ProjectionEvidenceExpired(p.evidencedAt, now, maxAge) {
			expired = append(expired, src.name)
		}
	}
	// Zero (unknown) sorts before every real time; ties keep catalogue order.
	sort.SliceStable(expired, func(i, j int) bool {
		return prior[expired[i]].evidencedAt.Before(prior[expired[j]].evidencedAt)
	})
	if len(expired) > maxEvidenceRefloorsPerPass {
		expired = expired[:maxEvidenceRefloorsPerPass]
	}
	for _, name := range expired {
		p := prior[name]
		p.evidenceExpired = true
		prior[name] = p
	}
	return expired
}

// carriedEvidenceDetail names how old the evidence behind a carried claim is.
func carriedEvidenceDetail(evidencedAt time.Time) string {
	if evidencedAt.IsZero() {
		return "projection: the carried prefix has no full-range reconcile on record"
	}
	return "projection: the carried prefix was last reconciled in full at " + evidencedAt.UTC().Format(time.RFC3339)
}

// projectionEvidence is the evidence time a claim publishes: now for a full clean reconcile, the
// prior claim's time for a carried prefix, none for a false claim.
func projectionEvidence(projOK bool, servedFrom, runFrom uint32, prior priorProjection) (now bool, carried time.Time) {
	switch {
	case !projOK:
		return false, time.Time{}
	case runFrom <= servedFrom:
		return true, time.Time{}
	default:
		return false, prior.evidencedAt
	}
}

// buildPriorVerdicts turns the last snapshots into per-axis carry inputs plus each source's last watermark (the -pass floor). Projection
// is bounded by s.Watermark, not s.Tip: only substrate and recognition reconcile to the tip.
func buildPriorVerdicts(snaps []timescale.CompletenessSnapshot) (priorProj, priorSub, priorRec map[string]priorProjection, priorWatermark map[string]uint32) {
	priorProj = make(map[string]priorProjection, len(snaps))
	priorSub = make(map[string]priorProjection, len(snaps))
	priorRec = make(map[string]priorProjection, len(snaps))
	// priorWatermark is each source's last lake watermark, the per-source projection floor in
	// -pass mode; a source absent here (never seeded) floors at genesis.
	priorWatermark = make(map[string]uint32, len(snaps))
	for _, s := range snaps {
		priorProj[s.Source] = priorProjection{known: true, ok: s.ProjectionOK, tip: s.Watermark, verifiedFrom: s.ProjectionVerifiedFrom, evidencedAt: s.ProjectionEvidencedAt}
		priorWatermark[s.Source] = s.Watermark
		// Substrate and recognition need prior verdicts too: an incremental or skipped run
		// must not publish a genesis-to-tip claim off a suffix (see substrateClaim).
		priorSub[s.Source] = priorProjection{known: true, ok: s.SubstrateOK, tip: s.Tip}
		priorRec[s.Source] = priorProjection{known: true, ok: s.RecognitionOK, tip: s.Tip}
	}
	return priorProj, priorSub, priorRec, priorWatermark
}

// projectionClaim gates what a run may publish on the served `complete` axis: an incremental -from sits at tip, so complete=false could
// regress to true. Fail-closed: a mismatch fails; reconciling from <= servedFrom may publish true (the only way a failing verdict
// clears); a partial run carries a prior clean verdict only if contiguous (prior.tip+1 >= runFrom), reaching servedFrom, every present
// target's floor proven; else false.
func projectionClaim(servedFrom, runFrom, hi uint32, runClean bool, runDetail string, prior priorProjection, scope claimScope) (bool, string) {
	if !runClean {
		return false, "projection: " + runDetail
	}
	if runFrom <= servedFrom {
		return true, fmt.Sprintf("projection: verified [%d,%d] over the served range of the reconciled tables — %s", servedFrom, hi, scope.text)
	}
	skipped := fmt.Sprintf("[%d,%d]", servedFrom, runFrom-1)
	switch {
	case !prior.known:
		return false, fmt.Sprintf("projection: verified only [%d,%d]; %s was NOT reconciled by this run and no prior verdict exists to carry — not claiming it (re-run without -from)", runFrom, hi, skipped)
	case !prior.ok:
		return false, fmt.Sprintf("projection: verified only [%d,%d]; %s was NOT reconciled by this run and the prior verdict's projection was FAILING — refusing to upgrade without evidence (re-run without -from)", runFrom, hi, skipped)
	case runFrom > prior.tip+1:
		return false, fmt.Sprintf("projection: verified only [%d,%d]; the prior clean verdict only reached tip=%d, leaving [%d,%d] verified by nobody — not claiming it (re-run without -from)", runFrom, hi, prior.tip, prior.tip+1, runFrom-1)
	case prior.verifiedFrom > servedFrom:
		return false, fmt.Sprintf("projection: verified only [%d,%d]; the prior clean verdict only covered from ledger %d, leaving [%d,%d] verified by nobody — not claiming it (re-run without -from)", runFrom, hi, prior.verifiedFrom, servedFrom, prior.verifiedFrom-1)
	case len(scope.unproven) > 0:
		return false, fmt.Sprintf("projection: verified only [%d,%d]; carried prefix %s never reconciled %s — re-run without -from", runFrom, hi, skipped, strings.Join(scope.unproven, ", "))
	default:
		return true, fmt.Sprintf("projection: verified [%d,%d]; %s carried from the prior clean verdict (tip=%d), not re-verified this run — %s", runFrom, hi, skipped, prior.tip, scope.text)
	}
}

// projectionWithoutEvidence refuses a clean claim no served row stands behind: an empty expectation matching an empty served tier is what
// a wrong or redeployed contract identity produces. Returns vacuous=true with the reason.
func projectionWithoutEvidence(projOK bool, nTargets int, servedMins []servedFloor, genesis, hi uint32) (bool, string) {
	if !projOK {
		return false, ""
	}
	for _, m := range servedMins {
		if m.present {
			return false, ""
		}
	}
	return true, fmt.Sprintf("projection: no evidence — none of this source's %d target table(s) holds a row in [%d,%d], so the clean reconcile compared nothing with nothing; "+
		"an empty served tier matching an empty expectation is not a verification (check the catalogue's contract identities)", nTargets, genesis, hi)
}

// verdictPublisher is the slice of the store [publishSourceVerdict] needs.
type verdictPublisher interface {
	PublishCompletenessVerdict(ctx context.Context, snap timescale.CompletenessSnapshot, clearWindow *timescale.DirtyWindowClear) (timescale.VerdictPublication, error)
}

// publishSourceVerdict stores a verdict and, only if it was stored (the never-regress guard can reject a low -to run), discharges the
// replay-rewind window in the same transaction; otherwise the window survives to force a full re-reconcile.
func publishSourceVerdict(ctx context.Context, store verdictPublisher, snap timescale.CompletenessSnapshot, win timescale.ProjectionDirtyWindow, earned bool) (timescale.VerdictPublication, error) {
	var clearWindow *timescale.DirtyWindowClear
	if earned {
		clearWindow = &timescale.DirtyWindowClear{From: win.From, To: win.To, UpdatedAt: win.UpdatedAt}
	}
	return store.PublishCompletenessVerdict(ctx, snap, clearWindow)
}

// verdictNotStoredNote is the log suffix for a verdict the never-regress guard rejected, which
// would otherwise read like a stored one. Empty when the verdict was applied.
func verdictNotStoredNote(pub timescale.VerdictPublication, tip uint32, windowPending bool) string {
	if pub.Applied {
		return ""
	}
	note := fmt.Sprintf(" — VERDICT NOT STORED: this run's tip %d is below the stored verdict's tip and it found no problem, so the never-regress guard kept the stored verdict", tip)
	if windowPending {
		note += "; the replay-rewind window stays PENDING until a run at or above the stored tip re-verifies it"
	}
	return note
}

// substrateClaim gates the lake verdict on the scanned range (projectionClaim's twin): a clean suffix [scanFrom, hi] must not certify
// genesis-to-tip. Fail-closed: a problem fails; a scan from <= genesis may publish true (the only way a failing verdict clears); a
// partial run carries a prior clean verdict only if contiguous (prior.tip+1 >= scanFrom), as does scanFrom > hi (-skip-substrate); else
// false.
func substrateClaim(genesis, hi, scanFrom uint32, scanClean bool, problem uint32, prior priorProjection) (bool, string) {
	if !scanClean {
		return false, fmt.Sprintf("substrate: lake gap/break at %d", problem)
	}
	if scanFrom <= genesis {
		return true, fmt.Sprintf("substrate: verified [%d,%d] — contiguous + hash-chained from this source's genesis", genesis, hi)
	}
	scanned := fmt.Sprintf("[%d,%d]", scanFrom, hi)
	if scanFrom > hi {
		scanned = "nothing (-skip-substrate)"
	}
	skipped := fmt.Sprintf("[%d,%d]", genesis, scanFrom-1)
	switch {
	case !prior.known:
		return false, fmt.Sprintf("substrate: verified only %s; %s was NOT scanned by this run and no prior verdict exists to carry — not claiming it (re-run without -from)", scanned, skipped)
	case !prior.ok:
		return false, fmt.Sprintf("substrate: verified only %s; %s was NOT scanned by this run and the prior verdict's substrate was FAILING — refusing to upgrade without evidence (re-run without -from)", scanned, skipped)
	case scanFrom > prior.tip+1:
		return false, fmt.Sprintf("substrate: verified only %s; the prior clean verdict only reached tip=%d, leaving [%d,%d] verified by nobody — not claiming it (re-run without -from)", scanned, prior.tip, prior.tip+1, scanFrom-1)
	default:
		return true, fmt.Sprintf("substrate: verified %s; %s carried from the prior clean verdict (tip=%d), not re-scanned this run", scanned, skipped, prior.tip)
	}
}

// substrateFloorLoss is detectFloorLoss for the substrate axis: a carried prefix would let a dropped partition of the oldest ledgers leave
// lake_complete asserting a hash chain from genesis. floorHasProblem is a 1-ledger probe, fired only when a prefix is carried; it sees a
// rising bottom edge, not an interior hole.
func substrateFloorLoss(genesis, subScanFrom, floorProblem uint32, floorHasProblem bool) (uint32, bool, string) {
	if subScanFrom <= genesis || !floorHasProblem {
		return 0, false, ""
	}
	return floorProblem, true, fmt.Sprintf(
		"substrate: bottom-edge loss — genesis ledger %d is no longer present/intact in the lake, "+
			"but this run scanned only [%d,tip] and substrateClaim carried [%d,%d] as clean "+
			"(archive/DROP PARTITION below the carried floor) — re-run without -from to re-seed substrate from genesis",
		genesis, subScanFrom, genesis, subScanFrom-1)
}

// readsContractEvents reports whether the source reads stellar.contract_events; sdex and the
// event-less ContractCall sources (band, soroswap-router) read stellar.operations instead.
func (src reconSource) readsContractEvents() bool {
	return !src.census && src.callDec == nil
}

// eventCensusLoss fails the source at max(first short event partition's ledger, genesis) (so ComputeWatermark cannot drop it as below
// genesis). Substrate proves only stellar.ledgers, so a dropped event partition would otherwise leave lake_complete true.
func eventCensusLoss(src reconSource, genesis uint32, shortfalls []clickhouse.EventCensusShortfall) (uint32, bool, string) {
	if !src.readsContractEvents() {
		return 0, false, ""
	}
	for _, s := range shortfalls {
		if (uint64(s.Partition)+1)*1_000_000 <= uint64(genesis) {
			continue
		}
		p := max(s.FirstEventLedger, genesis)
		return p, true, fmt.Sprintf(
			"substrate: stellar.contract_events partition %d holds %d row(s) but stellar.ledgers records %d event(s) there (first at ledger %d) — "+
				"the event table recognition and projection read is short; neither lake_complete nor recognition is certifiable over it",
			s.Partition, s.Present, s.Expected, s.FirstEventLedger)
	}
	return 0, false, ""
}

// lakeCoverageProblem is substrateClaim's numeric twin: the ledger injected into the coverage watermark's problem set when the claim
// refuses a clean suffix scan, so coverage does not read full beside substrate_ok=false. 0 when the claim holds or the scan found a more
// precise problem; else prior.tip+1 (clean prior) or genesis.
func lakeCoverageProblem(genesis uint32, scanClean, substrateOK bool, prior priorProjection) uint32 {
	if substrateOK || !scanClean {
		return 0
	}
	floor := genesis
	if prior.ok && prior.tip+1 > floor {
		floor = prior.tip + 1
	}
	return floor
}

// reconcileProjectionAggregate is the ClickHouse projection check (ADR-0033 Claim 2b), strict per ledger (totals let a drop net against a
// phantom overcount); sources with mismatched keying opt out via reconSource.aggregate. Returns summed deltas (0 = clean).
func reconcileProjectionAggregate(ctx context.Context, store *timescale.Store, chStreamer completeness.EventStreamer, chAddr string, src reconSource, scopes []projectionScope) (int, completeness.BlindSpots, string, error) {
	if len(scopes) == 0 {
		return 0, completeness.BlindSpots{}, "", nil
	}
	lo, hi := scopeUnion(scopes)
	if lo > hi {
		return 0, completeness.BlindSpots{}, "", nil // every target's scope is empty
	}
	expectedFor, blind, eerr := expectedProjection(ctx, chStreamer, chAddr, src, lo, hi)
	if eerr != nil {
		return 0, completeness.BlindSpots{}, "", eerr
	}
	var totalDelta int
	var details []string
	// Blindness goes first: it explains why a zero delta is not evidence.
	if d := blind.Detail(); d != "" {
		details = append(details, d)
	}
	for i, tgt := range src.targets {
		d, detail, terr := reconcileTarget(ctx, store, src, tgt, expectedFor(tgt), scopes[i])
		if terr != nil {
			return 0, completeness.BlindSpots{}, "", terr
		}
		if d != 0 {
			totalDelta += d
			details = append(details, detail)
		}
	}
	return totalDelta, blind, strings.Join(details, "; "), nil
}

// expectedProjection re-derives Claim 2b's expected side once over [lo, hi]: a per-target accessor plus blind spots. A ContractCall whose
// Decode errors is missing from census and writer alike and nets to zero; the SDEX census soft-fails per claim.
func expectedProjection(ctx context.Context, chStreamer completeness.EventStreamer, chAddr string, src reconSource, lo, hi uint32) (func(reconTarget) map[uint32]int, completeness.BlindSpots, error) {
	switch {
	case src.callDec != nil:
		// Event-less ContractCall source: census from the lake's InvokeContract ops, decoded by
		// the same decoder the live dispatcher routes.
		expected, blind, err := reDeriveContractCallCensus(ctx, chAddr, src.callContract, src.callDec, lo, hi)
		if err != nil {
			return nil, completeness.BlindSpots{}, err
		}
		return func(reconTarget) map[uint32]int { return expected }, blind, nil

	case src.census:
		// Run the SDEX decoder over the lake's operations and count its trades, the same decode
		// live ingest applies. The residual is then only ops the served tier dropped, and the
		// lake is an independent source, so this cannot pass by construction.
		expected, blind, err := reDeriveSDEXCensusViaDecoder(ctx, chAddr, lo, hi)
		if err != nil {
			return nil, completeness.BlindSpots{}, err
		}
		return func(reconTarget) map[uint32]int { return expected }, blind, nil

	default:
		// Factory-anchored sources (ADR-0035): seed the gate registry from creation events in
		// [genesis, lo) so earlier children are not dropped. A static seed goes stale as pools
		// deploy and would report complete=false where the live decoder captures them.
		var walkBlind completeness.BlindSpots
		if len(src.factories) > 0 {
			pb, err := preseedFactoryChildren(ctx, chStreamer, src, lo)
			if err != nil {
				return nil, completeness.BlindSpots{}, fmt.Errorf("%s preseed: %w", src.name, err)
			}
			walkBlind = pb
		}
		// Identity-gated sources use a contract-id prefilter so the re-derive skips the whole
		// event lake, which would overrun the -pass deadline. It is built after the preseed so it
		// is a superset of the registry at every point; Matches() stays the per-event gate.
		contractIDs := src.contractIDs
		if src.newGatedDec != nil {
			pf, pfBlind, pferr := gatedPrefilter(ctx, chStreamer, src, hi)
			if pferr != nil {
				return nil, completeness.BlindSpots{}, fmt.Errorf("%s gated prefilter: %w", src.name, pferr)
			}
			contractIDs = pf
			walkBlind = walkBlind.Merge(pfBlind)
		}
		byKind, blind, err := completeness.ReDeriveOutputCountsByKindFromEvents(ctx, chStreamer, src.dec, contractIDs, src.topic0Syms, lo, hi)
		if err != nil {
			return nil, completeness.BlindSpots{}, err
		}
		blind = blind.Merge(walkBlind)
		// Each table receives only the kinds it is registered for; counting
		// every output would overcount any table that receives a subset.
		return func(tgt reconTarget) map[uint32]int { return completeness.SumKinds(byKind, tgt.kinds...) }, blind, nil
	}
}

// gatedPrefilter builds the contract-id prefilter for a factory-gated source: a superset of what the decoder accepts in [lo,hi]
// (Matches() stays the gate). It is the gate preseeded to lo (GatedContractSet) plus every child announced through hi, walked on a
// throwaway decoder; factory ids always included. The walk must match String topics as well as Symbols, and runs under completeness.Guard
// so a panic becomes a blind spot.
func gatedPrefilter(ctx context.Context, chStreamer completeness.EventStreamer, src reconSource, hi uint32) ([]string, completeness.BlindSpots, error) {
	set := make(map[string]struct{})
	blind := completeness.NewBlindTracker()
	// (a) the registry the real re-derive starts the stream with.
	if g, ok := src.dec.(gatedContractSetter); ok {
		for _, c := range g.GatedContractSet() {
			set[c] = struct{}{}
		}
	}
	// (b) children announced through hi, on a throwaway decoder so src.dec is untouched.
	fresh := src.newGatedDec()
	for _, f := range src.factories {
		set[f] = struct{}{} // ensure the factory streams so its add_pool self-seeds
		if len(src.creationSym) == 0 {
			continue
		}
		werr := chStreamer.StreamContractEvents(ctx, src.genesis, hi, []string{f}, []string{src.creationSym},
			func(ev events.Event) error {
				if perr := completeness.Guard(func() {
					if fresh.Matches(ev) {
						// Decode registers the announced child in `fresh`'s registry; an error on a
						// malformed creation event is soft-failed like the re-derive, so the walk
						// stays non-fatal.
						_, _ = fresh.Decode(ev)
					}
				}); perr != nil {
					blind.Undecodable(ev.Ledger)
				}
				return nil
			})
		if werr != nil {
			return nil, completeness.BlindSpots{}, fmt.Errorf("walk %s creation events: %w", src.name, werr)
		}
	}
	for _, c := range fresh.GatedContractSet() {
		set[c] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	if len(out) > maxGatedPrefilterIDs {
		// The IN list is inlined into the SQL and would overrun ClickHouse's
		// max_query_size (256 KiB); unscoped, Matches() still gates every event.
		return nil, blind.Result(), nil
	}
	sort.Strings(out)
	return out, blind.Result(), nil
}

// maxGatedPrefilterIDs keeps the inlined contract_id list well under 256 KiB (~60 bytes each).
var maxGatedPrefilterIDs = 2000

// gatedContractSetter is the read-only half of gatedDecoder: a decoder that can enumerate its gate.
type gatedContractSetter interface {
	GatedContractSet() []string
}

// reconcileTarget compares one target's re-derived expected counts (clipped to
// the target's own scope) against its served counts over that same scope.
func reconcileTarget(ctx context.Context, store *timescale.Store, src reconSource, tgt reconTarget, expected map[uint32]int, sc projectionScope) (int, string, error) {
	if sc.empty() {
		return 0, "", nil
	}
	actual, err := store.CountRowsByLedger(ctx, tgt.table, "ledger", tgt.countFilter(), sc.From, sc.To)
	if err != nil {
		return 0, "", err
	}
	d, detail := projectionDelta(src, tgt.table, clipCounts(expected, sc), actual, sc.From, sc.To)
	return d, detail, nil
}

// reDeriveSDEXCensusViaDecoder re-derives the expected SDEX trade count per ledger via the decoder, counting distinct valid priceable
// trades (served rows upsert, so a colliding op_index overwrites). One-side-zero fills are excluded here and from the served count
// (countFilter). Read-only.
func reDeriveSDEXCensusViaDecoder(ctx context.Context, chAddr string, from, to uint32) (map[uint32]int, completeness.BlindSpots, error) {
	return reDeriveSDEXCensus(ctx, func(ctx context.Context, lo, hi uint32, fn func(clickhouse.SDEXOp) error) error {
		return clickhouse.StreamSDEXOps(ctx, chAddr, lo, hi, fn)
	}, from, to)
}

// sdexCensusWindow bounds the per-window join input; 100k windows ran out of memory.
const sdexCensusWindow = 25_000

// sdexCensusWorkers windows stream at once: ops_batch caps each query at 2 threads, so one window at a time left the weekly
// re-proof CH-bound past its 360m budget. 3 x ~4 GiB stays inside the profile's 8 concurrent queries and memory default.
const sdexCensusWorkers = 3

func reDeriveSDEXCensus(ctx context.Context, stream func(context.Context, uint32, uint32, func(clickhouse.SDEXOp) error) error, from, to uint32) (map[uint32]int, completeness.BlindSpots, error) {
	out := make(map[uint32]int)
	blind := completeness.NewBlindTracker()
	var mu sync.Mutex
	dec := sdex.NewDecoder()
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(sdexCensusWorkers)
	for lo := uint64(from); lo <= uint64(to); lo += sdexCensusWindow {
		wlo, whi := uint32(lo), uint32(min(lo+sdexCensusWindow-1, uint64(to)))
		g.Go(func() error {
			seen := sdexServedCensus{}
			var undecodable []uint32
			if err := stream(gctx, wlo, whi, func(op clickhouse.SDEXOp) error {
				// SDEX Decode soft-fails per claim, never with an error; DecodeCounted also reports
				// claim atoms that failed to decode so the ledger is marked BLIND instead of silently
				// reading as zero trades. Both-zero no-op claims are a symmetric drop, not a failure.
				var outs []consumer.Event
				var failed int
				if perr := completeness.Guard(func() {
					outs, failed = dec.DecodeCounted(dispatcher.OpContext{
						Ledger:   op.Ledger,
						ClosedAt: op.ClosedAt,
						TxHash:   op.TxHash,
						TxSource: op.Source,
						OpIndex:  int(op.OpIndex),
						Op:       op.Op,
						OpResult: op.OpResult,
					})
				}); perr != nil {
					// The dispatcher recovers this panic and skips the op; the
					// census counts it blind instead of crashing the audit.
					undecodable = append(undecodable, op.Ledger)
					return nil //nolint:nilerr // intentional: the op is counted blind via blind.Undecodable, not aborted
				}
				for i := 0; i < failed; i++ {
					undecodable = append(undecodable, op.Ledger)
				}
				seen.add(outs)
				return nil
			}); err != nil {
				return err
			}
			mu.Lock()
			defer mu.Unlock()
			seen.addTo(out)
			for _, l := range undecodable {
				blind.Undecodable(l)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, completeness.BlindSpots{}, err
	}
	return out, blind.Result(), nil
}

// contractCallRowID is a ContractCall event's served-row identity (table PK minus per-ledger constants); the auth tree repeats a call
// across CallPaths. soroswap-router PK: (..., tx_hash, op_index, call_sig), ts unused. band PK: (..., tx_hash, op_index, ts), op_index
// fanned per feed, callSig unused.
type contractCallRowID struct {
	tx      string
	op      uint32
	ts      int64
	callSig string
}

// contractCallRowIdentity returns the served-row identity for a ContractCall source event;
// ok=false (defensive) for an event type not routed through one.
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

// reDeriveContractCallCensus re-derives expected rows per ledger for an event-less ContractCall source, counting distinct
// contractCallRowID so auth-tree duplicates the upsert collapses are no gap. Shares forEachContractCallEvent with the ch-rebuild writer
// (zero-delta re-verify).
func reDeriveContractCallCensus(ctx context.Context, chAddr, contractStrkey string, dec dispatcher.ContractCallDecoder, from, to uint32) (map[uint32]int, completeness.BlindSpots, error) {
	seen := make(map[uint32]map[contractCallRowID]struct{})
	blind, err := forEachContractCallEvent(ctx, chAddr, contractStrkey, dec, from, to, func(ledger uint32, ev consumer.Event) error {
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
		return nil, completeness.BlindSpots{}, err
	}
	out := make(map[uint32]int, len(seen))
	for ledger, ids := range seen {
		out[ledger] = len(ids)
	}
	return out, blind, nil
}

// forEachContractCallEvent streams the lake's InvokeContract ops for contractStrkey, runs dec over each op's auth-tree calls (as the live
// dispatcher does) and calls fn per event. The single decode path shared with the ch-rebuild writer. Windowed to bound the tx IN-set.
//
// linear pipeline; splitting hurts the read.
//
//nolint:gocognit // windowed stream → per-op call-tree → Matches/Decode is a
func forEachContractCallEvent(ctx context.Context, chAddr, contractStrkey string, dec dispatcher.ContractCallDecoder, from, to uint32, fn func(ledger uint32, ev consumer.Event) error) (completeness.BlindSpots, error) {
	blind := completeness.NewBlindTracker()
	raw, err := strkey.Decode(strkey.VersionByteContract, contractStrkey)
	if err != nil {
		return completeness.BlindSpots{}, fmt.Errorf("decode contract strkey %s: %w", contractStrkey, err)
	}
	contractHex := hex.EncodeToString(raw)
	const window = 250_000
	for lo := from; lo <= to; lo += window {
		hi := lo + window - 1
		if hi > to {
			hi = to
		}
		if err := clickhouse.StreamContractCallOps(ctx, chAddr, contractHex, lo, hi, func(op clickhouse.ContractCallOp) error {
			return decodeContractCallTree(op, dispatcher.ExtractContractCallTree(op.Op), dec, blind, fn)
		}); err != nil {
			return completeness.BlindSpots{}, err
		}
		if hi == to {
			break
		}
	}
	return blind.Result(), nil
}

// decodeContractCallTree decodes every call in one op's tree that dec owns, recording decode failures on blind. The ch-rebuild writer
// shares it, so a malformed call is missing from served tier and census alike and nets to zero; hence the skip is counted.
func decodeContractCallTree(
	op clickhouse.ContractCallOp,
	calls []dispatcher.ContractCall,
	dec dispatcher.ContractCallDecoder,
	blind *completeness.BlindTracker,
	fn func(ledger uint32, ev consumer.Event) error,
) error {
	for _, call := range calls {
		evs, matched, derr := decodeContractCall(op, call, dec)
		if !matched {
			continue
		}
		if derr != nil {
			blind.Undecodable(op.Ledger)
			continue
		}
		for _, ev := range evs {
			if ferr := fn(op.Ledger, ev); ferr != nil {
				return ferr
			}
		}
	}
	return nil
}

// decodeContractCall runs dec over one call under completeness.Guard. A panic in Matches or
// Decode comes back as a decode error, so the caller records it blind as the live path skips it.
func decodeContractCall(op clickhouse.ContractCallOp, call dispatcher.ContractCall, dec dispatcher.ContractCallDecoder) (evs []consumer.Event, matched bool, err error) {
	if perr := completeness.Guard(func() {
		if matched = dec.Matches(call.ContractID, call.FunctionName); !matched {
			return
		}
		// The live dispatcher refuses these before Decode; mirror it so the census and
		// ch-rebuild neither expect nor write a row the served tier never gets.
		if dispatcher.RefusesUncorroborated(dec, call.ExecutionCorroborated) {
			matched = false
			return
		}
		evs, err = dec.Decode(dispatcher.ContractCallContext{
			Ledger:                op.Ledger,
			ClosedAt:              op.ClosedAt,
			TxHash:                op.TxHash,
			TxSource:              op.Source,
			OpSource:              op.Source,
			OpIndex:               int(op.OpIndex),
			ContractID:            call.ContractID,
			FunctionName:          call.FunctionName,
			Args:                  call.Args,
			CallPath:              call.CallPath,
			CallPathContracts:     call.CallPathContracts,
			AuthOccurrence:        call.AuthOccurrence,
			ExecutionCorroborated: call.ExecutionCorroborated,
		})
	}); perr != nil {
		return nil, true, fmt.Errorf("decoder panicked: %w", perr)
	}
	return evs, matched, err
}

func absDiff(a, b int) int {
	if a > b {
		return a - b
	}
	return b - a
}

// projectionDelta compares one target's expected and served counts strictly per ledger (totals let a drop net against a phantom). An
// aggregate waiver nets only up to its boundary. Returns summed deltas (0 = clean) and a detail.
func projectionDelta(src reconSource, table string, expected, actual map[uint32]int, lo, hi uint32) (int, string) {
	w := src.aggregate
	switch {
	case w == nil:
		return strictPerLedgerDelta(table, expected, actual, lo, hi)
	case w.boundary == 0:
		d, detail := strictPerLedgerDelta(table, expected, actual, lo, hi)
		if d != 0 {
			detail += " (aggregate waiver without boundary — reconciled strict)"
		}
		return d, detail
	case hi <= w.boundary:
		// The whole window is pre-boundary vintage: accept the netting residual.
		return aggregateDelta(src, table, expected, actual, lo, hi)
	case lo > w.boundary:
		// The whole window is post-boundary: the served ledger keys 1:1 with
		// the re-derive, so reconcile strict per-ledger.
		return strictPerLedgerDelta(table, expected, actual, lo, hi)
	default:
		// The window straddles the boundary: aggregate up to it, strict above, so a real
		// post-boundary drop cannot net against a pre-boundary phantom.
		b := w.boundary
		preD, preDetail := aggregateDelta(src, table,
			countsAtOrBelow(expected, b), countsAtOrBelow(actual, b), lo, b)
		postD, postDetail := strictPerLedgerDelta(table,
			countsAbove(expected, b), countsAbove(actual, b), b+1, hi)
		return combineVintageDelta(table, b, preD, preDetail, postD, postDetail)
	}
}

// aggregateDelta compares window totals (the netting compare) for an aggregate-waiver source's
// pre-boundary span, where the served ledger can legitimately differ from the event ledger.
func aggregateDelta(src reconSource, table string, expected, actual map[uint32]int, lo, hi uint32) (int, string) {
	e, a := sumCounts(expected), sumCounts(actual)
	if d := absDiff(e, a); d != 0 {
		return d, fmt.Sprintf("%s: expected=%d served=%d Δ=%d [%d,%d] (aggregate compare — %s)",
			table, e, a, d, lo, hi, src.aggregate.reason)
	}
	return 0, ""
}

// strictPerLedgerDelta is the default per-ledger reconcile: a real
// drop in one ledger cannot net against a phantom in another.
func strictPerLedgerDelta(table string, expected, actual map[uint32]int, lo, hi uint32) (int, string) {
	gaps := completeness.ReconcileCounts(expected, actual)
	if len(gaps) == 0 {
		return 0, ""
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].Ledger < gaps[j].Ledger })
	delta := 0
	for _, g := range gaps {
		delta += absDiff(g.Expected, g.Actual)
	}
	// The stored detail names only the first mismatched ledger to stay short. SI_DUMP_GAPS also
	// streams every gap to stderr so a targeted purge can be built; it changes no verdict.
	if os.Getenv("SI_DUMP_GAPS") != "" {
		for _, g := range gaps {
			fmt.Fprintf(os.Stderr, "GAP\t%s\tledger=%d\texpected=%d\tserved=%d\tdelta=%+d\n",
				table, g.Ledger, g.Expected, g.Actual, g.Actual-g.Expected)
		}
	}
	first := gaps[0]
	return delta, fmt.Sprintf("%s: %d mismatched ledger(s), Σ|Δ|=%d, first: ledger=%d expected=%d served=%d [%d,%d]",
		table, len(gaps), delta, first.Ledger, first.Expected, first.Actual, lo, hi)
}

// combineVintageDelta merges the pre-boundary aggregate and post-boundary strict deltas. The
// total drives the verdict; the detail leads with the strict signal, the real gap.
func combineVintageDelta(table string, boundary uint32, preD int, preDetail string, postD int, postDetail string) (int, string) {
	total := preD + postD
	switch {
	case total == 0:
		return 0, ""
	case preD == 0:
		return total, "post-vintage strict — " + postDetail
	case postD == 0:
		return total, fmt.Sprintf("pre-vintage aggregate (≤%d, accepted netting) — %s", boundary, preDetail)
	default:
		return total, fmt.Sprintf("%s: vintage-split@%d — post-vintage strict Σ|Δ|=%d [%s]; pre-vintage aggregate Δ=%d [%s]",
			table, boundary, postD, postDetail, preD, preDetail)
	}
}

// countsAtOrBelow / countsAbove partition a per-ledger count map at a boundary
// ledger (inclusive below) for the vintage split.
func countsAtOrBelow(m map[uint32]int, b uint32) map[uint32]int {
	out := make(map[uint32]int, len(m))
	for l, c := range m {
		if l <= b {
			out[l] = c
		}
	}
	return out
}

func countsAbove(m map[uint32]int, b uint32) map[uint32]int {
	out := make(map[uint32]int, len(m))
	for l, c := range m {
		if l > b {
			out[l] = c
		}
	}
	return out
}

// recognitionGlobalExcludeSyms must be FirehoseExcludeSyms, not the wider ClassicTokenTopic0Syms, which would hide the Blend/Comet
// set_admin collision on the shared "POOL" topic. A var so tests can check the wiring.
var recognitionGlobalExcludeSyms = clickhouse.FirehoseExcludeSyms

// computeRecognitionGapsCH is the ClickHouse recognition audit: distinct (contract, topic) shapes from the lake through the dispatcher's
// Recognize(). The scan excludes FirehoseExcludeSyms except on the watched SEP-41 contracts (recognised because buildCensusDispatcher
// registers sep41 decoders), so a watched source dropping its own event kinds still gaps.
func computeRecognitionGapsCH(ctx context.Context, cfg config.Config, chAddr string, gated map[string][]contractid.Option, from, tip uint32, soroswapOpts ...soroswap.DecoderOption) ([]completeness.RecognitionGap, error) {
	disp, err := buildCensusDispatcher(cfg, gated, soroswapOpts...)
	if err != nil {
		return nil, err
	}
	shapes, err := clickhouse.DistinctTopicShapes(ctx, chAddr, from, tip, recognitionGlobalExcludeSyms, cfg.Supply.WatchedSEP41Contracts)
	if err != nil {
		return nil, err
	}
	if verr := recognitionScanEmptyErr(len(shapes), from, tip); verr != nil {
		return nil, verr
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

// buildCensusDispatcher builds the census decoder chain as the indexer does: the enabled sources
// plus the watched-set-gated sep41 event decoders, which BuildDispatcher alone does not register.
func buildCensusDispatcher(cfg config.Config, gated map[string][]contractid.Option, soroswapOpts ...soroswap.DecoderOption) (*dispatcher.Dispatcher, error) {
	disp, err := pipeline.BuildDispatcher(cfg.Ingestion.EnabledSources, cfg.Oracle, gated, soroswapOpts...)
	if err != nil {
		return nil, fmt.Errorf("build dispatcher: %w", err)
	}
	if _, err := pipeline.RegisterSupplyEventDecoders(disp, cfg.Supply); err != nil {
		return nil, fmt.Errorf("register supply event decoders: %w", err)
	}
	return disp, nil
}

// recognitionScanEmptyErr fails closed on a scan that read zero shapes (no samples would read as clean); verify-recognition refuses the
// same vacuous pass. Reachable for any pre-Soroban range.
func recognitionScanEmptyErr(nShapes int, from, tip uint32) error {
	if nShapes > 0 {
		return nil
	}
	return fmt.Errorf("recognition scan read 0 event shapes in ledgers [%d, %d] — "+
		"the source read nothing for this range; refusing to certify recognition coverage vacuously",
		from, tip)
}

func nilOrOne(v uint32) []uint32 {
	if v == 0 {
		return nil
	}
	return []uint32{v}
}
