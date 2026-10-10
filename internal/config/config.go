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

// Config is the root configuration for every Stellar Index binary.
//
// Fields carry four struct tags:
//
//   - `toml:"…"`       — wire name in the TOML file
//   - `doc:"…"`        — one-line description (required; lint checks)
//   - `env:"…"`        — optional env-var override
//   - `default:"…"`    — default value (for documentation + loader)
//
// Adding a field without `doc:` fails `make docs-config`.
type Config struct {
	Region        RegionConfig        `toml:"region" doc:"Region identity — ID, display name, home domain."`
	Stellar       StellarConfig       `toml:"stellar" doc:"Endpoints for stellar-core and stellar-rpc."`
	Storage       StorageConfig       `toml:"storage" doc:"Postgres/TimescaleDB, Redis, MinIO connection details."`
	Ingestion     IngestionConfig     `toml:"ingestion" doc:"Source orchestration — which connectors to run, backfill bounds, cursor store."`
	Oracle        OracleConfig        `toml:"oracle" doc:"On-chain oracle contract addresses (Reflector, Redstone, Band)."`
	External      ExternalConfig      `toml:"external" doc:"Off-chain connectors — CEX/FX/aggregator sources that run parallel to the on-chain dispatcher."`
	Aggregate     AggregateConfig     `toml:"aggregate" doc:"VWAP/TWAP windows + outlier thresholds."`
	Anomaly       AnomalyConfig       `toml:"anomaly" doc:"Per-asset-class anomaly detection thresholds (Phase 1) + Phase-2 freeze thresholds (per-asset MAD-baseline + multi-factor confidence + source count). Both layers run; either freezes on its own (Phase 1: class freeze_pct AND source_count<=1; Phase 2: the 3-signal AND) and both share one freeze lifecycle (ADR-0019)."`
	API           APIConfig           `toml:"api" doc:"Public API serving plane — port, auth mode, rate limits, CDN."`
	Metadata      MetadataConfig      `toml:"metadata" doc:"Asset metadata overlay — SEP-1 issuer→home-domain map, operator overrides."`
	Supply        SupplyConfig        `toml:"supply" doc:"Supply pipeline config — SDF reserve list, operator-managed reserve balances (fallback when the LCM AccountEntry observer hasn't yet covered the watched set), watched classic + SEP-41 asset lists, SAC wrappers, and aggregator-refresh cadence. ADR-0011 (XLM) + ADR-0022 (classic) + ADR-0023 (SEP-41)."`
	Trades        TradesConfig        `toml:"trades" doc:"Trade-insert policy — operator-declared USD-pegged stablecoins so on-chain DEX trades populate trades.usd_volume at insert time (launch-readiness L2.2 phase 1)."`
	PricingGuard  PricingGuardConfig  `toml:"pricing_guard" doc:"Serving-side price guards — the thin-market substance gate that withholds aggregated price claims for on-chain pairs whose trailing market activity is below the serve floor (2026-08-04 valuation incident)."`
	DecimalsGuard DecimalsGuardConfig `toml:"decimals_guard" doc:"internal/decimalsguard's one-time startup backfill pass — how far back it scans trade history to self-seed nonstandard_decimals_assets for Soroban tokens that traded and then went dormant."`
	Divergence    DivergenceConfig    `toml:"divergence" doc:"Cross-check references the divergence service consults (CoinGecko + Chainlink HTTP, plus the on-chain Reflector/Redstone/Band oracle feeds read from ingested oracle_updates rows). Empty disables; the divergence_warning envelope flag stays unset."`
	PriceAlerts   PriceAlertsConfig   `toml:"price_alerts" doc:"Customer price-threshold alert evaluator. Off by default; when enabled the aggregator sweeps price_alerts against the latest closed VWAP every tick and enqueues price.alert webhook deliveries."`
	SignupReaper  SignupReaperConfig  `toml:"signup_reaper" doc:"F-1255 speculative-account reaper. Deletes orphan accounts left by a lost signup race (Suspended with a 'signup-race:' reason, no user, no key). Runs in the API binary when the dashboard is wired. On by default — the rows are pure garbage."`
	HashDB        HashDBConfig        `toml:"hashdb" doc:"Drift detector — on-disk (ledger_seq -> sha256(LCM)) record appended by the indexer's live ingest loop and periodically re-verified against a fresh re-read of the same bucket, catching upstream rewrites of previously-fetched ledger bytes. Off by default (opt-in first deploy)."`
	Obs           ObsConfig           `toml:"obs" doc:"Metrics, logs, traces — exporters + sampling."`
}

// HashDBConfig gates the hashdb drift detector (internal/hashdb): an on-disk
// ledger_seq → sha256(LCM) record plus a verifier that re-reads a recent
// window, catching an upstream rewrite of already-fetched ledger bytes that
// chain-link and signed-history checks cannot (see hashdb's "Trust model").
// One Enabled flag covers both halves: either alone detects nothing.
type HashDBConfig struct {
	// Enabled starts hashdb.Append in the indexer's live LCM read
	// loop AND the indexer's periodic verify sweep. Off by default;
	// operators opt in per region.
	Enabled bool `toml:"enabled" doc:"Start the hashdb append-on-ingest + periodic verify sweep in the indexer. Off by default — opt in per region once proven." default:"false"`

	// Path is the on-disk hashdb file. Default matches the indexer's
	// existing ReadWritePaths mount (see
	// configs/ansible/.../stellarindex-indexer.service.j2, which
	// already grants /var/lib/stellarindex — the same mount
	// verify-archive's state file uses).
	Path string `toml:"path" doc:"Filesystem path of the hashdb file (ledger_seq -> sha256(LCM)). Created on first run if missing." default:"/var/lib/stellarindex/hashdb.bin"`

	// VerifyIntervalMinutes is the gap between periodic verify
	// sweeps. 0 falls back to the indexer's default (60m) rather than
	// reaching time.NewTicker(0) at runtime.
	VerifyIntervalMinutes int `toml:"verify_interval_minutes" doc:"Minutes between hashdb verify sweeps. 0 = the indexer default (60)." default:"60"`

	// VerifyWindowLedgers is how many trailing ledgers each sweep
	// re-reads from the bucket and re-verifies against hashdb. 0
	// falls back to the indexer's default (20000 — roughly a day of
	// ledger closes at ~5s/ledger). Kept well below the indexer's
	// live-append edge (see the SafetyMargin in the verify loop) so
	// the sweep never races an in-flight Append for the same ledger.
	VerifyWindowLedgers uint32 `toml:"verify_window_ledgers" doc:"Trailing ledger count each verify sweep re-checks against hashdb. 0 = the indexer default (20000, ~1 day)." default:"20000"`
}

// validate enforces HashDBConfig's constraints only when Enabled —
// a disabled block with zero-value fields is fine (nothing consumes
// them). Same pattern as SignupReaperConfig.validate /
// PriceAlertsConfig.validate.
func (hc HashDBConfig) validate() error {
	if !hc.Enabled {
		return nil
	}
	if hc.Path == "" {
		return fmt.Errorf("hashdb: path must be set when enabled")
	}
	if hc.VerifyIntervalMinutes < 0 {
		return fmt.Errorf("hashdb: verify_interval_minutes must be >= 0, got %d", hc.VerifyIntervalMinutes)
	}
	// Above ~1 day between sweeps the periodic half of the hashdb drift
	// detector stops meaningfully bounding how long a rewrite can go
	// unnoticed. 0 defers to the indexer's 60m default, so it's exempt
	// from the ceiling.
	if hc.VerifyIntervalMinutes > maxHashDBVerifyIntervalMinutes {
		return fmt.Errorf("hashdb: verify_interval_minutes must be <= %d (24h), got %d",
			maxHashDBVerifyIntervalMinutes, hc.VerifyIntervalMinutes)
	}
	// Unbounded, VerifyWindowLedgers can exceed the chain's whole
	// height: every tick then re-streams and re-hashes the ENTIRE
	// archive from verifyDB.StartLedger() instead of a trailing
	// window, hammering the live bucket on the same cadence meant for
	// a ~day-sized re-check. 0 defers to the 20000 (~1 day) library
	// default, so it's exempt from the ceiling.
	if hc.VerifyWindowLedgers > maxHashDBVerifyWindowLedgers {
		return fmt.Errorf("hashdb: verify_window_ledgers must be <= %d (~10 days), got %d",
			maxHashDBVerifyWindowLedgers, hc.VerifyWindowLedgers)
	}
	return nil
}

// maxHashDBVerifyIntervalMinutes / maxHashDBVerifyWindowLedgers cap
// HashDBConfig's two tuning knobs at roughly 10x their library
// defaults (60m / 20000 ledgers, see defaultHashDBConfig) — enough
// headroom for a slower region without letting a config typo turn
// the periodic verify sweep into a rare, archive-wide re-read.
const (
	maxHashDBVerifyIntervalMinutes = 24 * 60
	maxHashDBVerifyWindowLedgers   = 200000
)

// SignupReaperConfig gates the speculative-account reaper
// (internal/signupreaper). The reaper deletes orphan `accounts` rows
// left behind when two concurrent /v1/auth/callback provisions raced
// for the same just-verified email: the loser's account is Suspended
// with a `signup-race:` reason and never gets a user attached. Those
// rows are unambiguous garbage (no users, no api_keys), so the reaper
// is ON by default. It runs in the API binary and only starts when the
// dashboard bundle (Postgres platform store) is wired.
type SignupReaperConfig struct {
	// Enabled starts the reaper loop in the API binary. On by default.
	Enabled bool `toml:"enabled" doc:"Start the speculative-account reaper loop in the API binary. On by default — the reaped rows (Suspended signup-race orphans with no user/key) are pure garbage. Set false to disable." default:"true"`

	// IntervalMinutes is the sweep cadence. Signup-race orphans are
	// rare, so hourly is ample. 0 falls back to the library default
	// (60m). Validated >= 0 when Enabled.
	IntervalMinutes int `toml:"interval_minutes" doc:"Minutes between reaper sweeps. 0 = library default (60)." default:"60"`

	// MinAgeMinutes is how long an orphan must have been suspended
	// before it is eligible for deletion — a safety window well past
	// any in-flight signup race. 0 falls back to the library default
	// (1440m = 24h). Validated >= 0 when Enabled.
	MinAgeMinutes int `toml:"min_age_minutes" doc:"Minimum minutes a suspended orphan must age before the reaper deletes it (safety window). 0 = library default (1440 = 24h)." default:"1440"`
}

// validate is the sub-validator hook Config.Validate calls. Only
// enforces constraints when the reaper is enabled.
func (sc SignupReaperConfig) validate() error {
	if !sc.Enabled {
		return nil
	}
	if sc.IntervalMinutes < 0 {
		return fmt.Errorf("signup_reaper: interval_minutes must be >= 0, got %d", sc.IntervalMinutes)
	}
	if sc.MinAgeMinutes < 0 {
		return fmt.Errorf("signup_reaper: min_age_minutes must be >= 0, got %d", sc.MinAgeMinutes)
	}
	return nil
}

// PriceAlertsConfig gates the aggregator's price-alert evaluator
// (internal/pricealerts). Off by default — the evaluator
// goroutine is only started when Enabled is true AND the platform v1
// schema (migration 0027) + price_alerts table (migration 0080) are
// present. When off, the price-alert CRUD surface still mounts on the
// API binary (customers can register alerts); nothing evaluates them
// until an operator flips this on.
type PriceAlertsConfig struct {
	// Enabled starts the evaluator loop in the aggregator binary.
	Enabled bool `toml:"enabled" doc:"Start the price-alert evaluator loop in the aggregator. Off by default." default:"false"`

	// IntervalSeconds is the sweep cadence — the gap between successive
	// passes over the enabled price_alerts set. 0 falls back to the
	// library default (30s). Validated > 0 when Enabled so an operator
	// enabling the worker with a zero cadence fails at boot rather than
	// reaching time.NewTicker(0) at runtime.
	IntervalSeconds int `toml:"interval_seconds" doc:"Sweep cadence in seconds between price-alert evaluation passes. 0 = library default (30s)." default:"30"`
}

// validate is the sub-validator hook Config.Validate calls. Only
// enforces constraints when the worker is enabled — a disabled worker
// with a zero cadence is fine (the field is simply unused).
func (pc PriceAlertsConfig) validate() error {
	if !pc.Enabled {
		return nil
	}
	if pc.IntervalSeconds < 0 {
		return fmt.Errorf("price_alerts: interval_seconds must be >= 0, got %d", pc.IntervalSeconds)
	}
	return nil
}

// TradesConfig configures policy that runs at trade-insert time
// (`internal/storage/timescale.Store.InsertTrade`). All fields are
// optional — empty config preserves the off-chain-only `usd_volume`
// behaviour.
type TradesConfig struct {
	// USDPeggedClassicAssets is the operator's allow-list of classic
	// credit assets (canonical "CODE-ISSUER" wire form, e.g.
	// "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	// they trust as 1.0-USD-pegged. On-chain trades quoted in any
	// of these assets — or in the SAC contract that wraps any of
	// them, looked up via [SupplyConfig.SACWrappers] — populate
	// `trades.usd_volume` at insert time using `quote_amount /
	// 10^7` (the Stellar classic decimal scale).
	//
	// Empty means only off-chain CEX/FX trades populate `usd_volume`;
	// on-chain DEX trades store NULL. See the lower-bound caveat on
	// the volume_24h_usd OpenAPI surface.
	USDPeggedClassicAssets []string `toml:"usd_pegged_classic_assets" doc:"Classic credit asset_keys (CODE-ISSUER) the operator declares as USD-pegged stablecoins. On-chain DEX trades quoted in these (or their SAC wrappers, transitive via [supply.sac_wrappers]) populate trades.usd_volume at insert time. Empty preserves the off-chain-only default." default:"[]"`
}

// validate requires every declared USD peg to be a CLASSIC credit asset
// (CODE-ISSUER) at load. usd_volume and the stablecoin→fiat proxy scale a
// peg by 10^7, correct only for 7-decimal classic assets; a Soroban token or
// ticker would mis-scale usd_volume by orders of magnitude. It also makes
// `TradesConfig.USDPeggedClassics`' soft-skip unreachable, matching
// `timescale.NewUSDVolumeQuoteSpec`'s hard error.
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

// USDPeggedClassics parses USDPeggedClassicAssets, logging and skipping any
// entry validate would reject: a missing peg beats refusing to start.
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

