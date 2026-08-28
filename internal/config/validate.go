package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ErrInvalidConfig is the sentinel error every validation failure
// wraps. Use errors.Is(err, ErrInvalidConfig) in callers that need
// to distinguish config bugs from I/O / decode failures.
var ErrInvalidConfig = errors.New("config: invalid")

// KnownSources is the set of source names the indexer's
// buildSources() switch recognises today. Listed here so
// config.Validate can reject typos at boot rather than at
// runtime — an operator running `-dry-run` with `sorowsap`
// in enabled_sources should get a clear error before storage
// connect + RPC probe burn 5+ seconds.
//
// DO NOT import source packages from config (cycle avoidance, see
// contractIDPattern). When you add a source in cmd/stellarindex-indexer/
// main.go buildSources(), mirror the name here.
var KnownSources = map[string]struct{}{
	"soroswap":        {},
	"soroswap-router": {},
	"defindex":        {},
	"aquarius":        {},
	"phoenix":         {},
	"comet":           {},
	"sdex":            {},
	"blend":           {},
	"blend_backstop":  {},
	"blend_emitter":   {},
	"reflector-dex":   {},
	"reflector-cex":   {},
	"reflector-fx":    {},
	"redstone":        {},
	"band":            {},
	"cctp":            {},
	"rozo":            {},
	"sorocredit":      {},
}

// Validate checks the loaded Config against the same constraints
// the operator's runbook assumes. Called by [Load] so a malformed
// config fails at startup, not mid-request.
//
// Returns the first error encountered — callers that want a full
// report should fix the first error and re-run.
func (c Config) Validate() error {
	if err := c.Region.validate(); err != nil {
		return err
	}
	if err := c.Stellar.validate(); err != nil {
		return err
	}
	if err := c.Storage.validate(); err != nil {
		return err
	}
	if err := c.Ingestion.validate(); err != nil {
		return err
	}
	if err := c.Oracle.validate(); err != nil {
		return err
	}
	if err := c.Aggregate.validate(); err != nil {
		return err
	}
	if err := c.Anomaly.validate(); err != nil {
		return err
	}
	// CFG-05 (audit-2026-07-23): the [divergence] section was never
	// wired into Validate() at all, so refresh_interval_seconds<=0
	// reached time.NewTicker(0) at aggregator startup and panicked —
	// the same failure mode G19-02 already fixed for c.Supply above.
	if err := c.Divergence.validate(); err != nil {
		return err
	}
	if err := c.API.validate(); err != nil {
		return err
	}
	if err := c.Trades.validate(); err != nil {
		return err
	}
	if err := c.PricingGuard.validate(); err != nil {
		return err
	}
	if err := c.DecimalsGuard.validate(); err != nil {
		return err
	}
	if err := c.Obs.validate(); err != nil {
		return err
	}
	// G19-02: Supply.Validate() was never wired here, so enabling the
	// in-aggregator supply worker without a cadence reached
	// time.NewTicker(0) at runtime. It's a no-op for the default
	// (disabled) config and only enforces the <30s-cadence + reserve-set
	// invariants when the operator opts in.
	if err := c.Supply.Validate(); err != nil {
		return err
	}
	if err := c.PriceAlerts.validate(); err != nil {
		return err
	}
	if err := c.SignupReaper.validate(); err != nil {
		return err
	}
	if err := c.HashDB.validate(); err != nil {
		return err
	}
	// Cross-section checks: enabled sources must have the config
	// they need. These can't live on the individual sub-structs
	// because they span two sections (ingestion + oracle).
	if err := c.validateCrossSection(); err != nil {
		return err
	}
	return nil
}

// validateCrossSection catches config errors that span multiple
// sections — e.g. "you enabled reflector-dex but didn't set the
// contract address." Runs after per-section validates so we can
// assume each section's internal shape is already sound.
func (c Config) validateCrossSection() error {
	for _, name := range c.Ingestion.EnabledSources {
		key := strings.ToLower(strings.TrimSpace(name))
		switch key {
		case "reflector-dex":
			if c.Oracle.Reflector.DEXContract == "" {
				return fmt.Errorf(
					"%w: ingestion.enabled_sources lists %q but oracle.reflector.dex_contract is empty",
					ErrInvalidConfig, name)
			}
		case "reflector-cex":
			if c.Oracle.Reflector.CEXContract == "" {
				return fmt.Errorf(
					"%w: ingestion.enabled_sources lists %q but oracle.reflector.cex_contract is empty",
					ErrInvalidConfig, name)
			}
		case "reflector-fx":
			if c.Oracle.Reflector.FXContract == "" {
				return fmt.Errorf(
					"%w: ingestion.enabled_sources lists %q but oracle.reflector.fx_contract is empty",
					ErrInvalidConfig, name)
			}
		case "redstone":
			if c.Oracle.Redstone.AdapterContract == "" {
				return fmt.Errorf(
					"%w: ingestion.enabled_sources lists %q but oracle.redstone.adapter_contract is empty",
					ErrInvalidConfig, name)
			}
		case "band":
			if c.Oracle.Band.StandardReferenceContract == "" {
				return fmt.Errorf(
					"%w: ingestion.enabled_sources lists %q but oracle.band.standard_reference_contract is empty",
					ErrInvalidConfig, name)
			}
		}
	}
	return nil
}

