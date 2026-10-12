package config_test

import (
	"strings"
	"testing"

	cfg "github.com/Stellar-Index/StellarIndex/internal/config"
)

// AggregatorPairs and AggregatorWindows are infallible in practice (validate() rejects bad entries at startup) but still
// return errors so bypassed validation fails loudly. These tests call them on a struct that skips validation, to catch a
// refactor that swallows the parse error.

func TestAggregatorPairs_emptyReturnsNil(t *testing.T) {
	a := cfg.AggregateConfig{}
	pairs, err := a.AggregatorPairs()
	if err != nil {
		t.Fatalf("empty pairs: %v", err)
	}
	if pairs != nil {
		t.Errorf("got %v, want nil for empty Pairs", pairs)
	}
}

func TestAggregatorPairs_directBadEntryErrors(t *testing.T) {
	// Bypass validate() by constructing AggregateConfig directly with
	// a pair string that won't parse. The method's error branch must
	// surface the offending entry in the error message.
	a := cfg.AggregateConfig{Pairs: []string{"bogus-not-a-pair"}}
	_, err := a.AggregatorPairs()
	if err == nil {
		t.Fatal("expected error from malformed pair, got nil")
	}
	if !strings.Contains(err.Error(), "bogus-not-a-pair") {
		t.Errorf("error %q should cite the offending entry", err.Error())
	}
}

func TestAggregatorWindows_emptyReturnsNil(t *testing.T) {
	a := cfg.AggregateConfig{}
	wins, err := a.AggregatorWindows()
	if err != nil {
		t.Fatalf("empty windows: %v", err)
	}
	if wins != nil {
		t.Errorf("got %v, want nil for empty Windows", wins)
	}
}

func TestAggregatorWindows_directBadEntryErrors(t *testing.T) {
	a := cfg.AggregateConfig{Windows: []string{"7 fortnights"}}
	_, err := a.AggregatorWindows()
	if err == nil {
		t.Fatal("expected error from malformed window, got nil")
	}
	if !strings.Contains(err.Error(), "7 fortnights") {
		t.Errorf("error %q should cite the offending entry", err.Error())
	}
}

// A negative window inverts the trade query range every tick, a zero one
// is a silently empty bucket, and a repeat ("5m" vs "300s") double-refreshes
// one window; all three must fail at config load, not at the first tick.
func TestAggregatorWindows_rejectsNonPositiveAndDuplicate(t *testing.T) {
	for name, windows := range map[string][]string{
		"negative":  {"5m", "-5m"},
		"zero":      {"0s", "1h"},
		"duplicate": {"5m", "1h", "300s"},
	} {
		t.Run(name, func(t *testing.T) {
			a := cfg.AggregateConfig{Windows: windows}
			if _, err := a.AggregatorWindows(); err == nil {
				t.Errorf("AggregatorWindows(%q) = nil error, want rejection", windows)
			}
		})
	}
}
