// Binary stellarindex-indexer runs the production ingestion pipeline:
//
//	Galexie MinIO → internal/ledgerstream → internal/dispatcher
//	              → per-source decoders → canonical.Trade /
//	                canonical.OracleUpdate → TimescaleDB
//
// This is the single production code path (docs/architecture/ingest-pipeline.md): no stellar-rpc
// client, no per-source goroutines. One goroutine drives ledgerstream + dispatcher; a second
// drains the resulting consumer.Events to Timescale. That second goroutine is deliberately
// unguarded: a recover() would be cosmetic (persistWorker panics end the process anyway) and
// would leave /healthz answering with a frozen cursor; crashing lets systemd restart from the
// last cursor.
//
// Flags: -config PATH (required), -dry-run (load config, open connections,
// validate, exit), -verify-hashdb-from N / -verify-hashdb-to N (one hashdb
// verify pass over [from,to], both required, then exit).
//
// SIGINT + SIGTERM cancel the root context; the binary waits up to 30 s.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/archivecompleteness"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/canonical/discovery"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher/statsflush"
	"github.com/Stellar-Index/StellarIndex/internal/hashdb"
	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/projector"
	"github.com/Stellar-Index/StellarIndex/internal/redact"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	externalbinance "github.com/Stellar-Index/StellarIndex/internal/sources/external/binance"
	externalbitstamp "github.com/Stellar-Index/StellarIndex/internal/sources/external/bitstamp"
	externalcoinbase "github.com/Stellar-Index/StellarIndex/internal/sources/external/coinbase"
	externalcoingecko "github.com/Stellar-Index/StellarIndex/internal/sources/external/coingecko"
	externalcoinmarketcap "github.com/Stellar-Index/StellarIndex/internal/sources/external/coinmarketcap"
	externalcryptocompare "github.com/Stellar-Index/StellarIndex/internal/sources/external/cryptocompare"
	externalecb "github.com/Stellar-Index/StellarIndex/internal/sources/external/ecb"
	externalexchangerates "github.com/Stellar-Index/StellarIndex/internal/sources/external/exchangeratesapi"
	externalkraken "github.com/Stellar-Index/StellarIndex/internal/sources/external/kraken"
	externaltiingo "github.com/Stellar-Index/StellarIndex/internal/sources/external/tiingo"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
	soroswap_router "github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/version"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// errLedgerstreamPanic marks a fatal error that began as a ledgerstream producer panic: a panic needs a code fix, a stream error usually
// needs MinIO back.
var errLedgerstreamPanic = errors.New("ledgerstream producer panicked")

// cursorSource is the one `source` label in ingestion_cursors: the whole pipeline tracks a single
// last-processed ledger.
const cursorSource = "ledgerstream"

// main is a thin shim over realMain because os.Exit skips defers, which would lose the
// SilenceSDKChecksumWarnings flush.
func main() {
	os.Exit(realMain())
}

func realMain() int {
	// Wrap fd 2 with a line-filter BEFORE any aws-sdk-go-v2 code captures os.Stderr
	// (config.LoadDefaultConfig binds the default logger then). It drops the per-S3-GET "Response
	// has no supported checksum" WARN that floods journald on MinIO; the SDK hardcodes
	// ChecksumMode: Enabled, so an env-var opt-out would be a no-op. Fail-soft: a pipe/dup2 error
	// logs to the original stderr and startup continues unfiltered.
	//
	// The flush MUST run before exit or short-lived runs (-version, -dry-run failure) lose
	// buffered output; see SilenceSDKChecksumWarnings.
	flush := pipeline.SilenceSDKChecksumWarnings()
	defer flush()

	var (
		cfgPath     = flag.String("config", "", "Path to TOML config file (required)")
		dryRun      = flag.Bool("dry-run", false, "Load config + open connections + exit without ingesting")
		showVersion = flag.Bool("version", false, "Print version and exit")
		verifyFrom  = flag.Uint("verify-hashdb-from", 0, "Run one hashdb verify pass over [from,to] against the archive bucket and exit, instead of ingesting (requires -verify-hashdb-to)")
		verifyTo    = flag.Uint("verify-hashdb-to", 0, "See -verify-hashdb-from")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return 0
	}

	rangeFlags := verifyHashDBRangeFlags{from: *verifyFrom, to: *verifyTo}
	from, to, requested, rerr := rangeFlags.validate()
	if rerr != nil {
		fmt.Fprintf(os.Stderr, "stellarindex-indexer: %v\n", rerr)
		return 2
	}

	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "stellarindex-indexer: -config is required")
		flag.Usage()
		return 2
	}

	if requested {
		if err := runVerifyHashDBRange(context.Background(), *cfgPath, from, to); err != nil {
			fmt.Fprintf(os.Stderr, "stellarindex-indexer: %s\n", redact.Credentials(err.Error()))
			return 1
		}
		return 0
	}

	if err := run(*cfgPath, *dryRun); err != nil {
		fmt.Fprintf(os.Stderr, "stellarindex-indexer: %s\n", redact.Credentials(err.Error()))
		return 1
	}
	return 0
}

// verifyHashDBRangeFlags holds the -verify-hashdb-from/-to pair (zero value = not requested). hashDBVerifySweep covers only a trailing
// window off the tip,
// so this verifies an older range without deleting the hashdb file and losing its tamper-evidence baseline.
type verifyHashDBRangeFlags struct {
	from, to uint
}

// validate reports whether either flag was set (requested) and, if so, that the pair is well-formed.
func (f verifyHashDBRangeFlags) validate() (from, to uint32, requested bool, err error) {
	if f.from == 0 && f.to == 0 {
		return 0, 0, false, nil
	}
	if f.from == 0 || f.to == 0 {
		return 0, 0, true, errors.New("-verify-hashdb-from and -verify-hashdb-to must both be set")
	}
	if f.from > f.to {
		return 0, 0, true, fmt.Errorf("-verify-hashdb-from (%d) must be <= -verify-hashdb-to (%d)", f.from, f.to)
	}
	return uint32(f.from), uint32(f.to), true, nil
}

// runVerifyHashDBRange runs one bounded hashdb verify over the operator's [from, to] against the archive bucket (the historical-reads
// config), then returns;
// it does not start ingestion.
func runVerifyHashDBRange(ctx context.Context, cfgPath string, from, to uint32) error {
	cfg, err := config.LoadWithEnv(cfgPath)
	if err != nil {
		return err
	}
	logger := mkLogger(cfg.Obs)

	if !cfg.HashDB.Enabled {
		return fmt.Errorf("verify-hashdb-range: hashdb.enabled is false in %s", cfgPath)
	}

	verifyDB, err := hashdb.Open(cfg.HashDB.Path)
	if err != nil {
		return fmt.Errorf("verify-hashdb-range: open hashdb %s: %w", cfg.HashDB.Path, err)
	}
	defer func() {
		if cerr := verifyDB.Close(); cerr != nil {
			logger.Warn("hashdb verify handle close", "err", cerr)
		}
	}()

	archiveCfg := pipeline.LedgerstreamConfig(cfg, cfg.Storage.S3BucketArchive)
	// nil dedup set: a one-off operator pass has no overlapping sweeps to dedup (see countNewDrift).
	outcome := hashDBVerifyPass(ctx, logger, verifyDB, archiveCfg, from, to, nil, hashDBWindowRecent)
	if outcome != sweepOutcomeOK {
		return fmt.Errorf("verify-hashdb-range: sweep did not complete cleanly (outcome=%d) — see log for detail", outcome)
	}
	return nil
}

