// Binary stellarindex-aggregator computes VWAP over the ingested canonical trade stream and writes
// pre-aggregated results to Redis, so API requests serve from cache rather than recomputing. Its
// workers (VWAP, triangulation, outlier filter, confidence + ADR-0019 freeze, divergence cache,
// supply snapshots) are each driven from one orchestrator.Config field
// (internal/aggregate/orchestrator); TOML knobs are under [aggregate] in
// docs/reference/config/README.md.
//
// VWAP is exchange-class only by default: aggregator and oracle classes would double-count or mix
// methodologies. CAGG refresh stays Timescale-driven; the orchestrator never refreshes manually.
//
// Flags:
//
//	-config PATH    TOML config file (required).
//	-dry-run        Load config, open connections, validate, exit.
//
// SIGINT and SIGTERM cancel the root context; Tick unwinds on its next iteration.
//
// A cagg `twap` column is not necessarily time-weighted (migration 0002 defined it as the
// arithmetic mean of trade prices); the time-weighted computation is internal/aggregate/twap.go.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/assetcharacterrollup"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/assetvolrollup"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/changesummary"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/mev"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/orchestrator"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/protoeventsrollup"
	"github.com/Stellar-Index/StellarIndex/internal/api/streaming/redispub"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/customerwebhook"
	"github.com/Stellar-Index/StellarIndex/internal/decimalsguard"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
	"github.com/Stellar-Index/StellarIndex/internal/pricealerts"
	"github.com/Stellar-Index/StellarIndex/internal/pricelesscoverage"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/redact"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/redisclient"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
	"github.com/Stellar-Index/StellarIndex/internal/version"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// Baseline-refresh tunables. Deliberately not surfaced as TOML knobs
// in this slice — sensible defaults today, an operator override only
// if production usage shows we need it.
const (
	// baselineRefreshCadence: how often to recompute every pair's
	// baseline. 30-day rolling MAD barely moves minute-to-minute, so
	// hourly is plenty (and cheap on the hypertable).
	baselineRefreshCadence = 1 * time.Hour
	// baselineRefreshConcurrency: pairs computed in flight at once.
	// 4 keeps the DB connection pool well-fed without saturating it
	// even on the largest pair sets.
	baselineRefreshConcurrency = 4
)

