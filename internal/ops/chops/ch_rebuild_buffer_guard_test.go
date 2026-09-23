package chops

import (
	"os"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
)

// Every ch-rebuild pass appends into the one in-process buffer, so the
// maxBufferedRange guard must engage for an sdex-only or ContractCall-only
// invocation, not just when an event decoder is selected.
func TestCHRebuildBuffers_EveryPassEngagesTheRangeGuard(t *testing.T) {
	t.Parallel()
	cat, _, err := buildReconciliationCatalogue(config.Config{})
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	sep41Cat := []reconSource{{name: sep41supply.SourceName}, {name: sep41transfers.SourceName}}
	var eventSrc, callSrc string
	hasSDEX := false
	for _, src := range cat {
		switch {
		case src.dec != nil && eventSrc == "":
			eventSrc = src.name
		case src.callDec != nil && callSrc == "":
			callSrc = src.name
		case src.name == sdex.SourceName:
			hasSDEX = true
		}
	}
	if eventSrc == "" || callSrc == "" || !hasSDEX {
		t.Fatalf("catalogue lacks a source of each pass kind (event %q, call %q, sdex %v) — this test is asserting nothing",
			eventSrc, callSrc, hasSDEX)
	}
	only := func(name string) func(string) bool { return func(n string) bool { return n == name } }
	all := func(string) bool { return true }

	cases := []struct {
		name    string
		passes  chRebuildPasses
		enabled func(string) bool
		want    bool
	}{
		{"-sdex -sources sdex", chRebuildPasses{sdex: true}, only(sdex.SourceName), true},
		{"-contract-calls -sources " + callSrc, chRebuildPasses{contractCalls: true}, only(callSrc), true},
		{"-sources " + eventSrc, chRebuildPasses{}, only(eventSrc), true},
		{"-sep41", chRebuildPasses{sep41: true}, only(callSrc), true},
		{"no -sources", chRebuildPasses{}, all, true},
		{"-sources sdex without -sdex decodes nothing", chRebuildPasses{}, only(sdex.SourceName), false},
		{"-sources " + callSrc + " without -contract-calls decodes nothing", chRebuildPasses{}, only(callSrc), false},
	}
	for _, tc := range cases {
		if got := chRebuildBuffers(cat, sep41Cat, tc.passes, tc.enabled); got != tc.want {
			t.Errorf("%s: chRebuildBuffers = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The predicate is only a guard if chRebuild consults it at the range check.
func TestCHRebuild_RangeGuardUsesThePassSelection(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("ch_rebuild.go")
	if err != nil {
		t.Fatal(err)
	}
	body := funcBodyFrom(string(b), "chRebuild")
	if !strings.Contains(body, "chRebuildBuffers(cat, sep41Cat, passes, enabled) && *to-*from > maxBufferedRange") {
		t.Fatal("chRebuild's maxBufferedRange guard no longer keys on chRebuildBuffers(cat, sep41Cat, passes, enabled)")
	}
}