//nolint:funlen,gocognit,gocyclo // top-level binary lifecycle; splitting reduces readability of dependency-construction order
func run(cfgPath string, dryRun bool) error {
	cfg, err := config.LoadWithEnv(cfgPath)
	if err != nil {
		return err
	}

	if err := canonical.InstallNetwork(cfg.Stellar.Passphrase(), cfg.Supply.SACWrappers); err != nil {
		return err
	}

	logger := mkLogger(cfg.Obs)
	logger.Info("starting",
		"version", version.String(),
		"region", cfg.Region.ID,
		"network", cfg.Stellar.Network,
		"sources", cfg.Ingestion.EnabledSources,
		"dry_run", dryRun,
	)

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	// ─── Storage ────────────────────────────────────────────────
	// OpenBackground sets a generous session statement_timeout as the runaway backstop; heavy
	// batch scans SET LOCAL a longer bound inside their own transactions.
	store, err := timescale.OpenBackground(rootCtx, cfg.Storage.PostgresDSN, timescale.BackgroundStatementTimeout)
	if err != nil {
		cancel() // nothing else registered yet; release the signal ctx
		return fmt.Errorf("storage: %w", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Warn("storage close", "err", err)
		}
	}()
	// One indexer per database. Deferred before cancel so the lock is held
	// through the drain; a dry-run takes none so it can run beside the daemon.
	var instanceLock *timescale.InstanceLock
	if !dryRun {
		instanceLock, err = store.HoldInstanceLock(rootCtx, timescale.IndexerInstanceLockName, cancel, logger.With("component", "instance-lock"))
		if err != nil {
			cancel()
			return instanceLockRefusal(err)
		}
		defer releaseInstanceLock(instanceLock, logger)
	}
	// Registered after store.Close so LIFO cancels workers before the pool closes.
	defer cancel()
	logger.Info("storage connected")

	// USD-volume quote spec: wires on-chain DEX trades into usd_volume. The operator declares
	// which classic credits count as USD-pegged in `[trades].usd_pegged_classic_assets`; SAC
	// wrappers come transitively via `[supply.sac_wrappers]`. An empty list leaves the spec nil.
	//
	// Both tiers install together via [timescale.InstallUSDVolumeResolution]: a store missing the
	// resolvers does not merely skip usd_volume, it can overwrite good values with NULL.
	if err := timescale.InstallUSDVolumeResolution(
		store,
		cfg.Trades.USDPeggedClassicAssets,
		cfg.Supply.SACWrappers,
		cfg.PricingGuard.FiatPeggedClassicAssets,
	); err != nil {
		return err
	}
	if len(cfg.Trades.USDPeggedClassicAssets) > 0 {
		logger.Info("usd_volume resolution enabled",
			"classic_pegs", len(cfg.Trades.USDPeggedClassicAssets),
			"sac_wrappers", len(cfg.Supply.SACWrappers),
		)
	}

	// ─── Dispatcher + decoders ─────────────────────────────────
	// Soroswap pair registry: load from postgres so the decoder boots with every previously-seen
	// pair, and arm a live-upsert hook so factory new_pair events persist as they stream. An
	// empty registry on a fresh deployment is fine; `stellarindex-ops seed-soroswap-pairs`
	// bootstraps it.
	soroswapOpts, err := pipeline.SoroswapPersistenceOptions(rootCtx, store, logger, rootCtx)
	if err != nil {
		return fmt.Errorf("soroswap registry: %w", err)
	}
	// Factory-anchored gated decoders (blend, ...): warm each contractid.Registry from
	// protocol_contracts and arm a live-upsert hook so factory creation events persist as they
	// stream (ADR-0035). An empty registry on a fresh deployment is fine;
	// `stellarindex-ops seed-protocol-contracts -source <name>` bootstraps it. withHook=true:
	// this is the writer path.
	gatedOpts, err := pipeline.GatedRegistryOptions(rootCtx, store, logger, rootCtx, true)
	if err != nil {
		return fmt.Errorf("gated registries: %w", err)
	}
	disp, err := pipeline.BuildDispatcher(cfg.Ingestion.EnabledSources, cfg.Oracle, gatedOpts, soroswapOpts...)
	if err != nil {
		return fmt.Errorf("build dispatcher: %w", err)
	}
	// The dispatcher logs only a recovered decoder panic, with the ledger/tx/op coordinate the
	// runbook needs, so give it the configured logger instead of slog.Default().
	disp.SetLogger(logger)

	// ─── Supply observers (opt-in via [supply] watched-sets) ──────
	// Wires accounts (Algorithm 1, XLM), trustlines / claimable / liquidity_pools / sac_balances
	// (Algorithm 2, LCM-based) and sep41_supply (Algorithm 3, event stream). An empty watched-set
	// leaves that observer unregistered.
	supplyObservers, err := pipeline.RegisterSupplyEntryDecoders(disp, cfg.Supply, cfg.Metadata)
	if err != nil {
		return fmt.Errorf("supply observers (entry): %w", err)
	}
	supplyEvents, err := pipeline.RegisterSupplyEventDecoders(disp, cfg.Supply)
	if err != nil {
		return fmt.Errorf("supply observers (event): %w", err)
	}
	supplyObservers = append(supplyObservers, supplyEvents...)
	if len(supplyObservers) > 0 {
		logger.Info("supply observers wired",
			"observers", supplyObservers,
			"sdf_reserve_accounts", len(cfg.Supply.SDFReserveAccounts),
			"watched_issuer_accounts", len(cfg.Metadata.WatchedIssuerAccounts),
			"watched_classic_assets", len(cfg.Supply.WatchedClassicAssets),
			"sac_wrappers", len(cfg.Supply.SACWrappers),
			"watched_sep41_contracts", len(cfg.Supply.WatchedSEP41Contracts))
	} else {
		// An absent supply pipeline is silent: with every [supply] watched-set empty no observer
		// registers, supply fields on /v1/assets/{id} stay null and asset_supply_history stays
		// empty. Warn at boot so an operator who forgot the watched-sets sees it in the log.
		logger.Warn("supply pipeline is OFF — no [supply] watched-sets configured",
			"hint", "set sdf_reserve_accounts (Algorithm 1 XLM), watched_classic_assets (Algorithm 2), and/or watched_sep41_contracts (Algorithm 3) in your TOML to enable; see ADR-0011/0021/0022/0023",
			"effect", "asset_supply_history will not populate; F2 fields (market_cap_usd, fdv_usd, circulating_supply, total_supply, max_supply) on /v1/assets/{id} will be null for every asset")
	}

	// The account observer is registered iff its watch set is non-empty, the exact condition
	// pipeline.RegisterSupplyEntryDecoders gates on. The per-ledger loop advances its freshness
	// watermark only when it is live: a watermark must NOT advance for an observer that isn't
	// running.
	accountObserverActive := len(pipeline.AccountObserverWatchSet(cfg.Supply, cfg.Metadata)) > 0

	// ─── Starting ledger + dry-run exit ────────────────────────
	// Dry-run exits before ANY background worker starts: the taggers UPDATE trades on their first
	// pass and the stats flusher writes on shutdown (TestMainWiringRules/dry_run_exits_before_background_workers).
	from, err := resolveStartLedger(rootCtx, store, cfg.Ingestion.BackfillFromLedger)
	if err != nil {
		return fmt.Errorf("resolve start ledger: %w", err)
	}
	logger.Info("starting ledger resolved", "from", from)

	if dryRun {
		logger.Info("dry-run complete — exiting")
		return nil
	}

	// Probes the pool every 60 s to surface dead conns lingering after Postgres recovers; the
	// reconnect is the pool's PoolConnMaxLifetime (30 min). Together they cap a cascade gap at that
	// lifetime and alert within minutes.
	postgresPingStop, postgresPingDone := watchPostgresPing(rootCtx, store, logger.With("component", "postgres-ping"))
	defer func() {
		postgresPingStop()
		<-postgresPingDone
	}()

	// ─── Router attribution sweeper ────
	// Periodically tags recent same-tx soroswap `trades` rows with routed_via='soroswap-router' by
	// joining soroswap_router_swaps. It is a trailing-window sweep, not an inline UPDATE at
	// router-persist time, because the projector writes the pair-level trades and races the
	// dispatcher's router-call persist (internal/pipeline/routedvia.go). Gated on the router
	// source being enabled; without its decoder there is nothing to sweep.
	if routerEnabled(cfg.Ingestion.EnabledSources) {
		routedViaStop, routedViaDone := startRoutedViaTagger(rootCtx, store, logger.With("component", "routed-via-tagger"))
		defer func() {
			routedViaStop()
			<-routedViaDone
		}()
		logger.Info("routed-via attribution sweeper started")
	}

	// AMM signer attribution back-tags trades.signer from the lake; skipped without an AMM source.
	if ammSignerEnabled(cfg.Ingestion.EnabledSources) {
		chAddr := cfg.Storage.ClickHouseAddr
		if chAddr == "" {
			chAddr = "127.0.0.1:9300"
		}
		signerStop, signerDone := startSignerTagger(rootCtx, chAddr,
			cfg.Storage.ClickHouseServingUser, cfg.Storage.ClickHouseServingPassword,
			store, logger.With("component", "signer-tagger"))
		defer func() {
			signerStop()
			<-signerDone
		}()
		logger.Info("AMM signer attribution sweeper started")
	}

	// Intra-ledger apply order: back-tags trades.tx_index from the lake for every on-chain source.
	if chAddr := cfg.Storage.ClickHouseAddr; chAddr != "" {
		txIndexStop, txIndexDone := startTxIndexTagger(rootCtx, chAddr, store,
			logger.With("component", "tx-index-tagger"))
		defer func() {
			txIndexStop()
			<-txIndexDone
		}()
		logger.Info("tx-index apply-order sweeper started")
	}

	// ─── Decoder-stats periodic flush ────────────────────────────
	// Snapshots dispatcher.Stats() every 5 min and writes per-source deltas to the
	// decoder_stats_5m hypertable, which powers /v1/diagnostics/decoders and the explorer
	// /diagnostics page.
	decoderStatsFlusher := statsflush.New(disp, store,
		logger.With("component", "decoder-stats-flush"),
		statsflush.Options{Interval: 5 * time.Minute})
	decoderStatsCtx, decoderStatsCancel := context.WithCancel(rootCtx)
	decoderStatsDone := make(chan struct{})
	go func() {
		defer close(decoderStatsDone)
		// RECOVER. decoder_stats_5m is a diagnostics rollup no ingest path reads, so a missed flush
		// costs one row and a panic must not kill every source's ingest. Unguarded it would also be
		// invisible, because the deferred close(decoderStatsDone) reads as a clean finish.
		defer worker.Recover(logger, "decoder-stats-flush")
		_ = decoderStatsFlusher.Run(decoderStatsCtx)
	}()
	defer func() {
		decoderStatsCancel()
		<-decoderStatsDone
	}()

	// ─── SEP-41 auto-discovery sink ──────────────────────────────
	// Buffers Hits to a channel drained by a worker into timescale.Store.RecordDiscovered. The
	// dispatcher's Push is non-blocking, and repeated (contract_id, event_type) pairs are
	// deduplicated before enqueue (the recorder upserts on the same key). If the buffer still
	// fills (Postgres outage, cold-start burst), Hits drop and increment DroppedCount; the
	// seen-mark rolls back so a later Push for the same key retries. Alert on a sustained climb.
	discoverySink := discovery.NewAsyncSink(discoveryRecorderAdapter{s: store}, discovery.AsyncSinkOptions{
		BufferSize:    1024,
		RecordTimeout: 2 * time.Second,
		DrainTimeout:  pipeline.DiscoveryDrainBudget,
		Logger:        logger.With("component", "discovery"),
	})
	discoverySink.Start()
	discoveryMetricsStop, discoveryMetricsDone := watchDiscoveryDrops(discoverySink, logger.With("component", "discovery"))
	defer func() {
		discoveryMetricsStop()
		<-discoveryMetricsDone
	}()
	defer func() {
		discoverySink.Stop()
		if dropped := discoverySink.DroppedCount(); dropped > 0 {
			logger.Warn("discovery: hits dropped on shutdown drain",
				"dropped", dropped)
		}
	}()
	disp.SetDiscoverySink(discoverySink)
	logger.Info("discovery sink wired", "buffer_size", 1024)

	// ─── Soroban-events raw-event landing zone (ADR-0029) ────────
	// Every Soroban contract event the dispatcher routes is also captured to the `soroban_events`
	// hypertable. It is additive and does not replace per-source decoders; it lets per-source
	// backfills run as SQL `INSERT ... SELECT FROM soroban_events` instead of re-walking MinIO.
	rawEventSink := sorobanevents.NewAsyncSink(store, sorobanevents.AsyncSinkOptions{
		IsPermanentFault: timescale.IsPermanentDataError,
		BufferSize:       4096,
		BatchSize:        1000,
		FlushInterval:    time.Second,
		WriteTimeout:     10 * time.Second,
		DrainGrace:       pipeline.RawEventDrainBudget,
		Logger:           logger.With("component", "soroban-events"),
	})
	rawEventSink.Start()
	rawEventMetricsStop, rawEventMetricsDone := watchSorobanEventsSink(rawEventSink, logger.With("component", "soroban-events"))
	defer func() {
		rawEventMetricsStop()
		<-rawEventMetricsDone
	}()
	// PushEvent blocks under back-pressure and the dispatcher hot path has no ctx awareness, so a
	// postgres stall could pin the streaming loop past SIGTERM. Stop the sink on rootCtx cancel to
	// unblock producers; the deferred Stop below is idempotent.
	go func() {
		// RECOVER. Losing this costs nothing (the deferred Stop repeats it), but a panic in a
		// non-main goroutine would end the process WITHOUT main's defers, skipping the sink drain.
		defer worker.Recover(logger, "soroban-events-stop-watch")
		<-rootCtx.Done()
		rawEventSink.Stop()
	}()
	defer func() {
		rawEventSink.Stop()
		logger.Info("soroban-events sink drained on shutdown",
			"written", rawEventSink.WrittenCount(),
			"dropped", rawEventSink.DroppedCount(),
			"skipped", rawEventSink.SkippedCount(),
			"lost", rawEventSink.LostCount(),
		)
		if dropped := rawEventSink.DroppedCount(); dropped > 0 {
			logger.Warn("soroban-events: rows dropped at shutdown — last batch may be partial",
				"dropped", dropped)
		}
		if lost := rawEventSink.LostCount(); lost > 0 {
			logger.Error("soroban-events: rows permanently lost — re-derive the logged ledger ranges from the CH lake (ADR-0034)",
				"lost", lost)
		}
	}()
	disp.SetRawEventSink(rawEventSink)
	logger.Info("soroban-events sink wired",
		"buffer_size", 4096, "batch_size", 1000)

	// ─── ClickHouse real-time dual-sink (ADR-0041) ─────────
	// Each ledger's extract is pushed to ClickHouse inline and non-blocking; the ~10-min
	// ch-live-catchup timer backstops anything dropped under CH pressure. The sink never blocks
	// ingest, even its own dial: a ClickHouse not up yet after a cold reboot must delay the sink,
	// not fail boot. chLiveSink.Load() is nil until the dial succeeds.
	var chLiveSink atomic.Pointer[clickhouse.LiveSink]
	if cfg.Storage.ClickHouseLiveSink {
		// The struct-tag default is docs-only and not applied at runtime, so default the address
		// here rather than fail boot on a missing one.
		chAddr := cfg.Storage.ClickHouseAddr
		if chAddr == "" {
			chAddr = "127.0.0.1:9300"
		}
		chSinkStop, chSinkDone := startCHLiveSink(rootCtx, chAddr, &chLiveSink, logger.With("component", "ch-live-sink"))
		defer func() {
			chSinkStop()
			<-chSinkDone
		}()
	}

	setSourceEnabled(cfg.Ingestion.EnabledSources, true)
	defer setSourceEnabled(cfg.Ingestion.EnabledSources, false)

	// ─── Metrics HTTP endpoint ──────────────────────────────────
	metricsSrv := startMetricsServer(cfg.Obs, store, logger)

	// ─── Sink goroutine ────────────────────────────────────────
	// Sink mode depends on the projector config (ADR-0032); see pipeline.SinkModeForProjector:
	//   - projector disabled -> SinkModeAll: this goroutine is the only writer.
	//   - projector enabled, persist_per_source=true -> SinkModeSkipSoleWriter: parallel for
	//     un-promoted sources, but the projector owns the SoleWriter specs outright.
	//   - projector enabled, persist_per_source=false -> SinkModeSkipProjected: the projector is
	//     sole writer for all projected sources.
	// Non-Soroban events (sdex, external, band, supply observers) always ride this path.
	sinkMode := pipeline.SinkModeForProjector(cfg.Ingestion.Projector.Enabled, cfg.Ingestion.Projector.PersistPerSource)
	if err := pipeline.VerifySoleWriterCAGGCoverage(rootCtx, store, sinkMode); err != nil {
		return fmt.Errorf("refusing to start: %w", err)
	}
	switch sinkMode {
	case pipeline.SinkModeSkipProjected:
		logger.Info("dispatcher events-goroutine: SKIP-PROJECTED mode — projector is sole writer for Soroban-derived events (ADR-0032 Phase 4)")
	case pipeline.SinkModeSkipSoleWriter:
		logger.Info("dispatcher events-goroutine: SKIP-SOLE-WRITER mode — projector is sole writer for the SoleWriter source specs; other projected sources double-write in Phase-3 parallel (ADR-0032 / F-1316)")
	case pipeline.SinkModeAll:
		// Projector disabled — events-goroutine writes every class.
	}
	// Both live trade writers below record late trades here so they reach the trades caggs.
	lateTrades := pipeline.NewLateTradeRefresher(store, pipeline.LateTradeRefresherOptions{Logger: logger.With("component", "late-trade-refresh")})
	if err := lateTrades.Prime(rootCtx); err != nil {
		logger.Warn("late-trade cagg refresh policies not loaded; every trade counts as late until the first flush loads them", "err", err)
	}
	go func() {
		defer worker.Recover(logger, "late-trade-refresh")
		lateTrades.Run(rootCtx)
	}()
	events := make(chan consumer.Event, 256)
	sinkDone := make(chan struct{})
	var sinkLoss pipeline.ShutdownLoss // read only after <-sinkDone
	go func() {
		// CRASH, deliberately unguarded: recover() would be cosmetic because writes run in the
		// persistWorker goroutines PersistEvents fans out, and if this frame unwound, nothing would
		// read `events` again. The producer would block mid-ledger while /healthz keeps answering
		// with a frozen cursor. Exiting loses nothing durable (cursor upserted per ledger, lake
		// written separately), so systemd restarts from the last cursor.
		defer close(sinkDone)
		sinkLoss = pipeline.PersistEvents(rootCtx, logger, store, events, sinkMode, lateTrades)
	}()

	// ─── Projector (ADR-0032) ──────────────────────────────────
	// Phase 3 (persist_per_source=true): projector and dispatcher both write Soroban-derived
	// events, with PK duplicates resolved by ON CONFLICT DO NOTHING. Phase 4
	// (persist_per_source=false): the projector is sole writer.
	var projectorDone chan struct{}
	if cfg.Ingestion.Projector.Enabled {
		registry, perr := projector.BuildRegistry(cfg.Ingestion.EnabledSources, cfg.Oracle, cfg.Supply.WatchedSEP41Contracts, gatedOpts, soroswapOpts...)
		if perr != nil {
			return fmt.Errorf("projector registry: %w", perr)
		}
		// Same pipeline.HandleEvent as the events goroutine, so rows take the same write path.
		sinkFn := lateTrades.ObservingSink(func(ctx context.Context, ev consumer.Event) error {
			return pipeline.HandleEvent(ctx, logger, store, ev)
		})
		proj := projector.New(store, registry, sinkFn, logger.With("component", "projector"))
		// soroban_events mode only: the cursor advances when a ledger's rows are enqueued to
		// rawEventSink, not when they commit.
		proj.SetRawEventBarrier(rawEventSink.Sync)
		// Reads forward events from the dual-sink-fed CH lake instead of Postgres soroban_events,
		// so it must go off wherever clickhouse_live_sink does.
		if cfg.Storage.ClickHouseProjectorSource {
			chAddr := cfg.Storage.ClickHouseAddr
			if chAddr == "" {
				chAddr = "127.0.0.1:9300"
			}
			proj.SetClickHouseSource(chAddr)
			logger.Info("projector reading from ClickHouse lake (soroban_events feed-switch)", "addr", chAddr)
		}
		projectorDone = make(chan struct{})
		go func() {
			defer close(projectorDone)
			// RECOVER, even though the projector is sole writer in Phase 4. Its output is
			// re-derivable (durable per-source cursor; catch-up is `stellarindex-ops
			// projector-replay`), so a stopped projector only delays rows, whereas a crashed process
			// stops substrate capture (lake dual-sink, hashdb append, cursor) that is NOT
			// re-derivable once galexie's live-bucket retention passes. Reconcile turns the gap into
			// /v1/coverage complete=false and worker_panics_total{worker="projector"} pages. This
			// covers Run's own frame only; the goroutines Run fans out defer their own.
			defer worker.Recover(logger, "projector")
			if err := proj.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Warn("projector exited with error", "err", err)
			}
		}()
		logger.Info("projector wired (Phase 3 parallel mode)",
			"sources", len(registry.Sources))
	} else {
		logger.Info("projector disabled — dispatcher per-source sinks remain primary")
	}

	// Verified-currency catalogue: drives the CG poller's ticker map and the aggregator pair set.
	verifiedCurrencies, err := currency.LoadEmbedded()
	if err != nil {
		return fmt.Errorf("verified-currency catalogue: %w", err)
	}
	logger.Info("verified-currency catalogue loaded", "entries", len(verifiedCurrencies.All()))

	// ─── External streamers (off-chain CEX/FX/aggregators) ──────
	// Parallel to the Galexie path, same sink; per-venue goroutines live in external.Run and we
	// keep its wait func for the drain.
	externalWait, externalSources, err := startExternalConnectors(rootCtx, cfg.External, verifiedCurrencies, events, logger)
	if err != nil {
		return fmt.Errorf("external connectors: %w", err)
	}
	setSourceEnabled(externalSources, true)
	defer setSourceEnabled(externalSources, false)

	// ─── Ledgerstream → dispatcher loop ─────────────────────────
	// StreamArchiveThenLive switches from the archive to the live bucket at LiveSeamLedger; seam=0
	// or from>=seam degrades to a plain live-only Stream.
	archiveCfg := pipeline.LedgerstreamConfig(cfg, cfg.Storage.S3BucketArchive)
	liveCfg := pipeline.LedgerstreamConfig(cfg, cfg.Storage.S3BucketLive)

	// SDK BufferedStorageBackend buffer metrics register only when Config.Registry != nil, and
	// LedgerstreamConfig leaves it nil ON PURPOSE (Stream is called repeatedly and the SDK's
	// registration panics on a second call). Export a 0/1 gauge so their absence is alertable.
	// The TieredDataStore's own tier_read_total metrics are registered unconditionally, so the
	// `both_missing` page is unaffected.
	if liveCfg.Registry != nil {
		obs.MetricsRegistryPresent.WithLabelValues("ledgerstream").Set(1)
	} else {
		obs.MetricsRegistryPresent.WithLabelValues("ledgerstream").Set(0)
		logger.Warn("ledgerstream running WITHOUT a Prometheus Registry — "+
			"its SDK BufferedStorageBackend buffer metrics are not registered "+
			"(the tier_read_total / both_missing page is unaffected: that metric "+
			"is obs package-level and always live — see audit C4-4 / W5-mon-3)",
			"component", "ledgerstream",
			"metric", "stellarindex_metrics_registry_present",
		)
	}

	// ─── HashDB drift detector ───────────────────────
	// Opt-in. Two *hashdb.DB handles on the SAME file, one appended from the live read loop and one
	// read by the sweep goroutine, because hashdb.DB is documented not safe for concurrent use.
	var (
		hashdbAppendDB     *hashdb.DB
		hashdbLastAppended atomic.Uint32
	)
	if cfg.HashDB.Enabled {
		hashdbAppendDB, err = openOrCreateHashDB(cfg.HashDB.Path, from)
		if err != nil {
			return fmt.Errorf("hashdb: open append handle: %w", err)
		}
		defer func() {
			if cerr := hashdbAppendDB.Close(); cerr != nil {
				logger.Warn("hashdb append handle close", "err", cerr)
			}
		}()

		hashdbVerifyDB, verr := hashdb.Open(cfg.HashDB.Path)
		if verr != nil {
			return fmt.Errorf("hashdb: open verify handle: %w", verr)
		}
		defer func() {
			if cerr := hashdbVerifyDB.Close(); cerr != nil {
				logger.Warn("hashdb verify handle close", "err", cerr)
			}
		}()

		hashdbVerifyStop, hashdbVerifyDone := startHashDBVerifier(
			rootCtx, hashdbVerifyDB, liveCfg,
			&hashdbLastAppended, logger.With("component", "hashdb-verify"),
		)
		defer func() {
			hashdbVerifyStop()
			<-hashdbVerifyDone
		}()

		logger.Info("hashdb drift detector enabled",
			"path", cfg.HashDB.Path,
			"verify_interval", hashDBVerifyInterval,
			"verify_window_ledgers", hashDBVerifyWindow,
		)
	}

	streamErr := make(chan error, 1)
	go func() {
		// CRASH, but drain on the way out. Recovering in place is the worst option: nothing would
		// send on streamErr, so main would wait until SIGTERM while /healthz answers with the cursor
		// frozen at the poison ledger. The fault stays FATAL and systemd restarts from the last
		// durable cursor. A panic in a non-main goroutine skips main's defers and would discard
		// buffered events for ledgers the cursor has passed, so it is converted once into a fatal
		// error (counted by worker.Report) and handed to main, which drains the sink.
		var err error
		defer func() {
			if r := recover(); r != nil {
				worker.Report(logger, "ledgerstream-producer", r)
				err = fmt.Errorf("%w: %v", errLedgerstreamPanic, r)
			}
			streamErr <- err
		}()
		err = ledgerstream.StreamArchiveThenLive(
			rootCtx, archiveCfg, liveCfg, from, cfg.Ingestion.LiveSeamLedger, logger,
			func(lcm sdkxdr.LedgerCloseMeta) error {
				if perr := processAndPersistCursor(rootCtx, disp, events, store, logger, lcm, cfg.Stellar.Passphrase(), accountObserverActive); perr != nil {
					return perr
				}
				// hashdb append is best-effort and never stalls or fails ingest (see recordHashdb).
				if hashdbAppendDB != nil {
					recordHashdb(hashdbAppendDB, lcm, logger, &hashdbLastAppended)
				}
				// Real-time CH fan-out is non-blocking, so a slow CH never stalls ingest and drops are
				// backstopped by the catch-up timer.
				if sink := chLiveSink.Load(); sink != nil {
					ext, eerr := clickhouse.ExtractLedger(lcm, cfg.Stellar.Passphrase())
					if eerr != nil {
						// Do NOT swallow this: a persistent failure means the lake's live edge is
						// stalling. Count it and sample the WARN so a meta-version break stays loud.
						obs.ChLiveSinkLedgersTotal.WithLabelValues("errored").Inc()
						logCHExtractErrSampled(logger, lcm.LedgerSequence(), eerr)
					} else {
						// A non-zero tx-read undercount means this ledger's contract_events are
						// incomplete; surface it or it would pass as a clean empty ledger.
						recordCHLiveSinkUndercount(ext, logger)
						sink.PushLedger(ext)
					}
				}
				return nil
			},
		)
	}()

	// ─── Shutdown ──────────────────────────────────────────────
	// streamExited records whether the producer has returned (it sends once on streamErr); never
	// close(events) while it might still send.
	streamExited := false
	// fatalErr is returned AFTER the drain, never instead of it: returning early would discard up
	// to one channel buffer (256) of events already counted against the cursor, so the next start
	// would resume past them. A decoder panic arrives here as a ledger error.
	var fatalErr error
	select {
	case <-rootCtx.Done():
		logger.Info("shutdown signal received — draining", "budget", pipeline.ShutdownDeadline.String())
	case err := <-streamErr:
		streamExited = true
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("ledgerstream exited with error — draining before exit", "err", err)
			fatalErr = err
		}
	}

	// Cancel rootCtx BEFORE draining: the producer-error path has not cancelled it, and without it
	// the connectors and sink would never be told to stop, burning the shutdown budget and
	// dropping the buffer the drain exists to persist. Both paths then drain identically.
	cancel()

	shutdownCtx, stopDrain := context.WithTimeout(context.Background(), pipeline.ShutdownDeadline)
	defer stopDrain()

	// After a signal the producer may be mid-`events <- ev`, and closing `events` under it panics.
	// Wait for it before close(events), bounded by shutdownCtx so a stuck producer cannot hang us.
	safeToClose := true
	if !streamExited {
		select {
		case <-streamErr:
			streamExited = true
			logger.Info("ledgerstream producer exited")
		case <-shutdownCtx.Done():
			// Leaving `events` UNCLOSED is safe: a live producer would panic on a closed channel,
			// and the sink workers drain via their ctx.Done() arm, so no events are lost.
			safeToClose = false
			logger.Warn("ledgerstream producer did not exit before drain deadline — leaving events channel open to avoid send-on-closed panic")
		}
	}

	// Connectors must drain before the shared events channel closes, or an in-flight send panics.
	// The wait is bounded because one wedged connector would otherwise hold the binary until
	// systemd's SIGKILL, the one shutdown that skips the sink drain. On timeout do NOT
	// close(events): a running connector is a live sender.
	if !waitBounded(shutdownCtx, logger, "external-connectors-drain", externalWait) {
		safeToClose = false
		logger.Warn("external connectors did not drain before deadline — leaving events channel open to avoid send-on-closed panic")
	}

	// Close events so the sink returns after draining; safe only once the producer has exited and
	// the connectors have drained.
	if safeToClose {
		close(events)
	}
	writersStopped := true // both live trade writers; read by the late-trade flush below
	select {
	case <-sinkDone:
		logger.Info("clean shutdown")
		// Fresh ctx: shutdownCtx may be spent by a drain that ran to its deadline, and the rewind is
		// the loss's only fix.
		rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
		rewindCursorForSinkLoss(rctx, store, sinkLoss, streamExited, logger) //nolint:contextcheck // deliberate fresh ctx, see above
		rcancel()
	case <-shutdownCtx.Done():
		writersStopped = false
		logger.Warn("drain timeout exceeded — hard exit")
	}

	// Each projector cycle is bounded by PerSourceTimeout (60s), so this returns promptly on cancel.
	if projectorDone != nil {
		select {
		case <-projectorDone:
			logger.Info("projector drained")
		case <-shutdownCtx.Done():
			writersStopped = false
			logger.Warn("projector drain timeout — hard exit")
		}
	}
	// Flush the late trades the writers' shutdown drain landed, on a fresh ctx as for the rewind.
	// After a drain timeout a writer may still run and its durable window is kept for the next
	// start; ~70s of systemd's 120s TimeoutStopSec is the worst case to here.
	if !writersStopped {
		logger.Warn("late-trade cagg flush runs before every trade writer stopped (drain timeout); " +
			"late trades written after it are refreshed by the next start")
	}
	lctx, lcancel := context.WithTimeout(context.Background(), pipeline.LateTradeShutdownFlushBudget)
	lateTrades.FlushOnShutdown(lctx) //nolint:contextcheck // deliberate fresh ctx, see above
	lcancel()

	// Shut the metrics server down last, after the drain completes or times out: stopping it
	// earlier would make /metrics unscrapable for the whole drain, exactly when an operator needs
	// to see drain progress.
	if metricsSrv != nil {
		if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("metrics server shutdown", "err", err)
		}
	}

	// Surface the producer's failure only after everything it produced has been persisted.
	return errors.Join(fatalErr, instanceLock.Lost())
}