func (r RegionConfig) validate() error {
	if r.ID == "" {
		return fmt.Errorf("%w: region.id required", ErrInvalidConfig)
	}
	if !regionIDPattern.MatchString(r.ID) {
		return fmt.Errorf("%w: region.id %q must be lowercase alphanumeric (e.g. r1, r2, r3)",
			ErrInvalidConfig, r.ID)
	}
	if r.HomeDomain == "" {
		return fmt.Errorf("%w: region.home_domain required", ErrInvalidConfig)
	}
	if strings.Contains(r.HomeDomain, "/") || strings.Contains(r.HomeDomain, "://") {
		return fmt.Errorf("%w: region.home_domain %q must be a bare DNS name, not a URL",
			ErrInvalidConfig, r.HomeDomain)
	}
	return nil
}

func (s StellarConfig) validate() error {
	switch s.Network {
	case "pubnet", "testnet", "futurenet":
		// ok
	default:
		return fmt.Errorf("%w: stellar.network %q must be pubnet/testnet/futurenet",
			ErrInvalidConfig, s.Network)
	}
	if len(s.RPCEndpoints) == 0 {
		return fmt.Errorf("%w: stellar.rpc_endpoints must have at least one URL",
			ErrInvalidConfig)
	}
	// Reject duplicate endpoints. Failover is the whole point of
	// providing a list — duplicates don't buy redundancy, they just
	// look redundant. Case-fold so "http://X" and "HTTP://X" compare
	// equal (URL schemes are case-insensitive per RFC 3986).
	seen := make(map[string]struct{}, len(s.RPCEndpoints))
	for i, ep := range s.RPCEndpoints {
		if _, err := url.Parse(ep); err != nil || !strings.Contains(ep, "://") {
			return fmt.Errorf("%w: stellar.rpc_endpoints[%d] %q must be a full URL",
				ErrInvalidConfig, i, ep)
		}
		key := strings.ToLower(strings.TrimRight(ep, "/"))
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%w: stellar.rpc_endpoints has duplicate %q",
				ErrInvalidConfig, ep)
		}
		seen[key] = struct{}{}
	}
	// CoreHTTPEndpoint is optional — empty means "don't probe core".
	// When set it must parse as an absolute URL.
	if s.CoreHTTPEndpoint != "" {
		if _, err := url.Parse(s.CoreHTTPEndpoint); err != nil || !strings.Contains(s.CoreHTTPEndpoint, "://") {
			return fmt.Errorf("%w: stellar.core_http_endpoint %q must be a full URL",
				ErrInvalidConfig, s.CoreHTTPEndpoint)
		}
	}
	// CFG-05 (audit-2026-07-23): history_archive_url backs the
	// backfill-catchup archive read path and has a mandatory
	// non-empty default (unlike the optional core_http_endpoint) —
	// treat it as required, and apply the same scheme-check its
	// sibling URL fields (rpc_endpoints, core_http_endpoint) already
	// get. Bare url.Parse accepts scheme-less and even empty strings,
	// which let a malformed archive URL reach the archive client
	// unnoticed until the first backfill request failed.
	if s.HistoryArchiveURL == "" {
		return fmt.Errorf("%w: stellar.history_archive_url required", ErrInvalidConfig)
	}
	if _, err := url.Parse(s.HistoryArchiveURL); err != nil || !strings.Contains(s.HistoryArchiveURL, "://") {
		return fmt.Errorf("%w: stellar.history_archive_url %q must be a full URL",
			ErrInvalidConfig, s.HistoryArchiveURL)
	}
	// Network/archive mismatch guard (audit 2026-08-26): the PUBNET archive
	// (core-live) must never be paired with a test-net `network`, or a
	// testnet/futurenet node would ingest PUBNET checkpoints into a test-net
	// store — silent cross-network corruption, and config validation is the
	// only gate before the archive client uses this URL. A test net's own
	// archive carries a core-testnet / core-futurenet path segment instead.
	if s.Network != "pubnet" {
		lower := strings.ToLower(s.HistoryArchiveURL)
		if strings.Contains(lower, "core-live") || strings.Contains(lower, "core_live") {
			return fmt.Errorf("%w: stellar.network=%q but stellar.history_archive_url %q is a PUBNET archive (core-live) — point it at the %s history archive so the node does not ingest pubnet ledgers into a test-net store",
				ErrInvalidConfig, s.Network, s.HistoryArchiveURL, s.Network)
		}
	}
	return nil
}

