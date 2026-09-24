package timescale

import (
	"context"
	"strings"
	"testing"
)

// Defensive-guard coverage. The INSERT/SELECT round-trip runs against
// real TimescaleDB in test/integration/soroswap_pairs_storage_test.go.

func TestUpsertSoroswapPair_rejectsEmptyPair(t *testing.T) {
	s := &Store{}
	err := s.UpsertSoroswapPair(context.Background(), "", "C0", "C1")
	if err == nil || !strings.Contains(err.Error(), "pair_strkey") {
		t.Errorf("err=%v should mention pair_strkey", err)
	}
}

func TestUpsertSoroswapPair_rejectsEmptyToken0(t *testing.T) {
	s := &Store{}
	err := s.UpsertSoroswapPair(context.Background(), "Cpair", "", "C1")
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("err=%v should mention token", err)
	}
}

func TestUpsertSoroswapPair_rejectsEmptyToken1(t *testing.T) {
	s := &Store{}
	err := s.UpsertSoroswapPair(context.Background(), "Cpair", "C0", "")
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("err=%v should mention token", err)
	}
}