// PricingGuardConfig configures internal/pricingguard's thin-market SUBSTANCE
// gate: every raw prices_1m serving path (/v1/price + batch, /v1/price/tip,
// the SEP-40 passthrough, the GlobalAssetView headline, price alerts) withholds
// a price for a pair with an on-chain leg unless its trailing market clears a
// volume + persistence floor. Anyone can author a whole DEX market, baseline
// included, so a low-volume PRICE is not publishable; low volume itself stays
// visible on /v1/ohlc, /v1/observations and /v1/history. Zero values use the
// defaults in internal/pricingguard/substance.go.
type PricingGuardConfig struct {
	// DisableSubstanceGate switches the gate off entirely (every pair
	// serves). An operator escape hatch for
	// diagnosing a suspected false-withhold — not a tuning knob; to
	// loosen the gate, lower the floors instead.
	DisableSubstanceGate bool `toml:"disable_substance_gate" doc:"Disable the thin-market substance gate entirely (serve every pair, pre-2026-08 behaviour). Diagnostic escape hatch, not a tuning knob." default:"false"`

	// SubstanceMinVolumeUSD is the minimum trailing-window USD volume.
	SubstanceMinVolumeUSD float64 `toml:"substance_min_volume_usd" doc:"Minimum trailing-window USD volume (summed over prices_1m.volume_usd, both directions, alias union) below which an aggregated price is withheld. 0 = pricingguard default (1000)." default:"1000"`

	// SubstanceMinBuckets is the minimum count of distinct closed
	// 1-minute buckets with a trade in the window.
	SubstanceMinBuckets int `toml:"substance_min_buckets" doc:"Minimum distinct closed 1-minute buckets with at least one trade in the trailing window. 0 = pricingguard default (20)." default:"20"`

	// SubstanceMinSpanMinutes is the minimum wall-clock spread between
	// the oldest and newest active bucket in the window — the
	// cross-timeframe persistence floor.
	SubstanceMinSpanMinutes int `toml:"substance_min_span_minutes" doc:"Minimum minutes between the oldest and newest active 1m bucket in the trailing window (persistence floor — a one-burst market never clears it). 0 = pricingguard default (360)." default:"360"`

	// SubstanceWindowHours is the trailing measurement window.
	SubstanceWindowHours int `toml:"substance_window_hours" doc:"Trailing measurement window in hours. 0 = pricingguard default (24)." default:"24"`

	// FXCrossMaxAgeHours bounds the age of the forex snapshot's matched
	// rate the /v1/price fiat-cross-rate and USD-anchored-fiat-cross
	// fallbacks (ADR-0051) may serve. The in-memory forex Cache never
	// expires on its own, so without this bound a stalled forex
	// worker would keep answering forever from its last good fetch,
	// stamped with a fresh-looking observed_at.
	FXCrossMaxAgeHours int `toml:"fx_cross_max_age_hours" doc:"Staleness budget in hours for the forex snapshot rate backing /v1/price's fiat-cross-rate and USD-anchored-fiat-cross fallbacks; older than this is refused rather than served. Mirrors aggregate.composite_reference.fx_max_age_hours (same fx_quotes staleness profile — daily buckets that pause over market closes). 0 = pricingguard default (76)." default:"76"`

	// DisableFiatBasis switches off the ADR-0053 basis rule, so a
	// single-venue fiat book is served even where a multi-venue USD
	// leg could anchor the price. Kill switch, not a tuning knob.
	DisableFiatBasis bool `toml:"disable_fiat_basis" doc:"Disable the ADR-0053 fiat basis rule: /v1/price and /v1/price/batch serve a single-venue direct fiat book as-is instead of the USD-anchored derivation (multi-venue USD leg × bound FX fixing). Kill switch, not a tuning knob." default:"false"`

	// FiatPeggedClassicAssets maps a classic asset_key (CODE-ISSUER) to the
	// ISO-4217 ticker the OPERATOR declares it 1:1-pegged to (AUDD-G… → "AUD").
	// When no market price survives the substance gate, asset surfaces fill
	// `price_usd` from fx_quotes with `price_basis: "declared_peg"`; it never
	// overwrites a market price. Validated at load: classic asset, known fiat
	// (ADR-0010).
	FiatPeggedClassicAssets map[string]string `toml:"fiat_pegged_classic_assets" doc:"Maps classic credit asset_keys (CODE-ISSUER) to the ISO-4217 fiat ticker the operator declares them 1:1-pegged to (e.g. AUDD-G… = \"AUD\"). The API fills the asset's listing/detail price_usd from the declared peg × current fiat→USD FX rate when no market-derived price survives the substance gate, stamped price_basis=\"declared_peg\" on the wire. Never overwrites a market-derived price. Empty disables the fill." default:"{}"`
}

// validate rejects negative floors — a negative value is always a
// typo, and silently treating it as "use default" (the 0 convention)
// would hide it.
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
	// Declared fiat pegs: fail at load, loudly, like the
	// usd_pegged_classic_assets sibling (TradesConfig.validate). A
	// non-classic key or an unknown fiat ticker is always an operator
	// typo, and a silently-skipped peg would just leave the asset
	// priceless with no signal why.
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

// maxSubstanceWindowHours caps substance_window_hours at 400 days: far
// above any sane trailing window, far below the ~2,562,047 h at which
// time.Duration(h)*time.Hour wraps negative and the gate fails open.
const maxSubstanceWindowHours = 400 * 24

// validateSubstanceSatisfiable rejects floor combinations no market can
// clear (every pair withheld) or a window that overflows time.Duration
// (every pair served unguarded). The window is `bucket >= now-window AND
// bucket <= now-1min`; at any instant not on a minute boundary — nearly
// every read — that admits window*60-1 closed 1m buckets spanning
// window*60-2 minutes, so those are the largest floors a market can
// reliably clear. A 0 knob is checked at the default it resolves to.
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

// defaultPricingGuardConfig mirrors the pricingguard.DefaultSubstance*
// constants (config cannot import pricingguard — it depends on this
// package); TestDefaultPricingGuard_MatchesPricingguardConstants pins the
// lockstep, TestDefault_MatchesStructTags pins the `default:` doc tags.
func defaultPricingGuardConfig() PricingGuardConfig {
	return PricingGuardConfig{
		SubstanceMinVolumeUSD:   1000,
		SubstanceMinBuckets:     20,
		SubstanceMinSpanMinutes: 360,
		SubstanceWindowHours:    24,
		FXCrossMaxAgeHours:      76,
	}
}

// DecimalsGuardConfig configures internal/decimalsguard's startup backfill
// pass (Guard.Backfill), which seeds non-7-decimal Soroban tokens that went
// dormant before the periodic sweep's fixed 20-minute window saw them (token
// CC2RB…, decimals()=9, went unseeded for weeks).
type DecimalsGuardConfig struct {
	// BackfillWindowDays bounds the startup pass's index-sargable scan for
	// Soroban-legged (source, asset) pairs. 0 => 90
	// (decimalsguard.DefaultBackfillWindow). Tokens dormant longer than this need
	// the dex.md runbook's manual seed.
	BackfillWindowDays int `toml:"backfill_window_days" doc:"How many days of trade history the decimals-guard's one-time startup backfill pass scans for distinct Soroban-legged (source, asset) pairs, to self-seed nonstandard_decimals_assets for tokens that traded and then went dormant. 0 = library default (90)." default:"90"`
}

// validate is the sub-validator hook Config.Validate calls.
func (dc DecimalsGuardConfig) validate() error {
	if dc.BackfillWindowDays < 0 {
		return fmt.Errorf("%w: decimals_guard: backfill_window_days must be >= 0, got %d", ErrInvalidConfig, dc.BackfillWindowDays)
	}
	return nil
}

// DivergenceConfig wires the divergence cross-check references. The
// AGGREGATOR constructs them, refreshes each pair per cycle and writes Redis;
// the API builds a reference-less Service only to read that cache and set
// `divergence_warning` past [Threshold]. No references means no entries.
// Default() enables CoinGecko (no auth) so detection fires out of the box.
type DivergenceConfig struct {
	// Threshold is the divergence percentage above which
	// WarningFired is true on the cached result. Forwarded to
	// divergence.ServiceOptions.Threshold. Default 5.0 (5%).
	Threshold float64 `toml:"threshold_pct" doc:"Divergence percentage above which the warning flag fires." default:"5.0"`

	// MinSourcesForWarning is the minimum number of successful
	// references required before WarningFired can be true.
	// Default 2 — a single dissenting source isn't enough.
	MinSourcesForWarning int `toml:"min_sources_for_warning" doc:"Minimum successful references before warning_fired can be true." default:"2"`

	// CoinGecko config. Always-on by default (free tier, no
	// auth). Set Enabled=false to skip.
	CoinGecko DivergenceCoinGeckoConfig `toml:"coingecko" doc:"CoinGecko reference (free tier, no auth required)."`

	// Chainlink config. Off by default (operator must provide
	// FeedMap with mainnet feed addresses). When enabled, queries
	// public Ethereum RPC for AggregatorV3 latestAnswer().
	Chainlink DivergenceChainlinkConfig `toml:"chainlink" doc:"Chainlink reference (HTTP cross-check only; not a VWAP contributor)."`

	// On-chain oracle references — read our OWN ingested
	// oracle_updates rows (served tier) and compare the oracle's
	// latest value against our VWAP for pairs both sides cover.
	// On by default: they consume no external quota and are a
	// no-op (asset_unsupported per pair) until the feed tables
	// hold data. Reflector toggles all three variant references
	// (reflector-dex / reflector-cex / reflector-fx) together —
	// they're one protocol across three contracts.
	Reflector DivergenceOracleConfig `toml:"reflector" doc:"Reflector on-chain oracle references (reflector-dex/cex/fx) read from ingested oracle_updates rows."`
	Redstone  DivergenceOracleConfig `toml:"redstone" doc:"Redstone on-chain oracle reference read from ingested oracle_updates rows."`
	Band      DivergenceOracleConfig `toml:"band" doc:"Band on-chain oracle reference read from ingested oracle_updates rows (relay/force_relay op-args ingest)."`

	// Supply cross-check — compares OUR served circulating_supply
	// against an external authoritative reference (Stellar Network
	// Dashboard / CoinGecko), separate from the price cross-checks
	// above. Off by default; the archival-node role renders enabled from
	// the stellarindex_divergence_supply_enabled inventory variable.
	Supply DivergenceSupplyConfig `toml:"supply" doc:"Supply cross-check: compare our served circulating_supply against the Stellar Network Dashboard (XLM) and/or CoinGecko. Catches a stale SDF-reserve exclusion list. Off by default."`
}

// DivergenceSupplyConfig gates the supply cross-check worker — the
// automated counterpart to the manual "is our circulating supply
// right?" investigation (docs/methodology/xlm-circulating-supply.md).
// Off by default: it makes outbound HTTP calls, so a fresh deployment
// stays silent until the operator opts in. When enabled, it needs at
// least one enabled reference below or the worker refuses to start.
type DivergenceSupplyConfig struct {
	Enabled bool `toml:"enabled" doc:"Whether the supply cross-check worker runs. Off by default (makes outbound HTTP calls). The archival-node ansible role renders it from the stellarindex_divergence_supply_enabled inventory variable, itself default false." default:"false"`
	// ThresholdPct is the relative-divergence percentage above which
	// the ratio gauge reads `divergent` and the supply-divergence alert
	// fires. Default 1.0 — two-plus orders of magnitude above the
	// ~0.03% XLM Fee-Pool noise floor, so it fires only on a REAL drift.
	ThresholdPct float64 `toml:"threshold_pct" doc:"Relative-divergence percentage above which a supply cross-check reads 'divergent' and the alert fires. Default 1.0 (well above the ~0.03% XLM noise floor)." default:"1.0"`
	// Dashboard is the Stellar Network Dashboard reference (XLM only).
	// On by default WITHIN this block (free, no auth, authoritative) —
	// but the block itself is Enabled=false, so it only runs once the
	// operator flips the parent gate.
	Dashboard DivergenceSupplyDashboardConfig `toml:"dashboard" doc:"Stellar Network Dashboard reference (dashboard.stellar.org) — authoritative XLM circulating supply, free, no auth. On by default within this block."`
	// CoinGecko is the CoinGecko `/coins/{id}` circulating-supply
	// reference. Off by default — the free tier is 429-throttled;
	// enable it once a Pro key is set.
	CoinGecko DivergenceSupplyCoinGeckoConfig `toml:"coingecko" doc:"CoinGecko /coins/{id} market_data.circulating_supply reference. Off by default (free tier 429-throttled since 2026-06-19; enable with a Pro key)."`
}

// DivergenceSupplyDashboardConfig configures the Stellar Dashboard
// supply reference. Covers XLM only; every other asset is
// asset_unsupported for this reference.
type DivergenceSupplyDashboardConfig struct {
	Enabled bool   `toml:"enabled" doc:"Whether the Stellar Dashboard supply reference is consulted. On by default within [divergence.supply]." default:"true"`
	BaseURL string `toml:"base_url" doc:"Dashboard API base. Empty defaults to https://dashboard.stellar.org/api/v3. The reference GETs base_url + /lumens." default:""`
}

// DivergenceSupplyCoinGeckoConfig configures the CoinGecko supply
// reference. Distinct from [DivergenceCoinGeckoConfig] (the price
// reference): supply reads `/coins/{id}` not `/simple/price`.
type DivergenceSupplyCoinGeckoConfig struct {
	Enabled bool `toml:"enabled" doc:"Whether the CoinGecko supply reference is consulted. Off by default (free tier 429-throttled)." default:"false"`
	// APIKey follows the secret-field convention: prefer the env var;
	// TOML fallback for local-dev only. Sent as the x-cg-pro-api-key
	// header (Pro-tier auth that lifts the 429 ceiling).
	APIKey  string            `toml:"api_key" doc:"CoinGecko Pro API key, sent as x-cg-pro-api-key. Prefer env var COINGECKO_API_KEY." env:"COINGECKO_API_KEY" default:""`
	BaseURL string            `toml:"base_url" doc:"CoinGecko API base. Empty defaults to https://api.coingecko.com/api/v3, or https://pro-api.coingecko.com/api/v3 when api_key is set (a Pro key 404s the public host)." default:""`
	IDMap   map[string]string `toml:"id_map" doc:"Maps canonical asset_id → CoinGecko coin id for the supply lookup. Empty falls back to the built-in default (native/crypto:XLM → stellar)." default:"{}"`
}

// DivergenceOracleConfig gates one on-chain oracle reference family
// for the divergence service. Unlike the HTTP references these read
// the SERVED oracle_updates rows our own indexer ingested, so
// enabling them costs nothing when the tables are empty — every
// lookup records asset_unsupported for the pair until rows exist.
type DivergenceOracleConfig struct {
	Enabled bool `toml:"enabled" doc:"Whether this on-chain oracle reference is wired into the divergence service." default:"true"`
}

// DivergenceCoinGeckoConfig configures the CoinGecko reference.
// IDMap is operator-overridable but defaults to the small built-in
// set covering XLM + the major stablecoins we curate.
type DivergenceCoinGeckoConfig struct {
	Enabled bool              `toml:"enabled" doc:"Whether the CoinGecko reference is wired into the divergence service." default:"true"`
	BaseURL string            `toml:"base_url" doc:"CoinGecko API base URL. Empty defaults to https://api.coingecko.com/api/v3, or https://pro-api.coingecko.com/api/v3 when external.coingecko.api_key is set. The reference authenticates with the external.coingecko keys." default:""`
	IDMap   map[string]string `toml:"id_map" doc:"Maps canonical asset_id → CoinGecko slug. Operator-curated; empty falls back to the built-in default covering XLM + major stables." default:"{}"`
}

// DivergenceChainlinkConfig configures the Chainlink reference.
// Off by default — operator opts in by setting RPCURL + FeedMap
// to mainnet AggregatorV3 feed addresses for the pairs they want
// cross-checked.
type DivergenceChainlinkConfig struct {
	Enabled bool   `toml:"enabled" doc:"Whether the Chainlink reference is wired into the divergence service." default:"false"`
	RPCURL  string `toml:"rpc_url" doc:"Ethereum JSON-RPC endpoint. Shares the CHAINLINK_RPC_URL env var with the ingest poller (env overrides TOML). Empty defaults to https://cloudflare-eth.com." env:"CHAINLINK_RPC_URL" default:""`
	// FeedMap maps canonical pair string → mainnet feed address.
	// Pair string format: "<base>/<quote>" e.g. "fiat:EUR/fiat:USD".
	// Decimals omitted → the feed's on-chain decimals() is adopted; set →
	// verified against decimals(), refused on disagreement.
	FeedMap map[string]ChainlinkFeedConfig `toml:"feeds" doc:"Maps pair strings to {address, decimals, invert}. Empty disables Chainlink in practice." default:"{}"`
}

