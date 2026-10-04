// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"iter"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/historyarchive"
	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// cap76Checkpoint is the first checkpoint after the CAP-0076 upgrade ledger
// (59,501,299), whose hot-archive rewrite left no trace in ledger meta.
const cap76Checkpoint = 59_501_311

// hotArchiveFixture serves a checkpoint HAS and its hot-archive buckets from
// memory. Bucket hashes are the real sha256 of their bytes, so the SDK's
// per-bucket hash validation stays on.
type hotArchiveFixture struct {
	historyarchive.MockArchive
	has     historyarchive.HistoryArchiveState
	buckets map[historyarchive.Hash][]byte
}

// newHotArchiveFixture places levels[i] at hot-archive level i's curr.
func newHotArchiveFixture(t *testing.T, levels ...[]xdr.HotArchiveBucketEntry) *hotArchiveFixture {
	t.Helper()
	f := &hotArchiveFixture{buckets: map[historyarchive.Hash][]byte{}}
	f.has.CurrentLedger = cap76Checkpoint
	zero := strings.Repeat("0", 64)
	for i := range f.has.HotArchiveBuckets {
		f.has.HotArchiveBuckets[i].Curr = zero
		f.has.HotArchiveBuckets[i].Snap = zero
	}
	for i, entries := range levels {
		var buf bytes.Buffer
		for _, e := range entries {
			if err := xdr.MarshalFramed(&buf, e); err != nil {
				t.Fatalf("marshal bucket entry: %v", err)
			}
		}
		h := historyarchive.Hash(sha256.Sum256(buf.Bytes()))
		f.buckets[h] = buf.Bytes()
		f.has.HotArchiveBuckets[i].Curr = hex.EncodeToString(h[:])
	}
	return f
}

func (f *hotArchiveFixture) GetCheckpointManager() historyarchive.CheckpointManager {
	return historyarchive.NewCheckpointManager(historyarchive.DefaultCheckpointFrequency)
}

func (f *hotArchiveFixture) GetCheckpointHAS(chk uint32) (historyarchive.HistoryArchiveState, error) {
	if chk != cap76Checkpoint {
		return historyarchive.HistoryArchiveState{}, fmt.Errorf("no HAS at %d", chk)
	}
	return f.has, nil
}

func (f *hotArchiveFixture) BucketExists(h historyarchive.Hash) (bool, error) {
	_, ok := f.buckets[h]
	return ok, nil
}

func (f *hotArchiveFixture) BucketSize(h historyarchive.Hash) (int64, error) {
	return int64(len(f.buckets[h])), nil
}

func (f *hotArchiveFixture) GetXdrStreamForHash(h historyarchive.Hash) (*xdr.Stream, error) {
	b, ok := f.buckets[h]
	if !ok {
		return nil, fmt.Errorf("no bucket %s", h)
	}
	return xdr.NewStream(io.NopCloser(bytes.NewReader(b))), nil
}

func hotArchiveMeta() xdr.HotArchiveBucketEntry {
	listType := xdr.BucketListTypeHotArchive
	return xdr.HotArchiveBucketEntry{
		Type: xdr.HotArchiveBucketEntryTypeHotArchiveMetaentry,
		MetaEntry: &xdr.BucketMetadata{
			LedgerVersion: 24,
			Ext:           xdr.BucketMetadataExt{V: 1, BucketListType: &listType},
		},
	}
}

func archived(e xdr.LedgerEntry) xdr.HotArchiveBucketEntry {
	return xdr.HotArchiveBucketEntry{Type: xdr.HotArchiveBucketEntryTypeHotArchiveArchived, ArchivedEntry: &e}
}

func restoredKey(t *testing.T, e xdr.LedgerEntry) xdr.HotArchiveBucketEntry {
	t.Helper()
	k, err := e.LedgerKey()
	if err != nil {
		t.Fatalf("ledger key: %v", err)
	}
	return xdr.HotArchiveBucketEntry{Type: xdr.HotArchiveBucketEntryTypeHotArchiveLive, Key: &k}
}

func persistentData(name string, val uint64, lastModified uint32) xdr.LedgerEntry {
	id := xdr.ContractId{0xca, 0x76}
	sym := xdr.ScSymbol(name)
	v := xdr.Uint64(val)
	return xdr.LedgerEntry{
		LastModifiedLedgerSeq: xdr.Uint32(lastModified),
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.ContractDataEntry{
				Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id},
				Key:        xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
				Durability: xdr.ContractDataDurabilityPersistent,
				Val:        xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &v},
			},
		},
	}
}

func contractCode(code string, lastModified uint32) xdr.LedgerEntry {
	return xdr.LedgerEntry{
		LastModifiedLedgerSeq: xdr.Uint32(lastModified),
		Data: xdr.LedgerEntryData{
			Type:         xdr.LedgerEntryTypeContractCode,
			ContractCode: &xdr.ContractCodeEntry{Hash: xdr.Hash(sha256.Sum256([]byte(code))), Code: []byte(code)},
		},
	}
}

// fakeLake is ledger_entries_current keyed by (entry_type, key_xdr).
type fakeLake map[string]map[string]clickhouse.CurrentEntry

