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
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

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

// sorobanEraFloor is the network's first Soroban ledger — the lower bound for
// the global recognition scan and event census. A test net starts at 1, so the
// pubnet boundary would invert the range; zero (no defaults applied) is pubnet.
func sorobanEraFloor(cfg config.Config) uint32 {
	if cfg.Stellar.SorobanGenesisLedger == 0 {
		return clickhouse.SorobanGenesisLedger
	}
	return cfg.Stellar.SorobanGenesisLedger
}

// sourceSubstrateOK is the per-source Claim-1 verdict from a whole-range
// SubstrateProblem result: a source is substrate-OK iff there is no problem, or
// the reported problem ledger is strictly below the source's genesis (the
// break/absence lies before the source's own data). Correctness depends on
// SubstrateProblem returning a COVERAGE-correct problem ledger — an empty or
// tail-truncated lake returns the range tip, so `problem < genesis` is false for
// every source and none can green itself on an absent lake (a point value
// like `from` would let soroswap, genesis 50.7M, read `2 < 50.7M = true` on
// an EMPTY lake). Pure — unit-testable.
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

// substrateScanner performs a [from,to] CH substrate query. It is the seam
// clickhouse.SubstrateProblem is exposed through so substrateForGenesis is
// unit-testable without a live lake.
type substrateScanner func(ctx context.Context, from, to uint32) (problem uint32, hasProblem bool, detail string, err error)

