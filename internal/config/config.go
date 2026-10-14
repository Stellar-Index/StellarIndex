package config

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// Config is the root configuration for every Stellar Index binary; every field needs a `doc:` tag (make docs-config fails without it).
type Config struct {
	Region       RegionConfig       `toml:"region" doc:"Region identity — ID, display name, home domain."`
	Stellar      StellarConfig      `toml:"stellar" doc:"Endpoints for stellar-core and stellar-rpc."`
	Storage      StorageConfig      `toml:"storage" doc:"Postgres/TimescaleDB, Redis, MinIO connection details."`
	Ingestion    IngestionConfig    `toml:"ingestion" doc:"Source orchestration — which connectors to run, backfill bounds, cursor store."`
	Oracle       OracleConfig       `toml:"oracle" doc:"On-chain oracle contract addresses (Reflector, Redstone, Band)."`
	External     ExternalConfig     `toml:"external" doc:"Off-chain connectors — CEX/FX/aggregator sources that run parallel to the on-chain dispatcher."`
	Aggregate    AggregateConfig    `toml:"aggregate" doc:"VWAP/TWAP windows + outlier thresholds."`
	Anomaly      AnomalyConfig      `toml:"anomaly" doc:"Per-asset-class anomaly detection thresholds (Phase 1) + Phase-2 freeze thresholds (per-asset MAD-baseline + multi-factor confidence + source count). Both layers run; either freezes on its own (Phase 1: class freeze_pct AND source_count<=1; Phase 2: the 3-signal AND) and both share one freeze lifecycle (ADR-0019)."`
	API          APIConfig          `toml:"api" doc:"Public API serving plane — port, auth mode, rate limits, CDN."`
	Metadata     MetadataConfig     `toml:"metadata" doc:"Asset metadata overlay — SEP-1 issuer→home-domain map, operator overrides."`
	Supply       SupplyConfig       `toml:"supply" doc:"Supply pipeline config — SDF reserve list, operator-managed reserve balances (fallback when the LCM AccountEntry observer hasn't yet covered the watched set), watched classic + SEP-41 asset lists, SAC wrappers, and aggregator-refresh cadence. ADR-0011 (XLM) + ADR-0022 (classic) + ADR-0023 (SEP-41)."`
	Trades       TradesConfig       `toml:"trades" doc:"Trade-insert policy — operator-declared USD-pegged stablecoins so on-chain DEX trades populate trades.usd_volume at insert time (launch-readiness L2.2 phase 1)."`
	PricingGuard PricingGuardConfig `toml:"pricing_guard" doc:"Serving-side price guards — the thin-market substance gate that withholds aggregated price claims for on-chain pairs whose trailing market activity is below the serve floor (2026-08-04 valuation incident)."`
	Divergence   DivergenceConfig   `toml:"divergence" doc:"Cross-check references the divergence service consults (CoinGecko + Chainlink HTTP, plus the on-chain Reflector/Redstone/Band oracle feeds read from ingested oracle_updates rows). Empty disables; the divergence_warning envelope flag stays unset."`
	PriceAlerts  PriceAlertsConfig  `toml:"price_alerts" doc:"Customer price-threshold alert evaluator. Off by default; when enabled the aggregator sweeps price_alerts against the latest closed VWAP every tick and enqueues price.alert webhook deliveries."`
	SignupReaper SignupReaperConfig `toml:"signup_reaper" doc:"F-1255 speculative-account reaper. Deletes orphan accounts left by a lost signup race (Suspended with a 'signup-race:' reason, no user, no key). Runs in the API binary when the dashboard is wired. On by default — the rows are pure garbage."`
	HashDB       HashDBConfig       `toml:"hashdb" doc:"Drift detector — on-disk (ledger_seq -> sha256(LCM)) record appended by the indexer's live ingest loop and periodically re-verified against a fresh re-read of the same bucket, catching upstream rewrites of previously-fetched ledger bytes. Off by default (opt-in first deploy)."`
	Obs          ObsConfig          `toml:"obs" doc:"Metrics, logs, traces — exporters + sampling."`
}

// HashDBConfig gates the hashdb drift detector (internal/hashdb); one Enabled flag covers append and verify since either alone detects nothing.
type HashDBConfig struct {
	// Enabled starts hashdb.Append in the live LCM read loop AND the periodic verify sweep. Off by default.
	Enabled bool `toml:"enabled" doc:"Start the hashdb append-on-ingest + periodic verify sweep in the indexer. Off by default — opt in per region once proven." default:"false"`

	// Path default matches the indexer's existing ReadWritePaths mount (/var/lib/stellarindex).
	Path string `toml:"path" doc:"Filesystem path of the hashdb file (ledger_seq -> sha256(LCM)). Created on first run if missing." default:"/var/lib/stellarindex/hashdb.bin"`
}

// validate enforces HashDBConfig's constraints only when Enabled; a disabled zero-value block is fine.
func (hc HashDBConfig) validate() error {
	if !hc.Enabled {
		return nil
	}
	if hc.Path == "" {
		return fmt.Errorf("hashdb: path must be set when enabled")
	}
	return nil
}

// SignupReaperConfig gates the reaper for orphan `accounts` rows left by a lost /v1/auth/callback race; on by default, starts only when the dashboard bundle is wired.
type SignupReaperConfig struct {
	// Enabled starts the reaper loop in the API binary. On by default.
	Enabled bool `toml:"enabled" doc:"Start the speculative-account reaper loop in the API binary. On by default — the reaped rows (Suspended signup-race orphans with no user/key) are pure garbage. Set false to disable." default:"true"`
}

// PriceAlertsConfig gates the aggregator's price-alert evaluator (internal/pricealerts); off by default, needs migrations 0027 and 0080.
type PriceAlertsConfig struct {
	// Enabled starts the evaluator loop in the aggregator binary.
	Enabled bool `toml:"enabled" doc:"Start the price-alert evaluator loop in the aggregator. Off by default." default:"false"`
}

// TradesConfig configures policy run at trade-insert time (timescale.Store.InsertTrade); empty config keeps the off-chain-only `usd_volume` behaviour.
type TradesConfig struct {
	// USDPeggedClassicAssets is the allow-list of classic "CODE-ISSUER" assets trusted as
	// 1.0-USD-pegged. On-chain trades quoted in one, or in its SAC (via [SupplyConfig.SACWrappers]),
	// set trades.usd_volume = quote_amount/10^7 at insert. Empty: on-chain DEX trades store NULL
	// (see the lower-bound caveat on volume_24h_usd).
	USDPeggedClassicAssets []string `toml:"usd_pegged_classic_assets" doc:"Classic credit asset_keys (CODE-ISSUER) the operator declares as USD-pegged stablecoins. On-chain DEX trades quoted in these (or their SAC wrappers, transitive via [supply.sac_wrappers]) populate trades.usd_volume at insert time. Empty preserves the off-chain-only default." default:"[]"`
}

// validate requires every declared USD peg to be a CLASSIC credit asset: usd_volume scales a peg
// by 10^7, correct only for 7-decimal classics, and anything else would mis-scale it by orders
// of magnitude. Matches timescale.NewUSDVolumeQuoteSpec's hard error.
func (tc TradesConfig) validate() error {
	for i, raw := range tc.USDPeggedClassicAssets {
		if raw == "" {
			return fmt.Errorf("%w: trades: usd_pegged_classic_assets[%d] is empty", ErrInvalidConfig, i)
		}
		asset, err := canonical.ParseAsset(raw)
		if err != nil {
			return fmt.Errorf("%w: trades: usd_pegged_classic_assets[%d] (%q): %w", ErrInvalidConfig, i, raw, err)
		}
		if asset.Type != canonical.AssetClassic {
			return fmt.Errorf(
				"%w: trades: usd_pegged_classic_assets[%d] (%q) must be a classic credit asset "+
					"in CODE-ISSUER form — the usd_volume 10^7 scaling assumes 7 decimals, which "+
					"only classic Stellar assets guarantee (got %s)",
				ErrInvalidConfig, i, raw, asset.Type)
		}
	}
	return nil
}

// USDPeggedClassics parses USDPeggedClassicAssets, logging and skipping any entry validate would reject.
func (tc TradesConfig) USDPeggedClassics(logger *slog.Logger) []canonical.Asset {
	if len(tc.USDPeggedClassicAssets) == 0 {
		return nil
	}
	out := make([]canonical.Asset, 0, len(tc.USDPeggedClassicAssets))
	for _, raw := range tc.USDPeggedClassicAssets {
		asset, err := canonical.ParseAsset(raw)
		if err != nil {
			logger.Warn("usd_pegged_classic_assets: skipping malformed entry",
				"raw", raw, "err", err)
			continue
		}
		if asset.Type != canonical.AssetClassic {
			logger.Warn("usd_pegged_classic_assets: ignoring non-classic asset",
				"raw", raw, "type", asset.Type)
			continue
		}
		out = append(out, asset)
	}
	return out
}

// PricingGuardConfig configures the thin-market substance gate (internal/pricingguard): serving paths withhold a pair with an on-chain leg unless its trailing market clears a volume and persistence floor; zero values use the package defaults.
type PricingGuardConfig struct {
	// DisableSubstanceGate switches the gate off (every pair serves): an escape hatch for
	// diagnosing a suspected false-withhold, not a tuning knob; lower the floors to loosen it.
	DisableSubstanceGate bool `toml:"disable_substance_gate" doc:"Disable the thin-market substance gate entirely (serve every pair, pre-2026-08 behaviour). Diagnostic escape hatch, not a tuning knob." default:"false"`

	// SubstanceMinVolumeUSD is the minimum trailing-window USD volume.
	SubstanceMinVolumeUSD float64 `toml:"substance_min_volume_usd" doc:"Minimum trailing-window USD volume (summed over prices_1m.volume_usd, both directions, alias union) below which an aggregated price is withheld. 0 = pricingguard default (1000)." default:"1000"`

	// SubstanceMinBuckets is the minimum count of distinct closed
	// 1-minute buckets with a trade in the window.
	SubstanceMinBuckets int `toml:"substance_min_buckets" doc:"Minimum distinct closed 1-minute buckets with at least one trade in the trailing window. 0 = pricingguard default (20)." default:"20"`

	// SubstanceMinSpanMinutes is the minimum spread between the oldest and newest active bucket
	// in the window: the cross-timeframe persistence floor.
	SubstanceMinSpanMinutes int `toml:"substance_min_span_minutes" doc:"Minimum minutes between the oldest and newest active 1m bucket in the trailing window (persistence floor — a one-burst market never clears it). 0 = pricingguard default (360)." default:"360"`

	// SubstanceWindowHours is the trailing measurement window.
	SubstanceWindowHours int `toml:"substance_window_hours" doc:"Trailing measurement window in hours. 0 = pricingguard default (24)." default:"24"`

	// FXCrossMaxAgeHours bounds the age of the forex rate the /v1/price fiat-cross fallbacks
	// (ADR-0051) may serve; the forex Cache never expires, so a stalled worker would otherwise
	// answer forever with a fresh-looking observed_at.
	FXCrossMaxAgeHours int `toml:"fx_cross_max_age_hours" doc:"Staleness budget in hours for the forex snapshot rate backing /v1/price's fiat-cross-rate and USD-anchored-fiat-cross fallbacks; older than this is refused rather than served. Mirrors aggregate.composite_reference.fx_max_age_hours (same fx_quotes staleness profile — daily buckets that pause over market closes). 0 = pricingguard default (76)." default:"76"`

	// DisableFiatBasis switches off the ADR-0053 basis rule, so a single-venue fiat book is
	// served even where a multi-venue USD leg could anchor the price. Kill switch only.
	DisableFiatBasis bool `toml:"disable_fiat_basis" doc:"Disable the ADR-0053 fiat basis rule: /v1/price and /v1/price/batch serve a single-venue direct fiat book as-is instead of the USD-anchored derivation (multi-venue USD leg × bound FX fixing). Kill switch, not a tuning knob." default:"false"`

	// FiatPeggedClassicAssets maps a classic asset_key (CODE-ISSUER) to the ISO-4217 ticker the
	// OPERATOR declares it 1:1-pegged to. Fills price_usd from fx_quotes with price_basis
	// "declared_peg" only when no market price survives the gate. Validated at load (ADR-0010).
	// The trade path reads it too: a DEX trade with a declared leg stores usd_volume off the fiat
	// rate while the leg holds within 2% of it (the usd_volume fiat-peg tier).
	FiatPeggedClassicAssets map[string]string `toml:"fiat_pegged_classic_assets" doc:"Maps classic credit asset_keys (CODE-ISSUER) to the ISO-4217 fiat ticker the operator declares them 1:1-pegged to (e.g. AUDD-G… = \"AUD\"). The API fills the asset's listing/detail price_usd from the declared peg × current fiat→USD FX rate when no market-derived price survives the substance gate, stamped price_basis=\"declared_peg\" on the wire. Never overwrites a market-derived price. The trade path reads the same map: an on-chain DEX trade with a declared leg stores usd_volume = amount/10^7 × the fiat→USD rate while that leg's own on-chain USD price holds within 2% of the rate (docs/operations/usd-volume-coverage-plan.md#sac-wrappers-and-fiat-pegged-classics). Empty disables the fill." default:"{}"`
}

// validate rejects negative floors: always a typo, and treating it as "use default" would hide it.
func (pg PricingGuardConfig) validate() error {
	if pg.SubstanceMinVolumeUSD < 0 {
		return fmt.Errorf("%w: pricing_guard: substance_min_volume_usd must be >= 0", ErrInvalidConfig)
	}
	if pg.SubstanceMinBuckets < 0 {
		return fmt.Errorf("%w: pricing_guard: substance_min_buckets must be >= 0", ErrInvalidConfig)
	}
	if pg.SubstanceMinSpanMinutes < 0 {
		return fmt.Errorf("%w: pricing_guard: substance_min_span_minutes must be >= 0", ErrInvalidConfig)
	}
	if pg.SubstanceWindowHours < 0 {
		return fmt.Errorf("%w: pricing_guard: substance_window_hours must be >= 0", ErrInvalidConfig)
	}
	if err := pg.validateSubstanceSatisfiable(); err != nil {
		return err
	}
	if pg.FXCrossMaxAgeHours < 0 {
		return fmt.Errorf("%w: pricing_guard: fx_cross_max_age_hours must be >= 0", ErrInvalidConfig)
	}
	// Fail at load: a non-classic key or unknown fiat ticker is an operator typo, and a
	// silently skipped peg would leave the asset priceless with no signal why.
	for rawAsset, ticker := range pg.FiatPeggedClassicAssets {
		asset, err := canonical.ParseAsset(rawAsset)
		if err != nil {
			return fmt.Errorf("%w: pricing_guard: fiat_pegged_classic_assets key %q: %w", ErrInvalidConfig, rawAsset, err)
		}
		if asset.Type != canonical.AssetClassic {
			return fmt.Errorf(
				"%w: pricing_guard: fiat_pegged_classic_assets key %q must be a classic credit asset "+
					"in CODE-ISSUER form (got %s)",
				ErrInvalidConfig, rawAsset, asset.Type)
		}
		if _, err := canonical.NewFiatAsset(ticker); err != nil {
			return fmt.Errorf("%w: pricing_guard: fiat_pegged_classic_assets[%q] ticker %q: %w", ErrInvalidConfig, rawAsset, ticker, err)
		}
	}
	return nil
}

// maxSubstanceWindowHours caps substance_window_hours at 400 days, far below the ~2,562,047 h at
// which time.Duration(h)*time.Hour wraps negative and the gate fails open.
const maxSubstanceWindowHours = 400 * 24

// validateSubstanceSatisfiable rejects floors no market can clear (every pair withheld) and
// windows overflowing time.Duration (every pair served unguarded). Off a minute boundary a window
// holds window*60-1 closed 1m buckets spanning window*60-2 minutes; a 0 knob is checked at its default.
func (pg PricingGuardConfig) validateSubstanceSatisfiable() error {
	def := defaultPricingGuardConfig()
	windowHours := cmp.Or(pg.SubstanceWindowHours, def.SubstanceWindowHours)
	spanMinutes := cmp.Or(pg.SubstanceMinSpanMinutes, def.SubstanceMinSpanMinutes)
	buckets := cmp.Or(pg.SubstanceMinBuckets, def.SubstanceMinBuckets)
	if windowHours > maxSubstanceWindowHours {
		return fmt.Errorf("%w: pricing_guard: substance_window_hours = %d exceeds the maximum %d (400 days)",
			ErrInvalidConfig, windowHours, maxSubstanceWindowHours)
	}
	windowMinutes := windowHours * 60
	if spanMinutes > windowMinutes-2 {
		return fmt.Errorf("%w: pricing_guard: substance_min_span_minutes (effective %d) must be at most "+
			"substance_window_hours*60-2 (effective %d) — the window's closed buckets span no more, so every pair would be withheld",
			ErrInvalidConfig, spanMinutes, windowMinutes-2)
	}
	if buckets > windowMinutes-1 {
		return fmt.Errorf("%w: pricing_guard: substance_min_buckets (effective %d) must be at most "+
			"substance_window_hours*60-1 (effective %d) closed 1-minute buckets — every pair would be withheld",
			ErrInvalidConfig, buckets, windowMinutes-1)
	}
	return nil
}