func main() {
	var (
		cfgPath     = flag.String("config", "", "Path to TOML config file (required)")
		dryRun      = flag.Bool("dry-run", false, "Load config + open connections + exit without running the ticker")
		showVersion = flag.Bool("version", false, "Print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "stellarindex-aggregator: -config is required")
		flag.Usage()
		os.Exit(2)
	}

	if err := run(*cfgPath, *dryRun); err != nil {
		fmt.Fprintf(os.Stderr, "stellarindex-aggregator: %s\n", redact.Credentials(err.Error()))
		os.Exit(1)
	}
}

// formatDivergencePrice renders a divergence comparator price as a decimal string. The values are
// float64 external-reference arithmetic (not a stored canonical.Amount), so strconv preserves
// exactly what was computed instead of inventing fixed-point precision.
func formatDivergencePrice(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// divergenceSourceNames renders the divergence comparator's per-source
// price map as the array of source NAMES the spec documents, not the
// map MarshalPayload's plain json.Marshal would otherwise serialize
// (an object keyed on name -> price).
func divergenceSourceNames(sources map[string]float64) []string {
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// anomalyFreezeWebhookPayload is the JSON body of an `anomaly.freeze` webhook delivery;
// TestAnomalyFreezeWebhookPayloadMatchesSpec keeps it in lockstep with the OpenAPI spec.
type anomalyFreezeWebhookPayload struct {
	Event       string `json:"event"`
	Asset       string `json:"asset"`
	Quote       string `json:"quote"`
	FrozenValue string `json:"frozen_value,omitempty"` // absent on a first-tick freeze: no price to pin
	Reason      string `json:"reason"`
	At          string `json:"at"`
}

// divergenceFiringWebhookPayload is the JSON body of a `divergence.firing` webhook delivery;
// TestDivergenceFiringWebhookPayloadMatchesSpec keeps it in lockstep with the OpenAPI spec.
type divergenceFiringWebhookPayload struct {
	Event         string   `json:"event"`
	Pair          string   `json:"pair"`
	OurPrice      string   `json:"our_price"`
	Median        string   `json:"median"`
	DivergencePct float64  `json:"divergence_pct"`
	SuccessCount  int      `json:"success_count"`
	Sources       []string `json:"sources"`
	At            string   `json:"at"`
}

//nolint:gocognit,gocyclo,funlen // top-level binary lifecycle — splitting reduces readability of dependency-construction order
func run(cfgPath string, dryRun bool) error {
	cfg, err := config.LoadWithEnv(cfgPath)
	if err != nil {
		return err
	}

	// Publish the process-wide asset alias registry BEFORE any tier walk: the alias-looping paths
	// (tryVWAPTier etc.) resolve classic<->SAC pairs through it, SAC form LAST. Fail-closed on a
	// malformed wrapper, since a silently dropped one becomes under-counted volume.
	if err := canonical.InstallNetwork(cfg.Stellar.Passphrase(), cfg.Supply.SACWrappers); err != nil {
		return err
	}

	logger := mkLogger(cfg.Obs)
	logger.Info("starting",
		"version", version.String(),
		"region", cfg.Region.ID,
		"dry_run", dryRun,
	)

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	// ─── Storage ─────────────────────────────────────────────────
	// OpenBackground applies a generous session-level statement_timeout as
	// the SQL-side runaway backstop. Heavy batch scans SET LOCAL
	// a longer bound inside their own transactions, overriding it.
	store, err := timescale.OpenBackground(rootCtx, cfg.Storage.PostgresDSN, cfg.Storage.BackgroundStatementTimeout)
	if err != nil {
		cancel() // nothing else registered yet; release the signal ctx
		return fmt.Errorf("storage: %w", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Warn("storage close", "err", err)
		}
	}()
	logger.Info("storage connected")

	// One aggregator per database. Deferred before cancel so the lock is held
	// through the drain; a dry-run takes none so it can run beside the daemon.
	var instanceLock *timescale.InstanceLock
	if !dryRun {
		instanceLock, err = store.HoldInstanceLock(rootCtx, timescale.AggregatorInstanceLockName, cancel, logger.With("component", "instance-lock"))
		if err != nil {
			cancel()
			return instanceLockRefusal(err)
		}
		defer releaseInstanceLock(instanceLock, logger)
	}

	// ─── Redis ───────────────────────────────────────────────────
	// Required: there is nothing to pre-compute without a cache to write to. redisclient.Build
	// picks Sentinel mode (ADR-0024) when redis_sentinel_addrs is set. Dry-run pings explicitly so
	// config drift surfaces at startup rather than at first tick.
	rdb := redisclient.Build(cfg.Storage)
	if rdb == nil {
		cancel() // store.Close deferred above will still run; release the signal ctx
		return errors.New("storage.redis_addr or storage.redis_sentinel_addrs is required — aggregator writes VWAP to Redis")
	}
	defer func() { _ = rdb.Close() }()
	// Register cancel LAST so LIFO runs it FIRST on shutdown: the orchestrator and worker
	// goroutines must unwind BEFORE rdb.Close() / store.Close() pull the resources they query.
	defer cancel()
	mode := redisclient.Mode(cfg.Storage)
	if dryRun {
		pingCtx, cancelPing := context.WithTimeout(rootCtx, 5*time.Second)
		err := rdb.Ping(pingCtx).Err()
		cancelPing()
		if err != nil {
			return fmt.Errorf("redis: ping (%s mode): %w", mode, err)
		}
	}
	logger.Info("redis configured", "mode", mode)

	// ─── Pair + window resolution ────────────────────────────────
	// Operator override via [aggregate].pairs / .windows wins; an
	// empty list falls back to the built-in defaults so a fresh
	// deployment with no aggregator config still runs.
	pairs, err := cfg.Aggregate.AggregatorPairs()
	if err != nil {
		// Validate() already rejected this at startup; reaching here
		// means validation was bypassed. Fail loud rather than silently
		// fall back.
		return fmt.Errorf("aggregator: %w", err)
	}
	if len(pairs) == 0 {
		pairs = defaultPairs()
		logger.Info("aggregator pair set: using built-in default", "count", len(pairs))
	} else {
		logger.Info("aggregator pair set: operator override", "count", len(pairs))
	}

	windows, err := cfg.Aggregate.AggregatorWindows()
	if err != nil {
		return fmt.Errorf("aggregator: %w", err)
	}
	if len(windows) > 0 {
		logger.Info("aggregator windows: operator override", "count", len(windows))
	}

	// ─── Anomaly checker + freeze writer (ADR-0019) ─────────────
	// The Phase 1 checker is wired only when anomaly.enabled is set; nil makes the orchestrator
	// skip Phase 1 evaluate-and-maybe-freeze.
	//
	// The freeze WRITER is deliberately NOT gated on the checker: the Phase 2 confidence lifecycle
	// (stepPhase2Freeze) runs on every scored bucket and REFUSES publication when its 3-signal AND
	// fires. Gated on the checker, a Phase-1-off deployment would engage real freezes that write NO
	// Redis marker, so frozen windows would serve their last value with flags.frozen absent: a stale
	// price presented as fresh. rdb is always non-nil here, so the writer is built unconditionally.
	//
	// ─── Price withholding ([pricing_guard]) ─────────────────────
	// The same pair decision /v1/price serves under, built once and consulted by every
	// customer-facing price this binary pushes (price-alert, anomaly.freeze and divergence.firing
	// webhooks). A scam-flagged issuer or a thin market must not reach a signed delivery the API
	// itself would refuse to publish.
	withholding := pricingguard.Gate{
		Substance: buildAggregatorSubstanceGate(cfg.PricingGuard, store, logger),
		Scam: pricingguard.NewScamGate(store, pricingguard.ScamGateOptions{
			Logger: logger.With("component", "price-withholding"),
		}),
	}

	checker, err := buildAnomalyChecker(cfg.Anomaly)
	if err != nil {
		return fmt.Errorf("anomaly checker: %w", err)
	}
	var freezeWriter orchestrator.FreezeMarker
	var freezeRecovery *freeze.Recovery
	if rdb != nil {
		// Durable mirror of every freeze decision in the freeze_events hypertable, for the
		// /anomalies timeline. Idempotent on the currently-firing row, so a Redis TTL refresh
		// creates no duplicates.
		sinkOpts := []timescale.FreezeEventSinkOption{
			timescale.WithFreezeLedgerProvider(divergenceLedgerAdapter{cursors: store}),
		}
		// Customer-webhook fan-out for `anomaly.freeze`. The aggregator owns the freeze signal and
		// only inserts pending rows into the shared Postgres; the API binary's delivery worker
		// drains them. Skipped silently when the platform v1 schema is absent (dev).
		webhookStore := postgresstore.NewWebhookStore(postgresstore.New(store.DB()))
		fanout := customerwebhook.NewFanout(webhookStore, logger.With("component", "webhook-fanout"))
		if fanout != nil {
			sinkOpts = append(sinkOpts, timescale.WithFreezeHook(anomalyFreezeHook(logger, fanout, withholding)))
			logger.Info("freeze events: customer-webhook fan-out wired")
		}
		sink := timescale.NewFreezeEventSink(store, sinkOpts...)
		opts := []freeze.WriterOption{
			freeze.WithEventSink(sink),
			// Durable ADR-0019 ladder. Without it the extension count and ESCALATED flag live only
			// in process memory and the Redis marker, and the orchestrator reads a missing marker
			// as an operator force-unfreeze, so a Redis flush would release every held pair. Same
			// sink object: the ladder columns hang off the freeze_events row RecordFreeze opens.
			freeze.WithLadderStore(sink, 0), // 0 → freeze.DefaultLadderGrace
		}
		w, err := freeze.NewWriter(rdb, 0, opts...) // 0 → cachekeys.FreezeTTL default
		if err != nil {
			return fmt.Errorf("freeze writer: %w", err)
		}
		freezeWriter = w
		// Recovery worker: closes durable freeze rows once the Redis marker TTL elapses (the
		// orchestrator stops refreshing the marker when the anomaly clears). Without it
		// freeze_events accumulates open rows and /anomalies shows resolved freezes as firing.
		freezeRecovery = freeze.NewRecovery(rdb, sink, sink, freeze.RecoveryOptions{
			Logger: logger,
			// Without the ladder, the sweep is a marker-miss consumer that stamps recovered_at,
			// the exact predicate LoadLadder tests, so it destroys the rehydrate's evidence and
			// records a normal recovery. After a Redis flush its immediate first tick beats the
			// orchestrator's (which computes VWAPs before evaluating freezes), so it wins that race.
			Ladder: sink,
		})
		logger.Info("anomaly + freeze: wired",
			"thresholds", len(cfg.Anomaly.Thresholds),
			"event_sink", "timescale",
			"ladder_store", "timescale",
			"recovery_worker", "wired")
	} else if checker != nil {
		logger.Warn("anomaly enabled but no Redis — freeze markers won't be written; anomaly metric still emits")
	}

	// ─── Triangulation chains ───────────────────────────────────
	triangulations, err := buildTriangulations(cfg.Aggregate)
	if err != nil {
		return fmt.Errorf("triangulations: %w", err)
	}
	compositeReference, err := buildCompositeReference(cfg.Aggregate)
	if err != nil {
		return fmt.Errorf("composite reference: %w", err)
	}
	if compositeReference.Enabled {
		logger.Info("composite reference: enabled",
			"targets", cfg.Aggregate.CompositeReference.Targets,
			"tolerance_bps", cfg.Aggregate.CompositeReference.ToleranceBps,
			"min_leg_sources", cfg.Aggregate.CompositeReference.MinLegSources,
			"fx_max_age_hours", cfg.Aggregate.CompositeReference.FXMaxAgeHours,
			"leg_dispersion_bps", cfg.Aggregate.CompositeReference.LegDispersionBps,
			"release_band_pct", cfg.Aggregate.CompositeReference.ReleaseBandPct,
			"chains", len(triangulations))
	}
	if len(triangulations) > 0 {
		logger.Info("triangulation chains: configured", "count", len(triangulations))
	}

	// ─── Divergence service ────────────────────────────────────
	// Builds the CoinGecko + Chainlink HTTP reference clients plus the on-chain oracle references
	// (read from our own served oracle_updates rows) from `[divergence]` config; Tick refreshes the
	// `div:<asset>` cache once per pair per tick. An empty reference list leaves the producer nil,
	// so the cache stays empty and the API's `flags.divergence_warning` stays false.
	var divRefresher orchestrator.DivergenceRefresher
	divRefs := buildDivergenceReferences(cfg.Divergence, cfg.External.CoinGecko, store, logger)
	if len(divRefs) > 0 {
		// Every (pair, reference) tick is mirrored into divergence_observations for the
		// /divergences page and post-mortems. This webhook half builds its own Fanout (the freeze
		// sink's is local to the freeze block); OnWarningFired fires only on the below-to-above
		// threshold transition, so an elevated divergence does not re-spam subscribers per tick.
		divFanout := customerwebhook.NewFanout(
			postgresstore.NewWebhookStore(postgresstore.New(store.DB())),
			logger.With("component", "webhook-fanout"))
		var divWarningHook divergence.WarningHook
		if divFanout != nil {
			divWarningHook = divergenceFiringHook(logger, divFanout, withholding)
		}

		divSvc, err := divergence.NewService(divergence.ServiceOptions{
			Cache:                rdb,
			References:           divRefs,
			Threshold:            cfg.Divergence.Threshold,
			MinSourcesForWarning: cfg.Divergence.MinSourcesForWarning,
			ObservationSink: timescale.NewDivergenceSink(store,
				timescale.WithDivergenceLedgerProvider(divergenceLedgerAdapter{cursors: store}),
			),
			RefreshInterval: divergenceRefreshCadence(cfg.Aggregate),
			Window:          orchestrator.ShortestWindow(windows),
			PairCount:       len(pairs),
			Logger:          logger.With("component", "divergence"),
			OnWarningFired:  divWarningHook,
		})
		if err != nil {
			return fmt.Errorf("divergence service: %w", err)
		}
		divRefresher = divSvc
		names := make([]string, len(divRefs))
		for i, r := range divRefs {
			names[i] = r.Name()
		}
		logger.Info("divergence refresher wired",
			"reference_count", len(divRefs),
			"references", names)
	}

	// ─── Closed-bucket stream publisher ────────────────────────
	// Fans each successful (pair, window) VWAP cache write out to `/v1/price/stream` subscribers
	// via Redis pub/sub. Always wired (only the channel is configurable; empty means
	// DefaultChannel) because PUBLISH with no subscribers is a Redis no-op.
	streamPub, err := redispub.NewPublisher(rdb, cfg.Storage.RedisClosedBucketChannel)
	if err != nil {
		return fmt.Errorf("redispub.NewPublisher: %w", err)
	}
	logger.Info("closed-bucket stream publisher wired",
		"channel", streamPub.Channel(), "producer_id", streamPub.ProducerID())

	// ─── Decimals-normalization lookup (dex-nonstandard-decimals) ──
	// Mirrors `nonstandard_decimals_assets` so the orchestrator can scale a window's raw VWAP by
	// 10^(base_decimals-quote_decimals) (aggregate.AdjustPrice). One blocking refresh so the first
	// Tick, which runs before the cache's background loop, does not see an empty snapshot.
	//
	// FATAL on failure: a refresh failure keeps the last-good snapshot, but at BOOT there is none,
	// and an empty map says "nothing is flagged" for every offender, publishing windows unnormalized
	// (wrong by 10^(7-decimals)) into prices_1m and the aggregates that outlive the outage. A wrong
	// price is a false statement; refusing to start is recoverable and loud.
	decimalsLookup := newDecimalsCache(store, logger.With("component", "decimals-cache"))
	if err := decimalsLookup.Refresh(rootCtx); err != nil {
		return fmt.Errorf("decimals-cache initial refresh (refusing to publish unnormalized VWAP): %w", err)
	}

	orch := orchestrator.New(store, rdb, orchestrator.Config{
		Pairs:              pairs,
		Windows:            windows, // nil → orchestrator.DefaultWindows
		Interval:           time.Duration(cfg.Aggregate.IntervalSeconds) * time.Second,
		MaxTradesPerWindow: cfg.Aggregate.MaxTradesPerWindow,
		DecimalsLookup:     decimalsLookup,
		Anomaly:            checker,
		FreezeWriter:       freezeWriter,
		Triangulations:     triangulations,
		MaxHops:            cfg.Aggregate.MaxHops,
		MinRouteConfidence: cfg.Aggregate.MinRouteConfidence,
		FXStore:            store, // X2.5: snap fiat-vs-fiat legs to bucket-end FX quote
		CompositeReference: compositeReference,
		Baselines:          baselineLookupAdapter{store: store},
		Phase2Thresholds: orchestrator.Phase2Thresholds{
			ConfidenceMaxFreeze:  cfg.Anomaly.Phase2.ConfidenceMaxFreeze,
			ZScoreMinFreeze:      cfg.Anomaly.Phase2.ZScoreMinFreeze,
			SourceCountMaxFreeze: cfg.Anomaly.Phase2.SourceCountMaxFreeze,
			// Freeze lifecycle knobs (ADR-0019). Zero values fall through to the package defaults in
			// Policy.withDefaults; a SET knob must take effect, since a config key that parses and
			// does nothing is a dormant knob.
			Lifecycle: freeze.Policy{
				InitialHold:               time.Duration(cfg.Anomaly.Phase2.InitialHoldMinutes) * time.Minute,
				UncorroboratedInitialHold: time.Duration(cfg.Anomaly.Phase2.UncorroboratedInitialHoldMinutes) * time.Minute,
				Extension:                 time.Duration(cfg.Anomaly.Phase2.ExtensionMinutes) * time.Minute,
				MaxExtensions:             cfg.Anomaly.Phase2.MaxExtensions,
				UnfreezeConfidenceMin:     cfg.Anomaly.Phase2.UnfreezeConfidenceMin,
				UnfreezeZScoreMax:         cfg.Anomaly.Phase2.UnfreezeZScoreMax,
				UnfreezeBuckets:           cfg.Anomaly.Phase2.UnfreezeBuckets,
			},
		},
		DisableClassFilter:        cfg.Aggregate.DisableClassFilter,
		ExcludedSources:           cfg.Aggregate.ExcludedSources,
		EnableStablecoinFiatProxy: cfg.Aggregate.EnableStablecoinFiatProxy,
		USDPeggedClassicAssets:    cfg.Trades.USDPeggedClassics(logger),
		USDPeggedSorobanAssets:    resolveUSDPeggedSorobanAssets(cfg.Trades.USDPeggedClassicAssets, cfg.Supply.SACWrappers, logger),
		OutlierSigmaThreshold:     cfg.Aggregate.OutlierSigmaThreshold,
		MinUSDVolume:              cfg.Aggregate.MinUSDVolume,
		DivergenceRefresher:       divRefresher,
		DivergenceMinInterval:     time.Duration(cfg.Aggregate.DivergenceMinIntervalSeconds) * time.Second,
		// Same source as the divergence Service's own quorum
		// (wired a few lines above into ServiceOptions.MinSourcesForWarning)
		// and the API adapter's mirror — one config value, three consumers.
		DivergenceMinSources: cfg.Divergence.MinSourcesForWarning,
		StreamPublisher:      streamPub,
		// Per-source contribution mirror — feeds the explorer
		// source-donut on every price card. See migrations/0026.
		ContributionSink: newContributionSink(store),
		Logger:           logger,
	})

	// ─── Fallible startup constructors ───────────────────────────
	// Built before the dry-run early-return so a config that opens every connection above but
	// cannot construct one of these (change-summary, close-time reader, supply / cross-check
	// refreshers) fails -dry-run instead of surfacing only at a real start.
	changeSummaryWorker, err := changesummary.New(
		changeSummaryPriceSource{store: store},
		changeSummarySink{store: store},
		buildChangeSummaryEntities(pairs),
		logger.With("component", "change-summary"),
		changesummary.Options{Interval: 5 * time.Minute},
	)
	if err != nil {
		return fmt.Errorf("change-summary worker init: %w", err)
	}

	var supplyRefresherBindings []supplyRefresherBinding
	var ccRefresher *supply.CrossCheckRefresher
	if cfg.Supply.AggregatorRefreshEnabled {
		// Each refresh stamps the snapshot's ObservedAt with the ledger's REAL close_time from the
		// ClickHouse lake, never wall-clock write time. Without an authoritative close-time source
		// a re-derive would corrupt point-in-time supply queries, so fail closed rather than fall
		// back to time.Now().
		if cfg.Storage.ClickHouseAddr == "" {
			return errors.New("supply aggregator refresh requires storage.clickhouse_addr (source of the ledger close time stamped as snapshot ObservedAt) — set it or disable [supply] aggregator_refresh_enabled")
		}
		var closeTimes ledgerCloseTimeReader
		if dryRun {
			// -dry-run validates connectivity synchronously so a bad
			// config fails -dry-run instead of only surfacing at a real
			// start. The process exits right after this branch either way,
			// so a single dial attempt (no retry) is correct here.
			supplyCloseTimes, err := clickhouse.NewExplorerReader(rootCtx, cfg.Storage.ClickHouseAddr)
			if err != nil {
				return fmt.Errorf("supply refresher close-time reader: %w", err)
			}
			defer func() { _ = supplyCloseTimes.Close() }()
			closeTimes = supplyCloseTimes
		} else {
			// Dial in the background: inline, one ClickHouse blip would abort the aggregator's boot
			// (the cold-boot race dialDecimalsResolver also retries, clickhouse-server loading
			// metadata for minutes after a reboot). Only the supply-refresh tick waits for it.
			lazy := newLazyCloseTimeReader(rootCtx, cfg.Storage.ClickHouseAddr, logger.With("component", "supply-refresh"), clickhouse.NewExplorerReader)
			defer lazy.Close()
			closeTimes = lazy
		}

		supplyRefresherBindings, err = buildSupplyRefreshers(cfg, store, closeTimes, logger.With("component", "supply-refresh"))
		if err != nil {
			return fmt.Errorf("supply refresher init: %w", err)
		}

		// The cross-check refresher reads the per-asset snapshots above and emits the
		// supply_cross_check_divergence_stroops gauge (ADR-0011). No-op when no classic <-> SAC
		// pairs are declared in [supply].sac_wrappers and the watched sets.
		ccRefresher, err = buildCrossCheckRefresher(cfg, store, logger.With("component", "supply-cross-check"))
		if err != nil {
			return fmt.Errorf("supply cross-check refresher init: %w", err)
		}
	}

	if dryRun {
		logger.Info("dry-run complete — exiting")
		return nil
	}

	// ─── Metrics HTTP endpoint ──────────────────────────────────
	// Aggregator counters register into internal/obs at package init and are unreachable without a
	// listener; Prometheus must scrape them for the aggregator-silent / outlier-storm /
	// class-drop-spike alerts in deploy/monitoring/rules/aggregator.yml to fire.
	metricsSrv := startMetricsServer(cfg.Obs, store, logger)

	// ─── Baseline refresh worker (ADR-0019 Phase 2) ─────────────
	// Hourly: pulls each pair's 30-day VWAP window from prices_1m, computes Median + MAD
	// (internal/aggregate/baseline) and UPSERTs volatility_baseline_1m. Runs in its own goroutine so
	// a slow refresh never holds up orch.Run's tick.
	refresher := baseline.NewRefresher(
		baselineSourceAdapter{store: store},
		baselineSinkAdapter{store: store},
		baseline.DefaultWindow,
		logger.With("component", "baseline-refresh"),
	).WithMinuteNotionalFloor(baseline.MinuteNotionalFloor(
		cfg.Aggregate.MinUSDVolume, orchestrator.LongestWindow(windows)))
	var refresherWG sync.WaitGroup
	refresherWG.Add(1)
	go func() {
		defer worker.Recover(logger, "baseline-refresh")
		defer refresherWG.Done()
		runBaselineRefresh(rootCtx, refresher, pairs, logger.With("component", "baseline-refresh"))
	}()

	// ─── Change-summary rollup worker ───────────────────────────
	// Refreshes the change_summary_5m table every 5 min so every
	// list view + delta strip on the explorer reads in O(1) rather
	// than re-scanning prices_1m per request. See migrations/0022.
	// Constructed above (before the dry-run check); started here.
	refresherWG.Add(1)
	go func() {
		defer worker.Recover(logger, "change-summary")
		defer refresherWG.Done()
		// Log the exit reason: a change-summary worker that gave up on a non-retryable error would
		// leave the explorer's delta strips frozen with nothing in the journal saying why, and the
		// rollup package has no metrics of its own.
		if err := changeSummaryWorker.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("change-summary worker exited with error", "err", err)
		}
	}()

	// ─── Protocol-events rollup worker ────────────────────
	// Folds the trailing-24h per-source event census into protocol_events_24h every couple of
	// minutes, so /v1/protocols' events_24h is a keyed-on-PK lookup instead of a slow cold
	// ~17-table UNION count. Always on (backs a core API read).
	protoEventsRollup := protoeventsrollup.New(store, protoeventsrollup.Options{
		Interval: protoeventsrollup.DefaultInterval,
		Logger:   logger.With("component", "protocol-events-rollup"),
	})
	refresherWG.Add(1)
	go func() {
		defer worker.Recover(logger, "protocol-events-rollup")
		defer refresherWG.Done()
		if err := protoEventsRollup.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("protocol-events rollup worker exited with error", "err", err)
		}
	}()

	// ─── Asset-volume rollup worker ───────────────────────
	// Folds the trailing-24h per-asset USD-volume SUM over prices_1m into asset_volume_24h every
	// couple of minutes, so /v1/assets is a keyed-on-PK lookup instead of a per-request scan of
	// ~256k rows. Always on (backs a core API read).
	assetVolRollup := assetvolrollup.New(store, assetvolrollup.Options{
		Interval: assetvolrollup.DefaultInterval,
		Logger:   logger.With("component", "asset-volume-rollup"),
	})
	refresherWG.Add(1)
	go func() {
		defer worker.Recover(logger, "asset-volume-rollup")
		defer refresherWG.Done()
		if err := assetVolRollup.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("asset-volume rollup worker exited with error", "err", err)
		}
	}()

	// ─── Asset volume-character rollup worker (§2) ──────────────
	// Folds the trailing-window account-structure roll over `trades` (each trade counted on BOTH
	// sides, folded onto canonical assets) into asset_volume_character, so /v1/assets{,/{id}} read
	// a keyed-on-PK lookup instead of a ~4s per-request roll that tripped the request timeout and
	// returned null on busy assets. Always on. Runs after the alias registry is installed, so its
	// canonical fold matches the per-asset read.
	assetCharRollup := assetcharacterrollup.New(store, assetcharacterrollup.Options{
		Interval:     assetcharacterrollup.DefaultInterval,
		StartupDelay: assetcharacterrollup.DefaultStartupDelay,
		Logger:       logger.With("component", "asset-character-rollup"),
	})
	refresherWG.Add(1)
	go func() {
		defer worker.Recover(logger, "asset-character-rollup")
		defer refresherWG.Done()
		if err := assetCharRollup.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("asset-character rollup worker exited with error", "err", err)
		}
	}()

	// ─── Supply-snapshot refresh worker (ADR-0011) ────
	// Opt-in via cfg.Supply.AggregatorRefreshEnabled. When false (the default) the systemd timer
	// (deploy/systemd/supply-snapshot.timer) drives refresh; when true this goroutine path takes
	// over and the timer should be disabled.
	if cfg.Supply.AggregatorRefreshEnabled {
		// SEP-41 supply rollup worker. Advances the per-contract mint/burn/clawback checkpoint the
		// SEP41KindTotalsAtOrBefore fast path reads, so refreshers never re-sum a contract's full
		// sep41_supply_events history each tick. Started first so its immediate fold warms the
		// checkpoint. No-op when no SEP-41 contract is watched.
		if len(cfg.Supply.WatchedSEP41Contracts) > 0 {
			refresherWG.Add(1)
			go func() {
				defer worker.Recover(logger, "sep41-supply-rollup")
				defer refresherWG.Done()
				runSEP41SupplyRollup(rootCtx, store, cfg.Supply.WatchedSEP41Contracts,
					cfg.Supply.AggregatorRefreshCadence, logger.With("component", "sep41-supply-rollup"))
			}()
		}

		// The close-time reader, supply refreshers and cross-check
		// refresher are constructed above (before the dry-run check);
		// started here.
		for _, b := range supplyRefresherBindings {
			refresherWG.Add(1)
			go func(binding supplyRefresherBinding) {
				defer worker.Recover(logger, "supply-refresh")
				defer refresherWG.Done()
				runSupplyRefresh(rootCtx, binding.refresher, cfg.Supply.AggregatorRefreshCadence, binding.assetKey)
			}(b)
		}

		if ccRefresher != nil {
			refresherWG.Add(1)
			go func() {
				defer worker.Recover(logger, "supply-cross-check")
				defer refresherWG.Done()
				runCrossCheckRefresh(rootCtx, ccRefresher, cfg.Supply.AggregatorRefreshCadence)
			}()
		}
	}

	// ─── Supply-divergence cross-check worker ───────────────────
	// Compares OUR served circulating_supply against an external reference (Stellar Network
	// Dashboard for XLM; CoinGecko when a Pro key is set) and emits
	// stellarindex_supply_divergence_ratio + _total{outcome}, catching e.g. a stale SDF-reserve
	// exclusion list. A dark reference degrades to `no_reference`, not a false alarm. Gated on
	// [divergence.supply].enabled; it reads snapshots from either refresh path, so it lives OUTSIDE
	// the aggregator_refresh_enabled gate.
	supplyDivSvc, err := buildSupplyDivergenceService(cfg.Divergence.Supply, store,
		logger.With("component", "supply-divergence"))
	if err != nil {
		return fmt.Errorf("supply-divergence service init: %w", err)
	}
	if supplyDivSvc != nil {
		interval := supplyDivergenceRefreshInterval
		refresherWG.Add(1)
		go func() {
			defer worker.Recover(logger, "supply-divergence")
			defer refresherWG.Done()
			runSupplyDivergenceRefresh(rootCtx, supplyDivSvc, interval)
		}()
	}

	// ─── Freeze-recovery worker ────────────────────────
	// Closes durable freeze rows once the Redis marker TTL elapses;
	// without it, the freeze_events table accumulates open rows
	// forever even after the anomaly clears. Wired only when the
	// freeze writer itself is wired (anomaly + Redis both present).
	if freezeRecovery != nil {
		refresherWG.Add(1)
		go func() {
			defer worker.Recover(logger, "freeze-recovery")
			defer refresherWG.Done()
			_ = freezeRecovery.Run(rootCtx)
		}()
	}

	// Data-derived gap detector: every 5 min scans soroban_events for ledger-coverage gaps of
	// >= 1000 ledgers and emits the gauges behind `stellarindex_ingest_gap_ledgers_significant`, so
	// a halted Soroban-events writer pages instead of needing manual SQL on the cursors.
	refresherWG.Add(1)
	go func() {
		defer worker.Recover(logger, "gap-detector")
		defer refresherWG.Done()
		// Network-scoped: every target Genesis is a PUBNET
		// contract-deploy ledger, so on a test net the pubnet-only
		// targets scan [genesis, tip] with genesis ABOVE tip and error
		// out every cycle forever. sourcenet decides which exist here.
		if err := timescale.RunGapDetector(rootCtx, store, logger.With("component", "gap-detector"),
			cfg.Stellar.Network); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("gap-detector exited with error", "err", err)
		}
	}()

	// ─── MEV detection worker (§7.20) ───────────────────────────
	// Every 5 min scans the recent served-row window for MEV patterns: arbitrage and wash trading
	// from trades, liquidation cascades from blend_auctions x oracle_updates, and (when the lake is
	// reachable) sandwich / oracle-sandwich, which need the intra-ledger tx order only the
	// ClickHouse lake carries (stellar.tx_hash_index). Idempotent via the dedup key, so
	// overlapping windows are safe.
	mevCfg := mev.WorkerConfig{
		Logger:   logger.With("component", "mev"),
		Observer: mevObserver{},
		Oracles:  store,
		Auctions: store,
	}
	mevWorker := mev.NewWorker(store, store, mevCfg)
	if addr := cfg.Storage.ClickHouseAddr; addr != "" {
		// A one-shot inline dial failure would disable the sandwich detectors for the process
		// lifetime (the cold-boot race dialDecimalsResolver also handles). Retry in the background
		// and arm them when the lake answers; the rest of the worker starts immediately.
		refresherWG.Add(1)
		go func() {
			defer worker.Recover(logger, "mev-order-dial")
			defer refresherWG.Done()
			txr, ok := dialLakeReaderWithRetry(rootCtx, logger, addr, clickhouse.NewTxIndexReader,
				"mev: ClickHouse tx-order resolver unavailable — sandwich/oracle-sandwich detection NOT YET armed, retrying",
				"mev: ClickHouse tx-order resolver reached — sandwich/oracle-sandwich detection armed")
			if !ok {
				return // shutdown before the lake ever answered
			}
			defer func() { _ = txr.Close() }()
			mevWorker.SetOrder(txr)
			<-rootCtx.Done()
		}()
	}
	refresherWG.Add(1)
	go func() {
		defer worker.Recover(logger, "mev")
		defer refresherWG.Done()
		if err := mevWorker.Run(rootCtx, 5*time.Minute); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("mev worker exited with error", "err", err)
		}
	}()

	// ─── Decimals-normalization cache background refresh ───────────
	// Keeps decimalsLookup current with `nonstandard_decimals_assets`, so a new offender's
	// normalization phases in, and a removed row stops applying, within one interval. Independent
	// of the decimals-guard detection sweep below.
	refresherWG.Add(1)
	go func() {
		defer worker.Recover(logger, "decimals-cache")
		defer refresherWG.Done()
		decimalsLookup.run(rootCtx)
	}()

	// ─── Decimals-assumption guard ─
	// The served price is Sum(quote)/Sum(base) on raw smallest-unit integers, so decimals cancel
	// only when base and quote share a scale; a non-7-decimal SEP-41 token with DEX liquidity skews
	// its pairs by 10^(7-decimals). The sweep resolves each recently traded Soroban token's
	// decimals() from the lake and raises stellarindex_dex_trade_nonstandard_decimals_total when
	// one is != 7. It only detects; Writer persists into nonstandard_decimals_assets.
	//
	// The lake dial retries with backoff in the guard's goroutine, never one inline dial: after a
	// reboot clickhouse-server spends minutes loading metadata and this unit's After= does not name
	// it, so one failed dial would leave the aggregator unguarded until restart.
	//
	// MarkEnabled seeds the sweep heartbeat before the dial, so decimals_guard_sweep_stale measures
	// a cold boot from now and a lake-less aggregator reads "disabled", not "stale".
	if addr := cfg.Storage.ClickHouseAddr; addr != "" {
		decimalsguard.MarkEnabled(time.Now())
		refresherWG.Add(1)
		go func() {
			defer worker.Recover(logger, "decimals-guard")
			defer refresherWG.Done()
			er, ok := dialDecimalsResolver(rootCtx, logger, addr, clickhouse.NewExplorerReader)
			if !ok {
				// Shutdown before the lake ever answered.
				return
			}
			defer func() { _ = er.Close() }()
			guard := decimalsguard.New(store, er, decimalsguard.Options{
				Window: decimalsguard.DefaultWindow,
				Logger: logger.With("component", "decimals-guard"),
				// Persists each confirmed offender into
				// nonstandard_decimals_assets (migration 0093) so the
				// API's read-time serving guard can decline pricing —
				// turns detection into an actual stop-serving lever.
				Writer: store,
			})
			// One-time startup self-seed: catches a non-7-decimal token that traded and went
			// DORMANT before the periodic sweep's short (20m) window saw it. A failure is
			// logged and does not block the periodic sweep.
			if err := guard.Backfill(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("decimals-guard: startup backfill failed — a dormant nonstandard-decimals token may remain unseeded until the operator hand-seeds it per the runbook", "err", err)
			}
			if err := guard.Run(rootCtx, decimalsguard.DefaultInterval); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("decimals-guard exited with error", "err", err)
			}
		}()
	} else {
		logger.Warn("decimals-guard: disabled — no ClickHouse lake configured (storage.clickhouse_addr); non-7-decimal DEX-token detection is OFF for this process")
	}

	// ─── Price-alert evaluator ─────────────────────────────────
	// Off by default. When [price_alerts] enabled=true, each tick sweeps the enabled alert rows
	// against the latest closed 1m VWAP and enqueues ACCOUNT-scoped `price.alert` deliveries (via
	// ListWebhooksForAccount, not the global fan-out). Lives here because this binary writes
	// prices_1m; delivery is the separate internal/customerwebhook worker in the API binary.
	if cfg.PriceAlerts.Enabled {
		paStore := postgresstore.New(store.DB())
		paWorker := pricealerts.New(
			postgresstore.NewPriceAlertStore(paStore),
			postgresstore.NewWebhookStore(paStore),
			priceAlertVWAPReader{
				store:  store,
				logger: logger.With("component", "price-alert-guard"),
				// The binary's shared withholding gate: an alert must not
				// fire off a pair the API itself would refuse to price.
				gate: withholding,
				// Same decimals correction /v1/price applies before serving
				// — without it a nonstandard-decimals asset's threshold
				// (set from the corrected price) is compared against a raw
				// ratio 10^k off.
				decimals: decimalsLookup,
			},
			pricealerts.Options{
				Logger: logger.With("component", "price-alerts"),
			},
		)
		refresherWG.Add(1)
		go func() {
			defer worker.Recover(logger, "price-alerts")
			defer refresherWG.Done()
			if err := paWorker.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("price-alert evaluator exited with error", "err", err)
			}
		}()
		logger.Info("price-alert evaluator: wired", "interval", pricealerts.DefaultInterval)
	}

	// ─── Priceless-popular coverage tripwire ────
	// Sweeps for assets popular by MARKET-CHARACTER volume (single-account-pair wash excluded) that
	// have NO served price and NO recorded withheld verdict, an unexplained coverage gap, and
	// publishes stellarindex_assets_popular_priceless so it pages. Always on: a coverage
	// regression must not depend on an opt-in flag.
	pricelessOpts := pricelesscoverage.Options{
		Interval: pricelesscoverage.DefaultInterval,
		Logger:   logger.With("component", "priceless-coverage"),
		IsPriced: store.AssetIsPriced,
		// "Withheld" is the serving listing's own substance verdict, from
		// the same [pricing_guard] policy and USD pegs the API applies.
		Withheld: pricelesscoverage.SubstanceWithheld(
			buildAggregatorSubstanceGate(cfg.PricingGuard, store, logger),
			cfg.Trades.USDPeggedClassics(logger)),
	}
	pricelessTripwire := pricelesscoverage.New(store, pricelessOpts)
	// A Soroban trade is keyed by the token contract but a SAC's price is served under its classic
	// asset; resolve one to the other from the lake so a wrapped classic asset is not ticketed as
	// priceless. One failed inline dial would leave SAC candidates unresolved for the process
	// lifetime (the cold-boot race dialDecimalsResolver also handles), so retry in the background
	// and arm alias resolution when the lake answers; the sweep itself starts immediately.
	if addr := cfg.Storage.ClickHouseAddr; addr != "" {
		refresherWG.Add(1)
		go func() {
			defer worker.Recover(logger, "priceless-coverage-sac-dial")
			defer refresherWG.Done()
			sacReader, ok := dialLakeReaderWithRetry(rootCtx, logger, addr, clickhouse.NewExplorerReader,
				"priceless-coverage: ClickHouse SAC resolver unavailable — SAC candidates read as given, retrying",
				"priceless-coverage: ClickHouse SAC resolver reached — SAC alias resolution armed")
			if !ok {
				return // shutdown before the lake ever answered
			}
			defer func() { _ = sacReader.Close() }()
			pricelessTripwire.SetResolveSAC(func(ctx context.Context, contractID string) (string, bool) {
				name, ok, err := sacReader.SACClassicAssetName(ctx, contractID)
				if err != nil || !ok {
					return "", false
				}
				return canonicalSACName(name)
			})
			<-rootCtx.Done()
		}()
	}
	refresherWG.Add(1)
	go func() {
		defer worker.Recover(logger, "priceless-coverage")
		defer refresherWG.Done()
		if err := pricelessTripwire.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("priceless-popular coverage tripwire exited with error", "err", err)
		}
	}()
	logger.Info("priceless-popular coverage tripwire: wired", "interval", pricelesscoverage.DefaultInterval)

	// ─── Run ─────────────────────────────────────────────────────
	logger.Info("orchestrator starting")
	if err := orch.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("orchestrator: %w", err)
	}
	logger.Info("orchestrator stopped", "stats", orch.Stats())

	if metricsSrv != nil {
		shutdownCtx, stopShutdown := context.WithTimeout(context.Background(), 10*time.Second)
		if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("metrics server shutdown", "err", err)
		}
		stopShutdown()
	}

	// Wait for the baseline + supply goroutines to honour rootCtx
	// cancellation.
	refresherWG.Wait()
	return instanceLock.Lost()
}