// instanceLockRefusal names the other indexer as the reason not to start.
func instanceLockRefusal(err error) error {
	if errors.Is(err, timescale.ErrInstanceLockHeld) {
		return fmt.Errorf("refusing to start: another stellarindex-indexer is running against this database: %w", err)
	}
	return fmt.Errorf("instance lock: %w", err)
}

func releaseInstanceLock(l *timescale.InstanceLock, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := l.Release(ctx); err != nil {
		logger.Warn("instance lock release", "err", err)
	}
}

// waitBounded runs wait on its own goroutine and reports whether it finished before ctx expired.
// False means still running or faulted, so the caller must treat what it protects as still live.
func waitBounded(ctx context.Context, logger *slog.Logger, name string, wait func()) bool {
	res := make(chan bool, 1)
	go func() {
		// RECOVER, reporting NOT-drained: after a panic we cannot say whether the senders stopped, so
		// false skips closing the events channel and lets the sink drain via ctx cancellation.
		// Crashing would skip the drain this path exists for.
		defer func() {
			if r := recover(); r != nil {
				worker.Report(logger, name, r)
				res <- false
			}
		}()
		wait()
		res <- true
	}()
	select {
	case ok := <-res:
		return ok
	case <-ctx.Done():
		return false
	}
}

// newECBPoller builds the ECB poller with the operator's poll_interval override, split out so the
// override is testable without network egress.
func newECBPoller(cfg config.ExternalVenueConfig) *externalecb.Poller {
	p := externalecb.NewPoller()
	if cfg.PollInterval > 0 {
		p.Interval = cfg.PollInterval
	}
	return p
}

