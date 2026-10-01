package ingest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type txIndexKeysStore struct {
	keys          []timescale.TxIndexKey
	chunks        []timescale.TradeChunk
	untaggedCalls int
	tagCalls      int
}

func (s *txIndexKeysStore) TradesChunksInRange(_ context.Context, from, to time.Time) ([]timescale.TradeChunk, error) {
	var out []timescale.TradeChunk
	for _, c := range s.chunks {
		if c.RangeStart.Before(to) && c.RangeEnd.After(from) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *txIndexKeysStore) UntaggedTxIndexTrades(_ context.Context, _, _ time.Time, afterLedger uint32, _ string, _ int) ([]timescale.TxIndexKey, error) {
	s.untaggedCalls++
	if afterLedger > 0 {
		return nil, nil
	}
	return s.keys, nil
}

func (s *txIndexKeysStore) TagTradesTxIndex(_ context.Context, _, _ time.Time, tags []timescale.TxIndexTag) (int64, error) {
	s.tagCalls++
	return int64(len(tags)), nil
}

type txIndexMapLake map[string][]clickhouse.TxLedgerIndex

func (m txIndexMapLake) TxLedgerIndexes(_ context.Context, hashes []string) (map[string][]clickhouse.TxLedgerIndex, error) {
	out := map[string][]clickhouse.TxLedgerIndex{}
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
	lake := txIndexMapLake{"aa": {{Ledger: 10, TxIndex: 1}}, "bb": {{Ledger: 10, TxIndex: 0}}}

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

// An UPDATE into a compressed chunk decompresses it inside the transaction;
// -write over a range touching one must refuse before walking anything,
// while the preview still runs.
func TestRunTagTxIndex_WriteRefusesCompressedChunk(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	keys := []timescale.TxIndexKey{{Ledger: 10, TxHash: "aa", MinTs: t0.Add(time.Hour), MaxTs: t0.Add(time.Hour)}}
	lake := txIndexMapLake{"aa": {{Ledger: 10, TxIndex: 1}}}
	chunks := []timescale.TradeChunk{
		{Schema: "_timescaledb_internal", Name: "_hyper_1_1_chunk", RangeStart: t0, RangeEnd: t0.Add(24 * time.Hour)},
		{Schema: "_timescaledb_internal", Name: "_hyper_1_2_chunk", RangeStart: t0.Add(24 * time.Hour), RangeEnd: t0.Add(48 * time.Hour), Compressed: true},
	}

	store := &txIndexKeysStore{keys: keys, chunks: chunks}
	_, err := runTagTxIndex(context.Background(), lake, store, true, t0, t0.Add(48*time.Hour), time.Hour)
	if !errors.Is(err, errTxIndexCompressedChunk) {
		t.Fatalf("write over a compressed chunk returned %v, want errTxIndexCompressedChunk", err)
	}
	if !strings.Contains(err.Error(), "_hyper_1_2_chunk") || strings.Contains(err.Error(), "_hyper_1_1_chunk") {
		t.Errorf("refusal %q must name the compressed chunk and only it", err)
	}
	if store.untaggedCalls != 0 || store.tagCalls != 0 {
		t.Errorf("refused write walked: UntaggedTxIndexTrades=%d TagTradesTxIndex=%d, want 0/0", store.untaggedCalls, store.tagCalls)
	}

	store = &txIndexKeysStore{keys: keys, chunks: chunks}
	if _, err := runTagTxIndex(context.Background(), lake, store, false, t0, t0.Add(48*time.Hour), 24*time.Hour); err != nil {
		t.Fatalf("preview over a compressed chunk returned %v, want nil", err)
	}
	if store.tagCalls != 0 {
		t.Errorf("preview called TagTradesTxIndex %d times, want 0", store.tagCalls)
	}

	store = &txIndexKeysStore{keys: keys, chunks: chunks}
	if _, err := runTagTxIndex(context.Background(), lake, store, true, t0, t0.Add(24*time.Hour), 24*time.Hour); err != nil {
		t.Fatalf("write over only the uncompressed chunk returned %v, want nil", err)
	}
	if store.tagCalls != 1 {
		t.Errorf("write over the uncompressed chunk called TagTradesTxIndex %d times, want 1", store.tagCalls)
	}
}

// The policy can compress a chunk mid-run; the per-window recheck must stop
// the walk before the window that now touches it.
func TestRunTagTxIndex_WriteRechecksEachWindow(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	store := &compressingChunkStore{txIndexKeysStore: txIndexKeysStore{chunks: []timescale.TradeChunk{
		{Schema: "_timescaledb_internal", Name: "_hyper_1_1_chunk", RangeStart: t0, RangeEnd: t0.Add(time.Hour)},
		{Schema: "_timescaledb_internal", Name: "_hyper_1_2_chunk", RangeStart: t0.Add(time.Hour), RangeEnd: t0.Add(2 * time.Hour)},
	}}}
	_, err := runTagTxIndex(context.Background(), txIndexMapLake{}, store, true, t0, t0.Add(2*time.Hour), time.Hour)
	if !errors.Is(err, errTxIndexCompressedChunk) {
		t.Fatalf("write after a mid-run compression returned %v, want errTxIndexCompressedChunk", err)
	}
	if store.untaggedCalls != 1 {
		t.Errorf("walked %d windows, want only the first", store.untaggedCalls)
	}
}

// compressingChunkStore compresses the second chunk once the first window
// has been walked.
type compressingChunkStore struct {
	txIndexKeysStore
}

func (s *compressingChunkStore) TradesChunksInRange(ctx context.Context, from, to time.Time) ([]timescale.TradeChunk, error) {
	if s.untaggedCalls > 0 {
		s.chunks[1].Compressed = true
	}
	return s.txIndexKeysStore.TradesChunksInRange(ctx, from, to)
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