// instanceLockRefusal names the other aggregator as the reason not to start.
func instanceLockRefusal(err error) error {
	if errors.Is(err, timescale.ErrInstanceLockHeld) {
		return fmt.Errorf("refusing to start: another stellarindex-aggregator is running against this database: %w", err)
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

// supplyRefresherBinding pairs a [supply.Refresher] with the asset_key labelling its outcome
// metrics, so stellarindex_aggregator_supply_refresh_total shows which watched asset is failing.
type supplyRefresherBinding struct {
	refresher *supply.Refresher
	assetKey  string
}

// buildSupplyRefreshers composes one [supply.Refresher] per watched asset: native XLM
// (Algorithm 1), each watched_classic_assets entry (Algorithm 2) and each watched_sep41_contracts
// entry (Algorithm 3). It errors on operator-config inconsistencies.
func buildSupplyRefreshers(cfg config.Config, store *timescale.Store, closeTimes ledgerCloseTimeReader, logger *slog.Logger) ([]supplyRefresherBinding, error) {
	out := make([]supplyRefresherBinding, 0, 1+len(cfg.Supply.WatchedClassicAssets)+len(cfg.Supply.WatchedSEP41Contracts))

	xlmRefresher, xlmAssetKey, err := buildXLMRefresher(cfg, store, closeTimes, logger)
	if err != nil {
		return nil, fmt.Errorf("xlm refresher: %w", err)
	}
	out = append(out, supplyRefresherBinding{refresher: xlmRefresher, assetKey: xlmAssetKey})

	classicBindings, err := buildClassicRefreshers(cfg, store, closeTimes, logger)
	if err != nil {
		return nil, err
	}
	out = append(out, classicBindings...)

	sep41Bindings, err := buildSEP41Refreshers(cfg, store, closeTimes, logger)
	if err != nil {
		return nil, err
	}
	out = append(out, sep41Bindings...)

	return out, nil
}

// buildSupplyPolicy resolves the per-asset locked-set / max-supply overrides into a
// [supply.Policy] for the classic and SEP-41 computers, and validates it so a malformed override
// fails startup rather than mid-snapshot.
//
// Policy.SDFReserveAccounts is left unset: only native XLM reads it, and buildXLMRefresher wires
// it. Both maps are re-keyed onto supply.AssetKey form because the computers match keys exactly;
// the dash spelling watched_classic_assets uses would load cleanly and never apply.
func buildSupplyPolicy(cfg config.SupplyConfig) (supply.Policy, error) {
	maxSupply, err := supply.CanonicalizePolicyKeys(cfg.MaxSupplyOverrides)
	if err != nil {
		return supply.Policy{}, fmt.Errorf("supply policy: max_supply_overrides: %w", err)
	}
	lockedSets, err := supply.CanonicalizePolicyKeys(cfg.PerAssetLockedSets)
	if err != nil {
		return supply.Policy{}, fmt.Errorf("supply policy: per_asset_locked_sets: %w", err)
	}
	policy := supply.Policy{
		MaxSupplyOverrides: maxSupply,
	}
	if len(lockedSets) > 0 {
		policy.PerAsset = make(map[string]supply.LockedSet, len(lockedSets))
		for assetKey, ls := range lockedSets {
			policy.PerAsset[assetKey] = supply.LockedSet{
				Accounts:  ls.Accounts,
				Contracts: ls.Contracts,
			}
		}
	}
	if err := policy.Validate(); err != nil {
		return supply.Policy{}, fmt.Errorf("supply policy: %w", err)
	}
	return policy, nil
}

func buildClassicRefreshers(cfg config.Config, store *timescale.Store, closeTimes ledgerCloseTimeReader, logger *slog.Logger) ([]supplyRefresherBinding, error) {
	if len(cfg.Supply.WatchedClassicAssets) == 0 {
		return nil, nil
	}
	policy, err := buildSupplyPolicy(cfg.Supply)
	if err != nil {
		return nil, err
	}
	classicReader := supply.NewStorageClassicSupplyReader(store, supply.ClassicSupplyReaderOptions{Logger: logger})
	classicComputer, err := supply.NewClassicComputer(policy, classicReader)
	if err != nil {
		return nil, fmt.Errorf("classic computer: %w", err)
	}
	out := make([]supplyRefresherBinding, 0, len(cfg.Supply.WatchedClassicAssets))
	for _, raw := range cfg.Supply.WatchedClassicAssets {
		asset, err := canonical.ParseAsset(raw)
		if err != nil {
			return nil, fmt.Errorf("parse watched classic asset %q: %w", raw, err)
		}
		bound, err := supply.NewAssetBoundClassicComputer(classicComputer, asset)
		if err != nil {
			return nil, fmt.Errorf("bind classic computer to %q: %w", raw, err)
		}
		assetKey, err := supply.AssetKey(asset)
		if err != nil {
			return nil, fmt.Errorf("derive asset_key for %q: %w", raw, err)
		}
		opts, err := supplyRefresherOptions(cfg, assetKey)
		if err != nil {
			return nil, err
		}
		out = append(out, supplyRefresherBinding{
			refresher: supply.NewRefresher(
				supplyAggregatorLedgers{s: store, closeTimes: closeTimes},
				bound,
				supplyAggregatorInserter{s: store},
				logger.With("asset", raw),
				opts...,
			),
			assetKey: assetKey,
		})
	}
	return out, nil
}

// supplyRefresherOptions builds the [supply] freshness options for one
// refresher; assetKey must be [supply.AssetKey] form, the key the gate
// reads off each snapshot, or a per-asset override never applies.
func supplyRefresherOptions(cfg config.Config, assetKey string) ([]supply.RefresherOption, error) {
	opts := []supply.RefresherOption{
		supply.WithStrictFreshnessRequired(cfg.Supply.StrictFreshnessRequired),
		supply.WithStaleComponentLedgers(cfg.Supply.StaleComponentLedgers),
		supply.WithMaxDormantComponentLedgers(cfg.Supply.MaxDormantComponentLedgers),
	}
	byAsset, err := supply.CanonicalizeStaleComponentLedgers(cfg.Supply.StaleComponentLedgersByAsset)
	if err != nil {
		return nil, fmt.Errorf("stale_component_ledgers_by_asset: %w", err)
	}
	if maxLag, ok := byAsset[assetKey]; ok {
		opts = append(opts, supply.WithStaleComponentLedgersFor(assetKey, maxLag))
	}
	return opts, nil
}

func buildSEP41Refreshers(cfg config.Config, store *timescale.Store, closeTimes ledgerCloseTimeReader, logger *slog.Logger) ([]supplyRefresherBinding, error) {
	if len(cfg.Supply.WatchedSEP41Contracts) == 0 {
		return nil, nil
	}
	policy, err := buildSupplyPolicy(cfg.Supply)
	if err != nil {
		return nil, err
	}
	sacClassicKeys, err := supply.SACClassicKeys(cfg.Supply.SACWrappers)
	if err != nil {
		return nil, err
	}
	sep41Reader := supply.NewStorageSEP41SupplyReader(supplyAggregatorSEP41Store{s: store}, sacClassicKeys)
	sep41Computer, err := supply.NewSEP41Computer(policy, sep41Reader)
	if err != nil {
		return nil, fmt.Errorf("sep41 computer: %w", err)
	}
	seeder := newGenesisAutoSeeder(cfg, store)
	out := make([]supplyRefresherBinding, 0, len(cfg.Supply.WatchedSEP41Contracts))
	for _, contractID := range cfg.Supply.WatchedSEP41Contracts {
		asset, err := canonical.NewSorobanAsset(contractID)
		if err != nil {
			return nil, fmt.Errorf("watched sep41 contract %q: %w", contractID, err)
		}
		bound, err := supply.NewAssetBoundSEP41Computer(sep41Computer, asset)
		if err != nil {
			return nil, fmt.Errorf("bind sep41 computer to %q: %w", contractID, err)
		}
		opts, err := supplyRefresherOptions(cfg, contractID)
		if err != nil {
			return nil, err
		}
		out = append(out, supplyRefresherBinding{
			refresher: supply.NewRefresher(
				supplyAggregatorLedgers{s: store, closeTimes: closeTimes},
				supply.NewGenesisSeedingComputer(bound, contractID, supply.GenesisSeedingOptions{Seed: seeder, RetryAfter: genesisAutoSeedRetry, Logger: logger.With("asset", contractID)}),
				supplyAggregatorInserter{s: store},
				logger.With("asset", contractID),
				opts...,
			),
			assetKey: contractID, // supply.AssetKey form for SEP-41 is the bare contract id
		})
	}
	return out, nil
}

func buildXLMRefresher(cfg config.Config, store *timescale.Store, closeTimes ledgerCloseTimeReader, logger *slog.Logger) (*supply.Refresher, string, error) {
	staticReader, err := cfg.Supply.NewStaticReserveReader()
	if err != nil {
		return nil, "", fmt.Errorf("config reserve reader: %w", err)
	}
	chained := supply.NewChainedReserveBalanceReader(
		supply.NewLCMReserveBalanceReader(supplyAggregatorStoreLookup{s: store}),
		staticReader,
	)
	// Network-aware: a testnet / futurenet aggregator must snapshot
	// against ITS ledger's native total (100 B genesis), not the
	// frozen pubnet 50.0 B constant.
	computer, err := supply.NewXLMComputerForNetwork(cfg.Stellar.Passphrase(), cfg.Supply.SDFReserveAccounts, chained)
	if err != nil {
		return nil, "", fmt.Errorf("xlm computer: %w", err)
	}
	// assetKey must be supply.AssetKey's "XLM" form (what StaleComponentLedgersByAsset overrides
	// and snapshot lookups key on), not the "native" AssetType literal. It is returned so the
	// metric label and the internal registration cannot drift apart.
	assetKey, err := supply.AssetKey(canonical.NativeAsset())
	if err != nil {
		return nil, "", fmt.Errorf("derive asset_key for native XLM: %w", err)
	}
	opts, err := supplyRefresherOptions(cfg, assetKey)
	if err != nil {
		return nil, "", err
	}
	return supply.NewRefresher(
		supplyAggregatorLedgers{s: store, closeTimes: closeTimes},
		computer,
		supplyAggregatorInserter{s: store},
		logger.With("asset", assetKey),
		opts...,
	), assetKey, nil
}

// runSupplyRefresh ticks the supply refresher on `cadence`, counting per-(asset_key, outcome).
// The first cycle runs at startup so a fresh deployment has a snapshot before one interval elapses.
// sep41RollupAdvancer is the narrow seam runSEP41SupplyRollup drives: *timescale.Store in
// production, a fake in the worker test.
type sep41RollupAdvancer interface {
	AdvanceSEP41SupplyRollup(ctx context.Context, contractID string) (timescale.SEP41RollupAdvance, error)
}

// sep41RollupOutcomeNoCursor labels a pass that folded nothing because the
// projector's sep41_supply cursor row is absent: the fold stays pinned and the
// supply reader pays the full-history scan, so it must not read as "noop".
const sep41RollupOutcomeNoCursor = "no_cursor"

// sep41RollupOutcome maps one advance to its
// stellarindex_sep41_supply_rollup_advances_total outcome label.
func sep41RollupOutcome(res timescale.SEP41RollupAdvance, err error) string {
	switch {
	case err != nil:
		return "error"
	case res.CursorAbsent:
		return sep41RollupOutcomeNoCursor
	case !res.Advanced:
		return "noop"
	}
	return "ok"
}

// runSEP41SupplyRollup periodically folds each watched SEP-41 contract's newly-settled
// mint/burn/clawback events into its rollup checkpoint, so the reader adds a bounded live delta
// instead of re-summing the whole `sep41_supply_events` history each refresh.
//
// Contracts advance SEQUENTIALLY: a cold contract's first fold is a one-off full-history sum, and
// fanning those out would run concurrent full-table scans that saturate Postgres and blow up API
// p95/p99.
func runSEP41SupplyRollup(ctx context.Context, advancer sep41RollupAdvancer, contracts []string, cadence time.Duration, logger *slog.Logger) {
	// Contracts currently pinned by an absent projector cursor; WARN once on
	// entering that state, not every cadence.
	noCursor := make(map[string]bool, len(contracts))
	advanceAll := func() {
		for _, c := range contracts {
			if ctx.Err() != nil {
				return
			}
			start := time.Now()
			res, err := advancer.AdvanceSEP41SupplyRollup(ctx, c)
			outcome := sep41RollupOutcome(res, err)
			obs.SEP41SupplyRollupAdvancesTotal.WithLabelValues(c, outcome).Inc()
			obs.SEP41SupplyRollupAdvanceDurationSeconds.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
			switch {
			case err != nil && ctx.Err() == nil:
				logger.Warn("sep41 supply rollup advance failed", "contract_id", c, "err", err)
			case outcome == sep41RollupOutcomeNoCursor && !noCursor[c]:
				logger.Warn("sep41 supply rollup pinned: projector cursor for sep41_supply is absent; supply reads use the full-history scan until it appears",
					"contract_id", c, "checkpoint_ledger", res.ToLedger)
			case res.Advanced:
				logger.Debug("sep41 supply rollup advanced",
					"contract_id", c, "from_ledger", res.FromLedger, "to_ledger", res.ToLedger)
			}
			if err == nil {
				noCursor[c] = outcome == sep41RollupOutcomeNoCursor
			}
		}
	}

	advanceAll() // immediate first fold — warms the checkpoint before the refresher reads it

	ticker := time.NewTicker(cadence)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			advanceAll()
		}
	}
}

