package config_test

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// #328: [api] status_services declares the background services a
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

// The test-net render of the ansible template sets [region] deployment;
// pubnet keeps the "production" default so r1's /v1/status wire contract
// is byte-identical. Before #328 the tier was a Go string literal, so
// api.testnet.stellarindex.io answered `"deployment": "production"`.
func TestRegionDeployment_DefaultsToProduction(t *testing.T) {
	if got := config.Default().Region.Deployment; got != "production" {
		t.Errorf("Default().Region.Deployment = %q, want production", got)
	}

	c, err := config.LoadReader(strings.NewReader(`
[region]
id = "testnet"
deployment = "testnet"
`), "inline")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Region.Deployment != "testnet" {
		t.Errorf("Region.Deployment = %q, want testnet", c.Region.Deployment)
	}
}
