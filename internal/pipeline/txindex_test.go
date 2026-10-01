package pipeline

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeTxIndexRow is one trades row as the tx-index walk sees it.
type fakeTxIndexRow struct {
	source  string
	ledger  uint32
	txHash  string
	ts      time.Time
	txIndex *uint32
}

// fakeTxIndexStore applies the same predicates as the SQL in
// timescale/tx_index.go: ts window, ledger > 0, tx_index IS NULL, keyset.
type fakeTxIndexStore struct {
	rows       []*fakeTxIndexRow
	tagBounds  [][2]time.Time
	tagCalls   int
	pageCalls  int
	failTagErr error
}

func (f *fakeTxIndexStore) UntaggedTxIndexTrades(_ context.Context, from, to time.Time, afterLedger uint32, afterHash string, limit int) ([]timescale.TxIndexKey, error) {
	f.pageCalls++
	byKey := map[[2]any]*timescale.TxIndexKey{}
	for _, r := range f.rows {
		if r.ts.Before(from) || !r.ts.Before(to) || r.ledger == 0 || r.txIndex != nil {
			continue
		}
		if r.ledger < afterLedger || (r.ledger == afterLedger && r.txHash <= afterHash) {
			continue
		}
		k := [2]any{r.ledger, r.txHash}
		if cur, ok := byKey[k]; ok {
			if r.ts.Before(cur.MinTs) {
				cur.MinTs = r.ts
			}
			if r.ts.After(cur.MaxTs) {
				cur.MaxTs = r.ts
			}
			continue
		}
		byKey[k] = &timescale.TxIndexKey{Ledger: r.ledger, TxHash: r.txHash, MinTs: r.ts, MaxTs: r.ts}
	}
	out := make([]timescale.TxIndexKey, 0, len(byKey))
	for _, k := range byKey {
		out = append(out, *k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ledger != out[j].Ledger {
			return out[i].Ledger < out[j].Ledger
		}
		return out[i].TxHash < out[j].TxHash
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeTxIndexStore) TagTradesTxIndex(_ context.Context, from, to time.Time, tags []timescale.TxIndexTag) (int64, error) {
	f.tagCalls++
	if f.failTagErr != nil {
		return 0, f.failTagErr
	}
	f.tagBounds = append(f.tagBounds, [2]time.Time{from, to})
	var n int64
	for _, tag := range tags {
		for _, r := range f.rows {
			if r.ts.Before(from) || !r.ts.Before(to) || r.ledger == 0 || r.txIndex != nil {
				continue
			}
			if r.ledger == tag.Ledger && r.txHash == tag.TxHash {
				v := tag.TxIndex
				r.txIndex = &v
				n++
			}
		}
	}
	return n, nil
}

type fakeTxIndexLake struct {
	order map[string]uint32
	err   error
	reads [][]string
}

func (f *fakeTxIndexLake) TxIndexes(_ context.Context, hashes []string) (map[string]uint32, error) {
	f.reads = append(f.reads, append([]string(nil), hashes...))
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]uint32{}
	for _, h := range hashes {
		if v, ok := f.order[h]; ok {
			out[h] = v
		}
	}
	return out, nil
}

func txIndexOf(t *testing.T, r *fakeTxIndexRow) (uint32, bool) {
	t.Helper()
	if r.txIndex == nil {
		return 0, false
	}
	return *r.txIndex, true
}

// Two same-ledger txs of one pair must come out in APPLY order, which here is
// the reverse of their tx_hash order; off-chain and out-of-window rows stay NULL.
func TestTagTxIndexWindow_TagsEveryOnChainSourceInApplyOrder(t *testing.T) {
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	sdexA := &fakeTxIndexRow{source: "sdex", ledger: 100, txHash: "aa", ts: t0}
	sdexF := &fakeTxIndexRow{source: "sdex", ledger: 100, txHash: "ff", ts: t0}
	soroswap := &fakeTxIndexRow{source: "soroswap", ledger: 101, txHash: "bb", ts: t0.Add(5 * time.Second)}
	offChain := &fakeTxIndexRow{source: "binance", ledger: 0, txHash: "cc", ts: t0}
	outside := &fakeTxIndexRow{source: "sdex", ledger: 90, txHash: "dd", ts: t0.Add(-time.Hour)}
	store := &fakeTxIndexStore{rows: []*fakeTxIndexRow{sdexA, sdexF, soroswap, offChain, outside}}
	lake := &fakeTxIndexLake{order: map[string]uint32{"ff": 0, "aa": 1, "bb": 3, "cc": 7, "dd": 2}}

	tagged, err := TagTxIndexWindow(context.Background(), lake, store, t0.Add(-time.Minute), t0.Add(time.Minute), TxIndexPageSize)
	if err != nil {
		t.Fatalf("TagTxIndexWindow: %v", err)
	}
	if tagged != 3 {
		t.Errorf("tagged = %d, want 3 (two sdex + one soroswap)", tagged)
	}
	gotF, okF := txIndexOf(t, sdexF)
	gotA, okA := txIndexOf(t, sdexA)
	if !okF || !okA || gotF != 0 || gotA != 1 {
		t.Errorf("same-ledger sdex tx_index: ff=(%d,%v) aa=(%d,%v), want ff=0 aa=1", gotF, okF, gotA, okA)
	}
	if got, ok := txIndexOf(t, soroswap); !ok || got != 3 {
		t.Errorf("soroswap tx_index = (%d,%v), want 3", got, ok)
	}
	if _, ok := txIndexOf(t, offChain); ok {
		t.Error("off-chain (ledger 0) row was tagged")
	}
	if _, ok := txIndexOf(t, outside); ok {
		t.Error("row outside [from, to) was tagged")
	}
	for _, h := range lake.reads[0] {
		if h == "cc" || h == "dd" {
			t.Errorf("lake asked for %q, which is off-chain or outside the window", h)
		}
	}
	if len(store.tagBounds) != 1 {
		t.Fatalf("tag calls = %d, want 1", len(store.tagBounds))
	}
	if b := store.tagBounds[0]; b[0].After(t0) || !b[1].After(t0.Add(5*time.Second)) {
		t.Errorf("tag ts bound [%s, %s) does not cover the tagged rows [%s, %s]", b[0], b[1], t0, t0.Add(5*time.Second))
	}

	// First-wins: a second pass changes nothing and never reaches the UPDATE.
	again, err := TagTxIndexWindow(context.Background(), lake, store, t0.Add(-time.Minute), t0.Add(time.Minute), TxIndexPageSize)
	if err != nil || again != 0 || store.tagCalls != 1 {
		t.Errorf("second pass: tagged=%d err=%v tagCalls=%d, want 0, nil, 1", again, err, store.tagCalls)
	}
}

// A full page of txs the lake cannot resolve must not starve the txs after it.
func TestTagTxIndexWindow_WalksPastUnresolvedPages(t *testing.T) {
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	unknown1 := &fakeTxIndexRow{source: "sdex", ledger: 100, txHash: "a1", ts: t0}
	unknown2 := &fakeTxIndexRow{source: "sdex", ledger: 100, txHash: "a2", ts: t0}
	known := &fakeTxIndexRow{source: "phoenix", ledger: 102, txHash: "b1", ts: t0.Add(10 * time.Second)}
	store := &fakeTxIndexStore{rows: []*fakeTxIndexRow{unknown1, unknown2, known}}
	lake := &fakeTxIndexLake{order: map[string]uint32{"b1": 4}}

	tagged, err := TagTxIndexWindow(context.Background(), lake, store, t0.Add(-time.Minute), t0.Add(time.Minute), 2)
	if err != nil {
		t.Fatalf("TagTxIndexWindow: %v", err)
	}
	if got, ok := txIndexOf(t, known); tagged != 1 || !ok || got != 4 {
		t.Errorf("tagged=%d known=(%d,%v), want 1 and tx_index 4 — the walk stopped at the unresolved page", tagged, got, ok)
	}
	if store.pageCalls != 2 {
		t.Errorf("page reads = %d, want 2 (a full page, then the short last page)", store.pageCalls)
	}
}

func TestTagTxIndexWindow_LakeErrorTagsNothing(t *testing.T) {
	t0 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	row := &fakeTxIndexRow{source: "sdex", ledger: 100, txHash: "aa", ts: t0}
	store := &fakeTxIndexStore{rows: []*fakeTxIndexRow{row}}
	lake := &fakeTxIndexLake{err: errors.New("MEMORY_LIMIT_EXCEEDED")}

	if _, err := TagTxIndexWindow(context.Background(), lake, store, t0.Add(-time.Minute), t0.Add(time.Minute), TxIndexPageSize); err == nil {
		t.Fatal("lake failure returned nil error")
	}
	if store.tagCalls != 0 {
		t.Errorf("tag calls = %d after a failed lake read, want 0", store.tagCalls)
	}
}

func TestTagTxIndexWindow_NothingUntaggedSkipsTheLake(t *testing.T) {
	store := &fakeTxIndexStore{}
	lake := &fakeTxIndexLake{}
	now := time.Now()
	if _, err := TagTxIndexWindow(context.Background(), lake, store, now.Add(-time.Minute), now, TxIndexPageSize); err != nil {
		t.Fatalf("TagTxIndexWindow: %v", err)
	}
	if len(lake.reads) != 0 {
		t.Errorf("lake reads = %d with nothing untagged, want 0", len(lake.reads))
	}
}