// defaultPricingGuardConfig mirrors the pricingguard.DefaultSubstance* constants (config cannot
// import pricingguard); TestDefaultPricingGuard_MatchesPricingguardConstants pins the lockstep,
// TestDefault_MatchesStructTags the `default:` doc tags.
func defaultPricingGuardConfig() PricingGuardConfig {
	return PricingGuardConfig{
		SubstanceMinVolumeUSD:   1000,
		SubstanceMinBuckets:     20,
		SubstanceMinSpanMinutes: 360,
		SubstanceWindowHours:    24,
		FXCrossMaxAgeHours:      76,
	}
}

// DivergenceConfig wires the divergence cross-check references; the AGGREGATOR refreshes them into Redis and the API only reads that cache, setting `divergence_warning` past [Threshold].
type DivergenceConfig struct {
	// Threshold is the divergence percentage above which WarningFired is true. Default 5.0.
	Threshold float64 `toml:"threshold_pct" doc:"Divergence percentage above which the warning flag fires." default:"5.0"`

	// MinSourcesForWarning is the minimum successful references before WarningFired can be true.
	// Default 2: one dissenting source is not enough.
	MinSourcesForWarning int `toml:"min_sources_for_warning" doc:"Minimum successful references before warning_fired can be true." default:"2"`

	// CoinGecko config. Always-on by default (free tier, no
	// auth). Set Enabled=false to skip.
	CoinGecko DivergenceCoinGeckoConfig `toml:"coingecko" doc:"CoinGecko reference (free tier, no auth required)."`

	// Chainlink reads AggregatorV3 latestAnswer() over public Ethereum RPC. Off by default; needs a FeedMap.
	Chainlink DivergenceChainlinkConfig `toml:"chainlink" doc:"Chainlink reference (HTTP cross-check only; not a VWAP contributor)."`

	// On-chain oracle references read our OWN ingested oracle_updates rows. On by default: no
	// external quota, and a per-pair no-op (asset_unsupported) until the feed tables hold data.
	// Reflector toggles all three variants (dex/cex/fx) together.
	Reflector DivergenceOracleConfig `toml:"reflector" doc:"Reflector on-chain oracle references (reflector-dex/cex/fx) read from ingested oracle_updates rows."`
	Redstone  DivergenceOracleConfig `toml:"redstone" doc:"Redstone on-chain oracle reference read from ingested oracle_updates rows."`
	Band      DivergenceOracleConfig `toml:"band" doc:"Band on-chain oracle reference read from ingested oracle_updates rows (relay/force_relay op-args ingest)."`

	// Supply cross-check compares OUR served circulating_supply against an external reference,
	// separate from the price cross-checks. Off by default; the archival-node role renders it from
	// stellarindex_divergence_supply_enabled.
	Supply DivergenceSupplyConfig `toml:"supply" doc:"Supply cross-check: compare our served circulating_supply against the Stellar Network Dashboard (XLM) and/or CoinGecko. Catches a stale SDF-reserve exclusion list. Off by default."`
}

// DivergenceSupplyConfig gates the supply cross-check worker; off by default (makes outbound HTTP) and needs at least one enabled reference when enabled or the worker refuses to start.
type DivergenceSupplyConfig struct {
	Enabled bool `toml:"enabled" doc:"Whether the supply cross-check worker runs. Off by default (makes outbound HTTP calls). The archival-node ansible role renders it from the stellarindex_divergence_supply_enabled inventory variable, itself default false." default:"false"`
	// ThresholdPct is the divergence percentage above which the ratio gauge reads `divergent` and
	// the alert fires. Default 1.0, far above the ~0.03% XLM Fee-Pool noise floor, so only REAL drift fires.
	ThresholdPct float64 `toml:"threshold_pct" doc:"Relative-divergence percentage above which a supply cross-check reads 'divergent' and the alert fires. Default 1.0 (well above the ~0.03% XLM noise floor)." default:"1.0"`
	// Dashboard is the Stellar Network Dashboard reference (XLM only). On by default within this
	// block, which is itself Enabled=false, so it runs only once the parent gate is flipped.
	Dashboard DivergenceSupplyDashboardConfig `toml:"dashboard" doc:"Stellar Network Dashboard reference (dashboard.stellar.org) — authoritative XLM circulating supply, free, no auth. On by default within this block."`
	// CoinGecko is the `/coins/{id}` circulating-supply reference. Off by default: the free tier
	// is 429-throttled, so enable it once a Pro key is set.
	CoinGecko DivergenceSupplyCoinGeckoConfig `toml:"coingecko" doc:"CoinGecko /coins/{id} market_data.circulating_supply reference. Off by default (free tier 429-throttled since 2026-06-19; enable with a Pro key)."`
}

// DivergenceSupplyDashboardConfig configures the Stellar Dashboard supply reference; XLM only, every other asset is asset_unsupported.
type DivergenceSupplyDashboardConfig struct {
	Enabled bool   `toml:"enabled" doc:"Whether the Stellar Dashboard supply reference is consulted. On by default within [divergence.supply]." default:"true"`
	BaseURL string `toml:"base_url" doc:"Dashboard API base. Empty defaults to https://dashboard.stellar.org/api/v3. The reference GETs base_url + /lumens." default:""`
}

// DivergenceSupplyCoinGeckoConfig configures the CoinGecko supply reference (`/coins/{id}`), distinct from [DivergenceCoinGeckoConfig].
type DivergenceSupplyCoinGeckoConfig struct {
	Enabled bool `toml:"enabled" doc:"Whether the CoinGecko supply reference is consulted. Off by default (free tier 429-throttled)." default:"false"`
	// APIKey follows the secret-field convention (env var; TOML for local dev only). Sent as the
	// x-cg-pro-api-key header, which lifts the 429 ceiling.
	APIKey  string            `toml:"api_key" doc:"CoinGecko Pro API key, sent as x-cg-pro-api-key. Prefer env var COINGECKO_API_KEY." env:"COINGECKO_API_KEY" default:""`
	BaseURL string            `toml:"base_url" doc:"CoinGecko API base. Empty defaults to https://api.coingecko.com/api/v3, or https://pro-api.coingecko.com/api/v3 when api_key is set (a Pro key 404s the public host)." default:""`
	IDMap   map[string]string `toml:"id_map" doc:"Maps canonical asset_id → CoinGecko coin id for the supply lookup. Empty falls back to the built-in default (native/crypto:XLM → stellar)." default:"{}"`
}

// DivergenceOracleConfig gates one on-chain oracle reference family reading SERVED oracle_updates rows; lookups record asset_unsupported until rows exist.
type DivergenceOracleConfig struct {
	Enabled bool `toml:"enabled" doc:"Whether this on-chain oracle reference is wired into the divergence service." default:"true"`
}

// DivergenceCoinGeckoConfig configures the CoinGecko reference; IDMap is overridable and defaults to a built-in set (XLM, major stablecoins and the served Stellar assets CoinGecko lists).
type DivergenceCoinGeckoConfig struct {
	Enabled bool              `toml:"enabled" doc:"Whether the CoinGecko reference is wired into the divergence service." default:"true"`
	BaseURL string            `toml:"base_url" doc:"CoinGecko API base URL. Empty defaults to https://api.coingecko.com/api/v3, or https://pro-api.coingecko.com/api/v3 when external.coingecko.api_key is set. The reference authenticates with the external.coingecko keys." default:""`
	IDMap   map[string]string `toml:"id_map" doc:"Maps canonical asset_id → CoinGecko slug. Operator-curated; empty falls back to the built-in default covering XLM, major stables and the served Stellar assets CoinGecko lists." default:"{}"`
}

// DivergenceChainlinkConfig configures the Chainlink reference; off by default, opt in by setting RPCURL and a FeedMap of mainnet AggregatorV3 addresses.
type DivergenceChainlinkConfig struct {
	Enabled bool   `toml:"enabled" doc:"Whether the Chainlink reference is wired into the divergence service." default:"false"`
	RPCURL  string `toml:"rpc_url" doc:"Ethereum JSON-RPC endpoint. Shares the CHAINLINK_RPC_URL env var with the ingest poller (env overrides TOML). Empty defaults to https://cloudflare-eth.com." env:"CHAINLINK_RPC_URL" default:""`
	// FeedMap maps a "<base>/<quote>" pair (e.g. "fiat:EUR/fiat:USD") to a mainnet feed address.
	// Omitted decimals adopt the feed's on-chain decimals(); a set value is verified against it.
	FeedMap map[string]ChainlinkFeedConfig `toml:"feeds" doc:"Maps pair strings to {address, decimals, invert}. Empty disables Chainlink in practice." default:"{}"`
}

// ChainlinkFeedConfig is one entry in the [DivergenceChainlinkConfig.FeedMap].
type ChainlinkFeedConfig struct {
	Address  string `toml:"address" doc:"0x-prefixed mainnet feed contract address." default:""`
	Decimals int    `toml:"decimals" doc:"Power-of-10 divisor for the raw int256. Omit to adopt the feed's on-chain decimals() (8 on every Chainlink USD feed). When set it is verified against decimals() on first use and daily; on disagreement the feed's readings are refused (ERROR log + stellarindex_chainlink_feed_decimals_mismatch_total) until they agree." default:"0"`
	Invert   bool   `toml:"invert" doc:"Set true when canonical pair is reciprocal of the feed's natural quote." default:"false"`
	// MaxAgeHours is the staleness ceiling: an older latestRoundData is rejected as
	// reference-unavailable. 0 = the pair's default budget (see the doc tag).
	MaxAgeHours int `toml:"max_age_hours" doc:"Staleness ceiling in hours for the feed's latestRoundData updatedAt; rounds older than this are rejected as reference-unavailable (CS-089). 0 = the default: the built-in feed's budget for a built-in pair, 76h for any other fiat/fiat pair (FX feeds pause over market closes), else 3h. Must be >= 0." default:"0"`
}

// defaultDivergenceConfig returns the Default() divergence settings: CoinGecko on (free, no auth)
// so divergence_warning works out of the box; Chainlink off until a FeedMap is set.
func defaultDivergenceConfig() DivergenceConfig {
	return DivergenceConfig{
		Threshold:            5.0,
		MinSourcesForWarning: 2,
		CoinGecko: DivergenceCoinGeckoConfig{
			Enabled: true,
			IDMap:   map[string]string{},
		},
		Chainlink: DivergenceChainlinkConfig{
			Enabled: false,
			FeedMap: map[string]ChainlinkFeedConfig{},
		},
		// On-chain oracle references default ON: they read our own oracle_updates rows (no external
		// quota) and no-op per pair until the feed tables have data.
		Reflector: DivergenceOracleConfig{Enabled: true},
		Redstone:  DivergenceOracleConfig{Enabled: true},
		Band:      DivergenceOracleConfig{Enabled: true},
		// Supply cross-check OFF by default (outbound HTTP). Within the block Dashboard is on, so
		// enabling the parent gives XLM-vs-Dashboard; CoinGecko stays off (free tier 429-throttled).
		Supply: DivergenceSupplyConfig{
			Enabled:      false,
			ThresholdPct: 1.0,
			Dashboard:    DivergenceSupplyDashboardConfig{Enabled: true},
			CoinGecko:    DivergenceSupplyCoinGeckoConfig{Enabled: false, IDMap: map[string]string{}},
		},
	}
}

// MetadataConfig configures the asset-metadata overlay; an observed on-chain home_domain (or observed absence) is final, so the curated issuer map only answers for unobserved issuers.
type MetadataConfig struct {
	// IssuerHomeDomains maps issuer G-strkey → home-domain (TOML: [metadata.issuer_home_domains]).
	// An empty value equals an absent key.
	IssuerHomeDomains map[string]string `toml:"issuer_home_domains" doc:"Static curated map of issuer-account G-strkey → home-domain. Fallback for issuers with no AccountEntry observation; an observed on-chain home_domain, or an observed absence of one, always wins." default:"{}"`

	// WatchedIssuerAccounts are the issuer G-strkeys whose AccountEntry the indexer observes for
	// home_domain. Kept apart from [SupplyConfig.SDFReserveAccounts], which is subtracted from XLM supply.
	WatchedIssuerAccounts []string `toml:"watched_issuer_accounts" doc:"Issuer-account G-strkeys whose AccountEntry the indexer observes so the API serves their on-chain home_domain. Independent of [supply].sdf_reserve_accounts: watching an issuer here does not change circulating supply." default:"[]"`
}

func (m MetadataConfig) validate() error {
	for i, acc := range m.WatchedIssuerAccounts {
		if !canonical.IsAccountID(acc) {
			return fmt.Errorf("metadata: watched_issuer_accounts[%d] %q is not a valid G-strkey", i, acc)
		}
	}
	return nil
}

// HomeDomainFor returns the issuer's curated home domain, or ("", false) if absent or empty.
func (m MetadataConfig) HomeDomainFor(issuer string) (string, bool) {
	if len(m.IssuerHomeDomains) == 0 {
		return "", false
	}
	h, ok := m.IssuerHomeDomains[issuer]
	if !ok || h == "" {
		return "", false
	}
	return h, true
}

// ExternalConfig controls off-chain connectors (internal/sources/external/); each venue is off by default so a fresh deployment makes no network egress, and pair lists are code, not config.
type ExternalConfig struct {
	Binance           ExternalStreamerConfig      `toml:"binance"          doc:"Binance spot WebSocket aggTrade streamer. Pair list: internal/sources/external/binance/pairs.yaml."`
	Kraken            ExternalStreamerConfig      `toml:"kraken"           doc:"Kraken v2 WebSocket trade streamer. Pair list: internal/sources/external/kraken/pairs.go."`
	Bitstamp          ExternalStreamerConfig      `toml:"bitstamp"         doc:"Bitstamp v2 WebSocket live_trades streamer. Pair list: internal/sources/external/bitstamp/pairs.go."`
	Coinbase          ExternalStreamerConfig      `toml:"coinbase"         doc:"Coinbase Exchange WebSocket matches streamer. Pair list: internal/sources/external/coinbase/pairs.go."`
	ExchangeRatesApi  ExchangeRatesApiVenueConfig `toml:"exchangeratesapi" doc:"ExchangeRatesApi.io REST poller for fiat cross-rates (Professional tier required for USD base + 1-min cadence + redistribution)."`
	CoinGecko         CoinGeckoVenueConfig        `toml:"coingecko"        doc:"CoinGecko /simple/price poller. Class=aggregator (divergence-only). Its keys are also used by the divergence price reference and the listing-sync ops command."`
	CoinMarketCap     CoinMarketCapVenueConfig    `toml:"coinmarketcap"    doc:"CoinMarketCap /v2 quotes poller. Class=aggregator. Paid API key; Standard tier ($79/mo+) for commercial redistribution."`
	CryptoCompare     CryptoCompareVenueConfig    `toml:"cryptocompare"    doc:"CryptoCompare /data/pricemultifull poller (rows stamped with upstream LASTUPDATE). Class=aggregator. Paid API key via Authorization header."`
	ECB               ExternalVenueConfig         `toml:"ecb"              doc:"European Central Bank daily FX reference rates. Class=authority_sanity (daily anchor, not VWAP). Free, no auth."`
	Chainlink         ChainlinkVenueConfig        `toml:"chainlink"        doc:"Chainlink Data Feeds via EVM JSON-RPC (Alchemy / Infura / public). Class=oracle (no VWAP contribution). Lives parallel to internal/divergence/chainlink.go which is the synchronous cross-check."`
	Tiingo            TiingoVenueConfig           `toml:"tiingo"           doc:"Tiingo end-of-day poller for the published daily NAV of the bound tokenized funds (tickers come from internal/rwa's fund bindings). Class=oracle, off-chain, no VWAP contribution; read only by the RWA reference surface. Free tier: 50 req/h, 1,000 req/day; hourly polling of 12 tickers is 288/day, 8,928/month."`
	Massive           MassiveConfig               `toml:"massive"           doc:"massive.com forex rates behind /v1/currencies, fetched every refresh_interval (default hourly) by stellarindex-api."`
	OpenExchangeRates OpenExchangeRatesConfig     `toml:"openexchangerates" doc:"Open Exchange Rates hourly USD-base board, built by stellarindex-api's forex worker when enabled. Not in the serving chain: the worker holds it but neither fetches nor serves it yet."`
	Dune              DuneConfig                  `toml:"dune"              doc:"Dune API read by the curated-rwa-sync ops command."`
}

// ExternalStreamerConfig is the toggle for credential-less WebSocket streamers; they hold an open socket so there is no PollInterval and a `poll_interval` is an unknown key.
type ExternalStreamerConfig struct {
	Enabled bool `toml:"enabled" doc:"Whether this connector runs. Off by default — no network egress until operator opts in." default:"false"`
}

// ExternalVenueConfig is the toggle for credential-less public venues that poll on a cadence (currently ECB); keyed venues use their own struct.
type ExternalVenueConfig struct {
	Enabled bool `toml:"enabled" doc:"Whether this connector runs. Off by default — no network egress until operator opts in." default:"false"`
	// PollInterval overrides the connector's default cadence; empty/zero keeps it (e.g. ECB's 6h).
	PollInterval time.Duration `toml:"poll_interval" doc:"Override the connector's built-in default poll cadence (e.g. \"12h\"). Empty/zero uses the connector default." default:""`
}