func (s StorageConfig) validate() error { //nolint:gocognit,gocyclo // dispatch-heavy; splitting would reduce linearity
	if s.PostgresDSN == "" {
		return fmt.Errorf("%w: storage.postgres_dsn required", ErrInvalidConfig)
	}
	// CFG-03 (audit-2026-07-23): two conflicting `_env`-suffixed
	// conventions share the identical toml suffix with nothing
	// stopping an operator from swapping them — redis_password_env /
	// clickhouse_serving_password_env hold the secret VALUE (C3-15
	// legacy misnomer; see field docs), while s3_access_key_env /
	// s3_secret_key_env hold the NAME of an env var to dereference.
	// Catch the swap heuristically: a "holds the value" field that
	// itself looks like one of this project's own env-var names is
	// almost certainly a copy-paste of the wrong convention.
	if envVarNameLikePattern.MatchString(s.RedisPassword) {
		return fmt.Errorf("%w: storage.redis_password_env looks like an env-var NAME (%q), not a secret value — "+
			"this field holds the password itself (see field doc); did you mean to export that variable "+
			"instead of pasting its name?", ErrInvalidConfig, s.RedisPassword)
	}
	if envVarNameLikePattern.MatchString(s.ClickHouseServingPassword) {
		return fmt.Errorf("%w: storage.clickhouse_serving_password_env looks like an env-var NAME (%q), not a secret "+
			"value — this field holds the password itself (see field doc)", ErrInvalidConfig, s.ClickHouseServingPassword)
	}
	if s.S3AccessKeyEnv != "" && !envVarNameShapePattern.MatchString(s.S3AccessKeyEnv) {
		return fmt.Errorf("%w: storage.s3_access_key_env %q doesn't look like an env-var NAME (expected "+
			"UPPER_SNAKE_CASE) — this field holds the NAME to dereference, not the credential itself",
			ErrInvalidConfig, s.S3AccessKeyEnv)
	}
	if s.S3SecretKeyEnv != "" && !envVarNameShapePattern.MatchString(s.S3SecretKeyEnv) {
		return fmt.Errorf("%w: storage.s3_secret_key_env %q doesn't look like an env-var NAME (expected "+
			"UPPER_SNAKE_CASE) — this field holds the NAME to dereference, not the credential itself",
			ErrInvalidConfig, s.S3SecretKeyEnv)
	}
	if !strings.HasPrefix(s.PostgresDSN, "postgres://") &&
		!strings.HasPrefix(s.PostgresDSN, "postgresql://") {
		return fmt.Errorf("%w: storage.postgres_dsn %q must start with postgres:// or postgresql://",
			ErrInvalidConfig, s.PostgresDSN)
	}
	if s.RedisAddr != "" {
		if _, _, err := net.SplitHostPort(s.RedisAddr); err != nil {
			return fmt.Errorf("%w: storage.redis_addr %q must be host:port: %w",
				ErrInvalidConfig, s.RedisAddr, err)
		}
	}
	if len(s.RedisSentinelAddrs) > 0 {
		if s.RedisMasterName == "" {
			return fmt.Errorf("%w: storage.redis_master_name required when redis_sentinel_addrs is set",
				ErrInvalidConfig)
		}
		for i, addr := range s.RedisSentinelAddrs {
			if _, _, err := net.SplitHostPort(addr); err != nil {
				return fmt.Errorf("%w: storage.redis_sentinel_addrs[%d] %q must be host:port: %w",
					ErrInvalidConfig, i, addr, err)
			}
		}
	}
	// S3 block is all-or-nothing. If an endpoint is set, the
	// dependent fields must also be set — operators who set only
	// some of them get a silent failure at archive-publish time.
	// Empty endpoint disables object storage (local dev / testing).
	if s.S3Endpoint != "" {
		if _, err := url.Parse(s.S3Endpoint); err != nil || !strings.Contains(s.S3Endpoint, "://") {
			return fmt.Errorf("%w: storage.s3_endpoint %q must be a full URL",
				ErrInvalidConfig, s.S3Endpoint)
		}
		if s.S3BucketArchive == "" {
			return fmt.Errorf("%w: storage.s3_bucket_archive required when s3_endpoint is set",
				ErrInvalidConfig)
		}
		if s.S3BucketLive == "" {
			return fmt.Errorf("%w: storage.s3_bucket_live required when s3_endpoint is set",
				ErrInvalidConfig)
		}
		if s.S3AccessKeyEnv == "" {
			return fmt.Errorf("%w: storage.s3_access_key_env required when s3_endpoint is set",
				ErrInvalidConfig)
		}
		if s.S3SecretKeyEnv == "" {
			return fmt.Errorf("%w: storage.s3_secret_key_env required when s3_endpoint is set",
				ErrInvalidConfig)
		}
		// Bucket names must be DNS-compatible per AWS S3 rules:
		// lowercase, 3–63 chars, alnum + hyphen, can't be an IP.
		// MinIO is more permissive but the AWS rule is a safe
		// super-set for portability.
		for _, b := range []struct{ name, v string }{
			{"s3_bucket_archive", s.S3BucketArchive},
			{"s3_bucket_live", s.S3BucketLive},
		} {
			if !s3BucketPattern.MatchString(b.v) {
				return fmt.Errorf("%w: storage.%s %q must be lowercase alnum + hyphen, 3-63 chars",
					ErrInvalidConfig, b.name, b.v)
			}
		}
	}
	// Cold-tier credentials are all-or-nothing (ADR-0027). Like their
	// hot counterparts these hold the NAME of an env var, not the
	// credential; unlike them, EMPTY is a meaningful value — it selects
	// anonymous reads, which is correct for the production target
	// (aws-public-blockchain is a public AWS Open Data bucket). Half a
	// pair is therefore never a sane state: pipeline.NewColdDataStore
	// would have to guess whether the operator meant "anonymous" or
	// "static creds", and guessing "anonymous" on a private bucket is
	// the exact silent-degradation shape the 2026-07-25 cold-tier
	// incident was made of. Reject at load time instead.
	if (s.S3ColdAccessKeyEnv == "") != (s.S3ColdSecretKeyEnv == "") {
		return fmt.Errorf("%w: storage.s3_cold_access_key_env (%q) and storage.s3_cold_secret_key_env (%q) must "+
			"be set together — leave BOTH empty for anonymous reads (public buckets), or name BOTH env vars for "+
			"static credentials", ErrInvalidConfig, s.S3ColdAccessKeyEnv, s.S3ColdSecretKeyEnv)
	}
	if s.S3ColdAccessKeyEnv != "" && !envVarNameShapePattern.MatchString(s.S3ColdAccessKeyEnv) {
		return fmt.Errorf("%w: storage.s3_cold_access_key_env %q doesn't look like an env-var NAME (expected "+
			"UPPER_SNAKE_CASE) — this field holds the NAME to dereference, not the credential itself",
			ErrInvalidConfig, s.S3ColdAccessKeyEnv)
	}
	if s.S3ColdSecretKeyEnv != "" && !envVarNameShapePattern.MatchString(s.S3ColdSecretKeyEnv) {
		return fmt.Errorf("%w: storage.s3_cold_secret_key_env %q doesn't look like an env-var NAME (expected "+
			"UPPER_SNAKE_CASE) — this field holds the NAME to dereference, not the credential itself",
			ErrInvalidConfig, s.S3ColdSecretKeyEnv)
	}
	// ClickHouse feed-switch dependency (ADR-0034 #10, C3-20): the
	// projector reads forward events from the CH lake's contract_events,
	// so CH must actually be BEING WRITTEN — i.e. the real-time dual-sink
	// must be on. projector_source=true with live_sink=false silently
	// mis-reads a lake nothing keeps current. Enforce the documented
	// "Requires clickhouse_live_sink" invariant at validate-time.
	if s.ClickHouseProjectorSource && !s.ClickHouseLiveSink {
		return fmt.Errorf("%w: storage.clickhouse_projector_source requires storage.clickhouse_live_sink "+
			"(the projector reads forward events from ClickHouse, so the dual-sink must be writing them)",
			ErrInvalidConfig)
	}
	return nil
}

