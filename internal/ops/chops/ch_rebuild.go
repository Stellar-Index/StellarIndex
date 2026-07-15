package chops

import (
	"context"
	"flag"
	"fmt"
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
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
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
//     lake-replay successor to the retired backfill-router MinIO walk.
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
	contractCalls := fs.Bool("contract-calls", false, "also re-derive the event-less ContractCall sources (band, soroswap-router) from the lake's InvokeContract ops — filtered on the contract's bytes in body_xdr (no contract_id column) — and run their ContractCallDecoders. These have NO soroban_events landing zone, so neither the event pass nor the projector can rebuild them; this is the ADR-0034 lake-replay successor to the retired backfill-router MinIO walk. Respects -sources.")
	includeSEP41 := fs.Bool("sep41", false, "also re-derive the SEP-41 watched-contract sources (sep41_transfers, sep41_supply) from the lake via a contract_id-prefiltered event pass (their topics are the CAP-67 firehose the main pass excludes). FULL re-derive contract: for a whole-history rebuild run this as part of the truncate+re-derive procedure — TRUNCATE sep41_transfers + sep41_supply_events FIRST (historical rows predate the migration-0057 event_index PK, so multiple same-op events sit COLLAPSED on disk; the idempotent ON CONFLICT writes cannot un-collapse them — recover-into-existing is accepted only if you accept that residue). ROLLUP: when the SUPPLY source is re-derived, -write AUTO-RESETS the sep41_supply_rollup fold checkpoint after the events land (a FULL re-derive resets every watched contract's fold columns) so the aggregator worker re-folds from zero instead of double-counting the re-derived history (the KALE 2× served-value bug, incident 2026-07-06); the seeded migration-0088 genesis baseline is PRESERVED, so no manual TRUNCATE sep41_supply_rollup + re-seed is needed. After a full-history -write re-derive the two sources become eligible for the ADR-0033 projection reconcile — DONE (2026-07-11, windows 50.0M-63.42M, rc=0): buildReconciliationCatalogue now promotes them into the default catalogue unconditionally whenever [supply] watched_sep41_contracts is configured (no further code change needed), so verify-reconciliation/ch-reproject/compute-completeness all see them. Because that promotion is config-gated, not re-derive-state-gated, a FUTURE full truncate+re-derive will show the two sources as reconcile-red for the DURATION of the rebuild (truncated table vs. lake expectation) exactly like any other source mid-rebuild — expected, not a regression. For a SCOPED dropped-rows recovery (a decoder bug that lost a handful of rows from post-0057-clean data), use -contracts to narrow to the affected contracts instead — no truncate needed, the additive ON CONFLICT write only ADDS the missing rows (docs/operations/sep41-mint-recovery.md). Requires [supply] watched_sep41_contracts. Respects -sources.")
	contractsCSV := fs.String("contracts", "", "comma-separated contract C-strkeys to SCOPE the read to (default: no scope). For -sep41 this REPLACES [supply] watched_sep41_contracts as the contract_id READ prefilter, so a scoped recovery does an indexed scan of ONLY these contracts' events (far cheaper than all watched contracts) and idempotently ADDS their missing rows — the leanest way to recover dropped rows without a full re-derive. With -sep41 -write on the SUPPLY source, ONLY these contracts' sep41_supply_rollup fold rows are reset afterwards (genesis baseline preserved), so the worker re-folds their recovered below-checkpoint rows — a scoped recovery is safe by default, no manual rollup surgery. Must be a SUBSET of the watched set: the sep41 decoders still gate Matches() on the full watched set, so a contract outside it is read but decoded to nothing (a warning is printed). For the general event pass it is an extra decode-time contract gate. See docs/operations/sep41-mint-recovery.md.")
	sep41SupplyOnly := fs.Bool("sep41-supply-only", false, "with -sep41 -sources sep41_supply: narrow the CH read to the supply-affecting topics (mint/burn/clawback) via the topic_0_sym prefilter, skipping the transfer firehose at the SQL layer — so recovering a high-transfer-volume contract's few mints does not re-read millions of transfer events. Invalid unless sep41_transfers is disabled (via -sources sep41_supply): the topic prefilter would otherwise silently drop transfer recovery.")
	write := fs.Bool("write", false, "actually write to Postgres (default: dry-run, count only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *from == 0 || *to == 0 || *to < *from {
		return fmt.Errorf("-config, -from, -to are required; -to must be >= -from")
	}
	// -contracts scopes both passes to a contract subset; the sep41 pass pushes
	// it into the CH read as the contract_id prefilter (narrows the scan), the
	// general event pass applies it as a decode-time gate.
	contractsOverride := parseCSVList(*contractsCSV)
	if *sep41SupplyOnly && !*includeSEP41 {
		return fmt.Errorf("-sep41-supply-only requires -sep41")
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

	// Warn-level logger: HandleEvent debug-logs per event, which would flood at
	// rebuild volume. Errors + warns still surface.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	lo, hi := uint32(*from), uint32(*to)

	cat, soroswapDec, cerr := buildReconciliationCatalogue(cfg)
	if cerr != nil {
		return fmt.Errorf("ch-rebuild: reconciliation catalogue: %w", cerr)
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

	mode := "DRY-RUN (count only)"
	if *write {
		mode = "WRITE"
	}
	// Buffer-pass range guard (2026-07-05): the event + sep41 passes
	// buffer a whole invocation's decoded events in this process. A
	// 12.9M-ledger -sep41 run ballooned until the kernel killed it
	// silently — and the memory pressure swapped galexie's captive
	// core into an invalid-local-state wedge (11h lake stall). The
	// tool's docs always said "window your invocation"; docs aren't
	// guards. 2M ledgers ≈ a comfortable single-window ceiling.
	const maxBufferedRange = 2_000_000
	buffering := *includeSEP41 || len(srcFilter) == 0 || anyEventSourceEnabled(cat, srcFilter)
	if buffering && *to-*from > maxBufferedRange {
		return fmt.Errorf("ch-rebuild: range [%d,%d] spans %d ledgers — the event/sep41 passes buffer in-process; window invocations to <=%d ledgers (loop externally, resume per window)",
			*from, *to, *to-*from, maxBufferedRange)
	}

	fmt.Fprintf(os.Stderr, "ch-rebuild: [%d,%d] mode=%s sources=%q sdex=%v contract-calls=%v sep41=%v ch=%s\n",
		lo, hi, mode, *only, *includeSDEX, *contractCalls, *includeSEP41, *chAddr)

	written := map[string]int{} // source name -> events written (or counted in dry-run)

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
	// census Δ=0. This is the ADR-0034 lake-replay replacement for the retired
	// backfill-router MinIO walk (which under-produced: it pre-dated the
	// auth-tree-roots extraction, so it missed router calls nested inside
	// aggregator contracts).
	if *contractCalls {
		ccStart := time.Now()
		for _, src := range cat {
			if src.callDec == nil || !enabled(src.name) {
				continue
			}
			before := len(buf)
			if cerr := forEachContractCallEvent(ctx, *chAddr, src.callContract, src.callDec, lo, hi, func(_ uint32, ev consumer.Event) error {
				buf = append(buf, ev)
				return nil
			}); cerr != nil {
				return fmt.Errorf("ch-rebuild: contract-call stream %s: %w", src.name, cerr)
			}
			fmt.Fprintf(os.Stderr, "ch-rebuild: contract-call %s read done (%d events)\n", src.name, len(buf)-before)
		}
		fmt.Fprintf(os.Stderr, "ch-rebuild: contract-call read done in %s\n", time.Since(ccStart).Round(time.Second))
	}

	// ─── write the buffered events to Postgres (trades batched) ──────────
	// Per-row HandleEvent does 2 PG round-trips per trade (WouldPopulateUSDVolume
	// + InsertTrade); at dense-window volume (175k events/window) that's hours.
	// Batch trade-shaped events via BatchInsertTrades (one multi-row INSERT per
	// batch); everything else (protocol entities, fewer rows) stays per-row.
	wStart := time.Now()
	const tradeBatchN = 1000
	batch := make([]canonical.Trade, 0, tradeBatchN)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := store.BatchInsertTrades(ctx, batch); err != nil {
			logger.Warn("batch trade insert failed; per-row fallback", "n", len(batch), "err", err)
			for _, t := range batch {
				if ierr := store.InsertTrade(ctx, t); ierr != nil {
					logger.Error("per-row trade insert failed", "err", ierr)
				}
			}
		}
		batch = batch[:0]
	}
	// sep41 batches (2026-07-05): the full-history re-derive buffers
	// tens of millions of sep41 events per window; per-row HandleEvent
	// capped writes at ~520/s (round-trip + cold PK-page per insert).
	// Same batching pattern as trades, same per-row fallback.
	const sepBatchN = 50_000
	xferBatch := make([]timescale.SEP41TransferRow, 0, sepBatchN)
	flushXfer := func() {
		if len(xferBatch) == 0 {
			return
		}
		if err := store.CopyMergeSEP41Transfers(ctx, xferBatch); err != nil {
			logger.Warn("sep41_transfers batch failed; per-row fallback", "n", len(xferBatch), "err", err)
			for _, r := range xferBatch {
				if ierr := store.InsertSEP41Transfer(ctx, r); ierr != nil {
					logger.Error("per-row sep41 transfer insert failed", "err", ierr)
				}
			}
		}
		xferBatch = xferBatch[:0]
	}
	supBatch := make([]timescale.SEP41SupplyEvent, 0, sepBatchN)
	flushSup := func() {
		if len(supBatch) == 0 {
			return
		}
		if err := store.CopyMergeSEP41SupplyEvents(ctx, supBatch); err != nil {
			logger.Warn("sep41_supply batch failed; per-row fallback", "n", len(supBatch), "err", err)
			for _, r := range supBatch {
				if ierr := store.InsertSEP41SupplyEvent(ctx, r); ierr != nil {
					logger.Error("per-row sep41 supply insert failed", "err", ierr)
				}
			}
		}
		supBatch = supBatch[:0]
	}
	fmt.Fprintf(os.Stderr, "ch-rebuild: drain start (%d events)\n", len(buf))
	for i, ev := range buf {
		if i > 0 && i%1_000_000 == 0 {
			fmt.Fprintf(os.Stderr, "ch-rebuild: drain %dM/%dM in %s\n", i/1_000_000, len(buf)/1_000_000, time.Since(wStart).Round(time.Second))
		}
		if *write {
			switch e := ev.(type) {
			case sep41transfers.Event:
				xferBatch = append(xferBatch, pipeline.SEP41TransferRowOf(e))
				if len(xferBatch) >= sepBatchN {
					flushXfer()
				}
				written[ev.Source()]++
				continue
			case sep41supply.Event:
				supBatch = append(supBatch, pipeline.SEP41SupplyRowOf(e))
				if len(supBatch) >= sepBatchN {
					flushSup()
				}
				written[ev.Source()]++
				continue
			}
			if t, ok := tradeOf(ev); ok {
				batch = append(batch, t)
				if len(batch) >= tradeBatchN {
					flush()
				}
			} else {
				pipeline.HandleEvent(ctx, logger, store, ev)
			}
		}
		written[ev.Source()]++
	}
	if *write {
		flush()
		flushXfer()
		flushSup()
		fmt.Fprintf(os.Stderr, "ch-rebuild: wrote %d events in %s\n", len(buf), time.Since(wStart).Round(time.Second))

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
	fmt.Printf("%-16s %14s\n", "source", "events")
	var total int
	for _, src := range cat {
		n, ok := written[src.name]
		if !ok {
			continue
		}
		fmt.Printf("%-16s %14d\n", src.name, n)
		total += n
	}
	fmt.Printf("%-16s %14d\n", "TOTAL", total)
	if !*write {
		fmt.Printf("\n(dry-run — re-run with -write to persist to Postgres)\n")
	}
	return nil
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

// anyEventSourceEnabled reports whether the -sources filter selects at
// least one event-decoder source (the buffering pass runs for those).
func anyEventSourceEnabled(cat []reconSource, srcFilter map[string]bool) bool {
	for _, src := range cat {
		if src.dec != nil && srcFilter[src.name] {
			return true
		}
	}
	return false
}