// ExchangeRatesApiVenueConfig extends the common toggle with the API-key slot and base-currency override; keep APIKey empty in TOML and set the `env:` var.
type ExchangeRatesApiVenueConfig struct {
	Enabled bool   `toml:"enabled" doc:"Whether this connector runs. Off by default." default:"false"`
	APIKey  string `toml:"api_key" doc:"ExchangeRatesApi access key. Prefer env var; TOML fallback exists for local-dev convenience." env:"EXCHANGERATESAPI_KEY" default:""`
	Base    string `toml:"base" doc:"Base currency (USD, EUR, GBP, …). Defaults to USD. Free tier locked to EUR; paid tier accepts any allow-listed fiat." default:"USD"`
}

// TiingoVenueConfig is [ExternalVenueConfig] plus the Tiingo API key.
type TiingoVenueConfig struct {
	Enabled      bool          `toml:"enabled" doc:"Whether this connector runs. Off by default — no network egress until operator opts in." default:"false"`
	PollInterval time.Duration `toml:"poll_interval" doc:"Override the connector's hourly default. One request per bound ticker per poll, so a shorter interval can exceed the free tier's 50 req/h." default:""`
	APIKey       string        `toml:"api_key" doc:"Tiingo API token, sent as 'Authorization: Token <key>', never in the URL. Required when enabled. Prefer env var." env:"TIINGO_API_KEY" default:""`
}

// CoinGeckoVenueConfig is [ExternalVenueConfig] plus CoinGecko's two key tiers; Pro wins when both are set.
type CoinGeckoVenueConfig struct {
	Enabled      bool          `toml:"enabled" doc:"Whether this connector runs. Off by default — no network egress until operator opts in." default:"false"`
	PollInterval time.Duration `toml:"poll_interval" doc:"Override the connector's built-in default poll cadence (e.g. \"120s\"). Empty/zero uses the connector default." default:""`
	APIKey       string        `toml:"api_key" doc:"CoinGecko Pro API key, sent as x-cg-pro-api-key against the pro-api host; wins over demo_api_key. Prefer env var." env:"COINGECKO_API_KEY" default:""`
	DemoAPIKey   string        `toml:"demo_api_key" doc:"CoinGecko Demo API key, sent as x-cg-demo-api-key. With api_key also empty, requests go out anonymously and are heavily 429-throttled. Prefer env var." env:"COINGECKO_DEMO_API_KEY" default:""`
}

// MassiveConfig carries the massive.com forex API key.
type MassiveConfig struct {
	APIKey string `toml:"api_key" doc:"massive.com API key. Empty still starts the forex worker, but every fetch 401s and /v1/currencies serves warming-up. Prefer env var." env:"MASSIVE_API_KEY" default:""`
}

// OpenExchangeRatesConfig carries the Open Exchange Rates app id and toggle.
type OpenExchangeRatesConfig struct {
	Enabled  bool   `toml:"enabled" doc:"Construct the Open Exchange Rates provider in stellarindex-api. Off by default; not yet consulted for serving." default:"false"`
	AppID    string `toml:"app_id" doc:"Open Exchange Rates app id, sent only in the Authorization header. Free plan: 1,000 requests/month, hourly updates, USD base only. Prefer env var." env:"OPENEXCHANGERATES_APP_ID" default:""`
	Endpoint string `toml:"endpoint" doc:"API root override. Empty uses https://openexchangerates.org/api." default:""`
}

// DuneConfig carries the Dune API key.
type DuneConfig struct {
	APIKey string `toml:"api_key" doc:"Dune API key, sent as X-Dune-API-Key. Empty makes curated-rwa-sync refuse the run and stamp its refused gauge. Prefer env var." env:"DUNE_API_KEY" default:""`
}

// CoinMarketCapVenueConfig carries the CMC Pro API auth and toggle; APIKey follows the env-override convention of the FX sources.
type CoinMarketCapVenueConfig struct {
	Enabled bool   `toml:"enabled" doc:"Whether this connector runs. Off by default." default:"false"`
	APIKey  string `toml:"api_key" doc:"CMC Pro API key, passed as X-CMC_PRO_API_KEY header. Prefer env var." env:"COINMARKETCAP_API_KEY" default:""`
}

// CryptoCompareVenueConfig carries the CryptoCompare API auth and toggle.
type CryptoCompareVenueConfig struct {
	Enabled bool   `toml:"enabled" doc:"Whether this connector runs. Off by default." default:"false"`
	APIKey  string `toml:"api_key" doc:"CryptoCompare API key, passed as 'Authorization: Apikey <KEY>'. Prefer env var." env:"CRYPTOCOMPARE_API_KEY" default:""`
}

// ChainlinkVenueConfig carries the Chainlink ingest settings; RPCUrl IS the credential (the key sits in the URL path), so treat it as a secret: blank in tracked TOML, set via env.
type ChainlinkVenueConfig struct {
	Enabled      bool                            `toml:"enabled"       doc:"Whether the Chainlink ingest poller runs. Off by default."  default:"false"`
	RPCUrl       string                          `toml:"rpc_url"       doc:"Ethereum mainnet JSON-RPC endpoint (Alchemy / Infura / public). For Alchemy this includes the API key in the URL path (.../v2/<KEY>) — treat the whole value as a secret. Prefer env var." env:"CHAINLINK_RPC_URL" default:""`
	PollInterval time.Duration                   `toml:"poll_interval" doc:"Override the default 30s poll cadence. Empty/zero uses the package default." default:""`
	FeedMap      map[string]ChainlinkFeedSetting `toml:"feed_map"      doc:"Maps canonical pair string ('crypto:BTC/fiat:USD' etc.) to the AggregatorV3 contract address + decimals + invert + max_age_hours. Empty falls back to the built-in default covering BTC/ETH/LINK/EUR/GBP/JPY vs USD." default:"{}"`
}

// ChainlinkFeedSetting is one entry in [ChainlinkVenueConfig.FeedMap], mirroring [DivergenceChainlinkConfig]'s feed as its own type so the schemas can evolve apart.
type ChainlinkFeedSetting struct {
	Address  string `toml:"address"  doc:"0x-prefixed AggregatorV3 contract address on Ethereum mainnet."`
	Decimals uint8  `toml:"decimals" doc:"Power-of-10 divisor for the raw int256 answer. Omit to adopt the feed's on-chain decimals() (8 on every Chainlink USD feed). When set it is verified against decimals() on the first poll and daily; on disagreement the feed is refused (ERROR log + stellarindex_chainlink_feed_decimals_mismatch_total) until they agree." default:"0"`
	Invert   bool   `toml:"invert"   doc:"If true, the canonical pair is the reciprocal of the feed's natural quote — e.g. operator wants USD/EUR but the feed publishes EUR/USD. price → 1/price after scaling." default:"false"`
	// MaxAgeHours is the live-poll staleness budget; 0 = the pair's default.
	MaxAgeHours int `toml:"max_age_hours" doc:"Staleness budget in hours, set from the feed's heartbeat: a latestRoundData round whose updatedAt is older is refused (not written) and counted on stellarindex_chainlink_feed_polls_total{outcome=\"stale\"}. 0 = the default: the built-in feed's budget for a built-in pair (3h for the 1h-heartbeat crypto feeds, 76h for the 24h-heartbeat FX feeds, which pause over market closes), 76h for any other fiat/fiat pair, else 3h. Negative is rejected at startup. Backfill is not subject to it." default:"0"`
}

// OracleConfig gathers on-chain oracle contract addresses; each provider nests its own sub-struct, e.g. [oracle.reflector].
type OracleConfig struct {
	Reflector ReflectorOracleConfig `toml:"reflector" doc:"Reflector oracle contract addresses per variant (DEX / CEX / FX)."`
	Redstone  RedstoneOracleConfig  `toml:"redstone"  doc:"RedStone Adapter contract address (single adapter owns every feed)."`
	Band      BandOracleConfig      `toml:"band"      doc:"Band Protocol StandardReference contract address (Soroban-native, emits no events — observed via InvokeContract call args)."`
	Soroswap  SoroswapConfig        `toml:"soroswap"  doc:"Soroswap factory contract — used at boot to seed the pair→tokens registry via stellar-rpc view calls. Not required for live ingest, but without it the decoder skips swaps from pairs created before the first processed ledger."`

	StalenessOverrides []OracleStalenessOverrideConfig `toml:"staleness_overrides" doc:"Per-(source, asset) exceptions to the oracle-staleness budget the stellarindex_oracle_stale alert reads. Empty (default) leaves every asset on its source's default budget — 10 × the source's declared resolution. Each row is a written-down claim that ONE asset publishes on a different rhythm than its source's cadence; see OracleStalenessOverrideConfig." default:"[]"`
}

// OracleStalenessOverrideConfig replaces the staleness budget (default 10 x the source's resolution) for one (source, asset) pair that publishes only on moves; not a mute, a real outage still tickets past the wider bound.
type OracleStalenessOverrideConfig struct {
	Source        string `toml:"source" doc:"Oracle source name exactly as it appears in the metric's source label: an on-chain oracle (reflector-dex, reflector-cex, reflector-fx, redstone, band) or an oracle_updates poller (chainlink, coingecko, coinmarketcap, cryptocompare, ecb, exchangeratesapi)."`
	Asset         string `toml:"asset" doc:"Canonical asset identifier exactly as it appears in the metric's asset label — \"crypto:DAI\", not \"DAI\". Oracle symbols pass through canonical.MapOracleSymbol (known fiat → fiat:CODE, known crypto → crypto:CODE, known RWA → rwa:CODE, anything else → raw:SYMBOL), so the label is the mapped form; a bare or non-round-tripping identifier is rejected at startup rather than silently matching no series."`
	BudgetSeconds int    `toml:"budget_seconds" doc:"Seconds this pair may go without a publication before the alert tickets, replacing the source default (10 × declared resolution). Must be > 0."`
	Reason        string `toml:"reason" doc:"Why this pair's publication rhythm differs from its source's cadence, with the observation behind the number. Required — an override without a stated reason is indistinguishable from a silenced alert."`
}

// ReflectorOracleConfig carries the three Reflector contract addresses; an empty variant is disabled and buildSources rejects an enabled source with no address.
type ReflectorOracleConfig struct {
	DEXContract string `toml:"dex_contract" doc:"Reflector DEX contract (C-prefix) on mainnet."`
	CEXContract string `toml:"cex_contract" doc:"Reflector CEX contract (C-prefix) on mainnet."`
	FXContract  string `toml:"fx_contract"  doc:"Reflector FX contract (C-prefix) on mainnet."`
	DEXDecimals uint8  `toml:"dex_decimals" doc:"Price scale (power of 10) of the DEX contract's SEP-40 decimals(). 0 keeps the Reflector default of 14. Not read from the contract: confirm decimals() on-chain before re-pointing dex_contract." default:"0"`
	CEXDecimals uint8  `toml:"cex_decimals" doc:"Price scale (power of 10) of the CEX contract's SEP-40 decimals(). 0 keeps the Reflector default of 14. Not read from the contract: confirm decimals() on-chain before re-pointing cex_contract." default:"0"`
	FXDecimals  uint8  `toml:"fx_decimals"  doc:"Price scale (power of 10) of the FX contract's SEP-40 decimals(). 0 keeps the Reflector default of 14. Not read from the contract: confirm decimals() on-chain before re-pointing fx_contract." default:"0"`
}

// RedstoneOracleConfig carries the mainnet RedStone Adapter address; the per-feed contracts are event-less proxies, all activity is on the single Adapter.
type RedstoneOracleConfig struct {
	AdapterContract string `toml:"adapter_contract" doc:"RedStone Adapter contract (C-prefix) on mainnet — CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG."`
}

// BandOracleConfig carries the mainnet Band StandardReference address; the contract emits no events, so we observe `relay()` / `force_relay()` InvokeContract calls.
type BandOracleConfig struct {
	StandardReferenceContract string `toml:"standard_reference_contract" doc:"Band Protocol StandardReference contract (C-prefix) on mainnet — CCQXWMZVM3KRTXTUPTN53YHL272QGKF32L7XEDNZ2S6OSUFK3NFBGG5M."`
}

// SoroswapConfig carries the factory address plus an optional stellar-rpc endpoint that seeds the pair registry at boot; empty FactoryContract disables it.
type SoroswapConfig struct {
	FactoryContract string `toml:"factory_contract" doc:"Soroswap factory contract (C-prefix) on mainnet — CA4HEQTL2WPEUYKYKCDOHCDNIV4QHNJ7EL4J4NQ6VADP7SYHVRYZ7AW2."`
	SeedRPCEndpoint string `toml:"seed_rpc_endpoint" doc:"stellar-rpc URL used for the boot-time factory sweep. Any public pubnet endpoint works (e.g. https://mainnet.sorobanrpc.com). Falls back to stellar.rpc_endpoints[0] when empty."`
}

// RegionConfig identifies the region this node belongs to, to tag metrics and decide replication direction.
type RegionConfig struct {
	ID string `toml:"id" doc:"Short region identifier, lowercase (r1/r2/r3)." default:"r1"`
	// Deployment is the tier label /v1/status reports as `region.deployment`. Default "production" is
	// pubnet's wire value; test-net inventories set "testnet"/"futurenet" so the status page does not tag them PRODUCTION.
	Deployment string `toml:"deployment" doc:"Deployment tier this node reports in /v1/status's region block (production / testnet / futurenet / staging). Purely a label — nothing keys behaviour off it." default:"production"`
}

// StellarConfig points a binary at the stellar-core and stellar-rpc endpoints it reads; empty disables the client.
type StellarConfig struct {
	Network           string   `toml:"network" doc:"Network passphrase name — pubnet / testnet / futurenet." default:"pubnet"`
	RPCEndpoints      []string `toml:"rpc_endpoints" doc:"stellar-rpc endpoints for getEvents/getLedgers. Tried in order on failover. Default is a local, unkeyed node; a hosted third-party endpoint may embed an API key in the URL path/query — treat that value as a secret, same as chainlink's rpc_url." default:"[\"http://127.0.0.1:8000\"]"`
	HistoryArchiveURL string   `toml:"history_archive_url" doc:"Public history archive (SDF or ours) for backfill catchup." default:"https://history.stellar.org/prd/core-live/core_live_001"`

	// SorobanGenesisLedger and MovementsFloorLedger default to the pubnet protocol-transition ledgers.
	// A reset test net is post-Soroban and post-P23 from ledger 1, so both must be 1 there or the SEP-41
	// supply observer and CAP-67 movements feed floor above every ledger and produce nothing.
	SorobanGenesisLedger uint32 `toml:"soroban_genesis_ledger" doc:"Soroban (protocol-20) activation ledger — the pre-Soroban↔Soroban-era boundary the SEP-41 supply observer floors at. Defaults to the pubnet value; set to 1 (genesis) on testnet/futurenet." default:"50457424"`
	MovementsFloorLedger uint32 `toml:"movements_floor_ledger" doc:"P23 / CAP-67 boundary — at/above it the Postgres SEP-41 movements tail serves, below it the ClickHouse pre-P23 archive serves (ADR-0048 D5). Defaults to the pubnet value; set to 2 (ledger 1 has no predecessor) on testnet/futurenet." default:"58762517"`
}

// APIMaxHandlerBudget is the longest per-handler timeout; [APIConfig.validate] requires api.request_timeout to exceed it, and it is declared here because config must not import internal/api.
const APIMaxHandlerBudget = 12 * time.Second

// Well-known Stellar network passphrases, aliased from internal/canonical so Passphrase() cannot
// drift from the value leaf packages switch on (canonical keeps them local to avoid pulling the SDK).
const (
	pubnetPassphrase    = canonical.PubnetPassphrase
	testnetPassphrase   = canonical.TestnetPassphrase
	futurenetPassphrase = canonical.FuturenetPassphrase
)

// Passphrase translates the short network name (pubnet / testnet / futurenet) into the full passphrase, or "" for unknown values, which Validate rejects at startup.
func (s StellarConfig) Passphrase() string {
	switch s.Network {
	case "pubnet":
		return pubnetPassphrase
	case "testnet":
		return testnetPassphrase
	case "futurenet":
		return futurenetPassphrase
	}
	return ""
}

