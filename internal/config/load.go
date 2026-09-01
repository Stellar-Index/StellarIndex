package config

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// Load reads a TOML config file from path and returns a fully-
// populated [Config] with defaults applied for any field the file
// omits. File precedence beats the built-in defaults; env-var
// overrides (see [ApplyEnvOverrides]) beat the file.
//
// Returns a wrapped error identifying the path + offending line on
// parse failure.
func Load(path string) (Config, error) {
	// G304 false positive: operator-supplied config path is the
	// whole point of the flag. No user-controlled input reaches
	// here — the indexer's -config flag is parsed from argv.
	f, err := os.Open(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return Config{}, fmt.Errorf("config: open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return LoadReader(f, path)
}

// LoadReader is [Load] with a supplied io.Reader. Useful for tests
// that don't want to touch the filesystem.
func LoadReader(r io.Reader, origin string) (Config, error) {
	c := Default()
	meta, err := toml.NewDecoder(r).Decode(&c)
	if err != nil {
		return Config{}, fmt.Errorf("config: decode %s: %w", origin, err)
	}
	if undec := meta.Undecoded(); len(undec) > 0 {
		// Unknown keys are a hard error — silent typos in config
		// are one of the most common deployment bugs.
		keys := make([]string, 0, len(undec))
		for _, k := range undec {
			keys = append(keys, k.String())
		}
		return Config{}, fmt.Errorf("config: unknown keys in %s: %s",
			origin, strings.Join(keys, ", "))
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("config: %s: %w", origin, err)
	}
	return c, nil
}

// ApplyEnvOverrides mutates c in place, replacing any field that has
// an `env:` tag with the env-var's value if that var is set. Returns
// the config-path (dotted, e.g. "storage.postgres_dsn") of every
// field an env var actually overrode, in override order — NEVER the
// values themselves (most of these fields are secrets). CFG-01
// (audit-2026-07-23): callers that want to know WHICH fields the
// environment silently replaced — without echoing a secret — use
// this return value; see [LoadWithEnv] for the standard "log it at
// boot" consumer.
//
// Secret fields follow the `env: "NAME"` convention where NAME is
// the var holding the actual secret — see
// [StorageConfig.PostgresDSN] for the canonical example.
//
// Unknown / empty env vars are ignored; no field is overwritten with
// an empty string.
//
// Does NOT re-validate. Callers that want env-driven values held to
// the same invariants as file-driven values should use [LoadWithEnv]
// or call [Config.Validate] after this.
func (c *Config) ApplyEnvOverrides() []string {
	var overridden []string
	if v := os.Getenv("STELLARINDEX_POSTGRES_DSN"); v != "" {
		c.Storage.PostgresDSN = v
		overridden = append(overridden, "storage.postgres_dsn")
	}
	if v := os.Getenv("STELLARINDEX_REDIS_PASSWORD"); v != "" {
		c.Storage.RedisPassword = v
		overridden = append(overridden, "storage.redis_password_env")
	}
	if v := os.Getenv("STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD"); v != "" {
		c.Storage.ClickHouseServingPassword = v
		overridden = append(overridden, "storage.clickhouse_serving_password_env")
	}
	// NOTE: STELLARINDEX_S3_ACCESS_KEY / STELLARINDEX_S3_SECRET_KEY are
	// deliberately NOT overridden here. StorageConfig.S3AccessKeyEnv holds the
	// NAME of the env var, not its value; buildS3Client resolves it via
	// os.Getenv(name). Overwriting the name with the value here corrupted the
	// resolution (os.Getenv("AKIA…")→"") and silently dropped S3 static creds
	// (audit-2026-06-14 A16-01). The fields carry no `env:` tag for the same
	// reason — see config.go StorageConfig.
	if v := os.Getenv("EXCHANGERATESAPI_KEY"); v != "" {
		c.External.ExchangeRatesApi.APIKey = v
		overridden = append(overridden, "external.exchangeratesapi.api_key")
	}
	if v := os.Getenv("COINMARKETCAP_API_KEY"); v != "" {
		c.External.CoinMarketCap.APIKey = v
		overridden = append(overridden, "external.coinmarketcap.api_key")
	}
	if v := os.Getenv("CRYPTOCOMPARE_API_KEY"); v != "" {
		c.External.CryptoCompare.APIKey = v
		overridden = append(overridden, "external.cryptocompare.api_key")
	}
	if v := os.Getenv("COINGECKO_API_KEY"); v != "" {
		// Feeds the divergence supply cross-check's CoinGecko reference
		// (internal/divergence/supply.go) — the Pro key that lifts the
		// free-tier 429 ceiling the reference otherwise hits.
		c.Divergence.Supply.CoinGecko.APIKey = v
		overridden = append(overridden, "divergence.supply.coingecko.api_key")
	}
	if v := os.Getenv("CHAINLINK_RPC_URL"); v != "" {
		c.External.Chainlink.RPCUrl = v
		// The divergence Chainlink reference is a SECOND consumer of the
		// same Ethereum JSON-RPC endpoint (internal/divergence/chainlink.go).
		// It historically carried its own env-less rpc_url, which silently
		// fell back to a public RPC that now answers eth_call with a
		// Cloudflare JS-challenge HTML page instead of JSON — so every
		// LookupPrice failed its JSON decode and the divergence service
		// recorded 0 chainlink rows, ever (audit 2026-06-19). Point both
		// consumers at the one operator-provided endpoint so a single
		// CHAINLINK_RPC_URL keeps the cross-check working.
		c.Divergence.Chainlink.RPCURL = v
		overridden = append(overridden, "external.chainlink.rpc_url", "divergence.chainlink.rpc_url")
	}
	return overridden
}

// LoadWithEnv is [Load] + [ApplyEnvOverrides] + a second [Validate].
// Use this in binaries so a bad env-var value (e.g., malformed
// STELLARINDEX_POSTGRES_DSN overriding a known-good DSN from the file)
// fails fast with the same ErrInvalidConfig error as a bad file,
// instead of opening the pool and getting a confusing DB error
// at connect time.
//
// CFG-01 (audit-2026-07-23): env overrides used to apply completely
// silently — an operator debugging "why is this deployment using the
// wrong DSN" had no signal that the environment, not the TOML file,
// won. Logs (at Info, via the package-default slog logger — this runs
// before the binary constructs its own obs-configured logger from
// [Config.Obs]) the field-path list [ApplyEnvOverrides] returns.
// Field VALUES are never logged, only paths — most overridden fields
// are secrets by construction (see ApplyEnvOverrides's doc).
func LoadWithEnv(path string) (Config, error) {
	c, err := Load(path)
	if err != nil {
		return Config{}, err
	}
	if overridden := c.ApplyEnvOverrides(); len(overridden) > 0 {
		slog.Info("config: environment overrides applied (values redacted)",
			"path", path, "fields", overridden)
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("config: %s (with env overrides): %w", path, err)
	}
	return c, nil
}
