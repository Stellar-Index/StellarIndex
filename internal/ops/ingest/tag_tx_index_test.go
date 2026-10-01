package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type txIndexKeysStore struct {
	keys     []timescale.TxIndexKey
	tagCalls int
}

func (s *txIndexKeysStore) UntaggedTxIndexTrades(_ context.Context, _, _ time.Time, afterLedger uint32, _ string, _ int) ([]timescale.TxIndexKey, error) {
	if afterLedger > 0 {
		return nil, nil
	}
	return s.keys, nil
}

func (s *txIndexKeysStore) TagTradesTxIndex(_ context.Context, _, _ time.Time, tags []timescale.TxIndexTag) (int64, error) {
	s.tagCalls++
	return int64(len(tags)), nil
}

type txIndexMapLake map[string]uint32

func (m txIndexMapLake) TxIndexes(_ context.Context, hashes []string) (map[string]uint32, error) {
	out := map[string]uint32{}
	for _, h := range hashes {
		if v, ok := m[h]; ok {
			out[h] = v
		}
	}
	return out, nil
}

// The tag-tx-index preview may read untagged rows but must never reach the
// UPDATE; -write must.
func TestTagTxIndexWindow_PreviewNeverTags(t *testing.T) {
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	keys := []timescale.TxIndexKey{
		{Ledger: 10, TxHash: "aa", MinTs: t0, MaxTs: t0},
		{Ledger: 10, TxHash: "bb", MinTs: t0, MaxTs: t0},
		{Ledger: 11, TxHash: "cc", MinTs: t0, MaxTs: t0},
	}
	lake := txIndexMapLake{"aa": 1, "bb": 0}

	store := &txIndexKeysStore{keys: keys}
	got, err := tagTxIndexWindow(context.Background(), lake, store, false, t0.Add(-time.Hour), t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("preview returned %v, want nil", err)
	}
	if got != 2 {
		t.Errorf("preview reported %d, want the 2 txs the lake resolved", got)
	}
	if store.tagCalls != 0 {
		t.Errorf("preview called TagTradesTxIndex %d times, want 0", store.tagCalls)
	}

	store = &txIndexKeysStore{keys: keys}
	if _, err := tagTxIndexWindow(context.Background(), lake, store, true, t0.Add(-time.Hour), t0.Add(time.Hour)); err != nil {
		t.Fatalf("write returned %v, want nil", err)
	}
	if store.tagCalls != 1 {
		t.Errorf("write called TagTradesTxIndex %d times, want 1", store.tagCalls)
	}
}

func TestParseTxIndexRange(t *testing.T) {
	if _, _, err := parseTxIndexRange("", "2026-09-30T00:00:00Z"); err == nil {
		t.Error("missing -from accepted")
	}
	if _, _, err := parseTxIndexRange("2026-09-30T00:00:00Z", "2026-09-29T00:00:00Z"); err == nil {
		t.Error("-to before -from accepted")
	}
	from, to, err := parseTxIndexRange("2026-09-29T02:00:00+02:00", "2026-09-30T00:00:00Z")
	if err != nil {
		t.Fatalf("valid range rejected: %v", err)
	}
	if from.Location() != time.UTC || !from.Equal(time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)) || !to.Equal(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("range = [%s, %s), want UTC [2026-09-29, 2026-09-30)", from, to)
	}
}