// StorageConfig captures every persistent-store connection; DSNs NEVER embed passwords, use the `env:` tag to reference a secret.
type StorageConfig struct {
	PostgresDSN string `toml:"postgres_dsn" doc:"Postgres DSN; password resolved via env: prefix." env:"STELLARINDEX_POSTGRES_DSN" default:"postgres://stellarindex@127.0.0.1:5432/stellarindex?sslmode=disable"`
	RedisAddr   string `toml:"redis_addr" doc:"Redis master address host:port. Used when redis_sentinel_addrs is empty (single-node / direct mode). When sentinel addrs are set, this is ignored." default:"127.0.0.1:6379"`
	// Sentinel mode: a non-empty redis_sentinel_addrs makes the client use go-redis FailoverClient
	// (ADR-0024, the production topology); redis_addr is the fallback for dev/single-node.
	RedisSentinelAddrs []string `toml:"redis_sentinel_addrs" doc:"List of Sentinel host:port addresses. Non-empty enables FailoverClient mode (production HA per ADR-0024); empty falls back to single-node redis_addr." default:"[]"`
	RedisMasterName    string   `toml:"redis_master_name" doc:"Sentinel master name as set in inventory (e.g. stellarindex-r1-cache). Required when redis_sentinel_addrs is non-empty." default:""`
	RedisPassword      string   `toml:"redis_password" doc:"The Redis password itself, NOT an env-var NAME to dereference — inject it via the STELLARINDEX_REDIS_PASSWORD env override (the project's standard secret path) rather than writing it to this file. The old key redis_password_env is a deprecated alias (accepted with a boot warning). Used as both requirepass (client auth) and SentinelPassword (sentinel auth) — same secret per the role." env:"STELLARINDEX_REDIS_PASSWORD" default:""`
	RedisUsername      string   `toml:"redis_username" doc:"Optional Redis ACL username. Empty (default) AUTHs as Redis's legacy 'default' user — same wire shape as redis_password alone. Set to 'stellarindex' (or the operator's per-component user) when redis_acl_lockdown is enabled in the ansible role (F-1213 audit-2026-05-12); without a username the broker-side ACL rejects the connection." default:""`
	S3Endpoint         string   `toml:"s3_endpoint" doc:"S3-compatible object-store endpoint (MinIO / AWS S3)." default:"http://127.0.0.1:9000"`
	S3Region           string   `toml:"s3_region" doc:"S3 region label (free-form for MinIO; AWS region name otherwise)." default:"r1"`
	S3BucketArchive    string   `toml:"s3_bucket_archive" doc:"Immutable history-archive bucket name." default:"galexie-archive"`
	S3BucketLive       string   `toml:"s3_bucket_live" doc:"Live Galexie export bucket name." default:"galexie-live"`
	// These hold the NAME of the env var carrying the credential, NOT the credential: buildS3Client
	// does os.Getenv(S3AccessKeyEnv). They have NO `env:` tag on purpose, since ApplyEnvOverrides would
	// overwrite the name with the secret and os.Getenv(secret) returns "".
	S3AccessKeyEnv string `toml:"s3_access_key_env" doc:"NAME of the env var holding the S3 access key ID (the value lives in that env var, not here)." default:"STELLARINDEX_S3_ACCESS_KEY"`
	S3SecretKeyEnv string `toml:"s3_secret_key_env" doc:"NAME of the env var holding the S3 secret access key (the value lives in that env var, not here)." default:"STELLARINDEX_S3_SECRET_KEY"`

	// Cold-tier LCM reads (ADR-0027): when S3ColdBucketArchive is set, reads cascade hot MinIO → this
	// READ-ONLY bucket; enable only with the first bulk trim. Region must be us-east-2 (path style; us-east-1 301s).
	// The *_key_env pair holds env-var NAMES; resolve via pipeline.NewColdDataStore, never datastore.NewDataStore (its ambient AWS chain would send MinIO's keys to AWS).
	S3ColdEndpoint      string `toml:"s3_cold_endpoint" doc:"Cold-tier S3 endpoint — must be the REGIONAL endpoint (the client is path-style). Empty disables tiering. Production (aws-public-blockchain): https://s3.us-east-2.amazonaws.com" default:""`
	S3ColdRegion        string `toml:"s3_cold_region" doc:"Cold-tier S3 region. Production (aws-public-blockchain): us-east-2 (verified 2026-07-25 — us-east-1 is wrong and 301s)" default:""`
	S3ColdBucketArchive string `toml:"s3_cold_bucket_archive" doc:"Cold-tier bucket + prefix for historical LCMs. Empty disables tiering. Production: aws-public-blockchain/v1.1/stellar/ledgers/pubnet" default:""`
	S3ColdAccessKeyEnv  string `toml:"s3_cold_access_key_env" doc:"NAME of the env var holding the cold-tier S3 access key (the value lives in that env var, not here). Empty = anonymous reads, which is correct for the public aws-public-blockchain bucket. Must be set together with s3_cold_secret_key_env." env:"" default:""`
	S3ColdSecretKeyEnv  string `toml:"s3_cold_secret_key_env" doc:"NAME of the env var holding the cold-tier S3 secret key (the value lives in that env var, not here). Empty = anonymous reads. Must be set together with s3_cold_access_key_env; a named-but-unset env var is a startup error, never a silent downgrade to anonymous." env:"" default:""`

	// ClickHouse Tier-1 lake (ADR-0034). ClickHouseLiveSink writes each ledger's structural extract to
	// ClickHouse inline (non-blocking: drops under CH pressure; the ~10-min ch-live-catchup timer backstops).
	// ON by default per ADR-0041: the lake is substrate, so a deployment without ClickHouse opts OUT.
	ClickHouseAddr     string `toml:"clickhouse_addr" doc:"ClickHouse native address host:port for the Tier-1 lake (ADR-0034); used by the indexer real-time dual-sink." default:"127.0.0.1:9300"`
	ClickHouseLiveSink bool   `toml:"clickhouse_live_sink" doc:"Enable the real-time ClickHouse dual-sink: the indexer writes each ledger's structural extract to CH inline (non-blocking), keeping the lake within ~seconds of the chain. ON by default (ADR-0041): the certified-lake substrate backs the coverage claim, the CH completeness path, and lake-derived supply — opt out only on deployments that cannot run ClickHouse, accepting the loss of all three." default:"true"`
	// ClickHouseProjectorSource (ADR-0041): when true the projector reads forward events from the CH
	// lake's contract_events instead of the Postgres soroban_events landing zone. Requires the dual-sink
	// (ClickHouseLiveSink); ON by default, turn it off wherever the dual-sink is off.
	ClickHouseProjectorSource bool `toml:"clickhouse_projector_source" doc:"Feed-switch: the projector reads forward events from the ClickHouse lake (contract_events) instead of Postgres soroban_events, enabling soroban_events decommission. Requires clickhouse_live_sink. ON by default (ADR-0041), matching the production topology." default:"true"`

	// ClickHouseServingUser isolates the API's per-request CH reads (ADR-0048 D4): when set they
	// authenticate as this user under the bounded `api_serving` profile
	// (configs/ansible/roles/archival-node/tasks/20-clickhouse-serving-profile.yml). Both empty is safe before the profile exists.
	ClickHouseServingUser string `toml:"clickhouse_serving_user" doc:"ClickHouse username the API's serving reads (explorer endpoints, incl. GET /v1/accounts/{g}/movements) authenticate as (ADR-0048 D4). Empty (default) uses the environment's identity: STELLARINDEX_CLICKHOUSE_LIVE_USER when set, else ClickHouse's default user." default:""`
	// ClickHouseServingPassword holds the resolved password (direct-value `env:` convention, as
	// RedisPassword), NOT an env-var NAME like S3AccessKeyEnv.
	ClickHouseServingPassword string `toml:"clickhouse_serving_password" doc:"The ClickHouse serving user's password itself, NOT an env-var NAME to dereference — inject it via the STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD env override rather than writing it to this file. The old key clickhouse_serving_password_env is a deprecated alias (accepted with a boot warning). Empty (default) uses no password, matching an empty clickhouse_serving_user." env:"STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD" default:""`
}

// ColdTieringEnabled reports whether the cold-tier read path is wired: the presence of the cold-bucket field is the flag, so unset is the safe default.
func (s StorageConfig) ColdTieringEnabled() bool {
	return s.S3ColdBucketArchive != ""
}

// IngestionConfig controls the indexer's source fleet.
type IngestionConfig struct {
	EnabledSources     []string `toml:"enabled_sources" doc:"List of source connector names to run on this indexer replica. See config.KnownSources for valid values." default:"[\"soroswap\",\"aquarius\",\"phoenix\"]"`
	BackfillFromLedger uint32   `toml:"backfill_from_ledger" doc:"Earliest ledger to backfill from; 0 = continue-from-persisted-cursor." default:"0"`

	// LiveSeamLedger is the first ledger in the live bucket (galexie-live); lower ledgers are read from
	// galexie-archive, in order, when from < seam. Set it to galexie-append.sh's --start from the first
	// galexie.service run (r1: check the process args). 0 = no seam; indexer reads only galexie-live.
	LiveSeamLedger uint32 `toml:"live_seam_ledger" doc:"First ledger in the live bucket. Below this, indexer reads from galexie-archive. 0 disables the archive bucket entirely." default:"0"`

	// Projector is the ADR-0032 projection loop. Enabled, it runs beside the dispatcher's
	// per-source sinks (Phase 3) unless PersistPerSource=false makes it sole writer (Phase 4).
	Projector ProjectorConfig `toml:"projector" doc:"ADR-0032 projector — tails soroban_events and writes per-source rows. Phase 3 runs in parallel with the dispatcher's existing per-source sinks; Phase 4 will flip it primary."`
}

// ProjectorConfig governs the ADR-0032 projection loop; Enabled=true with PersistPerSource=false makes the dispatcher skip projected events, and Phase 4 is refused until every aggregate lookback covers ProjectorStallBound.
type ProjectorConfig struct {
	Enabled          bool `toml:"enabled"            doc:"Master switch. When false the projector goroutines are not started." default:"false"`
	PersistPerSource bool `toml:"persist_per_source" doc:"When false (Phase 4+), the dispatcher's events-goroutine skips Soroban-derived events so the projector is sole writer. Requires Enabled=true. Defaults true (Phase 3 parallel mode); flipping it to false needs more than low projector lag: the indexer refuses to start in that mode unless every continuous aggregate's refresh start_offset covers the projector's stall bound (pipeline.VerifySoleWriterCAGGCoverage), because rows the projector delivers late are otherwise never materialized. Sources whose spec sets SoleWriter (internal/pipeline/source_spec.go) are exempt — the projector is always their sole writer." default:"true"`
}

// AnomalyConfig configures both ADR-0019 anomaly phases (per-class thresholds, per-asset MAD baseline), consulted at bucket close; either can freeze the VWAP on its own.
type AnomalyConfig struct {
	// Enabled gates anomaly checks; when false every bucket is published as-is. Off by default during
	// roll-out to avoid surprise freezes; flip on once the operator has classified all assets.
	Enabled bool `toml:"enabled" doc:"Master switch. When false, anomaly checks are disabled and every bucket is published as-is. Flip to true after operator has classified the asset set." default:"false"`

	// Thresholds maps asset class → (warn_pct, freeze_pct); each row needs 0 < warn_pct < freeze_pct.
	// Empty or partial maps fall back to anomaly.DefaultThresholds(); a `default` entry (for unclassified
	// assets) is required and filled from package defaults if omitted. TOML: [anomaly.thresholds.<class>].
	Thresholds map[string]AnomalyThreshold `toml:"thresholds" doc:"Per-class threshold table. Keys are asset class names (stablecoin/treasury/crypto/governance/default). Empty falls back to package defaults; partial maps merge over defaults. The default row is required (loader fills it from package defaults if absent)." default:"{}"`

	// Classifications maps a canonical asset_id (canonical.Asset.String()) to its asset class; unlisted ones
	// fall through to ClassDefault. A class set on one alias spelling (native, crypto:XLM, a SAC twin) covers
	// all of them; two spellings of one asset with different classes fail aggregator boot.
	Classifications map[string]string `toml:"classifications" doc:"Operator-curated map of canonical asset_id → asset class (stablecoin/treasury/crypto/governance). Anything absent falls through to the default class." default:"{}"`

	// Phase2 holds the tunable thresholds for the ADR-0019 Phase 2 freeze (3-signal AND on
	// confidence, z and source count). Overrides merge atop the package defaults.
	Phase2 Phase2FreezeConfig `toml:"phase2" doc:"Phase 2 (per-asset baseline) freeze thresholds. All three conditions must hold for a freeze: confidence < 0.45 AND z > 5.0 AND sources <= 1. The confidence bound is 0.45 rather than ADR-0019's original 0.10 because confidence decays gently in z — at 0.10 the freeze needed z ~= 15 (a ~30% single-bucket move for XLM) and was effectively dormant. 0.45 puts the trigger just past the ADR's own z > 5. See DefaultPhase2ConfidenceMaxFreeze for the measured curve."`
}

// Phase2FreezeConfig exposes the ADR-0019 Phase 2 freeze thresholds; all three conditions must hold for a freeze, and defaults match phase2_freeze.go.
type Phase2FreezeConfig struct {
	ConfidenceMaxFreeze  float64 `toml:"confidence_max_freeze" doc:"Freeze fires when confidence is strictly less than this. 0.45 (operator decision 2026-07-25, coupled with the COR-14 fix); ADR-0019 originally said 0.10, which in practice required z ~= 15 and never fired." default:"0.45"`
	ZScoreMinFreeze      float64 `toml:"z_score_min_freeze" doc:"Freeze fires when z-score is strictly greater than this. ADR-0019 default 5.0 (the documented 5σ trigger)." default:"5.0"`
	SourceCountMaxFreeze int     `toml:"source_count_max_freeze" doc:"Freeze fires when source count is at or below this. ADR-0019 default 1 (single-source pattern)." default:"1"`

	// Freeze DURATION (ADR-0019): the fields above decide WHETHER a bucket freezes; these decide how long it
	// holds, when it re-evaluates and what ends it. Firing too readily serves a stale price, holding too
	// briefly serves a manipulated one. Durations are minutes; 0 = the ADR default.
	InitialHoldMinutes int `toml:"initial_hold_minutes" doc:"Minimum freeze duration for a pair that had a corroborating lens (a checked triangulation composite or a trusted cross-oracle reading) in the bucket that froze it. ADR-0019 §'Freeze duration': 30 minutes. 0 = use the default." default:"30"`

	UncorroboratedInitialHoldMinutes int `toml:"uncorroborated_initial_hold_minutes" doc:"Minimum freeze duration for a pair with NO corroborating lens at all — one venue and its own history. Deliberately shorter than ADR-0019's flat 30 min: that population is where false freezes concentrate, and a false freeze bills the customer 100% of its duration in stale last-known-good price. Must stay comfortably above the longest window that can carry a spike so a one-bucket spike cannot be waited out. 0 = use the default." default:"10"`

	ExtensionMinutes int `toml:"extension_minutes" doc:"Added to the hold at each expiry the freeze has not earned its auto-unfreeze at. ADR-0019: 30 minutes. 0 = use the default." default:"30"`

	MaxExtensions int `toml:"max_extensions" doc:"Extensions granted before the freeze escalates to operator review (a P1 alert) and stops auto-unfreezing. ADR-0019: 4, i.e. 2 hours of extensions on top of the initial hold. 0 = use the default." default:"4"`

	UnfreezeConfidenceMin float64 `toml:"unfreeze_confidence_min" doc:"Auto-unfreeze requires confidence strictly ABOVE this. ADR-0019 §'Auto-unfreeze trigger': 0.30. Note the deliberate hysteresis against confidence_max_freeze (0.45) — a freeze must not be released by the same score band that would still fire it. 0 = use the default." default:"0.30"`

	UnfreezeZScoreMax float64 `toml:"unfreeze_z_score_max" doc:"Auto-unfreeze requires z strictly BELOW this. ADR-0019: 3.0, against the 5.0 fire threshold — the gap is the hysteresis band that stops a freeze flapping on a signal hovering at the trigger. 0 = use the default." default:"3.0"`

	UnfreezeBuckets int `toml:"unfreeze_buckets" doc:"How many CONSECUTIVE buckets must meet both auto-unfreeze conditions before the freeze ends. ADR-0019: 2. A bucket that cannot be scored at all resets the count. 0 = use the default." default:"2"`
}

// NOTE — the *Minutes fields are ints, not a `freeze.Policy`, because this package must not import
// internal/aggregate/freeze (freeze → obs → config: a cycle). cmd/stellarindex-aggregator/main.go
// maps them onto orchestrator.Phase2Thresholds.Lifecycle.

// AnomalyThreshold is one row of the anomaly threshold table, mirroring `anomaly.Thresholds` in TOML-friendly types.
type AnomalyThreshold struct {
	WarnPct   float64 `toml:"warn_pct" doc:"Deviation above this percentage triggers ActionWarn: the bucket is still published, and the warning is recorded operator-side via stellarindex_anomaly_warn_total plus a Warn log. It does NOT set the divergence_warning wire flag, which belongs to the cross-reference divergence service." default:"30.0"`
	FreezePct float64 `toml:"freeze_pct" doc:"Deviation above this percentage triggers ActionFreeze when source_count<=1 (don't publish; serve last-known-good)." default:"75.0"`
}