// newTiingoPoller builds the fund-NAV poller over the curated fund tickers.
func newTiingoPoller(cfg config.TiingoVenueConfig) (*externaltiingo.Poller, error) {
	p, err := externaltiingo.NewPoller(cfg.APIKey, rwa.FundNAVTickers())
	if err != nil {
		return nil, err
	}
	if cfg.PollInterval > 0 {
		p.Interval = cfg.PollInterval
	}
	return p, nil
}

// startExternalConnectors builds the enabled off-chain connectors and hands them to external.Run; the returned wait drains them
// (a no-op when none are enabled, so shutdown stays unconditional).
func startExternalConnectors( //nolint:gocognit,gocyclo,funlen // dispatch-heavy; splitting would reduce linearity
	ctx context.Context,
	cfg config.ExternalConfig,
	catalogue *currency.Catalogue,
	events chan<- consumer.Event,
	logger *slog.Logger,
) (func(), []string, error) {
	var streamers []external.StreamerSpec
	var pollers []external.PollerSpec
	var enabled []string

	if cfg.Binance.Enabled {
		pairMap, err := externalbinance.DefaultPairs()
		if err != nil {
			return nil, nil, fmt.Errorf("binance default pairs: %w", err)
		}
		pairs, err := externalbinance.DefaultPairList()
		if err != nil {
			return nil, nil, fmt.Errorf("binance default pair list: %w", err)
		}
		s := externalbinance.NewStreamer(pairMap)
		s.Logger = logger
		streamers = append(streamers, external.StreamerSpec{
			Streamer: s,
			Pairs:    pairs,
		})
		logger.Info("external connector enabled",
			"source", externalbinance.SourceName,
			"pairs", len(pairs))
		enabled = append(enabled, externalbinance.SourceName)
	}

	if cfg.Kraken.Enabled {
		pairMap, err := externalkraken.DefaultPairs()
		if err != nil {
			return nil, nil, fmt.Errorf("kraken default pairs: %w", err)
		}
		pairs, err := externalkraken.DefaultPairList()
		if err != nil {
			return nil, nil, fmt.Errorf("kraken default pair list: %w", err)
		}
		s := externalkraken.NewStreamer(pairMap)
		s.Logger = logger
		streamers = append(streamers, external.StreamerSpec{
			Streamer: s,
			Pairs:    pairs,
		})
		logger.Info("external connector enabled",
			"source", externalkraken.SourceName,
			"pairs", len(pairs))
		enabled = append(enabled, externalkraken.SourceName)
	}

	if cfg.Bitstamp.Enabled {
		pairMap, err := externalbitstamp.DefaultPairs()
		if err != nil {
			return nil, nil, fmt.Errorf("bitstamp default pairs: %w", err)
		}
		pairs, err := externalbitstamp.DefaultPairList()
		if err != nil {
			return nil, nil, fmt.Errorf("bitstamp default pair list: %w", err)
		}
		s := externalbitstamp.NewStreamer(pairMap)
		s.Logger = logger
		streamers = append(streamers, external.StreamerSpec{
			Streamer: s,
			Pairs:    pairs,
		})
		logger.Info("external connector enabled",
			"source", externalbitstamp.SourceName,
			"pairs", len(pairs))
		enabled = append(enabled, externalbitstamp.SourceName)
	}

	if cfg.Coinbase.Enabled {
		pairMap, err := externalcoinbase.DefaultPairs()
		if err != nil {
			return nil, nil, fmt.Errorf("coinbase default pairs: %w", err)
		}
		pairs, err := externalcoinbase.DefaultPairList()
		if err != nil {
			return nil, nil, fmt.Errorf("coinbase default pair list: %w", err)
		}
		s := externalcoinbase.NewStreamer(pairMap)
		s.Logger = logger
		streamers = append(streamers, external.StreamerSpec{
			Streamer: s,
			Pairs:    pairs,
		})
		logger.Info("external connector enabled",
			"source", externalcoinbase.SourceName,
			"pairs", len(pairs))
		enabled = append(enabled, externalcoinbase.SourceName)
	}

	if cfg.ExchangeRatesApi.Enabled {
		// APIKey is resolved via env override at config load time
		// (see config.ApplyEnvOverrides → EXCHANGERATESAPI_KEY).
		p, err := externalexchangerates.NewPoller(cfg.ExchangeRatesApi.APIKey)
		if err != nil {
			return nil, nil, fmt.Errorf("exchangeratesapi: %w", err)
		}
		if cfg.ExchangeRatesApi.Base != "" {
			p.Base = cfg.ExchangeRatesApi.Base
		}
		pollers = append(pollers, external.PollerSpec{
			Poller: p,
			Pairs:  defaultFXPairs(p.Base),
		})
		logger.Info("external poller enabled",
			"source", externalexchangerates.SourceName,
			"base", p.Base)
		enabled = append(enabled, externalexchangerates.SourceName)
	}

	// Aggregator pollers are cross-check only (class=aggregator), so their OracleUpdates are
	// excluded from VWAP. The catalogue-derived pairs are unioned with `defaultAggregatorPairs`,
	// not replacing them, so a hardcoded ticker without a coingecko_id yet is never dropped.
	aggregatorPairs := mergeAggregatorPairs(aggregatorPairsFromCatalogue(catalogue, logger), defaultAggregatorPairs())

	if cfg.CoinGecko.Enabled {
		p := externalcoingecko.NewPoller()
		if cfg.CoinGecko.PollInterval > 0 {
			p.Interval = cfg.CoinGecko.PollInterval
		}
		// An empty catalogue map leaves the poller on its package default coverage.
		if ids := catalogue.CoinGeckoIDs(); len(ids) > 0 {
			p.TickerToID = ids
		}
		// The no-auth tier throttles or rejects requests; the Pro key wins when both are set.
		if k := strings.TrimSpace(cfg.CoinGecko.APIKey); k != "" {
			p.APIKey = k
		}
		if k := strings.TrimSpace(cfg.CoinGecko.DemoAPIKey); k != "" {
			p.DemoAPIKey = k
		}
		authMode := "anonymous"
		if p.APIKey != "" {
			authMode = "pro"
		} else if p.DemoAPIKey != "" {
			authMode = "demo"
		}
		pollers = append(pollers, external.PollerSpec{
			Poller: p,
			Pairs:  aggregatorPairs,
		})
		logger.Info("external poller enabled",
			"source", externalcoingecko.SourceName,
			"pairs", len(aggregatorPairs),
			"poll_interval", p.PollInterval(),
			"auth_mode", authMode)
		enabled = append(enabled, externalcoingecko.SourceName)
	}

	if cfg.CoinMarketCap.Enabled {
		p, err := externalcoinmarketcap.NewPoller(cfg.CoinMarketCap.APIKey)
		if err != nil {
			return nil, nil, fmt.Errorf("coinmarketcap: %w", err)
		}
		// Query by authoritative numeric id to disambiguate polluted tickers (LUNA, LUNC).
		p.CMCIDs = catalogue.CoinMarketCapIDs()
		p.Logger = logger
		pollers = append(pollers, external.PollerSpec{
			Poller: p,
			Pairs:  aggregatorPairs,
		})
		logger.Info("external poller enabled",
			"source", externalcoinmarketcap.SourceName,
			"pairs", len(aggregatorPairs),
			"cmc_ids", len(p.CMCIDs))
		enabled = append(enabled, externalcoinmarketcap.SourceName)
	}

	if cfg.CryptoCompare.Enabled {
		p, err := externalcryptocompare.NewPoller(cfg.CryptoCompare.APIKey)
		if err != nil {
			return nil, nil, fmt.Errorf("cryptocompare: %w", err)
		}
		pollers = append(pollers, external.PollerSpec{
			Poller: p,
			Pairs:  aggregatorPairs,
		})
		logger.Info("external poller enabled",
			"source", externalcryptocompare.SourceName,
			"pairs", len(aggregatorPairs))
		enabled = append(enabled, externalcryptocompare.SourceName)
	}

	if cfg.ECB.Enabled {
		p := newECBPoller(cfg.ECB)
		// ECB is fiat-only; its poller filters these fiat/EUR crosses to currencies it has rates for.
		pairs := defaultFXPairs("EUR")
		pollers = append(pollers, external.PollerSpec{
			Poller: p,
			Pairs:  pairs,
		})
		logger.Info("external poller enabled",
			"source", externalecb.SourceName,
			"pairs", len(pairs),
			"poll_interval", p.PollInterval())
		enabled = append(enabled, externalecb.SourceName)
	}

	if cfg.Tiingo.Enabled {
		// APIKey is resolved via env override at config load time
		// (see config.ApplyEnvOverrides → TIINGO_API_KEY).
		p, err := newTiingoPoller(cfg.Tiingo)
		if err != nil {
			return nil, nil, fmt.Errorf("tiingo: %w", err)
		}
		p.Logger = logger
		pollers = append(pollers, external.PollerSpec{Poller: p})
		logger.Info("external poller enabled",
			"source", externaltiingo.SourceName,
			"tickers", len(p.Tickers),
			"poll_interval", p.PollInterval())
		enabled = append(enabled, externaltiingo.SourceName)
	}

	if len(streamers) == 0 && len(pollers) == 0 {
		logger.Info("no external connectors enabled")
		return func() {}, nil, nil
	}

	wait, err := external.Run(ctx, streamers, pollers, events, logger)
	if err != nil {
		return nil, nil, err
	}
	return wait, enabled, nil
}

