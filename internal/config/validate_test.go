package config_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

const (
	testReserveAccount = "GDUY7J7A33TQWOSOQGDO776GGLM3UQERL4J3SPT56F6YS4ID7MLDERI4"
	testReflectorC     = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	testClassicUSDC    = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

// withBad returns Default() with a mutator applied.
func withBad(mut func(*config.Config)) config.Config {
	c := config.Default()
	mut(&c)
	return c
}

// reflectorSourceWithout enables source with every reflector contract set,
// then applies unset, so only the cleared contract is empty.
func reflectorSourceWithout(source string, unset func(*config.Config)) func(*config.Config) {
	return func(c *config.Config) {
		c.Ingestion.EnabledSources = []string{source}
		c.Oracle.Reflector.DEXContract = testReflectorC
		c.Oracle.Reflector.CEXContract = testReflectorC
		c.Oracle.Reflector.FXContract = testReflectorC
		unset(c)
	}
}

// coldTier points the cold tier at the public AWS bucket.
func coldTier(c *config.Config) {
	c.Storage.S3ColdEndpoint = "https://s3.us-east-2.amazonaws.com"
	c.Storage.S3ColdRegion = "us-east-2"
	c.Storage.S3ColdBucketArchive = "aws-public-blockchain/v1.1/stellar/ledgers/pubnet"
}

// TestValidate_Accepts is the other half of the rejection tables: each row
// is a shape Validate must let through, so a guard that refused everything
// cannot satisfy the rejections alone.
func TestValidate_Accepts(t *testing.T) {
	cases := map[string]func(*config.Config){
		// Default() MUST pass: every binary depends on a fresh install working.
		"defaults": func(*config.Config) {},

		"testnet with its own archive": func(c *config.Config) {
			c.Stellar.Network = "testnet"
			c.Stellar.HistoryArchiveURL = "https://history.stellar.org/prd/core-testnet/core_testnet_001"
		},
		"futurenet with its own archive": func(c *config.Config) {
			c.Stellar.Network = "futurenet"
			c.Stellar.HistoryArchiveURL = "http://history.stellar.org/dev/core-futurenet/core_futurenet_001"
		},

		// request_timeout must outlive the longest handler budget: the
		// shipped default, one tick above the bound, and 0 (middleware off).
		"request timeout default":            func(c *config.Config) { c.API.RequestTimeout = config.Default().API.RequestTimeout },
		"request timeout one tick above max": func(c *config.Config) { c.API.RequestTimeout = config.APIMaxHandlerBudget + time.Nanosecond },
		"request timeout disabled":           func(c *config.Config) { c.API.RequestTimeout = 0 },

		"classic USD peg": func(c *config.Config) { c.Trades.USDPeggedClassicAssets = []string{testClassicUSDC} },
		"classic fiat pegs": func(c *config.Config) {
			c.PricingGuard.FiatPeggedClassicAssets = map[string]string{
				"AUDD-GDC7X2MXTYSAKUUGAIQ7J7RPEIM7GXSAIWFYWWH4GLNFECQVJJLB2EEU": "AUD",
				"AUDR-GAAVW6EQ4N4SHNTKBLTOBXKS6CEIMT2KZI7YQ5B37ECNVPFLBIGRKLIL": "AUD",
			}
		},

		"empty oracle contracts": func(c *config.Config) {
			c.Oracle.Reflector.DEXContract = ""
			c.Oracle.Reflector.CEXContract = ""
			c.Oracle.Reflector.FXContract = ""
		},
		// Format-only validation: not a real mainnet address.
		"valid reflector C-strkey": func(c *config.Config) { c.Oracle.Reflector.DEXContract = testReflectorC },

		"empty S3 block": func(c *config.Config) {
			c.Storage.S3Endpoint = ""
			c.Storage.S3BucketArchive = ""
			c.Storage.S3BucketLive = ""
			c.Storage.S3AccessKeyEnv = ""
			c.Storage.S3SecretKeyEnv = ""
			c.Storage.S3Region = ""
		},

		// Cold-tier credential pair: both empty selects anonymous reads of
		// the public bucket; both named is the private-bucket shape.
		"cold tier anonymous": coldTier,
		"cold tier static credentials": func(c *config.Config) {
			coldTier(c)
			c.Storage.S3ColdAccessKeyEnv = "STELLARINDEX_S3_COLD_ACCESS_KEY"
			c.Storage.S3ColdSecretKeyEnv = "STELLARINDEX_S3_COLD_SECRET_KEY"
		},

		"clickhouse projector on, sink on": func(c *config.Config) { c.Storage.ClickHouseProjectorSource, c.Storage.ClickHouseLiveSink = true, true },
		"clickhouse projector off, sink on": func(c *config.Config) {
			c.Storage.ClickHouseProjectorSource, c.Storage.ClickHouseLiveSink = false, true
		},
		"clickhouse both off": func(c *config.Config) {
			c.Storage.ClickHouseProjectorSource, c.Storage.ClickHouseLiveSink = false, false
		},

		// The runtime ConfigReserveBalanceReader rejects a genuinely
		// uncovered account; Validate has no DB access to know whether the
		// AccountEntry observer covers it.
		"observer-only SDF reserve account": func(c *config.Config) {
			c.Supply.SDFReserveAccounts = []string{testReserveAccount}
			c.Supply.ReserveBalancesStroops = nil
		},

		"max_market_cap_volume_ratio 0 is the off switch": func(c *config.Config) { c.Aggregate.MaxMarketCapVolumeRatio = 0 },

		"composite_reference boundaries and zero sentinels": func(c *config.Config) {
			cr := &c.Aggregate.CompositeReference
			cr.LegDispersionBps = 10_000
			cr.ReleaseBandPct = 100
			cr.ToleranceBps = 0
			cr.MinLegSources = 0
			cr.FXMaxAgeHours = 0
			c.Aggregate.Triangulations = []config.TriangulationChainConfig{
				{Target: "crypto:XLM/fiat:GBP", Legs: []string{"crypto:XLM/fiat:USD", "fiat:USD/fiat:GBP"}},
				{Target: "crypto:XLM/fiat:EUR", Legs: []string{"crypto:XLM/fiat:USD", "fiat:USD/fiat:EUR"}},
			}
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			if err := withBad(mut).Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

// TestValidate_CompositeReferenceDefaults pins the shipped defaults.
func TestValidate_CompositeReferenceDefaults(t *testing.T) {
	cr := config.Default().Aggregate.CompositeReference
	if !cr.Enabled || cr.ToleranceBps != 75 || cr.MinLegSources != 2 || cr.FXMaxAgeHours != 76 ||
		cr.ReleaseBandPct != 2.0 || cr.LegDispersionBps != 0 || len(cr.Targets) != 2 {
		t.Fatalf("shipped composite_reference defaults drifted: %+v", cr)
	}
}

// TestValidate_RPCEndpointErrorsOmitURL: a hosted stellar-rpc endpoint can
// embed an API key in its path, and the validation error is printed at
// boot, so neither the malformed nor the duplicate branch may echo the URL.
func TestValidate_RPCEndpointErrorsOmitURL(t *testing.T) {
	const pathMarker = "fixture-path-marker"
	for name, tc := range map[string]struct {
		endpoints []string
		wantIdx   string
	}{
		"malformed": {[]string{"rpc.example.test/v1/" + pathMarker}, "rpc_endpoints[0]"},
		"duplicate": {
			[]string{"https://rpc.example.test/v1/" + pathMarker, "https://rpc.example.test/v1/" + pathMarker + "/"},
			"rpc_endpoints[1] is a duplicate of stellar.rpc_endpoints[0]",
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := config.Default()
			c.Stellar.RPCEndpoints = tc.endpoints
			err := c.Validate()
			if !errors.Is(err, config.ErrInvalidConfig) {
				t.Fatalf("Validate() = %v, want ErrInvalidConfig", err)
			}
			if strings.Contains(err.Error(), pathMarker) {
				t.Errorf("error %q echoes the endpoint URL, which may carry an API key", err)
			}
			if !strings.Contains(err.Error(), tc.wantIdx) {
				t.Errorf("error %q should identify the entry as %q", err, tc.wantIdx)
			}
		})
	}
}

// TestDefault_BackgroundStatementTimeoutIsGenerousBackstop pins that the indexer/aggregator pools must ship with a
// non-zero, GENEROUS SQL-side statement_timeout backstop out of the box.
// Zero would leave those pools unbounded; a tight value would
// clip legitimate heavy work. It must
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

func TestValidate_RejectsBadFields(t *testing.T) {
	cases := map[string]struct {
		mut    func(*config.Config)
		errSub string
	}{
		"empty region id":               {func(c *config.Config) { c.Region.ID = "" }, "region.id"},
		"capitalized region":            {func(c *config.Config) { c.Region.ID = "R1" }, "region.id"},
		"unknown network":               {func(c *config.Config) { c.Stellar.Network = "futurenett" }, "network"},
		"testnet with pubnet archive":   {func(c *config.Config) { c.Stellar.Network = "testnet" }, "history_archive_url"},
		"futurenet with pubnet archive": {func(c *config.Config) { c.Stellar.Network = "futurenet" }, "history_archive_url"},
		"empty rpc list":                {func(c *config.Config) { c.Stellar.RPCEndpoints = nil }, "rpc_endpoints"},
		"rpc not url":                   {func(c *config.Config) { c.Stellar.RPCEndpoints = []string{"host:8000"} }, "rpc_endpoints"},
		"duplicate rpc":                 {func(c *config.Config) { c.Stellar.RPCEndpoints = []string{"http://rpc1:8000", "http://rpc1:8000"} }, "duplicate"},
		"duplicate rpc case":            {func(c *config.Config) { c.Stellar.RPCEndpoints = []string{"http://Rpc1:8000", "HTTP://rpc1:8000"} }, "duplicate"},
		"duplicate rpc trailing slash":  {func(c *config.Config) { c.Stellar.RPCEndpoints = []string{"http://rpc1:8000", "http://rpc1:8000/"} }, "duplicate"},
		"missing postgres":              {func(c *config.Config) { c.Storage.PostgresDSN = "" }, "postgres_dsn"},
		"wrong postgres scheme":         {func(c *config.Config) { c.Storage.PostgresDSN = "mysql://x" }, "postgres_dsn"},
		"bad redis addr":                {func(c *config.Config) { c.Storage.RedisAddr = "127.0.0.1" }, "redis_addr"},
		"duplicate source":              {func(c *config.Config) { c.Ingestion.EnabledSources = []string{"soroswap", "soroswap"} }, "duplicate"},
		"duplicate case-fold":           {func(c *config.Config) { c.Ingestion.EnabledSources = []string{"soroswap", "Soroswap"} }, "duplicate"},
		"empty source entry":            {func(c *config.Config) { c.Ingestion.EnabledSources = []string{"soroswap", ""} }, "empty entry"},
		"bad reflector addr":            {func(c *config.Config) { c.Oracle.Reflector.DEXContract = "not-a-c-key" }, "dex_contract"},
		"negative sigma":                {func(c *config.Config) { c.Aggregate.OutlierSigmaThreshold = -1 }, "outlier_sigma_threshold"},
		"no listen":                     {func(c *config.Config) { c.API.ListenAddr = "" }, "listen_addr"},
		"bad listen":                    {func(c *config.Config) { c.API.ListenAddr = "3000" }, "listen_addr"},
		"unknown auth":                  {func(c *config.Config) { c.API.AuthMode = "oauth" }, "auth_mode"},
		"unknown auth backend":          {func(c *config.Config) { c.API.AuthBackend = "mysql" }, "auth_backend"},
		"cors wildcard with credentials": {func(c *config.Config) {
			c.API.AllowedOrigins = []string{"*"}
			c.API.AllowCredentials = true
		}, "allow_credentials"},
		"credentialed origin not allowed": {func(c *config.Config) {
			c.API.AllowedOrigins = []string{"https://a.example"}
			c.API.CredentialedOrigins = []string{"https://b.example"}
		}, "credentialed_origins"},
		"neg rate limit":             {func(c *config.Config) { c.API.AnonRateLimitPerMin = -5 }, "anon_rate_limit"},
		"bad log level":              {func(c *config.Config) { c.Obs.LogLevel = "verbose" }, "log_level"},
		"bad log format":             {func(c *config.Config) { c.Obs.LogFormat = "xml" }, "log_format"},
		"s3 endpoint not url":        {func(c *config.Config) { c.Storage.S3Endpoint = "minio-host" }, "s3_endpoint"},
		"s3 bucket archive missing":  {func(c *config.Config) { c.Storage.S3BucketArchive = "" }, "s3_bucket_archive"},
		"s3 bucket live missing":     {func(c *config.Config) { c.Storage.S3BucketLive = "" }, "s3_bucket_live"},
		"s3 access key env missing":  {func(c *config.Config) { c.Storage.S3AccessKeyEnv = "" }, "s3_access_key_env"},
		"s3 secret key env missing":  {func(c *config.Config) { c.Storage.S3SecretKeyEnv = "" }, "s3_secret_key_env"},
		"s3 bucket uppercase":        {func(c *config.Config) { c.Storage.S3BucketArchive = "MyBucket" }, "s3_bucket_archive"},
		"s3 bucket too short":        {func(c *config.Config) { c.Storage.S3BucketArchive = "ab" }, "s3_bucket_archive"},
		"s3 bucket underscore":       {func(c *config.Config) { c.Storage.S3BucketArchive = "my_bucket" }, "s3_bucket_archive"},
		"usd peg empty":              {func(c *config.Config) { c.Trades.USDPeggedClassicAssets = []string{""} }, "usd_pegged_classic_assets"},
		"usd peg unparseable":        {func(c *config.Config) { c.Trades.USDPeggedClassicAssets = []string{"not an asset"} }, "usd_pegged_classic_assets"},
		"usd peg native not classic": {func(c *config.Config) { c.Trades.USDPeggedClassicAssets = []string{"native"} }, "classic"},
		"usd peg crypto not classic": {func(c *config.Config) { c.Trades.USDPeggedClassicAssets = []string{"crypto:USDT"} }, "classic"},
		"usd peg fiat not classic":   {func(c *config.Config) { c.Trades.USDPeggedClassicAssets = []string{"fiat:USD"} }, "classic"},
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

		// history_archive_url must be a
		// full URL like its sibling fields, not just anything
		// url.Parse tolerates (scheme-less, empty).
		"history archive url empty": {func(c *config.Config) { c.Stellar.HistoryArchiveURL = "" }, "history_archive_url"},
		"history archive url scheme-less": {func(c *config.Config) {
			c.Stellar.HistoryArchiveURL = "history.stellar.org/prd/core-live/core_live_001"
		}, "history_archive_url"},

		// serving_statement_timeout must
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

		// The same ordering rule one layer up: request_timeout must
		// EXCEED the longest per-handler budget. At or below it the
		// blanket deadline is what fires on every slow read, so the
		// per-handler `…-timeout` 503 branches are unreachable and the
		// ceilings they advertise are fiction — the operator-settable
		// half of the bodyless-200 class, which the compile-time guard
		// in internal/api/v1 cannot see because it only ever checks
		// against the 15s DEFAULT.
		"request timeout equals the longest handler budget": {
			func(c *config.Config) { c.API.RequestTimeout = config.APIMaxHandlerBudget },
			"request_timeout",
		},
		"request timeout below the longest handler budget": {
			func(c *config.Config) { c.API.RequestTimeout = config.APIMaxHandlerBudget - time.Second },
			"request_timeout",
		},
		"request timeout negative": {
			func(c *config.Config) { c.API.RequestTimeout = -time.Second },
			"request_timeout",
		},

		// The two conflicting `_env`
		// conventions (value-vs-name) getting swapped.
		"redis password looks like env var name": {
			func(c *config.Config) { c.Storage.RedisPassword = "STELLARINDEX_REDIS_PASSWORD" },
			"storage.redis_password looks like",
		},
		"clickhouse password looks like env var name": {
			func(c *config.Config) {
				c.Storage.ClickHouseServingPassword = "STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD"
			},
			"storage.clickhouse_serving_password looks like",
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

		// anomaly.thresholds / classifications
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
		// A key no asset can ever String() to never matches, so the asset
		// silently stays on the looser default thresholds.
		"anomaly classifications unparseable key": {
			func(c *config.Config) {
				c.Anomaly.Classifications = map[string]string{"USDC-GA5Z": "stablecoin"}
			},
			"not a canonical asset id",
		},

		// ADR-0019 §"Freeze duration" the auto-unfreeze band
		// must not overlap the fire band on the z axis, or a signal
		// hovering at the trigger flaps the pair frozen/unfrozen every
		// bucket — republishing, each time it unfreezes, the value the
		// freeze just refused. That is the flapping behaviour, so
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

		// divergence.supply.refresh_interval_seconds<=0
		// while enabled would reach time.NewTicker(0) and panic the
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
		// A negative ceiling is non-zero, so it escapes the default and
		// rejects every round as stale — the feed reads dead forever.
		"divergence chainlink negative max age": {
			func(c *config.Config) {
				c.Divergence.Chainlink.FeedMap = map[string]config.ChainlinkFeedConfig{
					"fiat:GBP/fiat:USD": {Address: "0x5c0Ab2d9b5a7ed9f470386e82BB36A3613cDd4b5", MaxAgeHours: -76},
				}
			},
			"divergence.chainlink.feeds",
		},

		// ADR-0027 cold tier: the *_key_env pair
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

		// The bucket field is the sole ColdTieringEnabled gate, so
		// a bucket set without its region/endpoint would reach
		// pipeline.NewColdDataStore with a zero-value Region — SigV4
		// cannot sign against that. Reject at load time.
		"cold bucket set without region": {
			func(c *config.Config) {
				c.Storage.S3ColdBucketArchive = "aws-public-blockchain/v1.1/stellar/ledgers/pubnet"
				c.Storage.S3ColdEndpoint = "https://s3.us-east-2.amazonaws.com"
			},
			"s3_cold_region",
		},
		"cold bucket set without endpoint": {
			func(c *config.Config) {
				c.Storage.S3ColdBucketArchive = "aws-public-blockchain/v1.1/stellar/ledgers/pubnet"
				c.Storage.S3ColdRegion = "us-east-2"
			},
			"s3_cold_endpoint",
		},

		// Reflector contracts: enabling a reflector source whose contract is
		// empty must fail Validate, not defer to indexer startup.
		"reflector-dex source without dex_contract": {reflectorSourceWithout("reflector-dex", func(c *config.Config) { c.Oracle.Reflector.DEXContract = "" }), "reflector-dex"},
		"reflector-cex source without cex_contract": {reflectorSourceWithout("reflector-cex", func(c *config.Config) { c.Oracle.Reflector.CEXContract = "" }), "reflector-cex"},
		"reflector-fx source without fx_contract":   {reflectorSourceWithout("reflector-fx", func(c *config.Config) { c.Oracle.Reflector.FXContract = "" }), "reflector-fx"},

		// A typo in enabled_sources is caught at Validate time, before the
		// storage-open and RPC-probe budget is spent.
		"unknown source": {func(c *config.Config) { c.Ingestion.EnabledSources = []string{"soroswap", "sorowsap"} }, "unknown source"},

		// projector_source reads forward events FROM ClickHouse, which only
		// makes sense while the live sink is WRITING them.
		"clickhouse projector source without live sink (names projector_source)": {
			func(c *config.Config) {
				c.Storage.ClickHouseProjectorSource = true
				c.Storage.ClickHouseLiveSink = false
			},
			"clickhouse_projector_source",
		},
		"clickhouse projector source without live sink (names live_sink)": {
			func(c *config.Config) {
				c.Storage.ClickHouseProjectorSource = true
				c.Storage.ClickHouseLiveSink = false
			},
			"clickhouse_live_sink",
		},

		// 0 is the documented off switch; a negative ceiling would silently
		// disable the guard while reading as a configured value.
		"negative max_market_cap_volume_ratio": {func(c *config.Config) { c.Aggregate.MaxMarketCapVolumeRatio = -1 }, "max_market_cap_volume_ratio"},

		"composite leg_dispersion_bps above 10000": {func(c *config.Config) { c.Aggregate.CompositeReference.LegDispersionBps = 10_001 }, "aggregate.composite_reference.leg_dispersion_bps"},
		"composite leg_dispersion_bps negative":    {func(c *config.Config) { c.Aggregate.CompositeReference.LegDispersionBps = -1 }, "aggregate.composite_reference.leg_dispersion_bps"},
		"composite release_band_pct above 100":     {func(c *config.Config) { c.Aggregate.CompositeReference.ReleaseBandPct = 100.5 }, "aggregate.composite_reference.release_band_pct"},
		"composite release_band_pct negative":      {func(c *config.Config) { c.Aggregate.CompositeReference.ReleaseBandPct = -0.1 }, "aggregate.composite_reference.release_band_pct"},
		"composite tolerance_bps above 10000":      {func(c *config.Config) { c.Aggregate.CompositeReference.ToleranceBps = 10_001 }, "aggregate.composite_reference.tolerance_bps"},
		"composite min_leg_sources negative":       {func(c *config.Config) { c.Aggregate.CompositeReference.MinLegSources = -1 }, "aggregate.composite_reference.min_leg_sources"},
		"composite fx_max_age_hours negative":      {func(c *config.Config) { c.Aggregate.CompositeReference.FXMaxAgeHours = -1 }, "aggregate.composite_reference.fx_max_age_hours"},
		"composite target unparseable":             {func(c *config.Config) { c.Aggregate.CompositeReference.Targets = []string{"not-a-pair"} }, "aggregate.composite_reference.targets"},
		"composite target without chain when chains exist": {func(c *config.Config) {
			c.Aggregate.Triangulations = []config.TriangulationChainConfig{
				{Target: "crypto:XLM/fiat:EUR", Legs: []string{"crypto:XLM/fiat:USD", "fiat:USD/fiat:EUR"}},
			}
			c.Aggregate.CompositeReference.Targets = []string{"crypto:XLM/fiat:GBP"}
		}, "no [[aggregate.triangulations]] row"},
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

// TestValidate_RejectsWithoutSentinel covers checks whose errors do not wrap
// ErrInvalidConfig (HashDBConfig, SupplyConfig and MetadataConfig validate
// in the older style), so asserting the sentinel would test something they
// never satisfy.
func TestValidate_RejectsWithoutSentinel(t *testing.T) {
	// A flipped character keeps the prefix, length and alphabet but breaks the
	// CRC: the account matches nothing on-chain and would contribute 0 to
	// circulating supply with a clean boot.
	flipped := testReserveAccount[:20] + "A" + testReserveAccount[21:]
	hashDB := func(c *config.Config) {
		c.HashDB.Enabled = true
		c.HashDB.Path = "/var/lib/stellarindex/hashdb.bin"
	}
	cases := map[string]struct {
		mut    func(*config.Config)
		errSub string
	}{
		"hashdb enabled without path": {
			func(c *config.Config) { hashDB(c); c.HashDB.Path = "" },
			"hashdb: path",
		},
		// A typo is a config mistake, not "this account has zero reserves".
		"sdf reserve account malformed": {
			func(c *config.Config) { c.Supply.SDFReserveAccounts = []string{"not-a-g-strkey"} },
			"sdf_reserve_accounts",
		},
		"sdf reserve account bad checksum": {
			func(c *config.Config) { c.Supply.SDFReserveAccounts = []string{testReserveAccount, flipped} },
			"sdf_reserve_accounts[1]",
		},
		"watched issuer account malformed": {
			func(c *config.Config) {
				c.Metadata.WatchedIssuerAccounts = []string{"GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2", "USDC"}
			},
			"watched_issuer_accounts[1]",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := withBad(tc.mut).Validate()
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.errSub) {
				t.Errorf("err = %v; want substring %q", err, tc.errSub)
			}
		})
	}
}

func TestValidate_ReflectorDecimalsCeiling(t *testing.T) {
	for name, set := range map[string]func(*config.Config, uint8){
		"oracle.reflector.dex_decimals": func(c *config.Config, d uint8) { c.Oracle.Reflector.DEXDecimals = d },
		"oracle.reflector.cex_decimals": func(c *config.Config, d uint8) { c.Oracle.Reflector.CEXDecimals = d },
		"oracle.reflector.fx_decimals":  func(c *config.Config, d uint8) { c.Oracle.Reflector.FXDecimals = d },
	} {
		t.Run(name, func(t *testing.T) {
			c := config.Default()
			set(&c, 38)
			if err := c.Validate(); err != nil {
				t.Fatalf("%s = 38 should pass: %v", name, err)
			}
			set(&c, 39)
			err := c.Validate()
			if !errors.Is(err, config.ErrInvalidConfig) || !strings.Contains(err.Error(), name) {
				t.Fatalf("%s = 39: err = %v, want ErrInvalidConfig naming the key", name, err)
			}
		})
	}
}

// Config URLs that feed outbound fetches must be absolute http(s) URLs
// with a host, checked at boot; empty keeps the built-in default. The
// value is never echoed: an RPC URL can carry an API key in its path.
func TestValidate_OutboundURLFieldsNeedSchemeAndHost(t *testing.T) {
	fields := map[string]func(*config.Config, string){
		"api.prometheus_url":                   func(c *config.Config, v string) { c.API.PrometheusURL = v },
		"divergence.coingecko.base_url":        func(c *config.Config, v string) { c.Divergence.CoinGecko.BaseURL = v },
		"divergence.supply.dashboard.base_url": func(c *config.Config, v string) { c.Divergence.Supply.Dashboard.BaseURL = v },
		"divergence.supply.coingecko.base_url": func(c *config.Config, v string) { c.Divergence.Supply.CoinGecko.BaseURL = v },
		"divergence.chainlink.rpc_url":         func(c *config.Config, v string) { c.Divergence.Chainlink.RPCURL = v },
	}
	const marker = "fixture-path-key"
	for field, set := range fields {
		for _, bad := range []string{
			"localhost:9090/" + marker,    // scheme-less: parses with scheme "localhost"
			"ftp://example.com/" + marker, // wrong scheme
			"https:///" + marker,          // no host
			"/relative/" + marker,         // relative
		} {
			t.Run(field+"/"+bad, func(t *testing.T) {
				c := config.Default()
				set(&c, bad)
				err := c.Validate()
				if err == nil || !strings.Contains(err.Error(), field) {
					t.Fatalf("err = %v, want a rejection naming %s", err, field)
				}
				if strings.Contains(err.Error(), marker) {
					t.Errorf("error echoes the URL: %v", err)
				}
			})
		}
		for _, ok := range []string{"", "https://example.com/api", "http://127.0.0.1:9090"} {
			c := config.Default()
			set(&c, ok)
			if err := c.Validate(); err != nil {
				t.Errorf("%s = %q: unexpected error %v", field, ok, err)
			}
		}
	}
}

// TestValidate_OracleStalenessOverrideAccepted is the shape an operator
// writes when an asset is legitimately slow.
func TestValidate_OracleStalenessOverrideAccepted(t *testing.T) {
	c := config.Default()
	c.Oracle.StalenessOverrides = []config.OracleStalenessOverrideConfig{{
		Source:        "reflector-cex",
		Asset:         "crypto:DAI",
		BudgetSeconds: 32400,
		Reason:        "peg asset; publishes only on movement, observed gaps to 7h",
	}}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid staleness override should pass: %v", err)
	}
}

// TestValidate_RejectsBadOracleStalenessOverride covers every way a
// per-asset budget can be written down and silently do nothing.
//
// The override's only job is to change one alert's threshold, and each
// row below changes NOTHING while looking like it does: a source
// nothing emits, an asset spelled differently from the metric's label,
// a budget that would ticket on every evaluation, an unexplained
// claim, or two rows fighting over one pair. None of them produce a
// runtime error — the alert just keeps firing on its old schedule
// while the config reads as if it were handled — so they have to fail
// at startup.
func TestValidate_RejectsBadOracleStalenessOverride(t *testing.T) {
	base := config.OracleStalenessOverrideConfig{
		Source:        "reflector-cex",
		Asset:         "crypto:DAI",
		BudgetSeconds: 32400,
		Reason:        "peg asset; publishes only on movement",
	}
	// with returns a copy of base with one field bent out of shape.
	with := func(mutate func(*config.OracleStalenessOverrideConfig)) config.OracleStalenessOverrideConfig {
		row := base
		mutate(&row)
		return row
	}

	cases := []struct {
		name string
		rows []config.OracleStalenessOverrideConfig
		want string
	}{
		{
			name: "source is not an oracle",
			rows: []config.OracleStalenessOverrideConfig{
				with(func(r *config.OracleStalenessOverrideConfig) { r.Source = "soroswap" }),
			},
			want: "is not an oracle source",
		},
		{
			name: "source typo",
			rows: []config.OracleStalenessOverrideConfig{
				with(func(r *config.OracleStalenessOverrideConfig) { r.Source = "reflector_cex" }),
			},
			want: "is not an oracle source",
		},
		{
			// The metric's asset label is canonical.Asset.String(), so a
			// bare oracle symbol keys a series that never exists.
			name: "bare symbol instead of canonical asset",
			rows: []config.OracleStalenessOverrideConfig{
				with(func(r *config.OracleStalenessOverrideConfig) { r.Asset = "DAI" }),
			},
			want: "not a canonical asset identifier",
		},
		{
			// "XLM" parses — to the native asset, which stringifies back
			// as "native". An override written this way would look
			// correct and match nothing.
			name: "asset alias that does not round-trip",
			rows: []config.OracleStalenessOverrideConfig{
				with(func(r *config.OracleStalenessOverrideConfig) { r.Asset = "XLM" }),
			},
			want: "is an alias for",
		},
		{
			name: "zero budget",
			rows: []config.OracleStalenessOverrideConfig{
				with(func(r *config.OracleStalenessOverrideConfig) { r.BudgetSeconds = 0 }),
			},
			want: "budget_seconds must be > 0",
		},
		{
			name: "negative budget",
			rows: []config.OracleStalenessOverrideConfig{
				with(func(r *config.OracleStalenessOverrideConfig) { r.BudgetSeconds = -1 }),
			},
			want: "budget_seconds must be > 0",
		},
		{
			name: "no stated reason",
			rows: []config.OracleStalenessOverrideConfig{
				with(func(r *config.OracleStalenessOverrideConfig) { r.Reason = "   " }),
			},
			want: "reason is required",
		},
		{
			name: "two budgets for one pair",
			rows: []config.OracleStalenessOverrideConfig{
				base,
				with(func(r *config.OracleStalenessOverrideConfig) { r.BudgetSeconds = 3600 }),
			},
			want: "duplicates",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := config.Default()
			c.Oracle.StalenessOverrides = tc.rows
			err := c.Validate()
			if err == nil {
				t.Fatal("expected rejection — this row would silently match no series")
			}
			if !errors.Is(err, config.ErrInvalidConfig) {
				t.Errorf("error = %v, want it to wrap ErrInvalidConfig", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tc.want)
			}
		})
	}
}

// [api] status_services declares the background services a
// deployment actually runs, and /v1/status reports + rolls up exactly
// those. The lean test-net inventories render `["indexer"]` (they already
// set run_aggregator: false), which must load and validate — and a typo'd
// service name must be rejected at boot, because a name Prometheus never
// publishes a heartbeat for can never be reported down and would hold
// `overall` at "degraded" forever, i.e. the exact symptom the field exists
// to remove.
func TestValidate_StatusServices(t *testing.T) {
	t.Run("lean test-net shape validates", func(t *testing.T) {
		c := config.Default()
		c.API.StatusServices = []string{"indexer"}
		if err := c.Validate(); err != nil {
			t.Fatalf(`status_services = ["indexer"] should validate, got: %v`, err)
		}
	})

	t.Run("pubnet default is both services", func(t *testing.T) {
		got := config.Default().API.StatusServices
		if len(got) != 2 || got[0] != "indexer" || got[1] != "aggregator" {
			t.Errorf("Default().API.StatusServices = %v, want [indexer aggregator]", got)
		}
	})

	t.Run("rejects an unknown service name", func(t *testing.T) {
		c := config.Default()
		c.API.StatusServices = []string{"indexer", "agregator"} // typo
		err := c.Validate()
		if err == nil {
			t.Fatal("a typo'd service name must be rejected at boot, got nil")
		}
		if !strings.Contains(err.Error(), "status_services") {
			t.Errorf("error should name the offending key, got: %v", err)
		}
	})
}

// A config-validation failure is fatal at boot, so its message lands on
// stderr -> journald -> promtail -> Loki, where anyone with Grafana read
// access can read it for the 720h retention. That makes a validation
// branch that formats the offending VALUE into its message a credential
// leak whenever the value can be a credential — and the branches below
// are exactly the ones that fire on the paste-the-secret-where-the-name-
// goes mistake they exist to catch.
//
// Every case here is a real operator misconfiguration, not a contrived
// one:
//
//   - a managed-Redis URI (Upstash / Redis Cloud / Railway / Heroku all
//     issue rediss://default:<password>@host:port, never the bare
//     host:port storage.redis_addr wants) pasted into redis_addr or a
//     sentinel entry. net.SplitHostPort rejects it with an *net.AddrError
//     that embeds the whole address, so a `: %w` wrap re-leaks it even
//     when the %q is redacted.
//   - a real DSN with an inline password under a scheme we don't accept.
//   - an S3 secret key pasted where the env-var NAME belongs.
func TestValidate_FatalErrorsDoNotEchoCredentials(t *testing.T) {
	const secret = "hunter2-NOT-IN-THE-BOOT-LOG"

	cases := map[string]func(*config.Config){
		"redis_addr as a managed-Redis URI": func(c *config.Config) {
			c.Storage.RedisAddr = "rediss://default:" + secret + "@myredis.example.com:6380"
		},
		"redis_sentinel_addrs entry as a managed-Redis URI": func(c *config.Config) {
			c.Storage.RedisMasterName = "mymaster"
			c.Storage.RedisSentinelAddrs = []string{
				"127.0.0.1:26379",
				"rediss://default:" + secret + "@sentinel.example.com:26380",
			}
		},
		"postgres_dsn under a wrong scheme, password inline": func(c *config.Config) {
			c.Storage.PostgresDSN = "mysql://stellarindex:" + secret + "@db.example.com:3306/stellarindex"
		},
		"s3_access_key_env holding the credential": func(c *config.Config) {
			c.Storage.S3AccessKeyEnv = secret
		},
		"s3_secret_key_env holding the credential": func(c *config.Config) {
			c.Storage.S3SecretKeyEnv = secret
		},
		"s3_cold_access_key_env holding the credential": func(c *config.Config) {
			c.Storage.S3ColdAccessKeyEnv = secret
			c.Storage.S3ColdSecretKeyEnv = "STELLARINDEX_S3_COLD_SECRET_KEY"
		},
		"s3_cold_secret_key_env holding the credential": func(c *config.Config) {
			c.Storage.S3ColdAccessKeyEnv = "STELLARINDEX_S3_COLD_ACCESS_KEY"
			c.Storage.S3ColdSecretKeyEnv = secret
		},
		"half a cold-tier pair, the set half holding the credential": func(c *config.Config) {
			c.Storage.S3ColdAccessKeyEnv = secret
			c.Storage.S3ColdSecretKeyEnv = ""
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := config.Default()
			mutate(&c)

			err := c.Validate()
			if err == nil {
				t.Fatal("expected a validation error — this case is a misconfiguration")
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("fatal boot error echoes the credential:\n  %v", err)
			}
		})
	}
}

// TestValidate_RedactedErrorsStillIdentifyTheSetting is the other half
// of the contract: withholding the value must not cost the operator the
// ability to tell WHICH setting is wrong (and, for the all-or-nothing
// cold-tier pair, which half of it they left out).
func TestValidate_RedactedErrorsStillIdentifyTheSetting(t *testing.T) {
	cases := map[string]struct {
		mutate func(*config.Config)
		want   []string
	}{
		"redis_addr": {
			func(c *config.Config) { c.Storage.RedisAddr = "127.0.0.1" },
			[]string{"storage.redis_addr", "host:port", "missing port"},
		},
		"redis_sentinel_addrs names the index": {
			func(c *config.Config) {
				c.Storage.RedisMasterName = "mymaster"
				c.Storage.RedisSentinelAddrs = []string{"127.0.0.1:26379", "127.0.0.1"}
			},
			[]string{"storage.redis_sentinel_addrs[1]", "host:port"},
		},
		"postgres_dsn keeps the scheme, which is the mistake": {
			func(c *config.Config) { c.Storage.PostgresDSN = "mysql://u:p@h/db" },
			[]string{"storage.postgres_dsn", "mysql://<redacted>", "postgres://"},
		},
		"s3_access_key_env": {
			func(c *config.Config) { c.Storage.S3AccessKeyEnv = "not-a-name" },
			[]string{"storage.s3_access_key_env", "UPPER_SNAKE_CASE"},
		},
		"cold pair says which half was set": {
			func(c *config.Config) {
				c.Storage.S3ColdAccessKeyEnv = "STELLARINDEX_S3_COLD_ACCESS_KEY"
				c.Storage.S3ColdSecretKeyEnv = ""
			},
			[]string{
				"storage.s3_cold_access_key_env (set)",
				"storage.s3_cold_secret_key_env (empty)",
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := config.Default()
			tc.mutate(&c)

			err := c.Validate()
			if err == nil {
				t.Fatal("expected a validation error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error lost the %q diagnostic:\n  %v", want, err)
				}
			}
		})
	}
}