// AggregateConfig controls the aggregator's VWAP/TWAP computation.
type AggregateConfig struct {
	MinUSDVolume float64 `toml:"min_usd_volume" doc:"Per-pair minimum USD volume within the window for VWAP eligibility." default:"10000"`
	//floatmoney:ok operator-set TOML config constant (compile-time knob, not a stored/served amount) — capExceedsObservedTurnover/dustLiquiditySuppressed (internal/api/v1/assets.go) compare it via big.NewFloat against ADR-0003 decimal-string money, never accumulated as float
	MinMarketCapVolumeUSD float64 `toml:"min_market_cap_volume_usd" doc:"Valuation-integrity floor (USD): a market cap / FDV is SUPPRESSED (served null with market_cap_low_liquidity=true) when its backing price came from a single venue AND the asset's trailing-24h USD volume is below this floor. The AND is load-bearing — a single-venue asset with real volume, or any multi-source asset, keeps its cap. Stops one dust trade ('0.00001 of an asset for $10') presenting an obscure asset as worth billions. 0 disables the guard." default:"1000"`
	//floatmoney:ok operator-set TOML config constant, a dimensionless turnover-ratio ceiling (days-to-turn-over multiple) despite the "cap" substring — same non-money class as capExceedsObservedTurnover's maxRatio parameter (internal/api/v1/assets.go)
	MaxMarketCapVolumeRatio      float64                    `toml:"max_market_cap_volume_ratio" doc:"Valuation-integrity ceiling: a market cap / FDV is SUPPRESSED (served null with market_cap_low_liquidity=true) when the computed figure exceeds this multiple of the asset's own trailing-24h USD volume. Read it as days-to-turn-over — a cap of N x volume is the number of days the whole float would take to change hands once at the observed rate. The absolute floor beside it (min_market_cap_volume_usd) cannot see this case: it asks whether trading is small, and an asset can clear it with real four-figure volume while still claiming a cap nine orders of magnitude larger. Measured on pubnet 2026-09-15 the served set separated cleanly at this line: every recognised asset sat at or below 2,856x (7.8 years) and two vanity mints from one domain sat at 826,462x and 928,117x (2,300-2,500 years), together publishing $5.89B of the surface's headline total on $6,759 of combined daily volume. 0 disables the guard." default:"50000"`
	OutlierSigmaThreshold        float64                    `toml:"outlier_sigma_threshold" doc:"Reject a trade whose price is more than N robust scales from EVERY reference it is scored against — the whole window's MEDIAN and its time-local neighbourhood's (own/adjacent 1-minute buckets, or the nearest prints for thin series) — before VWAP. Implementation is a masking-resistant MEDIAN + 1.4826×MAD scale (internal/aggregate/outliers_local.go), NOT mean+stdev; an agreed regime shift survives, a lone wild print does not. 0 disables it and fewer than 3 valid prices is a no-op. The centre is per-print (one price per trade), but a trim that would keep less base volume than it drops withholds the window instead of publishing the remainder, so neither a count majority of dust nor one large print sets the price alone." default:"4"`
	TriangulationEnabled         bool                       `toml:"triangulation_enabled" doc:"Master switch for the post-refresh triangulation pass. When true (default), the aggregator runs each aggregate.triangulations chain × window after the per-pair refresh, multiplying the leg VWAPs and writing the implied target price. When false, the pass is skipped entirely regardless of aggregate.triangulations entries — an operator-side kill-switch for the triangulation feature without having to clear the chain table." default:"true"`
	IntervalSeconds              int                        `toml:"interval_seconds" doc:"Tick cadence — gap between successive (pair, window) refresh passes. 0 falls back to the library default (30s)." default:"30"`
	DivergenceMinIntervalSeconds int                        `toml:"divergence_min_interval_seconds" doc:"Minimum wall-clock seconds between divergence-refresh passes. Tick still fires at interval_seconds, but the divergence pass is skipped if elapsed < this value. Default 300s burns ~10× less of the CMC monthly-quota than every-tick refreshes (F-0030 follow-up); the div:<base>/<quote> key TTL is sized from this cadence plus a worst-case pass, so it stays populated between passes at any value. Set to 0 to refresh every tick (legacy)." default:"300"`
	MaxTradesPerWindow           int                        `toml:"max_trades_per_window" doc:"Per-(pair, window) cap on TradesInRange row count to bound a runaway scan. 0 falls back to the library default (10000)." default:"10000"`
	DisableClassFilter           bool                       `toml:"disable_class_filter" doc:"Disable the default ClassExchange-only VWAP filter so every fetched trade contributes regardless of source class. Off by default — see internal/sources/external/registry.go for class semantics." default:"false"`
	ExcludedSources              []string                   `toml:"excluded_sources" doc:"Source names whose already-stored trades are dropped from every aggregator VWAP window at read time (before the class filter). The read-side kill-switch: [external.<venue>] enabled=false only stops NEW ingest, this removes the source's existing rows from the computed price without a purge. The prices_1m continuous aggregate is not source-filtered." default:"[]"`
	EnableStablecoinFiatProxy    bool                       `toml:"enable_stablecoin_fiat_proxy" doc:"Expand fiat-denominated target pairs to include stablecoin backers (XLM/fiat:USD also pulls XLM/USDT/USDC/DAI/PYUSD/USDP and collapses onto the target). Off by default — N+1 TradesInRange calls per (pair, window)." default:"false"`
	Pairs                        []string                   `toml:"pairs" doc:"Aggregator coverage set as canonical pair strings (\"crypto:XLM/fiat:USD\", \"native/USDC-G…\"). Empty leaves the binary's built-in default (XLM/BTC/ETH × USD/EUR/GBP). Each entry is parsed via canonical.ParseAsset on both sides; an unparseable entry fails Validate." default:"[]"`
	Windows                      []string                   `toml:"windows" doc:"Per-window cadences as Go time.Duration strings (\"5m\", \"1h\", \"24h\"). Empty leaves the orchestrator's built-in default ([5m, 1h, 24h])." default:"[]"`
	Triangulations               []TriangulationChainConfig `toml:"triangulations" doc:"Operator-configured chain pricing entries — each row defines a target pair plus an ordered chain of leg pairs. After the per-pair refresh runs, the orchestrator prices each target via the graph-based cross-rate router (internal/aggregate/router.go) over the edge set built from this tick's priced-pair VWAPs plus the resolved chain legs, and writes the implied target VWAP to its own cache key. Empty (default) skips triangulation entirely." default:"[]"`
	MaxHops                      int                        `toml:"max_hops" doc:"Maximum LEGS in a router cross-rate route (base→hub→quote is 2 legs). Bounds the graph search and the composite chain length. 0 falls back to the library default (3); values are accepted only in [2,4] (4 covers the obscure×obscure worst case). Applies to the triangulation targets priced via the router." default:"3"`
	MinRouteConfidence           float64                    `toml:"min_route_confidence" doc:"Confidence floor in [0,1] a router route's weakest-link edge must clear to back a composite as CONFIDENT. Routes below it are excluded so a dust/thin edge can't set a confident cross; when NO route clears it the composite is served as low-confidence (flagged, not published over the direct price). 0 (default) disables the floor — every route is treated as confident, matching pre-router behaviour. Dust USD pairs are already excluded from the edge set by min_usd_volume regardless of this knob." default:"0"`
	CompositeReference           CompositeReferenceConfig   `toml:"composite_reference" doc:"Current-bucket composite-reference corroboration of the phase-2 freeze for structurally single-venue targets (2026-08-29 product decision; design doc §10 amendment). For an allow-listed target whose bucket is single-venue, the aggregator rebuilds the target's [[aggregate.triangulations]] chain on the CURRENT bucket (this tick's crypto/USD leg publish × a fresh FX snap) and compares it with the direct print: agreement within tolerance_bps means the move is market-wide and the phase-2 fire is suppressed (corroboration_basis=composite); disagreement or an unavailable reference freezes exactly as before (corroboration_basis=venue, reason names why). The composite NEVER enters VWAP and NEVER raises source_count."`
}

// CompositeReferenceConfig is the `[aggregate.composite_reference]` block, mirroring orchestrator.CompositeReferenceConfig.
type CompositeReferenceConfig struct {
	Enabled          bool     `toml:"enabled" doc:"Master switch. Default ON for the targets allow-list below; set false to restore the pre-2026-08-29 freeze-and-auto-release posture for every pair without editing the list." default:"true"`
	Targets          []string `toml:"targets" doc:"Allow-list of structurally single-venue target pairs (canonical wire form) the composite reference may corroborate. Each must also have an [[aggregate.triangulations]] row — its legs ARE the reference. A target with >= 2 real venues on a bucket is never evaluated regardless of this list." default:"[\"crypto:XLM/fiat:GBP\",\"crypto:XLM/fiat:EUR\"]"`
	ToleranceBps     int      `toml:"tolerance_bps" doc:"Maximum |direct - composite| / composite, in basis points, for the composite to CORROBORATE the direct print. 0 = default (75). Must be in (0, 10000]." default:"75"`
	MinLegSources    int      `toml:"min_leg_sources" doc:"Minimum distinct real exchange venues each priced (crypto/USD) leg must carry on the CURRENT bucket for the composite to count; a single-venue leg cannot corroborate (reason: composite_unavailable: leg_sources=N). 0 = default (2)." default:"2"`
	LegDispersionBps int      `toml:"leg_dispersion_bps" doc:"Leg-dispersion guard: every venue's own bucket VWAP on a priced (crypto/USD) leg must be within this many basis points of the leg VWAP for the leg to corroborate; otherwise composite_unavailable: leg_dispersion=… (fail-closed). Two venues only count as two when they agree — a dominant venue plus a dust print 3% off is one opinion. 0 = tolerance_bps." default:"0"`
	ReleaseBandPct   float64  `toml:"release_band_pct" doc:"Mid-hold auto-release agreement band (%) between the fresh candidate and the current-bucket composite for a target whose reference resolved on the bucket. Dedicated and tighter than the shared 5% cross-oracle band, which would release a held +4% venue-specific offset. 0 = default (2.0)." default:"2.0"`
	FXMaxAgeHours    int      `toml:"fx_max_age_hours" doc:"Staleness budget for the FX leg's fx_quotes snap (bucket age at evaluation). fx_quotes buckets are DAILY and the feed pauses over market closes, so this mirrors the Chainlink FX feed budget (76h), not the 6h poll-liveness alert. The FX leg must also come from the FX source class (massive) — never an oracle. 0 = default (76)." default:"76"`
}

// TriangulationChainConfig is one triangulation row: Target is the implied pair and Legs the ordered chain; the loader validates target = Legs[0].Base / Legs[-1].Quote and Legs[i].Quote == Legs[i+1].Base.
type TriangulationChainConfig struct {
	Target string   `toml:"target" doc:"Implied target pair (canonical wire form)."`
	Legs   []string `toml:"legs" doc:"Ordered chain of leg pairs; product yields the target price. Must have at least 2 entries and adjacent legs must share their pivot asset."`
}

// APIConfig controls the public REST+SSE server.
type APIConfig struct {
	ListenAddr          string   `toml:"listen_addr" doc:"Bind address for the HTTP server." default:"0.0.0.0:3000"`
	ExternalBaseURL     string   `toml:"external_base_url" doc:"Public-facing /v1 root (e.g. https://api.stellarindex.io/v1). Emailed signup-verification links are built from it; self-hosters must set their own." default:"https://api.stellarindex.io/v1"`
	TLSCertProbeHosts   []string `toml:"tls_cert_probe_hosts" doc:"Public hostnames whose TLS leaf cert NotAfter the API binary should periodically probe and surface as stellarindex_tls_cert_not_after_unix{host}. Each entry may include :port; bare hostnames default to :443. The probe goroutine ticks every 6h. F-0051 (audit-2026-05-26): Caddy auto-renews Let's Encrypt 30d before expiry but silent renewal failures (DNS, rate limit, ACME quota) would otherwise only surface at cert expiry. Empty list disables the probe." default:"[\"api.stellarindex.io\",\"status.stellarindex.io\",\"stellarindex.io\"]"`
	AuthMode            string   `toml:"auth_mode" doc:"Authentication mode — none / apikey / apikey_optional / sep10. 'none' attaches anonymous Subject to every request. 'apikey' requires Authorization: Bearer <key> on every request; missing → 401. 'apikey_optional' is the freemium shape — anonymous floor (60/min) without a key, per-key tier (1000/min default) with a valid key, invalid key → 401. 'sep10' requires a SEP-10 JWT. The API binary wires real validators when the required dependencies are present; deployments that opt into auth without satisfying those fail loud rather than silently demoting to anonymous." default:"none"`
	AuthBackend         string   `toml:"auth_backend" doc:"Backing store for API-key validation. 'redis' (default) uses the legacy apikey:<hash> JSON records minted by /v1/signup. 'postgres' uses the platform.api_keys table (the dashboard's source of truth) with Redis as a read-through cache. Dashboard-minted keys are also mirrored into apikey:<hash> records whenever Redis is configured, so they authenticate under either backend. Cutover knob: deployments running both /v1/signup keys and dashboard-minted keys should use 'postgres' (the validator falls back to Postgres on Redis cache miss + writes back, so existing legacy keys keep working transparently). CUTOVER PROCEDURE — this is the hot auth path on a live API, so flip after a soak, not blind: (1) leave 'redis' running and confirm the dashboard bundle is wired (api.dashboard.base_url set, Postgres reachable) — the Postgres validator is constructed regardless of this flag, so its InvalidateCachedKey path is already active on dashboard revoke; (2) flip a canary instance to 'postgres' and watch that authenticated traffic still 200s and that dashboard-minted keys still authenticate; (3) soak, then roll the fleet. ROLLBACK is instant and lossless: set 'redis' and restart — no data migration either direction (Postgres stays the dashboard's source of truth, Redis keeps the legacy /v1/signup records; the two populations coexist). The 'postgres' read-through cache lives under apikey-cache:<hash>, apart from the apikey:<hash> records, so after a flip the 'redis' validator never serves a leftover cache row as a credential; the rows roll off on their own TTL. Invalidation on revoke/update works in BOTH modes: 'redis' rewrites the canonical record in place; 'postgres' evicts the read-through cache entry (dashboard revoke + the admin tier clamp both call InvalidateCachedKey). NOTE: 'postgres' disables the legacy /v1/account/keys self-service surface (it writes only to Redis, which the Postgres validator does not read as canonical) — customers manage keys via /v1/dashboard/keys instead." default:"redis"`
	AnonRateLimitPerMin int      `toml:"anon_rate_limit_per_min" doc:"Per-IP rate limit for anonymous requests. 0 DISABLES the anonymous tier entirely (fail-open, unbounded) — Validate() accepts 0 as a deliberate opt-out, but the API binary logs a boot-time WARN so the choice isn't silent (CFG-08, audit-2026-07-23)." default:"60"`
	KeyRateLimitPerMin  int      `toml:"key_rate_limit_per_min" doc:"Per-API-key rate limit, default tier. 0 DISABLES the authenticated tier entirely (fail-open, unbounded) — Validate() accepts 0 as a deliberate opt-out, but the API binary logs a boot-time WARN so the choice isn't silent (CFG-08, audit-2026-07-23)." default:"1000"`

	// RateLimitDwell wires ratelimit.WithDwellTime for the anon/key/failed-auth buckets, tunable around the
	// stellarindex_ratelimit_fail_open alert (>100 fail-open requests per 15 min) without a rebuild.
	// Negative disables the fail-open→fail-closed inversion.
	RateLimitDwell time.Duration `toml:"rate_limit_dwell" doc:"Fail-open dwell window for the anon/key/failed-auth rate-limit buckets before Take starts returning ratelimit.ErrThrottleUnavailable (fail-CLOSED, 503) on sustained Redis errors. Mirrors ratelimit.DefaultDwellTime. Negative disables the inversion (legacy fail-open-always)." default:"30s"`

	// MonthlyQuotaDwell wires middleware.WithMonthlyQuotaDwellTime.
	// Mirrors RateLimitDwell for the monthly-quota gate.
	MonthlyQuotaDwell time.Duration `toml:"monthly_quota_dwell" doc:"Fail-open dwell window for the monthly-quota middleware before month-to-date read errors flip it to fail-CLOSED (429 + Retry-After). Mirrors middleware.DefaultMonthlyQuotaDwellTime. Negative disables the inversion (legacy fail-open-always)." default:"30s"`

	// FailedAuthRateLimitPerMin caps invalid-credential attempts, which are rejected (401) before the main
	// limiter. The cap applies independently per client IP and, in API-key modes, per presented key prefix,
	// so rotating IPs does not reset the budget against one key. Only when auth_mode != none; 0 disables.
	FailedAuthRateLimitPerMin int `toml:"failed_auth_rate_limit_per_min" doc:"Cap on INVALID-credential (failed-auth) attempts per minute, enforced inside the Auth middleware so credential-stuffing / API-key guessing is throttled even though auth rejects before the main rate limiter (C3-5). Only active when auth_mode != none. Applied independently per resolved client IP and, in the apikey / apikey_optional modes, per presented key prefix (the 12-char display prefix), so guessing aimed at one key from many IPs is still capped; a valid key is never throttled by it. Redis-backed when available, in-process fixed-window fallback otherwise. 0 disables the failed-auth throttle." default:"20"`

	HoldsFile string `toml:"holds_file" doc:"Path to a TOML file of [[hold]] entries (asset, contract_id, ledger_from, ledger_to, reason) that mark matching supply, balance and holder responses as under review. Polled every 15s; a missing file means no holds; an invalid or zero-byte file keeps the previous list, so delete the file to lift every hold. Empty disables the feature." default:""`

	// SingleInstance asserts exactly ONE API instance, unlocking per-process fallbacks for the auth throttles
	// and passkey replay guard when Redis is absent. Across instances those are unsafe (a replayed passkey
	// finish-login on another instance mints a session), so the API refuses to start without Redis unless set.
	SingleInstance bool `toml:"single_instance" doc:"Assert this deployment runs exactly ONE API instance. Only consulted when Redis is DISABLED (storage.redis_addr empty and no sentinels): the auth throttles + passkey ceremony replay guard then fall back to per-PROCESS state, which is unsafe behind more than one instance (cross-instance ceremony replay → session mint; throttle caps multiply by replica count). With Redis absent the API refuses to start unless this is true, so a multi-instance-without-Redis topology cannot silently downgrade a session-minting path. No effect when Redis is configured. Default false (the safe assumption: assume multiple instances until told otherwise)." default:"false"`

	// RequestTimeout + ServingStatementTimeout are the two layers of the unauthenticated-DoS chokepoint:
	// the app-layer deadline is the primary bound, the SQL statement_timeout the backstop when Go-side
	// cancellation races. Keep statement_timeout LONGER so the app-layer deadline fires first.
	RequestTimeout          time.Duration `toml:"request_timeout" doc:"Per-request context deadline applied to every non-streaming request by the API's RequestTimeout middleware, so every handler inherits a bound even when it forgets its own. Streaming (SSE) endpoints are exempt (they own their lifecycle via client-disconnect ctx cancellation). HARD MINIMUM: it must EXCEED 12s, the longest per-handler budget (config.APIMaxHandlerBudget), or the API refuses to boot — at or below it the blanket deadline reaches every reader first, so each handler's own '…-timeout' 503 is unreachable and the ceilings they advertise are fiction. Keep it under the 30s http.Server WriteTimeout. 0 disables the middleware entirely (no deadline injected, and the minimum does not apply)." default:"15s"`
	ServingStatementTimeout time.Duration `toml:"serving_statement_timeout" doc:"Session-level Postgres statement_timeout applied to every connection in the API's serving pool (via a post-connect SET), so a runaway request-path query is bounded SQL-side even if Go-side ctx cancellation races. Keep it LONGER than request_timeout so the app-layer deadline fires first (defense in depth). The indexer/aggregator pools are unaffected — their heavy batch scans set their own longer SET LOCAL statement_timeout inside a transaction, which overrides this session default. 0 disables it (plain Open, no session timeout)." default:"30s"`

	// SignupRequireEmailVerification opts into the email-verification gate: API-key Subjects with a zero
	// EmailVerifiedAt and a /v1/signup-origin identifier get 403 pointing at the verify endpoint. Default true.
	SignupRequireEmailVerification bool            `toml:"signup_require_email_verification" doc:"F-1218: when true, /v1/signup-minted API keys must complete email-ownership-proof (clicking the link emailed at signup) before they can authenticate. Default true (2026-05-13): we are still pre-launch with no consumer traffic, so the safe default is to require verification — operators who want to allow unverified signup must opt in explicitly. Pre-launch default-flip narrows the launch-blocker surface; F-1218 closure required this." default:"true"`
	CDNEnabled                     bool            `toml:"cdn_enabled" doc:"Emit CDN-friendly Cache-Control headers on long-immutable endpoints." default:"true"`
	AllowedOrigins                 []string        `toml:"allowed_origins" doc:"CORS allow-list for browser clients. Empty (default) is same-origin only — no cross-origin browser client can read responses. SEC-14 (audit-2026-07-23): a wildcard here is fully cross-origin readable by every website out of the box; operators opt into cross-origin explicitly by listing their own hostnames." default:"[]"`
	AllowCredentials               bool            `toml:"allow_credentials" doc:"Emit Access-Control-Allow-Credentials: true on CORS responses, but ONLY to origins also listed in credentialed_origins. Required for cookie-bearing cross-origin requests (magic-link session on /v1/account/me, /v1/account/keys). Browser-incompatible with allowed_origins=[\"*\"]; config validation rejects the combination." default:"false"`
	CredentialedOrigins            []string        `toml:"credentialed_origins" doc:"Subset of allowed_origins trusted to (a) receive Access-Control-Allow-Credentials: true and (b) bypass the same-site write guard (RequireSameSiteWrite) with a plain Origin match. RSEC-X1: allowed_origins is a public read allow-list — a status/docs subdomain that only ever reads /v1/ledger/tip has no business being trusted to drive a cookie-authenticated write. Ignored when allow_credentials is false. Empty (default) trusts none, even if allowed_origins is non-empty — the safe, fail-closed default. Must be a subset of allowed_origins; the API panics at boot otherwise." default:"[]"`
	TrustedProxyCIDRs              []string        `toml:"trusted_proxy_cidrs" doc:"Immediate peer CIDR allow-list that is permitted to supply X-Forwarded-For. Empty means the API ignores that header and uses the socket peer address for logging, anonymous identity, and IP-based rate limiting." default:"[]"`
	SEP10                          SEP10Config     `toml:"sep10" doc:"SEP-10 Web Auth — server signing seed, JWT secret, TTLs. Active when auth_mode=sep10 OR when /v1/auth/sep10/* endpoints are exposed."`
	Streaming                      StreamingConfig `toml:"streaming" doc:"Closed-bucket SSE fanout — pairs the API binary republishes to the streaming Hub on every new closed prices_1m bucket. Empty Pairs leaves /v1/price/stream returning 503; Hub still constructs so subscribers can connect (and immediately drop) without a panic."`
	PrometheusURL                  string          `toml:"prometheus_url" doc:"Prometheus HTTP API root (e.g. http://localhost:9090) backing /v1/status. Empty leaves /v1/status serving an in-process surface (uptime + region only)." default:""`
	// StatusServices names the BACKGROUND services /v1/status reports and rolls `overall` up from
	// ("api" is always reported). Test nets run no aggregator; listing it would pin overall at
	// "degraded". The default (indexer + aggregator) is pubnet's set.
	StatusServices    []string        `toml:"status_services" doc:"Background services whose heartbeats /v1/status reports and rolls up (subset of: indexer, aggregator). Drop one only on a deployment that genuinely does not run it — a service omitted here can never be reported down." default:"[\"indexer\",\"aggregator\"]"`
	ArchiveReportPath string          `toml:"archive_report_path" doc:"Filesystem path of the archive-completeness daemon's latest JSON report (the -output-file of 'stellarindex-ops archive-completeness verify'; the systemd unit writes /var/lib/galexie/last-completeness-report.json). Backs GET /v1/diagnostics/archive. The endpoint 404s while the file doesn't exist yet and 503s when this is empty." default:"/var/lib/galexie/last-completeness-report.json"`
	Dashboard         DashboardConfig `toml:"dashboard" doc:"Customer dashboard auth flow — passwordless email login (6-digit code + magic link) + cookie sessions backing the in-site dashboard at stellarindex.io/account. Empty leaves /v1/auth/{login,callback,verify-code,logout} returning 503."`
}

