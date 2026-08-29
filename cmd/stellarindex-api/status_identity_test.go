package main

import (
	"reflect"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// #328: /v1/status's deployment TIER and background-service list were
// hardcoded at the v1.Options construction site
// (`RegionDeployment: "production"`, and a literal
// {"indexer","aggregator"} inside the handler). Every lean test-net
// deployment therefore served `"deployment":"production"` from
// api.testnet.stellarindex.io — which the explorer renders as a
// PRODUCTION tag on a test net — and reported an aggregator it
// deliberately does not run, holding `overall` at "degraded" forever.
//
// Both are operator facts, so both must come from config.
func TestStatusIdentity_PlumbedFromConfig(t *testing.T) {
	tests := []struct {
		name           string
		mutate         func(*config.Config)
		wantRegion     string
		wantDeployment string
		wantServices   []string
	}{
		{
			// Pubnet is unchanged — the defaults ARE r1's shape.
			name:           "pubnet defaults",
			mutate:         func(*config.Config) {},
			wantRegion:     "r1",
			wantDeployment: "production",
			wantServices:   []string{"indexer", "aggregator"},
		},
		{
			// The lean test net: no aggregator, and not production.
			name: "lean testnet",
			mutate: func(c *config.Config) {
				c.Region.ID = "testnet"
				c.Region.Deployment = "testnet"
				c.API.StatusServices = []string{"indexer"}
			},
			wantRegion:     "testnet",
			wantDeployment: "testnet",
			wantServices:   []string{"indexer"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			tc.mutate(&cfg)

			region, deployment, services := statusIdentity(cfg)

			if region != tc.wantRegion {
				t.Errorf("region = %q, want %q", region, tc.wantRegion)
			}
			if deployment != tc.wantDeployment {
				t.Errorf("deployment = %q, want %q — the tier is an operator "+
					"fact, not a literal", deployment, tc.wantDeployment)
			}
			if !reflect.DeepEqual(services, tc.wantServices) {
				t.Errorf("services = %v, want %v", services, tc.wantServices)
			}
		})
	}
}
