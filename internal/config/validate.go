package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// ErrInvalidConfig is the sentinel error every validation failure
// wraps. Use errors.Is(err, ErrInvalidConfig) in callers that need
// to distinguish config bugs from I/O / decode failures.
var ErrInvalidConfig = errors.New("config: invalid")

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
	if err := c.API.validate(); err != nil {
		return err
	}
	if err := c.Obs.validate(); err != nil {
		return err
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
	for i, ep := range s.RPCEndpoints {
		if _, err := url.Parse(ep); err != nil || !strings.Contains(ep, "://") {
			return fmt.Errorf("%w: stellar.rpc_endpoints[%d] %q must be a full URL",
				ErrInvalidConfig, i, ep)
		}
	}
	if _, err := url.Parse(s.HistoryArchiveURL); err != nil {
		return fmt.Errorf("%w: stellar.history_archive_url %q: %v",
			ErrInvalidConfig, s.HistoryArchiveURL, err)
	}
	return nil
}

func (s StorageConfig) validate() error {
	if s.PostgresDSN == "" {
		return fmt.Errorf("%w: storage.postgres_dsn required", ErrInvalidConfig)
	}
	if !strings.HasPrefix(s.PostgresDSN, "postgres://") &&
		!strings.HasPrefix(s.PostgresDSN, "postgresql://") {
		return fmt.Errorf("%w: storage.postgres_dsn %q must start with postgres:// or postgresql://",
			ErrInvalidConfig, s.PostgresDSN)
	}
	if s.RedisAddr != "" {
		if _, _, err := net.SplitHostPort(s.RedisAddr); err != nil {
			return fmt.Errorf("%w: storage.redis_addr %q must be host:port: %v",
				ErrInvalidConfig, s.RedisAddr, err)
		}
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
		seen[key] = struct{}{}
	}
	return nil
}

func (o OracleConfig) validate() error {
	// Empty is allowed — operator can disable every oracle.
	// When set, addresses must be valid C-strkeys.
	for name, addr := range map[string]string{
		"oracle.reflector.dex_contract": o.Reflector.DEXContract,
		"oracle.reflector.cex_contract": o.Reflector.CEXContract,
		"oracle.reflector.fx_contract":  o.Reflector.FXContract,
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
	if a.OutlierSigmaThreshold <= 0 {
		return fmt.Errorf("%w: aggregate.outlier_sigma_threshold must be > 0",
			ErrInvalidConfig)
	}
	return nil
}

func (a APIConfig) validate() error {
	if a.ListenAddr == "" {
		return fmt.Errorf("%w: api.listen_addr required", ErrInvalidConfig)
	}
	if _, _, err := net.SplitHostPort(a.ListenAddr); err != nil {
		return fmt.Errorf("%w: api.listen_addr %q must be host:port: %v",
			ErrInvalidConfig, a.ListenAddr, err)
	}
	switch a.AuthMode {
	case "none", "apikey", "sep10":
		// ok
	default:
		return fmt.Errorf("%w: api.auth_mode %q must be none/apikey/sep10",
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
	case "none", "otlp":
		// ok
	default:
		return fmt.Errorf("%w: obs.trace_exporter %q must be none/otlp",
			ErrInvalidConfig, o.TraceExporter)
	}
	if o.TraceSample < 0 || o.TraceSample > 1 {
		return fmt.Errorf("%w: obs.trace_sample %v must be in [0, 1]",
			ErrInvalidConfig, o.TraceSample)
	}
	if o.MetricsListen != "" {
		if _, _, err := net.SplitHostPort(o.MetricsListen); err != nil {
			return fmt.Errorf("%w: obs.metrics_listen %q must be host:port: %v",
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
)