// DashboardConfig wires passwordless email login and cookie sessions for stellarindex.io/account; without BaseURL or a Resend key (read from an env var, not TOML) the auth endpoints stay unwired.
type DashboardConfig struct {
	BaseURL string `toml:"base_url" doc:"Absolute URL of the explorer hosting the in-site dashboard (e.g. https://stellarindex.io). The magic-link callback URL embedded in emails is {base_url}/auth/callback?token=<plaintext>, and the post-login redirect lands on {base_url}/account." default:""`

	EmailFrom string `toml:"email_from" doc:"From: address for transactional emails (e.g. 'Stellar Index <hello@stellarindex.io>'). Must match a domain Resend has verified for the configured API key." default:"Stellar Index <hello@stellarindex.io>"`

	ResendAPIKeyEnv string `toml:"resend_api_key_env" doc:"Environment variable holding the Resend transactional-email API key (re_…). An unset or empty value wires an unconfigured mail sender: POST /v1/auth/login answers 503 and counts a failed send, and signup reports email_verification_sent:false. Production sets this." default:"STELLARINDEX_RESEND_API_KEY"`

	ResendDomainID string `toml:"resend_domain_id" doc:"Resend id of the sending domain. When set, the API turns that domain's click and open tracking off at boot (Resend has no per-send tracking switch, and click tracking rewrites sign-in links); a failure is logged and does not block boot. Empty leaves the domain's tracking as the Resend dashboard has it." default:""`

	SuppressedRecipientSHA256 []string `toml:"suppressed_recipient_sha256" doc:"Hex SHA-256 digests of lowercased, trimmed addresses that must never be mailed (hard bounces, complaints). Pseudonymised, not anonymous: anyone holding a candidate address can confirm membership. A suppressed send returns success to the caller, makes no Resend call and counts result=suppressed; suppressed logins therefore return faster, the same timing shape the throttle path already has. Empty suppresses nothing." default:"[]"`

	CodeSecretEnv string `toml:"code_secret_env" doc:"Environment variable holding the server secret that keys the 6-digit email-code derivation (HMAC over the stored token hash — without it a Postgres read would reveal every in-flight sign-in code) AND the WebAuthn passkey-ceremony, magic-link login-intent and login-device cookie MACs. Each consumer MACs under its own HKDF-derived key. Any long random string (32+ bytes). Required while passkeys are wired (the API refuses to start without it); otherwise an unset/empty env falls back to a random per-process secret: still keyed, but in-flight codes, magic links and browsers' login-device markers stop verifying across a restart or another instance." default:"STELLARINDEX_DASHBOARD_CODE_SECRET"`

	WebhookSealKeyEnv string `toml:"webhook_seal_key_env" doc:"Environment variable holding the secret that seals customer-webhook signing keys at rest (AES-256-GCM under an HKDF-derived key; column customer_webhooks.signing_key_sealed). At least 32 bytes; a shorter value refuses to start. Separate from code_secret_env so either can be rotated alone. Unset/empty stores new signing keys raw (logged at startup) and cannot read a sealed one, so deliveries to sealed webhooks wait until it is set. Changing the value makes every sealed key unreadable: their deliveries fail terminally, and the webhooks must be recreated (dashboard edit and delete still work without the key)." default:"STELLARINDEX_WEBHOOK_SEAL_KEY"`

	MagicLinkTTLMinutes int `toml:"magic_link_ttl_minutes" doc:"Magic-link validity in minutes. Default 15 — long enough for an email to arrive + the user to switch contexts; short enough to limit replay-window if a phone is briefly unattended." default:"15"`

	SessionTTLDays int `toml:"session_ttl_days" doc:"Session-cookie lifetime in days. Default 30 — matches typical SaaS dashboards; users sign in monthly without re-authing." default:"30"`

	CookieSecure bool `toml:"cookie_secure" doc:"Set the Secure flag on the JS-readable session-presence hint cookie. The credential cookies (session, login intent, passkey ceremony) are __Host- prefixed and always Secure; browsers treat http://localhost as a secure context. Production = true; dev (http://localhost) may set false." default:"true"`

	CookieDomain string `toml:"cookie_domain" doc:"Domain attribute of the JS-readable session-presence hint cookie only, so an explorer on a sibling host can see that a session exists; it carries no credential. Empty (default) means host-only. The credential cookies (session, login intent, passkey ceremony) are always host-only __Host- cookies and ignore this setting." default:""`
}

// StreamingConfig lists the (asset, quote) pairs the closed-bucket SSE producer broadcasts on /v1/price/stream, polling PriceReader at [PollInterval]; static, a new pair needs a restart.
type StreamingConfig struct {
	// Pairs is the operator-declared list of [base, quote] canonical asset-string pairs to broadcast, e.g.
	// ["native","fiat:USD"]. Empty disables the producer while still letting the Hub construct.
	Pairs [][]string `toml:"pairs" doc:"Operator-declared closed-bucket fanout pair list. Each entry is a two-element [base, quote] array of canonical asset strings (e.g. [[\"native\", \"fiat:USD\"], [\"credit:USDC:GA5Z…\", \"fiat:USD\"]]). Empty disables the producer; clients that connect see SSE open + heartbeats but no price_update events." default:"[]"`

	// PollInterval is the per-pair poll cadence. Sub-second values clamp to 1 s; zero means 5 s,
	// which sees a new 1-minute closed bucket within 5 s, inside Freighter's 30 s freshness target.
	PollInterval time.Duration `toml:"poll_interval" doc:"Per-pair poll cadence for the closed-bucket producer. Default 5s; clamped to 1s minimum." default:"5s"`

	// MaxStreamsPerIP caps concurrently-held SSE connections per client IP across every stream endpoint;
	// without it one non-reading client can exhaust file descriptors/goroutines, since the global cap
	// alone lets one IP take the whole budget. Over the cap a new connection gets 503. 0 disables it.
	MaxStreamsPerIP int `toml:"max_streams_per_ip" doc:"Maximum concurrently-held SSE stream connections per client IP across all stream endpoints (/v1/price/stream, /v1/price/tip/stream, /v1/observations/stream, /v1/ledger/stream). Guards against a single client exhausting file descriptors / goroutines by holding many stalled streams (C3-8 / CS-013). Over the cap a new stream is rejected with 429. 0 disables the per-IP cap; a separate global cap still bounds total concurrent streams." default:"20"`

	// MaxConcurrentStreams caps simultaneous SSE connections GLOBALLY, the backstop against many DISTINCT
	// client IPs (a botnet) that MaxStreamsPerIP cannot stop. Sets streaming.SetMaxConcurrentStreams
	// (package default 8192).
	MaxConcurrentStreams int64 `toml:"max_concurrent_streams" doc:"Global cap on simultaneous SSE connections across all stream endpoints, independent of the per-IP cap (guards against a flood of DISTINCT client IPs). Over the cap, a new connection is rejected with 503. <= 0 disables the global cap." default:"8192"`

	// MaxTipProducers caps distinct shared tip-stream producers (one
	// compute loop per (asset, quote, window)). Negative disables it.
	MaxTipProducers int `toml:"max_tip_producers" doc:"Global cap on distinct shared tip-stream producers (one compute loop per watched (asset, quote, window) triple), independent of connection counts — a detached producer outlives the connection that minted it for a linger window. Guards against an unauthenticated abort-loop flood enumerating the key space (UNAUTH-DOS-1). Negative disables the ceiling; 0 keeps the built-in default." default:"512"`

	// MaxTipProducersPerCaller caps the [MaxTipProducers] slots one
	// client-IP-derived caller may hold minted. Negative disables it.
	MaxTipProducersPerCaller int `toml:"max_tip_producers_per_caller" doc:"Per-caller cap on minted shared tip-stream producers, charged to the client-IP-derived principal for the life of the registry entry (through its linger). Prevents one address from filling the whole MaxTipProducers pool. Negative disables the per-caller quota; 0 keeps the built-in default." default:"24"`

	// MaxTopics overrides the Hub's topic-map ceiling. <= 0 keeps
	// streaming.DefaultMaxTopics (4096).
	MaxTopics int `toml:"max_topics" doc:"Ceiling on distinct live topics the Hub tracks (one per (asset, quote) pair actually subscribed to), independent of [Pairs] — guards unbounded topic growth from ad-hoc subscriptions. <= 0 keeps the built-in default (4096)." default:"4096"`
}

// SEP10Config configures the SEP-10 Web Auth validator; SeedEnv and JWTSecretEnv are env var NAMES, not values, and auth_mode=sep10 with an unset var fails at startup.
type SEP10Config struct {
	SeedEnv       string        `toml:"seed_env" doc:"Environment variable holding the server signing keypair S-strkey. Operators rotate this on a schedule; ansible-vault stores the actual value." default:"STELLARINDEX_SEP10_SEED"`
	JWTSecretEnv  string        `toml:"jwt_secret_env" doc:"Environment variable holding the HMAC-SHA256 JWT secret (≥ 32 bytes of entropy required)." default:"STELLARINDEX_SEP10_JWT_SECRET"`
	WebAuthDomain string        `toml:"web_auth_domain" doc:"SEP-10 web_auth_domain — the host that serves /v1/auth/sep10/*. Carried inside the challenge tx so clients verify before signing. Typically the API's external host (e.g. api.stellarindex.io)." default:"api.stellarindex.io"`
	HomeDomain    string        `toml:"home_domain" doc:"Issuer home_domain. Carried in the JWT iss claim and in the challenge's first manage_data op. Typically same as the project root domain." default:"stellarindex.io"`
	ChallengeTTL  time.Duration `toml:"challenge_ttl" doc:"How long a SEP-10 challenge is valid for signing. SDK requires ≥ 1s; SEP-10 spec recommends 15m." default:"15m"`
	JWTTTL        time.Duration `toml:"jwt_ttl" doc:"Lifetime of an issued JWT. Clients refresh by repeating the challenge → verify flow." default:"1h"`
}

