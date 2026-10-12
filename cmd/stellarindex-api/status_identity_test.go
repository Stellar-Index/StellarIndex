package main

import (
	"reflect"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// /v1/status deployment tier and background-service list must come from config, not be hardcoded: a lean test net would
// otherwise show a PRODUCTION tag and report an aggregator it doesn't run, holding overall at degraded forever.
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
