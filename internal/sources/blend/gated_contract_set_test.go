package blend

import (
	"slices"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// The completeness re-derive scopes its lake read to GatedContractSet; a set
// narrower than what Matches accepts would undercount expected events and
// report a false complete. Every factory and every announced pool must be in it.
func TestDecoder_GatedContractSetCoversWhatMatchesAccepts(t *testing.T) {
	pool := contractStrkeyFromSeed(t, 0x66)
	foreign := contractStrkeyFromSeed(t, 0x77)

	d := NewDecoder()
	if _, err := d.Decode(makeDeployEventFrom(t, MainnetPoolFactories[0], pool)); err != nil {
		t.Fatalf("Decode(deploy): %v", err)
	}
	got := d.GatedContractSet()
	for _, f := range MainnetPoolFactories {
		if !slices.Contains(got, f) {
			t.Errorf("gated set omits factory %s", f)
		}
		if !d.Matches(makeDeployEventFrom(t, f, pool)) {
			t.Errorf("Matches rejects deploy from factory %s", f)
		}
	}
	supply := func(c string) events.Event {
		return events.Event{Topic: []string{TopicSymbolSupply}, ContractID: c}
	}
	if !slices.Contains(got, pool) || !d.Matches(supply(pool)) {
		t.Errorf("announced pool: listed=%v matches=%v, want both", slices.Contains(got, pool), d.Matches(supply(pool)))
	}
	if slices.Contains(got, foreign) || d.Matches(supply(foreign)) {
		t.Errorf("foreign contract %s must be neither listed nor matched", foreign)
	}
}
