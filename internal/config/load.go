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
	doc, err := io.ReadAll(r)
	if err != nil {
		return Config{}, fmt.Errorf("config: read %s: %w", origin, err)
	}
	c := Default()
	meta, err := toml.Decode(string(doc), &c)
	if err != nil {
		return Config{}, fmt.Errorf("config: decode %s: %w", origin, err)
	}
	aliased, err := applyDeprecatedKeys(string(doc), meta, &c)
	if err != nil {
		return Config{}, fmt.Errorf("config: %s: %w", origin, err)
	}
	if len(aliased) > 0 {
		// Key paths only: both aliased fields are secrets.
		slog.Warn("config: deprecated keys present, rename them",
			"path", origin, "keys", aliased)
	}
	if undec := meta.Undecoded(); len(undec) > 0 {
		// Unknown keys are a hard error — silent typos in config are one
		// of the most common deployment bugs. A key on RetiredKeys is a
		// known exception: it once existed and a deployment upgrading
		// from an old configs/example.toml still carries it (#890), so
		// warn instead of refusing to boot.
		var unknown, retired []string
		for _, k := range undec {
			path := k.String()
			if isDeprecatedKey(path) {
				continue
			}
			if note, ok := retiredKeyMatch(path); ok {
				retired = append(retired, path+" ("+note+")")
				continue
			}
			unknown = append(unknown, path)
		}
		if len(retired) > 0 {
			slog.Warn("config: retired keys present, ignoring",
				"path", origin, "keys", retired)
		}
		if len(unknown) > 0 {
			return Config{}, fmt.Errorf("config: unknown keys in %s: %s",
				origin, strings.Join(unknown, ", "))
		}
	}
	c.Obs.MetricsListenSet = meta.IsDefined("obs", "metrics_listen")
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("config: %s: %w", origin, err)
	}
	return c, nil
}

// deprecatedStorageKeys decodes the values of the renamed keys, which the
// main decode leaves undecoded.
type deprecatedStorageKeys struct {
	Storage struct {
		RedisPasswordEnv             string `toml:"redis_password_env"`
		ClickHouseServingPasswordEnv string `toml:"clickhouse_serving_password_env"`
	} `toml:"storage"`
}

// deprecatedKeys lists renamed keys. Unlike a RetiredKeys entry the old
// key still takes effect, with a boot warning.
var deprecatedKeys = []struct {
	from, to string
	val      func(*deprecatedStorageKeys) string
	dst      func(*Config) *string
}{
	{
		"storage.redis_password_env", "storage.redis_password",
		func(d *deprecatedStorageKeys) string { return d.Storage.RedisPasswordEnv },
		func(c *Config) *string { return &c.Storage.RedisPassword },
	},
	{
		"storage.clickhouse_serving_password_env", "storage.clickhouse_serving_password",
		func(d *deprecatedStorageKeys) string { return d.Storage.ClickHouseServingPasswordEnv },
		func(c *Config) *string { return &c.Storage.ClickHouseServingPassword },
	},
}

func isDeprecatedKey(path string) bool {
	for _, k := range deprecatedKeys {
		if k.from == path {
			return true
		}
	}
	return false
}

// applyDeprecatedKeys copies each deprecated key present in doc onto its
// replacement field and returns the renames it applied. Setting both an
// old key and its replacement is an error: neither silently wins.
func applyDeprecatedKeys(doc string, meta toml.MetaData, c *Config) ([]string, error) {
	var old deprecatedStorageKeys
	if _, err := toml.Decode(doc, &old); err != nil {
		return nil, fmt.Errorf("decode deprecated keys: %w", err)
	}
	var applied []string
	for _, k := range deprecatedKeys {
		if !meta.IsDefined(strings.Split(k.from, ".")...) {
			continue
		}
		if meta.IsDefined(strings.Split(k.to, ".")...) {
			return nil, fmt.Errorf("%w: both %s and its deprecated alias %s are set; remove %s",
				ErrInvalidConfig, k.to, k.from, k.from)
		}
		*k.dst(c) = k.val(&old)
		applied = append(applied, k.from+" -> "+k.to)
	}
	return applied, nil
}

// retiredKeyMatch reports whether path is covered by RetiredKeys, either
// directly or as a child of a retired table (toml's Undecoded() lists both
// the table and its leaves when a whole table was removed from the schema,
// e.g. "external.retired_source" and "external.retired_source.api_key" for
// one deleted [external.retired_source] block).
func retiredKeyMatch(path string) (string, bool) {
	if note, ok := RetiredKeys[path]; ok {
		return note, true
	}
	for retiredPath, note := range RetiredKeys {
		if strings.HasPrefix(path, retiredPath+".") {
			return note, true
		}
	}
	return "", false
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
		overridden = append(overridden, "storage.redis_password")
	}
	if v := os.Getenv("STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD"); v != "" {
		c.Storage.ClickHouseServingPassword = v
		overridden = append(overridden, "storage.clickhouse_serving_password")
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
	if v := os.Getenv("TIINGO_API_KEY"); v != "" {
		c.External.Tiingo.APIKey = v
		overridden = append(overridden, "external.tiingo.api_key")
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
		// One Pro key feeds the aggregator poller and the divergence
		// supply cross-check's CoinGecko reference (internal/divergence/supply.go).
		c.External.CoinGecko.APIKey = v
		c.Divergence.Supply.CoinGecko.APIKey = v
		overridden = append(overridden, "external.coingecko.api_key", "divergence.supply.coingecko.api_key")
	}
	if v := os.Getenv("COINGECKO_DEMO_API_KEY"); v != "" {
		c.External.CoinGecko.DemoAPIKey = v
		overridden = append(overridden, "external.coingecko.demo_api_key")
	}
	if v := os.Getenv("MASSIVE_API_KEY"); v != "" {
		c.External.Massive.APIKey = v
		overridden = append(overridden, "external.massive.api_key")
	}
	if v := os.Getenv("DUNE_API_KEY"); v != "" {
		c.External.Dune.APIKey = v
		overridden = append(overridden, "external.dune.api_key")
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