func runSupplyRefresh(ctx context.Context, r *supply.Refresher, cadence time.Duration, assetKey string) {
	tick := func() {
		// Times the full Tick (ledger lookup, freshness reads, snapshot insert). The histogram is
		// labelled by outcome only, not asset_key, to bound cardinality when many assets are watched.
		start := time.Now()
		out := r.Tick(ctx)
		obs.AggregatorSupplyRefreshTotal.WithLabelValues(assetKey, string(out.Kind)).Inc()
		obs.AggregatorSupplyRefreshDurationSeconds.WithLabelValues(string(out.Kind)).Observe(time.Since(start).Seconds())
		if out.BandBreach != "" {
			obs.SupplyWriteBandBreachTotal.WithLabelValues(assetKey, out.BandBreach).Inc()
		}
	}

	// Seed both directions so increase() reads zero, not absent, before a breach.
	for _, direction := range []string{"up", "down"} {
		obs.SupplyWriteBandBreachTotal.WithLabelValues(assetKey, direction)
	}
	tick() // immediate first refresh

	ticker := time.NewTicker(cadence)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}

// ledgerCloseTimeReader resolves the lake's landed tip at or before a requested ledger and its
// real on-chain close time (ClickHouse stellar.ledgers); an interface so wiring is testable
// without a live lake. found=false means no row in the bounded window (the ledger and the
// [maxSupplyLakeClampLedgers] below it), which covers every position the resolver would accept.
type ledgerCloseTimeReader interface {
	LatestLedgerAtOrBefore(ctx context.Context, maxSeq uint32) (uint32, time.Time, bool, error)
}