func (l fakeLake) put(t *testing.T, e xdr.LedgerEntry, seq uint32, changeType string) string {
	t.Helper()
	k, err := e.LedgerKey()
	if err != nil {
		t.Fatalf("ledger key: %v", err)
	}
	entryType, key, err := clickhouse.LedgerKeyColumns(k)
	if err != nil {
		t.Fatalf("key columns: %v", err)
	}
	body, err := xdr.MarshalBase64(e)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	if l[entryType] == nil {
		l[entryType] = map[string]clickhouse.CurrentEntry{}
	}
	l[entryType][key] = clickhouse.CurrentEntry{ChangeType: changeType, LedgerSeq: seq, EntryXDR: body}
	return key
}

func (l fakeLake) lookup(_ context.Context, entryType string, keys []string) (map[string]clickhouse.CurrentEntry, error) {
	out := map[string]clickhouse.CurrentEntry{}
	for _, k := range keys {
		if e, ok := l[entryType][k]; ok {
			out[k] = e
		}
	}
	return out, nil
}

// TestReconcileHotArchive_CAP76AmendmentFailsOnAmendedKeys replays the
// CAP-0076 shape: the upgrade rewrote archived entries inside the hot-archive
// bucket list only, so the lake still holds their pre-amendment values. The
// run must fail on exactly the amended keys and on nothing else.
func TestReconcileHotArchive_CAP76AmendmentFailsOnAmendedKeys(t *testing.T) {
	const evicted = 59_000_000
	lake := fakeLake{}
	amendedA := lake.put(t, persistentData("amended_a", 1, evicted), evicted, "updated")
	amendedB := lake.put(t, persistentData("amended_b", 2, evicted), evicted, "updated")
	// Same body under a different lastModified stamp is the same state.
	lake.put(t, persistentData("untouched", 3, evicted-7), evicted, "updated")
	lake.put(t, contractCode("wasm", evicted), evicted, "created")
	lake.put(t, persistentData("touched_later", 60, cap76Checkpoint+5), cap76Checkpoint+5, "restored")
	lake.put(t, persistentData("restored", 4, cap76Checkpoint-3), cap76Checkpoint-3, "restored")

	newest := []xdr.HotArchiveBucketEntry{
		hotArchiveMeta(),
		archived(persistentData("amended_a", 101, evicted)),
		archived(persistentData("amended_b", 102, evicted)),
		restoredKey(t, persistentData("restored", 4, evicted)),
	}
	older := []xdr.HotArchiveBucketEntry{
		hotArchiveMeta(),
		archived(persistentData("amended_b", 2, evicted)), // shadowed by the amended copy
		archived(persistentData("untouched", 3, evicted)),
		archived(persistentData("restored", 4, evicted)), // shadowed by the restore
		archived(persistentData("never_ingested", 5, evicted)),
		archived(persistentData("touched_later", 6, evicted)),
		archived(contractCode("wasm", evicted)),
	}
	arch := newHotArchiveFixture(t, newest, older)

	n, err := reconcileHotArchive(context.Background(), openFixture(arch), cap76Checkpoint, lake.lookup)
	if err != nil {
		t.Fatalf("reconcileHotArchive: %v", err)
	}
	got := hotArchiveCounts{checkpoint: n.checkpoint, archived: n.archived, matched: n.matched, newer: n.newer, absent: n.absent, mismatches: n.mismatches}
	want := hotArchiveCounts{checkpoint: cap76Checkpoint, archived: 6, matched: 2, newer: 1, absent: 1, mismatches: 2}
	if got.checkpoint != want.checkpoint || got.archived != want.archived || got.matched != want.matched ||
		got.newer != want.newer || got.absent != want.absent || got.mismatches != want.mismatches {
		t.Fatalf("counts = %+v, want %+v", got, want)
	}
	var keys []string
	for _, s := range n.sample {
		keys = append(keys, strings.TrimSuffix(strings.Fields(s)[1], ":"))
	}
	slices.Sort(keys)
	wantKeys := []string{amendedA, amendedB}
	slices.Sort(wantKeys)
	if !slices.Equal(keys, wantKeys) {
		t.Fatalf("mismatched keys = %v, want the amended keys %v", keys, wantKeys)
	}
	if n.failures() != 2 {
		t.Fatalf("failures() = %d, want 2", n.failures())
	}
	res := networkStateResult{hot: &n}
	if prom := res.renderProm(time.Unix(1_700_000_000, 0)); !strings.Contains(prom, `stellarindex_network_state_verify_failures{check="hot_archive"} 2`) {
		t.Fatalf("prom missing the hot_archive failure series:\n%s", prom)
	}
}

func openFixture(arch historyarchive.ArchiveInterface) func(context.Context) iter.Seq2[xdr.LedgerEntry, error] {
	return func(ctx context.Context) iter.Seq2[xdr.LedgerEntry, error] {
		return ingest.NewHotArchiveIterator(ctx, arch, cap76Checkpoint)
	}
}