// routerEnabled reports whether the soroswap-router source is enabled: the gate for the routed-via attribution sweeper.
func routerEnabled(enabledSources []string) bool {
	for _, s := range enabledSources {
		if strings.ToLower(strings.TrimSpace(s)) == soroswap_router.SourceName {
			return true
		}
	}
	return false
}

// startRoutedViaTagger runs pipeline.RunRoutedViaTagger in its own goroutine, with the same
// (cancel, done) shape as [watchPostgresPing].
func startRoutedViaTagger(parent context.Context, store *timescale.Store, logger *slog.Logger) (context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// RECOVER. This trailing-window UPDATE back-tags routed_via on rows the projector already
		// wrote; each sweep re-covers the previous window, so a missed pass self-heals and the
		// worst case is a NULL attribution column. Killing the indexer over it would cost ledgers
		// to save a label.
		defer worker.Recover(logger, "routed-via-tagger")
		pipeline.RunRoutedViaTagger(ctx, logger, store, 0, 0)
	}()
	return cancel, done
}

// ammSignerEnabled reports whether any AMM/Soroban swap source (taker is the on-chain caller) is enabled: the gate for the signer sweeper.
// It reads the SAME timescale.AMMSignerSources the tagger filters on, so gate and UPDATE cannot drift.
func ammSignerEnabled(enabledSources []string) bool {
	for _, s := range enabledSources {
		folded := strings.ToLower(strings.TrimSpace(s))
		for _, amm := range timescale.AMMSignerSources {
			if folded == amm {
				return true
			}
		}
	}
	return false
}

// startSignerTagger runs pipeline.RunSignerTagger in a goroutine, back-tagging trades.signer from the lake. NON-FATAL: the dial retries
// inside the goroutine, so a ClickHouse outage at boot delays attribution only for its duration.
func startSignerTagger(parent context.Context, chAddr, chUser, chPass string, store *timescale.Store, logger *slog.Logger) (context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// RECOVER. Back-tagging trades.signer is a re-derivable overlay: the authority is the
		// ClickHouse lake and each sweep re-covers its trailing window, so a stopped tagger
		// leaves a NULL column a later run fills. The dial retry already treats this subsystem as
		// optional; a panic must not be the one failure mode that takes ingest with it.
		defer worker.Recover(logger, "signer-tagger")
		var lake *clickhouse.ExplorerReader
		ok := retryUntil(ctx, logger, "signer tagger: ClickHouse reader unavailable",
			signerDialMinBackoff, signerDialMaxBackoff,
			func(ctx context.Context) error {
				l, err := clickhouse.NewExplorerReaderAuth(ctx, chAddr, chUser, chPass)
				if err != nil {
					return err
				}
				lake = l
				return nil
			})
		if !ok {
			return // ctx cancelled while waiting to retry
		}
		defer func() { _ = lake.Close() }()
		pipeline.RunSignerTagger(ctx, logger, lake, store, 0, 0)
	}()
	return cancel, done
}

// startTxIndexTagger runs pipeline.RunTxIndexTagger in its own goroutine, back-tagging
// trades.tx_index from the lake. Non-fatal and dial-retried, like startSignerTagger.
func startTxIndexTagger(parent context.Context, chAddr string, store *timescale.Store, logger *slog.Logger) (context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer worker.Recover(logger, "tx-index-tagger")
		var lake *clickhouse.TxIndexReader
		ok := retryUntil(ctx, logger, "tx-index tagger: ClickHouse reader unavailable",
			signerDialMinBackoff, signerDialMaxBackoff,
			func(ctx context.Context) error {
				l, err := clickhouse.NewTxIndexReader(ctx, chAddr)
				if err != nil {
					return err
				}
				lake = l
				return nil
			})
		if !ok {
			return
		}
		defer func() { _ = lake.Close() }()
		pipeline.RunTxIndexTagger(ctx, logger, lake, store, 0, 0)
	}()
	return cancel, done
}

// Backoff bounds for the signer tagger's ClickHouse dial. The one-minute ceiling is because the
// wait is on an operator action or restart, so faster retries only add log noise.
const (
	signerDialMinBackoff = 5 * time.Second
	signerDialMaxBackoff = time.Minute
)

// Backoff bounds for the ClickHouse live-sink's dial, mirroring signerDialMinBackoff/Max: the wait
// is on a deploy or cold-boot metadata load, not packet loss.
const (
	chLiveSinkDialMinBackoff = 5 * time.Second
	chLiveSinkDialMaxBackoff = time.Minute
)

// startCHLiveSink dials the ClickHouse real-time dual-sink in its own goroutine (retrying until ctx ends) so a dial failure cannot fail
// boot;
// sink.Load() is nil until then and the ledgerstream callback skips the push. The caller MUST cancel() and wait on done: the shutdown
// drain runs in it.
func startCHLiveSink(parent context.Context, chAddr string, sink *atomic.Pointer[clickhouse.LiveSink], logger *slog.Logger) (context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// RECOVER: the sink is a best-effort mirror backstopped by the ch-live-catchup timer, so its
		// dial/watch loop dying must cost only the lake's real-time edge, never ingest.
		defer worker.Recover(logger, "ch-live-sink-dial")
		var live *clickhouse.LiveSink
		ok := retryUntil(ctx, logger, "ch live-sink: ClickHouse unavailable",
			chLiveSinkDialMinBackoff, chLiveSinkDialMaxBackoff,
			func(ctx context.Context) error {
				l, err := clickhouse.NewLiveSink(ctx, chAddr, clickhouse.LiveSinkOptions{
					StopTimeout: pipeline.CHLiveSinkStopBudget,
					Logger:      logger,
				})
				if err != nil {
					return err
				}
				live = l
				return nil
			})
		if !ok {
			return // ctx cancelled while waiting to retry
		}
		live.Start() //nolint:contextcheck // lifecycle call: Start takes no ctx by design, see LiveSink.add's per-op context.Background()
		sink.Store(live)
		// Log the effective (post-fallback) address, since clickhouse_addr may be unset.
		logger.Info("ClickHouse real-time dual-sink enabled", "addr", chAddr)
		// Emit counter deltas on a short interval so a CH write stall or bounded drop reaches
		// Prometheus, not just the shutdown log line.
		metricsStop, metricsDone := watchCHLiveSink(live, logger) //nolint:contextcheck // watchCHLiveSink deliberately uses context.Background(), not parent — see its doc: the last flush must survive the same SIGTERM that cancels ctx
		<-ctx.Done()
		// Drain the sink FIRST, then stop the watcher: its exit runs one final flush, which carries
		// the drain's written/dropped/errored deltas into Prometheus.
		live.Stop() //nolint:contextcheck // lifecycle call: Stop takes no ctx by design and must drain/flush even though ctx is already cancelled
		metricsStop()
		<-metricsDone
		logger.Info("ch live-sink drained on shutdown",
			"written", live.WrittenCount(),
			"buffered", live.BufferedCount(),
			"dropped", live.DroppedCount(),
			"errored", live.ErroredCount())
	}()
	return cancel, done
}