// supplyCursorLister is the ingestion-cursor half of [supplyAggregatorLedgers], an interface so
// ledger resolution is unit-testable without Postgres. *timescale.Store satisfies it.
type supplyCursorLister interface {
	ListCursors(ctx context.Context) ([]timescale.Cursor, error)
}

// supplyChainCursorSource is the ingestion_cursors row recording how far the LIVE indexer has
// walked the chain (the indexer's `cursorSource`). It alone means "the chain position the supply
// components were derived from"; every other row describes an ops job's own progress.
const supplyChainCursorSource = "ledgerstream"

// maxSupplyLakeClampLedgers bounds how far the snapshot-ledger clamp may walk back from the chain
// cursor to the lake's landed tip. The clamp absorbs the dual-sink landing race (seconds, a
// single-digit number of ledgers); a larger gap is a stalled lake, and stamping a snapshot far
// behind the chain would hide it, so resolution fails closed. 512 ledgers is about 45 min at
// mainnet cadence: far above any race, far below a day.
//
// It IS the lookup's scan window ([clickhouse.LatestLedgerLookbackLedgers] below the cursor), not
// a second copy: a row this bound refuses is not worth a ClickHouse read.
const maxSupplyLakeClampLedgers = clickhouse.LatestLedgerLookbackLedgers

// supplyAggregatorLedgers adapts the ingestion cursors + the lake to supply.LedgerLookup (same
// shape as the auto branch of internal/ops/supply/supply.go::resolveSnapshotLedger).
//
// ObservedAt is the resolved ledger's real close_time from ClickHouse stellar.ledgers, never
// time.Now(): a wall-clock stamp corrupts point-in-time supply queries.
//
// Resolution has two load-bearing steps:
//  1. The chain cursor names the position, not MAX(last_ledger) over every
//     cursor: an operator backfilling near the tip would win the max and stamp
//     the snapshot at a ledger no component was observed at. MAX survives only
//     as the named pre-first-run fallback.
//  2. The lake's landed tip bounds it. ingestion_cursors leads stellar.ledgers
//     by seconds, so an exact lookup routinely misses; resolution clamps to the
//     newest landed ledger at or before the cursor, reading only the
//     [maxSupplyLakeClampLedgers] below it (unbounded prunes no partition).
//
// Fail-closed: no cursor, no landed row within the clamp, or a lake trailing the cursor by more
// than that, returns a retryable no_ledger error, never a wall-clock guess.
type supplyAggregatorLedgers struct {
	s          supplyCursorLister
	closeTimes ledgerCloseTimeReader
}

func (a supplyAggregatorLedgers) LatestKnownLedger(ctx context.Context) (uint32, time.Time, error) {
	cursors, err := a.s.ListCursors(ctx)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("ListCursors: %w", err)
	}
	cursorLedger, source := supplyChainCursorLedger(cursors)
	if cursorLedger == 0 {
		return 0, time.Time{}, errors.New("no ingestion cursors yet — refresher will retry next tick")
	}
	lakeLedger, closeTime, found, err := a.closeTimes.LatestLedgerAtOrBefore(ctx, cursorLedger)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("resolve lake tip at or before cursor ledger %d: %w", cursorLedger, err)
	}
	if !found {
		return 0, time.Time{}, fmt.Errorf("stellar.ledgers has no row within %d ledgers below the %q cursor ledger %d — lake empty, gapped below the chain position, or a sink stalled further back than the clamp would accept; refusing to stamp the snapshot with wall-clock time", maxSupplyLakeClampLedgers, source, cursorLedger)
	}
	gap := cursorLedger - lakeLedger
	// Publish the gap before deciding on it, so the series carries the gap behind a refusal as
	// well as the ordinary lead. A lake outside the lookup window stops updating the gauge and
	// refuses above, the same alerting condition one step later.
	obs.AggregatorSupplyLakeClampLedgers.Set(float64(gap))
	// The production reader cannot return a row beyond the bound (same constant as its scan
	// window); this guards whatever [ledgerCloseTimeReader] it was handed, because stamping an
	// unchecked row is the failure being prevented.
	if gap > maxSupplyLakeClampLedgers {
		return 0, time.Time{}, fmt.Errorf("lake tip %d trails the %q cursor %d by %d ledgers (> %d) — that is a stalled lake, not a landing race; refusing to stamp a snapshot that far behind the chain", lakeLedger, source, cursorLedger, gap, maxSupplyLakeClampLedgers)
	}
	return lakeLedger, closeTime, nil
}

// supplyChainCursorLedger picks the chain position a snapshot is bounded by and names its cursor.
// Pure, so unit-testable without Postgres. See step 1 on [supplyAggregatorLedgers] for why the
// named cursor beats MAX(last_ledger); the MAX fallback names itself so an operator reading the
// resolution error sees that a job cursor supplied the bound.
func supplyChainCursorLedger(cursors []timescale.Cursor) (ledger uint32, source string) {
	for _, c := range cursors {
		if c.Source == supplyChainCursorSource {
			return c.LastLedger, c.Source
		}
	}
	var best timescale.Cursor
	for _, c := range cursors {
		if c.LastLedger > best.LastLedger {
			best = c
		}
	}
	if best.LastLedger == 0 {
		return 0, ""
	}
	name := best.Source
	if best.Sub != "" {
		name += "/" + best.Sub
	}
	return best.LastLedger, name + " (no " + supplyChainCursorSource + " cursor yet — FALLBACK)"
}

// supplyAggregatorStoreLookup adapts *timescale.Store to
// supply.AccountObservationLookup. Mirrors
// internal/ops/supply/supply.go::supplyStoreLookup.
type supplyAggregatorStoreLookup struct{ s *timescale.Store }

func (a supplyAggregatorStoreLookup) LatestAccountObservationAtOrBefore(ctx context.Context, accountID string, asOfLedger uint32) (supply.AccountObservationRow, error) {
	row, err := a.s.LatestAccountObservationAtOrBefore(ctx, accountID, asOfLedger)
	if err != nil {
		return supply.AccountObservationRow{}, err
	}
	return supply.AccountObservationRow{
		Balance:   row.Balance,
		IsRemoval: row.IsRemoval,
		Ledger:    row.Ledger,
	}, nil
}

// MaxAccountObservationLedger forwards the account-observer watermark used by
// the XLM freshness gate.
func (a supplyAggregatorStoreLookup) MaxAccountObservationLedger(ctx context.Context, asOfLedger uint32) (uint32, error) {
	return a.s.MaxAccountObservationLedger(ctx, asOfLedger)
}

// supplyAggregatorInserter adapts *timescale.Store to
// supply.SnapshotInserter.
type supplyAggregatorInserter struct{ s *timescale.Store }

func (a supplyAggregatorInserter) InsertSupply(ctx context.Context, snap supply.Supply) error {
	return a.s.InsertSupply(ctx, snap)
}

// supplyAggregatorSEP41Store adapts *timescale.Store to supply.SEP41SupplyStore, projecting
// SEP41KindTotals into the supply package's identical-shape type to avoid a cyclic import
// (timescale already imports supply for InsertSupply).
type supplyAggregatorSEP41Store struct{ s *timescale.Store }

func (a supplyAggregatorSEP41Store) SEP41KindTotalsAtOrBefore(ctx context.Context, contractID string, asOfLedger uint32) (supply.SEP41KindTotals, error) {
	t, err := a.s.SEP41KindTotalsAtOrBefore(ctx, contractID, asOfLedger)
	if err != nil {
		return supply.SEP41KindTotals{}, err
	}
	return supply.SEP41KindTotals{
		Mint:     t.Mint,
		Burn:     t.Burn,
		Clawback: t.Clawback,
	}, nil
}

func (a supplyAggregatorSEP41Store) SACBalanceForContractAtOrBefore(ctx context.Context, holder, assetKey string, asOfLedger uint32) (*big.Int, error) {
	return a.s.SACBalanceForContractAtOrBefore(ctx, holder, assetKey, asOfLedger)
}

func (a supplyAggregatorSEP41Store) TrustlineBalanceForAccountAtOrBefore(ctx context.Context, accountID, assetKey string, asOfLedger uint32) (*big.Int, error) {
	return a.s.TrustlineBalanceForAccountAtOrBefore(ctx, accountID, assetKey, asOfLedger)
}

func (a supplyAggregatorSEP41Store) MinSEP41ComponentLedger(ctx context.Context, contractID string, asOfLedger uint32) (uint32, error) {
	return a.s.MinSEP41ComponentLedger(ctx, contractID, asOfLedger)
}

func (a supplyAggregatorSEP41Store) SEP41GenesisBaselineSeeded(ctx context.Context, contractID string) (bool, error) {
	return a.s.SEP41GenesisBaselineSeeded(ctx, contractID)
}

// runBaselineRefresh ticks the baseline refresher on [baselineRefreshCadence], counting per-outcome.
// The first cycle runs at startup so a fresh deployment does not wait a full interval before the
// API can use baseline lookups for the confidence score.
func runBaselineRefresh(ctx context.Context, r *baseline.Refresher, pairs []canonical.Pair, logger *slog.Logger) {
	tick := func() {
		started := time.Now()
		sum := r.RefreshAll(ctx, pairs, baselineRefreshConcurrency)
		obs.AggregatorBaselineRefreshTotal.WithLabelValues("ok").Add(float64(sum.OK))
		obs.AggregatorBaselineRefreshTotal.WithLabelValues("not_enough_samples").Add(float64(sum.NotEnoughSamples))
		obs.AggregatorBaselineRefreshTotal.WithLabelValues("read_error").Add(float64(sum.ReadErrors))
		obs.AggregatorBaselineRefreshTotal.WithLabelValues("write_error").Add(float64(sum.WriteErrors))
		obs.AggregatorBaselineRefreshTotal.WithLabelValues("ok_per_minute_fallback").Add(float64(sum.OKPerMinuteFallback))
		obs.AggregatorBaselineRefreshTotal.WithLabelValues("ok_unvalued").Add(float64(sum.OKUnvalued))
		logger.Info("baseline refresh complete",
			"ok", sum.OK,
			"ok_unvalued", sum.OKUnvalued,
			"not_enough_samples", sum.NotEnoughSamples,
			"ok_per_minute_fallback", sum.OKPerMinuteFallback,
			"read_errors", sum.ReadErrors,
			"write_errors", sum.WriteErrors,
			"elapsed", time.Since(started).String(),
		)
	}

	tick() // immediate first refresh

	ticker := time.NewTicker(baselineRefreshCadence)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}

// baselineLookupAdapter wraps *timescale.Store to satisfy orchestrator.BaselineSource, returning a
// pair's cached MultiBaseline and computed_at for the confidence z-score. A pair with no row yields
// the zero-value triple, which the confidence step treats as bootstrap.
type baselineLookupAdapter struct{ store *timescale.Store }

func (a baselineLookupAdapter) LatestBaseline(ctx context.Context, pair canonical.Pair) (baseline.MultiBaseline, time.Time, error) {
	sb, err := a.store.LatestBaseline(ctx, pair)
	if err != nil {
		return baseline.MultiBaseline{}, time.Time{}, err
	}
	return sb.Multi, sb.ComputedAt, nil
}

// baselineSourceAdapter wraps *timescale.Store to satisfy
// baseline.TimedVWAPSource.
type baselineSourceAdapter struct{ store *timescale.Store }

func (a baselineSourceAdapter) TimedVWAPsForPair1m(ctx context.Context, pair canonical.Pair, from, to time.Time) ([]baseline.TimedVWAP, error) {
	return a.store.TimedVWAPsForPair1m(ctx, pair, from, to)
}

// baselineSinkAdapter wraps *timescale.Store to satisfy
// baseline.Sink. The adapter builds a timescale.StoredBaseline so
// the dep direction stays clean — the storage package doesn't need
// to import the baseline package as a Sink consumer.
type baselineSinkAdapter struct{ store *timescale.Store }

func (a baselineSinkAdapter) UpsertBaseline(
	ctx context.Context,
	pair canonical.Pair,
	computedAt, windowStart, windowEnd time.Time,
	m baseline.MultiBaseline,
) error {
	return a.store.UpsertBaseline(ctx, timescale.StoredBaseline{
		Pair:        pair,
		ComputedAt:  computedAt,
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
		Multi:       m,
	})
}

// cursorGetter is the narrow seam divergenceLedgerAdapter depends
// on — *timescale.Store satisfies it directly; tests substitute a
// fake.
type cursorGetter interface {
	GetCursor(ctx context.Context, source, sub string) (timescale.Cursor, error)
}

// divergenceLedgerTimeout bounds divergenceLedgerAdapter's per-
// observation cursor read so a stalled database never blocks a
// divergence observation insert.
const divergenceLedgerTimeout = 2 * time.Second

// divergenceLedgerAdapter satisfies timescale.LedgerProvider from the live-ingest `ledgerstream`
// cursor, so divergence_observations rows carry a real observed_at_ledger (without a provider the
// sink writes 0). It fails open to 0, the sink's own "unknown" sentinel, on any read error, so a
// transient cursor failure degrades an insert rather than blocking it.
type divergenceLedgerAdapter struct{ cursors cursorGetter }