// substrateForGenesis returns the CH substrate verdict for one source's own
// [max(floor,genesis), tip], scanning once per distinct floor (memoised in
// cache). One shared scan at the global floor would be wrong two ways:
// SubstrateProblem stops at the first problem, so a hole below a
// high-genesis source hides a later hole inside its range; and its head guard
// fires on truncation below a low-genesis source and skips the contiguity walk
// for every source. floor > tip (-skip-substrate) returns the zero value
// without scanning.
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
// no point re-deriving where an earlier claim already failed. Its LOWER
// bound is derived from the served tier's own data, per target
// (projectionScopes) — never a hardcoded retention guess — and the range
// it actually covered is stated in the verdict detail, so `complete=true`
// is a claim about exactly what was reconciled and nothing more
// (see targetScope and projectionClaim).
//
// Exit status reports whether the pass ran, not the verdict: an incomplete
// verdict alerts via stellarindex_completeness_incomplete. A per-source error
// does not stop the pass: that source keeps its prior verdict, every other
// source is still evaluated, and the run exits non-zero naming each failure
// (evaluateEachSource).
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
	pass := fs.Bool("pass", false, "WHOLE-PASS mode: prove recognition + substrate ONCE at full range for the ENTIRE source catalogue, then reconcile EACH source's projection incrementally from its own prior watermark (projectionFloor). One process replaces the per-source, per-chunk driver that re-ran the load-heaviest global recognition scan (DistinctTopicShapes, ~60s) on EVERY 25k chunk — the nightly timeout that froze the alphabetical tail's verdicts. Because substrate is proven at FULL tip, its write advances the tip and is never blocked by the CS-083 guard (the low-tip substrate flap). Every catalogue source gets a verdict — a never-seeded source, and a source whose prior projection verdict was FAILING, both reconcile from genesis (a red source has no verified ground to resume from, so the pass re-verifies it rather than carrying the failure forward forever). Requires -ch; incompatible with -source / -from / -skip-substrate / -skip-recognition (the per-chunk knobs it replaces).")
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
	// Fail CLOSED on -pass combined with the per-source / per-chunk knobs it
	// replaces (validatePassFlags): a partial pass that publishes over a subset
	// of sources, or carries a stale substrate/recognition verdict, would look
	// complete while re-opening exactly the redundancy + flap this mode fixes.
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

	// sep41_transfers/sep41_supply are promoted into this catalogue by
	// buildReconciliationCatalogue itself
	// whenever [supply] watched_sep41_contracts is configured —
	// see its doc comment.
	catalogue, soroswapDec, err := buildReconciliationCatalogue(cfg)
	if err != nil {
		return fmt.Errorf("compute-completeness: reconciliation catalogue: %w", err)
	}
	// Fail CLOSED on an unknown -source before the per-source loop, which
	// would otherwise skip every source and report SUCCESS having verified
	// nothing.
	if verr := validateSourceFilter(*only, catalogue); verr != nil {
		return fmt.Errorf("compute-completeness: %w", verr)
	}

	// Warm the factory-anchored gated registries (ADR-0035) so the
	// recognition dispatcher correctly recognizes real protocol children
	// (registered pools/vaults) and correctly flags FOREIGN emitters of
	// the same topic as gaps. Read-only (withHook=false) — the audit must
	// not mutate the registry. Depends on protocol_contracts being seeded
	// (`stellarindex-ops seed-protocol-contracts`); an empty table would
	// surface every real child shape as a false gap.
	//
	// Warmed HERE, ahead of every reader of the catalogue, because the
	// same options also gate the PROJECTION side below.
	gatedOpts, gerr := pipeline.GatedRegistryOptions(ctx, store, slog.Default(), ctx, false)
	if gerr != nil {
		return fmt.Errorf("gated registry warm: %w", gerr)
	}
	// Re-derive the EXPECTED side on the gate the live indexer runs with —
	// curated set ∪ protocol_contracts — not on the bare in-code seed.
	// buildReconciliationCatalogue takes only a config, so it
	// can only build each gated decoder bare; a contract an operator
	// admitted through protocol_contracts would otherwise be decoded live and
	// produce served rows that no expected side could account for, which
	// this command would publish as a projection mismatch — phantom rows on
	// the public /v1/coverage. Must precede the ownerOf attribution below
	// (it reads src.contractIDs) and every per-source re-derive, and the
	// preseed inside them, which seeds INTO the decoders this rebuilds.
	if catalogue, err = applyGatedOptions(catalogue, gatedOpts); err != nil {
		return fmt.Errorf("compute-completeness: %w", err)
	}

	if *only == "" || *only == "soroswap" {
		// Fail CLOSED, like every other pre-loop input: a failed or
		// partial seed publishes a false projection red for soroswap. Returning
		// writes NO snapshot, so the last real verdict stands. A DISABLED seed
		// (no factory configured) is not a failure — see seedSoroswapForRecon.
		if serr := seedSoroswapForRecon(ctx, cfg, soroswapDec); serr != nil {
			return fmt.Errorf("compute-completeness: soroswap pair seed: %w", serr)
		}
	}

	// The CH lake event source for projection re-derive (ADR-0034) is built
	// PER SOURCE inside the loop below: the wide op_args_xdr column is read
	// only for sources whose decoder consumes events.Event.OpArgs (redstone).

	// ── Recognition (Claim 2a): one global scan, attributed per source ──
	//
	// FAIL CLOSED: a scan error must abort the run. Logging it and continuing
	// with an empty recGaps — indistinguishable from a clean "no gaps" scan —
	// would let the per-source loop below read recognition_ok=true and (with
	// substrate ∧ projection clean) write lake_complete=true / complete=true: a
	// FALSE "complete" verdict on the public /v1/coverage. A transient CH
	// fault / query timeout / the DistinctTopicShapes memory-cap hit (the
	// load-heaviest step — see -skip-recognition) would launder into a
	// passing trust verdict. runRecognitionScan returns the error instead;
	// returning it here writes NO snapshot (the last real verdict stands) and
	// gives the cron a non-zero exit. A genuine INCOMPLETE (a scan that
	// SUCCEEDS and finds a real gap) still flows through as recGaps below,
	// keeping the recognition_ok=false / complete=false behavior.
	if *skipRecognition {
		fmt.Fprintln(os.Stderr, "compute-completeness: -skip-recognition — trusting prior recognition audit (no shape scan)")
	}
	// Seed the recognition census's soroswap decoder from the SAME pair
	// registry attributeRecognitionGaps folds into ownerOf (loadRegistryOwners
	// → LoadSoroswapPairRegistry). Without this the census dispatcher's soroswap
	// decoder starts with an EMPTY pairTokens map, so Matches() rejects every
	// real SoroswapPair protocol event (swap/sync/deposit/withdraw/skim — a
	// String topic[0], so topic_0_sym=""): each becomes a false "unhandled
	// topic" gap attributed to soroswap even though the indexer decodes + serves
	// them (they ARE the trades). "Attribution knows a pair the census decoder
	// does not" also cascade-clamps soroswap's projection
	// watermark to first_problem-1, producing spurious floor-loss alarms.
	// Matches() needs only key presence, so empty PairTokens suffice (the census
	// only Recognize()s, never Decode()s). FAIL CLOSED on the load error — a
	// partial registry would silently under-recognize. Skipped under
	// -skip-recognition (no census runs).
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
		// Recognition (Claim 2a) is a FULL-HISTORY property — "is every
		// topic shape a gated contract has EVER emitted recognized by some
		// decoder" — NOT an incremental one. It must NOT be scoped to the
		// incremental -from window: a low-volume source's rare wrong-topic
		// event (rozo emitted 393 payment_events over ~2 months, none in a
		// recent incremental window) would slip through and never flip
		// recognition_ok (the rozo blind spot).
		// DistinctTopicShapes is bounded-memory at any lake size (windowed
		// per-partition narrow-column scan + batched exemplar fetch — the
		// single-query argMax-over-wide-XDR form died at any server memory
		// cap once P23/CAP-67 grew the distinct set), so always scan from
		// genesis regardless of -from — which correctly scopes only the
		// expensive row-by-row projection reconcile below.
		return computeRecognitionGapsCH(ctx, cfg, *chAddr, gatedOpts, sorobanFloor, tip, soroswapOpts...)
	})
	if recErr != nil {
		return recErr
	}
	ownerOf := contractOwners(catalogue)
	// The TOPIC-MATCHED sources (soroswap/aquarius/phoenix/comet/defindex/
	// blend — empty contractIDs) have no static ownerOf entries, so without
	// this fold a dropped/altered topic on one of THEIR pools (the phoenix
	// orphaned-swap class) falls into `unattributed` and their per-source
	// recognition axis is STRUCTURALLY unable to fail: recBySource[soroswap]
	// stays empty and recognition_ok reads true over a real drop. Their pool
	// membership already exists in the same registries the recognition gate
	// warms — protocol_contracts (factory-anchored children) and soroswap_pairs
	// — so fold those into ownerOf. A gap on a registered pool attributes to
	// its owning source and caps it; a gap on a contract NO source owns stays
	// unattributed and is published by the system `recognition` snapshot below
	// (which this run refreshes every pass — see that block). FAIL CLOSED on
	// a registry read error, like the prior/floor reads: an owner map missing
	// its registry members would silently make that axis always-true.
	// Only paid when the scan actually found gaps to attribute — a clean or
	// -skip-recognition run has nothing to fold.
	if len(recGaps) > 0 {
		if oerr := loadRegistryOwners(ctx, store, ownerOf); oerr != nil {
			return fmt.Errorf("recognition owner attribution (failing closed — an incomplete owner map cannot fail recognition for the topic-matched sources): %w", oerr)
		}
	}
	recBySource, unattributed := attributeRecognitionGaps(ownerOf, recGaps)

	// Substrate (Claim 1) is a property of the lake, but "does THIS source's
	// own [genesis,tip] have a problem" is scoped per source (see
	// substrateForGenesis): each source gets its own scan, floored at
	// its own genesis and memoised by that floor so sources sharing a
	// genesis reuse the same scan. The CH lake is the certified authoritative
	// substrate.
	chSubstrateScanner := substrateScanner(func(ctx context.Context, from, to uint32) (uint32, bool, string, error) {
		return clickhouse.SubstrateProblem(ctx, *chAddr, from, to)
	})
	chSubstrateCache := make(map[uint32]substrateScan)
	// subScanFrom is the FLOOR this run's substrate scan actually reached.
	// subScanFrom > tip means "this run scanned no substrate at all"
	// (-skip-substrate). Carried out of the switch because substrateClaim
	// below needs it per source: it is what makes the difference between
	// "proven from genesis" and "trusted below a floor".
	subScanFrom := tip + 1
	if *skipSubstrate {
		fmt.Fprintln(os.Stderr, "compute-completeness: -skip-substrate — no substrate scan; the prior verdict is carried, not re-proven")
	} else {
		subScanFrom = uint32(2)
		if *fromLedger > 2 {
			subScanFrom = uint32(*fromLedger) //nolint:gosec // ledger seq fits uint32
		}
	}
	// The event table recognition and projection read, censused against
	// ledgers over the whole Soroban era every run — including -skip-substrate
	// and -from runs, whose carried prefix is exactly where a dropped partition
	// hides (eventCensusLoss).
	evCensus, err := clickhouse.EventCensusShortfalls(ctx, *chAddr, sorobanFloor, tip)
	if err != nil {
		return fmt.Errorf("contract_events census (failing closed — cannot certify the event table recognition and projection read): %w", err)
	}

	// Prior verdicts. An INCREMENTAL run (-from) reconciles only a
	// SUFFIX of each source's served range, so it may CONFIRM or DOWNGRADE the
	// served (`complete`) axis but must NEVER upgrade it: the only evidence for
	// the prefix it did not touch is the previously published verdict. Read
	// those verdicts BEFORE the loop overwrites them, and fail CLOSED on a read
	// error (same discipline as runRecognitionScan) — publishing verdicts while
	// blind to the prior ones lets a `complete=false` silently become
	// `complete=true` (see projectionClaim).
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

	// Durable per-target projection floors (migration 0116). Loaded once and
	// FAILING CLOSED on error, exactly as the prior verdicts above do: these
	// are the only record of how far back each target has ever been verified,
	// so a run that cannot read them cannot honestly judge whether the served
	// tier has lost its oldest rows. Continuing without them would silently
	// downgrade every target to the pre-0116 behaviour — the reconcile floor
	// tracking the loss — which is the failure this table exists to close.
	targetFloors, err := store.CompletenessTargetFloors(ctx)
	if err != nil {
		return fmt.Errorf("durable projection floors (failing closed — cannot distinguish served-tier loss from never-projected without them): %w", err)
	}

	// Pending replay-rewind dirty windows (migration 0125). A
	// projector-replay that rewound BELOW a source's watermark rewrote
	// served rows the carried projection claim (projectionClaim rule 3)
	// still certifies — the carry's evidence is invalid until the rewound
	// range is re-reconciled. Loaded once, FAILING CLOSED like the two
	// reads above: a run blind to pending windows would republish carried
	// claims over ranges a replay has rewritten, which let a cctp
	// replay's 19,366 over-projected rows escape the verifier.
	dirtyWindows, err := store.ProjectionDirtyWindows(ctx)
	if err != nil {
		return fmt.Errorf("pending replay-rewind dirty windows (failing closed — carrying a projection claim over a rewound range is the invalidation this record exists to prevent): %w", err)
	}

	// ── System recognition snapshot (gaps on contracts no source owns) ──
	//
	// Refreshed on every run, whatever the -source filter, and BEFORE the
	// per-source loop so a pass that runs out of time still publishes it. An
	// upsert error does not stop the loop; it is joined into the run's error.
	var recSnapErr error
	if !*skipRecognition && !write {
		fmt.Fprintf(os.Stderr, "compute-completeness: recognition  unattributed=%d (dry-run: not stored)\n", len(unattributed))
	} else if !*skipRecognition {
		// The census is what /v1/coverage publishes as typed numbers on its
		// `recognition` audit axis: the distinct-contract count says how
		// concentrated the unattributed shapes are, not just how many.
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
		// Scan THIS source's own [genesis,tip] (memoised per floor — see
		// substrateForGenesis); it's this source's problem only if the
		// reported problem ledger falls at/after the source's genesis.
		// SubstrateProblem returns coverage-correct problem ledgers for
		// empty/head/tail absences (see its substrateHeadProblem doc) so
		// a high-genesis source can't read a COVERAGE failure as "below
		// my genesis, I'm fine".
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
		// Gate what the run may PUBLISH on the range it actually scanned:
		// the scan floor and the claim are different things.
		var subDetail string
		substrateOK, subDetail = substrateClaim(genesis, tip, srcScanFrom, scanClean, srcSub.problem, priorSub[src.name])
		detail = append(detail, subDetail)
		// Numeric-field gap: substrateClaim can refuse a CLEAN
		// suffix scan (no prior / a FAILING prior / a stale prior leaving an
		// unverified band) — cases where `problems` holds no substrate ledger
		// yet substrate_ok is false. Feed the unproven-prefix floor into
		// `problems` so the NUMERIC coverage watermark (coverage_pct /
		// watermark_ledger / first_problem) tracks substrate_ok exactly as
		// srW.Complete does, never publishing coverage_pct=1.0 / watermark=tip
		// while substrate_ok=false. See lakeCoverageProblem.
		if p := lakeCoverageProblem(genesis, scanClean, substrateOK, priorSub[src.name]); p != 0 {
			problems = append(problems, p)
		}
		// This is the substrate twin of detectFloorLoss.
		// An incremental run scanned only [subScanFrom, tip] and
		// substrateClaim rule 3 CARRIES the prior clean [genesis,
		// subScanFrom] verdict — so a capacity-archive DROP PARTITION (the
		// documented archive-to-S3 plan) that deletes the source's oldest
		// ledgers BELOW subScanFrom is invisible to the scan, and
		// substrate_ok / lake_complete stay true over an absent prefix.
		// Projection catches its own bottom-edge loss (detectFloorLoss +
		// completeness_target_floors, migration 0116); this is the
		// substrate equivalent. Probe the bottom edge directly: unlike the projection
		// floor (a served-tier MIN needing a durable row to tell "lost" from
		// "never written"), the substrate floor is the FIXED src.genesis —
		// the first-possible-data ledger the lake must always reach — so no
		// durable floor is needed. A cheap 1-ledger SubstrateProblem at
		// genesis reports whether the lake still holds it; an absent/broken
		// genesis is bottom-edge loss that must fail substrate_ok. Only
		// probed when the run CARRIES a prefix (subScanFrom > genesis); a
		// deep scan (subScanFrom <= genesis) already re-reads genesis and its
		// own head-presence guard reports the same loss.
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

		// Claim 2a: recognition gaps attributed to this source's contracts
		// (static contractIDs OR the factory-child / soroswap-pair registry
		// members folded into ownerOf above).
		recOK, recProblems := sourceRecognitionOK(genesis, tip, recBySource[src.name], *skipRecognition, priorRec[src.name])
		problems = append(problems, recProblems...)
		recDetail := recognitionClaim(recOK, *skipRecognition)
		if eventsShort {
			recOK, recDetail = false, "recognition: not certifiable — the contract_events it scans are short of what stellar.ledgers records (see substrate)"
		}
		detail = append(detail, recDetail)

		// Substrate∧recognition watermark drives COVERAGE — coverage means "did
		// we capture every event" (substrate is the proof). Projection is a
		// fidelity claim on the served tier, evaluated separately so its keying
		// artifacts / retention-scoping don't corrupt the coverage signal.
		srW := completeness.ComputeWatermark(genesis, tip, problems)
		// Lake (archive) axis: substrate ∧ recognition only. `problems`
		// never carries projection gaps, so
		// srW.Complete is genuinely decoupled from projection — the
		// ADR-0033/0034 two-axis verdict. lake_complete must NEVER be gated by the retention-scoped
		// projection reconcile.
		//
		// It IS gated by substrateOK - a claim about the range this
		// run scanned rather than a raw scan result. And because the
		// lakeCoverageProblem floor is already in `problems` above, so are the
		// NUMERIC fields: an incremental run that scanned only [from,tip]
		// cleanly but cannot prove the prefix (no/failing/stale prior)
		// pins srW.Ledger / CoveragePct / FirstProblem to the
		// proven-from-genesis extent instead of reading genesis-to-tip off a
		// suffix. srW.Complete and substrateOK therefore agree by
		// construction; `&& substrateOK` is kept as a belt-and-suspenders
		// guard and `detail` still states the floor.
		lakeComplete := srW.Complete && substrateOK
		projOK := false
		// The projection axis's own floor, published alongside the verdict
		// (migration 0155). Without it a consumer reading the typed fields would
		// see genesis_ledger (the LAKE floor, often ledger 2) and read the
		// served-tier claim as reaching back to it. 0 stays "not
		// evaluated" — the non-reconciling cases below leave it alone.
		var projVerifiedFrom uint32
		// Set only when the CH reconcile FOUND a failure; carried to the
		// write so a lower-tip run still records it.
		var projFound bool
		// What this run itself proved, as distinct from what it carried
		// (migration 0201): its reconcile floor and the claim's evidence time.
		var (
			projReconciledFrom uint32
			evidencedNow       bool
			evidencedAt        time.Time
		)
		// Incremental: only reconcile [projFrom, srW.Ledger], trusting
		// [genesis, projFrom] as previously verified. In -pass mode projFrom is
		// THIS source's own prior watermark, so substrate + recognition stay
		// proven once at full range for the whole pass while only projection is
		// scoped per source (projectionFloor) — unless the prior PROJECTION
		// verdict was failing, in which case there is no verified ground to
		// resume from and the source re-verifies from genesis. Outside
		// -pass it is the global max(genesis, -from) incremental floor.
		//
		// A pending replay-rewind window overrides the incremental floor:
		// the replay rewrote served rows below the watermark, so the range
		// MUST be re-reconciled before any claim — carried or fresh — may
		// cover it. -from can never skip past it. A deferred window is not
		// reconciled at all, so no claim covers it.
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
			// A bottom-edge truncation is invisible to the
			// reconcile itself, because the reconcile's own floor moves with
			// it. Compare against the durable floor BEFORE reconciling, and
			// treat loss as a hard projection failure — the surviving rows
			// will reconcile perfectly and would otherwise read complete.
			floorLoss := detectFloorLoss(src, servedMins, targetFloors)
			delta, blind, pdetail, perr := reconcileProjectionAggregate(ctx, store, streamer, *chAddr, src, scopes)
			if perr != nil {
				return fmt.Errorf("%s: projection: %w", src.name, perr)
			}
			projFound = projectionFoundProblem(delta, blind, floorLoss)
			// Only a clean reconcile earns the right to record verified
			// ground; a run that found a mismatch must not enshrine its
			// range as verified. A run that DETECTED loss must not record
			// either, or it would immediately adopt the post-loss floor as
			// the new truth and erase the evidence on the very next run.
			//
			// A blind re-derive earns no ground either. Its zero
			// delta is an artifact of both sides dropping the same rows,
			// so recording the floor would enshrine an unverified range.
			if write && delta == 0 && len(floorLoss) == 0 && !blind.Any() {
				if ferr := recordFloors(ctx, store, src, scopes, servedMins); ferr != nil {
					return fmt.Errorf("%s: record projection floors: %w", src.name, ferr)
				}
			}
			// The scope travels WITH the verdict: a run may only claim the
			// range it actually reconciled (ADR-0033 — a source is complete
			// through W iff every claim holds contiguously to W).
			//
			// `delta == 0 && !blind.Any()` is the honest "clean"
			// predicate. A ledger whose rows neither side could decode
			// contributes 0 to delta while proving nothing, so a bare
			// delta==0 would certify projection_ok on exactly the ledgers
			// the check is blind to. pdetail already leads with the
			// blind-spot summary (reconcileProjectionAggregate).
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
		// Coverage = substrate∧recognition (proven data capture). complete
		// additionally requires the served-tier projection to reconcile;
		// lakeComplete (set above, pre-projection) never does. The served
		// axis can never be stronger than the lake axis it sits on, so an
		// unproven substrate claim gates it too — that is what
		// lakeComplete already carries.
		w := servedAxisVerdict(srW, lakeComplete && projOK, deferDirty, dirtyWin)

		// Replay-rewind window disposition. The clear is earned by the
		// reconcile itself, not the carry: only a run whose CLEAN projection
		// verdict actually covered the whole window may delete it. A dirty or
		// short run leaves the window pending so every subsequent run keeps
		// extending its floor over the rewound range until one verifies it.
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

	// On a non-pubnet network, stale rows for pubnet-only sources would keep the verdict
	// red by construction — clear them. Only canonical names sourcenet
	// itself classifies as not applicable are ever passed.
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

// evaluateEachSource runs eval for every catalogue source the -source filter
// selects and joins the failures, so one source's error cannot withhold a
// fresh verdict from every source after it. An errored source publishes
// nothing and keeps its prior verdict. A done ctx stops the walk, since every
// remaining eval would fail the same way, and the error names each skipped source.
// perSource > 0 bounds each eval: one that outlives it fails alone (no verdict,
// prior stands) instead of consuming the run's deadline.
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

// orderForPass moves every source whose -pass projection floor is its genesis
// (a full re-verify) after the sources that resume incrementally, keeping
// catalogue order within each group, so one long re-verify that exhausts the
// run's deadline cannot withhold a fresh verdict from the cheap ones. Within the
// from-genesis group the census (SDEX) source runs last: its full-history
// op re-derive can outlast the whole pass, while event sources take seconds; a
// reproofOutlastsPass source runs just before it.
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

// projectionFloor is the projection reconcile floor for one source.
//
// In -pass mode a source resumes from its own prior watermark, so the global
// recognition and substrate scans run once per pass rather than once per
// chunk. A source floors at genesis instead when it has no watermark, when
// its prior projection verdict failed (the watermark is the lake axis and
// sits at tip, so the failing range would never be re-seen and the source
// could never go green), or when expireStaleCarries picks a clean carry older
// than -max-carry-age.
//
// The floor only moves down, so a run can only verify more; [projectionClaim]
// still decides what is published. Outside -pass it is max(genesis, -from),
// because an operator-stated floor is not this function's to widen.
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

// maxTipCursorLagLedgers is how far the network may provably have run past the
// ledgerstream cursor before the cursor stops being usable as the verdict tip:
// ~1h at [completeness.SlowestLedgerClose], far above a live indexer's lag.
const maxTipCursorLagLedgers uint32 = 600

// tipFromLiveCursor resolves the verdict tip from the live ledgerstream cursor,
// refusing a cursor the network has provably left behind (ADR-0033: no cursor
// trust). A frozen ingest would otherwise stamp a fresh computed_at onto a
// verdict "complete to tip" for a tip the network passed long ago; refusing
// writes no snapshot, so the stored verdicts age into the API's stale gate.
func tipFromLiveCursor(cur timescale.Cursor, now time.Time) (uint32, error) {
	floor := completeness.NetworkTipLowerBound(cur.LastLedger, cur.UpdatedAt, now)
	if lag := floor - cur.LastLedger; lag > maxTipCursorLagLedgers {
		return 0, fmt.Errorf("ledgerstream cursor is frozen at ledger %d (last advanced %s ago; the network has closed at least %d ledgers since) — it is not the network tip, so no verdict is stamped against it (-allow-frozen-cursor with -to overrides)",
			cur.LastLedger, now.Sub(cur.UpdatedAt).Round(time.Second), lag)
	}
	return cur.LastLedger, nil
}

// verdictTipFromCursorRead applies the cursor read result: with an explicit -to,
// a missing cursor row means there is no live ingest to guard (a host that never
// ingested); any other read error, or a missing row without -to, fails closed.
func verdictTipFromCursorRead(to uint32, cur timescale.Cursor, readErr error, now time.Time, allowFrozen bool) (uint32, error) {
	if readErr != nil {
		if to != 0 && errors.Is(readErr, timescale.ErrNotFound) {
			return to, nil
		}
		return 0, fmt.Errorf("read ledgerstream cursor: %w", readErr)
	}
	return resolveVerdictTip(to, cur, now, allowFrozen)
}

// resolveVerdictTip picks the verdict tip. The frozen-cursor guard applies to an
// explicit -to as well, since the nightly wrapper passes -to (cursor minus a
// margin) and would otherwise keep refreshing computed_at against a stalled
// ingest. Only an explicit override skips it.
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

// sourceProjectionFloor is projectionFloor for one catalogue source. In -pass
// mode a genesis-claimed source whose clean prior verdict only covered from
// above genesis re-verifies from genesis now: projectionClaim would refuse to
// carry that prior anyway, so resuming at the watermark only buys a false.
func sourceProjectionFloor(src reconSource, pass bool, prior priorProjection, priorWatermark uint32, fromLedger uint) uint32 {
	if pass && src.servedWindowReason == "" && prior.verifiedFrom > src.genesis {
		return src.genesis
	}
	return projectionFloor(src.genesis, pass, prior, priorWatermark, fromLedger)
}

// validatePassFlags fails CLOSED when -pass is combined with the per-source /
// per-chunk knobs it REPLACES. A whole-pass run proves recognition + substrate
// ONCE at full range for EVERY source, then reconciles each source's projection
// incrementally from its own watermark (projectionFloor), so:
//   - -source (one source) defeats the "every source in one process" property
//     and re-introduces the per-source re-invocation whose repeated global
//     recognition scan is the timeout the pass exists to remove;
//   - -from is a GLOBAL floor that also raises subScanFrom (truncating the
//     substrate scan) and cannot express a per-source projection floor — the
//     pass derives that per source instead;
//   - -skip-substrate / -skip-recognition CARRY a prior verdict rather than
//     prove it fresh, which is the staleness the pass fixes; for substrate,
//     skipping the full-tip proof re-opens the low-tip flap.
//
// Rejecting these combinations rather than silently ignoring them keeps an operator (or a
// mis-edited wrapper) from publishing a partial pass that looks complete. Pure.
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

// runRecognitionScan runs the selected recognition audit (Claim 2a) and
// returns its gaps, FAILING CLOSED on a scan error. A
// scan ERROR (CH unreachable, query timeout, the
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

// loadRegistryOwners folds the factory-child (protocol_contracts) and
// soroswap-pair registries into ownerOf so the TOPIC-MATCHED sources (empty
// contractIDs) can attribute — and therefore FAIL — recognition on a dropped
// topic on one of their own pools. These are the SAME
// membership sets the recognition gate warms (GatedRegistryOptions /
// seedSoroswapForRecon), so the data is already the source of truth at compute
// time; this only makes the completeness verdict read it too. FAIL CLOSED on a
// read error — the caller must not proceed with a partial owner map.
//
// The DB reads live here; the pure fold is [mergeRegistryOwners] so the
// attribution → verdict chain is unit-testable without a live store.
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

// mergeRegistryOwners folds registry-derived contract→source membership into
// ownerOf: gatedChildren maps a factory-anchored source name to its
// protocol_contracts children, and soroswapPairs is the soroswap_pairs
// registry (all owned by "soroswap"). A contract ALREADY pinned by a source's
// static contractIDs gains no registry owner — the static catalogue pin wins,
// so a curated contract-pinned source keeps its exact attribution. A contract
// two registries both claim gets both owners. Pure — unit-testable. The
// "soroswap" literal mirrors the catalogue entry's own literal name
// (reconciliation_catalogue.go).
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

// contractOwners maps every catalogue source's static contractIDs to EVERY
// source that pins it. Sharing a contract set is a supported configuration
// (sep41_transfers and sep41_supply are both gated on the watched SEP-41
// list), so a single-owner map would hand all of one source's contracts to
// whichever was catalogued last and leave the other's recognition axis
// unable to fail.
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

// attributeRecognitionGaps splits the global recognition scan's gaps into the
// per-source (owned) map and the unattributed remainder (gaps on contracts no
// source owns), using ownerOf. A gap on an owned contract caps its owning
// source's recognition axis; an unattributed gap flows to the system
// `recognition` snapshot. Pure — unit-testable.
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

// sourceRecognitionOK is the per-source Claim-2a verdict: a source fails
// recognition (returns false) when any recognition gap attributed to it falls
// at or after its genesis, and returns those problem ledgers to fold into the
// coverage watermark. attributed is recBySource[source] — which
// includes gaps on the source's factory-child /
// soroswap-pair registry members, not just its static contractIDs.
//
// -skip-recognition runs NO scan at all (attributed is always empty under
// it), so the loop above alone would read recOK=true unconditionally while
// the flag's own doc says "trust the prior recognition audit". Gate it here
// exactly as substrateClaim gates -skip-substrate: carry the prior verdict only when it is
// known, was itself clean, and covered ledgers up to this run's tip; a
// missing, failing or stale prior fails closed instead of asserting a fresh
// true with zero evidence.
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

// recognitionClaim states what THIS run knows about the source's recognition
// axis (Claim 2a), mirroring substrateClaim/projectionClaim's explicit-carry
// contract: recOK alone cannot distinguish "this run's shape scan found
// nothing unrecognized" from "-skip-recognition trusted the prior audit with
// NO evidence from this run" (runRecognitionScan returns nil gaps on skip,
// so recOK reads true either way). substrateClaim already states this
// distinction for -skip-substrate; recognition had no equivalent, so the
// published detail — and the "complete: substrate + recognition + projection
// verified to tip" default — read identically whether recognition was
// proven or merely carried. Pure.
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

// projectionFoundProblem reports whether the CH aggregate reconcile FOUND a
// failure. It localises none to a ledger, so combineWatermark leaves
// FirstProblem at zero and the snapshot must carry this instead; a
// projection that was not evaluated, or failed only on scope, is not a find.
func projectionFoundProblem(delta int, blind completeness.BlindSpots, floorLoss []string) bool {
	return delta != 0 || blind.Any() || len(floorLoss) > 0
}

// combineWatermark applies the served-tier projection gate to the lake
// (substrate∧recognition) watermark srW: the returned watermark's
// Complete additionally requires projOK — this is the `complete`
// (served/combined) axis. It does NOT touch srW.Complete itself, which
// callers read separately as lake_complete — the ADR-0033/0034
// two-axis verdict.
// Pure and deterministic, mirroring completeness.ComputeWatermark.
func combineWatermark(srW completeness.Watermark, projOK bool) completeness.Watermark {
	w := srW
	w.Complete = srW.Complete && projOK
	return w
}

// projectionScope is the ledger range a Claim-2b reconcile ACTUALLY covered for
// one target. It travels with the verdict so a run can never publish
// completeness over ledgers it did not reconcile (ADR-0033: a source is
// complete through W iff every claim holds contiguously to W).
type projectionScope struct {
	From uint32 // inclusive
	To   uint32 // inclusive
}

// empty reports a degenerate scope, which reconcileTarget skips uncounted.
func (sc projectionScope) empty() bool { return sc.From > sc.To }

// targetScope derives one target's reconcile range from the served tier's own
// data: servedMin is that target's MIN(ledger) over [genesis, hi]. Scoping is
// per target, not per source or by a fixed retention window, because no
// reconcile target has a retention policy and each table's history starts at
// a different ledger (on r1, sdex trades begin at ledger 61,609,957 while
// soroswap trades begin at 50,746,445).
//
// An empty target floors at genesis, failing closed: a wiped table must
// reconcile expected>0 against served=0. runFrom can only raise the scope;
// what it excludes is left to [projectionClaim].
//
// A deleted bottom edge raises MIN(ledger) with the loss, so this scope cannot
// see it; [detectFloorLoss] against completeness_target_floors catches that.
// The scope stays data-derived because reconciling below the served rows
// would manufacture false gaps.
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

// projectionScopes resolves every target's reconcile scope (scopesFromServed)
// and returns (a) the scopes, parallel to src.targets, (b) servedFrom —
// the bottom of the full range the served axis claims to be faithful over
// (genesis, or the lowest served row of a windowed source) — and (c)
// runFrom, where THIS run's reconcile actually starts. servedFrom < runFrom is
// exactly the "-from skipped a range" case projectionClaim gates.
//
// One indexed MIN(ledger) per target (trades_source_ledger_idx et al); the same
// query buildClassicGapGate already runs from genesis in production.
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

// scopesFromServed is the pure half of projectionScopes. A source without a
// servedWindowReason scopes every target from genesis: its served tier claims
// genesis-to-tip, so a prefix that was never projected must reconcile as
// expected>0 vs served=0 instead of moving the floor up past it.
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

// servedFloor is one target's live bottom edge: `MIN(ledger)` over the
// reconcile window, and whether the target held anything at all. Carried out
// of projectionScopes so the caller can compare it against the DURABLE floor
// (completeness_target_floors, migration 0116) — the comparison targetScope
// itself cannot make, because it derives its floor from this very value.
type servedFloor struct {
	min     uint32
	present bool
}

// detectFloorLoss compares each target's live bottom edge against the durable
// floor earlier runs recorded (completeness_target_floors) and returns a
// detail line per target whose oldest rows have gone missing.
//
// targetScope cannot make this check: it floors the reconcile at the target's
// own MIN(ledger), so deleting the oldest rows raises the floor with the loss
// and "dropped the oldest 10M ledgers" reads the same as "never projected
// below there". Only a floor the served tier cannot rewrite tells them apart.
//
// A target with no recorded floor is skipped (first run; floor=0 would report
// loss for every target). An empty target that had a floor is the maximal
// loss and is reported, not skipped.
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

// floorsToRecord is what a clean run has evidence for, per target: the pure
// half of recordFloors. It records the target's live MIN(ledger) only when the
// reconcile scope reached that bottom edge (scopes[i].From <= servedMins[i].min).
//
// A narrowed -from or incremental run clips the scope above the served
// minimum and has no evidence below the clip; recording scopes[i].From would
// bank an inflated floor on a target's first clean pass (LEAST() only protects
// a target that already has a lower one) and disarm detectFloorLoss for the
// unverified gap.
//
// A target with no served rows is skipped for the same reason: targetScope
// floors it at genesis to fail closed, but a clean "0 expected, 0 served"
// says nothing about where rows begin. Recording genesis would make the first
// real row at L read as L−genesis ledgers lost, a permanent false failure that
// LEAST() can never raise back. Late-starting targets (a source whose first
// event has not happened yet) all take this path.
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
			// Clipped by an incremental floor above the true served
			// minimum — this run verified nothing below the clip, so
			// it cannot bank a floor there.
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

// recordFloors persists what this run actually verified, per target — see
// [floorsToRecord] for which targets earn a floor and why an empty one does
// not.
//
// Called ONLY after a clean reconcile (delta == 0). Recording a floor from a
// run that found a mismatch would enshrine an unverified range as verified.
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

// clipCounts drops per-ledger counts outside sc. The expected side is
// re-derived ONCE over the union of the source's target scopes, so each target
// must compare only the ledgers inside its own scope (the served side is
// already bounded by CountRowsByLedger's range). Returns a new map.
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

// priorProjection is what the LAST published verdict knows about a source's
// served-tier projection axis (completeness_snapshots.projection_ok /
// tip_ledger). known=false means no verdict has ever been written.
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

// maxEvidenceRefloorsPerPass caps how many expired carries one -pass re-proves
// from genesis. The rest keep carrying (and keep their old evidence time) until a
// later night, which staggers expiry instead of re-flooring every source at once.
const maxEvidenceRefloorsPerPass = 3

// expireStaleCarries marks, oldest evidence first (unknown counts as oldest), at
// most maxEvidenceRefloorsPerPass catalogue sources whose clean prior projection claim was last
// reconciled in full longer than maxAge ago (or never) as expired, and returns
// their names. Census sources are never marked: a full SDEX re-derive outlasts
// the pass's deadline, and a deadline-cut source writes nothing, so a forced
// re-floor would re-floor it every night and never refresh; its evidence ages
// honestly until the weekly -source run re-proves it. maxAge <= 0 disables it.
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

// projectionEvidence is the evidence time a projection claim publishes. A run
// that reconciled the whole served range cleanly stamps it now; a claim carried
// over a skipped prefix keeps the prior claim's time (zero when unknown), never
// this run's; a false claim has none. Pure.
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

// buildPriorVerdicts turns the last published snapshots into the per-axis
// carry-forward inputs for this run's claims, plus each source's last
// published watermark (the -pass mode projection floor — see
// projectionFloor).
//
// Substrate and recognition reconcile to the network tip, so s.Tip bounds
// their priors. Projection reconciles only to s.Watermark, which
// ComputeWatermark pins below tip when a recognition or substrate problem
// exists; bounding its prior by s.Tip would carry a clean claim over
// (s.Watermark, s.Tip], a band no run reconciled.
func buildPriorVerdicts(snaps []timescale.CompletenessSnapshot) (priorProj, priorSub, priorRec map[string]priorProjection, priorWatermark map[string]uint32) {
	priorProj = make(map[string]priorProjection, len(snaps))
	priorSub = make(map[string]priorProjection, len(snaps))
	priorRec = make(map[string]priorProjection, len(snaps))
	// priorWatermark is each source's last published lake watermark, used as the
	// per-source projection floor in -pass mode (projectionFloor): a whole-pass
	// run resumes every source from where it left off, so substrate + recognition
	// stay proven once at full range while only the projection reconcile is
	// scoped. A source absent here (0 — never seeded) floors at genesis. This is
	// the same value the per-source wrapper read as its -from.
	priorWatermark = make(map[string]uint32, len(snaps))
	for _, s := range snaps {
		priorProj[s.Source] = priorProjection{known: true, ok: s.ProjectionOK, tip: s.Watermark, verifiedFrom: s.ProjectionVerifiedFrom, evidencedAt: s.ProjectionEvidencedAt}
		priorWatermark[s.Source] = s.Watermark
		// The SUBSTRATE axis needs the same prior-verdict input the
		// projection axis has, for exactly the same reason —
		// an incremental run scans only a suffix and must not publish a
		// genesis-to-tip claim off it. See substrateClaim.
		priorSub[s.Source] = priorProjection{known: true, ok: s.SubstrateOK, tip: s.Tip}
		// The RECOGNITION axis needs the same prior-verdict input the
		// substrate axis has — -skip-recognition runs no scan at
		// all, so without this it can only ever read recognition_ok=true.
		// See sourceRecognitionOK.
		priorRec[s.Source] = priorProjection{known: true, ok: s.RecognitionOK, tip: s.Tip}
	}
	return priorProj, priorSub, priorRec, priorWatermark
}

// projectionClaim gates what a run is ALLOWED to publish on the served
// (`complete`) axis, given the range it ACTUALLY reconciled — so a verdict
// cannot silently regress from complete=false to complete=true.
//
// The risk is in the hot path: completeness-incremental.sh
// passes `-from = min(watermark)`, but watermark_ledger is the LAKE
// (substrate∧recognition) axis, which sits AT tip whenever the lake is clean.
// So an incremental run reconciles only the newest ledgers, never re-sees
// the projection mismatch that pinned `complete=false`, and without this gate
// writes complete=true — a verdict improving with no evidence, which ADR-0033 forbids
// (complete through W requires every claim to hold contiguously to W).
//
// Rules, fail-closed, in order:
//  1. A mismatch found by THIS run always fails — nothing can launder it.
//  2. A run whose reconcile started at or below servedFrom covered the WHOLE
//     served range: self-evidencing, may publish true. This is the only way a
//     failing verdict is ever cleared — deliberately, by a full re-verify.
//  3. A partial (incremental) run may CARRY FORWARD a prior clean verdict for
//     the prefix it skipped, but only if that prior verdict is contiguous with
//     this run's window (prior.tip+1 >= runFrom) and reached down to this
//     run's servedFrom (prior.verifiedFrom <= servedFrom), and every present
//     target has a proven floor (unprovenCarryTargets). Confirm, never upgrade.
//  4. Anything else — no prior verdict, a FAILING prior verdict, or a stale
//     prior that leaves an unverified band — publishes false.
//
// The returned detail always states the range actually verified, so
// `complete=true` is never read as a genesis-to-tip claim: the served tier's
// floor differs per source, and the genesis claim is the separate
// lake_complete axis.
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

// projectionWithoutEvidence refuses a clean projection claim that no served
// row stands behind: when none of the source's targets holds a row anywhere in
// [genesis, hi] (or it has no targets), a clean reconcile means expected ∅ ==
// served ∅. That is byte-identical on the wire to a real proof, and it is
// exactly what a wrong or redeployed contract identity produces. It returns
// vacuous=true with the detail naming why. Pure.
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

// publishSourceVerdict stores a source's verdict and, when this run earned
// the clear ([dirtyWindowSatisfied]), discharges the replay-rewind window
// in the SAME transaction — and only if the verdict was actually stored.
//
// `earned` is a statement about what this run RECONCILED; it says nothing
// about whether the store accepted the verdict. The never-regress
// guard rejects a run whose -to is below the stored tip, and the window
// must then survive: the stored verdict is still the pre-rewind one, and
// the window is the only thing forcing the next full-range run to
// re-reconcile the rewritten rows rather than carry that stale claim. The
// clear keeps [timescale.Store.PublishCompletenessVerdict]'s optimistic
// predicate, so a concurrent replay's re-record also survives.
func publishSourceVerdict(ctx context.Context, store verdictPublisher, snap timescale.CompletenessSnapshot, win timescale.ProjectionDirtyWindow, earned bool) (timescale.VerdictPublication, error) {
	var clearWindow *timescale.DirtyWindowClear
	if earned {
		clearWindow = &timescale.DirtyWindowClear{From: win.From, To: win.To, UpdatedAt: win.UpdatedAt}
	}
	return store.PublishCompletenessVerdict(ctx, snap, clearWindow)
}

// verdictNotStoredNote is the operator-facing suffix for a verdict the
// never-regress guard rejected. Without it the run's log line reads exactly like a
// stored verdict — and, with a window pending, like a discharged one.
// Empty when the verdict was applied. Pure — unit-testable.
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

// substrateClaim gates the lake verdict (substrate_ok, lake_complete) on the
// range the substrate scan actually covered; it is [projectionClaim]'s twin,
// and [lakeCoverageProblem] gates the coverage watermark the same way. The
// scan covers [scanFrom, hi] but the claim is about [genesis, hi], so without
// this gate a clean suffix would certify genesis-to-tip and the nightly
// incremental pass would flip a real failure below the floor to true. The
// returned detail names the verified range and is served on /v1/coverage.
//
// Rules, fail-closed, in order:
//  1. A problem found by this run always fails.
//  2. A scan starting at or below genesis covered the whole claim and may
//     publish true; this is the only way a failing verdict clears.
//  3. A partial run may carry a prior clean verdict only if it is contiguous
//     with this run's window (prior.tip+1 >= scanFrom).
//  4. Anything else publishes false.
//
// scanFrom > hi (-skip-substrate) falls through to rules 3 and 4, so a failing
// prior is never upgraded without evidence.
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

// substrateFloorLoss detects bottom-edge loss on the substrate axis, the twin
// of [detectFloorLoss] on the projection axis. An incremental run scans only
// [subScanFrom, tip] and carries the prior [genesis, subScanFrom] verdict, so a
// DROP PARTITION of the oldest ledgers would otherwise leave lake_complete
// asserting a contiguous, hash-chained archive from genesis.
//
// The substrate floor is the fixed src.genesis, so no durable floor row is
// needed: floorHasProblem is a 1-ledger clickhouse.SubstrateProblem probe at
// genesis. It fires only when the run carries a prefix (subScanFrom > genesis);
// a deep scan re-reads genesis itself and would double-count. Like
// detectFloorLoss it sees a rising bottom edge, not an interior hole in the
// carried prefix, which needs a full re-scan.
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

// readsContractEvents reports whether the source's recognition and projection
// axes read stellar.contract_events. sdex (the op census) and the event-less
// ContractCall sources (band, soroswap-router) read stellar.operations instead.
func (src reconSource) readsContractEvents() bool {
	return !src.census && src.callDec == nil
}

// eventCensusLoss is the per-source verdict on the lake-wide contract_events
// census (clickhouse.EventCensusShortfalls): the first short partition that
// reaches into [genesis, ∞) fails the source, at max(first event ledger,
// genesis) so ComputeWatermark cannot drop it as below genesis. Substrate
// proves stellar.ledgers only; without this a dropped or unrestored event
// partition left lake_complete and recognition_ok true over events nobody
// read. Pure.
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

// lakeCoverageProblem is the numeric twin of [substrateClaim]: the ledger to
// inject into the coverage watermark's problem set when the claim refuses an
// otherwise-clean suffix scan. Without it the watermark would read
// genesis-to-tip (coverage_pct=1.0) beside substrate_ok=false.
//
// Returns 0 when the claim holds, or when the raw scan already surfaced a more
// precise problem ledger (!scanClean). Otherwise, mirroring substrateClaim's
// rules 4a-4c: a clean prior proved [genesis, prior.tip], so the unverified
// band opens at prior.tip+1; a missing or failing prior proves nothing, so the
// floor is genesis.
//
// The floor is clamped to >= genesis because ComputeWatermark ignores problems
// below genesis and would restore the 1.0 claim; in rule 4c's case
// prior.tip+1 <= tip, so the floor always lands inside [genesis, tip].
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

// reconcileProjectionAggregate is the CH-backed projection check (ADR-0033
// Claim 2b). It compares strict per-ledger counts (projectionDelta →
// completeness.ReconcileCounts) because a totals compare lets a real drop at
// one ledger net against a phantom overcount elsewhere. Sources whose served
// `ledger` keying differs from the re-derive's over a fixed historical span
// opt out via reconSource.aggregate and accept the documented netting residual
// up to that boundary. Returns Σ|per-ledger Δ| across targets (0 = clean).
//
// Each target is reconciled over its own scope (projectionScopes /
// targetScope, parallel to src.targets): the range the served tier holds for
// that table, raised to the incremental -from floor. The expected side is
// re-derived once over the union and clipped per target, so a late-starting
// table (trades) and a full-history one (soroswap_skim_events) in the same
// source each verify over their true range.
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
	// State the blindness first — it explains why a zero delta below
	// is not evidence, and the caller uses it to refuse the claim outright.
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

// expectedProjection re-derives the EXPECTED side of Claim 2b once over
// [lo, hi] and returns a per-target accessor plus the blind spots the
// re-derive hit. Of the three oracles, two can be blind: the decoder-driven
// one (contract_events), and the ContractCall census, which soft-fails per
// call in forEachContractCallEvent. The ch-rebuild writer shares that
// function, so a call whose Decode errors is missing from both sides and nets
// to zero. The SDEX census soft-fails per claim and still emits the op, so it
// cannot drop a whole row from one side only.
func expectedProjection(ctx context.Context, chStreamer completeness.EventStreamer, chAddr string, src reconSource, lo, hi uint32) (func(reconTarget) map[uint32]int, completeness.BlindSpots, error) {
	switch {
	case src.callDec != nil:
		// Event-less ContractCall source (band, soroswap-router): re-derive the
		// census from the lake's InvokeContract ops (no soroban_events landing
		// zone) and reconcile against the served tier (oracle_updates /
		// soroswap_router_swaps) by the SAME decoder the live dispatcher routes.
		expected, blind, err := reDeriveContractCallCensus(ctx, chAddr, src.callContract, src.callDec, lo, hi)
		if err != nil {
			return nil, completeness.BlindSpots{}, err
		}
		return func(reconTarget) map[uint32]int { return expected }, blind, nil

	case src.census:
		// Re-derive the census by running the SDEX decoder over the certified CH
		// operations and counting its trade output — the SAME decode the indexer
		// applies to live ops. This matches served by identical logic, so the
		// only residual is ops the served tier dropped (real coverage gaps), not
		// the over-count claimAtomCount carries (it counts claims the decoder
		// later drops as malformed-asset). Independent SOURCE (the lake's full op
		// set, substrate-proven) vs the live-ingested ops — catches drops, never
		// passes by construction.
		expected, blind, err := reDeriveSDEXCensusViaDecoder(ctx, chAddr, lo, hi)
		if err != nil {
			return nil, completeness.BlindSpots{}, err
		}
		return func(reconTarget) map[uint32]int { return expected }, blind, nil

	default:
		// Factory-anchored sources (ADR-0035): seed the gate registry from the
		// factory's creation events [genesis, lo) before the re-derive, so children
		// deployed before this window aren't dropped — exactly as
		// verify-reconciliation does (verify_reconciliation.go). Without this the
		// daily verdict's child gate is only the static protocol_contracts seed and
		// goes STALE as new pools deploy: blend reports complete=false
		// (expected=0) on windows whose activity is on pools missing from the seed,
		// while the live decoder (which self-seeds from deploy events) captures them.
		// This keeps the watchdog self-maintaining.
		var walkBlind completeness.BlindSpots
		if len(src.factories) > 0 {
			pb, err := preseedFactoryChildren(ctx, chStreamer, src, lo)
			if err != nil {
				return nil, completeness.BlindSpots{}, fmt.Errorf("%s preseed: %w", src.name, err)
			}
			walkBlind = pb
		}
		// Factory-anchored IDENTITY-gated sources (aquarius, phoenix) opt into a
		// contract-id PREFILTER so the re-derive reads only the gated pool set
		// (contract-indexed) instead of streaming the whole ~6B-event lake, which
		// otherwise overruns the -pass 120-min deadline. Built AFTER the preseed so
		// the prefilter is a guaranteed SUPERSET of the registry the re-derive
		// will hold at every point in [lo,hi]; Matches() stays the per-event
		// gate, so the counts are byte-identical (see gatedPrefilter).
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

// gatedPrefilter builds the contract-id prefilter for a factory-gated source
// (aquarius, phoenix) so the -ch re-derive reads only its gated contracts
// instead of the whole ~6B-event lake, keeping it inside the -pass deadline.
// The set is a superset of what the real decoder can accept anywhere in
// [lo,hi], so counts are unchanged and Matches() stays the final gate:
//
//   - (a) the decoder's gate after preseeding to lo (GatedContractSet), plus
//   - (b) every child the lake announces through hi, walked on a throwaway
//     decoder so the real stream's in-order seeding is undisturbed.
//
// Factory ids are always included so in-window children still self-seed.
// The (b) walk must match String topics as well as Symbols: phoenix's
// ("create","liquidity_pool") topic is two ScvStrings. It runs under
// completeness.Guard, so a panicking creation event becomes a reported blind
// spot rather than a crash.
func gatedPrefilter(ctx context.Context, chStreamer completeness.EventStreamer, src reconSource, hi uint32) ([]string, completeness.BlindSpots, error) {
	set := make(map[string]struct{})
	blind := completeness.NewBlindTracker()
	// (a) the registry the real re-derive starts the stream with.
	if g, ok := src.dec.(gatedContractSetter); ok {
		for _, c := range g.GatedContractSet() {
			set[c] = struct{}{}
		}
	}
	// (b) children announced through hi, read from the same certified lake the
	// re-derive streams, on a throwaway decoder so src.dec is left untouched.
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
						// Decode registers the announced child in `fresh`'s registry
						// (its only side effect for a creation event); a decode error
						// on a malformed creation event is soft-failed exactly as the
						// re-derive would, so the walk stays non-fatal.
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

// maxGatedPrefilterIDs keeps the inlined contract_id list well under 256 KiB
// (~60 bytes per quoted strkey). sorocredit announces 139k+ children.
var maxGatedPrefilterIDs = 2000

// gatedContractSetter is the read-only half of gatedDecoder — a decoder that
// can enumerate its gate. Split out so gatedPrefilter can read src.dec (typed
// as the interface completeness.Decoder) without asserting the full
// gatedDecoder.
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

// reDeriveSDEXCensusViaDecoder re-derives the expected SDEX trade count per
// ledger by running the SDEX decoder over the certified CH operations and
// counting the DISTINCT, Validate-passing, priceable trades it emits (the
// served PK has been an ON CONFLICT DO UPDATE since migration 0109, so a
// colliding op_index overwrites rather than drops). One-side-zero fills are
// stored by the writer but excluded here AND from the served COUNT
// (reconTarget.countFilter), because ledgers written before they were
// admitted hold none; a full-history ch-rebuild -sdex retires both exclusions.
// The residual is then exactly the ops the served tier dropped (real coverage
// gaps), not a methodology artifact — see sdexServedCensus.
// Read-only; windowed so the operations⋈results join stays under the CH
// memory cap.
func reDeriveSDEXCensusViaDecoder(ctx context.Context, chAddr string, from, to uint32) (map[uint32]int, completeness.BlindSpots, error) {
	out := make(map[uint32]int)
	blind := completeness.NewBlindTracker()
	dec := sdex.NewDecoder()
	// 25k bounds the per-window join input (100k windows hit a series of
	// OOMs) — combined with the reader's grace_hash
	// spill this bounds memory regardless of history growth.
	const window = 25_000
	for lo := from; lo <= to; lo += window {
		hi := lo + window - 1
		if hi > to {
			hi = to
		}
		seen := sdexServedCensus{}
		if err := clickhouse.StreamSDEXOps(ctx, chAddr, lo, hi, func(op clickhouse.SDEXOp) error {
			// SDEX Decode soft-fails per claim (never a non-nil error);
			// DecodeCounted additionally reports how many claim atoms in
			// this op failed to decode, so a failure marks the ledger
			// BLIND instead of silently reading as zero trades.
			// Both-zero no-op claims are a symmetric drop, not a failure.
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
				blind.Undecodable(op.Ledger)
				return nil //nolint:nilerr // intentional: the op is counted blind via blind.Undecodable, not aborted
			}
			for i := 0; i < failed; i++ {
				blind.Undecodable(op.Ledger)
			}
			seen.add(outs)
			return nil
		}); err != nil {
			return nil, completeness.BlindSpots{}, err
		}
		seen.addTo(out)
		if hi == to {
			break
		}
	}
	return out, blind.Result(), nil
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

// decodeContractCallTree decodes every call in ONE op's call tree that `dec`
// owns, forwarding each emitted event to fn and recording per-call decode
// failures on `blind`.
//
// Split out of [forEachContractCallEvent] so the blind-spot accounting is
// testable without a live ClickHouse: the streaming half needs a lake, this
// half is pure.
//
// The `continue` on a decode error is symmetric with the ch-rebuild WRITER,
// which shares this same function — so a malformed call is missing from the
// served tier AND from the census, the per-ledger diff nets to zero, and the
// ledger reads clean while a row was provably dropped. That is precisely why
// the skip is counted rather than swallowed.
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

// decodeContractCall runs dec over one call under completeness.Guard. A panic
// in Matches or Decode comes back as a decode error on a matched call, so the
// caller records it blind exactly as the dispatcher skips it on the live path.
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

// projectionDelta compares one target's re-derived expected counts
// against its served counts, both keyed by ledger.
//
// Default is STRICT PER-LEDGER: a window-totals compare lets a real
// drop in ledger L net against a phantom elsewhere. A source with an aggregate
// waiver nets only up to its boundary; a waiver with no boundary reconciles
// strict, because netting without the bound that justifies it hides a live
// drop.
//
// Returns Σ|per-ledger Δ| (0 = clean) and a human detail string.
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
		// The window straddles the boundary: aggregate up to it, strict above
		// it, so a real post-boundary drop cannot net against a pre-boundary
		// phantom.
		b := w.boundary
		preD, preDetail := aggregateDelta(src, table,
			countsAtOrBelow(expected, b), countsAtOrBelow(actual, b), lo, b)
		postD, postDetail := strictPerLedgerDelta(table,
			countsAbove(expected, b), countsAbove(actual, b), b+1, hi)
		return combineVintageDelta(table, b, preD, preDetail, postD, postDetail)
	}
}

// aggregateDelta compares WINDOW TOTALS (the netting compare) — used for
// an aggregate-waiver source's pre-vintage span, where the served ledger can
// legitimately differ from the re-derive's event ledger.
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
	// Opt-in surgical-remediation aid: the verdict detail names only the FIRST
	// mismatched ledger (an operator hunting a whole class doesn't want a
	// thousand-ledger string persisted in completeness_snapshots.detail). When
	// SI_DUMP_GAPS is set, additionally stream EVERY per-ledger gap to stderr so
	// a targeted over/under-count purge can be built from the authoritative
	// per-ledger reconcile — the same numbers the verdict is derived from, not a
	// windowed proxy. Read-only; changes no verdict, no stored detail.
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

// combineVintageDelta merges the pre-boundary aggregate and post-boundary
// strict deltas of a vintage-split source. The total Σ|Δ| drives the verdict
// (0 = clean); the detail names the post-boundary strict signal first because
// that is the real gap the split exists to surface.
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

// recognitionGlobalExcludeSyms is the topic exclusion for the CH-backed
// global recognition census. MUST be FirehoseExcludeSyms, not the wider
// ClassicTokenTopic0Syms: excluding set_admin entirely hides the
// Blend/Comet pool-level set_admin collision on the shared "POOL" topic. A
// package var (not inlined) so the wiring choice is unit-testable without a
// live ClickHouse connection.
var recognitionGlobalExcludeSyms = clickhouse.FirehoseExcludeSyms

// computeRecognitionGapsCH is the CH-backed recognition audit: distinct
// (contract, topic) shapes from the certified lake, run through the
// dispatcher's Recognize(), off the serving DB.
//
// The global scan excludes FirehoseExcludeSyms (the CAP-67 classic-token
// topics minus set_admin — see that var's doc): auditing every contract in
// the lake for transfer/mint/burn/… would re-scan the 447 M-row firehose for
// a set no enabled protocol decoder consumes. But watched_sep41_contracts
// DOES consume exactly those topics, so excluding them wholesale would make a
// SEP-41 source silently dropping its OWN events unreachable, and
// excluding set_admin only from the exclusion (not entirely) is what
// surfaces the Blend/Comet pool-level set_admin collision on the shared
// "POOL" topic. watchedSep41RecognitionShapes re-adds exactly those excluded
// topics, scoped to the watched contract set, so both stay auditable without
// the firehose cost. Those watched shapes are recognised only because
// buildCensusDispatcher also registers the sep41 event decoders.
func computeRecognitionGapsCH(ctx context.Context, cfg config.Config, chAddr string, gated map[string][]contractid.Option, from, tip uint32, soroswapOpts ...soroswap.DecoderOption) ([]completeness.RecognitionGap, error) {
	disp, err := buildCensusDispatcher(cfg, gated, soroswapOpts...)
	if err != nil {
		return nil, err
	}
	shapes, err := clickhouse.DistinctTopicShapes(ctx, chAddr, from, tip, recognitionGlobalExcludeSyms)
	if err != nil {
		return nil, err
	}
	if verr := recognitionScanEmptyErr(len(shapes), from, tip); verr != nil {
		return nil, verr
	}
	watched, err := watchedSep41RecognitionShapes(ctx, cfg, chAddr, from, tip)
	if err != nil {
		return nil, err
	}
	shapes = append(shapes, watched...)

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

// buildCensusDispatcher builds the recognition census's decoder chain the
// way the indexer does: the enabled sources plus the watched-set-gated
// sep41 event decoders, which BuildDispatcher alone does not register.
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

// watchedSep41RecognitionShapes is the watched-set-scoped half of the
// recognition census: the CAP-67 topics FirehoseExcludeSyms drops
// from the global scan, restricted to the operator-curated
// watched_sep41_contracts, so a watched source dropping its own event kinds
// still produces a recognition gap. Empty watch list means nothing to audit.
func watchedSep41RecognitionShapes(ctx context.Context, cfg config.Config, chAddr string, from, tip uint32) ([]clickhouse.TopicShape, error) {
	watched := cfg.Supply.WatchedSEP41Contracts
	if len(watched) == 0 {
		return nil, nil
	}
	return clickhouse.DistinctTopicShapesForWatchedContracts(ctx, chAddr, from, tip, clickhouse.FirehoseExcludeSyms, watched)
}

// recognitionScanEmptyErr fails closed on a recognition scan that read zero
// event shapes: no samples means no gaps means recognition_ok=true with no
// distinction from "genuinely clean" — the same vacuous-pass
// verify-recognition already refuses (verify_recognition.go). Reachable for
// any pre-Soroban range and once soroban_events / the lake window is
// retention-dropped or decommissioned.
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