// retryUntil calls attempt until it succeeds or ctx ends, doubling the backoff from minBackoff to maxBackoff. False means ctx ended first:
// abandon the work, never use a half-built dependency.
func retryUntil(ctx context.Context, logger *slog.Logger, what string, minBackoff, maxBackoff time.Duration, attempt func(context.Context) error) bool {
	backoff := minBackoff
	for {
		err := attempt(ctx)
		if err == nil {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		if logger != nil {
			logger.Warn(what+" — retrying", "err", err, "retry_in", backoff)
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return false
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func setSourceEnabled(sources []string, enabled bool) {
	val := 0.0
	if enabled {
		val = 1
	}
	for _, source := range sources {
		if source == "" {
			continue
		}
		name := strings.ToLower(source)
		obs.SourceEnabled.WithLabelValues(name).Set(val)
		if enabled {
			// A counter first seen at its first event hides that event from rate(); the
			// 6h-30h source_stopped windows outlive the process, so start every enabled source at 0.
			obs.SourceEventsTotal.WithLabelValues(name).Add(0)
		}
	}
}

func watchDiscoveryDrops(sink *discovery.AsyncSink, logger *slog.Logger) (context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// RECOVER. A metrics bridge over three discovery-sink counters: it touches no data and
		// holds no lock the pipeline needs, so its death costs only visibility into dropped hits
		// (which worker_panics_total then reports), whereas a crash would stop ingest to keep a
		// gauge.
		defer worker.Recover(logger, "discovery-drop-watch")
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		var lastDropped, lastSkipped, lastFailed uint64
		flush := func() {
			lastDropped = emitDiscoveryDropMetricDelta(lastDropped, sink.DroppedCount(), logger)
			lastSkipped = emitDiscoverySkipMetricDelta(lastSkipped, sink.SkippedCount())
			lastFailed = emitDiscoveryRecordFailMetricDelta(lastFailed, sink.FailedCount(), logger)
		}

		for {
			select {
			case <-ticker.C:
				flush()
			case <-ctx.Done():
				flush()
				return
			}
		}
	}()
	return cancel, done
}

func emitDiscoveryDropMetricDelta(prev, current uint64, logger *slog.Logger) uint64 {
	if current <= prev {
		return current
	}
	delta := current - prev
	obs.DiscoveryDroppedHitsTotal.Add(float64(delta))
	if logger != nil {
		logger.Warn("discovery: hits dropped",
			"delta", delta,
			"total", current,
		)
	}
	return current
}

// emitDiscoveryRecordFailMetricDelta bridges the discovery sink's FailedCount() into obs.DiscoveryRecordFailuresTotal; without it a
// record-write failure is log-only and discovered_assets silently stops growing.
func emitDiscoveryRecordFailMetricDelta(prev, current uint64, logger *slog.Logger) uint64 {
	if current <= prev {
		return current
	}
	delta := current - prev
	obs.DiscoveryRecordFailuresTotal.Add(float64(delta))
	if logger != nil {
		logger.Warn("discovery: record writes failing",
			"delta", delta,
			"total", current,
		)
	}
	return current
}

// watchSorobanEventsSink bridges the raw-event sink's LostCount onto source_insert_errors_total{source="soroban-events",kind="dropped"}.
// It uses a background context like [watchDiscoveryDrops]: the final flush after Stop must outlive the root context.
func watchSorobanEventsSink(sink *sorobanevents.AsyncSink, logger *slog.Logger) (context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// RECOVER. A metrics bridge over one monotonic counter;
		// its death costs the loss signal, which worker_panics_total then
		// reports, and must never take ingest down with it.
		defer worker.Recover(logger, "soroban-events-lost-watch")
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		var lastLost uint64
		for {
			select {
			case <-ticker.C:
				lastLost = emitSorobanEventsLostMetricDelta(lastLost, sink.LostCount(), logger)
			case <-ctx.Done():
				emitSorobanEventsLostMetricDelta(lastLost, sink.LostCount(), logger)
				return
			}
		}
	}()
	return cancel, done
}

func emitSorobanEventsLostMetricDelta(prev, current uint64, logger *slog.Logger) uint64 {
	if current <= prev {
		return current
	}
	delta := current - prev
	obs.SourceInsertErrorsTotal.WithLabelValues(sorobanevents.SourceName, "dropped").Add(float64(delta))
	if logger != nil {
		logger.Error("soroban-events: rows permanently lost — re-derive from the CH lake (ADR-0034)",
			"delta", delta,
			"total", current,
		)
	}
	return current
}

// chExtractErrLog samples extract-failure WARNs 1-in-256: a meta-version break fails EVERY ledger. The counter carries the true rate.
var chExtractErrLog atomic.Uint64

func logCHExtractErrSampled(logger *slog.Logger, ledger uint32, err error) {
	if n := chExtractErrLog.Add(1); n%256 != 1 {
		return
	}
	logger.Warn("ch live-sink: extract failed (sampled 1/256) — lake live edge stalling",
		"ledger", ledger, "err", err, "errors_so_far", chExtractErrLog.Load())
}

// recordCHLiveSinkUndercount meters and logs a LedgerExtract's read-undercount; the ledger is still written, so this counter is the only
// alertable trace.
func recordCHLiveSinkUndercount(ext clickhouse.LedgerExtract, logger *slog.Logger) {
	if ext.TxReadErrors == 0 && ext.TxEventReadErrors == 0 && ext.EntryMetaUnsupported == 0 &&
		ext.SorobanFeeMetaUnsupported == 0 && ext.EvictedKeysUnreadable == 0 && ext.EntryChangesUnencodable == 0 {
		return
	}
	if ext.TxReadErrors > 0 {
		obs.ChLiveSinkReadUndercountTotal.WithLabelValues("tx_read_errors").Add(float64(ext.TxReadErrors))
	}
	if ext.TxEventReadErrors > 0 {
		obs.ChLiveSinkReadUndercountTotal.WithLabelValues("tx_event_read_errors").Add(float64(ext.TxEventReadErrors))
	}
	if ext.EntryMetaUnsupported > 0 {
		obs.ChLiveSinkReadUndercountTotal.WithLabelValues("entry_meta_unsupported").Add(float64(ext.EntryMetaUnsupported))
	}
	if ext.SorobanFeeMetaUnsupported > 0 {
		obs.ChLiveSinkReadUndercountTotal.WithLabelValues("soroban_fee_meta_unsupported").Add(float64(ext.SorobanFeeMetaUnsupported))
	}
	if ext.EvictedKeysUnreadable > 0 {
		obs.ChLiveSinkReadUndercountTotal.WithLabelValues("evicted_keys_unreadable").Add(float64(ext.EvictedKeysUnreadable))
	}
	if ext.EntryChangesUnencodable > 0 {
		obs.ChLiveSinkReadUndercountTotal.WithLabelValues("entry_changes_unencodable").Add(float64(ext.EntryChangesUnencodable))
	}
	logger.Warn("ch live-sink: ledger extracted with read undercount",
		"ledger", ext.Ledger.LedgerSeq,
		"tx_read_errors", ext.TxReadErrors,
		"tx_event_read_errors", ext.TxEventReadErrors,
		"entry_meta_unsupported", ext.EntryMetaUnsupported,
		"soroban_fee_meta_unsupported", ext.SorobanFeeMetaUnsupported,
		"evicted_keys_unreadable", ext.EvictedKeysUnreadable,
		"entry_changes_unencodable", ext.EntryChangesUnencodable)
}

// watchCHLiveSink samples the dual-sink's counters every 15 s and emits the
// per-tick delta on stellarindex_ch_live_sink_ledgers_total. The fan-out closure increments `errored` itself for ExtractLedger failures.
// It runs on context.Background(), NOT the root context, so the final deltas after LiveSink.Stop() still land (main stops it explicitly).
func watchCHLiveSink(sink *clickhouse.LiveSink, logger *slog.Logger) (context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// RECOVER. A counter-sampling loop that cannot lose a ledger (the sink owns those), so its
		// death costs only the lake's write-rate visibility, which worker_panics_total reports.
		defer worker.Recover(logger, "ch-live-sink-watch")
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		var lastWritten, lastBuffered, lastDropped, lastErrored uint64
		emit := func(outcome string, prev, current uint64) uint64 {
			if current > prev {
				obs.ChLiveSinkLedgersTotal.WithLabelValues(outcome).Add(float64(current - prev))
			}
			return current
		}
		flush := func() {
			lastWritten = emit("written", lastWritten, sink.WrittenCount())
			lastBuffered = emit("buffered", lastBuffered, sink.BufferedCount())
			// Drops are the operationally-interesting outcome — WARN when the
			// counter advances this tick (the metric carries the precise rate).
			dropped := sink.DroppedCount()
			if dropped > lastDropped {
				logger.Warn("ch live-sink: ledgers bounded-dropped since last tick",
					"delta", dropped-lastDropped, "dropped_total", dropped)
			}
			lastDropped = emit("dropped", lastDropped, dropped)
			lastErrored = emit("errored", lastErrored, sink.ErroredCount())
		}
		for {
			select {
			case <-ticker.C:
				flush()
			case <-ctx.Done():
				flush()
				return
			}
		}
	}()
	return cancel, done
}

// watchPostgresPing pings every 60 s and emits the pool resilience metrics: the OBSERVABILITY hook (reconnect is database/sql's) so a
// stuck pool surfaces via the `stellarindex_postgres_ping_failing` alert. It warns when the failure streak crosses 3 (about 3 min).
func watchPostgresPing(parent context.Context, store *timescale.Store, logger *slog.Logger) (context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// RECOVER. This is the observability hook, not the repair: the reconnect is database/sql's
		// PoolConnMaxLifetime and works whether or not this loop runs, so crashing ingest over a
		// faulted prober would be the alarm burning the building down. A stopped prober freezes
		// stellarindex_postgres_ping_* at its last value (the stale-gauge trap), so the panic
		// counter is the signal that the freeze is a dead worker, not a healthy pool.
		defer worker.Recover(logger, "postgres-ping")
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()

		var failures int
		// Probe once immediately so the metric is non-empty
		// before the first 60 s tick — gives a clean scrape
		// at process boot.
		failures = postgresPingProbe(ctx, store, logger, failures)
		for {
			select {
			case <-ticker.C:
				failures = postgresPingProbe(ctx, store, logger, failures)
			case <-ctx.Done():
				return
			}
		}
	}()
	return cancel, done
}

// postgresPingProbe runs one ping cycle and returns the new consecutive-failure count (split out for gocognit).
func postgresPingProbe(ctx context.Context, store *timescale.Store, logger *slog.Logger, prevFailures int) int {
	pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
	defer pcancel()
	if err := store.PingContext(pctx); err != nil {
		failures := prevFailures + 1
		obs.PostgresPingTotal.WithLabelValues("error").Inc()
		obs.PostgresPingFailureStreak.Set(float64(failures))
		logPostgresPingFailure(logger, err, failures)
		return failures
	}
	if prevFailures > 0 && logger != nil {
		logger.Info("postgres ping recovered", "previous_streak", prevFailures)
	}
	obs.PostgresPingTotal.WithLabelValues("ok").Inc()
	obs.PostgresPingFailureStreak.Set(0)
	return 0
}

// logPostgresPingFailure emits the per-failure line; the `streak == 3` message "pool may be wedged" is the canonical string operators grep
// for.
func logPostgresPingFailure(logger *slog.Logger, err error, streak int) {
	if logger == nil {
		return
	}
	if streak == 3 {
		logger.Error("postgres ping failed 3x — pool may be wedged",
			"err", err,
			"streak", streak,
			"safety_net", "ConnMaxLifetime refresh pending",
		)
		return
	}
	logger.Warn("postgres ping failed", "err", err, "streak", streak)
}

// emitDiscoverySkipMetricDelta updates the skip counter without logging: skips are the healthy steady state under in-process dedup.
func emitDiscoverySkipMetricDelta(prev, current uint64) uint64 {
	if current <= prev {
		return current
	}
	obs.DiscoverySkippedHitsTotal.Add(float64(current - prev))
	return current
}

// aggregatorPairsFromCatalogue crosses every catalogue ticker with a `coingecko_id` (the others key off the ticker, so a missing CG slug
// is the
// conservative "skip" signal) with the fixed fiat list USD / EUR / GBP, the divergence layer's coverage.
func aggregatorPairsFromCatalogue(cat *currency.Catalogue, logger *slog.Logger) []canonical.Pair {
	if cat == nil {
		return nil
	}
	cgIDs := cat.CoinGeckoIDs()
	if len(cgIDs) == 0 {
		return nil
	}
	fiats := []string{"USD", "EUR", "GBP"}
	out := make([]canonical.Pair, 0, len(cgIDs)*len(fiats))
	var skipped []string
	for ticker := range cgIDs {
		ca, err := canonical.NewCryptoAsset(ticker)
		if err != nil {
			// Ticker not on the canonical crypto allow-list (e.g. a
			// future entry we haven't added). Skip — adding to the
			// allow-list is a separate, deliberate change. Recorded
			// below so the gap is visible instead of a silent drop.
			skipped = append(skipped, ticker)
			continue
		}
		for _, f := range fiats {
			fa, err := canonical.NewFiatAsset(f)
			if err != nil {
				continue
			}
			p, err := canonical.NewPair(ca, fa)
			if err != nil {
				continue
			}
			out = append(out, p)
		}
	}
	reportSkippedAggregatorTickers(skipped, logger)
	return out
}

// reportSkippedAggregatorTickers surfaces the gap aggregatorPairsFromCatalogue finds: an active coingecko_id that canonical.NewCryptoAsset
// rejects.
func reportSkippedAggregatorTickers(skipped []string, logger *slog.Logger) {
	obs.AggregatorCatalogueTickersSkipped.Set(float64(len(skipped)))
	if len(skipped) == 0 || logger == nil {
		return
	}
	sort.Strings(skipped)
	logger.Warn("aggregator catalogue: tickers excluded from cross-check pairs — not on ADR-0014 crypto allow-list",
		"tickers", skipped, "count", len(skipped))
}