func (i IngestionConfig) validate() error {
	switch i.CursorStoreScheme {
	case "postgres", "redis":
		// ok
	default:
		return fmt.Errorf("%w: ingestion.cursor_store_scheme %q must be postgres/redis",
			ErrInvalidConfig, i.CursorStoreScheme)
	}
	if i.BackfillBatchSize == 0 {
		return fmt.Errorf("%w: ingestion.backfill_batch_size must be > 0",
			ErrInvalidConfig)
	}
	// Duplicate source names would spawn multiple consumers on the
	// same event stream — double-counting metrics and doubling orphan
	// buffer memory. Case-fold so ["soroswap", "Soroswap"] is caught
	// too (buildSources lowercases before dispatch).
	//
	// Unknown names are also rejected here. Without this check a typo
	// like "sorowsap" reaches the indexer's buildSources() switch at
	// startup — by then -dry-run has already paid for storage Open +
	// RPC probe, so the operator waits 5+ seconds for a one-char typo
	// to surface. Cross-checking KnownSources closes that loop.
	seen := make(map[string]struct{}, len(i.EnabledSources))
	for _, name := range i.EnabledSources {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			return fmt.Errorf("%w: ingestion.enabled_sources contains empty entry",
				ErrInvalidConfig)
		}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%w: ingestion.enabled_sources has duplicate %q",
				ErrInvalidConfig, name)
		}
		if _, known := KnownSources[key]; !known {
			return fmt.Errorf("%w: ingestion.enabled_sources has unknown source %q "+
				"(expected one of: see config.KnownSources)",
				ErrInvalidConfig, name)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func (o OracleConfig) validate() error {
	// Empty is allowed — operator can disable every oracle.
	// When set, addresses must be valid C-strkeys.
	for name, addr := range map[string]string{
		"oracle.reflector.dex_contract":           o.Reflector.DEXContract,
		"oracle.reflector.cex_contract":           o.Reflector.CEXContract,
		"oracle.reflector.fx_contract":            o.Reflector.FXContract,
		"oracle.redstone.adapter_contract":        o.Redstone.AdapterContract,
		"oracle.band.standard_reference_contract": o.Band.StandardReferenceContract,
	} {
		if addr == "" {
			continue
		}
		if !contractIDPattern.MatchString(addr) {
			return fmt.Errorf("%w: %s %q is not a valid C-strkey",
				ErrInvalidConfig, name, addr)
		}
	}
	return nil
}

func (a AggregateConfig) validate() error {
	if a.VWAPWindowSeconds <= 0 {
		return fmt.Errorf("%w: aggregate.vwap_window_seconds must be > 0",
			ErrInvalidConfig)
	}
	if a.TWAPWindowSeconds <= 0 {
		return fmt.Errorf("%w: aggregate.twap_window_seconds must be > 0",
			ErrInvalidConfig)
	}
	if a.MinUSDVolume < 0 {
		return fmt.Errorf("%w: aggregate.min_usd_volume must be >= 0",
			ErrInvalidConfig)
	}
	if a.MinMarketCapVolumeUSD < 0 {
		return fmt.Errorf("%w: aggregate.min_market_cap_volume_usd must be >= 0",
			ErrInvalidConfig)
	}
	// A cap above the store's ceiling is REFUSED, not clamped. Silently
	// clamping does double damage: the scan does not widen, AND the
	// orchestrator's truncation detector (len(t) >= MaxTradesPerWindow)
	// can never fire again — so an operator who follows the field's own
	// godoc ("raise the cap if that counter fires sustainedly") would see
	// the ~48%-of-windows truncation rate measured on r1 drop to a
	// reported 0% with nothing actually fixed (cold audit 2026-08-04).
	// tradesInRangeCeiling mirrors timescale.MaxTradesInRangeLimit.
	// Duplicated as a literal because internal/config is a leaf package
	// and must not import a storage adapter; timescale's own
	// TestMaxTradesInRangeLimit_matchesConfigCeiling keeps the two in
	// lockstep.
	const tradesInRangeCeiling = 10000
	if a.MaxTradesPerWindow > tradesInRangeCeiling {
		return fmt.Errorf(
			"%w: aggregate.max_trades_per_window is %d but the trade reader clamps at %d — "+
				"raising it above the ceiling silently does nothing AND blinds the truncation detector; "+
				"move the large windows to a SQL-side aggregate instead",
			ErrInvalidConfig, a.MaxTradesPerWindow, tradesInRangeCeiling)
	}

	if a.OutlierSigmaThreshold <= 0 {
		return fmt.Errorf("%w: aggregate.outlier_sigma_threshold must be > 0",
			ErrInvalidConfig)
	}
	// max_hops: 0 = "use library default (3)"; any other value must be
	// in [2,4] (a route needs at least 2 legs to be a cross, and 4
	// covers the obscure×obscure worst case — see internal/aggregate/
	// router.go FindRoutes).
	if a.MaxHops != 0 && (a.MaxHops < 2 || a.MaxHops > 4) {
		return fmt.Errorf("%w: aggregate.max_hops must be 0 (default) or in [2,4], got %d",
			ErrInvalidConfig, a.MaxHops)
	}
	// min_route_confidence is a weakest-link confidence floor in [0,1];
	// 0 disables the gate.
	if a.MinRouteConfidence < 0 || a.MinRouteConfidence > 1 {
		return fmt.Errorf("%w: aggregate.min_route_confidence must be in [0,1], got %v",
			ErrInvalidConfig, a.MinRouteConfidence)
	}
	for _, raw := range a.Pairs {
		if _, err := parsePairString(raw); err != nil {
			return fmt.Errorf("%w: aggregate.pairs entry %q: %w",
				ErrInvalidConfig, raw, err)
		}
	}
	if err := a.CompositeReference.validate(a); err != nil {
		return err
	}
	for _, raw := range a.Windows {
		if _, err := time.ParseDuration(raw); err != nil {
			return fmt.Errorf("%w: aggregate.windows entry %q: %w",
				ErrInvalidConfig, raw, err)
		}
	}
	return nil
}

// validate checks the AnomalyConfig's Phase 2 thresholds plus the
// Thresholds / Classifications map shapes. CFG-05 (audit-2026-07-23):
// Thresholds/Classifications used to be validated only at consumer
// time by silently falling through to the loose ClassDefault
// thresholds — so a typo'd class name (e.g. "stablecoins") in EITHER
// map applied the wrong (looser) thresholds to that class/asset with
// no error anywhere, defeating the anomaly-freeze safety net for
// exactly the assets an operator thought they'd tightened. Rejecting
// unknown class names at config-load time closes that gap.
func (a AnomalyConfig) validate() error {
	known := make(map[string]struct{}, len(anomaly.AllClasses()))
	for _, c := range anomaly.AllClasses() {
		known[c.String()] = struct{}{}
	}
	for class := range a.Thresholds {
		if _, ok := known[class]; !ok {
			return fmt.Errorf("%w: anomaly.thresholds has unknown class %q (see anomaly.AllClasses)",
				ErrInvalidConfig, class)
		}
	}
	for assetID, class := range a.Classifications {
		if _, ok := known[class]; !ok {
			return fmt.Errorf("%w: anomaly.classifications[%q] has unknown class %q (see anomaly.AllClasses)",
				ErrInvalidConfig, assetID, class)
		}
	}
	return a.Phase2.validate()
}

// validate enforces the per-field bounds documented on
// [Phase2FreezeConfig]. Catches config errors at startup rather
// than letting an out-of-band threshold silently disable the
// freeze gate (e.g. confidence_max_freeze=2.0 means "always freeze").
func (p Phase2FreezeConfig) validate() error {
	if p.ConfidenceMaxFreeze < 0 || p.ConfidenceMaxFreeze > 1 {
		return fmt.Errorf("%w: anomaly.phase2.confidence_max_freeze must be in [0, 1] (got %v)",
			ErrInvalidConfig, p.ConfidenceMaxFreeze)
	}
	if p.ZScoreMinFreeze <= 0 {
		return fmt.Errorf("%w: anomaly.phase2.z_score_min_freeze must be > 0 (got %v)",
			ErrInvalidConfig, p.ZScoreMinFreeze)
	}
	if p.SourceCountMaxFreeze < 0 {
		return fmt.Errorf("%w: anomaly.phase2.source_count_max_freeze must be >= 0 (got %d)",
			ErrInvalidConfig, p.SourceCountMaxFreeze)
	}
	return p.validateLifecycle()
}

// validateLifecycle enforces the ADR-0019 freeze-duration bounds.
//
// Split out to keep [Phase2FreezeConfig.validate] under the funlen
// ceiling, and because these checks guard a different failure mode:
// a bad THRESHOLD makes the freeze fire wrongly, a bad DURATION makes
// a correctly-fired freeze end at the wrong time — which is the
// silent failure ADR-0019's ladder exists to prevent.
func (p Phase2FreezeConfig) validateLifecycle() error {
	for _, f := range []struct {
		name string
		mins int
	}{
		{"initial_hold_minutes", p.InitialHoldMinutes},
		{"uncorroborated_initial_hold_minutes", p.UncorroboratedInitialHoldMinutes},
		{"extension_minutes", p.ExtensionMinutes},
	} {
		if f.mins < 0 {
			return fmt.Errorf("%w: anomaly.phase2.%s must be >= 0 (got %d; 0 = use the ADR-0019 default)",
				ErrInvalidConfig, f.name, f.mins)
		}
	}
	if p.MaxExtensions < 0 {
		return fmt.Errorf("%w: anomaly.phase2.max_extensions must be >= 0 (got %d; 0 = use the ADR-0019 default of 4)",
			ErrInvalidConfig, p.MaxExtensions)
	}
	if p.UnfreezeConfidenceMin < 0 || p.UnfreezeConfidenceMin > 1 {
		return fmt.Errorf("%w: anomaly.phase2.unfreeze_confidence_min must be in [0, 1] (got %v)",
			ErrInvalidConfig, p.UnfreezeConfidenceMin)
	}
	if p.UnfreezeZScoreMax < 0 {
		return fmt.Errorf("%w: anomaly.phase2.unfreeze_z_score_max must be >= 0 (got %v)",
			ErrInvalidConfig, p.UnfreezeZScoreMax)
	}
	if p.UnfreezeBuckets < 0 {
		return fmt.Errorf("%w: anomaly.phase2.unfreeze_buckets must be >= 0 (got %d; 0 = use the ADR-0019 default of 2)",
			ErrInvalidConfig, p.UnfreezeBuckets)
	}
	// Hysteresis, the invariant this whole lifecycle exists to
	// restore: no bucket may satisfy BOTH the freeze condition and the
	// auto-unfreeze condition. If one can, a signal sitting near the
	// trigger flaps the pair frozen and unfrozen bucket after bucket —
	// publishing, each time it unfreezes, exactly the value the freeze
	// just refused. That was the pre-lifecycle behaviour and it must
	// not be reachable by config.
	//
	// The z axis is what carries the disjointness, and it is the ONLY
	// axis that has to. The confidence bands deliberately DO overlap
	// at the shipped defaults — auto-unfreeze wants confidence > 0.30
	// while the freeze fires below 0.45, so (0.30, 0.45) satisfies
	// both confidence legs — and that is harmless because the two
	// conditions are ANDs whose z legs (z > 5.0 vs z < 3.0) cannot
	// hold together. Rejecting the confidence overlap would reject the
	// ADR's own numbers.
	//
	// Checked only when the operator set both, since 0 means "use the
	// default" and the defaults satisfy this by construction.
	if p.UnfreezeZScoreMax != 0 && p.ZScoreMinFreeze != 0 && p.UnfreezeZScoreMax > p.ZScoreMinFreeze {
		return fmt.Errorf("%w: anomaly.phase2.unfreeze_z_score_max (%v) must be <= z_score_min_freeze (%v) — "+
			"otherwise the release band overlaps the fire band and the pair flaps frozen/unfrozen at the trigger",
			ErrInvalidConfig, p.UnfreezeZScoreMax, p.ZScoreMinFreeze)
	}
	return nil
}

// validate checks the [divergence] section for boot-time crashers.
// Threshold / MinSourcesForWarning / PerReferenceTimeoutSeconds (both
// here and in Supply) are all clamped to sane defaults by
// divergence.NewService / SupplyService when <=0, so they're left
// unchecked here. Supply.RefreshIntervalSeconds is the exception: it
// feeds time.NewTicker(interval) directly in
// runSupplyDivergenceRefresh (cmd/stellarindex-aggregator/main.go)
// with no downstream clamp, so <=0 panics the aggregator at startup
// — the identical NewTicker(0) failure mode G19-02 already fixed for
// SupplyConfig.AggregatorRefreshCadence.
func (d DivergenceConfig) validate() error {
	if d.Supply.Enabled && d.Supply.RefreshIntervalSeconds <= 0 {
		return fmt.Errorf("%w: divergence.supply.refresh_interval_seconds must be > 0 when "+
			"divergence.supply.enabled is true (got %d)", ErrInvalidConfig, d.Supply.RefreshIntervalSeconds)
	}
	return nil
}

// parsePairString resolves a "<base>/<quote>" string into a
// canonical.Pair via canonical.ParseAsset on each side. Lives here
// (not in canonical/) so the canonical package stays free of
// validation-message specifics — the aggregator binary calls the
// same helper at startup to materialise its [Pair] slice.
func parsePairString(s string) (canonical.Pair, error) {
	slash := strings.LastIndex(s, "/")
	if slash <= 0 || slash == len(s)-1 {
		return canonical.Pair{}, fmt.Errorf("expected \"<base>/<quote>\" with a single slash separator")
	}
	base, err := canonical.ParseAsset(s[:slash])
	if err != nil {
		return canonical.Pair{}, fmt.Errorf("base: %w", err)
	}
	quote, err := canonical.ParseAsset(s[slash+1:])
	if err != nil {
		return canonical.Pair{}, fmt.Errorf("quote: %w", err)
	}
	return canonical.NewPair(base, quote)
}

// AggregatorPairs resolves the operator-supplied pair strings into
// canonical.Pair instances. Returns nil when no pairs are
// configured — callers fall back to their built-in default.
//
// validate() already rejects unparseable entries at startup, so
// this re-parse is infallible in practice; we still return an
// error to keep the seam testable and to surface the regression
// loudly if validation is ever bypassed.
func (a AggregateConfig) AggregatorPairs() ([]canonical.Pair, error) {
	if len(a.Pairs) == 0 {
		return nil, nil
	}
	out := make([]canonical.Pair, 0, len(a.Pairs))
	for _, raw := range a.Pairs {
		p, err := parsePairString(raw)
		if err != nil {
			return nil, fmt.Errorf("aggregate.pairs entry %q: %w", raw, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// AggregatorWindows is the time.Duration twin of AggregatorPairs —
// resolves the configured window strings, returning nil when the
// list is empty so callers fall back to orchestrator.DefaultWindows.
func (a AggregateConfig) AggregatorWindows() ([]time.Duration, error) {
	if len(a.Windows) == 0 {
		return nil, nil
	}
	out := make([]time.Duration, 0, len(a.Windows))
	for _, raw := range a.Windows {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("aggregate.windows entry %q: %w", raw, err)
		}
		out = append(out, d)
	}
	return out, nil
}

// validate enforces the [CompositeReferenceConfig] bounds and requires
// every allow-listed target to have a triangulation chain: the chain's
// legs ARE the reference, so a listed target without one would be
// silently dead ("composite_unavailable: no_chain" every bucket) — the
// config-gated-feature-ships-dead class. The chain check applies only
// when the mechanism is enabled AND a triangulation table exists: the
// binary default (chains empty, list populated) is a valid dormant
// state, since the reference is defined by the chains an operator
// deploys.
func (c CompositeReferenceConfig) validate(a AggregateConfig) error {
	if c.ToleranceBps < 0 || c.ToleranceBps > 10_000 {
		return fmt.Errorf("%w: aggregate.composite_reference.tolerance_bps must be in [0,10000] (0 = default), got %d",
			ErrInvalidConfig, c.ToleranceBps)
	}
	if c.MinLegSources < 0 {
		return fmt.Errorf("%w: aggregate.composite_reference.min_leg_sources must be >= 0 (0 = default), got %d",
			ErrInvalidConfig, c.MinLegSources)
	}
	if c.FXMaxAgeHours < 0 {
		return fmt.Errorf("%w: aggregate.composite_reference.fx_max_age_hours must be >= 0 (0 = default), got %d",
			ErrInvalidConfig, c.FXMaxAgeHours)
	}
	chains := make(map[string]struct{}, len(a.Triangulations))
	for _, row := range a.Triangulations {
		chains[row.Target] = struct{}{}
	}
	for _, raw := range c.Targets {
		if _, err := parsePairString(raw); err != nil {
			return fmt.Errorf("%w: aggregate.composite_reference.targets entry %q: %w",
				ErrInvalidConfig, raw, err)
		}
		if _, ok := chains[raw]; c.Enabled && len(a.Triangulations) > 0 && !ok {
			return fmt.Errorf("%w: aggregate.composite_reference.targets entry %q has no [[aggregate.triangulations]] row — the chain's legs are the reference",
				ErrInvalidConfig, raw)
		}
	}
	return nil
}

// CompositeReferenceTargets resolves the composite-reference allow-list
// into canonical pairs. Nil when the list is empty.
func (a AggregateConfig) CompositeReferenceTargets() ([]canonical.Pair, error) {
	if len(a.CompositeReference.Targets) == 0 {
		return nil, nil
	}
	out := make([]canonical.Pair, 0, len(a.CompositeReference.Targets))
	for i, raw := range a.CompositeReference.Targets {
		p, err := parsePairString(raw)
		if err != nil {
			return nil, fmt.Errorf("aggregate.composite_reference.targets[%d] %q: %w", i, raw, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// ResolvedTriangulationChain is the parsed representation of a
// TriangulationChainConfig — all pair strings resolved to
// canonical.Pair. Returned by [AggregatorTriangulations]; callers
// pass the slice straight into orchestrator.Config.Triangulations
// after type-converting each row.
type ResolvedTriangulationChain struct {
	Target canonical.Pair
	Legs   []canonical.Pair
}

// AggregatorTriangulations resolves the operator-supplied
// TriangulationChainConfig rows into ResolvedTriangulationChain
// instances. Returns nil when no chains are configured.
//
// Each row's Target + Legs are parsed via parsePairString. The
// per-chain structural validation (legs chainable, endpoints match
// target) lives in orchestrator.ValidateTriangulationChain — this
// helper only does the parse step.
func (a AggregateConfig) AggregatorTriangulations() ([]ResolvedTriangulationChain, error) {
	if len(a.Triangulations) == 0 {
		return nil, nil
	}
	out := make([]ResolvedTriangulationChain, 0, len(a.Triangulations))
	for i, row := range a.Triangulations {
		target, err := parsePairString(row.Target)
		if err != nil {
			return nil, fmt.Errorf("aggregate.triangulations[%d].target %q: %w", i, row.Target, err)
		}
		legs := make([]canonical.Pair, 0, len(row.Legs))
		for j, raw := range row.Legs {
			p, err := parsePairString(raw)
			if err != nil {
				return nil, fmt.Errorf("aggregate.triangulations[%d].legs[%d] %q: %w", i, j, raw, err)
			}
			legs = append(legs, p)
		}
		out = append(out, ResolvedTriangulationChain{Target: target, Legs: legs})
	}
	return out, nil
}

func (a APIConfig) validate() error {
	if a.ListenAddr == "" {
		return fmt.Errorf("%w: api.listen_addr required", ErrInvalidConfig)
	}
	if _, _, err := net.SplitHostPort(a.ListenAddr); err != nil {
		return fmt.Errorf("%w: api.listen_addr %q must be host:port: %w",
			ErrInvalidConfig, a.ListenAddr, err)
	}
	switch a.AuthMode {
	case "none", "apikey", "apikey_optional", "sep10":
		// ok
	default:
		return fmt.Errorf("%w: api.auth_mode %q must be none/apikey/apikey_optional/sep10",
			ErrInvalidConfig, a.AuthMode)
	}
	if a.AnonRateLimitPerMin < 0 {
		return fmt.Errorf("%w: api.anon_rate_limit_per_min must be >= 0",
			ErrInvalidConfig)
	}
	if a.KeyRateLimitPerMin < 0 {
		return fmt.Errorf("%w: api.key_rate_limit_per_min must be >= 0",
			ErrInvalidConfig)
	}
	for i, raw := range a.TrustedProxyCIDRs {
		if _, err := netip.ParsePrefix(strings.TrimSpace(raw)); err != nil {
			return fmt.Errorf("%w: api.trusted_proxy_cidrs[%d] %q must be a valid CIDR: %w",
				ErrInvalidConfig, i, raw, err)
		}
	}
	// CFG-05 (audit-2026-07-23): request_timeout is documented as the
	// PRIMARY bound and serving_statement_timeout as the SQL-side
	// backstop, which only works as defense-in-depth when the
	// statement timeout is the longer of the two (so the app-layer
	// deadline fires first). An inverted pair silently swaps which
	// layer actually governs — the DB kills the query first, so a
	// batch/admin caller relying on the documented request_timeout
	// ceiling gets cut off early instead. 0 on either side disables
	// that layer entirely and is unaffected by this check.
	if a.RequestTimeout > 0 && a.ServingStatementTimeout > 0 && a.ServingStatementTimeout <= a.RequestTimeout {
		return fmt.Errorf("%w: api.serving_statement_timeout (%v) must be longer than api.request_timeout (%v) — "+
			"the SQL-side backstop only works as defense-in-depth when it fires AFTER the app-layer deadline",
			ErrInvalidConfig, a.ServingStatementTimeout, a.RequestTimeout)
	}
	return nil
}

func (o ObsConfig) validate() error {
	switch o.LogLevel {
	case "debug", "info", "warn", "warning", "error":
		// ok
	default:
		return fmt.Errorf("%w: obs.log_level %q must be debug/info/warn/error",
			ErrInvalidConfig, o.LogLevel)
	}
	switch o.LogFormat {
	case "json", "text", "console":
		// ok
	default:
		return fmt.Errorf("%w: obs.log_format %q must be json/text/console",
			ErrInvalidConfig, o.LogFormat)
	}
	switch o.TraceExporter {
	case "none":
		// ok — the only currently-wired value
	case "otlp":
		// Reserved for the future tracing rollout. Reject loud now
		// so an operator who sets this thinks they enabled tracing
		// when actually nothing in the binary consumes the field.
		// Re-allow once an OTel TracerProvider + exporter are wired
		// in cmd/stellarindex-{api,indexer,aggregator}/main.go.
		return fmt.Errorf("%w: obs.trace_exporter %q is reserved for the future tracing rollout and is not yet wired in this build; set to \"none\"",
			ErrInvalidConfig, o.TraceExporter)
	default:
		return fmt.Errorf("%w: obs.trace_exporter %q must be \"none\" (the only currently-wired value)",
			ErrInvalidConfig, o.TraceExporter)
	}
	if o.TraceSample < 0 || o.TraceSample > 1 {
		return fmt.Errorf("%w: obs.trace_sample %v must be in [0, 1]",
			ErrInvalidConfig, o.TraceSample)
	}
	if o.MetricsListen != "" {
		if _, _, err := net.SplitHostPort(o.MetricsListen); err != nil {
			return fmt.Errorf("%w: obs.metrics_listen %q must be host:port: %w",
				ErrInvalidConfig, o.MetricsListen, err)
		}
	}
	return nil
}

var (
	// regionIDPattern — lowercase alphanumeric, 1-16 chars. Keeps
	// the identifier short + filesystem-safe.
	regionIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,15}$`)

	// contractIDPattern matches the Stellar Soroban C-strkey format —
	// identical to canonical.IsContractID, duplicated here so config
	// doesn't depend on canonical (cycle avoidance).
	contractIDPattern = regexp.MustCompile(`^C[A-Z2-7]{55}$`)

	// accountIDPattern matches the Stellar classic G-strkey (ed25519
	// public key) format. Same shallow shape-only check as
	// contractIDPattern (no checksum verification) — good enough to
	// catch a typo'd/truncated address at config-load time (DOM-11,
	// audit-2026-07-23); a byte-flip that still checksums is caught
	// downstream when the account doesn't resolve on-chain.
	accountIDPattern = regexp.MustCompile(`^G[A-Z2-7]{55}$`)

	// envVarNameLikePattern matches this project's own STELLARINDEX_*
	// env-var naming convention. Used to catch the "holds the VALUE"
	// `_env`-suffixed fields (redis_password_env,
	// clickhouse_serving_password_env — see their field docs, C3-15)
	// being populated with an env-var NAME by mistake instead of the
	// secret itself (CFG-03, audit-2026-07-23).
	envVarNameLikePattern = regexp.MustCompile(`^STELLARINDEX_[A-Z0-9_]+$`)

	// envVarNameShapePattern matches the general shape of an
	// UPPER_SNAKE_CASE identifier — what a "holds the NAME of an env
	// var" field (s3_access_key_env, s3_secret_key_env) should look
	// like. Catches the inverse CFG-03 mistake: a literal secret (or
	// anything else) shipped where a name was expected.
	envVarNameShapePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

	// s3BucketPattern — AWS S3 DNS-compatible bucket naming rules:
	// lowercase, 3–63 chars, alnum + hyphen, must start/end alnum.
	// MinIO is more permissive but we pick the strictest rule so
	// configs stay portable across providers.
	s3BucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
)