// ChainlinkFeedConfig is one entry in the [DivergenceChainlinkConfig.FeedMap].
type ChainlinkFeedConfig struct {
	Address  string `toml:"address" doc:"0x-prefixed mainnet feed contract address." default:""`
	Decimals int    `toml:"decimals" doc:"Power-of-10 divisor for the raw int256. Omit to adopt the feed's on-chain decimals() (8 on every Chainlink USD feed). When set it is verified against decimals() on first use and daily; on disagreement the feed's readings are refused (ERROR log + stellarindex_chainlink_feed_decimals_mismatch_total) until they agree." default:"0"`
	Invert   bool   `toml:"invert" doc:"Set true when canonical pair is reciprocal of the feed's natural quote." default:"false"`
	// MaxAgeHours is the staleness ceiling: a latestRoundData
	// round older than this is rejected as reference-unavailable.
	// 0 = the default budget for the pair (see the doc tag).
	MaxAgeHours int `toml:"max_age_hours" doc:"Staleness ceiling in hours for the feed's latestRoundData updatedAt; rounds older than this are rejected as reference-unavailable (CS-089). 0 = the default: the built-in feed's budget for a built-in pair, 76h for any other fiat/fiat pair (FX feeds pause over market closes), else 3h. Must be >= 0." default:"0"`
}

// defaultDivergenceConfig returns the Default()-shape divergence
// settings: CoinGecko on (free tier, no auth required) so
// divergence_warning fires out of the box; Chainlink off by
// default (operator opts in via FeedMap).
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
		// On-chain oracle references default ON — they read our own
		// served oracle_updates rows (no external quota) and are a
		// per-pair no-op until the feed tables have data.
		Reflector: DivergenceOracleConfig{Enabled: true},
		Redstone:  DivergenceOracleConfig{Enabled: true},
		Band:      DivergenceOracleConfig{Enabled: true},
		// Supply cross-check OFF by default (outbound HTTP; opt in on
		// r1). The Dashboard sub-reference is on WITHIN the block —
		// enabling the parent gate gives XLM-vs-Dashboard out of the
		// box — while CoinGecko stays off (free tier 429-throttled).
		Supply: DivergenceSupplyConfig{
			Enabled:      false,
			ThresholdPct: 1.0,
			Dashboard:    DivergenceSupplyDashboardConfig{Enabled: true},
			CoinGecko:    DivergenceSupplyCoinGeckoConfig{Enabled: false, IDMap: map[string]string{}},
		},
	}
}