// mergeAggregatorPairs unions the catalogue pairs with defaultAggregatorPairs, deduplicated by Pair.String(); the catalogue supplements
// the
// fallback, so a hardcoded ticker without a coingecko_id keeps its coverage.
func mergeAggregatorPairs(catalogue, defaults []canonical.Pair) []canonical.Pair {
	out := make([]canonical.Pair, 0, len(catalogue)+len(defaults))
	seen := make(map[string]struct{}, len(catalogue)+len(defaults))
	for _, sets := range [][]canonical.Pair{catalogue, defaults} {
		for _, p := range sets {
			key := p.String()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

// defaultAggregatorPairs is the fallback pair set aggregators query for cross-check when the catalogue isn't wired; add entries to
// `internal/currency/data/seed.yaml`, the canonical source.
func defaultAggregatorPairs() []canonical.Pair {
	// Anchors + top-cap globals. XLM first per its product-special
	// status; the rest in alphabetical order to keep diffs minimal.
	cryptos := []string{
		"XLM", "BTC", "ETH",
		"ADA", "ATOM", "AVAX", "BCH", "BNB", "DASH", "DOGE", "DOT",
		"LINK", "LTC", "NEAR", "SHIB", "SOL", "TON", "TRX", "UNI", "XRP",
	}
	fiats := []string{"USD", "EUR", "GBP"}
	out := make([]canonical.Pair, 0, len(cryptos)*len(fiats))
	for _, c := range cryptos {
		ca, err := canonical.NewCryptoAsset(c)
		if err != nil {
			continue
		}
		for _, f := range fiats {
			fa, err := canonical.NewFiatAsset(f)
			if err != nil {
				continue
			}
			p, err := canonical.NewPair(ca, fa)
			if err != nil {
				continue
			}
			out = append(out, p)
		}
	}
	return out
}

// defaultFXPairs returns the G10-ish fiat cross-rate set against base; all FX pollers share this target list (override per poller via
// Symbols).
func defaultFXPairs(base string) []canonical.Pair {
	return external.DefaultFXPairs(base)
}

// ─── Dispatcher wiring ──────────────────────────────────────────
// ─── Ledger processing ─────────────────────────────────────────
// cursorReader is the one method [resolveStartLedger] needs from [timescale.Store]; read-only, so precedence rules test without a
// database.
type cursorReader interface {
	GetCursor(ctx context.Context, source, sub string) (timescale.Cursor, error)
}

// resolveStartLedger picks the start ledger: persisted cursor+1, else cfg.Ingestion.BackfillFromLedger, else an error
// (a silent default ledger is how operators re-ingest genesis by accident).
func resolveStartLedger(ctx context.Context, store cursorReader, backfillFrom uint32) (uint32, error) {
	c, err := store.GetCursor(ctx, cursorSource, "")
	switch {
	case errors.Is(err, timescale.ErrNotFound):
		if backfillFrom == 0 {
			return 0, fmt.Errorf(
				"no persisted cursor and ingestion.backfill_from_ledger=0 — " +
					"set backfill_from_ledger to an explicit start, e.g. the " +
					"current network tip",
			)
		}
		return backfillFrom, nil
	case err != nil:
		return 0, fmt.Errorf("load cursor: %w", err)
	}
	return c.LastLedger + 1, nil
}

// cursorRewinder is what [rewindCursorForSinkLoss] needs from
// [timescale.Store]; narrow so the rewind rules run without a database.
type cursorRewinder interface {
	cursorReader
	RewindCursor(ctx context.Context, source, sub string, lastLedger uint32) (uint32, error)
}

// rewindCursorForSinkLoss moves the cursor below the lowest on-chain trade the sink abandoned, so the next start re-walks it (served-tier
// writes are
// ON CONFLICT idempotent). Only safe once the producer has stopped: a live producer's next UpsertCursor would carry the cursor over the
// rewind.
func rewindCursorForSinkLoss(ctx context.Context, store cursorRewinder, loss pipeline.ShutdownLoss, producerStopped bool, logger *slog.Logger) bool {
	if loss.Rows == 0 {
		return false
	}
	if loss.MinLedger == 0 {
		// Only ledger-less rows (non-trade events, off-chain trades): no
		// safe rewind target, so the undrained counter + ERROR stay the record.
		logger.Error("sink abandoned rows on exit with no ledger to rewind to — re-derive manually",
			"rows", loss.Rows)
		return false
	}
	target := loss.MinLedger - 1
	if !producerStopped {
		logger.Error("sink abandoned trades on exit but the ledgerstream producer is still running — cursor NOT rewound, re-derive manually",
			"rows", loss.Rows, "min_ledger", loss.MinLedger)
		return false
	}
	c, err := store.GetCursor(ctx, cursorSource, "")
	if err != nil {
		logger.Error("sink abandoned trades on exit; reading the cursor to rewind it failed — re-derive manually",
			"rows", loss.Rows, "min_ledger", loss.MinLedger, "err", err)
		return false
	}
	if c.LastLedger <= target {
		// The abandoned ledger was never checkpointed; the next start re-walks it anyway.
		logger.Warn("sink abandoned trades on exit at or past the cursor — no rewind needed",
			"rows", loss.Rows, "min_ledger", loss.MinLedger, "cursor", c.LastLedger)
		return false
	}
	prior, err := store.RewindCursor(ctx, cursorSource, "", target)
	if err != nil {
		logger.Error("sink abandoned trades on exit; cursor rewind failed — re-derive manually",
			"rows", loss.Rows, "min_ledger", loss.MinLedger, "err", err)
		return false
	}
	logger.Warn("rewound ledgerstream cursor below trades the sink abandoned on exit",
		"rows", loss.Rows, "prior_ledger", prior, "new_ledger", target,
		"ledger_unknown", loss.LedgerUnknown)
	return true
}

// processAndPersistCursor wraps pipeline.ProcessLedger with the cursor upsert and metric so a restart resumes at cursor+1.
// Backfill does NOT call it: it has explicit -from/-to and shares no cursor row.
func processAndPersistCursor(
	ctx context.Context,
	disp *dispatcher.Dispatcher,
	events chan<- consumer.Event,
	store *timescale.Store,
	logger *slog.Logger,
	lcm sdkxdr.LedgerCloseMeta,
	networkPassphrase string,
	accountObserverActive bool,
) error {
	if err := pipeline.ProcessLedger(ctx, disp, events, logger, lcm, networkPassphrase); err != nil {
		return err
	}
	// ADR-0033: write the substrate-continuity record once the ledger's events are ENQUEUED to
	// the sink channel. ProcessLedger returning nil guarantees only that, since the sink
	// persists asynchronously, so this is NOT a "rows are in postgres" marker; projection
	// reconcile is what proves the rows landed. Best-effort: a write failure must not stall
	// ingest, because the gap surfaces in the substrate continuity check.
	recordLedgerIngest(ctx, store, logger, lcm, networkPassphrase)
	if err := store.UpsertCursor(ctx, cursorSource, "", lcm.LedgerSequence()); err != nil {
		// Do NOT advance the cursor gauge on a persist failure. The
		// gauge tracks the DURABLE resume position — on restart, ingest
		// resumes from the last successfully-upserted cursor, so a gauge that
		// advanced past it would hide the stall/gap from the cursor-lag alert.
		logger.Warn("cursor upsert",
			"ledger", lcm.LedgerSequence(),
			"err", err,
		)
	} else {
		recordCursorMetric(lcm.LedgerSequence())
	}
	// Advance the account observer's TRUE processed-ledger watermark, only when the observer is
	// registered (SDF reserve accounts configured). It runs for every live ledger the observer
	// saw, whether or not a watched account changed, so the XLM supply freshness anchor tracks
	// observer liveness. Best-effort: a failed write lags the anchor a tick and the monotonic
	// guard catches it up next ledger.
	if accountObserverActive {
		if err := store.UpsertAccountObserverWatermark(ctx, lcm.LedgerSequence()); err != nil {
			logger.Warn("account observer watermark upsert",
				"ledger", lcm.LedgerSequence(),
				"err", err,
			)
		}
	}
	return nil
}

// recordLedgerIngest writes the ledger_ingest_log census. The census is a SECOND, independent LCM walk on purpose, so a bug in the
// dispatch walk cannot hide in it.
func recordLedgerIngest(
	ctx context.Context,
	store *timescale.Store,
	logger *slog.Logger,
	lcm sdkxdr.LedgerCloseMeta,
	networkPassphrase string,
) {
	census, err := dispatcher.CensusLedger(lcm, networkPassphrase)
	if err != nil {
		logger.Warn("ledger census", "ledger", lcm.LedgerSequence(), "err", err)
		return
	}
	if census.TxReadErrors > 0 || census.TxEventReadErrors > 0 {
		// A ledger we could not fully read (a malformed tx, or a tx whose GetTransactionEvents
		// failed, e.g. an unsupported future TransactionMeta version) undercounts its primitives:
		// SorobanEventCount drops to zero in lock-step with the live sink, so a projection
		// reconcile would falsely pass. Don't write an authoritative row we can't stand behind;
		// leave a substrate gap so a re-run on a fixed reader rewrites it. Mirrors the offline
		// census-backfill path (internal/ops/ingest/census_backfill.go).
		recordLedgerIngestCensusSkip(census)
		logger.Warn("ledger census read errors; skipping substrate record",
			"ledger", census.LedgerSeq,
			"tx_read_errors", census.TxReadErrors,
			"tx_event_read_errors", census.TxEventReadErrors)
		return
	}
	row := timescale.LedgerIngestRow{
		LedgerSeq:               census.LedgerSeq,
		LedgerCloseTime:         census.LedgerCloseTime,
		LedgerHash:              census.LedgerHash[:],
		PrevLedgerHash:          census.PrevLedgerHash[:],
		SorobanEventCount:       census.SorobanEventCount,
		ClassicTradeEffectCount: census.ClassicTradeEffectCount,
	}
	if err := store.UpsertLedgerIngestLog(ctx, row); err != nil {
		logger.Warn("ledger ingest log upsert", "ledger", census.LedgerSeq, "err", err)
	}
}

// recordLedgerIngestCensusSkip is recordCHLiveSinkUndercount's twin for the
// ledger_ingest_log path, where a non-zero census skips the substrate row.
func recordLedgerIngestCensusSkip(census dispatcher.Census) {
	if census.TxReadErrors > 0 {
		obs.ChLiveSinkReadUndercountTotal.WithLabelValues("tx_read_errors_census").Add(float64(census.TxReadErrors))
	}
	if census.TxEventReadErrors > 0 {
		obs.ChLiveSinkReadUndercountTotal.WithLabelValues("tx_event_read_errors_census").Add(float64(census.TxEventReadErrors))
	}
}

func recordCursorMetric(ledger uint32) {
	obs.CursorLastLedger.WithLabelValues(cursorSource).Set(float64(ledger))
}

// ─── HashDB drift detector ───────────────────────────────

// hashDBVerifyInterval / hashDBVerifyWindow schedule the periodic drift sweep;
// the window (~1 day of ledgers) trails the live-append edge.
const (
	hashDBVerifyInterval = 60 * time.Minute
	hashDBVerifyWindow   = uint32(20000)

	// hashDBVerifySafetyMargin trails the verify window's upper bound behind lastAppended: cheap defense in depth, and keeps the sweep off
	// the bucket's
	// bleeding edge (galexie's upload can lag the indexer's read by a few ledgers on a restart/catch-up race).
	hashDBVerifySafetyMargin = uint32(16)
)

// openOrCreateHashDB opens the hashdb file at path, creating it (starting at startLedger) if
// missing, so enabling cfg.HashDB.Enabled needs no separate bootstrap step.
func openOrCreateHashDB(path string, startLedger uint32) (*hashdb.DB, error) {
	db, err := hashdb.Open(path)
	if err == nil {
		return db, nil
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Fresh region: fall through to Create.
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		// A file shorter than the 16-byte header is the residue of a Create whose header never
		// reached disk (e.g. power loss), so no record was stored and recreating loses nothing.
		// Without this arm a 0-byte file crash-loops the daemon. Anything else (bad magic, bad
		// version, permissions) stays fail-closed: recreating a file that HAS content would
		// discard the tamper-evidence baseline this detector exists to keep.
		if rmErr := os.Remove(path); rmErr != nil {
			return nil, fmt.Errorf("hashdb: remove truncated file %s: %w", path, rmErr)
		}
	default:
		return nil, err
	}
	return hashdb.Create(path, startLedger)
}

// marshalLedgerCloseMeta is indirected so TestRecordHashdb_DurationExcludesMarshal can inject a slow marshal; production always takes the
// default.
var marshalLedgerCloseMeta = func(lcm sdkxdr.LedgerCloseMeta) ([]byte, error) {
	return lcm.MarshalBinary()
}

// recordHashdb appends sha256(LCM) into hdb once per ledger, synchronously (Append is one 32-byte WriteAt; a channel and drop policy would
// be risk for no gain).
// Errors log a WARN and bump HashdbAppendTotal{"error"} but never propagate: a diagnostics side channel must not stall customer-data
// ingest.
func recordHashdb(hdb *hashdb.DB, lcm sdkxdr.LedgerCloseMeta, logger *slog.Logger, lastAppended *atomic.Uint32) {
	seq := lcm.LedgerSequence()

	raw, err := marshalLedgerCloseMeta(lcm)
	// Timed region starts AFTER the multi-megabyte LedgerCloseMeta
	// marshal — HashdbAppendDurationSeconds documents itself as the
	// latency of the O(1) positional WriteAt, not of serializing the
	// whole ledger.
	start := time.Now()
	if err == nil {
		h := hashdb.Hash(raw)
		// Verify-then-Append-on-ErrMissing: a re-ingested ledger (restart, cursor rewind) must not
		// overwrite a recorded hash, which would erase the drift baseline for the one case (a
		// rewritten upstream object) the detector exists to catch.
		verr := hdb.Verify(seq, h)
		switch {
		case verr == nil:
			// Already recorded and matches — nothing to write.
		case errors.Is(verr, hashdb.ErrMissing):
			err = hdb.Append(seq, h)
		default:
			if errors.Is(verr, hashdb.ErrDrift) {
				obs.HashdbDriftTotal.Add(1)
				logger.Error("hashdb drift on live-append — refusing to overwrite recorded hash", "ledger", seq, "err", verr)
			}
			err = verr
		}
	}
	if err != nil {
		obs.HashdbAppendTotal.WithLabelValues("error").Inc()
		obs.HashdbAppendDurationSeconds.WithLabelValues("error").Observe(time.Since(start).Seconds())
		logger.Warn("hashdb append failed — drift detector not recording this ledger", "ledger", seq, "err", err)
		return
	}
	obs.HashdbAppendTotal.WithLabelValues("ok").Inc()
	obs.HashdbAppendDurationSeconds.WithLabelValues("ok").Observe(time.Since(start).Seconds())
	// Store AFTER Append succeeds — the periodic verifier's window
	// must never include a ledger that isn't durably recorded yet.
	lastAppended.Store(seq)
}

// startHashDBVerifier runs the periodic hashdb drift check: every cfg.VerifyIntervalMinutes it re-reads a trailing cfg.VerifyWindowLedgers
// window from the
// live bucket (lsCfg) and compares against verifyDB. Known limit: mid-catch-up the window can name ledgers the bucket lacks, giving
// outcome="error" until it crosses the archive/live seam.
func startHashDBVerifier(
	parent context.Context,
	verifyDB *hashdb.DB,
	lsCfg ledgerstream.Config,
	lastAppended *atomic.Uint32,
	logger *slog.Logger,
) (context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})

	// Consecutive ticks re-read an overlapping trailing window, so a still-drifted ledger is
	// observed on every tick it falls inside. seenDrifted is this goroutine's record of sequences
	// already tallied into HashdbDriftTotal, so the counter counts distinct drifted ledgers, not
	// observations.
	seenDrifted := make(map[uint32]struct{})

	go func() {
		defer close(done)
		// RECOVER. The sweep re-marshals arbitrary archived LedgerCloseMeta, so it is the goroutine
		// most likely to meet a malformed object, and a tamper detector must not become a way to
		// stop the node. A recovered sweep leaves its ledger unverified, but the next tick's
		// overlapping window re-reads it, so detection is delayed, not lost. Silence is the danger
		// for a detector: the panic counter is the point, since a stopped verifier otherwise looks
		// exactly like a clean chain.
		defer worker.Recover(logger, "hashdb-verify")
		ticker := time.NewTicker(hashDBVerifyInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				hashDBVerifySweep(ctx, logger, verifyDB, lsCfg, lastAppended, hashDBVerifyWindow, seenDrifted)
			case <-ctx.Done():
				return
			}
		}
	}()
	return cancel, done
}

