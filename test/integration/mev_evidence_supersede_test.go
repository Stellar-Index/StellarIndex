//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func mevSupersedeState(t *testing.T, ctx context.Context, store *timescale.Store, key string) (legs int, accounts string, detectedAt time.Time) {
	t.Helper()
	err := store.DB().QueryRowContext(ctx,
		`SELECT jsonb_array_length(detail -> 'legs'), array_to_string(accounts, ','), detected_at
		   FROM mev_events WHERE dedup_key = $1`, key).Scan(&legs, &accounts, &detectedAt)
	if err != nil {
		t.Fatalf("select %s: %v", key, err)
	}
	return legs, accounts, detectedAt
}

// TestStorage_MEVEventEvidenceSupersedes (#1248): a re-scan whose legs
// contain the stored legs replaces the stored evidence (a later scan found
// another victim), a re-scan that saw fewer legs never overwrites, and
// neither counts as a new event or moves the first detection's time.
func TestStorage_MEVEventEvidenceSupersedes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	store := openMEVStore(t, ctx)

	const key = "sandwich:supersede:GATK"
	first := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	legA := `{"tx_hash":"a","role":"bracket"}`
	legV1 := `{"tx_hash":"v1","role":"victim"}`
	legV2 := `{"tx_hash":"v2","role":"victim"}`
	write := func(at time.Time, accounts []string, legs string) bool {
		t.Helper()
		ok, err := store.InsertMEVEvent(ctx, domain.MEVStoredEvent{
			Kind: "sandwich", Ledger: 61_000_000, DetectedAtLedger: 61_000_000, Timestamp: at,
			TxHashes: []string{"a"}, Accounts: accounts, DedupKey: key,
			DetailJSON: []byte(`{"legs":[` + legs + `],"note":"n"}`),
		})
		if err != nil {
			t.Fatalf("InsertMEVEvent: %v", err)
		}
		return ok
	}

	if !write(first, []string{"GATK", "GV1"}, legA+","+legV1) {
		t.Fatal("first detection reported inserted=false")
	}
	if write(first.Add(5*time.Minute), []string{"GATK", "GV1", "GV2"}, legA+","+legV1+","+legV2) {
		t.Error("a superseding re-scan reported inserted=true")
	}
	legs, accounts, at := mevSupersedeState(t, ctx, store, key)
	if legs != 3 || accounts != "GATK,GV1,GV2" {
		t.Errorf("after a containing re-scan: legs=%d accounts=%q, want 3 and GATK,GV1,GV2", legs, accounts)
	}
	if !at.Equal(first) {
		t.Errorf("detected_at moved to %v; the first detection's time is the event's identity", at)
	}

	if write(first.Add(10*time.Minute), []string{"GATK", "GV2"}, legA+","+legV2) {
		t.Error("a narrower re-scan reported inserted=true")
	}
	if legs, accounts, _ := mevSupersedeState(t, ctx, store, key); legs != 3 || accounts != "GATK,GV1,GV2" {
		t.Errorf("a narrower re-scan overwrote the stored evidence: legs=%d accounts=%q", legs, accounts)
	}
}