// TestReconcileHotArchive_LookupErrorReturnsOnLargeArchive: an archive larger
// than the SDK's read buffer keeps its stream goroutine blocked on a send, so a
// lookup error must still return promptly rather than wait on that goroutine.
func TestReconcileHotArchive_LookupErrorReturnsOnLargeArchive(t *testing.T) {
	entries := []xdr.HotArchiveBucketEntry{hotArchiveMeta()}
	for i := range 50_000 + 2*hotArchiveBatch {
		entries = append(entries, archived(persistentData(fmt.Sprintf("k%d", i), uint64(i), 1)))
	}
	arch := newHotArchiveFixture(t, entries)
	errLake := errors.New("clickhouse: FINAL timeout")
	lookup := func(context.Context, string, []string) (map[string]clickhouse.CurrentEntry, error) {
		return nil, errLake
	}

	done := make(chan error, 1)
	go func() {
		_, err := reconcileHotArchive(context.Background(), openFixture(arch), cap76Checkpoint, lookup)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errLake) {
			t.Fatalf("reconcileHotArchive err = %v, want the lookup error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reconcileHotArchive did not return within 10s of a lookup error")
	}
}

func TestHotArchiveClassify_RemovedAndUndecodableAreMismatches(t *testing.T) {
	e := persistentData("k", 1, 10)
	body, err := e.Data.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	a := archivedEntry{key: "K", data: body}
	n := hotArchiveCounts{checkpoint: 100}
	n.classify("contract_data", a, clickhouse.CurrentEntry{ChangeType: "removed", LedgerSeq: 50}, true)
	n.classify("contract_data", a, clickhouse.CurrentEntry{ChangeType: "updated", LedgerSeq: 50, EntryXDR: "not-xdr"}, true)
	if n.mismatches != 2 || n.matched != 0 {
		t.Fatalf("counts = %+v, want 2 mismatches", n)
	}
}

// A run that archived entries but compared none (all absent) must fail.
func TestHotArchiveCounts_VacuousRunFails(t *testing.T) {
	n := hotArchiveCounts{archived: 3, absent: 2, newer: 1}
	if n.failures() != 1 {
		t.Fatalf("failures() = %d, want 1", n.failures())
	}
	if (&hotArchiveCounts{}).failures() != 0 {
		t.Fatal("an empty hot archive is not a failure")
	}
}

func TestPickHotArchiveCheckpoint(t *testing.T) {
	cm := historyarchive.NewCheckpointManager(historyarchive.DefaultCheckpointFrequency)
	cases := []struct {
		name    string
		want    uint64
		lake    uint32
		arch    uint32
		seq     uint32
		wantErr bool
	}{
		{"newest below both tips", 0, cap76Checkpoint + 9, cap76Checkpoint + 200, cap76Checkpoint, false},
		{"lake behind archive", 0, cap76Checkpoint - 1, cap76Checkpoint + 200, cap76Checkpoint - 64, false},
		{"explicit", cap76Checkpoint, cap76Checkpoint, cap76Checkpoint, cap76Checkpoint, false},
		{"not a checkpoint", cap76Checkpoint + 1, cap76Checkpoint + 500, cap76Checkpoint + 500, 0, true},
		{"above the lake tip", cap76Checkpoint + 64, cap76Checkpoint + 10, cap76Checkpoint + 500, 0, true},
	}
	for _, c := range cases {
		seq, err := pickHotArchiveCheckpoint(cm, c.want, c.lake, c.arch)
		if (err != nil) != c.wantErr || seq != c.seq {
			t.Errorf("%s: got (%d, %v), want (%d, err=%v)", c.name, seq, err, c.seq, c.wantErr)
		}
	}
}

func TestParseNetworkChecks(t *testing.T) {
	got, err := parseNetworkChecks(" lumens , hotarchive ")
	if err != nil || !got[networkCheckLumens] || !got[networkCheckHotArchive] {
		t.Fatalf("both checks: got %v, %v", got, err)
	}
	for _, bad := range []string{"", " , ", "hotarchive,bogus"} {
		if _, err := parseNetworkChecks(bad); err == nil {
			t.Errorf("parseNetworkChecks(%q) accepted", bad)
		}
	}
}

func TestNetworkStateResult_LumenResidualFails(t *testing.T) {
	tally := &clickhouse.LumenTally{
		TotalCoins: 1_000, FeePool: 10,
		Accounts: big.NewInt(985), ClaimableBalances: new(big.Int),
		LiquidityPools: new(big.Int), ContractBalances: new(big.Int),
	}
	res := networkStateResult{lumens: tally}
	if res.failures() != 1 {
		t.Fatalf("failures() = %d with residual %s, want 1", res.failures(), tally.Residual())
	}
	prom := res.renderProm(time.Unix(1_700_000_000, 0))
	if !strings.Contains(prom, `stellarindex_network_state_verify_failures{check="lumens"} 1`) ||
		strings.Contains(prom, `check="hot_archive"`) {
		t.Fatalf("prom must carry only the lumens series:\n%s", prom)
	}
	tally.Accounts = big.NewInt(990)
	if res.failures() != 0 {
		t.Fatalf("failures() = %d with zero residual", res.failures())
	}
}