// hashDBSweepComplete reports whether a pass actually checked its window: every ledger accounted for AND Verified > 0. An all
// Missing/OutOfRange window compared
// no hash, which is "we don't know", not "clean".
func hashDBSweepComplete(res archivecompleteness.HashDBVerifyResult, observed int, from, to uint32) bool {
	return observed == int(to-from)+1 && res.Verified > 0
}

// countNewDrift returns how many of res's drifted ledgers are not yet counted into HashdbDriftTotal, marking them seen. seen is nil for a
// one-off pass (all new);
// the periodic sweep re-reads an overlapping window, so without dedup a drifted ledger would inflate the counter every tick.
func countNewDrift(res archivecompleteness.HashDBVerifyResult, seen map[uint32]struct{}) int {
	if seen == nil {
		return res.Drifted
	}
	newly := 0
	for _, seq := range res.DriftSeqs {
		if _, ok := seen[seq]; !ok {
			seen[seq] = struct{}{}
			newly++
		}
	}
	if excess := res.Drifted - len(res.DriftSeqs); excess > 0 {
		// DriftSeqs is capped (MaxHashDBDriftSeqsReported); beyond the
		// cap individual sequences aren't identifiable to dedupe, so
		// count them raw rather than dropping a real drift signal.
		newly += excess
	}
	return newly
}

// hashDBVerifyPass runs one bounded verify over an explicit [from, to] and records its outcome (window is the runs-counter label); shared
// by the
// live trailing sweep and the operator's older-range mode.
func hashDBVerifyPass(
	ctx context.Context,
	logger *slog.Logger,
	verifyDB *hashdb.DB,
	lsCfg ledgerstream.Config,
	from, to uint32,
	seenDrifted map[uint32]struct{},
	window string,
) hashDBVerifySweepOutcome {
	// Stream STRICT: the shared live-tail config tolerates trailing-missing objects (a live reader
	// racing galexie's upload edge), but this window trails ledgers the indexer already read, so
	// "object missing" here is the deleted-history tamper class hashdb exists to catch, not a
	// race. With tolerance on, a deleted object silently ended the sweep early with outcome="ok",
	// a vacuously green detector.
	lsCfg = hashDBVerifyStreamConfig(lsCfg)

	start := time.Now()
	verifier := archivecompleteness.NewHashDBWindowVerifier(verifyDB, from, to)
	streamErr := ledgerstream.Stream(ctx, lsCfg, from, to, func(lcm sdkxdr.LedgerCloseMeta) error {
		raw, merr := lcm.MarshalBinary()
		if merr != nil {
			return fmt.Errorf("marshal ledger %d: %w", lcm.LedgerSequence(), merr)
		}
		return verifier.Observe(lcm.LedgerSequence(), hashdb.Hash(raw))
	})
	res := verifier.Result()
	dur := time.Since(start).Seconds()

	outcome := classifyHashDBVerifySweep(res, streamErr, from, to)
	switch outcome {
	case sweepOutcomeShutdown:
		// Shutdown mid-walk with nothing found: don't record any
		// outcome — a partial, driftless sweep is neither clean nor
		// failing, and stamping it "ok" would be the same
		// vacuous-green lie the strict-stream change above removes.
		return outcome
	case sweepOutcomeDrift:
		// Drift FIRST — even when the stream ALSO errored (including
		// a shutdown cancel) mid-walk, drift already tallied is the
		// signal this detector exists for; an error or cancel must
		// not suppress it.
		obs.HashdbVerifyRunsTotal.WithLabelValues("drift", window).Inc()
		obs.HashdbVerifyRunDurationSeconds.WithLabelValues("drift").Observe(dur)
		obs.HashdbDriftTotal.Add(float64(countNewDrift(res, seenDrifted)))
		// Loud: drift means rewritten upstream history or a corrupted
		// lake object — see docs/operations/runbooks/hashdb.md.
		logger.Error("hashdb DRIFT DETECTED — upstream history rewritten or lake object corrupted",
			"from", from, "to", to,
			"verified", res.Verified, "drifted", res.Drifted,
			"missing", res.Missing, "out_of_range", res.OutOfRange,
			"drifted_ledgers", res.DriftSeqs,
			"stream_err", streamErr,
		)
	case sweepOutcomeError:
		obs.HashdbVerifyRunsTotal.WithLabelValues("error", window).Inc()
		obs.HashdbVerifyRunDurationSeconds.WithLabelValues("error").Observe(dur)
		logger.Warn("hashdb verify sweep failed", "from", from, "to", to, "err", streamErr)
	case sweepOutcomeIncomplete:
		// A complete sweep observed every ledger in the window
		// exactly once; anything short means the stream ended early
		// without erroring, and "we couldn't check everything" must
		// not read as clean.
		observed := res.Verified + res.Drifted + res.Missing + res.OutOfRange
		obs.HashdbVerifyRunsTotal.WithLabelValues("error", window).Inc()
		obs.HashdbVerifyRunDurationSeconds.WithLabelValues("error").Observe(dur)
		logger.Warn("hashdb verify sweep incomplete — stream ended early without error, or no ledger in the window had a recorded baseline to compare against",
			"from", from, "to", to, "observed", observed, "expected", int(to-from)+1, "verified", res.Verified,
		)
	default: // sweepOutcomeOK
		obs.HashdbVerifyRunsTotal.WithLabelValues("ok", window).Inc()
		obs.HashdbVerifyRunDurationSeconds.WithLabelValues("ok").Observe(dur)
		logger.Info("hashdb verify sweep clean",
			"from", from, "to", to,
			"verified", res.Verified, "missing", res.Missing, "out_of_range", res.OutOfRange,
		)
	}
	return outcome
}

// hashDBVerifySweepOutcome is what a sweep pass amounts to, split out so the priority (drift outranks a shutdown cancel) is unit-testable.
type hashDBVerifySweepOutcome int

const (
	sweepOutcomeShutdown hashDBVerifySweepOutcome = iota
	sweepOutcomeDrift
	sweepOutcomeError
	sweepOutcomeIncomplete
	sweepOutcomeOK
)

// classifyHashDBVerifySweep turns a sweep's verifier result and stream error into the recorded outcome. Drift is checked BEFORE the
// shutdown-cancel
// case: drift tallied before context.Canceled is still drift.
func classifyHashDBVerifySweep(res archivecompleteness.HashDBVerifyResult, streamErr error, from, to uint32) hashDBVerifySweepOutcome {
	if res.AnyDrift() {
		return sweepOutcomeDrift
	}
	if streamErr != nil && errors.Is(streamErr, context.Canceled) {
		return sweepOutcomeShutdown
	}
	if streamErr != nil {
		return sweepOutcomeError
	}
	observed := res.Verified + res.Drifted + res.Missing + res.OutOfRange
	if !hashDBSweepComplete(res, observed, from, to) {
		return sweepOutcomeIncomplete
	}
	return sweepOutcomeOK
}

// newMetricsMux serves /metrics, the constant liveness /healthz, and /readyz, the schema-head check the deploy gate probes, so a binary
// swapped
// ahead of its migrations fails the deploy instead of passing `systemctl is-active` while every write errors.
func newMetricsMux(schema v1.SchemaVersionReader) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", obs.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /readyz", v1.ReadyzHandler(v1.NewSchemaVersionChecker(schema)))
	return mux
}

func startMetricsServer(obsCfg config.ObsConfig, schema v1.SchemaVersionReader, logger *slog.Logger) *http.Server {
	if obsCfg.MetricsListen == "" {
		logger.Warn("obs.metrics_listen is empty — /metrics endpoint disabled; Prometheus alerts on source metrics will not fire")
		return nil
	}
	srv := &http.Server{
		Addr:              obsCfg.MetricsListen,
		Handler:           newMetricsMux(schema),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		// CRASH, deliberately unguarded, as the API and aggregator listeners are. net/http already
		// isolates per-request handler panics, so a panic here is a fault in the accept loop. A
		// recover would leave the process running with nothing on /metrics, and the guard's only
		// output is a counter read over that very endpoint, so it would silence itself. Exiting
		// hands the decision to Prometheus's `up == 0` and systemd, both of which act.
		logger.Info("metrics endpoint listening", "addr", obsCfg.MetricsListen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server exited", "err", err)
		}
	}()
	return srv
}

func mkLogger(cfg config.ObsConfig) *slog.Logger {
	return obs.NewLogger(cfg, "stellarindex-indexer")
}

// discoveryRecorderAdapter wraps *timescale.Store as discovery.Recorder, mapping the table-suffixed names (RecordDiscovered /
// IsKnownDiscovered) to Record / IsKnown.
type discoveryRecorderAdapter struct {
	s *timescale.Store
}

func (a discoveryRecorderAdapter) Record(ctx context.Context, hit discovery.Hit) error {
	return a.s.RecordDiscovered(ctx, hit)
}

func (a discoveryRecorderAdapter) IsKnown(ctx context.Context, contractID string) (bool, error) {
	return a.s.IsKnownDiscovered(ctx, contractID)
}

// Compile-time guard.
var _ discovery.Recorder = discoveryRecorderAdapter{}
