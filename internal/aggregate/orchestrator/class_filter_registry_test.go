package orchestrator

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// noVenueRow lists the enabled-source names that deliberately carry no
// external.Registry row because they never write `trades`.
var noVenueRow = map[string]string{
	"blend_backstop": "projected lending surface; its replay audit is blend's (registry.go replayAuditCoveredBy)",
}

// Every source an indexer can enable must have an explicit registry row.
// Without one, filterForVWAP fail-closes it through external.Lookup while
// the prices_* CAGGs, which group every `trades` row, count it: the same
// trades would be excluded from the aggregator's VWAP and included in the
// served one.
func TestKnownSourcesHaveARegistryClass(t *testing.T) {
	for name := range config.KnownSources {
		if _, exempt := noVenueRow[name]; exempt {
			continue
		}
		if !external.Registered(name) {
			t.Errorf("config.KnownSources has %q but external.Registry has no row: its trades would be dropped by the aggregator's class filter and counted by the CAGGs", name)
		}
	}
	for name, why := range noVenueRow {
		if _, known := config.KnownSources[name]; !known {
			t.Errorf("noVenueRow exempts %q (%s), which is no longer a known source; drop the exemption", name, why)
		}
		if external.Registered(name) {
			t.Errorf("noVenueRow exempts %q but external.Registry now has a row; drop the exemption", name)
		}
	}
}
