package config_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

func TestValidate_DefaultPasses(t *testing.T) {
	// Default() MUST pass Validate — that's the "fresh install
	// works" contract every binary depends on.
	if err := config.Default().Validate(); err != nil {
		t.Fatalf("Default().Validate: %v", err)
	}
}

// TestDefault_BackgroundStatementTimeoutIsGenerousBackstop pins REC-08
// (audit-2026-08-14): the indexer/aggregator pools must ship with a
// non-zero, GENEROUS SQL-side statement_timeout backstop out of the box.
// Zero would leave those pools unbounded (the defect); a tight value would
// clip legitimate heavy work (the rejected global-timeout fix). It must
// also comfortably exceed the request-path serving bound — a background
// runaway is expected to run far longer than any serving query before it is
// unambiguously stuck.
func TestDefault_BackgroundStatementTimeoutIsGenerousBackstop(t *testing.T) {
	got := config.Default().Storage.BackgroundStatementTimeout
	if got != 30*time.Minute {
		t.Fatalf("Storage.BackgroundStatementTimeout = %v, want 30m (the generous REC-08 backstop)", got)
	}
	if serving := config.Default().API.ServingStatementTimeout; got <= serving {
		t.Fatalf("Storage.BackgroundStatementTimeout (%v) must exceed the serving bound (%v)", got, serving)
	}
}

// withBad returns Default() with a mutator applied. Helper so each
// test case is one line.
func withBad(mut func(*config.Config)) config.Config {
	c := config.Default()
	mut(&c)
	return c
}

