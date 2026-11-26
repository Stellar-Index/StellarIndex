package pipeline

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
)

// A pool warmed from protocol_contracts that is in neither in-code list restarts with no liquidity layout, so its
// deposits and withdraws are refused; the warm must say so rather than leave only decode_error counts.
func TestGatedRegistryOptions_aquariusPoolWithoutLayoutWarns(t *testing.T) {
	const unlisted = "CCPHUHQYFOJJ6WQUGUYHHPJYQGFLRQHJJTRJNWQG54MHCHPRFLWQI7SE"
	for _, tc := range []struct {
		name string
		rows []string
		want bool
	}{
		{"unlisted pool", []string{aquarius.MainnetPools[0], unlisted}, true},
		{"listed pools only", []string{aquarius.MainnetPools[0], aquarius.MainnetConcentratedPools[0]}, false},
	} {
		h := &capturingHandler{}
		store := &fakeProtocolContractStore{rows: map[string][]string{aquarius.SourceName: tc.rows}}
		if _, err := gatedRegistryOptions(context.Background(), store, slog.New(h), context.Background(), false); err != nil {
			t.Fatalf("%s: gatedRegistryOptions: %v", tc.name, err)
		}
		got := false
		for _, r := range h.records {
			if r.level >= slog.LevelWarn && r.source == aquarius.SourceName && strings.Contains(r.msg, "NO liquidity layout") {
				got = true
			}
		}
		if got != tc.want {
			t.Errorf("%s: layout warning logged = %v, want %v", tc.name, got, tc.want)
		}
	}
}