// MetadataConfig configures the asset-metadata overlay path. The API
// binary chains the curated issuer → home-domain map BEHIND the live
// LCM-derived resolver ([internal/metadata.ChainedHomeDomainLookup]),
// which reads the `account_observations` rows the
// `internal/sources/accounts` observer writes for the accounts it
// watches. An observed account's on-chain home_domain is final —
// including an observed absence — so the static map only answers for
// issuers the observer has not seen. On /v1/assets/{id} (and its
// /metadata route) the live ClickHouse account-state read also outranks
// the static map.
type MetadataConfig struct {
	// IssuerHomeDomains maps issuer-account G-strkey → home-domain.
	// E.g. `"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN" = "centre.io"`.
	// Empty entries (`""`) are equivalent to the key being absent.
	// TOML representation: `[metadata.issuer_home_domains]` table with
	// one entry per issuer.
	IssuerHomeDomains map[string]string `toml:"issuer_home_domains" doc:"Static curated map of issuer-account G-strkey → home-domain. Fallback for issuers with no AccountEntry observation; an observed on-chain home_domain, or an observed absence of one, always wins." default:"{}"`

	// WatchedIssuerAccounts are the issuer G-strkeys whose AccountEntry
	// the indexer observes for home_domain. Kept apart from
	// [SupplyConfig.SDFReserveAccounts], whose members are subtracted
	// from XLM circulating supply.
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

// HomeDomainFor returns the home-domain registered for the issuer,
// or ("", false) if the issuer isn't curated. Falsy entries (empty
// strings) are treated as "not curated."
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

// ExternalConfig controls off-chain connectors that live in
// internal/sources/external/. Each venue toggles via its own
// sub-struct; disabled by default so fresh deployments don't
// attempt network egress until the operator opts in.
//
// Pair lists are code, not config (see venue package's DefaultPairs);
// changing one is a pairs.go / pairs.yaml edit.
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

// ExternalStreamerConfig is the toggle shape for credential-less
// WebSocket streamers (Binance, Kraken, Bitstamp, Coinbase). They
// have no poll cadence to override — the connector holds an open
// socket — so unlike [ExternalVenueConfig] this carries no
// PollInterval field, and a `poll_interval` under `[external.binance]`
// et al. is an unknown key.
type ExternalStreamerConfig struct {
	Enabled bool `toml:"enabled" doc:"Whether this connector runs. Off by default — no network egress until operator opts in." default:"false"`
}

// ExternalVenueConfig is the toggle shape for credential-less public
// venues that DO poll on a cadence (currently ECB). Paid-tier venues
// with API keys use their own struct (e.g.
// [ExchangeRatesApiVenueConfig]) that embeds the same Enabled field.
type ExternalVenueConfig struct {
	Enabled bool `toml:"enabled" doc:"Whether this connector runs. Off by default — no network egress until operator opts in." default:"false"`
	// PollInterval overrides the connector's built-in default poll
	// cadence. Empty/zero falls back to whatever the connector
	// itself defines (e.g. ECB's 6h). Useful for tightening or
	// loosening a free-tier connector's cadence.
	PollInterval time.Duration `toml:"poll_interval" doc:"Override the connector's built-in default poll cadence (e.g. \"12h\"). Empty/zero uses the connector default." default:""`
}

// ExchangeRatesApiVenueConfig extends the common toggle with the
// API-key slot and base-currency override.
//
// APIKey follows the same secret-field convention as
// [StorageConfig.PostgresDSN]: the field holds the actual secret,
// and the `env:` tag names the env var that overrides it at
// [ApplyEnvOverrides] time. Production configs keep APIKey empty
// in the TOML and set the env var at the process level.
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

// CoinGeckoVenueConfig is [ExternalVenueConfig] plus CoinGecko's two key
// tiers. Pro wins when both are set.
type CoinGeckoVenueConfig struct {
	Enabled      bool          `toml:"enabled" doc:"Whether this connector runs. Off by default — no network egress until operator opts in." default:"false"`
	PollInterval time.Duration `toml:"poll_interval" doc:"Override the connector's built-in default poll cadence (e.g. \"120s\"). Empty/zero uses the connector default." default:""`
	APIKey       string        `toml:"api_key" doc:"CoinGecko Pro API key, sent as x-cg-pro-api-key against the pro-api host; wins over demo_api_key. Prefer env var." env:"COINGECKO_API_KEY" default:""`
	DemoAPIKey   string        `toml:"demo_api_key" doc:"CoinGecko Demo API key, sent as x-cg-demo-api-key. With api_key also empty, requests go out anonymously and are heavily 429-throttled. Prefer env var." env:"COINGECKO_DEMO_API_KEY" default:""`
}

// MassiveConfig carries the massive.com forex API key and the forex
// worker's poll cadence.
type MassiveConfig struct {
	APIKey          string        `toml:"api_key" doc:"massive.com API key. Empty still starts the forex worker, but every fetch 401s and /v1/currencies serves warming-up. Prefer env var." env:"MASSIVE_API_KEY" default:""`
	RefreshInterval time.Duration `toml:"refresh_interval" doc:"Forex worker poll cadence. One poll is one request to massive, plus one to each standby it falls through to. Zero uses 1h; values under 10m are raised to 10m and logged. Budget a metered feed against it: the Open Exchange Rates Free plan (1,000 requests/month, hourly updates) spends 720-744/month at 1h." default:"1h"`
}

// MinMassiveRefreshInterval floors [MassiveConfig.RefreshInterval]: every
// board in the chain updates at most hourly, so a faster poll only spends
// metered quota.
const MinMassiveRefreshInterval = 10 * time.Minute

// EffectiveRefreshInterval returns the forex worker's cadence: 1h when
// unset, raised to [MinMassiveRefreshInterval] (clamped=true) when below it.
func (m MassiveConfig) EffectiveRefreshInterval() (d time.Duration, clamped bool) {
	switch {
	case m.RefreshInterval <= 0:
		return time.Hour, false
	case m.RefreshInterval < MinMassiveRefreshInterval:
		return MinMassiveRefreshInterval, true
	default:
		return m.RefreshInterval, false
	}
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

// CoinMarketCapVenueConfig carries the CMC Pro API auth + toggle.
// APIKey follows the same env-override convention as the FX sources.
type CoinMarketCapVenueConfig struct {
	Enabled bool   `toml:"enabled" doc:"Whether this connector runs. Off by default." default:"false"`
	APIKey  string `toml:"api_key" doc:"CMC Pro API key, passed as X-CMC_PRO_API_KEY header. Prefer env var." env:"COINMARKETCAP_API_KEY" default:""`
}

// CryptoCompareVenueConfig carries the CryptoCompare API auth +
// toggle.
type CryptoCompareVenueConfig struct {
	Enabled bool   `toml:"enabled" doc:"Whether this connector runs. Off by default." default:"false"`
	APIKey  string `toml:"api_key" doc:"CryptoCompare API key, passed as 'Authorization: Apikey <KEY>'. Prefer env var." env:"CRYPTOCOMPARE_API_KEY" default:""`
}

// ChainlinkVenueConfig carries the Chainlink ingest connector
// settings.
//
// RPCUrl IS the credential — Alchemy / Infura encode the API key
// directly in the URL path (.../v2/<KEY>), so we treat the whole
// URL as the secret. Operator best-practice: leave this blank in
// any tracked TOML and set the env var at the process level.
type ChainlinkVenueConfig struct {
	Enabled      bool                            `toml:"enabled"       doc:"Whether the Chainlink ingest poller runs. Off by default."  default:"false"`
	RPCUrl       string                          `toml:"rpc_url"       doc:"Ethereum mainnet JSON-RPC endpoint (Alchemy / Infura / public). For Alchemy this includes the API key in the URL path (.../v2/<KEY>) — treat the whole value as a secret. Prefer env var." env:"CHAINLINK_RPC_URL" default:""`
	PollInterval time.Duration                   `toml:"poll_interval" doc:"Override the default 30s poll cadence. Empty/zero uses the package default." default:""`
	FeedMap      map[string]ChainlinkFeedSetting `toml:"feed_map"      doc:"Maps canonical pair string ('crypto:BTC/fiat:USD' etc.) to the AggregatorV3 contract address + decimals + invert + max_age_hours. Empty falls back to the built-in default covering BTC/ETH/LINK/EUR/GBP/JPY vs USD." default:"{}"`
}

// ChainlinkFeedSetting is one entry in [ChainlinkVenueConfig.FeedMap].
// Mirrors [DivergenceChainlinkConfig]'s ChainlinkFeed but kept on its
// own type so the operator-facing TOML schemas of the two consumers
// (divergence cross-check vs ingest source) can evolve independently.
type ChainlinkFeedSetting struct {
	Address  string `toml:"address"  doc:"0x-prefixed AggregatorV3 contract address on Ethereum mainnet."`
	Decimals uint8  `toml:"decimals" doc:"Power-of-10 divisor for the raw int256 answer. Omit to adopt the feed's on-chain decimals() (8 on every Chainlink USD feed). When set it is verified against decimals() on the first poll and daily; on disagreement the feed is refused (ERROR log + stellarindex_chainlink_feed_decimals_mismatch_total) until they agree." default:"0"`
	Invert   bool   `toml:"invert"   doc:"If true, the canonical pair is the reciprocal of the feed's natural quote — e.g. operator wants USD/EUR but the feed publishes EUR/USD. price → 1/price after scaling." default:"false"`
	// MaxAgeHours is the live-poll staleness budget; 0 = the pair's default.
	MaxAgeHours int `toml:"max_age_hours" doc:"Staleness budget in hours, set from the feed's heartbeat: a latestRoundData round whose updatedAt is older is refused (not written) and counted on stellarindex_chainlink_feed_polls_total{outcome=\"stale\"}. 0 = the default: the built-in feed's budget for a built-in pair (3h for the 1h-heartbeat crypto feeds, 76h for the 24h-heartbeat FX feeds, which pause over market closes), 76h for any other fiat/fiat pair, else 3h. Negative is rejected at startup. Backfill is not subject to it." default:"0"`
}

// OracleConfig gathers on-chain oracle contract addresses. Each
// provider nests its own sub-struct so the TOML reads naturally:
//
//	[oracle.reflector]
//	dex_contract = "C..."
//	cex_contract = "C..."
//	fx_contract  = "C..."
type OracleConfig struct {
	Reflector ReflectorOracleConfig `toml:"reflector" doc:"Reflector oracle contract addresses per variant (DEX / CEX / FX)."`
	Redstone  RedstoneOracleConfig  `toml:"redstone"  doc:"RedStone Adapter contract address (single adapter owns every feed)."`
	Band      BandOracleConfig      `toml:"band"      doc:"Band Protocol StandardReference contract address (Soroban-native, emits no events — observed via InvokeContract call args)."`
	Soroswap  SoroswapConfig        `toml:"soroswap"  doc:"Soroswap factory contract — used at boot to seed the pair→tokens registry via stellar-rpc view calls. Not required for live ingest, but without it the decoder skips swaps from pairs created before the first processed ledger."`

	StalenessOverrides []OracleStalenessOverrideConfig `toml:"staleness_overrides" doc:"Per-(source, asset) exceptions to the oracle-staleness budget the stellarindex_oracle_stale alert reads. Empty (default) leaves every asset on its source's default budget — 10 × the source's declared resolution. Each row is a written-down claim that ONE asset publishes on a different rhythm than its source's cadence; see OracleStalenessOverrideConfig." default:"[]"`
}

// OracleStalenessOverrideConfig replaces the staleness budget for one
// (source, asset) pair. `stellarindex_oracle_stale` defaults to 10 × the
// source's resolution, a per-SOURCE fact, but staleness is per-ASSET:
// `crypto:DAI` on Reflector's 300 s CEX oracle publishes only on moves and
// breached its 50-minute budget on 11.6% of evaluations with nothing broken.
// Not a mute: a real outage still tickets past the wider bound. Size it from
// observed gaps plus headroom and record the observation in Reason.
type OracleStalenessOverrideConfig struct {
	Source        string `toml:"source" doc:"Oracle source name exactly as it appears in the metric's source label: an on-chain oracle (reflector-dex, reflector-cex, reflector-fx, redstone, band) or an oracle_updates poller (chainlink, coingecko, coinmarketcap, cryptocompare, ecb, exchangeratesapi)."`
	Asset         string `toml:"asset" doc:"Canonical asset identifier exactly as it appears in the metric's asset label — \"crypto:DAI\", not \"DAI\". Oracle symbols pass through canonical.MapOracleSymbol (known fiat → fiat:CODE, known crypto → crypto:CODE, known RWA → rwa:CODE, anything else → raw:SYMBOL), so the label is the mapped form; a bare or non-round-tripping identifier is rejected at startup rather than silently matching no series."`
	BudgetSeconds int    `toml:"budget_seconds" doc:"Seconds this pair may go without a publication before the alert tickets, replacing the source default (10 × declared resolution). Must be > 0."`
	Reason        string `toml:"reason" doc:"Why this pair's publication rhythm differs from its source's cadence, with the observation behind the number. Required — an override without a stated reason is indistinguishable from a silenced alert."`
}

// ReflectorOracleConfig carries the three Reflector contract
// addresses. Leave any variant empty to disable it; the indexer's
// buildSources will reject an enabled source whose address is
// unset rather than silently no-op.
type ReflectorOracleConfig struct {
	DEXContract string `toml:"dex_contract" doc:"Reflector DEX contract (C-prefix) on mainnet."`
	CEXContract string `toml:"cex_contract" doc:"Reflector CEX contract (C-prefix) on mainnet."`
	FXContract  string `toml:"fx_contract"  doc:"Reflector FX contract (C-prefix) on mainnet."`
	DEXDecimals uint8  `toml:"dex_decimals" doc:"Price scale (power of 10) of the DEX contract's SEP-40 decimals(). 0 keeps the Reflector default of 14. Not read from the contract: confirm decimals() on-chain before re-pointing dex_contract." default:"0"`
	CEXDecimals uint8  `toml:"cex_decimals" doc:"Price scale (power of 10) of the CEX contract's SEP-40 decimals(). 0 keeps the Reflector default of 14. Not read from the contract: confirm decimals() on-chain before re-pointing cex_contract." default:"0"`
	FXDecimals  uint8  `toml:"fx_decimals"  doc:"Price scale (power of 10) of the FX contract's SEP-40 decimals(). 0 keeps the Reflector default of 14. Not read from the contract: confirm decimals() on-chain before re-pointing fx_contract." default:"0"`
}

// RedstoneOracleConfig carries the mainnet RedStone Adapter address.
// RedStone's 19 per-feed contracts are thin proxies that don't emit
// events (verified via stellar.expert's contract API) —
// all event activity is on the single Adapter, so one address is
// the full configuration surface. See docs/protocols/redstone.md.
type RedstoneOracleConfig struct {
	AdapterContract string `toml:"adapter_contract" doc:"RedStone Adapter contract (C-prefix) on mainnet — CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG."`
}

// BandOracleConfig carries the mainnet Band StandardReference
// address. Band's Stellar contract emits zero events — we observe
// `relay()` / `force_relay()` InvokeContract calls via the
// dispatcher's ContractCallDecoder interface. See
// docs/protocols/band.md.
type BandOracleConfig struct {
	StandardReferenceContract string `toml:"standard_reference_contract" doc:"Band Protocol StandardReference contract (C-prefix) on mainnet — CCQXWMZVM3KRTXTUPTN53YHL272QGKF32L7XEDNZ2S6OSUFK3NFBGG5M."`
}

// SoroswapConfig carries the factory address plus an optional stellar-rpc
// endpoint that seeds the pair → (token0, token1) registry at boot. Swap
// events carry no token identities; live new_pair events cover later pairs,
// and the seed covers pairs created before the dispatcher's first ledger.
// Empty FactoryContract disables the seed.
type SoroswapConfig struct {
	FactoryContract string `toml:"factory_contract" doc:"Soroswap factory contract (C-prefix) on mainnet — CA4HEQTL2WPEUYKYKCDOHCDNIV4QHNJ7EL4J4NQ6VADP7SYHVRYZ7AW2."`
	SeedRPCEndpoint string `toml:"seed_rpc_endpoint" doc:"stellar-rpc URL used for the boot-time factory sweep. Any public pubnet endpoint works (e.g. https://mainnet.sorobanrpc.com). Falls back to stellar.rpc_endpoints[0] when empty."`
}

// RegionConfig identifies the region this node belongs to, to tag
// metrics and decide replication direction.
type RegionConfig struct {
	ID string `toml:"id" doc:"Short region identifier, lowercase (r1/r2/r3)." default:"r1"`
	// Deployment is the tier label /v1/status reports in
	// `region.deployment` (and the ingestion diagnostics echo). The
	// default "production" is pubnet's wire value; the test-net
	// inventories set "testnet" / "futurenet" so the explorer's status
	// page does not tag a test net PRODUCTION.
	Deployment string `toml:"deployment" doc:"Deployment tier this node reports in /v1/status's region block (production / testnet / futurenet / staging). Purely a label — nothing keys behaviour off it." default:"production"`
}

// StellarConfig points a Stellar Index binary at the stellar-core +
// stellar-rpc endpoints it reads from. Empty values disable the
// corresponding client.
type StellarConfig struct {
	Network           string   `toml:"network" doc:"Network passphrase name — pubnet / testnet / futurenet." default:"pubnet"`
	RPCEndpoints      []string `toml:"rpc_endpoints" doc:"stellar-rpc endpoints for getEvents/getLedgers. Tried in order on failover. Default is a local, unkeyed node; a hosted third-party endpoint may embed an API key in the URL path/query — treat that value as a secret, same as chainlink's rpc_url." default:"[\"http://127.0.0.1:8000\"]"`
	HistoryArchiveURL string   `toml:"history_archive_url" doc:"Public history archive (SDF or ours) for backfill catchup." default:"https://history.stellar.org/prd/core-live/core_live_001"`

	// SorobanGenesisLedger and MovementsFloorLedger default to the pubnet
	// protocol-transition ledgers. A reset test net is post-Soroban and post-P23
	// from ledger 1, so both must be 1 there, or the SEP-41 supply observer and
	// CAP-67 movements feed floor above every ledger and produce nothing.
	// TestP23BoundaryConstantsAgree pins the defaults to the leaf constants.
	SorobanGenesisLedger uint32 `toml:"soroban_genesis_ledger" doc:"Soroban (protocol-20) activation ledger — the pre-Soroban↔Soroban-era boundary the SEP-41 supply observer floors at. Defaults to the pubnet value; set to 1 (genesis) on testnet/futurenet." default:"50457424"`
	MovementsFloorLedger uint32 `toml:"movements_floor_ledger" doc:"P23 / CAP-67 boundary — at/above it the Postgres SEP-41 movements tail serves, below it the ClickHouse pre-P23 archive serves (ADR-0048 D5). Defaults to the pubnet value; set to 2 (ledger 1 has no predecessor) on testnet/futurenet." default:"58762517"`
}

// APIMaxHandlerBudget is the longest per-handler timeout any API handler
// asks for (v1's `maxHandlerBudget`); [APIConfig.validate] requires
// api.request_timeout to exceed it. Otherwise the blanket deadline reaches
// every reader first and the handlers' specific 503s become unreachable (the
// bodyless-200 class). Declared here because internal/config must not import
// internal/api (lint-imports L/api-scope); TestMaxHandlerBudgetMatchesConfigBound
// keeps the two equal.
const APIMaxHandlerBudget = 12 * time.Second

// Well-known Stellar network passphrases. Aliased from
// internal/canonical (the single spelling every leaf package that
// keys network-dependent constants off the passphrase — e.g. the
// native-XLM total in internal/supply — compares against) so the
// value Passphrase() hands out can never drift from the value those
// packages switch on. internal/canonical keeps them local rather than
// pulling the SDK just for string constants.
const (
	pubnetPassphrase    = canonical.PubnetPassphrase
	testnetPassphrase   = canonical.TestnetPassphrase
	futurenetPassphrase = canonical.FuturenetPassphrase
)

// Passphrase translates the TOML-friendly short network name
// (pubnet / testnet / futurenet) into the full network passphrase
// string that the Stellar protocol actually uses everywhere —
// stellar-core, go-stellar-sdk datastore manifests, transaction
// signatures. Callers that talk to those subsystems must pass the
// passphrase, not the short name.
//
// Returns "" for unknown values; callers treat that as a config
// error. Validate() rejects unknown names at startup, so a real
// runtime "" here would mean someone bypassed validation.
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

// StorageConfig captures every persistent-store connection. DSN
// strings NEVER include passwords directly — use the `env:` tag
// pattern to reference a secret store.
type StorageConfig struct {
	PostgresDSN string `toml:"postgres_dsn" doc:"Postgres DSN; password resolved via env: prefix." env:"STELLARINDEX_POSTGRES_DSN" default:"postgres://stellarindex@127.0.0.1:5432/stellarindex?sslmode=disable"`
	// BackgroundStatementTimeout is the session statement_timeout the INDEXER
	// and AGGREGATOR pools set post-connect (timescale.OpenBackground), the
	// SQL-side runaway backstop. The 30m default must exceed every legitimate
	// unbounded query; heavy scans `SET LOCAL statement_timeout` their own value.
	// ops/migrate paths use plain timescale.Open and stay unbounded so multi-hour
	// migrations and backfills survive. 0 disables it.
	BackgroundStatementTimeout time.Duration `toml:"background_statement_timeout" doc:"Session-level Postgres statement_timeout applied to every connection in the long-running INDEXER and AGGREGATOR pools (via a post-connect SET), so a runaway background query is bounded SQL-side even after the Go ctx gives up (REC-08). Deliberately GENEROUS: it must exceed every legitimate query that does not set its own bound — the heavy batch scans SET LOCAL a longer value inside a transaction, which overrides this. Does NOT affect the one-shot ops/migrate/heavy-backfill pools (they open via plain Open and stay unbounded — a global timeout there would kill legitimate multi-hour migrations/backfills/reconcile, the rejected prior fix). 0 disables it (plain Open)." default:"30m"`
	RedisAddr                  string        `toml:"redis_addr" doc:"Redis master address host:port. Used when redis_sentinel_addrs is empty (single-node / direct mode). When sentinel addrs are set, this is ignored." default:"127.0.0.1:6379"`
	// Sentinel mode: when redis_sentinel_addrs is non-empty, the
	// client uses go-redis FailoverClient and asks Sentinel for the
	// current primary. Per ADR-0024 (Redis HA via Sentinel) this is
	// the production topology; redis_addr is the fallback for
	// dev/single-node deployments.
	RedisSentinelAddrs []string `toml:"redis_sentinel_addrs" doc:"List of Sentinel host:port addresses. Non-empty enables FailoverClient mode (production HA per ADR-0024); empty falls back to single-node redis_addr." default:"[]"`
	RedisMasterName    string   `toml:"redis_master_name" doc:"Sentinel master name as set in inventory (e.g. stellarindex-r1-cache). Required when redis_sentinel_addrs is non-empty." default:""`
	RedisPassword      string   `toml:"redis_password" doc:"The Redis password itself, NOT an env-var NAME to dereference — inject it via the STELLARINDEX_REDIS_PASSWORD env override (the project's standard secret path) rather than writing it to this file. The old key redis_password_env is a deprecated alias (accepted with a boot warning). Used as both requirepass (client auth) and SentinelPassword (sentinel auth) — same secret per the role." env:"STELLARINDEX_REDIS_PASSWORD" default:""`
	RedisUsername      string   `toml:"redis_username" doc:"Optional Redis ACL username. Empty (default) AUTHs as Redis's legacy 'default' user — same wire shape as redis_password alone. Set to 'stellarindex' (or the operator's per-component user) when redis_acl_lockdown is enabled in the ansible role (F-1213 audit-2026-05-12); without a username the broker-side ACL rejects the connection." default:""`
	// RedisClosedBucketChannel is the Redis pub/sub channel the
	// aggregator's redispub.Publisher writes closed-bucket VWAP events
	// to and the API's redispub.Subscriber listens on. Both
	// binaries pass this straight to redispub.NewPublisher /
	// NewSubscriber, which fall back to redispub.DefaultChannel when
	// empty. Only an operator running more than one StellarIndex deployment
	// against a shared Redis needs to set this, to keep the streams
	// partitioned (redispub's doc.go documents the channel as
	// "configurable"; this is the config key that makes it so).
	RedisClosedBucketChannel string `toml:"redis_closed_bucket_channel" doc:"Redis pub/sub channel the aggregator publishes closed-bucket VWAP events to and the API subscribes on. Empty (default) uses redispub.DefaultChannel (stellarindex:closed-bucket:v1). Set only when multiple deployments share one Redis and need partitioned streams." default:""`
	S3Endpoint               string `toml:"s3_endpoint" doc:"S3-compatible object-store endpoint (MinIO / AWS S3)." default:"http://127.0.0.1:9000"`
	S3Region                 string `toml:"s3_region" doc:"S3 region label (free-form for MinIO; AWS region name otherwise)." default:"r1"`
	S3BucketArchive          string `toml:"s3_bucket_archive" doc:"Immutable history-archive bucket name." default:"galexie-archive"`
	S3BucketLive             string `toml:"s3_bucket_live" doc:"Live Galexie export bucket name." default:"galexie-live"`
	// These hold the NAME of the env var that carries the credential, NOT
	// the credential itself — buildS3Client does os.Getenv(S3AccessKeyEnv).
	// They deliberately have NO `env:` tag: an `env:` tag means
	// "ApplyEnvOverrides replaces this field with the env var's VALUE", which
	// would overwrite the name with the secret and then os.Getenv(secret)→"".
	// The default already points at the canonical
	// env var, so an operator just exports STELLARINDEX_S3_ACCESS_KEY=<key>
	// and buildS3Client resolves it through this name.
	S3AccessKeyEnv string `toml:"s3_access_key_env" doc:"NAME of the env var holding the S3 access key ID (the value lives in that env var, not here)." default:"STELLARINDEX_S3_ACCESS_KEY"`
	S3SecretKeyEnv string `toml:"s3_secret_key_env" doc:"NAME of the env var holding the S3 secret access key (the value lives in that env var, not here)." default:"STELLARINDEX_S3_SECRET_KEY"`

	// Cold-tier LCM reads (ADR-0027): when S3ColdBucketArchive is set, reads
	// cascade hot MinIO → this READ-ONLY bucket via ledgerstream's
	// TieredDataStore; production target `aws-public-blockchain/v1.1/stellar/
	// ledgers/pubnet`. Enable only together with the first bulk trim.
	//
	// Region must be us-east-2: the client uses path style, and us-east-1
	// answers 301 PermanentRedirect for this bucket (`x-amz-bucket-region:
	// us-east-2`). The *_key_env pair holds env-var NAMES, both empty in
	// production (public-read). pipeline.NewColdDataStore resolves them; never
	// route through datastore.NewDataStore, whose ambient AWS chain would send
	// MinIO's keys to real AWS.
	S3ColdEndpoint      string `toml:"s3_cold_endpoint" doc:"Cold-tier S3 endpoint — must be the REGIONAL endpoint (the client is path-style). Empty disables tiering. Production (aws-public-blockchain): https://s3.us-east-2.amazonaws.com" default:""`
	S3ColdRegion        string `toml:"s3_cold_region" doc:"Cold-tier S3 region. Production (aws-public-blockchain): us-east-2 (verified 2026-07-25 — us-east-1 is wrong and 301s)" default:""`
	S3ColdBucketArchive string `toml:"s3_cold_bucket_archive" doc:"Cold-tier bucket + prefix for historical LCMs. Empty disables tiering. Production: aws-public-blockchain/v1.1/stellar/ledgers/pubnet" default:""`
	S3ColdAccessKeyEnv  string `toml:"s3_cold_access_key_env" doc:"NAME of the env var holding the cold-tier S3 access key (the value lives in that env var, not here). Empty = anonymous reads, which is correct for the public aws-public-blockchain bucket. Must be set together with s3_cold_secret_key_env." env:"" default:""`
	S3ColdSecretKeyEnv  string `toml:"s3_cold_secret_key_env" doc:"NAME of the env var holding the cold-tier S3 secret key (the value lives in that env var, not here). Empty = anonymous reads. Must be set together with s3_cold_access_key_env; a named-but-unset env var is a startup error, never a silent downgrade to anonymous." env:"" default:""`

	// ClickHouse Tier-1 lake (ADR-0034). When ClickHouseLiveSink is true the
	// indexer's real-time dual-sink (internal/storage/clickhouse.LiveSink)
	// writes each ledger's structural extract to ClickHouse inline, keeping the
	// lake within ~seconds of the chain (real-time, for the block explorer) vs
	// the ~10-min ch-live-catchup timer. The sink is non-blocking (drops under
	// CH pressure rather than stalling ingest); the catch-up timer backstops
	// drops. ON by default per ADR-0041: the lake is substrate, not an add-on, so a
	// deployment that cannot run ClickHouse opts OUT here rather than in.
	ClickHouseAddr     string `toml:"clickhouse_addr" doc:"ClickHouse native address host:port for the Tier-1 lake (ADR-0034); used by the indexer real-time dual-sink." default:"127.0.0.1:9300"`
	ClickHouseLiveSink bool   `toml:"clickhouse_live_sink" doc:"Enable the real-time ClickHouse dual-sink: the indexer writes each ledger's structural extract to CH inline (non-blocking), keeping the lake within ~seconds of the chain. ON by default (ADR-0041): the certified-lake substrate backs the coverage claim, the CH completeness path, and lake-derived supply — opt out only on deployments that cannot run ClickHouse, accepting the loss of all three." default:"true"`
	// ClickHouseProjectorSource feed-switch (ADR-0041): when true, the
	// projector reads forward events from the CH lake's contract_events instead
	// of the Postgres soroban_events landing zone, so soroban_events can be
	// decommissioned. Requires the dual-sink (ClickHouseLiveSink) so CH is
	// authoritative for forward events. ON by default per ADR-0041, matching the
	// production topology; turn it off wherever the dual-sink is off.
	ClickHouseProjectorSource bool `toml:"clickhouse_projector_source" doc:"Feed-switch: the projector reads forward events from the ClickHouse lake (contract_events) instead of Postgres soroban_events, enabling soroban_events decommission. Requires clickhouse_live_sink. ON by default (ADR-0041), matching the production topology." default:"true"`

	// ClickHouseServingUser isolates the API's per-request CH reads
	// (ADR-0048 D4): when set they authenticate as this user and run under the
	// bounded `api_serving` profile
	// (configs/ansible/roles/archival-node/tasks/20-clickhouse-serving-profile.yml)
	// instead of the ops_batch / live_daemon / `default` identity every other CH
	// connection resolves (internal/storage/clickhouse/ops_auth.go). Both empty
	// (the default) resolves the same way, safe before the profile exists.
	ClickHouseServingUser string `toml:"clickhouse_serving_user" doc:"ClickHouse username the API's serving reads (explorer endpoints, incl. GET /v1/accounts/{g}/movements) authenticate as (ADR-0048 D4). Empty (default) uses the environment's identity: STELLARINDEX_CLICKHOUSE_LIVE_USER when set, else ClickHouse's default user." default:""`
	// ClickHouseServingPassword holds the resolved password, not an
	// env-var NAME (the direct-value `env:` convention, same as
	// RedisPassword — see that field's doc comment — NOT the
	// name-of-env-var convention S3AccessKeyEnv uses).
	ClickHouseServingPassword string `toml:"clickhouse_serving_password" doc:"The ClickHouse serving user's password itself, NOT an env-var NAME to dereference — inject it via the STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD env override rather than writing it to this file. The old key clickhouse_serving_password_env is a deprecated alias (accepted with a boot warning). Empty (default) uses no password, matching an empty clickhouse_serving_user." env:"STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD" default:""`
}

// ColdTieringEnabled reports whether the cold-tier read path
// should be wired up. The flag is the presence/absence of the
// cold-bucket field — ADR-0027 §Decision "LCM_TIER_ENABLED=false"
// in its terms — so unset is the safe default for a deployment
// without a cold tier.
func (s StorageConfig) ColdTieringEnabled() bool {
	return s.S3ColdBucketArchive != ""
}

// IngestionConfig controls the indexer's source fleet.
type IngestionConfig struct {
	EnabledSources     []string `toml:"enabled_sources" doc:"List of source connector names to run on this indexer replica. See config.KnownSources for valid values." default:"[\"soroswap\",\"aquarius\",\"phoenix\"]"`
	BackfillFromLedger uint32   `toml:"backfill_from_ledger" doc:"Earliest ledger to backfill from; 0 = continue-from-persisted-cursor." default:"0"`

	// LiveSeamLedger is the first ledger written to the live bucket
	// (galexie-live). Ledgers below it live in the historical bucket
	// (galexie-archive); ledgers at or above live in galexie-live.
	// The indexer reads from archive for [from, seam-1] and from live
	// for [seam, ∞), in that order, when from < seam.
	//
	// Set to whatever galexie-append.sh passed as --start when
	// galexie.service first started writing — for r1, query
	// the running process args. 0 = no seam configured (the default);
	// indexer reads only galexie-live.
	LiveSeamLedger uint32 `toml:"live_seam_ledger" doc:"First ledger in the live bucket. Below this, indexer reads from galexie-archive. 0 disables the archive bucket entirely." default:"0"`

	// Projector is the ADR-0032 projection loop. When enabled it
	// runs in parallel with the dispatcher's per-source sinks
	// (Phase 3 mode) unless PersistPerSource=false makes it the
	// sole writer (Phase 4). Off by default.
	Projector ProjectorConfig `toml:"projector" doc:"ADR-0032 projector — tails soroban_events and writes per-source rows. Phase 3 runs in parallel with the dispatcher's existing per-source sinks; Phase 4 will flip it primary."`
}

// ProjectorConfig governs the ADR-0032 projection loop, which tails
// `soroban_events` (ADR-0029) and writes per-source rows via each protocol's
// decoder. In Phase-3 parallel mode the dispatcher writes the same PKs and
// conflicts absorb the duplicates. With Enabled=true and
// PersistPerSource=false the dispatcher skips projected events
// (`pipeline.SinkModeSkipProjected`); sdex, CEX/FX, band and supply observers
// still go through it. SoleWriter specs (internal/pipeline/source_spec.go)
// always route through the projector alone (pipeline.IsSoleWriterProjected).
// pipeline.VerifySoleWriterCAGGCoverage refuses Phase 4 until every
// aggregate's lookback covers pipeline.ProjectorStallBound, since a stall
// longer than a lookback never materializes the late rows.
type ProjectorConfig struct {
	Enabled          bool `toml:"enabled"            doc:"Master switch. When false the projector goroutines are not started." default:"false"`
	PersistPerSource bool `toml:"persist_per_source" doc:"When false (Phase 4+), the dispatcher's events-goroutine skips Soroban-derived events so the projector is sole writer. Requires Enabled=true. Defaults true (Phase 3 parallel mode); flipping it to false needs more than low projector lag: the indexer refuses to start in that mode unless every continuous aggregate's refresh start_offset covers the projector's stall bound (pipeline.VerifySoleWriterCAGGCoverage), because rows the projector delivers late are otherwise never materialized. Sources whose spec sets SoleWriter (internal/pipeline/source_spec.go) are exempt — the projector is always their sole writer." default:"true"`
}

// AnomalyConfig configures both ADR-0019 anomaly-detection phases, consulted
// at bucket close to publish, warn or freeze the VWAP: Phase 1 per-class
// thresholds (`internal/aggregate/anomaly/`) and Phase 2 per-asset MAD
// baseline + confidence (`baseline/`, `confidence/`). Either freezes on its
// own (Phase 1: freeze_pct breach with source_count<=1; Phase 2: its 3-signal
// AND); they share one freeze lifecycle.
type AnomalyConfig struct {
	// Enabled gates whether anomaly checks run at all. When false,
	// every bucket is published as-is (no warn / no freeze). Off by
	// default during initial roll-out to avoid surprise 401-with-
	// freeze responses; flip to true once the operator has
	// classified all assets.
	Enabled bool `toml:"enabled" doc:"Master switch. When false, anomaly checks are disabled and every bucket is published as-is. Flip to true after operator has classified the asset set." default:"false"`

	// Thresholds maps asset class → (warn_pct, freeze_pct). Empty
	// or partial maps fall back to the package-default thresholds
	// from `anomaly.DefaultThresholds()`. Each row must satisfy
	// `0 < warn_pct < freeze_pct`. The map MUST contain a `default`
	// entry (the fallback for unclassified assets); the loader
	// fills it from package defaults if the operator omits it.
	//
	// TOML representation:
	//   [anomaly.thresholds.stablecoin]  warn_pct=1.0   freeze_pct=3.0
	//   [anomaly.thresholds.treasury]    warn_pct=1.0   freeze_pct=3.0
	//   [anomaly.thresholds.crypto]      warn_pct=20.0  freeze_pct=50.0
	//   [anomaly.thresholds.governance]  warn_pct=50.0  freeze_pct=100.0
	//   [anomaly.thresholds.default]     warn_pct=30.0  freeze_pct=75.0
	Thresholds map[string]AnomalyThreshold `toml:"thresholds" doc:"Per-class threshold table. Keys are asset class names (stablecoin/treasury/crypto/governance/default). Empty falls back to package defaults; partial maps merge over defaults. The default row is required (loader fills it from package defaults if absent)." default:"{}"`

	// Classifications maps a canonical asset_id (as produced by
	// canonical.Asset.String()) to its asset class. Anything not in
	// the map falls through to ClassDefault. A class set on one alias
	// spelling (native, crypto:XLM, a SAC twin) covers all of them; two
	// spellings of one asset with different classes fail aggregator boot.
	//
	// TOML representation:
	//   [anomaly.classifications]
	//   "USDC-GA5Z…" = "stablecoin"
	//   "AQUA-GBN…"  = "governance"
	Classifications map[string]string `toml:"classifications" doc:"Operator-curated map of canonical asset_id → asset class (stablecoin/treasury/crypto/governance). Anything absent falls through to the default class." default:"{}"`

	// Phase2 holds the operator-tunable thresholds for the ADR-0019
	// Phase 2 freeze policy (3-signal AND on confidence + z + source
	// count). Defaults match the package-level hardcoded values; an
	// operator override merges atop those.
	Phase2 Phase2FreezeConfig `toml:"phase2" doc:"Phase 2 (per-asset baseline) freeze thresholds. All three conditions must hold for a freeze: confidence < 0.45 AND z > 5.0 AND sources <= 1. The confidence bound is 0.45 rather than ADR-0019's original 0.10 because confidence decays gently in z — at 0.10 the freeze needed z ~= 15 (a ~30% single-bucket move for XLM) and was effectively dormant. 0.45 puts the trigger just past the ADR's own z > 5. See DefaultPhase2ConfidenceMaxFreeze for the measured curve."`
}

// Phase2FreezeConfig surfaces the ADR-0019 Phase 2 freeze
// thresholds as TOML knobs. All three conditions must hold for a
// freeze; tightening any single threshold makes the gate stricter.
//
// Defaults match the package-level constants in
// `internal/aggregate/orchestrator/phase2_freeze.go`. Operators
// who haven't validated the per-asset baseline against their own
// market data are encouraged to leave these at the defaults until
// they have a sense of false-positive rate.
type Phase2FreezeConfig struct {
	ConfidenceMaxFreeze  float64 `toml:"confidence_max_freeze" doc:"Freeze fires when confidence is strictly less than this. 0.45 (operator decision 2026-07-25, coupled with the COR-14 fix); ADR-0019 originally said 0.10, which in practice required z ~= 15 and never fired." default:"0.45"`
	ZScoreMinFreeze      float64 `toml:"z_score_min_freeze" doc:"Freeze fires when z-score is strictly greater than this. ADR-0019 default 5.0 (the documented 5σ trigger)." default:"5.0"`
	SourceCountMaxFreeze int     `toml:"source_count_max_freeze" doc:"Freeze fires when source count is at or below this. ADR-0019 default 1 (single-source pattern)." default:"1"`

	// ─── Freeze DURATION (ADR-0019 §"Freeze duration") ────────────
	//
	// The three fields above decide WHETHER a bucket fires a freeze.
	// The ones below decide how long the resulting freeze holds, when
	// it re-evaluates, and what ends it. They are separate knobs
	// because they trade against different costs: firing too readily
	// serves a stale last-known-good price, while holding too
	// briefly serves a manipulated one.
	//
	// Durations are minutes, not Go duration strings, to match the
	// `*_seconds` / numeric convention every other cadence knob in
	// this file uses. 0 = "use the ADR default" for every field.
	InitialHoldMinutes int `toml:"initial_hold_minutes" doc:"Minimum freeze duration for a pair that had a corroborating lens (a checked triangulation composite or a trusted cross-oracle reading) in the bucket that froze it. ADR-0019 §'Freeze duration': 30 minutes. 0 = use the default." default:"30"`

	UncorroboratedInitialHoldMinutes int `toml:"uncorroborated_initial_hold_minutes" doc:"Minimum freeze duration for a pair with NO corroborating lens at all — one venue and its own history. Deliberately shorter than ADR-0019's flat 30 min: that population is where false freezes concentrate, and a false freeze bills the customer 100% of its duration in stale last-known-good price. Must stay comfortably above the longest window that can carry a spike so a one-bucket spike cannot be waited out. 0 = use the default." default:"10"`

	ExtensionMinutes int `toml:"extension_minutes" doc:"Added to the hold at each expiry the freeze has not earned its auto-unfreeze at. ADR-0019: 30 minutes. 0 = use the default." default:"30"`

	MaxExtensions int `toml:"max_extensions" doc:"Extensions granted before the freeze escalates to operator review (a P1 alert) and stops auto-unfreezing. ADR-0019: 4, i.e. 2 hours of extensions on top of the initial hold. 0 = use the default." default:"4"`

	UnfreezeConfidenceMin float64 `toml:"unfreeze_confidence_min" doc:"Auto-unfreeze requires confidence strictly ABOVE this. ADR-0019 §'Auto-unfreeze trigger': 0.30. Note the deliberate hysteresis against confidence_max_freeze (0.45) — a freeze must not be released by the same score band that would still fire it. 0 = use the default." default:"0.30"`

	UnfreezeZScoreMax float64 `toml:"unfreeze_z_score_max" doc:"Auto-unfreeze requires z strictly BELOW this. ADR-0019: 3.0, against the 5.0 fire threshold — the gap is the hysteresis band that stops a freeze flapping on a signal hovering at the trigger. 0 = use the default." default:"3.0"`

	UnfreezeBuckets int `toml:"unfreeze_buckets" doc:"How many CONSECUTIVE buckets must meet both auto-unfreeze conditions before the freeze ends. ADR-0019: 2. A bucket that cannot be scored at all resets the count. 0 = use the default." default:"2"`
}

// NOTE — the *Minutes fields above are ints rather than a
// `freeze.Policy`, and there is no conversion helper here, because
// this package must not import `internal/aggregate/freeze`:
// freeze → obs → config is a live import chain, so the reverse edge
// is a cycle. The binary boundary
// (cmd/stellarindex-aggregator/main.go) maps these onto
// `orchestrator.Phase2Thresholds.Lifecycle`, the same way it already
// maps the three threshold fields.

// AnomalyThreshold is one row of the anomaly threshold table.
// Mirrors `anomaly.Thresholds` but uses TOML-friendly types so the
// loader doesn't need a custom unmarshaller.
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

// CompositeReferenceConfig is the `[aggregate.composite_reference]`
// block. Mirrors orchestrator.CompositeReferenceConfig; the binary
// boundary (cmd/stellarindex-aggregator) maps it, the same way it maps
// the triangulation table.
type CompositeReferenceConfig struct {
	Enabled          bool     `toml:"enabled" doc:"Master switch. Default ON for the targets allow-list below; set false to restore the pre-2026-08-29 freeze-and-auto-release posture for every pair without editing the list." default:"true"`
	Targets          []string `toml:"targets" doc:"Allow-list of structurally single-venue target pairs (canonical wire form) the composite reference may corroborate. Each must also have an [[aggregate.triangulations]] row — its legs ARE the reference. A target with >= 2 real venues on a bucket is never evaluated regardless of this list." default:"[\"crypto:XLM/fiat:GBP\",\"crypto:XLM/fiat:EUR\"]"`
	ToleranceBps     int      `toml:"tolerance_bps" doc:"Maximum |direct - composite| / composite, in basis points, for the composite to CORROBORATE the direct print. 0 = default (75). Must be in (0, 10000]." default:"75"`
	MinLegSources    int      `toml:"min_leg_sources" doc:"Minimum distinct real exchange venues each priced (crypto/USD) leg must carry on the CURRENT bucket for the composite to count; a single-venue leg cannot corroborate (reason: composite_unavailable: leg_sources=N). 0 = default (2)." default:"2"`
	LegDispersionBps int      `toml:"leg_dispersion_bps" doc:"Leg-dispersion guard: every venue's own bucket VWAP on a priced (crypto/USD) leg must be within this many basis points of the leg VWAP for the leg to corroborate; otherwise composite_unavailable: leg_dispersion=… (fail-closed). Two venues only count as two when they agree — a dominant venue plus a dust print 3% off is one opinion. 0 = tolerance_bps." default:"0"`
	ReleaseBandPct   float64  `toml:"release_band_pct" doc:"Mid-hold auto-release agreement band (%) between the fresh candidate and the current-bucket composite for a target whose reference resolved on the bucket. Dedicated and tighter than the shared 5% cross-oracle band, which would release a held +4% venue-specific offset. 0 = default (2.0)." default:"2.0"`
	FXMaxAgeHours    int      `toml:"fx_max_age_hours" doc:"Staleness budget for the FX leg's fx_quotes snap (bucket age at evaluation). fx_quotes buckets are DAILY and the feed pauses over market closes, so this mirrors the Chainlink FX feed budget (76h), not the 6h poll-liveness alert. The FX leg must also come from the FX source class (massive) — never an oracle. 0 = default (76)." default:"76"`
}

// TriangulationChainConfig is one row of the triangulation table.
// Target is the implied pair (e.g. "crypto:XLM/fiat:EUR"); Legs is
// the ordered chain whose product yields the target price (e.g.
// ["crypto:XLM/fiat:USD", "fiat:USD/fiat:EUR"]). Loader validates
// that target = Legs[0].Base / Legs[-1].Quote and that adjacent
// legs share their pivot asset (Legs[i].Quote == Legs[i+1].Base).
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

	// RateLimitDwell wires ratelimit.WithDwellTime for the anon/key/
	// failed-auth buckets, so the dwell window can be tuned around the
	// stellarindex_ratelimit_fail_open alert (more than 100 fail-open
	// requests in a 15-minute window) without a rebuild. A negative value
	// disables the fail-open→fail-closed inversion.
	RateLimitDwell time.Duration `toml:"rate_limit_dwell" doc:"Fail-open dwell window for the anon/key/failed-auth rate-limit buckets before Take starts returning ratelimit.ErrThrottleUnavailable (fail-CLOSED, 503) on sustained Redis errors. Mirrors ratelimit.DefaultDwellTime. Negative disables the inversion (legacy fail-open-always)." default:"30s"`

	// MonthlyQuotaDwell wires middleware.WithMonthlyQuotaDwellTime.
	// Mirrors RateLimitDwell for the monthly-quota gate.
	MonthlyQuotaDwell time.Duration `toml:"monthly_quota_dwell" doc:"Fail-open dwell window for the monthly-quota middleware before month-to-date read errors flip it to fail-CLOSED (429 + Retry-After). Mirrors middleware.DefaultMonthlyQuotaDwellTime. Negative disables the inversion (legacy fail-open-always)." default:"30s"`

	// FailedAuthRateLimitPerMin caps invalid-credential attempts.
	// Auth runs before the main rate limiter, so a wrong API key / SEP-10
	// token is rejected (401) before it reaches the limiter. The same cap
	// applies independently per client IP and, in the API-key modes, per
	// presented key prefix — so rotating source IPs does not reset the
	// budget for guesses aimed at one key. Credential FAILURES only;
	// valid requests are unaffected. Only engaged when auth_mode != none.
	// 0 disables it.
	FailedAuthRateLimitPerMin int `toml:"failed_auth_rate_limit_per_min" doc:"Cap on INVALID-credential (failed-auth) attempts per minute, enforced inside the Auth middleware so credential-stuffing / API-key guessing is throttled even though auth rejects before the main rate limiter (C3-5). Only active when auth_mode != none. Applied independently per resolved client IP and, in the apikey / apikey_optional modes, per presented key prefix (the 12-char display prefix), so guessing aimed at one key from many IPs is still capped; a valid key is never throttled by it. Redis-backed when available, in-process fixed-window fallback otherwise. 0 disables the failed-auth throttle." default:"20"`

	HoldsFile           string        `toml:"holds_file" doc:"Path to a TOML file of [[hold]] entries (asset, contract_id, ledger_from, ledger_to, reason) that mark matching supply, balance and holder responses as under review. Polled every holds_reload_interval; a missing file means no holds; an invalid or zero-byte file keeps the previous list, so delete the file to lift every hold. Empty disables the feature." default:""`
	HoldsReloadInterval time.Duration `toml:"holds_reload_interval" doc:"How often the API re-reads holds_file." default:"15s"`

	// SingleInstance asserts exactly ONE API instance, unlocking the per-process
	// fallbacks for the auth throttles and passkey replay guard when Redis is
	// absent. Across several instances those are unsafe (a replayed passkey
	// finish-login on another instance mints a session; throttle caps multiply),
	// so the API refuses to start without Redis unless this is set. Ignored when
	// Redis is configured.
	SingleInstance bool `toml:"single_instance" doc:"Assert this deployment runs exactly ONE API instance. Only consulted when Redis is DISABLED (storage.redis_addr empty and no sentinels): the auth throttles + passkey ceremony replay guard then fall back to per-PROCESS state, which is unsafe behind more than one instance (cross-instance ceremony replay → session mint; throttle caps multiply by replica count). With Redis absent the API refuses to start unless this is true, so a multi-instance-without-Redis topology cannot silently downgrade a session-minting path. No effect when Redis is configured. Default false (the safe assumption: assume multiple instances until told otherwise)." default:"false"`

	// RequestTimeout + ServingStatementTimeout are the two layers of the
	// unauthenticated-DoS chokepoint. The
	// app-layer request deadline is the primary bound; the SQL
	// statement_timeout is the defense-in-depth backstop for when Go-side
	// ctx cancellation races. Keep statement_timeout LONGER than
	// request_timeout so the app-layer deadline fires first.
	RequestTimeout          time.Duration `toml:"request_timeout" doc:"Per-request context deadline applied to every non-streaming request by the API's RequestTimeout middleware, so every handler inherits a bound even when it forgets its own. Streaming (SSE) endpoints are exempt (they own their lifecycle via client-disconnect ctx cancellation). HARD MINIMUM: it must EXCEED 12s, the longest per-handler budget (config.APIMaxHandlerBudget), or the API refuses to boot — at or below it the blanket deadline reaches every reader first, so each handler's own '…-timeout' 503 is unreachable and the ceilings they advertise are fiction. Keep it under the 30s http.Server WriteTimeout. 0 disables the middleware entirely (no deadline injected, and the minimum does not apply)." default:"15s"`
	ServingStatementTimeout time.Duration `toml:"serving_statement_timeout" doc:"Session-level Postgres statement_timeout applied to every connection in the API's serving pool (via a post-connect SET), so a runaway request-path query is bounded SQL-side even if Go-side ctx cancellation races. Keep it LONGER than request_timeout so the app-layer deadline fires first (defense in depth). The indexer/aggregator pools are unaffected — their heavy batch scans set their own longer SET LOCAL statement_timeout inside a transaction, which overrides this session default. 0 disables it (plain Open, no session timeout)." default:"30s"`

	// SignupRequireEmailVerification opts the deployment into
	// the email-verification gate: API-key Subjects whose
	// EmailVerifiedAt is zero AND whose identifier indicates
	// /v1/signup origin get 403 with a Problem-JSON pointing
	// at the verify endpoint. Default true; operators set it false
	// to allow unverified signup keys.
	SignupRequireEmailVerification bool            `toml:"signup_require_email_verification" doc:"F-1218: when true, /v1/signup-minted API keys must complete email-ownership-proof (clicking the link emailed at signup) before they can authenticate. Default true (2026-05-13): we are still pre-launch with no consumer traffic, so the safe default is to require verification — operators who want to allow unverified signup must opt in explicitly. Pre-launch default-flip narrows the launch-blocker surface; F-1218 closure required this." default:"true"`
	CDNEnabled                     bool            `toml:"cdn_enabled" doc:"Emit CDN-friendly Cache-Control headers on long-immutable endpoints." default:"true"`
	AllowedOrigins                 []string        `toml:"allowed_origins" doc:"CORS allow-list for browser clients. Empty (default) is same-origin only — no cross-origin browser client can read responses. SEC-14 (audit-2026-07-23): a wildcard here is fully cross-origin readable by every website out of the box; operators opt into cross-origin explicitly by listing their own hostnames." default:"[]"`
	AllowCredentials               bool            `toml:"allow_credentials" doc:"Emit Access-Control-Allow-Credentials: true on CORS responses, but ONLY to origins also listed in credentialed_origins. Required for cookie-bearing cross-origin requests (magic-link session on /v1/account/me, /v1/account/keys). Browser-incompatible with allowed_origins=[\"*\"]; config validation rejects the combination." default:"false"`
	CredentialedOrigins            []string        `toml:"credentialed_origins" doc:"Subset of allowed_origins trusted to (a) receive Access-Control-Allow-Credentials: true and (b) bypass the same-site write guard (RequireSameSiteWrite) with a plain Origin match. RSEC-X1: allowed_origins is a public read allow-list — a status/docs subdomain that only ever reads /v1/ledger/tip has no business being trusted to drive a cookie-authenticated write. Ignored when allow_credentials is false. Empty (default) trusts none, even if allowed_origins is non-empty — the safe, fail-closed default. Must be a subset of allowed_origins; the API panics at boot otherwise." default:"[]"`
	TrustedProxyCIDRs              []string        `toml:"trusted_proxy_cidrs" doc:"Immediate peer CIDR allow-list that is permitted to supply X-Forwarded-For. Empty means the API ignores that header and uses the socket peer address for logging, anonymous identity, and IP-based rate limiting." default:"[]"`
	SEP10                          SEP10Config     `toml:"sep10" doc:"SEP-10 Web Auth — server signing seed, JWT secret, TTLs. Active when auth_mode=sep10 OR when /v1/auth/sep10/* endpoints are exposed."`
	Streaming                      StreamingConfig `toml:"streaming" doc:"Closed-bucket SSE fanout — pairs the API binary republishes to the streaming Hub on every new closed prices_1m bucket. Empty Pairs leaves /v1/price/stream returning 503; Hub still constructs so subscribers can connect (and immediately drop) without a panic."`
	PrometheusURL                  string          `toml:"prometheus_url" doc:"Prometheus HTTP API root (e.g. http://localhost:9090) backing /v1/status. Empty leaves /v1/status serving an in-process surface (uptime + region only)." default:""`
	// StatusServices names the BACKGROUND services this deployment runs, which
	// /v1/status reports and rolls `overall` up from ("api" is always reported).
	// Test nets run no aggregator; listing it would pin overall at "degraded"
	// forever. The default (indexer + aggregator) is pubnet's set.
	StatusServices    []string        `toml:"status_services" doc:"Background services whose heartbeats /v1/status reports and rolls up (subset of: indexer, aggregator). Drop one only on a deployment that genuinely does not run it — a service omitted here can never be reported down." default:"[\"indexer\",\"aggregator\"]"`
	ArchiveReportPath string          `toml:"archive_report_path" doc:"Filesystem path of the archive-completeness daemon's latest JSON report (the -output-file of 'stellarindex-ops archive-completeness verify'; the systemd unit writes /var/lib/galexie/last-completeness-report.json). Backs GET /v1/diagnostics/archive. The endpoint 404s while the file doesn't exist yet and 503s when this is empty." default:"/var/lib/galexie/last-completeness-report.json"`
	Dashboard         DashboardConfig `toml:"dashboard" doc:"Customer dashboard auth flow — passwordless email login (6-digit code + magic link) + cookie sessions backing the in-site dashboard at stellarindex.io/account. Empty leaves /v1/auth/{login,callback,verify-code,logout} returning 503."`
}

// DashboardConfig wires passwordless email login (6-digit code + magic link)
// and cookie sessions for stellarindex.io/account
// (docs/operations/cf-pages-setup.md). Without BaseURL or a Resend key the
// auth endpoints stay unwired and the explorer renders signed-out. The Resend
// key lives in an env var (default STELLARINDEX_RESEND_API_KEY), not TOML.
type DashboardConfig struct {
	BaseURL string `toml:"base_url" doc:"Absolute URL of the explorer hosting the in-site dashboard (e.g. https://stellarindex.io). The magic-link callback URL embedded in emails is {base_url}/auth/callback?token=<plaintext>, and the post-login redirect lands on {base_url}/account." default:""`

	EmailFrom string `toml:"email_from" doc:"From: address for transactional emails (e.g. 'Stellar Index <hello@stellarindex.io>'). Must match a domain Resend has verified for the configured API key." default:"Stellar Index <hello@stellarindex.io>"`

	ResendAPIKeyEnv string `toml:"resend_api_key_env" doc:"Environment variable holding the Resend transactional-email API key (re_…). An unset or empty value wires an unconfigured mail sender: POST /v1/auth/login answers 503 and counts a failed send, and signup reports email_verification_sent:false. Production sets this." default:"STELLARINDEX_RESEND_API_KEY"`

	SuppressedRecipientSHA256 []string `toml:"suppressed_recipient_sha256" doc:"Hex SHA-256 digests of lowercased, trimmed addresses that must never be mailed (hard bounces, complaints). Pseudonymised, not anonymous: anyone holding a candidate address can confirm membership. A suppressed send returns success to the caller, makes no Resend call and counts result=suppressed; suppressed logins therefore return faster, the same timing shape the throttle path already has. Empty suppresses nothing." default:"[]"`

	CodeSecretEnv string `toml:"code_secret_env" doc:"Environment variable holding the server secret that keys the 6-digit email-code derivation (HMAC over the stored token hash — without it a Postgres read would reveal every in-flight sign-in code) AND the WebAuthn passkey-ceremony, magic-link login-intent and login-device cookie MACs. Each consumer MACs under its own HKDF-derived key. Any long random string (32+ bytes). Required while passkeys are wired (the API refuses to start without it); otherwise an unset/empty env falls back to a random per-process secret: still keyed, but in-flight codes, magic links and browsers' login-device markers stop verifying across a restart or another instance." default:"STELLARINDEX_DASHBOARD_CODE_SECRET"`

	WebhookSealKeyEnv string `toml:"webhook_seal_key_env" doc:"Environment variable holding the secret that seals customer-webhook signing keys at rest (AES-256-GCM under an HKDF-derived key; column customer_webhooks.signing_key_sealed). At least 32 bytes; a shorter value refuses to start. Separate from code_secret_env so either can be rotated alone. Unset/empty stores new signing keys raw (logged at startup) and cannot read a sealed one, so deliveries to sealed webhooks wait until it is set. Changing the value makes every sealed key unreadable: their deliveries fail terminally, and the webhooks must be recreated (dashboard edit and delete still work without the key)." default:"STELLARINDEX_WEBHOOK_SEAL_KEY"`

	MagicLinkTTLMinutes int `toml:"magic_link_ttl_minutes" doc:"Magic-link validity in minutes. Default 15 — long enough for an email to arrive + the user to switch contexts; short enough to limit replay-window if a phone is briefly unattended." default:"15"`

	SessionTTLDays int `toml:"session_ttl_days" doc:"Session-cookie lifetime in days. Default 30 — matches typical SaaS dashboards; users sign in monthly without re-authing." default:"30"`

	CookieSecure bool `toml:"cookie_secure" doc:"Set the Secure flag on the JS-readable session-presence hint cookie. The credential cookies (session, login intent, passkey ceremony) are __Host- prefixed and always Secure; browsers treat http://localhost as a secure context. Production = true; dev (http://localhost) may set false." default:"true"`

	CookieDomain string `toml:"cookie_domain" doc:"Domain attribute of the JS-readable session-presence hint cookie only, so an explorer on a sibling host can see that a session exists; it carries no credential. Empty (default) means host-only. The credential cookies (session, login intent, passkey ceremony) are always host-only __Host- cookies and ignore this setting." default:""`
}

// StreamingConfig lists the (asset, quote) pairs the closed-bucket SSE
// producer broadcasts on /v1/price/stream, one goroutine per pair polling
// PriceReader at [PollInterval]. Static: adding a pair needs a restart.
type StreamingConfig struct {
	// Pairs is the operator-declared list of (asset, quote) pairs
	// to broadcast. Each entry is a two-element [base, quote] array
	// using canonical asset strings (e.g. ["native","fiat:USD"],
	// ["sac:CAS3J7…OWMA","fiat:USD"]). Empty disables the producer
	// while still letting the Hub construct.
	Pairs [][]string `toml:"pairs" doc:"Operator-declared closed-bucket fanout pair list. Each entry is a two-element [base, quote] array of canonical asset strings (e.g. [[\"native\", \"fiat:USD\"], [\"credit:USDC:GA5Z…\", \"fiat:USD\"]]). Empty disables the producer; clients that connect see SSE open + heartbeats but no price_update events." default:"[]"`

	// PollInterval is the per-pair poll cadence. Sub-second values
	// are clamped to 1 s by the publisher; zero falls back to 5 s.
	// 5 s detects a new 1-minute closed bucket within 5 s of its
	// end — well inside Freighter's 30 s freshness target.
	PollInterval time.Duration `toml:"poll_interval" doc:"Per-pair poll cadence for the closed-bucket producer. Default 5s; clamped to 1s minimum." default:"5s"`

	// MaxStreamsPerIP caps concurrently-held SSE connections from a
	// single client IP across every stream endpoint. Without it one
	// non-reading client can hold open (and
	// leak, until the write deadline fires) enough connections to
	// starve file descriptors / goroutines and deny the streams to
	// everyone else, since the global cap alone lets one IP consume the
	// whole budget. Over the cap, a new SSE connection is rejected with
	// 503. 0 disables the per-IP cap (the global cap still applies).
	MaxStreamsPerIP int `toml:"max_streams_per_ip" doc:"Maximum concurrently-held SSE stream connections per client IP across all stream endpoints (/v1/price/stream, /v1/price/tip/stream, /v1/observations/stream, /v1/ledger/stream). Guards against a single client exhausting file descriptors / goroutines by holding many stalled streams (C3-8 / CS-013). Over the cap a new stream is rejected with 503. 0 disables the per-IP cap; a separate global cap still bounds total concurrent streams." default:"20"`

	// MaxConcurrentStreams caps simultaneous SSE connections GLOBALLY
	// across every stream endpoint, independent of MaxStreamsPerIP's
	// per-client cap — the backstop against a flood of DISTINCT
	// client IPs (a botnet) rather than one client holding many
	// connections. It is the operator control for the underlying
	// streaming.SetMaxConcurrentStreams knob, whose package-level
	// default is 8192.
	MaxConcurrentStreams int64 `toml:"max_concurrent_streams" doc:"Global cap on simultaneous SSE connections across all stream endpoints, independent of the per-IP cap (guards against a flood of DISTINCT client IPs). Over the cap, a new connection is rejected with 503. <= 0 disables the global cap." default:"8192"`

	// MaxTipProducers caps distinct shared tip-stream producers (one
	// compute loop per (asset, quote, window)). Negative disables it.
	MaxTipProducers int `toml:"max_tip_producers" doc:"Global cap on distinct shared tip-stream producers (one compute loop per watched (asset, quote, window) triple), independent of connection counts — a detached producer outlives the connection that minted it for a linger window. Guards against an unauthenticated abort-loop flood enumerating the key space (UNAUTH-DOS-1). Negative disables the ceiling; 0 keeps the built-in default." default:"512"`

	// MaxTipProducersPerCaller caps the [MaxTipProducers] slots one
	// client-IP-derived caller may hold minted. Negative disables it.
	MaxTipProducersPerCaller int `toml:"max_tip_producers_per_caller" doc:"Per-caller cap on minted shared tip-stream producers, charged to the client-IP-derived principal for the life of the registry entry (through its linger). Prevents one address from filling the whole MaxTipProducers pool. Negative disables the per-caller quota; 0 keeps the built-in default." default:"24"`

	// BufferSize is the per-topic ring-buffer capacity for the
	// closed-bucket Hub. <= 0 keeps streaming.DefaultBufferSize (256).
	BufferSize int `toml:"buffer_size" doc:"Per-topic ring-buffer capacity for the streaming Hub — how many recent closed-bucket events a late-subscribing client can replay. <= 0 keeps the built-in default (256)." default:"256"`

	// TopicIdleTTL overrides how long a subscriber-less Hub topic
	// keeps its buffer before the reaper drops it. <= 0 keeps
	// streaming.DefaultTopicIdleTTL (15m).
	TopicIdleTTL time.Duration `toml:"topic_idle_ttl" doc:"How long a Hub topic with no subscribers keeps its replay buffer before the reaper drops it. <= 0 keeps the built-in default (15m)." default:"15m"`

	// MaxTopics overrides the Hub's topic-map ceiling. <= 0 keeps
	// streaming.DefaultMaxTopics (4096).
	MaxTopics int `toml:"max_topics" doc:"Ceiling on distinct live topics the Hub tracks (one per (asset, quote) pair actually subscribed to), independent of [Pairs] — guards unbounded topic growth from ad-hoc subscriptions. <= 0 keeps the built-in default (4096)." default:"4096"`
}

// SEP10Config configures the SEP-10 Web Auth validator. Both
// SeedEnv and JWTSecretEnv reference environment variable NAMES,
// not values — the actual secrets stay out of the config file
// and the docs-config output. A deployment with auth_mode=sep10
// AND an unset / empty env var fails loud at startup rather than
// silently 503-ing on every challenge.
type SEP10Config struct {
	SeedEnv       string        `toml:"seed_env" doc:"Environment variable holding the server signing keypair S-strkey. Operators rotate this on a schedule; ansible-vault stores the actual value." default:"STELLARINDEX_SEP10_SEED"`
	JWTSecretEnv  string        `toml:"jwt_secret_env" doc:"Environment variable holding the HMAC-SHA256 JWT secret (≥ 32 bytes of entropy required)." default:"STELLARINDEX_SEP10_JWT_SECRET"`
	WebAuthDomain string        `toml:"web_auth_domain" doc:"SEP-10 web_auth_domain — the host that serves /v1/auth/sep10/*. Carried inside the challenge tx so clients verify before signing. Typically the API's external host (e.g. api.stellarindex.io)." default:"api.stellarindex.io"`
	HomeDomain    string        `toml:"home_domain" doc:"Issuer home_domain. Carried in the JWT iss claim and in the challenge's first manage_data op. Typically same as the project root domain." default:"stellarindex.io"`
	ChallengeTTL  time.Duration `toml:"challenge_ttl" doc:"How long a SEP-10 challenge is valid for signing. SDK requires ≥ 1s; SEP-10 spec recommends 15m." default:"15m"`
	JWTTTL        time.Duration `toml:"jwt_ttl" doc:"Lifetime of an issued JWT. Clients refresh by repeating the challenge → verify flow." default:"1h"`
}

// SupplyConfig configures the supply-snapshot writer (`stellarindex-ops
// supply snapshot`, or in the aggregator when AggregatorRefreshEnabled). Per
// ADR-0011 nothing is fabricated: native XLM circulating excludes the SDF
// reserve accounts, whose balances come from `account_observations` (the LCM
// observer) or else the static `ReserveBalancesStroops` map. Empty
// SDFReserveAccounts yields circulating == total. Accounts without observer
// coverage or a static balance make the writer reject at start rather than
// overstate circulating supply.
type SupplyConfig struct {
	// SDFReserveAccounts is the G-strkey list whose XLM balances
	// are subtracted from the frozen total to yield circulating.
	// Per ADR-0011 these are operator-curated; the algorithm itself
	// is policy-agnostic. Curated is not static: verify-served-values
	// diffs this list daily against the one SDF publishes
	// (stellar/dashboard common/lumens.js) and tickets on any
	// missing/extra account — the 2% value cross-check alone cannot
	// see a single account change (internal/ops/chops).
	// It is also the accounts observer's watched set, but it is NOT a
	// general watch list: any account added here is subtracted from
	// circulating supply.
	SDFReserveAccounts []string `toml:"sdf_reserve_accounts" doc:"G-strkey list of SDF-controlled reserve accounts whose XLM balances are excluded from circulating supply per ADR-0011 Algorithm 1." default:"[]"`

	// ReserveBalancesStroops maps account G-strkey → balance in
	// stroops as a decimal string (NUMERIC-safe — no float
	// round-trip per ADR-0003). Operators update these manually
	// when SDF announces a reserve move. Every account in
	// SDFReserveAccounts MUST appear here; missing keys are a
	// configuration error caught at writer-start.
	ReserveBalancesStroops map[string]string `toml:"reserve_balances_stroops" doc:"Operator-managed snapshot of each SDF reserve account's XLM balance in stroops (decimal string). Updated manually on SDF reserve-move announcements. Used as the fallback source when the LCM AccountEntry observer (Task #54) hasn't yet populated account_observations for the watched reserve set; the live observer takes over once those rows land." default:"{}"`

	// ReserveBalancesAsOf dates the ReserveBalancesStroops snapshot
	// (YYYY-MM-DD, UTC). The static fallback refuses to answer when it
	// is unset or older than ReserveBalancesMaxAge, so a forgotten map
	// fails closed instead of being republished as the current reserve.
	ReserveBalancesAsOf string `toml:"reserve_balances_as_of" doc:"Date (YYYY-MM-DD, UTC) the reserve_balances_stroops snapshot was taken. The static fallback refuses to answer when this is unset or older than reserve_balances_max_age; a snapshot it does serve is published with supply basis xlm_sdf_reserve_exclusion_static." default:""`

	// ReserveBalancesMaxAge bounds how old the dated static snapshot may
	// be and still be served.
	ReserveBalancesMaxAge time.Duration `toml:"reserve_balances_max_age" doc:"Maximum age of the reserve_balances_stroops snapshot (measured from reserve_balances_as_of) at which the static fallback still answers. Past it, the XLM supply tick fails instead of republishing the old balances." default:"168h"`

	// AggregatorRefreshEnabled, when true, runs the supply-
	// snapshot writer as a goroutine inside the aggregator on a
	// fixed cadence (see [AggregatorRefreshCadence]). When false
	// (the default), operators are expected to drive the writer
	// via the systemd timer in deploy/systemd/supply-snapshot.timer
	// instead. Once the LCM observer covers
	// the live operator set, the goroutine path is preferred —
	// snapshots refresh per-cadence rather than per-day, and the
	// systemd timer becomes redundant.
	AggregatorRefreshEnabled bool `toml:"aggregator_refresh_enabled" doc:"Run the supply-snapshot writer as a goroutine in the aggregator instead of via the systemd timer. Requires the LCM AccountEntry observer to be backfilled across the watched accounts (or the static reserve_balances_stroops fallback to be valid)." default:"false"`

	// AggregatorRefreshCadence is the per-cycle interval for the
	// goroutine path (only relevant when AggregatorRefreshEnabled
	// is true). Defaults to 5 minutes — a balance between freshness
	// (operators want observed_at to track current ledger) and
	// table-write rate (asset_supply_history's ON CONFLICT DO
	// NOTHING dedupes per-(asset, ledger), but unique-ledger rows
	// still accumulate at one-per-tick when the chain advances).
	AggregatorRefreshCadence time.Duration `toml:"aggregator_refresh_cadence" doc:"Per-cycle interval for the in-aggregator supply-snapshot worker (only used when aggregator_refresh_enabled is true)." default:"5m"`

	// WatchedClassicAssets is the operator-curated list of classic
	// credit assets the supply pipeline computes Algorithm 2 for.
	// Per ADR-0022 — drives the four classic-supply observers
	// (trustlines / claimable_balances / liquidity_pools /
	// sac_balances) and the aggregator's classic-supply refresh
	// loop. Each entry is a canonical asset string in CODE-ISSUER
	// form (e.g. "USDC-GA5...").
	//
	// Empty (the default) leaves classic-supply observers + refresh
	// off — the existing XLM-only path stays the operator's only
	// surface.
	WatchedClassicAssets []string `toml:"watched_classic_assets" doc:"Operator-curated classic credit assets (CODE-ISSUER form) to track for Algorithm 2 supply per ADR-0022. Empty leaves the classic-supply pipeline off." default:"[]"`

	// SACWrappers maps the C-strkey of a Stellar-Asset-Contract
	// wrapper to the supply.AssetKey form (CODE:ISSUER) of the
	// classic asset it wraps. The SAC observer uses this map to
	// stamp asset_key on every observation row + filter to the
	// watched set.
	//
	// Each watched classic asset that has a SAC wrapper deployed
	// should have an entry here. Operators that haven't deployed
	// SACs (or aren't tracking them) leave this empty; the
	// SAC-component sum is then zero, which is correct for those
	// assets.
	SACWrappers map[string]string `toml:"sac_wrappers" doc:"SAC wrapper contract C-strkey → supply.AssetKey (CODE:ISSUER) map. Drives the SAC balance observer's watched-contract filter. Pure SEP-41 contracts reuse this map by mapping contract_id → contract_id." default:"{}"`

	// WatchedSEP41Contracts is the operator-curated list of SEP-41
	// Soroban contract ids the supply pipeline computes Algorithm 3
	// for. Per ADR-0023 — drives the SEP-41 supply observer
	// (`internal/sources/sep41_supply/`) and the aggregator's
	// SEP-41 supply refresh loop. Each entry is a C-strkey contract
	// id (e.g. "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7").
	//
	// Empty (the default) leaves the SEP-41 supply pipeline off.
	WatchedSEP41Contracts []string `toml:"watched_sep41_contracts" doc:"Operator-curated SEP-41 Soroban contract C-strkeys to track for Algorithm 3 supply per ADR-0023. Empty leaves the SEP-41 supply pipeline off." default:"[]"`

	// FullyWrappedSACs attests that a SAC wrapper (keyed as in SACWrappers)
	// holds the classic asset's ENTIRE supply. Listed ids get
	// supply.WrapClassFull (strict ADR-0011 total-vs-total compare); others get
	// WrapClassPartial (see internal/supply.CrossCheckSubsetBound). Empty is the
	// honest default; adding an entry needs the same evidence trail as a
	// BackfillSafe flip (docs/operations/wasm-audits/).
	FullyWrappedSACs []string `toml:"fully_wrapped_sacs" doc:"SAC wrapper contract C-strkeys (subset of sac_wrappers' keys) the operator attests are 100% SAC-represented — no classic-trustline supply outside the SAC. Selects the strict ADR-0011 equality cross-check (supply.WrapClassFull) instead of the default subset-bound compare (supply.WrapClassPartial). Empty by default; BACKLOG #59, 2026-07-08." default:"[]"`

	// StrictFreshnessRequired flips the supply Refresher into the
	// stricter posture:
	// snapshots arriving with `MinComponentLedger == 0` (no
	// freshness anchor — happens on the static-XLM fallback path
	// or when a freshness producer transiently fails) get
	// rejected with `OutcomeKindMissingFreshness` instead of
	// being published. Default false keeps the
	// permissive interpretation. Operators flip true once every
	// freshness producer is wired AND every reader is shown to
	// never fail-open under steady-state load — typically post-
	// launch, after a few weeks of green snapshot timers.
	StrictFreshnessRequired bool `toml:"strict_freshness_required" doc:"F-1236: when true, supply snapshots without a MinComponentLedger anchor (i.e. zero-value freshness, the static-XLM fallback or a transiently-failing producer) are rejected rather than published. Default false preserves backwards-compatible permissive behaviour; flip true after the freshness producers are confirmed wired in steady state." default:"false"`

	StaleComponentLedgers uint32 `toml:"stale_component_ledgers" doc:"Global stale-component threshold in ledgers: a supply snapshot whose oldest component observation lags the snapshot ledger by more than this is rejected (outcome stale_component). 0 disables the gate. Per-asset overrides live in stale_component_ledgers_by_asset. Independent of the classic/SAC cross-check alignment window, which stays fixed at 1000 ledgers." default:"1000"`

	MaxDormantComponentLedgers uint32 `toml:"max_dormant_component_ledgers" doc:"Dormancy horizon in ledgers: how far the snapshot ledger may run ahead of an UNCHANGED component ledger before a dormant-asset accept becomes a stale_component rejection. 0 disables the horizon, so a dead observer looks dormant forever; set it only for assets legitimately dormant for long stretches whose observers are monitored another way." default:"17280"`

	// StaleComponentLedgersByAsset relaxes or tightens the global
	// stale_component_ledgers gate for one asset, so a known low-activity
	// asset (PHO lags ~1200 ledgers between trustline observations) does
	// not force the gate open fleet-wide. Keys are canonicalised through
	// supply.CanonicalizeStaleComponentLedgers; Validate rejects a key
	// that names no watched asset. A zero value disables the gate for that
	// asset alone.
	StaleComponentLedgersByAsset map[string]uint32 `toml:"stale_component_ledgers_by_asset" doc:"Per-asset override of the F-1236 stale-component-ledger threshold. Map keys are asset_key in the internal supply.AssetKey() shape ('XLM' for native, CODE:ISSUER for classic, bare contract id for SEP-41); values are ledger counts. Empty map (default) keeps every asset on the global 1000-ledger threshold. F-0040 (audit-2026-05-26)." default:"{}"`

	// PerAssetLockedSets overrides the default locked set (issuer-only for
	// classic Algorithm 2, admin-only for SEP-41 Algorithm 3) for specific
	// assets, e.g. treasury/vesting accounts. Keys name a watched_classic_assets
	// entry (CODE-G… or CODE:G…) or a watched_sep41_contracts id, re-keyed to
	// supply.AssetKey at load; XLM, unwatched or unparseable keys fail Validate.
	// A missing key uses the default; a present-but-empty entry opts out of it.
	// buildSupplyPolicy builds internal/supply.Policy from this and
	// MaxSupplyOverrides.
	PerAssetLockedSets map[string]SupplyLockedSetConfig `toml:"per_asset_locked_sets" doc:"Per-asset override of the default locked-set (issuer-only for classic, admin-only for SEP-41) excluded from circulating_supply. Map key: a watched_classic_assets entry ('CODE-G...' or 'CODE:G...') or a watched_sep41_contracts C-strkey; 'XLM' and unwatched keys are rejected at startup. Members must be observed or startup fails: a classic key's contracts need a sac_wrappers entry for its SAC; a SEP-41 key needs a sac_wrappers entry mapping it to itself, or to its classic asset (a SAC), whose accounts then also need that asset in watched_classic_assets. Empty map preserves the per-algorithm default for every asset." default:"{}"`

	// MaxSupplyOverrides forces max_supply for a specific asset,
	// beating both the SEP-1 declaration and the per-algorithm
	// default. Keyed like PerAssetLockedSets (no XLM). Value is a
	// decimal string in the asset's base unit (stroops for classic,
	// contract-defined units for SEP-41).
	// Empty string means "fall through to the next source"
	// (equivalent to omitting the key) — see
	// internal/supply.Policy.MaxSupplyOverride.
	MaxSupplyOverrides map[string]string `toml:"max_supply_overrides" doc:"Per-asset max_supply override, beating the SEP-1 declaration and the per-algorithm default. Map key mirrors per_asset_locked_sets; value is a decimal string in base units. Empty string value falls through to the next source." default:"{}"`
}

// SupplyLockedSetConfig is the TOML-friendly mirror of
// internal/supply.LockedSet — see that type's doc for the
// classic/SEP-41 Algorithm 2/3 semantics. Kept as a separate config
// type (rather than embedding supply.LockedSet directly) so this
// package's TOML tags stay independent of the domain type's shape,
// matching the AnomalyThreshold / anomaly.Thresholds mirror pattern
// elsewhere in this file.
type SupplyLockedSetConfig struct {
	Accounts  []string `toml:"accounts" doc:"G-strkey accounts whose balance is excluded from circulating supply for this asset (treasury / reserve multisigs)." default:"[]"`
	Contracts []string `toml:"contracts" doc:"C-strkey contracts whose balance is excluded from circulating supply for this asset (vesting / treasury contracts)." default:"[]"`
}

// Validate reports inconsistencies in the supply block:
//
//  1. Every SDF reserve account is a CRC-valid G-strkey. A matching
//     reserve_balances_stroops entry is NOT required: observer coverage is
//     only visible in Postgres, and ConfigReserveBalanceReader rejects where
//     it is actually consulted.
//  2. The aggregator-refresh cadence is at least 30s.
//  3. No WatchedClassicAssets entry is empty (parsed at aggregator start).
//  4. Every SACWrappers asset_key is non-empty.
//  5. Every FullyWrappedSACs entry is a SACWrappers key, else it silently
//     no-ops.
//  6. Every StaleComponentLedgersByAsset key resolves to exactly one watched
//     asset, else the global threshold silently stays in force.
//  7. Every PerAssetLockedSets / MaxSupplyOverrides key resolves to exactly
//     one watched classic or SEP-41 asset; XLM is rejected.
//  8. Every PerAssetLockedSets member is a holder kind an observer records
//     for that asset (see validateLockedSetCoverage).
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

// DefaultReserveBalancesMaxAge is the reserve_balances_max_age default:
// one week, a conservative bound for balances SDF moves in
// multi-billion-XLM tranches.
const DefaultReserveBalancesMaxAge = 7 * 24 * time.Hour

// EffectiveReserveBalancesMaxAge is ReserveBalancesMaxAge, or the
// default when unset.
func (sc SupplyConfig) EffectiveReserveBalancesMaxAge() time.Duration {
	if sc.ReserveBalancesMaxAge <= 0 {
		return DefaultReserveBalancesMaxAge
	}
	return sc.ReserveBalancesMaxAge
}

// NewStaticReserveReader builds the dated static reserve-balance reader
// from reserve_balances_stroops / _as_of / _max_age — the one
// construction path for every XLM supply writer.
func (sc SupplyConfig) NewStaticReserveReader() (*supply.ConfigReserveBalanceReader, error) {
	asOf, err := sc.ReserveBalancesAsOfTime()
	if err != nil {
		return nil, err
	}
	return supply.NewConfigReserveBalanceReader(sc.ReserveBalancesStroops, asOf, sc.EffectiveReserveBalancesMaxAge())
}

// ReserveBalancesAsOfTime parses ReserveBalancesAsOf; unset yields the
// zero time, which the static reserve reader treats as undated.
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

// validateStaleComponentLedgersByAsset is check 6 of
// [SupplyConfig.Validate]. XLM is always watched; the aggregator
// builds its refresher unconditionally.
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

// validatePolicyOverrideKeys is check 7 of [SupplyConfig.Validate].
// Only the classic and SEP-41 computers read these two maps, so XLM
// is not a watched key here (CanonicalizePolicyKeys rejects it).
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

// validateLockedSetCoverage is check 8 of [SupplyConfig.Validate]. The
// locked-set readers read a holder with no observation row as a zero
// balance, so a member no observer records would exclude nothing while the
// snapshot still claims the override basis. Coverage is checked per (asset,
// holder kind), never per row: a never-funded holder legitimately has none.
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

// validateFullyWrappedSACs is check 5 of [SupplyConfig.Validate],
// split out to keep Validate's cognitive complexity under the
// gocognit ceiling.
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

// ObsConfig wires metrics and logs. Metrics
// exposure varies per-binary: the indexer, the aggregator, and the
// long-lived ops commands (e.g. `cross-region-monitor`,
// `verify-archive --metrics`) each bind a dedicated `/metrics`
// listener at [ObsConfig.MetricsListen]; the API binary serves
// `/metrics` on its public listener (so a CDN-fronted deployment
// doesn't need a sidecar port).
type ObsConfig struct {
	MetricsListen string `toml:"metrics_listen" doc:"Bind address for the dedicated /metrics Prometheus endpoint. Read by the indexer, the aggregator, and the long-lived ops binaries (cross-region-monitor, verify-archive --metrics). The API binary serves /metrics on its public listener and ignores this field." default:"127.0.0.1:9464"`
	// MetricsListenSet records whether obs.metrics_listen was present
	// in the loaded TOML file, as opposed to left at Default()'s
	// value. Populated by LoadReader from toml.MetaData — never set
	// by the decoder itself (toml:"-"). The aggregator's single-host
	// port-collision shift (cmd/stellarindex-aggregator/main.go) reads
	// this instead of comparing MetricsListen by value, so an operator
	// who explicitly pins 127.0.0.1:9464 for the aggregator is honoured
	// instead of silently overridden.
	MetricsListenSet bool   `toml:"-"`
	LogLevel         string `toml:"log_level" doc:"Minimum log level — debug / info / warn / error." default:"info"`
	LogFormat        string `toml:"log_format" doc:"Log format — json / text / console ('text' and 'console' are synonyms)." default:"json"`
}

// Default returns a Config pre-populated with every field's default
// value. Used by the docs-config generator to show what operators
// get out of the box, and as the starting point for config loading.
// defaultHashDBConfig is split out of Default() to keep that function
// under the funlen ceiling (same reason as defaultAPIConfig /
// defaultDivergenceConfig below). Off by default — see HashDBConfig's
// doc. Non-zero cadence + window so an accidental Enabled=true
// without overrides doesn't reach time.NewTicker(0). Kept in
// lockstep with the `default:` struct tags — see
// TestDefault_MatchesStructTags.
func defaultHashDBConfig() HashDBConfig {
	return HashDBConfig{
		Enabled:               false,
		Path:                  "/var/lib/stellarindex/hashdb.bin",
		VerifyIntervalMinutes: 60,
		VerifyWindowLedgers:   20000,
	}
}

// defaultAnomalyConfig is split out of Default() to keep that function
// under the funlen ceiling (same reason as defaultAPIConfig below).
//
// It holds the ADR-0019 anomaly-response defaults: the Phase 2
// 3-signal freeze thresholds AND the freeze-duration lifecycle
// (initial hold, extension ladder, escalation cap, auto-unfreeze
// condition). The duration values mirror the `freeze.Default*`
// constants in internal/aggregate/freeze — the two are kept in step
// by TestDefault_MatchesStructTags here (Default() vs the `default:`
// tags) and by TestPolicy_WithDefaults_MatchesADR0019 there
// (constants vs the ADR).
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

// defaultAPIConfig is split out of Default() to keep that function under
// the funlen ceiling; it holds the public-API / auth / dashboard defaults
// (kept in lockstep with the `default:` struct tags — see
// TestDefault_MatchesStructTags).
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
		HoldsReloadInterval:       15 * time.Second,
		// Unauthenticated-DoS chokepoint: the app-layer request
		// deadline (15s) is the primary bound; the serving-pool
		// statement_timeout (30s) is the SQL-side backstop, kept longer so
		// the app deadline fires first.
		RequestTimeout:          15 * time.Second,
		ServingStatementTimeout: 30 * time.Second,
		CDNEnabled:              true,
		AllowedOrigins:          []string{},
		CredentialedOrigins:     []string{},
		TrustedProxyCIDRs:       []string{},
		// Probe the public TLS leaf certs by default so silent
		// Let's Encrypt renewal failures surface before expiry (the alert
		// series only exists when this is populated).
		TLSCertProbeHosts: []string{"api.stellarindex.io", "status.stellarindex.io", "stellarindex.io"},
		// The safe default is to require email-ownership
		// proof on signup-minted keys; operators opt out explicitly.
		SignupRequireEmailVerification: true,
		// Matches the archive-completeness.service REPORT_OUTPUT path;
		// the handler degrades to 404 while the file doesn't exist, so
		// the default is harmless on hosts without the daemon.
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
			BufferSize:               256,
			TopicIdleTTL:             15 * time.Minute,
			MaxTopics:                4096,
		},
	}
}