func TestValidate_RejectsBadFields(t *testing.T) {
	cases := map[string]struct {
		mut    func(*config.Config)
		errSub string
	}{
		"empty region id":              {func(c *config.Config) { c.Region.ID = "" }, "region.id"},
		"capitalized region":           {func(c *config.Config) { c.Region.ID = "R1" }, "region.id"},
		"home domain is URL":           {func(c *config.Config) { c.Region.HomeDomain = "https://stellarindex.io" }, "home_domain"},
		"unknown network":              {func(c *config.Config) { c.Stellar.Network = "futurenett" }, "network"},
		"empty rpc list":               {func(c *config.Config) { c.Stellar.RPCEndpoints = nil }, "rpc_endpoints"},
		"rpc not url":                  {func(c *config.Config) { c.Stellar.RPCEndpoints = []string{"host:8000"} }, "rpc_endpoints"},
		"duplicate rpc":                {func(c *config.Config) { c.Stellar.RPCEndpoints = []string{"http://rpc1:8000", "http://rpc1:8000"} }, "duplicate"},
		"duplicate rpc case":           {func(c *config.Config) { c.Stellar.RPCEndpoints = []string{"http://Rpc1:8000", "HTTP://rpc1:8000"} }, "duplicate"},
		"duplicate rpc trailing slash": {func(c *config.Config) { c.Stellar.RPCEndpoints = []string{"http://rpc1:8000", "http://rpc1:8000/"} }, "duplicate"},
		"missing postgres":             {func(c *config.Config) { c.Storage.PostgresDSN = "" }, "postgres_dsn"},
		"wrong postgres scheme":        {func(c *config.Config) { c.Storage.PostgresDSN = "mysql://x" }, "postgres_dsn"},
		"bad redis addr":               {func(c *config.Config) { c.Storage.RedisAddr = "127.0.0.1" }, "redis_addr"},
		"bad cursor store":             {func(c *config.Config) { c.Ingestion.CursorStoreScheme = "kafka" }, "cursor_store_scheme"},
		"zero batch":                   {func(c *config.Config) { c.Ingestion.BackfillBatchSize = 0 }, "backfill_batch_size"},
		"duplicate source":             {func(c *config.Config) { c.Ingestion.EnabledSources = []string{"soroswap", "soroswap"} }, "duplicate"},
		"duplicate case-fold":          {func(c *config.Config) { c.Ingestion.EnabledSources = []string{"soroswap", "Soroswap"} }, "duplicate"},
		"empty source entry":           {func(c *config.Config) { c.Ingestion.EnabledSources = []string{"soroswap", ""} }, "empty entry"},
		"bad reflector addr":           {func(c *config.Config) { c.Oracle.Reflector.DEXContract = "not-a-c-key" }, "dex_contract"},
		"zero vwap window":             {func(c *config.Config) { c.Aggregate.VWAPWindowSeconds = 0 }, "vwap_window_seconds"},
		"negative sigma":               {func(c *config.Config) { c.Aggregate.OutlierSigmaThreshold = -1 }, "outlier_sigma_threshold"},
		"no listen":                    {func(c *config.Config) { c.API.ListenAddr = "" }, "listen_addr"},
		"bad listen":                   {func(c *config.Config) { c.API.ListenAddr = "3000" }, "listen_addr"},
		"unknown auth":                 {func(c *config.Config) { c.API.AuthMode = "oauth" }, "auth_mode"},
		"neg rate limit":               {func(c *config.Config) { c.API.AnonRateLimitPerMin = -5 }, "anon_rate_limit"},
		"bad log level":                {func(c *config.Config) { c.Obs.LogLevel = "verbose" }, "log_level"},
		"bad log format":               {func(c *config.Config) { c.Obs.LogFormat = "xml" }, "log_format"},
		"bad trace exporter":           {func(c *config.Config) { c.Obs.TraceExporter = "jaeger" }, "trace_exporter"},
		"otlp not yet wired":           {func(c *config.Config) { c.Obs.TraceExporter = "otlp" }, "trace_exporter"},
		"trace sample over 1":          {func(c *config.Config) { c.Obs.TraceSample = 1.5 }, "trace_sample"},
		"trace sample neg":             {func(c *config.Config) { c.Obs.TraceSample = -0.1 }, "trace_sample"},
		"core http not url":            {func(c *config.Config) { c.Stellar.CoreHTTPEndpoint = "host:11626" }, "core_http_endpoint"},
		"s3 endpoint not url":          {func(c *config.Config) { c.Storage.S3Endpoint = "minio-host" }, "s3_endpoint"},
		"s3 bucket archive missing":    {func(c *config.Config) { c.Storage.S3BucketArchive = "" }, "s3_bucket_archive"},
		"s3 bucket live missing":       {func(c *config.Config) { c.Storage.S3BucketLive = "" }, "s3_bucket_live"},
		"s3 access key env missing":    {func(c *config.Config) { c.Storage.S3AccessKeyEnv = "" }, "s3_access_key_env"},
		"s3 secret key env missing":    {func(c *config.Config) { c.Storage.S3SecretKeyEnv = "" }, "s3_secret_key_env"},
		"s3 bucket uppercase":          {func(c *config.Config) { c.Storage.S3BucketArchive = "MyBucket" }, "s3_bucket_archive"},
		"s3 bucket too short":          {func(c *config.Config) { c.Storage.S3BucketArchive = "ab" }, "s3_bucket_archive"},
		"s3 bucket underscore":         {func(c *config.Config) { c.Storage.S3BucketArchive = "my_bucket" }, "s3_bucket_archive"},
		"usd peg empty":                {func(c *config.Config) { c.Trades.USDPeggedClassicAssets = []string{""} }, "usd_pegged_classic_assets"},
		"usd peg unparseable":          {func(c *config.Config) { c.Trades.USDPeggedClassicAssets = []string{"not an asset"} }, "usd_pegged_classic_assets"},
		"usd peg native not classic":   {func(c *config.Config) { c.Trades.USDPeggedClassicAssets = []string{"native"} }, "classic"},
		"usd peg crypto not classic":   {func(c *config.Config) { c.Trades.USDPeggedClassicAssets = []string{"crypto:USDT"} }, "classic"},
		"usd peg fiat not classic":     {func(c *config.Config) { c.Trades.USDPeggedClassicAssets = []string{"fiat:USD"} }, "classic"},
		"fiat peg unknown ticker": {func(c *config.Config) {
			c.PricingGuard.FiatPeggedClassicAssets = map[string]string{
				"AUDD-GDC7X2MXTYSAKUUGAIQ7J7RPEIM7GXSAIWFYWWH4GLNFECQVJJLB2EEU": "AUX",
			}
		}, "fiat_pegged_classic_assets"},
		"fiat peg unparseable key": {func(c *config.Config) {
			c.PricingGuard.FiatPeggedClassicAssets = map[string]string{"not an asset": "AUD"}
		}, "fiat_pegged_classic_assets"},
		"fiat peg native not classic": {func(c *config.Config) {
			c.PricingGuard.FiatPeggedClassicAssets = map[string]string{"native": "AUD"}
		}, "classic"},

		// CFG-05 (audit-2026-07-23): history_archive_url must be a
		// full URL like its sibling fields, not just anything
		// url.Parse tolerates (scheme-less, empty).
		"history archive url empty": {func(c *config.Config) { c.Stellar.HistoryArchiveURL = "" }, "history_archive_url"},
		"history archive url scheme-less": {func(c *config.Config) {
			c.Stellar.HistoryArchiveURL = "history.stellar.org/prd/core-live/core_live_001"
		}, "history_archive_url"},

		// CFG-05 (audit-2026-07-23): serving_statement_timeout must
		// stay LONGER than request_timeout so the app-layer deadline
		// fires first (defense-in-depth ordering).
		"statement timeout equal request timeout": {
			func(c *config.Config) { c.API.ServingStatementTimeout = c.API.RequestTimeout },
			"serving_statement_timeout",
		},
		"statement timeout shorter than request timeout": {
			func(c *config.Config) { c.API.ServingStatementTimeout = c.API.RequestTimeout / 2 },
			"serving_statement_timeout",
		},

		// CFG-03 (audit-2026-07-23): the two conflicting `_env`
		// conventions (value-vs-name) getting swapped.
		"redis password looks like env var name": {
			func(c *config.Config) { c.Storage.RedisPassword = "STELLARINDEX_REDIS_PASSWORD" },
			"redis_password_env",
		},
		"clickhouse password looks like env var name": {
			func(c *config.Config) {
				c.Storage.ClickHouseServingPassword = "STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD"
			},
			"clickhouse_serving_password_env",
		},
		"s3 access key env holds a literal secret value": {
			// A realistic AWS SECRET key shape (lowercase + digits + '/')
			// — definitively not an UPPER_SNAKE_CASE env-var name, unlike
			// an access-key-ID-shaped string which happens to also look
			// like a valid identifier.
			func(c *config.Config) { c.Storage.S3AccessKeyEnv = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYzEXAMPLE" },
			"s3_access_key_env",
		},
		"s3 secret key env holds a lowercase value": {
			func(c *config.Config) { c.Storage.S3SecretKeyEnv = "not-an-env-var-name" },
			"s3_secret_key_env",
		},

		// CFG-05 (audit-2026-07-23): anomaly.thresholds / classifications
		// keys/values must be real anomaly.AssetClass names.
		"anomaly thresholds unknown class": {
			func(c *config.Config) {
				c.Anomaly.Thresholds = map[string]config.AnomalyThreshold{"stablecoins": {WarnPct: 1, FreezePct: 3}}
			},
			"anomaly.thresholds",
		},
		"anomaly classifications unknown class": {
			func(c *config.Config) {
				c.Anomaly.Classifications = map[string]string{"USDC-GA5Z": "stable"}
			},
			"anomaly.classifications",
		},

		// ADR-0019 §"Freeze duration" (N-F6): the auto-unfreeze band
		// must not overlap the fire band on the z axis, or a signal
		// hovering at the trigger flaps the pair frozen/unfrozen every
		// bucket — republishing, each time it unfreezes, the value the
		// freeze just refused. That IS the pre-lifecycle behaviour, so
		// it must not be reachable by config.
		"anomaly phase2 unfreeze z band overlaps the fire band": {
			func(c *config.Config) {
				c.Anomaly.Phase2.ZScoreMinFreeze = 5.0
				c.Anomaly.Phase2.UnfreezeZScoreMax = 6.0
			},
			"anomaly.phase2.unfreeze_z_score_max",
		},
		"anomaly phase2 negative initial hold": {
			func(c *config.Config) { c.Anomaly.Phase2.InitialHoldMinutes = -1 },
			"anomaly.phase2.initial_hold_minutes",
		},
		"anomaly phase2 negative max extensions": {
			func(c *config.Config) { c.Anomaly.Phase2.MaxExtensions = -2 },
			"anomaly.phase2.max_extensions",
		},
		"anomaly phase2 unfreeze confidence out of range": {
			func(c *config.Config) { c.Anomaly.Phase2.UnfreezeConfidenceMin = 1.5 },
			"anomaly.phase2.unfreeze_confidence_min",
		},

		// CFG-05 (audit-2026-07-23): divergence.supply.refresh_interval_seconds<=0
		// while enabled used to reach time.NewTicker(0) and panic the
		// aggregator at startup.
		"divergence supply zero refresh interval while enabled": {
			func(c *config.Config) {
				c.Divergence.Supply.Enabled = true
				c.Divergence.Supply.RefreshIntervalSeconds = 0
			},
			"divergence.supply.refresh_interval_seconds",
		},
		"divergence supply negative refresh interval while enabled": {
			func(c *config.Config) {
				c.Divergence.Supply.Enabled = true
				c.Divergence.Supply.RefreshIntervalSeconds = -1
			},
			"divergence.supply.refresh_interval_seconds",
		},

		// ADR-0027 cold tier (2026-07-25 incident): the *_key_env pair
		// is all-or-nothing, because EMPTY is a meaningful value here
		// (it selects anonymous reads on the public
		// aws-public-blockchain bucket). Half a pair would force
		// pipeline.NewColdDataStore to guess, and guessing "anonymous"
		// against a private bucket is a silent downgrade.
		"cold access key env without secret": {
			func(c *config.Config) { c.Storage.S3ColdAccessKeyEnv = "STELLARINDEX_S3_COLD_ACCESS_KEY" },
			"s3_cold_access_key_env",
		},
		"cold secret key env without access": {
			func(c *config.Config) { c.Storage.S3ColdSecretKeyEnv = "STELLARINDEX_S3_COLD_SECRET_KEY" },
			"s3_cold_secret_key_env",
		},
		"cold access key env holds a literal secret value": {
			func(c *config.Config) {
				c.Storage.S3ColdAccessKeyEnv = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYzEXAMPLE"
				c.Storage.S3ColdSecretKeyEnv = "STELLARINDEX_S3_COLD_SECRET_KEY"
			},
			"s3_cold_access_key_env",
		},
		"cold secret key env holds a lowercase value": {
			func(c *config.Config) {
				c.Storage.S3ColdAccessKeyEnv = "STELLARINDEX_S3_COLD_ACCESS_KEY"
				c.Storage.S3ColdSecretKeyEnv = "not-an-env-var-name"
			},
			"s3_cold_secret_key_env",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := withBad(tc.mut).Validate()
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !errors.Is(err, config.ErrInvalidConfig) {
				t.Errorf("err not wrapped as ErrInvalidConfig: %v", err)
			}
			if !strings.Contains(err.Error(), tc.errSub) {
				t.Errorf("err = %v; want substring %q", err, tc.errSub)
			}
		})
	}
}