// SupplyConfig configures the supply-snapshot writer; per ADR-0011 nothing is fabricated: native XLM circulating excludes SDF reserve accounts, and an account with no balance source makes the writer reject at start.
type SupplyConfig struct {
	// SDFReserveAccounts is the operator-curated G-strkey list (ADR-0011) whose XLM balances are subtracted from the
	// frozen total to yield circulating; verify-served-values diffs it daily against SDF's published list. It is also the
	// accounts observer's watched set but NOT a general watch list: every account added here reduces circulating supply.
	SDFReserveAccounts []string `toml:"sdf_reserve_accounts" doc:"G-strkey list of SDF-controlled reserve accounts whose XLM balances are excluded from circulating supply per ADR-0011 Algorithm 1." default:"[]"`

	// ReserveBalancesStroops maps account G-strkey → balance in stroops as a decimal string (NUMERIC-safe,
	// no float round-trip per ADR-0003). Updated manually when SDF announces a reserve move. Every account
	// in SDFReserveAccounts MUST appear here; a missing key is a config error caught at writer-start.
	ReserveBalancesStroops map[string]string `toml:"reserve_balances_stroops" doc:"Operator-managed snapshot of each SDF reserve account's XLM balance in stroops (decimal string). Updated manually on SDF reserve-move announcements. Used as the fallback source when the LCM AccountEntry observer (Task #54) hasn't yet populated account_observations for the watched reserve set; the live observer takes over once those rows land." default:"{}"`

	// ReserveBalancesAsOf dates the ReserveBalancesStroops snapshot (YYYY-MM-DD, UTC). The static
	// fallback refuses to answer when it is unset or older than ReserveBalancesMaxAge, so a
	// forgotten map fails closed instead of being republished as the current reserve.
	ReserveBalancesAsOf string `toml:"reserve_balances_as_of" doc:"Date (YYYY-MM-DD, UTC) the reserve_balances_stroops snapshot was taken. The static fallback refuses to answer when this is unset or older than reserve_balances_max_age; a snapshot it does serve is published with supply basis xlm_sdf_reserve_exclusion_static." default:""`

	// ReserveBalancesMaxAge bounds how old the dated static snapshot may
	// be and still be served.
	ReserveBalancesMaxAge time.Duration `toml:"reserve_balances_max_age" doc:"Maximum age of the reserve_balances_stroops snapshot (measured from reserve_balances_as_of) at which the static fallback still answers. Past it, the XLM supply tick fails instead of republishing the old balances." default:"168h"`

	// AggregatorRefreshEnabled runs the supply-snapshot writer as a goroutine in the aggregator on
	// [AggregatorRefreshCadence]. When false (default) the systemd timer (deploy/systemd/supply-snapshot.timer)
	// drives it; once the LCM observer covers the live operator set the goroutine path is preferred.
	AggregatorRefreshEnabled bool `toml:"aggregator_refresh_enabled" doc:"Run the supply-snapshot writer as a goroutine in the aggregator instead of via the systemd timer. Requires the LCM AccountEntry observer to be backfilled across the watched accounts (or the static reserve_balances_stroops fallback to be valid)." default:"false"`

	// AggregatorRefreshCadence is the interval for the goroutine path. Default 5 minutes balances freshness
	// (observed_at tracks the ledger) against table-write rate: asset_supply_history dedupes per-(asset, ledger)
	// but unique-ledger rows still accumulate one per tick.
	AggregatorRefreshCadence time.Duration `toml:"aggregator_refresh_cadence" doc:"Per-cycle interval for the in-aggregator supply-snapshot worker (only used when aggregator_refresh_enabled is true)." default:"5m"`

	// WatchedClassicAssets is the operator-curated list of classic assets (canonical CODE-ISSUER) the supply
	// pipeline computes Algorithm 2 for (ADR-0022): drives the four classic-supply observers and the aggregator's
	// refresh loop. Empty (default) leaves them off; the XLM-only path remains.
	WatchedClassicAssets []string `toml:"watched_classic_assets" doc:"Operator-curated classic credit assets (CODE-ISSUER form) to track for Algorithm 2 supply per ADR-0022. Empty leaves the classic-supply pipeline off." default:"[]"`

	// SACWrappers maps the C-strkey of a Stellar-Asset-Contract wrapper to the supply.AssetKey form
	// (CODE:ISSUER) of the classic asset it wraps; the SAC observer uses it to stamp asset_key and filter to the
	// watched set. Empty is correct for assets with no deployed or tracked SAC (the SAC component sums to zero).
	SACWrappers map[string]string `toml:"sac_wrappers" doc:"SAC wrapper contract C-strkey → supply.AssetKey (CODE:ISSUER) map. Drives the SAC balance observer's watched-contract filter. Pure SEP-41 contracts reuse this map by mapping contract_id → contract_id." default:"{}"`

	// WatchedSEP41Contracts is the operator-curated list of SEP-41 contract C-strkeys the supply pipeline
	// computes Algorithm 3 for (ADR-0023): drives internal/sources/sep41_supply and the aggregator's SEP-41
	// refresh loop. Empty (default) leaves it off.
	WatchedSEP41Contracts []string `toml:"watched_sep41_contracts" doc:"Operator-curated SEP-41 Soroban contract C-strkeys to track for Algorithm 3 supply per ADR-0023. Empty leaves the SEP-41 supply pipeline off." default:"[]"`

	// FullyWrappedSACs attests that a SAC wrapper (keyed as in SACWrappers) holds the classic asset's ENTIRE
	// supply. Listed ids get supply.WrapClassFull (strict ADR-0011 total-vs-total compare), others WrapClassPartial
	// (see supply.CrossCheckSubsetBound). Empty is the honest default; an entry needs the evidence trail of a BackfillSafe flip (docs/operations/wasm-audits/).
	FullyWrappedSACs []string `toml:"fully_wrapped_sacs" doc:"SAC wrapper contract C-strkeys (subset of sac_wrappers' keys) the operator attests are 100% SAC-represented — no classic-trustline supply outside the SAC. Selects the strict ADR-0011 equality cross-check (supply.WrapClassFull) instead of the default subset-bound compare (supply.WrapClassPartial). Empty by default; BACKLOG #59, 2026-07-08." default:"[]"`

	// StrictFreshnessRequired makes the supply Refresher reject snapshots with `MinComponentLedger == 0` (no
	// freshness anchor; static-XLM fallback or a failing producer) as `OutcomeKindMissingFreshness` instead of
	// publishing them. Default false; flip once every freshness producer is wired and no reader fails open under load.
	StrictFreshnessRequired bool `toml:"strict_freshness_required" doc:"F-1236: when true, supply snapshots without a MinComponentLedger anchor (i.e. zero-value freshness, the static-XLM fallback or a transiently-failing producer) are rejected rather than published. Default false preserves backwards-compatible permissive behaviour; flip true after the freshness producers are confirmed wired in steady state." default:"false"`

	StaleComponentLedgers uint32 `toml:"stale_component_ledgers" doc:"Global stale-component threshold in ledgers: a supply snapshot whose oldest component observation lags the snapshot ledger by more than this is rejected (outcome stale_component). 0 disables the gate. Per-asset overrides live in stale_component_ledgers_by_asset. Independent of the classic/SAC cross-check alignment window, which stays fixed at 1000 ledgers." default:"1000"`

	MaxDormantComponentLedgers uint32 `toml:"max_dormant_component_ledgers" doc:"Dormancy horizon in ledgers: how far the snapshot ledger may run ahead of an UNCHANGED component ledger before a dormant-asset accept becomes a stale_component rejection. 0 disables the horizon, so a dead observer looks dormant forever; set it only for assets legitimately dormant for long stretches whose observers are monitored another way." default:"17280"`

	// StaleComponentLedgersByAsset relaxes or tightens the global stale_component_ledgers gate for one asset,
	// so a low-activity asset (PHO lags ~1200 ledgers) does not force the gate open fleet-wide. Keys are
	// canonicalised via supply.CanonicalizeStaleComponentLedgers; Validate rejects an unwatched key. Zero disables the gate for that asset.
	StaleComponentLedgersByAsset map[string]uint32 `toml:"stale_component_ledgers_by_asset" doc:"Per-asset override of the F-1236 stale-component-ledger threshold. Map keys are asset_key in the internal supply.AssetKey() shape ('XLM' for native, CODE:ISSUER for classic, bare contract id for SEP-41); values are ledger counts. Empty map (default) keeps every asset on the global 1000-ledger threshold. F-0040 (audit-2026-05-26)." default:"{}"`

	// PerAssetLockedSets overrides the default locked set (issuer-only for classic Algorithm 2, admin-only for
	// SEP-41 Algorithm 3), e.g. treasury/vesting accounts. Keys name a watched_classic_assets entry (CODE-G… or CODE:G…)
	// or watched_sep41_contracts id, re-keyed to supply.AssetKey at load; XLM, unwatched or unparseable keys fail Validate.
	// A missing key uses the default; a present-but-empty entry opts out of it.
	PerAssetLockedSets map[string]SupplyLockedSetConfig `toml:"per_asset_locked_sets" doc:"Per-asset override of the default locked-set (issuer-only for classic, admin-only for SEP-41) excluded from circulating_supply. Map key: a watched_classic_assets entry ('CODE-G...' or 'CODE:G...') or a watched_sep41_contracts C-strkey; 'XLM' and unwatched keys are rejected at startup. Members must be observed or startup fails: a classic key's contracts need a sac_wrappers entry for its SAC; a SEP-41 key needs a sac_wrappers entry mapping it to itself, or to its classic asset (a SAC), whose accounts then also need that asset in watched_classic_assets. Empty map preserves the per-algorithm default for every asset." default:"{}"`

	// MaxSupplyOverrides forces max_supply for an asset, beating the SEP-1 declaration and the per-algorithm
	// default. Keyed like PerAssetLockedSets (no XLM); value is a decimal string in base units (stroops for
	// classic). An empty string falls through to the next source (supply.Policy.MaxSupplyOverride).
	MaxSupplyOverrides map[string]string `toml:"max_supply_overrides" doc:"Per-asset max_supply override, beating the SEP-1 declaration and the per-algorithm default. Map key mirrors per_asset_locked_sets; value is a decimal string in base units. Empty string value falls through to the next source." default:"{}"`
}

// SupplyLockedSetConfig is the TOML-friendly mirror of internal/supply.LockedSet, kept separate so TOML tags stay independent of the domain type.
type SupplyLockedSetConfig struct {
	Accounts  []string `toml:"accounts" doc:"G-strkey accounts whose balance is excluded from circulating supply for this asset (treasury / reserve multisigs)." default:"[]"`
	Contracts []string `toml:"contracts" doc:"C-strkey contracts whose balance is excluded from circulating supply for this asset (vesting / treasury contracts)." default:"[]"`
}

// Validate reports supply-block inconsistencies: bad G-strkeys, a refresh cadence under 30s, empty entries, and FullyWrappedSACs or per-asset keys not resolving to exactly one watched asset (XLM rejected).
func (sc SupplyConfig) Validate() error {
	for i, acc := range sc.SDFReserveAccounts {
		if !canonical.IsAccountID(acc) {
			return fmt.Errorf("supply: sdf_reserve_accounts[%d] %q is not a valid G-strkey", i, acc)
		}
	}
	if sc.AggregatorRefreshEnabled && sc.AggregatorRefreshCadence < 30*time.Second {
		return fmt.Errorf("supply: aggregator_refresh_cadence %v < 30s minimum", sc.AggregatorRefreshCadence)
	}
	for i, raw := range sc.WatchedClassicAssets {
		if raw == "" {
			return fmt.Errorf("supply: watched_classic_assets[%d] is empty", i)
		}
	}
	for cid, ak := range sc.SACWrappers {
		if cid == "" {
			return fmt.Errorf("supply: sac_wrappers has empty contract id (asset %q)", ak)
		}
		if ak == "" {
			return fmt.Errorf("supply: sac_wrappers[%q] has empty asset_key", cid)
		}
	}
	for i, c := range sc.WatchedSEP41Contracts {
		if c == "" {
			return fmt.Errorf("supply: watched_sep41_contracts[%d] is empty", i)
		}
	}
	if err := sc.validateFullyWrappedSACs(); err != nil {
		return err
	}
	if _, err := sc.ReserveBalancesAsOfTime(); err != nil {
		return err
	}
	if sc.ReserveBalancesMaxAge < 0 {
		return fmt.Errorf("supply: reserve_balances_max_age %v must not be negative", sc.ReserveBalancesMaxAge)
	}
	if err := sc.validateStaleComponentLedgersByAsset(); err != nil {
		return err
	}
	return sc.validatePolicyOverrideKeys()
}

// DefaultReserveBalancesMaxAge is the reserve_balances_max_age default: one week.
const DefaultReserveBalancesMaxAge = 7 * 24 * time.Hour

// EffectiveReserveBalancesMaxAge is ReserveBalancesMaxAge, or the default when unset.
func (sc SupplyConfig) EffectiveReserveBalancesMaxAge() time.Duration {
	if sc.ReserveBalancesMaxAge <= 0 {
		return DefaultReserveBalancesMaxAge
	}
	return sc.ReserveBalancesMaxAge
}

// NewStaticReserveReader builds the dated static reserve-balance reader from reserve_balances_stroops / _as_of / _max_age.
func (sc SupplyConfig) NewStaticReserveReader() (*supply.ConfigReserveBalanceReader, error) {
	asOf, err := sc.ReserveBalancesAsOfTime()
	if err != nil {
		return nil, err
	}
	return supply.NewConfigReserveBalanceReader(sc.ReserveBalancesStroops, asOf, sc.EffectiveReserveBalancesMaxAge())
}

// ReserveBalancesAsOfTime parses ReserveBalancesAsOf; unset yields the zero time, which the static reader treats as undated.
func (sc SupplyConfig) ReserveBalancesAsOfTime() (time.Time, error) {
	if sc.ReserveBalancesAsOf == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.DateOnly, sc.ReserveBalancesAsOf)
	if err != nil {
		return time.Time{}, fmt.Errorf("supply: reserve_balances_as_of %q is not a YYYY-MM-DD date: %w", sc.ReserveBalancesAsOf, err)
	}
	return t, nil
}

// validateStaleComponentLedgersByAsset is check 6 of [SupplyConfig.Validate]. XLM is always
// watched: the aggregator builds its refresher unconditionally.
func (sc SupplyConfig) validateStaleComponentLedgersByAsset() error {
	byAsset, err := supply.CanonicalizeStaleComponentLedgers(sc.StaleComponentLedgersByAsset)
	if err != nil {
		return fmt.Errorf("supply: stale_component_ledgers_by_asset: %w", err)
	}
	watched := sc.watchedSupplyKeys()
	if xlm, err := supply.AssetKey(canonical.NativeAsset()); err == nil {
		watched[xlm] = struct{}{}
	}
	return requireWatchedSupplyKeys("stale_component_ledgers_by_asset", byAsset, watched,
		"'XLM', a watched_classic_assets entry, or a watched_sep41_contracts id")
}

// validatePolicyOverrideKeys is check 7 of [SupplyConfig.Validate]. Only the classic and SEP-41
// computers read these maps, so XLM is not a valid key (CanonicalizePolicyKeys rejects it).
func (sc SupplyConfig) validatePolicyOverrideKeys() error {
	lockedSets, err := supply.CanonicalizePolicyKeys(sc.PerAssetLockedSets)
	if err != nil {
		return fmt.Errorf("supply: per_asset_locked_sets: %w", err)
	}
	maxSupply, err := supply.CanonicalizePolicyKeys(sc.MaxSupplyOverrides)
	if err != nil {
		return fmt.Errorf("supply: max_supply_overrides: %w", err)
	}
	const want = "a watched_classic_assets entry or a watched_sep41_contracts id"
	watched := sc.watchedSupplyKeys()
	if err := requireWatchedSupplyKeys("per_asset_locked_sets", lockedSets, watched, want); err != nil {
		return err
	}
	if err := requireWatchedSupplyKeys("max_supply_overrides", maxSupply, watched, want); err != nil {
		return err
	}
	return sc.validateLockedSetCoverage(lockedSets, watched)
}

// sacWrapperRows is the asset_key the sac_balances observer stamps on one
// sac_wrappers contract's balance rows.
type sacWrapperRows struct {
	assetKey string
	classic  bool
}

// validateLockedSetCoverage is the locked-set check of [SupplyConfig.Validate]. Readers read a holder with no
// observation row as zero, so a member no observer records excludes nothing while the snapshot claims the override
// basis. Checked per (asset, holder kind), never per row: a never-funded holder legitimately has none.
func (sc SupplyConfig) validateLockedSetCoverage(lockedSets map[string]SupplyLockedSetConfig, watched map[string]struct{}) error {
	rows := make(map[string]sacWrapperRows, len(sc.SACWrappers))
	sacWrapped := map[string]struct{}{}
	for cid, v := range sc.SACWrappers {
		key, classic, err := supply.SACWrapperAssetKey(v)
		if err != nil {
			return fmt.Errorf("supply: sac_wrappers[%q]: %w", cid, err)
		}
		rows[cid] = sacWrapperRows{assetKey: key, classic: classic}
		if classic {
			sacWrapped[key] = struct{}{}
		}
	}
	keys := make([]string, 0, len(lockedSets))
	for key := range lockedSets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := lockedSetObserved(key, lockedSets[key], rows, sacWrapped, watched); err != nil {
			return fmt.Errorf("supply: per_asset_locked_sets[%q]: %w", key, err)
		}
	}
	return nil
}