func (a divergenceLedgerAdapter) LatestLedger() uint32 {
	ctx, cancel := context.WithTimeout(context.Background(), divergenceLedgerTimeout)
	defer cancel()
	c, err := a.cursors.GetCursor(ctx, supplyChainCursorSource, "")
	if err != nil {
		return 0
	}
	return c.LastLedger
}

// resolveUSDPeggedSorobanAssets derives the SAC-wrapper contracts that inherit a USD peg from
// `[trades].usd_pegged_classic_assets` via `[supply].sac_wrappers` (SAC id -> classic asset key),
// the same inputs NewUSDVolumeQuoteSpec combines at insert time, so the min_usd_volume floor applies
// to SAC-quoted pairs with no extra knob.
//
// It soft-fails like config.TradesConfig.USDPeggedClassics: malformed entries are skipped, not
// fatal, since config validation already rejects those shapes.
func resolveUSDPeggedSorobanAssets(classicPegRaws []string, sacWrappers map[string]string, logger *slog.Logger) []canonical.Asset {
	if len(classicPegRaws) == 0 || len(sacWrappers) == 0 {
		return nil
	}
	pegged := make(map[string]struct{}, len(classicPegRaws))
	for _, raw := range classicPegRaws {
		asset, err := canonical.ParseAsset(raw)
		if err != nil || asset.Type != canonical.AssetClassic {
			continue // already validated + logged by TradesConfig.USDPeggedClassics
		}
		pegged[asset.Code+"-"+asset.Issuer] = struct{}{}
	}
	var out []canonical.Asset
	for sacID, supplyKey := range sacWrappers {
		classic, err := canonical.ParseAsset(supplyKey)
		if err != nil || classic.Type != canonical.AssetClassic {
			continue // SupplyConfig.Validate() already enforces asset_key shape
		}
		if _, ok := pegged[classic.Code+"-"+classic.Issuer]; !ok {
			continue // this SAC's underlying classic isn't a declared USD peg
		}
		soroban, err := canonical.NewSorobanAsset(sacID)
		if err != nil {
			logger.Warn("usd_pegged_soroban_assets: skipping malformed sac_wrappers key",
				"sac_id", sacID, "err", err)
			continue
		}
		out = append(out, soroban)
	}
	return out
}

// defaultPairs is the built-in pair set used when [aggregate].pairs is
// empty: XLM (as both crypto:XLM and native), BTC and ETH against fiat
// USD, EUR and GBP.
func defaultPairs() []canonical.Pair {
	cryptos := []string{"XLM", "BTC", "ETH"}
	fiats := []string{"USD", "EUR", "GBP"}
	var out []canonical.Pair
	for _, c := range cryptos {
		bases := make([]canonical.Asset, 0, 2)
		ca, err := canonical.NewCryptoAsset(c)
		if err == nil {
			bases = append(bases, ca)
		}
		// XLM has two identities: the abstract `crypto:XLM` (off-chain CEX/FX trades) and `native`
		// (every on-chain DEX / SDEX trade). One VWAP is published per (base, quote) cache key and
		// the API resolves the caller's asset literally, so `?asset=native` would not see a
		// `crypto:XLM` VWAP. Emit both so either form hits a populated key whenever any source
		// publishes under it.
		if c == "XLM" {
			bases = append(bases, canonical.NativeAsset())
		}
		for _, base := range bases {
			for _, f := range fiats {
				fa, err := canonical.NewFiatAsset(f)
				if err != nil {
					continue
				}
				p, err := canonical.NewPair(base, fa)
				if err != nil {
					continue
				}
				out = append(out, p)
			}
		}
	}
	return out
}

// startMetricsServer mounts a /metrics + /healthz listener at cfg.MetricsListen and returns the
// server for graceful shutdown. An empty MetricsListen disables it with a warning, since the alert
// rules in deploy/monitoring/rules/aggregator.yml need these counters scrapable.
//
// Single-host coexistence: the indexer also defaults obs.metrics_listen to 127.0.0.1:9464. When the
// field was never set (config.ObsConfig.MetricsListenSet false) and still holds that default, shift
// to :9465 so one binary does not silently lose its listener to "address already in use". An
// explicitly configured value is honoured verbatim, even if it equals the indexer's default.
func startMetricsServer(cfg config.ObsConfig, schema v1.SchemaVersionReader, logger *slog.Logger) *http.Server {
	if cfg.MetricsListen == "" {
		logger.Warn("obs.metrics_listen is empty — /metrics endpoint disabled; aggregator-silent / outlier-storm / class-drop-spike alerts will not fire")
		return nil
	}
	addr := metricsListenAddr(cfg, logger)
	srv := &http.Server{
		Addr:              addr,
		Handler:           newMetricsMux(schema),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("metrics endpoint listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server exited", "err", err)
		}
	}()
	return srv
}

// newMetricsMux serves /metrics, the constant liveness /healthz, and
// /readyz: the schema-head check the deploy gate probes, so a binary
// swapped ahead of its migrations fails the deploy instead of passing
// `systemctl is-active` while every write errors.
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

// metricsListenAddr decides the actual bind address for startMetricsServer.
// Split out so the single-host shift decision is testable without
// spinning up a real listener.
func metricsListenAddr(cfg config.ObsConfig, logger *slog.Logger) string {
	addr := cfg.MetricsListen
	if !cfg.MetricsListenSet && addr == aggregatorMetricsCollidingDefault {
		addr = aggregatorMetricsShiftedAddr
		logger.Info("obs.metrics_listen left at indexer default; shifting aggregator to "+aggregatorMetricsShiftedAddr+" to avoid single-host port collision",
			"original", aggregatorMetricsCollidingDefault, "shifted_to", aggregatorMetricsShiftedAddr)
	}
	return addr
}

// aggregatorMetricsCollidingDefault is the indexer's default
// metrics_listen address; the aggregator auto-shifts when it sees
// this exact value so single-host deploys don't collide silently.
const (
	aggregatorMetricsCollidingDefault = "127.0.0.1:9464"
	aggregatorMetricsShiftedAddr      = "127.0.0.1:9465"
)

// mkLogger builds the structured logger with the configured format /
// level. Parallel to cmd/stellarindex-indexer.
func mkLogger(cfg config.ObsConfig) *slog.Logger {
	return obs.NewLogger(cfg, "stellarindex-aggregator")
}

// divergenceRefreshCadence is the longest normal spacing between two
// divergence refreshes of one pair: the pass runs on the first tick at
// least divergence_min_interval_seconds after the previous one.
func divergenceRefreshCadence(a config.AggregateConfig) time.Duration {
	tick := time.Duration(a.IntervalSeconds) * time.Second
	if tick <= 0 {
		tick = orchestrator.DefaultInterval
	}
	return time.Duration(a.DivergenceMinIntervalSeconds)*time.Second + tick
}

// buildAnomalyChecker constructs an anomaly.Checker from AnomalyConfig, or (nil, nil) when disabled
// (the orchestrator then publishes every bucket unevaluated). Per-class thresholds merge over
// anomaly.DefaultThresholds, so specifying one class keeps the rest; assets absent from the
// classification overrides fall through to ClassDefault.
func buildAnomalyChecker(cfg config.AnomalyConfig) (*anomaly.Checker, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	thresholds := anomaly.DefaultThresholds()
	for className, t := range cfg.Thresholds {
		thresholds[anomaly.AssetClass(className)] = anomaly.Thresholds{
			WarnPct:   t.WarnPct,
			FreezePct: t.FreezePct,
		}
	}

	overrides := make(map[string]anomaly.AssetClass, len(cfg.Classifications))
	for assetID, className := range cfg.Classifications {
		overrides[assetID] = anomaly.AssetClass(className)
	}
	// Needs the alias registry installed above to see sac_wrappers twins.
	if err := anomaly.ValidateOverrides(overrides); err != nil {
		return nil, err
	}
	classifier := anomaly.NewClassifier(overrides)

	return anomaly.NewChecker(thresholds, classifier)
}

// buildTriangulations resolves the configured rows into validated orchestrator.TriangulationChain
// values (chainable legs, endpoints match target); an invalid chain fails startup rather than
// emitting missing-leg metrics in production.
//
// When aggregate.triangulation_enabled is false it returns nil so the orchestrator skips the
// triangulation tick. Validation still runs first, so a malformed row is caught even with the
// switch off instead of surprising the operator when it is flipped on.
func buildTriangulations(cfg config.AggregateConfig) ([]orchestrator.TriangulationChain, error) {
	resolved, err := cfg.AggregatorTriangulations()
	if err != nil {
		return nil, err
	}
	out := make([]orchestrator.TriangulationChain, 0, len(resolved))
	for i, r := range resolved {
		chain := orchestrator.TriangulationChain{Target: r.Target, Legs: r.Legs}
		if err := orchestrator.ValidateTriangulationChain(chain); err != nil {
			return nil, fmt.Errorf("aggregate.triangulations[%d]: %w", i, err)
		}
		out = append(out, chain)
	}
	if !cfg.TriangulationEnabled {
		return nil, nil
	}
	return out, nil
}

// buildCompositeReference maps `[aggregate.composite_reference]` onto
// the orchestrator's config. A knob that parses and does nothing is a
// dormant knob, so every field is carried; zero numerics fall through
// to the orchestrator's Default* constants.
func buildCompositeReference(cfg config.AggregateConfig) (orchestrator.CompositeReferenceConfig, error) {
	targets, err := cfg.CompositeReferenceTargets()
	if err != nil {
		return orchestrator.CompositeReferenceConfig{}, err
	}
	return orchestrator.CompositeReferenceConfig{
		Enabled:          cfg.CompositeReference.Enabled,
		Targets:          targets,
		ToleranceBps:     cfg.CompositeReference.ToleranceBps,
		MinLegSources:    cfg.CompositeReference.MinLegSources,
		FXMaxAge:         time.Duration(cfg.CompositeReference.FXMaxAgeHours) * time.Hour,
		LegDispersionBps: cfg.CompositeReference.LegDispersionBps,
		ReleaseBandPct:   cfg.CompositeReference.ReleaseBandPct,
	}, nil
}

// buildCrossCheckRefresher composes the periodic supply-cross-check emitter from `[supply]`. It
// returns (nil, nil) when no pair survives the watched-set intersection; every skipped classic
// wrapper is logged at Warn by [crossCheckPairs], never dropped silently.
func buildCrossCheckRefresher(cfg config.Config, store *timescale.Store, logger *slog.Logger) (*supply.CrossCheckRefresher, error) {
	pairs, err := crossCheckPairs(cfg.Supply, cfg.Stellar.Passphrase(), logger)
	if err != nil || len(pairs) == 0 {
		return nil, err
	}
	fullWrap := 0
	for _, p := range pairs {
		if p.WrapClass == supply.WrapClassFull {
			fullWrap++
		}
	}
	logger.Info("cross-check pairs registered", "count", len(pairs), "fully_wrapped", fullWrap)
	return supply.NewCrossCheckRefresher(
		pairs,
		supplyAggregatorSnapshotReader{s: store},
		obsCrossCheckEmitter{},
		logger,
	)
}

// crossCheckPairs derives one [supply.CrossCheckPair] per `sac_wrappers` entry whose classic asset
// is in `watched_classic_assets` AND whose contract is in `watched_sep41_contracts`.
//
//   - Both watched-classic lookups use [crossCheckClassicKey], so a "CODE-ISSUER" wrapper value
//     matches the "CODE:ISSUER" snapshot key instead of silently disabling the check.
//   - The contract id must be the classic asset's derived SAC on the configured network. That
//     discharges [supply.CrossCheck]'s same-asset precondition, and a SAC always carries 7
//     decimals, which makes the absolute 1-stroop [supply.CrossCheckTolerance] meaningful. A
//     mismatch is a startup error: the comparison would be between unrelated tokens.
//   - WrapClass is [supply.WrapClassFull] for `fully_wrapped_sacs`, else [supply.WrapClassPartial].
//
// Pure SEP-41 self-maps (contract → contract) have no classic side and are skipped. Output is
// ordered by SAC id so the logs are stable.
func crossCheckPairs(sc config.SupplyConfig, passphrase string, logger *slog.Logger) ([]supply.CrossCheckPair, error) {
	if len(sc.SACWrappers) == 0 {
		return nil, nil
	}
	watchedClassic := make(map[string]struct{}, len(sc.WatchedClassicAssets))
	for _, raw := range sc.WatchedClassicAssets {
		k, _, err := crossCheckClassicKey(raw)
		if err != nil {
			return nil, fmt.Errorf("watched classic asset %q: %w", raw, err)
		}
		watchedClassic[k] = struct{}{}
	}
	watchedSEP41 := make(map[string]struct{}, len(sc.WatchedSEP41Contracts))
	for _, c := range sc.WatchedSEP41Contracts {
		watchedSEP41[c] = struct{}{}
	}
	fullyWrapped := make(map[string]struct{}, len(sc.FullyWrappedSACs))
	for _, sacID := range sc.FullyWrappedSACs {
		fullyWrapped[sacID] = struct{}{}
	}

	sacIDs := make([]string, 0, len(sc.SACWrappers))
	for id := range sc.SACWrappers {
		sacIDs = append(sacIDs, id)
	}
	sort.Strings(sacIDs)

	pairs := make([]supply.CrossCheckPair, 0, len(sacIDs))
	for _, sacID := range sacIDs {
		pair, skip, err := crossCheckPairFor(sacID, sc.SACWrappers[sacID], watchedClassic, watchedSEP41, fullyWrapped, passphrase, logger)
		if err != nil {
			return nil, err
		}
		if skip {
			continue
		}
		pairs = append(pairs, pair)
	}
	return pairs, nil
}

