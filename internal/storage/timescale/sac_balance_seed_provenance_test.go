package timescale

import (
	"context"
	"strings"
	"testing"
)

// These tests cover the Upsert/Read defensive guards — the real
// round-trip against Postgres needs testcontainers-go and lives in
// test/integration/ (per the Test conventions in CLAUDE.md).

func TestUpsertSACBalanceSeedProvenance_RejectsEmptyContractID(t *testing.T) {
	s := &Store{}
	err := s.UpsertSACBalanceSeedProvenance(context.Background(), SACBalanceSeedProvenance{
		AssetKey: "PHO:GAX5...",
		Source:   SACBalanceSeedSourceFullHistory,
	})
	if err == nil || !strings.Contains(err.Error(), "ContractID") {
		t.Errorf("err=%v should mention ContractID", err)
	}
}

func TestUpsertSACBalanceSeedProvenance_RejectsEmptyAssetKey(t *testing.T) {
	s := &Store{}
	err := s.UpsertSACBalanceSeedProvenance(context.Background(), SACBalanceSeedProvenance{
		ContractID: "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32",
		Source:     SACBalanceSeedSourceFullHistory,
	})
	if err == nil || !strings.Contains(err.Error(), "AssetKey") {
		t.Errorf("err=%v should mention AssetKey", err)
	}
}

func TestUpsertSACBalanceSeedProvenance_RejectsInvalidSource(t *testing.T) {
	s := &Store{}
	err := s.UpsertSACBalanceSeedProvenance(context.Background(), SACBalanceSeedProvenance{
		ContractID: "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32",
		AssetKey:   "PHO:GAX5...",
		Source:     "made_up_source",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid source") {
		t.Errorf("err=%v should mention invalid source", err)
	}
}

func TestUpsertSACBalanceSeedProvenance_RejectsNegativeHoldersSeeded(t *testing.T) {
	s := &Store{}
	err := s.UpsertSACBalanceSeedProvenance(context.Background(), SACBalanceSeedProvenance{
		ContractID:    "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32",
		AssetKey:      "PHO:GAX5...",
		Source:        SACBalanceSeedSourceCurrentState,
		HoldersSeeded: -1,
	})
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf("err=%v should mention negative HoldersSeeded", err)
	}
}

func TestSACBalanceSeedProvenanceFor_RejectsEmptyContractID(t *testing.T) {
	s := &Store{}
	_, _, err := s.SACBalanceSeedProvenanceFor(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "contractID") {
		t.Errorf("err=%v should mention contractID", err)
	}
}