func TestValidate_USDPeggedClassicAssetsAccepted(t *testing.T) {
	// A well-formed classic credit asset (CODE-ISSUER, 7-decimal by
	// protocol) is the only accepted shape for a declared USD peg.
	c := withBad(func(c *config.Config) {
		c.Trades.USDPeggedClassicAssets = []string{
			"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
		}
	})
	if err := c.Validate(); err != nil {
		t.Fatalf("valid classic USD peg rejected: %v", err)
	}
}

func TestValidate_FiatPeggedClassicAssetsAccepted(t *testing.T) {
	// A classic credit asset mapped to a known ISO-4217 fiat ticker is
	// the accepted shape for a declared fiat peg (the AUDD → AUD entry
	// operator-approved 2026-08-24).
	c := withBad(func(c *config.Config) {
		c.PricingGuard.FiatPeggedClassicAssets = map[string]string{
			"AUDD-GDC7X2MXTYSAKUUGAIQ7J7RPEIM7GXSAIWFYWWH4GLNFECQVJJLB2EEU": "AUD",
			"AUDR-GAAVW6EQ4N4SHNTKBLTOBXKS6CEIMT2KZI7YQ5B37ECNVPFLBIGRKLIL": "AUD",
		}
	})
	if err := c.Validate(); err != nil {
		t.Fatalf("valid classic fiat peg rejected: %v", err)
	}
}

