package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/RatesEngine/rates-engine/internal/config"
)

func TestValidate_DefaultPasses(t *testing.T) {
	// Default() MUST pass Validate — that's the "fresh install
	// works" contract every binary depends on.
	if err := config.Default().Validate(); err != nil {
		t.Fatalf("Default().Validate: %v", err)
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
		"empty region id":       {func(c *config.Config) { c.Region.ID = "" }, "region.id"},
		"capitalized region":    {func(c *config.Config) { c.Region.ID = "R1" }, "region.id"},
		"home domain is URL":    {func(c *config.Config) { c.Region.HomeDomain = "https://ratesengine.net" }, "home_domain"},
		"unknown network":       {func(c *config.Config) { c.Stellar.Network = "futurenett" }, "network"},
		"empty rpc list":        {func(c *config.Config) { c.Stellar.RPCEndpoints = nil }, "rpc_endpoints"},
		"rpc not url":           {func(c *config.Config) { c.Stellar.RPCEndpoints = []string{"host:8000"} }, "rpc_endpoints"},
		"duplicate rpc":         {func(c *config.Config) { c.Stellar.RPCEndpoints = []string{"http://rpc1:8000", "http://rpc1:8000"} }, "duplicate"},
		"duplicate rpc case":    {func(c *config.Config) { c.Stellar.RPCEndpoints = []string{"http://Rpc1:8000", "HTTP://rpc1:8000"} }, "duplicate"},
		"duplicate rpc trailing slash": {func(c *config.Config) { c.Stellar.RPCEndpoints = []string{"http://rpc1:8000", "http://rpc1:8000/"} }, "duplicate"},
		"missing postgres":      {func(c *config.Config) { c.Storage.PostgresDSN = "" }, "postgres_dsn"},
		"wrong postgres scheme": {func(c *config.Config) { c.Storage.PostgresDSN = "mysql://x" }, "postgres_dsn"},
		"bad redis addr":        {func(c *config.Config) { c.Storage.RedisAddr = "127.0.0.1" }, "redis_addr"},
		"bad cursor store":      {func(c *config.Config) { c.Ingestion.CursorStoreScheme = "kafka" }, "cursor_store_scheme"},
		"zero batch":            {func(c *config.Config) { c.Ingestion.BackfillBatchSize = 0 }, "backfill_batch_size"},
		"duplicate source":      {func(c *config.Config) { c.Ingestion.EnabledSources = []string{"soroswap", "soroswap"} }, "duplicate"},
		"duplicate case-fold":   {func(c *config.Config) { c.Ingestion.EnabledSources = []string{"soroswap", "Soroswap"} }, "duplicate"},
		"empty source entry":    {func(c *config.Config) { c.Ingestion.EnabledSources = []string{"soroswap", ""} }, "empty entry"},
		"bad reflector addr":    {func(c *config.Config) { c.Oracle.Reflector.DEXContract = "not-a-c-key" }, "dex_contract"},
		"zero vwap window":      {func(c *config.Config) { c.Aggregate.VWAPWindowSeconds = 0 }, "vwap_window_seconds"},
		"negative sigma":        {func(c *config.Config) { c.Aggregate.OutlierSigmaThreshold = -1 }, "outlier_sigma_threshold"},
		"no listen":             {func(c *config.Config) { c.API.ListenAddr = "" }, "listen_addr"},
		"bad listen":            {func(c *config.Config) { c.API.ListenAddr = "3000" }, "listen_addr"},
		"unknown auth":          {func(c *config.Config) { c.API.AuthMode = "oauth" }, "auth_mode"},
		"neg rate limit":        {func(c *config.Config) { c.API.AnonRateLimitPerMin = -5 }, "anon_rate_limit"},
		"bad log level":         {func(c *config.Config) { c.Obs.LogLevel = "verbose" }, "log_level"},
		"bad log format":        {func(c *config.Config) { c.Obs.LogFormat = "xml" }, "log_format"},
		"bad trace exporter":    {func(c *config.Config) { c.Obs.TraceExporter = "jaeger" }, "trace_exporter"},
		"trace sample over 1":   {func(c *config.Config) { c.Obs.TraceSample = 1.5 }, "trace_sample"},
		"trace sample neg":      {func(c *config.Config) { c.Obs.TraceSample = -0.1 }, "trace_sample"},
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
