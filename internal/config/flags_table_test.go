package config_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	cfg "github.com/Stellar-Index/StellarIndex/internal/config"
)

const flagsTableBase = `
[region]
id = "r1"

[stellar]
network = "pubnet"

[storage]
postgres_dsn = "postgres://u:p@h/db"
`

// Every variable ApplyEnvOverrides reads; each override row clears them first so a developer's
// shell cannot leak into a row.
var overrideVars = []string{
	"STELLARINDEX_POSTGRES_DSN", "STELLARINDEX_REDIS_PASSWORD",
	"STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD", "EXCHANGERATESAPI_KEY",
	"COINMARKETCAP_API_KEY", "CRYPTOCOMPARE_API_KEY",
	"COINGECKO_API_KEY", "COINGECKO_DEMO_API_KEY", "MASSIVE_API_KEY",
	"DUNE_API_KEY", "CHAINLINK_RPC_URL", "OPENEXCHANGERATES_APP_ID", "TIINGO_API_KEY",
}

func describeField(t *testing.T, path string) cfg.SchemaField {
	t.Helper()
	for _, f := range cfg.Describe() {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("%s not found in Describe() output", path)
	return cfg.SchemaField{}
}

func TestDefault_Fields(t *testing.T) {
	cases := []struct {
		name string
		got  func(t *testing.T) any
		want any
	}{
		{"openexchangerates disabled with no app id or endpoint", func(*testing.T) any {
			o := cfg.Default().External.OpenExchangeRates
			return []any{o.Enabled, o.AppID, o.Endpoint}
		}, []any{false, "", ""}},
		{"tiingo disabled with no key", func(*testing.T) any {
			d := cfg.Default().External.Tiingo
			return []any{d.Enabled, d.APIKey}
		}, []any{false, ""}},
		// production keeps /v1/status byte-identical for every deployment that does not say otherwise
		{"region deployment", func(*testing.T) any { return cfg.Default().Region.Deployment }, "production"},
		{"region id set", func(*testing.T) any { return cfg.Default().Region.ID != "" }, true},
		{"stellar network set", func(*testing.T) any { return cfg.Default().Stellar.Network != "" }, true},
		{"rpc endpoints set", func(*testing.T) any { return len(cfg.Default().Stellar.RPCEndpoints) > 0 }, true},
		{"aggregate min usd volume positive", func(*testing.T) any { return cfg.Default().Aggregate.MinUSDVolume > 0 }, true},
		// default:"" documents an intentional empty default; neither it nor no tag makes a field Required
		{"explicit empty default is not required", func(t *testing.T) any {
			f := describeField(t, "external.exchangeratesapi.api_key")
			return []any{f.Required, f.Default}
		}, []any{false, ""}},
		{"coingecko api key env name", func(t *testing.T) any {
			return describeField(t, "external.coingecko.api_key").Env
		}, "COINGECKO_API_KEY"},
		{"coingecko demo api key env name", func(t *testing.T) any {
			return describeField(t, "external.coingecko.demo_api_key").Env
		}, "COINGECKO_DEMO_API_KEY"},
		{"massive api key env name", func(t *testing.T) any {
			return describeField(t, "external.massive.api_key").Env
		}, "MASSIVE_API_KEY"},
		{"openexchangerates app id env name", func(t *testing.T) any {
			return describeField(t, "external.openexchangerates.app_id").Env
		}, "OPENEXCHANGERATES_APP_ID"},
		{"dune api key env name", func(t *testing.T) any {
			return describeField(t, "external.dune.api_key").Env
		}, "DUNE_API_KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.got(t); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoadReader_Keys(t *testing.T) {
	cases := []struct {
		name    string
		toml    string
		got     func(c cfg.Config) any
		want    any
		wantErr string // substring the load error must carry; empty means the load must succeed
	}{
		{name: "aggregate flags round-trip", toml: flagsTableBase + `
[aggregate]
disable_class_filter         = true
enable_stablecoin_fiat_proxy = true
interval_seconds             = 15
max_trades_per_window        = 500
`, got: func(c cfg.Config) any {
			a := c.Aggregate
			return []any{a.DisableClassFilter, a.EnableStablecoinFiatProxy, a.IntervalSeconds, a.MaxTradesPerWindow}
		}, want: []any{true, true, 15, 500}},
		{name: "openexchangerates from toml", toml: flagsTableBase + `
[external.openexchangerates]
enabled = true
app_id = "toml-id"
endpoint = "http://127.0.0.1:9/api"
`, got: func(c cfg.Config) any {
			o := c.External.OpenExchangeRates
			return []any{o.Enabled, o.AppID, o.Endpoint}
		}, want: []any{true, "toml-id", "http://127.0.0.1:9/api"}},
		{name: "region deployment", toml: `
[region]
id = "testnet"
deployment = "testnet"
`, got: func(c cfg.Config) any { return c.Region.Deployment }, want: "testnet"},
		// a silent typo is a classic deployment bug: an unknown key is a hard error that names the key
		{name: "unknown key refused", toml: `
[region]
id = "r1"
nonsense_field = "oops"
`, wantErr: "nonsense_field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := cfg.LoadReader(strings.NewReader(tc.toml), "test.toml")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("LoadReader error = %v, want one naming %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadReader: %v", err)
			}
			if got := tc.got(c); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApplyEnvOverrides_Rows(t *testing.T) {
	cases := []struct {
		name  string
		vars  map[string]string
		prep  func(c *cfg.Config)
		got   func(c *cfg.Config) string
		want  string
		paths []string // overridden field paths the call must report
		exact bool     // paths is the whole list, in order, not a subset
	}{
		{
			name: "postgres dsn", vars: map[string]string{"STELLARINDEX_POSTGRES_DSN": "postgres://from-env/db"},
			got: func(c *cfg.Config) string { return c.Storage.PostgresDSN }, want: "postgres://from-env/db",
			paths: []string{"storage.postgres_dsn"}, exact: true,
		},
		{
			name: "nothing set leaves the default and reports no path",
			got:  func(c *cfg.Config) string { return c.Storage.PostgresDSN }, want: cfg.Default().Storage.PostgresDSN, exact: true,
		},
		{
			name: "openexchangerates app id beats the toml value", vars: map[string]string{"OPENEXCHANGERATES_APP_ID": "env-id"},
			prep: func(c *cfg.Config) { c.External.OpenExchangeRates.AppID = "toml-id" },
			got:  func(c *cfg.Config) string { return c.External.OpenExchangeRates.AppID }, want: "env-id",
			paths: []string{"external.openexchangerates.app_id"},
		},
		{
			name: "tiingo key beats the toml value", vars: map[string]string{"TIINGO_API_KEY": "tiingo-from-env"},
			prep: func(c *cfg.Config) { c.External.Tiingo.APIKey = "tiingo-from-toml" },
			got:  func(c *cfg.Config) string { return c.External.Tiingo.APIKey }, want: "tiingo-from-env",
			paths: []string{"external.tiingo.api_key"},
		},
		// LoadWithEnv logs these paths so an operator sees WHICH fields the environment won; unset vars never appear
		{
			name:  "reports exactly the replaced paths",
			vars:  map[string]string{"STELLARINDEX_POSTGRES_DSN": "postgres://from-env/db", "STELLARINDEX_REDIS_PASSWORD": "s3cr3t"},
			paths: []string{"storage.postgres_dsn", "storage.redis_password"}, exact: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, n := range overrideVars {
				t.Setenv(n, "")
			}
			for k, v := range tc.vars {
				t.Setenv(k, v)
			}
			c := cfg.Default()
			if tc.prep != nil {
				tc.prep(&c)
			}
			paths := c.ApplyEnvOverrides()
			if tc.got != nil {
				if got := tc.got(&c); got != tc.want {
					t.Errorf("field = %q, want %q", got, tc.want)
				}
			}
			if tc.exact && !slices.Equal(paths, tc.paths) {
				t.Errorf("ApplyEnvOverrides() = %v, want exactly %v", paths, tc.paths)
			}
			for _, p := range tc.paths {
				if !slices.Contains(paths, p) {
					t.Errorf("ApplyEnvOverrides() = %v, want %s listed", paths, p)
				}
			}
		})
	}
}