func TestValidate_OracleContractsOptional(t *testing.T) {
	// Every Reflector variant empty is fine — operator may run the
	// API without any oracle contracts configured.
	c := config.Default()
	c.Oracle.Reflector.DEXContract = ""
	c.Oracle.Reflector.CEXContract = ""
	c.Oracle.Reflector.FXContract = ""
	if err := c.Validate(); err != nil {
		t.Fatalf("empty-oracle config should validate: %v", err)
	}
}

func TestValidate_ValidReflectorAddressPasses(t *testing.T) {
	c := config.Default()
	// Known-format-valid C-strkey (not a real mainnet address —
	// validation is format-only per canonical/strkey.go).
	c.Oracle.Reflector.DEXContract = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	if err := c.Validate(); err != nil {
		t.Fatalf("valid C-strkey should pass: %v", err)
	}
}

func TestValidate_S3BlockOptional(t *testing.T) {
	// Operator running local dev without any object store: all S3
	// fields empty. Validate must accept this (Default() sets them
	// but clearing the whole block should be valid).
	c := config.Default()
	c.Storage.S3Endpoint = ""
	c.Storage.S3BucketArchive = ""
	c.Storage.S3BucketLive = ""
	c.Storage.S3AccessKeyEnv = ""
	c.Storage.S3SecretKeyEnv = ""
	c.Storage.S3Region = ""
	if err := c.Validate(); err != nil {
		t.Fatalf("empty S3 block should validate: %v", err)
	}
}