// RetiredKeys lists dotted config paths that once existed in this schema
// and were deliberately removed. LoadReader treats a key on this list as a
// deprecation warning instead of a boot-fatal unknown key: a self-hosted
// deployment's config is a copy of some past release's configs/example.toml
// (docs/operations/self-hosting.md §4.3), so deleting a struct field would
// otherwise turn every such upgrade into a hard outage. Add an entry
// here in the same commit that removes the field; the value is a short note
// on what replaced it, surfaced in the boot-time warning.
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
}

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
			PostgresDSN:                "postgres://stellarindex@127.0.0.1:5432/stellarindex?sslmode=disable",
			BackgroundStatementTimeout: 30 * time.Minute,
			RedisAddr:                  "127.0.0.1:6379",
			// RedisSentinelAddrs / RedisMasterName left empty — Default()
			// targets dev / single-node. Production inventories override
			// to enable Sentinel mode (ADR-0024).
			RedisSentinelAddrs: []string{},
			RedisMasterName:    "",
			S3Endpoint:         "http://127.0.0.1:9000",
			S3Region:           "r1",
			S3BucketArchive:    "galexie-archive",
			S3BucketLive:       "galexie-live",
			S3AccessKeyEnv:     "STELLARINDEX_S3_ACCESS_KEY",
			S3SecretKeyEnv:     "STELLARINDEX_S3_SECRET_KEY",
			ClickHouseAddr:     "127.0.0.1:9300",
			// ADR-0041: the certified-lake substrate is on by
			// default — it backs the coverage claim, the CH
			// completeness path, and lake-derived supply.
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
			// Reflector mainnet addresses are operator-supplied
			// (see docs/protocols/reflector.md). Empty by
			// default — enabling a reflector-* source without
			// setting its address is a startup error.
			Reflector: ReflectorOracleConfig{},
			Redstone:  RedstoneOracleConfig{},
			Band:      BandOracleConfig{},
			Soroswap:  SoroswapConfig{},
			// No shipped overrides: the default budget (10 × declared
			// resolution) is what every asset gets until an operator
			// writes down a per-asset exception.
			StalenessOverrides: []OracleStalenessOverrideConfig{},
		},
		External:     defaultExternalConfig(),
		Aggregate:    defaultAggregateConfig(),
		Anomaly:      defaultAnomalyConfig(),
		API:          defaultAPIConfig(),
		Divergence:   defaultDivergenceConfig(),
		PricingGuard: defaultPricingGuardConfig(),
		DecimalsGuard: DecimalsGuardConfig{
			// 90 days — matches decimalsguard.DefaultBackfillWindow.
			BackfillWindowDays: 90,
		},
		PriceAlerts: PriceAlertsConfig{
			// Off by default — operator opts in once alerts + webhooks
			// are wired. Non-zero cadence so an accidental Enabled=true
			// without an interval doesn't reach time.NewTicker(0).
			Enabled:         false,
			IntervalSeconds: 30,
		},
		SignupReaper: SignupReaperConfig{
			// On by default — signup-race orphans are pure
			// garbage (no user, no key). Non-zero cadence + age so the
			// worker never reaches time.NewTicker(0) and always leaves a
			// safety window before deleting.
			Enabled:         true,
			IntervalMinutes: 60,
			MinAgeMinutes:   1440,
		},
		Supply: SupplyConfig{
			// Cadence is only consumed when AggregatorRefreshEnabled is
			// flipped on; a non-zero default avoids time.NewTicker(0)
			// panicking if an operator enables the worker without setting
			// it.
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

// defaultAggregateConfig returns the price-aggregator defaults, including
// the composite-reference cross-check. Split out of Default() to keep it
// under funlen.
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
			// ON by default: the mechanism is
			// fail-closed on every leg (thin, stale, wrong class)
			// and evaluated only on single-venue buckets of the
			// listed targets — the pairs the design doc (§3) records
			// as structurally single-venue with a deep USD leg.
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

// defaultExternalConfig returns the off-chain-connector defaults. All venues
// are disabled by default; an operator opts in per-venue once they've confirmed
// network egress / credentials. Split out of Default() to keep it under funlen.
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
		Massive:          MassiveConfig{RefreshInterval: time.Hour},
		Tiingo:           TiingoVenueConfig{Enabled: false},
	}
}