// lockedSetObserved reports the first holder kind of ls that no observer
// records for the asset key (supply.AssetKey form).
func lockedSetObserved(key string, ls SupplyLockedSetConfig, rows map[string]sacWrapperRows, sacWrapped, watched map[string]struct{}) error {
	if !canonical.IsContractID(key) {
		// Classic: accounts are trustlines, observed for every watched asset.
		if _, ok := sacWrapped[key]; len(ls.Contracts) > 0 && !ok {
			return fmt.Errorf("contracts hold %s through its SAC, but no sac_wrappers entry maps a SAC to it, so each would exclude 0", key)
		}
		return nil
	}
	if len(ls.Accounts) == 0 && len(ls.Contracts) == 0 {
		return nil
	}
	r, ok := rows[key]
	switch {
	case !ok:
		return errors.New("the contract is not a sac_wrappers key, so no holder balance is observed and each member would exclude 0 (map it to itself, or to its classic asset if it is a SAC)")
	case r.assetKey == key:
		return nil
	case !r.classic:
		return fmt.Errorf("sac_wrappers maps the contract to %q, neither itself nor a classic asset, so no holder balance is observed", r.assetKey)
	}
	if _, ok := watched[r.assetKey]; len(ls.Accounts) > 0 && !ok {
		return fmt.Errorf("accounts hold this SAC as %s trustlines, which are observed only for watched_classic_assets; add %s there", r.assetKey, r.assetKey)
	}
	return nil
}

// watchedSupplyKeys is the supply.AssetKey set of the watched classic
// and SEP-41 assets.
func (sc SupplyConfig) watchedSupplyKeys() map[string]struct{} {
	watched := map[string]struct{}{}
	for _, raw := range sc.WatchedClassicAssets {
		// An unparseable entry is reported by the classic builder at startup.
		if key, err := supply.ParseAssetKey(raw); err == nil {
			watched[key] = struct{}{}
		}
	}
	for _, c := range sc.WatchedSEP41Contracts {
		watched[c] = struct{}{}
	}
	return watched
}

// requireWatchedSupplyKeys rejects, in sorted order, the first key of
// byAsset (already in supply.AssetKey form) that is not in watched.
func requireWatchedSupplyKeys[V any](field string, byAsset map[string]V, watched map[string]struct{}, want string) error {
	keys := make([]string, 0, len(byAsset))
	for key := range byAsset {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, ok := watched[key]; !ok {
			return fmt.Errorf("supply: %s key %q names no watched asset (want %s)", field, key, want)
		}
	}
	return nil
}

// validateFullyWrappedSACs is check 5 of [SupplyConfig.Validate], split out for gocognit.
func (sc SupplyConfig) validateFullyWrappedSACs() error {
	for i, sacID := range sc.FullyWrappedSACs {
		if sacID == "" {
			return fmt.Errorf("supply: fully_wrapped_sacs[%d] is empty", i)
		}
		if _, ok := sc.SACWrappers[sacID]; !ok {
			return fmt.Errorf("supply: fully_wrapped_sacs[%d] (%q) is not a key of sac_wrappers", i, sacID)
		}
	}
	return nil
}

// ObsConfig wires metrics and logs; long-lived binaries bind a dedicated `/metrics` listener at [ObsConfig.MetricsListen], while the API serves `/metrics` on its public listener.
type ObsConfig struct {
	MetricsListen string `toml:"metrics_listen" doc:"Bind address for the dedicated /metrics Prometheus endpoint. Read by the indexer, the aggregator, and the long-lived ops binaries (cross-region-monitor, verify-archive --metrics). The API binary serves /metrics on its public listener and ignores this field." default:"127.0.0.1:9464"`
	// MetricsListenSet records whether obs.metrics_listen was present in the loaded TOML (set by LoadReader from
	// toml.MetaData, never the decoder). The aggregator's single-host port-collision shift reads it instead of
	// comparing values, so an operator who explicitly pins 127.0.0.1:9464 is honoured.
	MetricsListenSet bool   `toml:"-"`
	LogLevel         string `toml:"log_level" doc:"Minimum log level — debug / info / warn / error." default:"info"`
	LogFormat        string `toml:"log_format" doc:"Log format — json / text / console ('text' and 'console' are synonyms)." default:"json"`
}

// defaultHashDBConfig is split out of Default() for the funlen ceiling. Non-zero cadence + window so an
// accidental Enabled=true doesn't reach time.NewTicker(0); kept in lockstep with the `default:` tags
// (TestDefault_MatchesStructTags).
func defaultHashDBConfig() HashDBConfig {
	return HashDBConfig{
		Enabled: false,
		Path:    "/var/lib/stellarindex/hashdb.bin",
	}
}

// defaultAnomalyConfig is split out of Default() for the funlen ceiling. It holds the ADR-0019 defaults: the Phase 2
// freeze thresholds and the freeze-duration lifecycle, whose values mirror freeze.Default* in internal/aggregate/freeze
// (pinned by TestDefault_MatchesStructTags and TestPolicy_WithDefaults_MatchesADR0019).
func defaultAnomalyConfig() AnomalyConfig {
	return AnomalyConfig{
		// Enabled defaults to false — operator opts in once
		// classifications are set per ADR-0019 stop-gap.
		Phase2: Phase2FreezeConfig{
			ConfidenceMaxFreeze:  0.45,
			ZScoreMinFreeze:      5.0,
			SourceCountMaxFreeze: 1,

			InitialHoldMinutes:               30,
			UncorroboratedInitialHoldMinutes: 10,
			ExtensionMinutes:                 30,
			MaxExtensions:                    4,
			UnfreezeConfidenceMin:            0.30,
			UnfreezeZScoreMax:                3.0,
			UnfreezeBuckets:                  2,
		},
	}
}

// defaultAPIConfig holds the public-API / auth / dashboard defaults, split out of Default() for
// funlen and kept in lockstep with the `default:` tags (TestDefault_MatchesStructTags).
func defaultAPIConfig() APIConfig {
	return APIConfig{
		ListenAddr:                "0.0.0.0:3000",
		ExternalBaseURL:           "https://api.stellarindex.io/v1",
		AuthMode:                  "none",
		AuthBackend:               "redis",
		AnonRateLimitPerMin:       60,
		KeyRateLimitPerMin:        1000,
		FailedAuthRateLimitPerMin: 20,
		RateLimitDwell:            30 * time.Second,
		MonthlyQuotaDwell:         30 * time.Second,
		// DoS chokepoint: the 15s app deadline is the primary bound; the 30s serving-pool
		// statement_timeout is the backstop, longer so the app deadline fires first.
		RequestTimeout:          15 * time.Second,
		ServingStatementTimeout: 30 * time.Second,
		CDNEnabled:              true,
		AllowedOrigins:          []string{},
		CredentialedOrigins:     []string{},
		TrustedProxyCIDRs:       []string{},
		// Probe the public TLS leaf certs so silent renewal failures surface before expiry (the
		// alert series only exists when this is populated).
		TLSCertProbeHosts: []string{"api.stellarindex.io", "status.stellarindex.io", "stellarindex.io"},
		// The safe default is to require email-ownership
		// proof on signup-minted keys; operators opt out explicitly.
		SignupRequireEmailVerification: true,
		// Matches archive-completeness.service REPORT_OUTPUT; the handler 404s while the file is
		// absent, so this is harmless on hosts without the daemon.
		ArchiveReportPath: "/var/lib/galexie/last-completeness-report.json",
		// Pubnet runs both; the lean test-net inventories drop
		// "aggregator".
		StatusServices: []string{"indexer", "aggregator"},
		SEP10: SEP10Config{
			SeedEnv:       "STELLARINDEX_SEP10_SEED",
			JWTSecretEnv:  "STELLARINDEX_SEP10_JWT_SECRET",
			WebAuthDomain: "api.stellarindex.io",
			HomeDomain:    "stellarindex.io",
			ChallengeTTL:  15 * time.Minute,
			JWTTTL:        1 * time.Hour,
		},
		Dashboard: DashboardConfig{
			EmailFrom:           "Stellar Index <hello@stellarindex.io>",
			ResendAPIKeyEnv:     "STELLARINDEX_RESEND_API_KEY",
			CodeSecretEnv:       "STELLARINDEX_DASHBOARD_CODE_SECRET",
			WebhookSealKeyEnv:   "STELLARINDEX_WEBHOOK_SEAL_KEY",
			MagicLinkTTLMinutes: 15,
			SessionTTLDays:      30,
			CookieSecure:        true, // dev (http://localhost) overrides to false
		},
		Streaming: StreamingConfig{
			Pairs:                    [][]string{},
			PollInterval:             5 * time.Second,
			MaxStreamsPerIP:          20,
			MaxConcurrentStreams:     8192,
			MaxTipProducers:          512,
			MaxTipProducersPerCaller: 24,
			MaxTopics:                4096,
		},
	}
}

// RetiredKeys lists dotted config paths removed from the schema; LoadReader warns on them instead of failing boot, so add an entry in the commit that removes a field.
var RetiredKeys = map[string]string{
	"aggregate.vwap_window_seconds":                   "GH-1129: unread — windows come from aggregate.windows via AggregatorWindows()",
	"aggregate.twap_window_seconds":                   "GH-1129: unread — windows come from aggregate.windows via AggregatorWindows()",
	"ingestion.cursor_store_scheme":                   "GH-1129: unread — cursors are unconditionally Postgres",
	"ingestion.backfill_batch_size":                   "GH-1129: unread",
	"region.name":                                     "GH-1129: unread — region label is region.id",
	"region.home_domain":                              "GH-1129: unread — SEP-10 reads api.sep10.home_domain",
	"stellar.core_http_endpoint":                      "GH-1129: unread — no liveness probe consumes it",
	"obs.trace_exporter":                              "unread — no tracer is wired",
	"obs.trace_sample":                                "unread — no tracer is wired",
	"divergence.per_reference_timeout_seconds":        "never set — divergence.DefaultPerReferenceTimeout",
	"divergence.supply.per_reference_timeout_seconds": "never set — divergence.DefaultSupplyPerReferenceTimeout",
	"divergence.supply.refresh_interval_seconds":      "never set — fixed 15-minute cadence in the aggregator",
	"divergence.reflector.max_age_minutes":            "never set — per-oracle default staleness ceiling",
	"divergence.redstone.max_age_minutes":             "never set — per-oracle default staleness ceiling",
	"divergence.band.max_age_minutes":                 "never set — per-oracle default staleness ceiling",
	"divergence.coingecko.max_age_minutes":            "never set — 30-minute default staleness ceiling",
	"decimals_guard":                                  "never set — backfill window fixed at 90d in decimalsguard",
	"decimals_guard.backfill_window_days":             "never set — fixed 90d in decimalsguard",
	"price_alerts.interval_seconds":                   "never set — fixed 30s in pricealerts",
	"signup_reaper.interval_minutes":                  "never set — fixed 60m in signupreaper",
	"signup_reaper.min_age_minutes":                   "never set — fixed 24h in signupreaper",
	"hashdb.verify_interval_minutes":                  "never set — fixed 60m in the indexer",
	"hashdb.verify_window_ledgers":                    "never set — fixed 20000 ledgers in the indexer",
	"api.holds_reload_interval":                       "never set — fixed 15s in the API",
	"storage.background_statement_timeout":            "never set — timescale.BackgroundStatementTimeout (30m)",
	"storage.redis_closed_bucket_channel":             "never set — redispub.DefaultChannel",
	"api.streaming.buffer_size":                       "never set — streaming.DefaultBufferSize (256)",
	"api.streaming.topic_idle_ttl":                    "never set — streaming.DefaultTopicIdleTTL (15m)",
	"external.massive.refresh_interval":               "never set — fixed 1h forex poll in the API",
}

// Default returns a Config with every field at its default; it always passes Validate.
func Default() Config {
	return Config{
		Region: RegionConfig{
			ID:         "r1",
			Deployment: "production",
		},
		Stellar: StellarConfig{
			Network:           "pubnet",
			RPCEndpoints:      []string{"http://127.0.0.1:8000"},
			HistoryArchiveURL: "https://history.stellar.org/prd/core-live/core_live_001",
			// Pubnet protocol-transition boundaries (pinned to the leaf consts
			// by TestP23BoundaryConstantsAgree; test nets override BOTH to 1).
			SorobanGenesisLedger: 50457424,
			MovementsFloorLedger: 58762517,
		},
		Storage: StorageConfig{
			PostgresDSN: "postgres://stellarindex@127.0.0.1:5432/stellarindex?sslmode=disable",
			RedisAddr:   "127.0.0.1:6379",
			// Sentinel fields stay empty (dev / single-node); production inventories set them (ADR-0024).
			RedisSentinelAddrs: []string{},
			RedisMasterName:    "",
			S3Endpoint:         "http://127.0.0.1:9000",
			S3Region:           "r1",
			S3BucketArchive:    "galexie-archive",
			S3BucketLive:       "galexie-live",
			S3AccessKeyEnv:     "STELLARINDEX_S3_ACCESS_KEY",
			S3SecretKeyEnv:     "STELLARINDEX_S3_SECRET_KEY",
			ClickHouseAddr:     "127.0.0.1:9300",
			// ADR-0041: the lake substrate is on by default; it backs the coverage claim, the CH
			// completeness path and lake-derived supply.
			ClickHouseLiveSink:        true,
			ClickHouseProjectorSource: true,
		},
		Ingestion: IngestionConfig{
			EnabledSources:     []string{"soroswap", "aquarius", "phoenix"},
			BackfillFromLedger: 0,
			LiveSeamLedger:     0,
			// Phase-3 PARALLEL by default: an enabled projector leaves the dispatcher
			// double-writing un-promoted Soroban sources. sep41 is a sole writer and goes
			// through the projector alone whatever this flag says.
			Projector: ProjectorConfig{
				Enabled:          false,
				PersistPerSource: true,
			},
		},
		Oracle: OracleConfig{
			// Reflector addresses are operator-supplied (docs/protocols/reflector.md); enabling a
			// reflector-* source without one is a startup error.
			Reflector: ReflectorOracleConfig{},
			Redstone:  RedstoneOracleConfig{},
			Band:      BandOracleConfig{},
			Soroswap:  SoroswapConfig{},
			// No shipped overrides: every asset gets the default 10 x resolution budget.
			StalenessOverrides: []OracleStalenessOverrideConfig{},
		},
		External:     defaultExternalConfig(),
		Aggregate:    defaultAggregateConfig(),
		Anomaly:      defaultAnomalyConfig(),
		API:          defaultAPIConfig(),
		Divergence:   defaultDivergenceConfig(),
		PricingGuard: defaultPricingGuardConfig(),
		PriceAlerts: PriceAlertsConfig{
			// Off by default: the operator opts in once alerts + webhooks are wired.
			Enabled: false,
		},
		SignupReaper: SignupReaperConfig{
			// On by default: signup-race orphans are pure garbage (no user, no key).
			Enabled: true,
		},
		Supply: SupplyConfig{
			// Used only when AggregatorRefreshEnabled is on; non-zero so time.NewTicker(0) cannot panic.
			AggregatorRefreshCadence:   5 * time.Minute,
			ReserveBalancesMaxAge:      DefaultReserveBalancesMaxAge,
			StaleComponentLedgers:      supply.DefaultStaleComponentLedgers,
			MaxDormantComponentLedgers: supply.DefaultMaxDormantComponentLedgers,
		},
		HashDB: defaultHashDBConfig(),
		Obs: ObsConfig{
			MetricsListen: "127.0.0.1:9464",
			LogLevel:      "info",
			LogFormat:     "json",
		},
	}
}

// defaultAggregateConfig returns the price-aggregator defaults, including the composite-reference cross-check.
func defaultAggregateConfig() AggregateConfig {
	return AggregateConfig{
		MinUSDVolume:                 10_000,
		MinMarketCapVolumeUSD:        1_000,
		MaxMarketCapVolumeRatio:      50_000,
		OutlierSigmaThreshold:        4,
		TriangulationEnabled:         true,
		IntervalSeconds:              30,
		DivergenceMinIntervalSeconds: 300,
		MaxTradesPerWindow:           10_000,
		MaxHops:                      3,
		MinRouteConfidence:           0,
		CompositeReference: CompositeReferenceConfig{
			// ON by default: fail-closed on every leg (thin, stale, wrong class) and evaluated only on single-venue buckets
			// of the listed targets, the pairs the design doc (§3) records as structurally single-venue with a deep USD leg.
			Enabled:       true,
			Targets:       []string{"crypto:XLM/fiat:GBP", "crypto:XLM/fiat:EUR"},
			ToleranceBps:  75,
			MinLegSources: 2,
			FXMaxAgeHours: 76,
			// leg_dispersion_bps 0 = tolerance_bps.
			ReleaseBandPct: 2.0,
		},
	}
}

// defaultExternalConfig returns the off-chain-connector defaults: every venue is disabled until opted in.
func defaultExternalConfig() ExternalConfig {
	return ExternalConfig{
		Binance:          ExternalStreamerConfig{Enabled: false},
		Kraken:           ExternalStreamerConfig{Enabled: false},
		Bitstamp:         ExternalStreamerConfig{Enabled: false},
		Coinbase:         ExternalStreamerConfig{Enabled: false},
		ExchangeRatesApi: ExchangeRatesApiVenueConfig{Enabled: false, Base: "USD"},
		CoinGecko:        CoinGeckoVenueConfig{Enabled: false},
		CoinMarketCap:    CoinMarketCapVenueConfig{Enabled: false},
		CryptoCompare:    CryptoCompareVenueConfig{Enabled: false},
		ECB:              ExternalVenueConfig{Enabled: false},
		Chainlink:        ChainlinkVenueConfig{Enabled: false, FeedMap: map[string]ChainlinkFeedSetting{}},
		Tiingo:           TiingoVenueConfig{Enabled: false},
	}
}
