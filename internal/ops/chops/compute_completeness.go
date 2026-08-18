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
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	soroswap_router "github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// sorobanEraGenesis is the first pubnet ledger with Soroban — the lower
// bound for the global recognition scan.
const sorobanEraGenesis = 50_457_424

// sourceSubstrateOK is the per-source Claim-1 verdict from a whole-range
// SubstrateProblem result: a source is substrate-OK iff there is no problem, or
// the reported problem ledger is strictly below the source's genesis (the
// break/absence lies before the source's own data). Correctness depends on
// SubstrateProblem returning a COVERAGE-correct problem ledger — an empty or
// tail-truncated lake returns the range tip, so `problem < genesis` is false for
// every source and none can green itself on an absent lake (the F1 consumer
// fail-open the reviewer caught: a point value like `from` let soroswap, genesis
// 50.7M, read `2 < 50.7M = true` on an EMPTY lake). Pure — unit-testable.
func sourceSubstrateOK(problem uint32, hasProblem bool, genesis uint32) bool {
	return !hasProblem || problem < genesis
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
// (DAT-09/N-F2 + INV-5; see targetScope and projectionClaim).
func computeCompleteness(args []string) error { //nolint:funlen,gocognit,gocyclo // linear computor; one block per claim.
	fs := flag.NewFlagSet("compute-completeness", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	toFlag := fs.Uint("to", 0, "Tip ledger (inclusive); 0 = resolve from the live ledgerstream cursor")
	only := fs.String("source", "", "Limit to one source (e.g. soroswap|blend|reflector-dex|sdex)")
	useCH := fs.Bool("ch", false, "Read all three claims from the certified ClickHouse lake (substrate + recognition + projection re-derive) instead of Postgres soroban_events — fast, off the serving DB (ADR-0033 + ADR-0034)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address (with -ch)")
	skipSubstrate := fs.Bool("skip-substrate", false, "Skip the hash-chain re-scan and CARRY the prior substrate verdict — fast per-source iteration once substrate is proven. This run scans nothing, so it can only CONFIRM a prior clean verdict that already reached this run's tip; a FAILING or short prior verdict publishes substrate_ok=false with the unverified band named in the detail (C4-057). It no longer asserts substrate_ok=true unconditionally.")
	skipRecognition := fs.Bool("skip-recognition", false, "Trust the prior recognition audit (recognition_ok=true) instead of re-scanning all topic shapes — the global DistinctTopicShapes scan is the load-heaviest step; skip it for gentle projection-only iteration once recognition is verified")
	fromLedger := fs.Uint("from", 0, "INCREMENTAL verify: only check [from, tip], trusting [genesis, from] as already verified (substrate + recognition + projection all scoped to [from, tip]); the watermark still extends to tip when the window is clean. 0 = full verify from each source's genesis. The completeness timer passes min(watermark) from the prior snapshots so each run re-checks only new ledgers — minutes, not hours. An incremental run can only CONFIRM or DOWNGRADE the served `complete` axis, never upgrade it: a range it did not reconcile is carried from the prior verdict, and a FAILING prior verdict is cleared only by a full run (INV-5 — see projectionClaim).")
	pass := fs.Bool("pass", false, "WHOLE-PASS mode: prove recognition + substrate ONCE at full range for the ENTIRE source catalogue, then reconcile EACH source's projection incrementally from its own prior watermark (projectionFloor). One process replaces the per-source, per-chunk driver that re-ran the load-heaviest global recognition scan (DistinctTopicShapes, ~60s) on EVERY 25k chunk — the nightly timeout that froze the alphabetical tail's verdicts. Because substrate is proven at FULL tip, its write advances the tip and is never blocked by the CS-083 guard (the low-tip substrate flap). Every catalogue source gets a verdict — a never-seeded source reconciles from genesis on its first pass. Requires -ch; incompatible with -source / -from / -skip-substrate / -skip-recognition (the per-chunk knobs it replaces).")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return fmt.Errorf("-config is required")
	}
	// Fail CLOSED on -pass combined with the per-source / per-chunk knobs it
	// replaces (validatePassFlags): a partial pass that publishes over a subset
	// of sources, or carries a stale substrate/recognition verdict, would look
	// complete while re-opening exactly the redundancy + flap this mode fixes.
	if perr := validatePassFlags(*pass, *useCH, *only, *fromLedger, *skipSubstrate, *skipRecognition); perr != nil {
		return fmt.Errorf("compute-completeness: %w", perr)
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
	if *pass {
		fmt.Fprintln(os.Stderr, "compute-completeness: -pass — recognition + substrate proven once at full range; projection reconciled per source from its own watermark")
	}

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
	// Seed the recognition census's soroswap decoder from the SAME pair
	// registry attributeRecognitionGaps folds into ownerOf (loadRegistryOwners
	// → LoadSoroswapPairRegistry). Without this the census dispatcher's soroswap
	// decoder starts with an EMPTY pairTokens map, so Matches() rejects every
	// real SoroswapPair protocol event (swap/sync/deposit/withdraw/skim — a
	// String topic[0], so topic_0_sym=""): each becomes a false "unhandled
	// topic" gap attributed to soroswap even though the indexer decodes + serves
	// them (they ARE the trades). "Attribution knows a pair the census decoder
	// does not" is the exact bug — it also cascade-clamps soroswap's projection
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
			return computeRecognitionGapsCH(ctx, cfg, *chAddr, gatedOpts, uint32(sorobanEraGenesis), tip, soroswapOpts...)
		}
		return computeRecognitionGaps(ctx, store, cfg, gatedOpts, tip, soroswapOpts...)
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
	// W1-flowcompleteness-1: the TOPIC-MATCHED sources (soroswap/aquarius/
	// phoenix/comet/defindex/blend — empty contractIDs) never appeared in
	// ownerOf, so a dropped/altered topic on one of THEIR pools (the phoenix
	// orphaned-swap class) fell into `unattributed` and their per-source
	// recognition axis was STRUCTURALLY unable to fail: recBySource[soroswap]
	// stayed empty and recognition_ok read true over a real drop. Their pool
	// membership already exists in the same registries the recognition gate
	// warms — protocol_contracts (factory-anchored children) and soroswap_pairs
	// — so fold those into ownerOf. Now a gap on a registered pool attributes to
	// its owning source and caps it; a gap on a contract NO source owns stays
	// unattributed and is published by the system `recognition` snapshot below
	// (which this run now refreshes every pass — see that block). FAIL CLOSED on
	// a registry read error, like the prior/floor reads: an owner map missing
	// its registry members would silently restore the pre-fix always-true axis.
	// Only paid when the scan actually found gaps to attribute — a clean or
	// -skip-recognition run has nothing to fold.
	if len(recGaps) > 0 {
		if oerr := loadRegistryOwners(ctx, store, ownerOf); oerr != nil {
			return fmt.Errorf("recognition owner attribution (failing closed — an incomplete owner map cannot fail recognition for the topic-matched sources): %w", oerr)
		}
	}
	recBySource, unattributed := attributeRecognitionGaps(ownerOf, recGaps)

	// Substrate (Claim 1) is a property of the lake, not a source — compute the
	// earliest gap/break ONCE in -ch mode (over the whole Soroban-era range) and
	// reuse per source. The CH lake is the certified authoritative substrate.
	var chSubProblem uint32
	var chSubHas bool
	// subScanFrom is the FLOOR this run's substrate scan actually reached.
	// subScanFrom > tip means "this run scanned no substrate at all"
	// (-skip-substrate). Carried out of the switch because substrateClaim
	// below needs it per source: it is what makes the difference between
	// "proven from genesis" and "trusted below a floor" (C4-057).
	subScanFrom := tip + 1
	switch {
	case *useCH && *skipSubstrate:
		fmt.Fprintln(os.Stderr, "compute-completeness: -skip-substrate — no substrate scan; the prior verdict is carried, not re-proven")
	case *useCH:
		subScanFrom = uint32(2)
		if *fromLedger > 2 {
			subScanFrom = uint32(*fromLedger) //nolint:gosec // ledger seq fits uint32
		}
		p, has, d, serr := clickhouse.SubstrateProblem(ctx, *chAddr, subScanFrom, tip)
		if serr != nil {
			return fmt.Errorf("ch substrate: %w", serr)
		}
		chSubProblem, chSubHas = p, has
		if has {
			fmt.Fprintf(os.Stderr, "compute-completeness: CH substrate problem at %d (%s)\n", p, d)
		} else {
			fmt.Fprintf(os.Stderr, "compute-completeness: CH substrate intact [%d,tip] — contiguous + hash-chained\n", subScanFrom)
		}
	}

	// Prior verdicts (INV-5). An INCREMENTAL run (-from) reconciles only a
	// SUFFIX of each source's served range, so it may CONFIRM or DOWNGRADE the
	// served (`complete`) axis but must NEVER upgrade it: the only evidence for
	// the prefix it did not touch is the previously published verdict. Read
	// those verdicts BEFORE the loop overwrites them, and fail CLOSED on a read
	// error (same discipline as runRecognitionScan) — publishing verdicts while
	// blind to the prior ones is exactly how a `complete=false` silently became
	// `complete=true` (see projectionClaim).
	priorSnaps, err := store.ListCompletenessSnapshots(ctx)
	if err != nil {
		return fmt.Errorf("prior completeness verdicts (failing closed — an incremental run cannot gate its claim without them): %w", err)
	}
	priorProj := make(map[string]priorProjection, len(priorSnaps))
	priorSub := make(map[string]priorProjection, len(priorSnaps))
	// priorWatermark is each source's last published lake watermark, used as the
	// per-source projection floor in -pass mode (projectionFloor): a whole-pass
	// run resumes every source from where it left off, so substrate + recognition
	// stay proven once at full range while only the projection reconcile is
	// scoped. A source absent here (0 — never seeded) floors at genesis. This is
	// the same value the per-source wrapper read as its -from.
	priorWatermark := make(map[string]uint32, len(priorSnaps))
	for _, s := range priorSnaps {
		priorProj[s.Source] = priorProjection{known: true, ok: s.ProjectionOK, tip: s.Tip}
		priorWatermark[s.Source] = s.Watermark
		// C4-057: the SUBSTRATE axis needs the same prior-verdict input the
		// projection axis has had since INV-5, for exactly the same reason —
		// an incremental run scans only a suffix and must not publish a
		// genesis-to-tip claim off it. See substrateClaim.
		priorSub[s.Source] = priorProjection{known: true, ok: s.SubstrateOK, tip: s.Tip}
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
	// claims over ranges a replay has rewritten, which is exactly how the
	// 07-30 cctp replay's 19,366 over-projected rows escaped the verifier
	// (the 2026-07-31 carried-claim invalidation gap).
	dirtyWindows, err := store.ProjectionDirtyWindows(ctx)
	if err != nil {
		return fmt.Errorf("pending replay-rewind dirty windows (failing closed — carrying a projection claim over a rewound range is the invalidation this record exists to prevent): %w", err)
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
			// problem only if the reported problem ledger falls at/after the
			// source's genesis. SubstrateProblem returns coverage-correct
			// problem ledgers for empty/head/tail absences (see its
			// substrateHeadProblem doc) so a high-genesis source can't read a
			// COVERAGE failure as "below my genesis, I'm fine" (F1 fail-open).
			scanClean := sourceSubstrateOK(chSubProblem, chSubHas, genesis)
			if !scanClean {
				problems = append(problems, chSubProblem)
			}
			// C4-057: gate what the run may PUBLISH on the range it actually
			// scanned. The scan floor and the claim are different things and
			// only the projection axis used to know that.
			var subDetail string
			substrateOK, subDetail = substrateClaim(genesis, tip, subScanFrom, scanClean, chSubProblem, priorSub[src.name])
			detail = append(detail, subDetail)
			// C4-057 (numeric-field gap): substrateClaim can refuse a CLEAN
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
			// W1-flowcompleteness-2: the substrate twin of detectFloorLoss.
			// An incremental run scanned only [subScanFrom, tip] and
			// substrateClaim rule 3 CARRIES the prior clean [genesis,
			// subScanFrom] verdict — so a capacity-archive DROP PARTITION (the
			// documented archive-to-S3 plan) that deletes the source's oldest
			// ledgers BELOW subScanFrom is invisible to the scan, and
			// substrate_ok / lake_complete stay true over an absent prefix.
			// Projection catches its own bottom-edge loss (detectFloorLoss +
			// completeness_target_floors, migration 0116); substrate had no
			// equivalent. Probe the bottom edge directly: unlike the projection
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

		// Claim 2a: recognition gaps attributed to this source's contracts
		// (static contractIDs OR the factory-child / soroswap-pair registry
		// members folded into ownerOf above — W1-flowcompleteness-1).
		recOK, recProblems := sourceRecognitionOK(genesis, recBySource[src.name])
		problems = append(problems, recProblems...)
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
		//
		// C4-057: it IS gated by substrateOK - a claim about the range this
		// run scanned rather than a raw scan result. And because the
		// lakeCoverageProblem floor is already in `problems` above, so are the
		// NUMERIC fields: an incremental run that scanned only [from,tip]
		// cleanly but cannot prove the prefix (no/failing/stale prior) now
		// pins srW.Ledger / CoveragePct / FirstProblem to the
		// proven-from-genesis extent instead of reading genesis-to-tip off a
		// suffix. srW.Complete and substrateOK therefore agree by
		// construction; `&& substrateOK` is kept as a belt-and-suspenders
		// guard and `detail` still states the floor.
		lakeComplete := srW.Complete && substrateOK
		projOK := false
		var w completeness.Watermark
		// Incremental: only reconcile [projFrom, srW.Ledger], trusting
		// [genesis, projFrom] as previously verified. In -pass mode projFrom is
		// THIS source's own prior watermark, so substrate + recognition stay
		// proven once at full range for the whole pass while only projection is
		// scoped per source (projectionFloor). Outside -pass it is the existing
		// global max(genesis, -from) incremental floor — byte-for-byte unchanged.
		projFrom := projectionFloor(genesis, *pass, priorWatermark[src.name], *fromLedger)
		// A pending replay-rewind window overrides the incremental floor:
		// the replay rewrote served rows below the watermark, so the range
		// MUST be re-reconciled before any claim — carried or fresh — may
		// cover it. -from can never skip past it.
		dirtyWin, hasDirty := dirtyWindows[src.name]
		if hasDirty {
			projFrom = dirtyReconcileFloor(projFrom, genesis, dirtyWin)
		}
		if *useCH {
			if srW.Ledger >= projFrom {
				streamer := clickhouse.ReconcileEventStreamer{Addr: *chAddr, NeedOpArgs: src.needsOpArgs, NeedStateWriteKeys: src.needsStateWriteKeys}
				scopes, servedMins, servedFrom, runFrom, serr := projectionScopes(ctx, store, src, genesis, projFrom, srW.Ledger)
				if serr != nil {
					return fmt.Errorf("%s: served floor: %w", src.name, serr)
				}
				// N-F2 residual: a bottom-edge truncation is invisible to the
				// reconcile itself, because the reconcile's own floor moves with
				// it. Compare against the durable floor BEFORE reconciling, and
				// treat loss as a hard projection failure — the surviving rows
				// will reconcile perfectly and would otherwise read complete.
				floorLoss := detectFloorLoss(src, servedMins, targetFloors)
				delta, blind, pdetail, perr := reconcileProjectionAggregate(ctx, store, streamer, *chAddr, src, scopes)
				if perr != nil {
					return fmt.Errorf("%s: projection: %w", src.name, perr)
				}
				// Only a clean reconcile earns the right to record verified
				// ground; a run that found a mismatch must not enshrine its
				// range as verified. A run that DETECTED loss must not record
				// either, or it would immediately adopt the post-loss floor as
				// the new truth and erase the evidence on the very next run.
				//
				// C4-059: a blind re-derive earns no ground either. Its zero
				// delta is an artifact of both sides dropping the same rows,
				// so recording the floor would enshrine an unverified range.
				if delta == 0 && len(floorLoss) == 0 && !blind.Any() {
					if ferr := recordFloors(ctx, store, src, scopes, servedMins); ferr != nil {
						return fmt.Errorf("%s: record projection floors: %w", src.name, ferr)
					}
				}
				// The scope travels WITH the verdict: a run may only claim the
				// range it actually reconciled (ADR-0033 — a source is complete
				// through W iff every claim holds contiguously to W).
				//
				// C4-059: `delta == 0 && !blind.Any()` is the honest "clean"
				// predicate. A ledger whose rows neither side could decode
				// contributes 0 to delta while proving nothing, so a bare
				// delta==0 would certify projection_ok on exactly the ledgers
				// the check is blind to. pdetail already leads with the
				// blind-spot summary (reconcileProjectionAggregate).
				var claimDetail string
				projOK, claimDetail = projectionClaim(servedFrom, runFrom, srW.Ledger, delta == 0 && !blind.Any(), pdetail, priorProj[src.name])
				detail = append(detail, claimDetail)
				if len(floorLoss) > 0 {
					projOK = false
					detail = append(detail, floorLoss...)
				}
			} else {
				detail = append(detail, "projection: not evaluated (earlier claim failed at genesis)")
			}
			// Coverage = substrate∧recognition (proven data capture). complete
			// additionally requires the served-tier projection to reconcile;
			// lakeComplete (set above, pre-projection) never does. The served
			// axis can never be stronger than the lake axis it sits on, so an
			// unproven substrate claim (C4-057) gates it too — that is what
			// lakeComplete already carries.
			w = combineWatermark(srW, lakeComplete && projOK)
		} else {
			// Legacy Postgres path: strict per-ledger projection pins the watermark.
			if srW.Ledger >= genesis {
				pgaps, pblind, perr := reconcileSourceProjection(ctx, store, nil, src, genesis, srW.Ledger)
				if perr != nil {
					return fmt.Errorf("%s: projection: %w", src.name, perr)
				}
				projOK = len(pgaps) == 0 && !pblind.Any()
				problems = append(problems, pgaps...)
				problems = append(problems, pblind.Ledgers...)
				// C4-059: name the two classes separately. A count mismatch
				// and a ledger the re-derive could not READ are different
				// findings with different fixes, and folding blind ledgers
				// into "N mismatched ledger(s)" sends an operator hunting
				// for a row-count discrepancy that does not exist.
				if len(pgaps) > 0 {
					detail = append(detail, fmt.Sprintf("projection: %d mismatched ledger(s) in [%d,%d]", len(pgaps), genesis, srW.Ledger))
				}
				if d := pblind.Detail(); d != "" {
					detail = append(detail, "projection: "+d)
				}
			} else {
				detail = append(detail, "projection: not evaluated (earlier claim failed at genesis)")
			}
			w = completeness.ComputeWatermark(genesis, tip, problems)
		}

		// Replay-rewind window disposition. The clear is earned by the
		// reconcile itself, not the carry: only a run whose CLEAN projection
		// verdict actually covered the whole window may delete it. A dirty or
		// short run leaves the window pending so every subsequent run keeps
		// extending its floor over the rewound range until one verifies it.
		var dirtyCleared bool
		if hasDirty {
			reconcileFloor := projFrom
			if !*useCH {
				// The legacy PG path always reconciles the full
				// [genesis, watermark] range — its floor is genesis.
				reconcileFloor = genesis
			}
			dirtyCleared = dirtyWindowSatisfied(dirtyWin, projOK, reconcileFloor, genesis, srW.Ledger)
			if dirtyCleared {
				detail = append(detail, fmt.Sprintf(
					"projection: replay-rewind window [%d,%d] re-verified clean this run — clearing it",
					dirtyWin.From, dirtyWin.To))
			} else {
				detail = append(detail, fmt.Sprintf(
					"projection: replay-rewind window [%d,%d] PENDING re-verification — the reconcile floor stays extended over it until a clean run covers it",
					dirtyWin.From, dirtyWin.To))
			}
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
		if dirtyCleared {
			// Bounded delete (ClearProjectionDirtyWindow's WHERE): if a
			// concurrent replay WIDENED the window between this run's read
			// and now, the widened row survives and the next run re-checks
			// the new ground.
			if cerr := store.ClearProjectionDirtyWindow(ctx, src.name, dirtyWin.From, dirtyWin.To); cerr != nil {
				return fmt.Errorf("%s: clear replay-rewind dirty window: %w", src.name, cerr)
			}
		}
		fmt.Fprintf(os.Stderr, "compute-completeness: %-14s watermark=%d coverage=%.4f complete=%v lake_complete=%v (%s)\n",
			src.name, w.Ledger, w.CoveragePct, w.Complete, lakeComplete, strings.Join(detail, "; "))
	}

	// ── System recognition snapshot (gaps on contracts no source owns) ──
	//
	// W1-flowcompleteness-1: this used to be gated on `*only == ""`, so the
	// DEPLOYED daily driver (run-compute-completeness.sh always passes
	// -source $SRC) NEVER refreshed it — the global scan detected an
	// unattributed gap and then discarded it, leaving /v1/coverage's recognition
	// row stale. The scan is already global on every per-source pass (line ~163,
	// the driver passes no -skip-recognition) and `unattributed` is already
	// fully computed, so refreshing the system snapshot every pass is free and
	// restores the system-level alarm for gaps on contracts no source owns. The
	// `-source` filter still governs which PER-SOURCE rows are written; it must
	// not silence the system-wide recognition verdict.
	if !*skipRecognition {
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

// projectionFloor is the incremental projection reconcile floor for ONE source.
//
// In a whole-pass run (-pass) each source resumes from its OWN prior watermark,
// so recognition (the ~60s global DistinctTopicShapes scan) and the full
// substrate scan are proven ONCE for the entire pass while only the per-source
// projection reconcile is scoped to the source's new suffix. That is what lets a
// single process replace the per-source, per-chunk wrapper that re-ran those two
// global scans on EVERY 25k chunk — the load that timed the nightly service out
// and froze the alphabetical tail's verdicts. A healthy source floors at its
// watermark (≈ prior tip), reconciling only the new suffix, so its verdict is
// identical to the wrapper's `-from = watermark` run. A source with NO prior
// watermark (0 — a never-seeded blend_emitter/blend_backstop/sorocredit, or a
// freshly reset source) floors at genesis and its first pass does the
// full-history reconcile that seeds it, so every catalogue source gets a verdict.
//
// Outside -pass it is the existing max(genesis, -from) incremental floor, so the
// per-chunk driver's behaviour is unchanged. Never below genesis (nothing exists
// there to reconcile). Pure — unit-testable.
func projectionFloor(genesis uint32, pass bool, priorWatermark uint32, fromLedger uint) uint32 {
	if pass {
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
//     skipping the full-tip proof is what re-opens the CS-083 low-tip flap.
//
// -pass also requires -ch: the full-tip substrate proof and the projection
// re-derive it relies on are certified-ClickHouse-lake operations. Rejecting
// these combinations rather than silently ignoring them keeps an operator (or a
// mis-edited wrapper) from publishing a partial pass that looks complete. Pure.
func validatePassFlags(pass, useCH bool, source string, fromLedger uint, skipSubstrate, skipRecognition bool) error {
	if !pass {
		return nil
	}
	switch {
	case !useCH:
		return fmt.Errorf("-pass requires -ch (the whole-pass substrate proof + projection re-derive read the certified ClickHouse lake)")
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

// loadRegistryOwners folds the factory-child (protocol_contracts) and
// soroswap-pair registries into ownerOf so the TOPIC-MATCHED sources (empty
// contractIDs) can attribute — and therefore FAIL — recognition on a dropped
// topic on one of their own pools (W1-flowcompleteness-1). These are the SAME
// membership sets the recognition gate warms (GatedRegistryOptions /
// seedSoroswapForRecon), so the data is already the source of truth at compute
// time; this only makes the completeness verdict read it too. FAIL CLOSED on a
// read error — the caller must not proceed with a partial owner map.
//
// The DB reads live here; the pure fold is [mergeRegistryOwners] so the
// attribution → verdict chain is unit-testable without a live store.
func loadRegistryOwners(ctx context.Context, store *timescale.Store, ownerOf map[string]string) error {
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
// static contractIDs is never reassigned — the static catalogue pin wins, so a
// curated contract-pinned source keeps its exact attribution. Pure —
// unit-testable. The "soroswap" literal mirrors the catalogue entry's own
// literal name (reconciliation_catalogue.go).
func mergeRegistryOwners(ownerOf map[string]string, gatedChildren map[string][]string, soroswapPairs []string) {
	for source, ids := range gatedChildren {
		for _, c := range ids {
			if _, taken := ownerOf[c]; !taken {
				ownerOf[c] = source
			}
		}
	}
	for _, p := range soroswapPairs {
		if _, taken := ownerOf[p]; !taken {
			ownerOf[p] = "soroswap"
		}
	}
}

// attributeRecognitionGaps splits the global recognition scan's gaps into the
// per-source (owned) map and the unattributed remainder (gaps on contracts no
// source owns), using ownerOf. A gap on an owned contract caps its owning
// source's recognition axis; an unattributed gap flows to the system
// `recognition` snapshot. Pure — unit-testable.
func attributeRecognitionGaps(ownerOf map[string]string, gaps []completeness.RecognitionGap) (map[string][]uint32, []completeness.RecognitionGap) {
	recBySource := map[string][]uint32{}
	var unattributed []completeness.RecognitionGap
	for _, g := range gaps {
		if owner, ok := ownerOf[g.ContractID]; ok {
			recBySource[owner] = append(recBySource[owner], g.MinLedger)
		} else {
			unattributed = append(unattributed, g)
		}
	}
	return recBySource, unattributed
}

// sourceRecognitionOK is the pure per-source Claim-2a verdict: a source fails
// recognition (returns false) when any recognition gap attributed to it falls
// at or after its genesis, and returns those problem ledgers to fold into the
// coverage watermark. attributed is recBySource[source] — which, since
// W1-flowcompleteness-1, includes gaps on the source's factory-child /
// soroswap-pair registry members, not just its static contractIDs. Pure.
func sourceRecognitionOK(genesis uint32, attributed []uint32) (bool, []uint32) {
	ok := true
	var problems []uint32
	for _, l := range attributed {
		if l >= genesis {
			problems = append(problems, l)
			ok = false
		}
	}
	return ok, problems
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

// projectionScope is the ledger range a Claim-2b reconcile ACTUALLY covered for
// one target. It travels with the verdict so a run can never publish
// completeness over ledgers it did not reconcile (ADR-0033: a source is
// complete through W iff every claim holds contiguously to W).
type projectionScope struct {
	From uint32 // inclusive
	To   uint32 // inclusive
}

// targetScope derives one target's reconcile range from the SERVED TIER'S OWN
// DATA — servedMin is `MIN(ledger)` of that target over [genesis, hi], and
// haveServedRows is false when the target holds nothing in range.
//
// This REPLACES the hardcoded `retentionStart = tip - 1_500_000` floor
// (DAT-09 / N-F2). That constant was a stale assumption: `trades` has had NO
// retention policy since migration 0031 ("operator wants every raw trade
// preserved forever"), so the served tier keeps full history while the verdict
// only ever reconciled the last ~1.5M ledgers (~100 days). Served-tier loss
// OLDER than that was structurally invisible to the `complete` axis. Worse, the
// floor was applied at SOURCE level, so a trades source's FULL-HISTORY targets
// (soroswap_skim_events, phoenix_liquidity/phoenix_stake_events,
// comet_liquidity) were silently un-verified below it too. Per TARGET is the
// correct granularity: each table is checked over exactly what the served tier
// holds for it, whether that is full history or a never-backfilled prefix
// (soroswap/sdex trades begin ~61.5M — see
// notes/DECISION-genesis-complete-verdict-2026-07-16.md, which lists this fix
// as decision item 3).
//
// An EMPTY target floors at `genesis` — fail CLOSED. A wiped table must
// reconcile expected>0 against served=0 and FAIL; "there is no data, so there
// is nothing to check" is the fail-open this whole verdict exists to prevent.
//
// runFrom is the incremental -from floor (0 for a full run): it can only RAISE
// the scope, and whatever it excludes is handled by projectionClaim, never
// silently claimed.
//
// NO RECONCILE TARGET HAS A RETENTION POLICY, verified against both the
// migrations and r1 (2026-07-25). This comment used to cite oracle_updates as
// "a real drop_chunks boundary, 90d per migration 0003"; migration 0040
// removed that policy, and 0031 removed retention from trades / prices_1m /
// prices_15m. The only surviving add_retention_policy in the tree is
// api_usage_events (12 months, migration 0027), which is not a reconcile
// target. That matters for anyone extending this: it means a RISING servedMin
// is unambiguously LOSS, with no legitimate drop_chunks case to exempt — so
// the durable-floor fix below does not need per-target retention windows. The
// stale version of this sentence very nearly produced exactly that
// unnecessary branch.
//
// A BOTTOM-EDGE truncation is self-erasing HERE and cannot be fixed in this
// function: if the oldest served rows are deleted (a rogue retention policy
// re-appearing on `trades` is the exact drift migration 0031's down-migration
// warns about), MIN(ledger) rises with the loss and this scope follows it up.
// Distinguishing "lost" from "never projected" needs a floor the served tier
// cannot rewrite, which is why migration 0116's completeness_target_floors and
// [detectFloorLoss] exist — that comparison, not this scope, is what fails the
// projection axis on bottom-edge loss. This function's floor stays
// data-derived on purpose: it decides what to RECONCILE, and reconciling below
// where the served tier holds rows would manufacture false gaps.
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

// projectionScopes resolves every target's reconcile scope against the served
// tier, and returns (a) the scopes, parallel to src.targets, (b) servedFrom —
// the lowest ledger the served tier holds for ANY of this source's targets,
// i.e. the full range the served axis claims to be faithful over — and (c)
// runFrom, where THIS run's reconcile actually starts. servedFrom < runFrom is
// exactly the "-from skipped a range" case projectionClaim gates.
//
// One indexed MIN(ledger) per target (trades_source_ledger_idx et al); the same
// query buildClassicGapGate already runs from genesis in production.
func projectionScopes(ctx context.Context, store *timescale.Store, src reconSource, genesis, runFrom, hi uint32) (scopes []projectionScope, servedMins []servedFloor, servedFromOut, runFromOut uint32, err error) {
	if len(src.targets) == 0 {
		return nil, nil, genesis, genesis, nil
	}
	scopes = make([]projectionScope, len(src.targets))
	servedMins = make([]servedFloor, len(src.targets))
	servedFrom, runLo := hi, hi
	for i, tgt := range src.targets {
		minL, ok, err := store.MinLedger(ctx, tgt.table, "ledger", tgt.whereFilter, genesis, hi)
		if err != nil {
			return nil, nil, 0, 0, fmt.Errorf("%s min served ledger: %w", tgt.table, err)
		}
		servedMins[i] = servedFloor{min: minL, present: ok}
		if served := targetScope(minL, ok, genesis, 0, hi).From; served < servedFrom {
			servedFrom = served
		}
		scopes[i] = targetScope(minL, ok, genesis, runFrom, hi)
		if scopes[i].From < runLo {
			runLo = scopes[i].From
		}
	}
	return scopes, servedMins, servedFrom, runLo, nil
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

// detectFloorLoss compares each target's LIVE bottom edge against the durable
// floor recorded by previous runs (completeness_target_floors, migration
// 0116) and returns a detail line per target whose oldest rows have gone
// missing.
//
// This is the check targetScope structurally cannot make. targetScope floors
// the reconcile at MIN(ledger) of the target itself, so when the oldest rows
// are deleted the floor RISES with the loss and the surviving rows reconcile
// perfectly — "someone dropped the oldest 10M ledgers" and "we never
// projected below there" produce identical verdicts. Only a floor the served
// tier cannot rewrite can tell them apart.
//
// A target with no recorded floor is skipped, not failed: that is the
// first-run case, and treating an absent row as floor=0 would report
// loss-below-zero for every target at once on the first run after the
// migration.
//
// An EMPTY target that previously had a floor is the maximal case — every
// row below the floor is gone — and is reported as such rather than skipped.
func detectFloorLoss(src reconSource, servedMins []servedFloor, floors map[string]timescale.CompletenessTargetFloor) []string {
	var out []string
	for i, tgt := range src.targets {
		if i >= len(servedMins) {
			break
		}
		prior, ok := floors[timescale.TargetFloorKey(src.name, tgt.table, tgt.whereFilter)]
		if !ok {
			continue // no floor recorded yet — nothing to compare against
		}
		sm := servedMins[i]
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

// floorsToRecord is what a clean run has EVIDENCE for, per target — the pure
// half of recordFloors.
//
// It records scopes[i].From — the ledger the reconcile genuinely started at —
// NOT the target's raw MIN(ledger). On a narrowed `-from` run those differ,
// and claiming to have verified from MIN when the run began higher would be a
// lie the next run then trusts. The store applies LEAST(), so a narrowed run's
// higher value is a harmless no-op and only a genuinely deeper run lowers the
// floor.
//
// A target holding NO served rows is SKIPPED, and that skip is the whole
// reason this is a separate function. targetScope floors an empty target at
// `genesis` (fail closed, so expected>0 vs served=0 reconciles as loss), and
// recording THAT as a verified floor asserts something the run never saw: a
// clean reconcile of an empty target proves only "0 expected, 0 served", not
// "this table's rows have been verified present from genesis". The next run
// where the table legitimately acquires its first row at ledger L then reads
// MIN=L > genesis and detectFloorLoss reports L−genesis ledgers "GONE" — a
// permanent false projection failure, because UpsertCompletenessTargetFloor's
// LEAST() can never raise the floor back and detectFloorLoss's own verdict
// blocks re-recording. Every late-starting target reaches production through
// exactly this path: a source whose first event has not happened yet (a rare
// skim, a newly watched SEP-41 contract promoted into the catalogue). No rows
// means no evidence about where the rows begin — so record nothing and let
// the first run that actually sees rows establish the floor.
func floorsToRecord(src reconSource, scopes []projectionScope, servedMins []servedFloor) []timescale.CompletenessTargetFloor {
	out := make([]timescale.CompletenessTargetFloor, 0, len(src.targets))
	for i, tgt := range src.targets {
		if i >= len(scopes) || i >= len(servedMins) {
			break
		}
		if !servedMins[i].present {
			continue
		}
		out = append(out, timescale.CompletenessTargetFloor{
			Source:       src.name,
			Table:        tgt.table,
			Filter:       tgt.whereFilter,
			VerifiedFrom: scopes[i].From,
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
}

// projectionClaim gates what a run is ALLOWED to publish on the served
// (`complete`) axis, given the range it ACTUALLY reconciled — the structural
// fix for INV-5 (a verdict that silently regressed from complete=false to
// complete=true).
//
// The regression was real and in the hot path: completeness-incremental.sh
// passes `-from = min(watermark)`, but watermark_ledger is the LAKE
// (substrate∧recognition) axis, which sits AT tip whenever the lake is clean.
// So the hourly run reconciled only the newest ~hour of ledgers, never re-saw
// the projection mismatch that had pinned `complete=false`, and wrote
// complete=true — a verdict improving with no evidence, which ADR-0033 forbids
// (complete through W requires every claim to hold contiguously to W).
//
// Rules, fail-closed, in order:
//  1. A mismatch found by THIS run always fails — nothing can launder it.
//  2. A run whose reconcile started at or below servedFrom covered the WHOLE
//     served range: self-evidencing, may publish true. This is the only way a
//     failing verdict is ever cleared — deliberately, by a full re-verify.
//  3. A partial (incremental) run may CARRY FORWARD a prior clean verdict for
//     the prefix it skipped, but only if that prior verdict is contiguous with
//     this run's window (prior.tip+1 >= runFrom). Confirm, never upgrade.
//  4. Anything else — no prior verdict, a FAILING prior verdict, or a stale
//     prior that leaves an unverified band — publishes false.
//
// The returned detail ALWAYS states the range actually verified, so
// `complete=true` can never be read as a genesis-to-tip claim (DAT-09: the
// served tier legitimately holds no trades below ~61.5M; the genesis claim is
// the separate lake_complete axis).
func projectionClaim(servedFrom, runFrom, hi uint32, runClean bool, runDetail string, prior priorProjection) (bool, string) {
	if !runClean {
		return false, "projection: " + runDetail
	}
	if runFrom <= servedFrom {
		return true, fmt.Sprintf("projection: verified [%d,%d] — the full range the served tier holds", servedFrom, hi)
	}
	skipped := fmt.Sprintf("[%d,%d]", servedFrom, runFrom-1)
	switch {
	case !prior.known:
		return false, fmt.Sprintf("projection: verified only [%d,%d]; %s was NOT reconciled by this run and no prior verdict exists to carry — not claiming it (re-run without -from)", runFrom, hi, skipped)
	case !prior.ok:
		return false, fmt.Sprintf("projection: verified only [%d,%d]; %s was NOT reconciled by this run and the prior verdict's projection was FAILING — refusing to upgrade without evidence (re-run without -from)", runFrom, hi, skipped)
	case runFrom > prior.tip+1:
		return false, fmt.Sprintf("projection: verified only [%d,%d]; the prior clean verdict only reached tip=%d, leaving [%d,%d] verified by nobody — not claiming it (re-run without -from)", runFrom, hi, prior.tip, prior.tip+1, runFrom-1)
	default:
		return true, fmt.Sprintf("projection: verified [%d,%d]; %s carried from the prior clean verdict (tip=%d), not re-verified this run", runFrom, hi, skipped, prior.tip)
	}
}

// dirtyReconcileFloor lowers an incremental run's projection reconcile floor
// to cover a pending replay-rewind dirty window (migration 0125) — the
// structural fix for the carried-claim invalidation gap (2026-07-31).
//
// projectionClaim rule 3 lets an incremental run CARRY the prior clean
// verdict for the prefix its -from floor skipped. That carry's premise is
// that the served tier below the floor is immutable. A projector-replay
// rewind breaks the premise: it rewrites served rows below the watermark,
// so the prior verdict's evidence no longer describes what the tables hold —
// yet nothing re-examined the range, because the daily driver's -from
// (min(watermark)) sits above it forever. The 07-30 cctp replay wrote
// 19,366 duplicate rows at 62.27M–63.55M and every subsequent incremental
// run carried the pre-replay clean claim right over them.
//
// The floor therefore extends DOWN to the window's genesis-clamped bottom,
// regardless of -from: the rewound range re-enters the reconcile scope and
// the claim is re-earned rather than carried. Ground the replay did not
// touch (below dirty.From) keeps the normal carry semantics. Pure —
// unit-testable.
func dirtyReconcileFloor(projFrom, genesis uint32, w timescale.ProjectionDirtyWindow) uint32 {
	lo := w.From
	if lo < genesis {
		lo = genesis // nothing exists below the source's genesis to re-verify
	}
	if lo < projFrom {
		return lo
	}
	return projFrom
}

// dirtyWindowSatisfied reports whether THIS run earned the right to clear a
// pending replay-rewind window: its projection verdict is CLEAN (projOK —
// which per projectionClaim requires this run's own reconcile to have found
// nothing, never a carried claim over an unchecked range) AND the run's
// reconcile floor reached the window's genesis-clamped bottom AND the
// reconciled range reached the window's top. Anything less keeps the window
// pending — a failing verdict must not erase the obligation, and a run whose
// scope stopped short of the window proved nothing about it. Pure —
// unit-testable.
func dirtyWindowSatisfied(w timescale.ProjectionDirtyWindow, projOK bool, reconcileFloor, genesis, hi uint32) bool {
	lo := w.From
	if lo < genesis {
		lo = genesis
	}
	return projOK && reconcileFloor <= lo && hi >= w.To
}

// substrateClaim gates what a run is ALLOWED to publish on the LAKE
// (`substrate_ok` → `lake_complete`) axis, given the range its substrate
// scan ACTUALLY covered. It is the exact twin of [projectionClaim], applied
// to the claim that INV-5 left ungated (C4-057).
//
// substrateClaim returns only the BOOLEAN verdict; its numeric twin
// [lakeCoverageProblem] feeds the unproven-prefix floor into the coverage
// watermark's problem set, so coverage_pct / watermark_ledger are gated in
// lockstep with substrate_ok (the half of C4-057 the boolean alone left open).
//
// The gap it closes: `computeCompleteness` scans substrate over
// [scanFrom, hi], where scanFrom is the `-from` incremental floor — but
// published `lake_complete` / `coverage_pct` over [genesis, hi]. On a clean
// suffix the `problems` slice comes back empty, so the watermark reads
// genesis-to-tip and the verdict asserts "the certified archive is
// contiguous + hash-chained from genesis" on evidence covering only the
// newest window. Worse, it is an UPGRADE path: the production driver
// (run-compute-completeness.sh) re-runs each source from its prior
// watermark on a daily timer, so a source pinned at substrate_ok=false by a
// real gap below the floor silently flipped to true on the next pass —
// the same regression shape ADR-0033 forbids and projectionClaim already
// blocks on the other axis.
//
// The `-from` flag's own help text discloses that it trusts
// [genesis, from]; the SERVED verdict did not. Now it does: the returned
// detail always states the range this run verified and where the rest came
// from, and `detail` is on the wire (`GET /v1/coverage` → `detail`), so a
// consumer can tell proven-from-genesis from trusted-below-a-floor.
//
// Rules, fail-closed, in order:
//  1. A problem found by THIS run always fails — nothing launders it.
//  2. A scan that started at or below the source's genesis covered the
//     WHOLE range the claim is about: self-evidencing, may publish true.
//     This is the only way a failing lake verdict is ever cleared —
//     deliberately, by a full re-verify.
//  3. A partial run may CARRY FORWARD a prior clean verdict for the prefix
//     it skipped, but only if that prior verdict is contiguous with this
//     run's window (prior.tip+1 >= scanFrom). Confirm, never upgrade.
//  4. Anything else — no prior verdict, a FAILING prior verdict, or a
//     stale prior that leaves an unverified band — publishes false.
//
// scanFrom > hi encodes "this run scanned no substrate at all"
// (-skip-substrate). It falls through to rules 3/4 naturally: the operator
// gets the prior verdict carried when the prior already reached this tip,
// and an honest false when it did not. That is a deliberate tightening —
// the flag previously asserted substrate_ok=true unconditionally, which
// upgraded a FAILING prior verdict to passing with zero evidence.
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

// substrateFloorLoss is the bottom-edge-loss detector for the SUBSTRATE axis —
// the twin of [detectFloorLoss] on the projection axis
// (completeness_target_floors, migration 0116), closing W1-flowcompleteness-2.
//
// An incremental run scans only [subScanFrom, tip] and [substrateClaim] rule 3
// CARRIES the prior clean [genesis, subScanFrom] verdict. A capacity-archive
// DROP PARTITION that deletes the source's oldest ledgers below subScanFrom is
// therefore invisible: the [subScanFrom, tip] scan is intact and the carried
// prefix is a lie, yet substrate_ok / lake_complete / coverage_pct=1.0 keep
// asserting "the certified archive is contiguous + hash-chained from genesis".
//
// Unlike the projection floor — a served-tier MIN that must be recorded durably
// to tell "lost" from "never written" — the substrate floor is the FIXED
// src.genesis, the first-possible-data ledger the lake must always reach, so no
// durable row is needed: the caller simply probes whether the lake still holds
// `genesis`. floorHasProblem is that probe's result (a cheap 1-ledger
// clickhouse.SubstrateProblem at genesis); a present genesis is clean, an
// absent/broken one is bottom-edge loss.
//
// Fires ONLY when the run CARRIES a prefix (subScanFrom > genesis): a deep scan
// (subScanFrom <= genesis) already re-reads genesis and its own head-presence
// guard reports the same loss, so probing there would double-count. Like
// detectFloorLoss this catches a rising BOTTOM edge (dropped oldest partitions),
// not an interior hole inside the carried prefix — that needs a full re-scan and
// is the same class the projection floor detector also cannot see. Pure —
// unit-testable.
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

// lakeCoverageProblem is the NUMERIC twin of [substrateClaim]: it returns the
// ledger to inject into the coverage watermark's problem set when the substrate
// CLAIM refuses an otherwise-clean suffix scan, so coverage_pct /
// watermark_ledger / first_problem track substrate_ok exactly as srW.Complete
// does. It closes the half of C4-057 that substrateClaim's BOOLEAN could not:
// on a clean suffix `problems` holds no substrate ledger, so without this the
// watermark read genesis-to-tip (coverage_pct=1.0, watermark=tip) while
// substrate_ok / lake_complete were already false — a consumer seeing
// "verified to tip" next to "substrate unproven".
//
// Returns 0 (nothing to inject) when the claim HOLDS (substrateOK) or when the
// raw scan already surfaced the problem (!scanClean — that ledger is fed to
// `problems` separately, and it is more precise). Otherwise the claim failed on
// a clean suffix, mirroring substrateClaim's rules 4a/4b/4c:
//   - a clean prior that reached prior.tip proved [genesis, prior.tip]
//     contiguously (rule 4c) → the unverified band opens at prior.tip+1;
//   - a missing or FAILING prior proves nothing from genesis (rules 4a/4b) →
//     the floor is genesis itself.
//
// The floor is clamped to >= genesis so ComputeWatermark (which ignores
// problems below genesis) can never silently drop it and restore the 1.0 lie.
// In the only case the prior.tip+1 branch fires (substrateClaim rule 4c) the
// caller has scanFrom > prior.tip+1 with scanFrom <= tip+1, so prior.tip+1 <= tip
// and the floor lands inside [genesis, tip]. Pure — unit-testable.
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

// reconcileSourceProjection reconciles every table a source writes over
// [genesis, hi] and returns the union of mismatched ledgers. SDEX uses
// the LCM census; event sources re-derive (by kind) and project each
// table's kinds.
func reconcileSourceProjection(ctx context.Context, store *timescale.Store, chStreamer completeness.EventStreamer, src reconSource, genesis, hi uint32) ([]uint32, completeness.BlindSpots, error) {
	var mismatched []uint32
	if src.census {
		expected, eerr := store.ClassicTradeEffectCountsByLedger(ctx, genesis, hi)
		if eerr != nil {
			return nil, completeness.BlindSpots{}, eerr
		}
		for _, tgt := range src.targets {
			actual, aerr := store.CountRowsByLedger(ctx, tgt.table, "ledger", tgt.whereFilter, genesis, hi)
			if aerr != nil {
				return nil, completeness.BlindSpots{}, aerr
			}
			for _, g := range completeness.ReconcileCounts(expected, actual) {
				mismatched = append(mismatched, g.Ledger)
			}
		}
		return mismatched, completeness.BlindSpots{}, nil
	}

	// Re-derive expected outputs: from the CH lake (certified, off the serving
	// DB) when -ch, else from Postgres soroban_events.
	var byKind map[string]map[uint32]int
	var blind completeness.BlindSpots
	var derr error
	if chStreamer != nil {
		byKind, blind, derr = completeness.ReDeriveOutputCountsByKindFromEvents(ctx, chStreamer, src.dec, src.contractIDs, src.topic0Syms, genesis, hi)
	} else {
		byKind, blind, derr = completeness.ReDeriveOutputCountsByKind(ctx, store, src.dec, src.contractIDs, src.topic0Syms, genesis, hi)
	}
	if derr != nil {
		return nil, completeness.BlindSpots{}, derr
	}
	// C4-059: a ledger the re-derive could not decode is a ledger this
	// function cannot certify — the projector dropped the same rows, so the
	// per-ledger diff nets to zero and reads clean. Returned SEPARATELY from
	// the count mismatches: both pin the watermark, but they are different
	// findings and the caller's verdict names them apart.
	for _, tgt := range src.targets {
		expected := completeness.SumKinds(byKind, tgt.kinds...)
		actual, aerr := store.CountRowsByLedger(ctx, tgt.table, "ledger", tgt.whereFilter, genesis, hi)
		if aerr != nil {
			return nil, completeness.BlindSpots{}, aerr
		}
		for _, g := range completeness.ReconcileCounts(expected, actual) {
			mismatched = append(mismatched, g.Ledger)
		}
	}
	return mismatched, blind, nil
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
// Scope: each target is reconciled over ITS OWN scope (projectionScopes /
// targetScope) — the range the served tier ACTUALLY holds for that table,
// derived from the data, raised to the incremental -from floor. scopes is
// parallel to src.targets. The expected side is re-derived ONCE over the union
// of those scopes and clipped per target, so a source that mixes a
// late-starting table (trades) with a full-history one (soroswap_skim_events)
// verifies each over its true range instead of flooring the whole source at the
// latest — the DAT-09/N-F2 source-level-floor defect.
func reconcileProjectionAggregate(ctx context.Context, store *timescale.Store, chStreamer completeness.EventStreamer, chAddr string, src reconSource, scopes []projectionScope) (int, completeness.BlindSpots, string, error) {
	if len(scopes) == 0 {
		return 0, completeness.BlindSpots{}, "", nil
	}
	lo, hi := scopeUnion(scopes)
	if lo > hi {
		return 0, completeness.BlindSpots{}, "", nil // every target's scope is empty
	}
	expectedFor, blind, eerr := expectedProjection(ctx, store, chStreamer, chAddr, src, lo, hi)
	if eerr != nil {
		return 0, completeness.BlindSpots{}, "", eerr
	}
	var totalDelta int
	var details []string
	// C4-059: state the blindness first — it explains why a zero delta below
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
// [lo, hi] and returns a per-target accessor plus the C4-059 blind spots the
// re-derive hit. Three oracles, by source class, and TWO of them can be
// blind:
//
//   - the decoder-driven oracle (soroban_events / contract_events), and
//   - the ContractCall census, which soft-fails PER CALL in
//     forEachContractCallEvent — symmetric with the ch-rebuild writer that
//     shares that same function, so a band or soroswap-router call whose
//     Decode errors is missing from BOTH sides and the diff nets to zero.
//
// Only the SDEX census is genuinely per-claim rather than per-row: its
// decoder soft-fails inside a single op's claim list and still emits the op,
// so a malformed claim cannot remove a whole row from the expected side
// without also removing it from served.
func expectedProjection(ctx context.Context, store *timescale.Store, chStreamer completeness.EventStreamer, chAddr string, src reconSource, lo, hi uint32) (func(reconTarget) map[uint32]int, completeness.BlindSpots, error) {
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
		expected, err := reDeriveSDEXCensusViaDecoder(ctx, chAddr, lo, hi)
		if err != nil {
			return nil, completeness.BlindSpots{}, err
		}
		return func(reconTarget) map[uint32]int { return expected }, completeness.BlindSpots{}, nil

	default:
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
			if err := preseedFactoryChildren(ctx, store, src, lo); err != nil {
				return nil, completeness.BlindSpots{}, fmt.Errorf("%s preseed: %w", src.name, err)
			}
		}
		// Factory-anchored IDENTITY-gated sources (aquarius, phoenix) opt into a
		// contract-id PREFILTER so the re-derive reads only the gated pool set
		// (contract-indexed) instead of streaming the whole ~6B-event lake — the
		// fix for the -pass 120-min-deadline timeout. Built AFTER the preseed so
		// the prefilter is a guaranteed SUPERSET of the registry the re-derive
		// will hold at every point in [lo,hi]; Matches() stays the per-event
		// gate, so the counts are byte-identical (see gatedPrefilter).
		contractIDs := src.contractIDs
		if src.newGatedDec != nil {
			pf, pferr := gatedPrefilter(ctx, chStreamer, src, hi)
			if pferr != nil {
				return nil, completeness.BlindSpots{}, fmt.Errorf("%s gated prefilter: %w", src.name, pferr)
			}
			contractIDs = pf
		}
		byKind, blind, err := completeness.ReDeriveOutputCountsByKindFromEvents(ctx, chStreamer, src.dec, contractIDs, src.topic0Syms, lo, hi)
		if err != nil {
			return nil, completeness.BlindSpots{}, err
		}
		// Each table receives only the kinds it is registered for; counting
		// every output would overcount any table that receives a subset.
		return func(tgt reconTarget) map[uint32]int { return completeness.SumKinds(byKind, tgt.kinds...) }, blind, nil
	}
}

// gatedPrefilter builds the contract-id prefilter for a factory-anchored
// IDENTITY-gated source (aquarius, phoenix) so the -ch re-derive reads only
// that source's gated contract set (factory ∪ children) instead of streaming
// the whole ~6B-event lake. The gated set is exactly the set of contracts
// Matches() can accept, so restricting the read to it (a contract-indexed
// scan) is counts-identical to the whole-lake stream, just far faster — the
// fix for the -pass 120-min-deadline timeout on aquarius's dirty-window
// re-derive (StreamContractEvents applies no contract prefilter when
// contractIDs is empty, so a factory-anchored source with empty catalogue
// contractIDs streams every event in [lo,tip]).
//
// The returned set is a guaranteed SUPERSET of the registry the real re-derive
// (src.dec) will hold at every point in [lo,hi], so no counted contract is
// ever missed and no undercount (false red) is possible:
//
//   - (a) src.dec's gate AFTER its preseed-to-lo = the curated in-code seed ∪
//     every child preseeded from the Postgres landing zone — i.e. the registry
//     state the stream STARTS from. Read via GatedContractSet().
//   - (b) every child the SAME certified lake announces through hi, walked from
//     the factory's creation events on a THROWAWAY decoder — a superset of the
//     children the real stream will self-seed in [lo,hi]. The throwaway keeps
//     src.dec's just-in-time in-stream seeding UNDISTURBED, so ordering-
//     sensitive edge cases (a pool created and traded in the same ledger with
//     adverse tx_hash sort order) resolve byte-identically to the unfiltered
//     stream — the prefilter only ever ADDS contract rows to the read; Matches()
//     is still the final per-event gate.
//
// The factory ids are always included (via GatedSet) so their creation events
// still stream and self-seed in-window children. Deduped + sorted (a stable
// prefilter keeps CH query plans / logs steady across a pass).
func gatedPrefilter(ctx context.Context, chStreamer completeness.EventStreamer, src reconSource, hi uint32) ([]string, error) {
	set := make(map[string]struct{})
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
				if fresh.Matches(ev) {
					// Decode registers the announced child in `fresh`'s registry
					// (its only side effect for a creation event); a decode error
					// on a malformed creation event is soft-failed exactly as the
					// re-derive would, so the walk stays non-fatal.
					_, _ = fresh.Decode(ev)
				}
				return nil
			})
		if werr != nil {
			return nil, fmt.Errorf("walk %s creation events: %w", src.name, werr)
		}
	}
	for _, c := range fresh.GatedContractSet() {
		set[c] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

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
	if sc.From > sc.To {
		return 0, "", nil
	}
	actual, err := store.CountRowsByLedger(ctx, tgt.table, "ledger", tgt.whereFilter, sc.From, sc.To)
	if err != nil {
		return 0, "", err
	}
	d, detail := projectionDelta(src, tgt.table, clipCounts(expected, sc), actual, sc.From, sc.To)
	return d, detail, nil
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
// Split out of [forEachContractCallEvent] so the C4-059 accounting is
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
func computeRecognitionGapsCH(ctx context.Context, cfg config.Config, chAddr string, gated map[string][]contractid.Option, from, tip uint32, soroswapOpts ...soroswap.DecoderOption) ([]completeness.RecognitionGap, error) {
	disp, err := pipeline.BuildDispatcher(cfg.Ingestion.EnabledSources, cfg.Oracle, gated, soroswapOpts...)
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
func computeRecognitionGaps(ctx context.Context, store *timescale.Store, cfg config.Config, gated map[string][]contractid.Option, tip uint32, soroswapOpts ...soroswap.DecoderOption) ([]completeness.RecognitionGap, error) {
	disp, err := pipeline.BuildDispatcher(cfg.Ingestion.EnabledSources, cfg.Oracle, gated, soroswapOpts...)
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