// crossCheckPairFor derives sacID's [supply.CrossCheckPair] from its sac_wrappers value, or
// skip=true for a pure SEP-41 self-map (no classic side) or an entry missing from a watched set
// (logged at Warn so it is not silently disabled).
func crossCheckPairFor(sacID, raw string, watchedClassic, watchedSEP41, fullyWrapped map[string]struct{}, passphrase string, logger *slog.Logger) (pair supply.CrossCheckPair, skip bool, err error) {
	classicKey, asset, err := crossCheckClassicKey(raw)
	if errors.Is(err, errNoClassicSide) {
		return supply.CrossCheckPair{}, true, nil
	}
	if err != nil {
		return supply.CrossCheckPair{}, false, fmt.Errorf("sac_wrappers[%q] = %q: %w", sacID, raw, err)
	}
	_, classicWatched := watchedClassic[classicKey]
	_, sacWatched := watchedSEP41[sacID]
	if !classicWatched || !sacWatched {
		logger.Warn("cross-check: sac_wrappers entry NOT cross-checked; add both sides to the watched sets to enable it",
			"sac_id", sacID, "classic_key", classicKey,
			"in_watched_classic_assets", classicWatched,
			"in_watched_sep41_contracts", sacWatched)
		return supply.CrossCheckPair{}, true, nil
	}
	derived, ok := xdrjson.SACContractID(asset.String(), passphrase)
	if !ok {
		return supply.CrossCheckPair{}, false, fmt.Errorf("sac_wrappers[%q] = %q: cannot derive the asset's SAC contract id", sacID, raw)
	}
	if derived != sacID {
		return supply.CrossCheckPair{}, false, fmt.Errorf("sac_wrappers[%q] = %q: the SAC of %s on this network is %s, so the cross-check would compare unrelated tokens",
			sacID, raw, classicKey, derived)
	}
	wrapClass := supply.WrapClassPartial
	if _, ok := fullyWrapped[sacID]; ok {
		wrapClass = supply.WrapClassFull
	}
	return supply.CrossCheckPair{ClassicKey: classicKey, SACKey: sacID, WrapClass: wrapClass}, false, nil
}

// errNoClassicSide marks a wrapper value naming a Soroban contract (a
// pure SEP-41 self-map), which has no classic supply to compare.
var errNoClassicSide = errors.New("not a classic or native asset")

// crossCheckClassicKey normalises a classic asset in "CODE-ISSUER" or
// "CODE:ISSUER" form (or native) to the key its supply snapshot is
// written under.
func crossCheckClassicKey(raw string) (string, canonical.Asset, error) {
	a, err := canonical.ParseAsset(raw)
	if err != nil {
		return "", canonical.Asset{}, err
	}
	if a.Type != canonical.AssetClassic && a.Type != canonical.AssetNative {
		return "", canonical.Asset{}, errNoClassicSide
	}
	k, err := supply.AssetKey(a)
	if err != nil {
		return "", canonical.Asset{}, err
	}
	return k, a, nil
}

// runCrossCheckRefresh ticks the cross-check refresher on `cadence`. The first cycle runs at
// startup so a fresh deployment shows `outcome=missing_snapshot` on first scrape instead of
// silence.
func runCrossCheckRefresh(ctx context.Context, r *supply.CrossCheckRefresher, cadence time.Duration) {
	tick := func() { _ = r.Tick(ctx) }
	tick()

	ticker := time.NewTicker(cadence)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}

// supplyAggregatorSnapshotReader adapts *timescale.Store to
// supply.SnapshotReader. Maps timescale's ErrNotFound onto
// supply.ErrNoSnapshot so the refresher distinguishes the bootstrap
// state (no rows yet) from transient read failures.
type supplyAggregatorSnapshotReader struct{ s *timescale.Store }

func (a supplyAggregatorSnapshotReader) LatestSupply(ctx context.Context, assetKey string) (supply.Supply, error) {
	snap, err := a.s.LatestSupply(ctx, assetKey)
	if errors.Is(err, timescale.ErrNotFound) {
		return supply.Supply{}, fmt.Errorf("LatestSupply %s: %w", assetKey, supply.ErrNoSnapshot)
	}
	return snap, err
}

// obsCrossCheckEmitter wires supply.CrossCheckEmitter onto the
// package-level obs Prometheus collectors. Kept as a tiny adapter so
// the supply package stays Prometheus-agnostic and unit-testable.
type obsCrossCheckEmitter struct{}

func (obsCrossCheckEmitter) Divergence(classicKey string, wrapClass supply.WrapClass, stroops float64) {
	obs.SupplyCrossCheckDivergenceStroops.WithLabelValues(classicKey, string(wrapClass)).Set(stroops)
}

func (obsCrossCheckEmitter) ClearDivergence(classicKey string, wrapClass supply.WrapClass) {
	obs.SupplyCrossCheckDivergenceStroops.DeleteLabelValues(classicKey, string(wrapClass))
}

func (obsCrossCheckEmitter) Outcome(kind supply.CrossCheckOutcomeKind, wrapClass supply.WrapClass) {
	obs.SupplyCrossCheckTotal.WithLabelValues(string(kind), string(wrapClass)).Inc()
}

// ─── Supply-divergence cross-check wiring ────────────────────────────

// buildSupplyDivergenceService composes the supply cross-check service
// from `[divergence.supply]`. Returns (nil, nil) when the check is
// disabled OR when no reference is enabled — a service with nothing to
// compare against would emit only `no_reference` forever, so it isn't
// wired.
func buildSupplyDivergenceService(cfg config.DivergenceSupplyConfig, store *timescale.Store, logger *slog.Logger) (*divergence.SupplyService, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	refs := buildSupplyDivergenceReferences(cfg)
	if len(refs) == 0 {
		logger.Warn("supply divergence enabled but every reference is disabled — skipping (no reference to compare against)")
		return nil, nil
	}
	svc, err := divergence.NewSupplyService(divergence.SupplyServiceOptions{
		References: refs,
		Reader:     divergenceServedSupplyReader{s: store},
		Emitter:    obsSupplyDivergenceEmitter{},
		Threshold:  cfg.ThresholdPct / 100.0, // percent → ratio
		Logger:     logger,
	})
	if err != nil {
		return nil, err
	}
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = r.Name()
	}
	logger.Info("supply-divergence worker wired",
		"references", names,
		"threshold_pct", cfg.ThresholdPct,
		"refresh_interval_seconds", int(supplyDivergenceRefreshInterval/time.Second))
	return svc, nil
}

// buildSupplyDivergenceReferences constructs the enabled external
// circulating-supply references. The Stellar Dashboard covers XLM (free,
// authoritative); CoinGecko covers any asset with a coin id but is off
// by default (its free tier is 429-throttled).
func buildSupplyDivergenceReferences(cfg config.DivergenceSupplyConfig) []divergence.SupplyReference {
	var refs []divergence.SupplyReference
	if cfg.Dashboard.Enabled {
		refs = append(refs, divergence.NewStellarDashboardReference(divergence.StellarDashboardOptions{
			BaseURL: cfg.Dashboard.BaseURL,
		}))
	}
	if cfg.CoinGecko.Enabled {
		refs = append(refs, divergence.NewCoinGeckoSupplyReference(divergence.CoinGeckoSupplyOptions{
			BaseURL: cfg.CoinGecko.BaseURL,
			APIKey:  cfg.CoinGecko.APIKey,
			IDMap:   cfg.CoinGecko.IDMap,
		}))
	}
	return refs
}

// supplyDivergenceRefreshInterval is the supply cross-check cycle; supply
// moves slowly, so a slow cadence keeps external-quota pressure minimal.
const supplyDivergenceRefreshInterval = 15 * time.Minute

// runSupplyDivergenceRefresh ticks the supply cross-check on interval. The first cycle runs at
// startup so the outcome metric appears on the first scrape instead of silence.
func runSupplyDivergenceRefresh(ctx context.Context, svc *divergence.SupplyService, interval time.Duration) {
	svc.Tick(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			svc.Tick(ctx)
		}
	}
}

// divergenceServedSupplyReader adapts *timescale.Store to divergence.ServedSupplyReader, mapping
// ErrNotFound onto divergence.ErrNoServedSupply so the bootstrap state (no snapshot yet) is
// distinct from a real read failure.
type divergenceServedSupplyReader struct{ s *timescale.Store }

func (a divergenceServedSupplyReader) LatestCirculatingSupply(ctx context.Context, assetKey string) (divergence.ServedSupply, error) {
	snap, err := a.s.LatestSupply(ctx, assetKey)
	if errors.Is(err, timescale.ErrNotFound) {
		return divergence.ServedSupply{}, fmt.Errorf("LatestSupply %s: %w", assetKey, divergence.ErrNoServedSupply)
	}
	if err != nil {
		return divergence.ServedSupply{}, err
	}
	return divergence.ServedSupply{
		Circulating:    snap.CirculatingSupply,
		LedgerSequence: snap.LedgerSequence,
		ObservedAt:     snap.ObservedAt,
	}, nil
}

// obsSupplyDivergenceEmitter wires divergence.SupplyEmitter onto the
// package-level obs collectors — kept as a tiny adapter so the
// divergence package stays Prometheus-agnostic (same split as
// obsCrossCheckEmitter).
type obsSupplyDivergenceEmitter struct{}

func (obsSupplyDivergenceEmitter) Ratio(asset, reference string, ratio float64) {
	obs.SupplyDivergenceRatio.WithLabelValues(asset, reference).Set(ratio)
}

func (obsSupplyDivergenceEmitter) Outcome(kind divergence.SupplyOutcomeKind) {
	obs.SupplyDivergenceTotal.WithLabelValues(string(kind)).Inc()
}

func (obsSupplyDivergenceEmitter) Duration(kind divergence.SupplyOutcomeKind, seconds float64) {
	obs.SupplyDivergenceDurationSeconds.WithLabelValues(string(kind)).Observe(seconds)
}

// buildDivergenceReferences is the sole divergence reference builder: the CoinGecko and Chainlink
// clients plus the on-chain oracle references (reading our own served oracle_updates rows). The API
// binary builds a cache-reading Service with no References. cgKeys are the CoinGecko poller's keys,
// which the price reference authenticates with. oracles may be nil (no Postgres), in which case the
// on-chain references are skipped with a warning.
func buildDivergenceReferences(cfg config.DivergenceConfig, cgKeys config.CoinGeckoVenueConfig, oracles divergence.OracleReader, logger *slog.Logger) []divergence.Reference {
	var refs []divergence.Reference

	if cfg.CoinGecko.Enabled {
		refs = append(refs, divergence.NewCoinGeckoReference(divergence.CoinGeckoOptions{
			BaseURL:    cfg.CoinGecko.BaseURL,
			APIKey:     cgKeys.APIKey,
			DemoAPIKey: cgKeys.DemoAPIKey,
			Logger:     logger,
			IDMap:      cfg.CoinGecko.IDMap,
		}))
	}

	if cfg.Chainlink.Enabled {
		if len(cfg.Chainlink.FeedMap) == 0 {
			logger.Warn("divergence: chainlink enabled but FeedMap is empty — skipping")
		} else {
			feedMap := make(map[string]divergence.ChainlinkFeed, len(cfg.Chainlink.FeedMap))
			for pair, f := range cfg.Chainlink.FeedMap {
				feedMap[pair] = divergence.ChainlinkFeed{
					Address:  f.Address,
					Decimals: f.Decimals,
					Invert:   f.Invert,
					MaxAge:   time.Duration(f.MaxAgeHours) * time.Hour,
				}
			}
			refs = append(refs, divergence.NewChainlinkReference(divergence.ChainlinkOptions{
				RPCURL:  cfg.Chainlink.RPCURL,
				Logger:  logger,
				FeedMap: feedMap,
			}))
		}
	}

	refs = append(refs, buildOracleDivergenceReferences(cfg, oracles, logger)...)
	return appendSyntheticCrossReference(refs, logger)
}

// appendSyntheticCrossReference derives the USD-cross reference from the ALREADY-GATED reference
// set, with no config gate of its own (a new gate is a new way for a feature to silently not
// exist). It exists whenever its ingredients do: a USD-quoted oracle leg (reflector-cex, chainlink,
// redstone, band; deliberately NOT CoinGecko, the one existing DIRECT reference for non-USD-fiat
// pairs, which would correlate the synthetic with the source it corroborates) and a fiat leg.
//
// It gives EUR/GBP-quoted pairs a second reference so SuccessCount reaches the divergence trust
// floor and the corroborated-release gate can auto-release genuine repricings unattended.
func appendSyntheticCrossReference(refs []divergence.Reference, logger *slog.Logger) []divergence.Reference {
	byName := make(map[string]divergence.Reference, len(refs))
	for _, r := range refs {
		byName[r.Name()] = r
	}
	pick := func(names ...string) []divergence.Reference {
		var out []divergence.Reference
		for _, n := range names {
			if r, ok := byName[n]; ok {
				out = append(out, r)
			}
		}
		return out
	}
	// Chainlink serves BOTH legs: its FeedMap carries crypto/USD feeds (base leg) and direct
	// fiat/USD feeds. It routes by pair and answers only what its FeedMap lists, so membership in
	// both leg sets cannot double-answer one leg. In the FX legs it is the FALLBACK after
	// reflector-fx (on-chain rows we already index, no extra RPC) and the only proven GBP/USD
	// source; without it XLM/GBP would stay single-reference.
	usdLegs := pick(
		divergence.OracleSourceReflectorCEX,
		divergence.ChainlinkSourceName,
		divergence.OracleSourceRedstone,
		divergence.OracleSourceBand,
	)
	fxLegs := pick(
		divergence.OracleSourceReflectorFX,
		divergence.ChainlinkSourceName,
	)
	syn, err := divergence.NewSyntheticCrossReference(divergence.SyntheticCrossOptions{
		USDLegs: usdLegs,
		FXLegs:  fxLegs,
	})
	if err != nil {
		// Missing a leg class — expected on minimal configs; the
		// non-USD-fiat pairs then simply keep their single direct
		// reference (and their operator-release posture).
		logger.Info("divergence: synthetic USD-cross reference not constructed", "reason", err)
		return refs
	}
	return append(refs, syn)
}

// buildOracleDivergenceReferences constructs the on-chain oracle reference set (reflector
// dex/cex/fx, redstone, band) per the `[divergence.*]` gates. Split out to stay under funlen.
func buildOracleDivergenceReferences(cfg config.DivergenceConfig, oracles divergence.OracleReader, logger *slog.Logger) []divergence.Reference {
	anyEnabled := cfg.Reflector.Enabled || cfg.Redstone.Enabled || cfg.Band.Enabled
	if oracles == nil {
		if anyEnabled {
			logger.Warn("divergence: on-chain oracle references enabled but no oracle_updates reader — skipping")
		}
		return nil
	}
	var refs []divergence.Reference
	add := func(source string, gate config.DivergenceOracleConfig) {
		if !gate.Enabled {
			return
		}
		ref, err := divergence.NewOracleReference(divergence.OracleReferenceOptions{
			Source: source,
			Reader: oracles,
		})
		if err != nil {
			// Unreachable with non-empty Source + non-nil Reader;
			// warn-and-skip keeps the rest of the reference set alive.
			logger.Warn("divergence: oracle reference construction failed",
				"source", source, "err", err)
			return
		}
		refs = append(refs, ref)
	}
	add(divergence.OracleSourceReflectorDEX, cfg.Reflector)
	add(divergence.OracleSourceReflectorCEX, cfg.Reflector)
	add(divergence.OracleSourceReflectorFX, cfg.Reflector)
	add(divergence.OracleSourceRedstone, cfg.Redstone)
	add(divergence.OracleSourceBand, cfg.Band)
	return refs
}

