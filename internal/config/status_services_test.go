package config_test

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// The test-net render of the ansible template sets [region] deployment;
// pubnet keeps the "production" default so r1's /v1/status wire contract
// is byte-identical. The tier is not a Go string literal, so
// api.testnet.stellarindex.io does not answer `"deployment": "production"`.
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