// TestValidate_ColdTierCredentialPairs pins both accepted shapes of the
// ADR-0027 cold-tier credential pair, so the all-or-nothing rule above
// can't be satisfied by simply rejecting everything.
func TestValidate_ColdTierCredentialPairs(t *testing.T) {
	// Production shape: cold tier enabled, both *_key_env empty =>
	// anonymous reads of the public aws-public-blockchain bucket.
	anon := config.Default()
	anon.Storage.S3ColdEndpoint = "https://s3.us-east-2.amazonaws.com"
	anon.Storage.S3ColdRegion = "us-east-2"
	anon.Storage.S3ColdBucketArchive = "aws-public-blockchain/v1.1/stellar/ledgers/pubnet"
	if err := anon.Validate(); err != nil {
		t.Fatalf("anonymous cold tier (both *_key_env empty) rejected: %v", err)
	}

	// Private-bucket shape: both names set, UPPER_SNAKE_CASE.
	static := anon
	static.Storage.S3ColdAccessKeyEnv = "STELLARINDEX_S3_COLD_ACCESS_KEY"
	static.Storage.S3ColdSecretKeyEnv = "STELLARINDEX_S3_COLD_SECRET_KEY"
	if err := static.Validate(); err != nil {
		t.Fatalf("static-credential cold tier (both *_key_env named) rejected: %v", err)
	}
}

func TestValidate_RejectsUnknownSource(t *testing.T) {
	// A typo in enabled_sources must be caught at Validate time so
	// dry-run doesn't waste the storage-open + RPC-probe budget before
	// reporting it.
	c := config.Default()
	c.Ingestion.EnabledSources = []string{"soroswap", "sorowsap"}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected validation error for unknown source")
	}
	if !errors.Is(err, config.ErrInvalidConfig) {
		t.Errorf("err not wrapped as ErrInvalidConfig: %v", err)
	}
	if !strings.Contains(err.Error(), "unknown source") {
		t.Errorf("expected 'unknown source' in error: %v", err)
	}
}

func TestValidate_ReflectorSourceRequiresContract(t *testing.T) {
	// enabled_sources lists reflector-dex but dex_contract is empty →
	// must fail Validate, not defer to indexer startup.
	cases := map[string]struct {
		source string
		clear  func(*config.Config)
	}{
		"reflector-dex": {"reflector-dex", func(c *config.Config) { c.Oracle.Reflector.DEXContract = "" }},
		"reflector-cex": {"reflector-cex", func(c *config.Config) { c.Oracle.Reflector.CEXContract = "" }},
		"reflector-fx":  {"reflector-fx", func(c *config.Config) { c.Oracle.Reflector.FXContract = "" }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := config.Default()
			c.Ingestion.EnabledSources = []string{tc.source}
			// Set ALL reflector contracts first so only the one we
			// clear below is empty; avoids false-positive matches.
			c.Oracle.Reflector.DEXContract = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
			c.Oracle.Reflector.CEXContract = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
			c.Oracle.Reflector.FXContract = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
			tc.clear(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("expected error when %s enabled but contract empty", tc.source)
			}
			if !strings.Contains(err.Error(), tc.source) {
				t.Errorf("error should name the source %q: %v", tc.source, err)
			}
		})
	}
}