// mevObserver adapts the MEV worker's per-run outcomes to Prometheus (counter, duration histogram
// and new-events counter).
// priceAlertVWAPStore is the storage seam priceAlertVWAPReader needs (latest closed bucket plus the
// guard's trailing baseline), kept an interface so the reader is unit-testable without a database.
type priceAlertVWAPStore interface {
	LatestClosedVWAP1mForPair(ctx context.Context, p canonical.Pair) (timescale.Vwap1mRow, error)
	RecentClosedVWAP1mCombined(ctx context.Context, p canonical.Pair, limit int) ([]timescale.Vwap1mRow, error)
}

// priceAlertVWAPReader adapts the timescale store to pricealerts.PriceReader. The latest-bucket
// query combines both stored pair orientations; sql.ErrNoRows maps to ok=false, a benign no-op
// rather than a failure. Vwap1mRow.Bucket is the START of the 1-minute window, so the reported
// close time is +1 minute.
type priceAlertVWAPReader struct {
	store  priceAlertVWAPStore
	logger *slog.Logger
	gate   pricingguard.Gate // zero → withholds nothing
	// decimals resolves a confirmed non-7dp scale as /v1/price does ([aggregate.ResolveDecimals]),
	// so a threshold set from the corrected price is compared against a corrected observation.
	// nil means every leg is StandardDecimals (7).
	decimals aggregate.DecimalsLookup
}

func (r priceAlertVWAPReader) LatestVWAP(ctx context.Context, base, quote canonical.Asset) (string, time.Time, bool, error) {
	if _, err := canonical.NewPair(base, quote); err != nil {
		return "", time.Time{}, false, err
	}
	// Walk every spelling of both legs, literal pair first and SAC forms last
	// (canonical.AssetAliases owns the order), as /v1/price does: `native`/fiat:USD has no rows of
	// its own because its market is recorded under crypto:XLM, so a literal-only read never fires.
	// The first spelling with a closed bucket answers; a read error ends the walk rather than
	// falling through to a thinner spelling.
	for _, b := range canonical.AssetAliases(base) {
		for _, q := range canonical.AssetAliases(quote) {
			pair, err := canonical.NewPair(b, q)
			if err != nil {
				continue // two spellings of one asset collapse to a self-pair
			}
			row, err := r.store.LatestClosedVWAP1mForPair(ctx, pair)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return "", time.Time{}, false, err
			}
			// Price-withholding chokepoint ([pricing_guard]), the expression the API serves under: an
			// alert must never fire off a thin market or a scam-flagged issuer on EITHER leg.
			// Withheld means ok=false and sticky (no other spelling is tried); the gate is
			// alias-invariant, so the requested pair's verdict is every spelling's.
			if r.gate.PriceWithheld(ctx, base, quote, "price_alert") {
				return "", time.Time{}, false, nil
			}
			// Same serving-sanity guard as the API raw-bucket paths: a bare closed bucket bypasses the
			// orchestrator's outlier / min-volume / freeze filters, so a fat-finger print would fire a
			// SPURIOUS alert. The guard serves last-known-good off the pair's trailing baseline and is
			// byte-identical on a healthy bucket. An unvalidated bucket is the guard's fail-open case;
			// an alert has no stale flag to carry that doubt, so it does not fire (sticky).
			served, lowConfidence, _ := pricingguard.GuardServedVWAP1mConfidence(ctx, r.store, r.logger, pair, row)
			if lowConfidence {
				return "", time.Time{}, false, nil
			}
			return r.adjustServed(b, q, served)
		}
	}
	return "", time.Time{}, false, nil
}

// adjustServed applies the /v1/price decimals correction to a guarded bucket.
func (r priceAlertVWAPReader) adjustServed(b, q canonical.Asset, served timescale.Vwap1mRow) (string, time.Time, bool, error) {
	// The guard's VWAP is the raw quote/base CAGG ratio, as /v1/price reads it before AdjustPrice.
	// Apply the same correction: the threshold was set from the corrected value, so an uncorrected
	// comparison is wrong by the pair's decimals skew.
	baseDec := aggregate.ResolveDecimals(r.decimals, b)
	quoteDec := aggregate.ResolveDecimals(r.decimals, q)
	adjusted, err := adjustDecimalString(served.VWAP, baseDec, quoteDec)
	if err != nil {
		return "", time.Time{}, false, err
	}
	return adjusted, served.Bucket.Add(time.Minute), true, nil
}

// adjustDecimalString scales a raw decimal-string quote/base ratio by the decimals correction
// factor and renders an exact decimal string. The factor ([aggregate.DecimalsAdjustment]) is an
// exact power of ten, so scaling only shifts the decimal point and never loses a digit, unlike a
// fixed-width FloatString(10), which truncates a corrected price below 1e-10 to "0" (a positive
// price must never reach the alert comparator as zero).
func adjustDecimalString(raw string, baseDecimals, quoteDecimals int) (string, error) {
	if baseDecimals == quoteDecimals {
		return raw, nil
	}
	r, ok := new(big.Rat).SetString(raw)
	if !ok {
		return "", fmt.Errorf("price-alert decimals adjust: %q is not a decimal", raw)
	}
	adjusted := aggregate.AdjustPrice(r, baseDecimals, quoteDecimals)
	delta := baseDecimals - quoteDecimals
	if delta < 0 {
		delta = -delta
	}
	return adjusted.FloatString(decimalFractionDigits(raw) + delta), nil
}

// decimalFractionDigits counts the fractional digits of a plain decimal
// string ("1.230" -> 3, "5" -> 0) — the only shape prices_1m.VWAP is ever
// stored in.
func decimalFractionDigits(s string) int {
	i := strings.IndexByte(s, '.')
	if i < 0 {
		return 0
	}
	return len(s) - i - 1
}

// buildAggregatorSubstanceGate maps [pricing_guard] onto the shared substance policy for this
// binary's customer webhooks (price alerts, anomaly.freeze, divergence.firing), mirroring the API's
// gate via SubstancePolicyFromValues so the two cannot drift. It returns nil, a valid
// allow-everything gate, when the operator disabled it.
func buildAggregatorSubstanceGate(cfg config.PricingGuardConfig, store *timescale.Store, logger *slog.Logger) *pricingguard.SubstanceGate {
	if cfg.DisableSubstanceGate {
		logger.Warn("pricing_guard: substance gate DISABLED by config — customer webhooks publish every pair regardless of trailing market substance")
		return nil
	}
	pol := pricingguard.SubstancePolicyFromValues(
		cfg.SubstanceMinVolumeUSD, cfg.SubstanceMinBuckets,
		cfg.SubstanceMinSpanMinutes, cfg.SubstanceWindowHours)
	return pricingguard.NewSubstanceGate(store, pricingguard.SubstanceGateOptions{Policy: pol, Logger: logger})
}

type mevObserver struct{}

func (mevObserver) Run(outcome string, dur time.Duration, _ int, inserted int) {
	obs.MEVDetectRunsTotal.WithLabelValues(outcome).Inc()
	obs.MEVDetectDurationSeconds.WithLabelValues(outcome).Observe(dur.Seconds())
	if inserted > 0 {
		obs.MEVEventsInsertedTotal.Add(float64(inserted))
	}
}

func (mevObserver) Truncated(input string) {
	obs.MEVScanTruncatedTotal.WithLabelValues(input).Inc()
}

// canonicalSACName turns a Stellar Asset Contract's own name ("CODE:ISSUER",
// or "native") into the canonical asset id the served tier keys prices by.
func canonicalSACName(name string) (string, bool) {
	if name == "native" {
		return canonical.NativeAsset().String(), true
	}
	code, issuer, ok := strings.Cut(name, ":")
	if !ok {
		return "", false
	}
	asset, err := canonical.NewClassicAsset(code, issuer)
	if err != nil {
		return "", false
	}
	return asset.String(), true
}

// Decimals-resolver dial backoff, overridden by the [dialDecimalsResolver] tests.
//
// The floor is short because the failure is a COLD ClickHouse loading metadata after a reboot
// (minutes), and arming the guard promptly is the point. The ceiling keeps a genuinely down lake
// from becoming a dial loop while still re-arming well inside one sweep interval of recovery.
var (
	decimalsResolverRetryMin = 15 * time.Second
	decimalsResolverRetryMax = 5 * time.Minute
)

// dialDecimalsResolver opens the lake reader the decimals-assumption guard needs, retrying with
// exponential backoff until it succeeds or ctx is done. ok is false only on shutdown.
//
// Retrying rather than giving up is the correctness property. The guard turns a newly-listed
// non-7-decimal SEP-41 token into a nonstandard_decimals_assets row; without it
// aggregate.AdjustPrice applies no correction and every served price on that token's pairs is
// skewed by 10^(7-decimals), with no way to tell "found nothing" from "never ran". A dial that
// fails once at boot (expected after a reboot, since clickhouse-server spends minutes loading
// metadata and this unit's ordering does not wait) must delay the guard, not retire it.
//
// Failures log at Warn on the FIRST attempt and then only once backoff reaches its ceiling, so an
// outage costs one line per [decimalsResolverRetryMax]. Arming after a retry logs too, so an
// operator can see the guard come up.
func dialDecimalsResolver(
	ctx context.Context,
	logger *slog.Logger,
	addr string,
	dial func(context.Context, string) (*clickhouse.ExplorerReader, error),
) (reader *clickhouse.ExplorerReader, ok bool) {
	return dialLakeReaderWithRetry(ctx, logger, addr, dial,
		"decimals-guard: ClickHouse decimals resolver unavailable — non-7-decimal DEX-token detection NOT YET armed, retrying",
		"decimals-guard: ClickHouse decimals resolver reached — non-7-decimal DEX-token detection armed")
}

// dialLakeReaderWithRetry is the backoff loop behind every best-effort ClickHouse-lake reader
// (decimals resolver, MEV tx-order resolver, priceless-coverage SAC resolver, supply close-time
// reader). The cold-boot race (clickhouse-server loading metadata for minutes while the unit's
// After= ordering does not wait) hits every such reader, so "retry until it answers or shutdown"
// is the shared correctness property.
func dialLakeReaderWithRetry[T any](
	ctx context.Context,
	logger *slog.Logger,
	addr string,
	dial func(context.Context, string) (T, error),
	unavailableMsg, armedMsg string,
) (reader T, ok bool) {
	backoff := decimalsResolverRetryMin
	for attempt := 1; ; attempt++ {
		r, err := dial(ctx, addr)
		if err == nil {
			if attempt > 1 {
				logger.Info(armedMsg, "addr", addr, "attempts", attempt)
			}
			return r, true
		}
		if ctx.Err() != nil {
			var zero T
			return zero, false
		}
		if attempt == 1 || backoff >= decimalsResolverRetryMax {
			logger.Warn(unavailableMsg, "addr", addr, "err", err, "attempt", attempt, "retry_in", backoff)
		}
		select {
		case <-ctx.Done():
			var zero T
			return zero, false
		case <-time.After(backoff):
		}
		if backoff < decimalsResolverRetryMax {
			backoff *= 2
			if backoff > decimalsResolverRetryMax {
				backoff = decimalsResolverRetryMax
			}
		}
	}
}

// lazyCloseTimeReader implements [ledgerCloseTimeReader] by dialing the lake in its own goroutine
// with [dialLakeReaderWithRetry], so a boot-time outage delays snapshot-ObservedAt stamping instead
// of aborting the aggregator. A refresh tick that runs before the dial succeeds blocks on [ready]
// rather than reading a nil reader or a stale one-shot failure.
type lazyCloseTimeReader struct {
	ready  chan struct{}
	reader *clickhouse.ExplorerReader
}

// dial is the [clickhouse.NewExplorerReader] signature, overridden by tests
// so they don't need a live ClickHouse to exercise the retry loop.
func newLazyCloseTimeReader(ctx context.Context, addr string, logger *slog.Logger,
	dial func(context.Context, string) (*clickhouse.ExplorerReader, error),
) *lazyCloseTimeReader {
	r := &lazyCloseTimeReader{ready: make(chan struct{})}
	go func() {
		defer worker.Recover(logger, "supply-refresh-close-time-dial")
		reader, ok := dialLakeReaderWithRetry(ctx, logger, addr, dial,
			"supply-refresh: ClickHouse close-time reader unavailable — snapshot ObservedAt stamping deferred, retrying",
			"supply-refresh: ClickHouse close-time reader reached — snapshot ObservedAt stamping armed")
		if ok {
			r.reader = reader
		}
		close(r.ready)
	}()
	return r
}

// LatestLedgerAtOrBefore blocks the calling refresh tick until the dial
// resolves (success or shutdown), never permanently degrading the guard.
func (r *lazyCloseTimeReader) LatestLedgerAtOrBefore(ctx context.Context, maxSeq uint32) (uint32, time.Time, bool, error) {
	select {
	case <-r.ready:
	case <-ctx.Done():
		return 0, time.Time{}, false, ctx.Err()
	}
	if r.reader == nil {
		return 0, time.Time{}, false, errors.New("supply-refresh: ClickHouse close-time reader unavailable (shutdown reached before the lake did)")
	}
	return r.reader.LatestLedgerAtOrBefore(ctx, maxSeq)
}

// Close releases the reader if the dial had already settled. It must not wait for an in-flight
// dial: defers run LIFO, so the rootCtx cancel that stops the retry loop may not have run yet, and
// blocking here would deadlock a non-signal error return. An in-flight dial dies with the process.
func (r *lazyCloseTimeReader) Close() {
	select {
	case <-r.ready:
		if r.reader != nil {
			_ = r.reader.Close()
		}
	default:
	}
}