// TestValidate_EnvVarNameSwapStillEchoesTheName pins the deliberate
// exception to the no-echo rule, so a later tightening pass does not
// redact a message whose entire diagnostic is the value. The
// redis_password / clickhouse_serving_password branches fire
// ONLY when the value matched `^STELLARINDEX_[A-Z0-9_]+$` — i.e. it is
// provably one of this project's own env-var NAMES and provably not the
// password the field is supposed to hold.
func TestValidate_EnvVarNameSwapStillEchoesTheName(t *testing.T) {
	for field, mutate := range map[string]func(*config.Config){
		"storage.redis_password": func(c *config.Config) {
			c.Storage.RedisPassword = "STELLARINDEX_REDIS_PASSWORD"
		},
		"storage.clickhouse_serving_password": func(c *config.Config) {
			c.Storage.ClickHouseServingPassword = "STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD"
		},
	} {
		t.Run(field, func(t *testing.T) {
			c := config.Default()
			mutate(&c)

			err := c.Validate()
			if err == nil {
				t.Fatalf("%s: expected the swapped-convention error", field)
			}
			if !strings.Contains(err.Error(), field) {
				t.Errorf("%s: error does not name the setting:\n  %v", field, err)
			}
			if !strings.Contains(err.Error(), "STELLARINDEX_") {
				t.Errorf("%s: error withheld the env-var NAME, which is the whole diagnostic here:\n  %v",
					field, err)
			}
		})
	}
}
