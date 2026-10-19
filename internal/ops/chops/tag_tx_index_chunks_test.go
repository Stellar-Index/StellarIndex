// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeTxIndexChunkStore reuses the walk's driver double and models one untagged trade per tx.
type fakeTxIndexChunkStore struct {
	*fakeChunkStore

	trades       []timescale.TxIndexKey
	tagged       map[string]bool
	recompressed string // the chunk TradesChunkCompressed reports compressed
}

func newFakeTxIndexChunkStore(chunks []timescale.TradeChunk, trades ...timescale.TxIndexKey) *fakeTxIndexChunkStore {
	return &fakeTxIndexChunkStore{fakeChunkStore: newFakeChunkStore(chunks), trades: trades, tagged: map[string]bool{}}
}

func (f *fakeTxIndexChunkStore) untagged(from, to time.Time) []timescale.TxIndexKey {
	var out []timescale.TxIndexKey
	for _, k := range f.trades {
		if !k.MinTs.Before(from) && k.MinTs.Before(to) && !f.tagged[k.TxHash] {
			out = append(out, k)
		}
	}
	return out
}

func (f *fakeTxIndexChunkStore) UntaggedTxIndexTradeExists(_ context.Context, from, to time.Time) (bool, error) {
	f.log = append(f.log, fmt.Sprintf("probe [%s, %s)", from.Format("01-02T15"), to.Format("01-02T15")))
	return len(f.untagged(from, to)) > 0, nil
}

func (f *fakeTxIndexChunkStore) UntaggedTxIndexTrades(_ context.Context, from, to time.Time, afterLedger uint32, _ string, _ int) ([]timescale.TxIndexKey, error) {
	var out []timescale.TxIndexKey
	for _, k := range f.untagged(from, to) {
		if k.Ledger > afterLedger {
			out = append(out, k)
		}
	}
	return out, nil
}

func (f *fakeTxIndexChunkStore) TagTradesTxIndex(_ context.Context, from, to time.Time, tags []timescale.TxIndexTag) (int64, error) {
	f.log = append(f.log, fmt.Sprintf("tag [%s, %s)", from.Format("01-02T15"), to.Format("01-02T15")))
	for _, tg := range tags {
		f.tagged[tg.TxHash] = true
	}
	return int64(len(tags)), nil
}

func (f *fakeTxIndexChunkStore) TradesChunkCompressed(_ context.Context, c timescale.TradeChunk) (bool, error) {
	return c.Name == f.recompressed, nil
}

// txIndexChunkFixture: untagged trades in the first and last chunk, none in the middle one, and one past the window.
func txIndexChunkFixture() (*fakeTxIndexChunkStore, txIndexMapLake, time.Time, time.Time) {
	chunks, from, to := threeChunks()
	at := func(day, hour int) time.Time { return time.Date(2026, 1, day, hour, 0, 0, 0, time.UTC) }
	store := newFakeTxIndexChunkStore(chunks,
		timescale.TxIndexKey{Ledger: 10, TxHash: "aa", MinTs: at(6, 3), MaxTs: at(6, 3)},
		timescale.TxIndexKey{Ledger: 20, TxHash: "cc", MinTs: at(17, 5), MaxTs: at(17, 5)},
		timescale.TxIndexKey{Ledger: 30, TxHash: "zz", MinTs: at(20, 1), MaxTs: at(20, 1)},
	)
	lake := txIndexMapLake{
		"aa": {{Ledger: 10, TxIndex: 2}},
		"cc": {{Ledger: 20, TxIndex: 0}},
		"zz": {{Ledger: 30, TxIndex: 1}},
	}
	return store, lake, from, to.AddDate(0, 0, 1)
}

func TestTagTxIndexChunks_TagsInsideEachDirtyChunkAndSkipsCleanOnes(t *testing.T) {
	store, lake, from, to := txIndexChunkFixture()
	_, copts, out := chunkTestOptions(true)

	if err := runTagTxIndexChunks(context.Background(), lake, store, "/etc/stellarindex.toml", true, from, to, time.Hour, nil, copts); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !store.tagged["aa"] || !store.tagged["cc"] || store.tagged["zz"] {
		t.Errorf("tagged = %v, want aa and cc only (zz is past -to)", store.tagged)
	}
	log := strings.Join(store.log, "\n")
	if strings.Contains(log, "decompress _hyper_1_2_chunk") {
		t.Errorf("the clean chunk was decompressed:\n%s", log)
	}
	open := ""
	for _, l := range store.log {
		switch {
		case strings.HasPrefix(l, "decompress "):
			open = strings.TrimPrefix(l, "decompress ")
		case strings.HasPrefix(l, "compress "):
			open = ""
		case strings.HasPrefix(l, "tag ") && open == "":
			t.Errorf("tag outside a decompressed chunk: %s\n%s", l, log)
		}
	}
	wantTeardown(t, store.fakeChunkStore)
	for _, want := range []string{"tagged 1 trade(s)", "nothing to change — skipped", "tag-tx-index: done. tagged trades: 2"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestTagTxIndexChunks_DryRunListsDirtyChunksAndTouchesNone(t *testing.T) {
	store, lake, from, to := txIndexChunkFixture()
	_, copts, out := chunkTestOptions(false)

	if err := runTagTxIndexChunks(context.Background(), lake, store, "/etc/stellarindex.toml", false, from, to, time.Hour, nil, copts); err != nil {
		t.Fatal(err)
	}
	log := strings.Join(store.log, "\n")
	for _, banned := range []string{"decompress", "tag [", "pause", "lock"} {
		if strings.Contains(log, banned) {
			t.Errorf("dry run did %q:\n%s", banned, log)
		}
	}
	for _, want := range []string{
		"DRY RUN: 2 chunk(s) hold untagged on-chain trades, 1 hold none",
		"stellarindex-ops tag-tx-index -config /etc/stellarindex.toml -chunks -from 2026-01-05T00:00:00Z -to 2026-01-19T00:00:00Z -window 1h0m0s -write",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestTagTxIndexChunks_StopsWhenTheChunkIsRecompressedUnderneath(t *testing.T) {
	store, lake, from, to := txIndexChunkFixture()
	store.recompressed = "_hyper_1_1_chunk"
	_, copts, out := chunkTestOptions(true)

	err := runTagTxIndexChunks(context.Background(), lake, store, "/etc/stellarindex.toml", true, from, to, time.Hour, nil, copts)
	if !errors.Is(err, timescale.ErrTradesChunkRecompressed) {
		t.Fatalf("err = %v, want the recompressed-underneath stop", err)
	}
	if !strings.HasPrefix(err.Error(), "tag-tx-index: ") {
		t.Errorf("err names the wrong command: %v", err)
	}
	if len(store.tagged) != 0 || strings.Contains(strings.Join(store.log, "\n"), "_hyper_1_3_chunk") {
		t.Errorf("walk wrote or continued after the guard tripped: tagged=%v\n%s", store.tagged, strings.Join(store.log, "\n"))
	}
	if !strings.Contains(out.String(), "RESUME (") || !strings.Contains(out.String(), "-chunks -from 2026-01-05T00:00:00Z") {
		t.Errorf("no resume line:\n%s", out.String())
	}
	wantTeardown(t, store.fakeChunkStore)
}