// TestValidate_ClickHouseProjectorSourceRequiresLiveSink locks the
// ADR-0034 #10 feed-switch dependency (C3-20): the projector reading
// forward events FROM ClickHouse only makes sense if the dual-sink is
// WRITING them. The invariant is documented on the field ("Requires
// clickhouse_live_sink") but was never enforced — a misconfig
// (projector_source=true, live_sink=false) would silently mis-read.
func TestValidate_ClickHouseProjectorSourceRequiresLiveSink(t *testing.T) {
	// The bad combo: read from CH but never write to it.
	bad := withBad(func(c *config.Config) {
		c.Storage.ClickHouseProjectorSource = true
		c.Storage.ClickHouseLiveSink = false
	})
	err := bad.Validate()
	if err == nil {
		t.Fatal("expected rejection of projector_source=true with live_sink=false, got nil")
	}
	if !errors.Is(err, config.ErrInvalidConfig) {
		t.Errorf("err not wrapped as ErrInvalidConfig: %v", err)
	}
	if !strings.Contains(err.Error(), "clickhouse_projector_source") ||
		!strings.Contains(err.Error(), "clickhouse_live_sink") {
		t.Errorf("err should name both fields; got %v", err)
	}

	// Every valid combination must pass.
	valid := []struct {
		name            string
		projectorSource bool
		liveSink        bool
	}{
		{"both on (production / Default)", true, true},
		{"sink on, projector off", false, true},
		{"both off", false, false},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			c := withBad(func(c *config.Config) {
				c.Storage.ClickHouseProjectorSource = tc.projectorSource
				c.Storage.ClickHouseLiveSink = tc.liveSink
			})
			if err := c.Validate(); err != nil {
				t.Fatalf("valid combo %s rejected: %v", tc.name, err)
			}
		})
	}
}

func TestValidate_CoreHTTPEndpointOptional(t *testing.T) {
	// Empty CoreHTTPEndpoint means "don't probe core" — valid.
	c := config.Default()
	c.Stellar.CoreHTTPEndpoint = ""
	if err := c.Validate(); err != nil {
		t.Fatalf("empty core_http_endpoint should validate: %v", err)
	}
}

// TestValidate_SDFReserveAccountObserverOnlyAccepted — DOM-11 / CFG-01
// (audit-2026-07-23). SupplyConfig.Validate used to unconditionally
// require a matching reserve_balances_stroops entry for every
// sdf_reserve_accounts entry, which made the documented "the LCM
// AccountEntry observer covers it, no static balance needed"
// deployment shape un-loadable — Validate() has no DB access and
// can't know whether the observer covers the account, so a blanket
// requirement was strictly wrong for that (fully supported) path.
// This asserts the corrected value: a syntactically valid G-strkey
// account with NO static balance entry passes config validation. The
// runtime rejection for a genuinely-uncovered account still happens
// downstream in ConfigReserveBalanceReader.ReserveBalanceTotal
// (internal/supply/config_reader.go) — see that type's doc.
func TestValidate_SDFReserveAccountObserverOnlyAccepted(t *testing.T) {
	c := config.Default()
	c.Supply.SDFReserveAccounts = []string{"GABBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"}
	c.Supply.ReserveBalancesStroops = nil
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v (observer-only SDF reserve account must be accepted without a static balance entry)", err)
	}
}

// TestValidate_SDFReserveAccountMalformedRejected — DOM-11
// (audit-2026-07-23): a typo'd sdf_reserve_accounts entry is a
// config mistake, not "this account happens to have zero reserves."
// Kept as its own test (not folded into TestValidate_RejectsBadFields)
// because SupplyConfig.Validate — unlike the lowercase validate()
// family — doesn't wrap ErrInvalidConfig; asserting that here would
// test a sentinel this pre-existing method never satisfies.
func TestValidate_SDFReserveAccountMalformedRejected(t *testing.T) {
	c := config.Default()
	c.Supply.SDFReserveAccounts = []string{"not-a-g-strkey"}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected an error for a malformed sdf_reserve_accounts entry, got nil")
	}
	if !strings.Contains(err.Error(), "sdf_reserve_accounts") {
		t.Errorf("err = %v; want substring %q", err, "sdf_reserve_accounts")
	}
}
