// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func TestReadLedgerList_SortsDedupesAndSkipsComments(t *testing.T) {
	in := "# upgrade ledgers\n\n300\n  7  # protocol 10\n300\n42\n"
	got, err := readLedgerList(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint32{7, 42, 300}; !reflect.DeepEqual(got, want) {
		t.Fatalf("readLedgerList = %v, want %v", got, want)
	}
}

func TestReadLedgerList_Refusals(t *testing.T) {
	over := make([]string, maxTargetedLedgers+1)
	for i := range over {
		over[i] = fmt.Sprint(i + 2)
	}
	for name, tc := range map[string]struct{ in, want string }{
		"empty":         {"", "no ledger sequences"},
		"comments only": {"# nothing\n\n", "no ledger sequences"},
		"genesis":       {"5\n1\n", "below 2"},
		"zero":          {"0\n", "below 2"},
		"not a number":  {"12a\n", "not a ledger sequence"},
		"negative":      {"-3\n", "not a ledger sequence"},
		"over uint32":   {"4294967296\n", "not a ledger sequence"},
		"over the cap":  {strings.Join(over, "\n"), "more than the 2000"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readLedgerList(strings.NewReader(tc.in))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	// The cap counts distinct ledgers: a list repeated past it is still a short list.
	if _, err := readLedgerList(strings.NewReader(strings.Repeat("9\n", maxTargetedLedgers+5))); err != nil {
		t.Fatalf("a repeated sequence counted toward the cap: %v", err)
	}
}

func TestResolveBackfillPlan_LedgersExcludesARange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledgers.txt")
	if err := os.WriteFile(path, []byte("10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ from, to uint }{{2, 0}, {0, 20}, {2, 20}} {
		if _, err := resolveBackfillPlan(tc.from, tc.to, path, 1); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
			t.Errorf("-from %d -to %d with -ledgers: err = %v, want a refusal", tc.from, tc.to, err)
		}
	}
	if _, err := resolveBackfillPlan(0, 0, "", 1); err == nil {
		t.Error("no -from/-to and no -ledgers was accepted")
	}
	plan, err := resolveBackfillPlan(0, 0, path, 4)
	if err != nil {
		t.Fatal(err)
	}
	if plan.want() != 1 || plan.lo != 10 || plan.hi != 10 || !plan.writes(10) || plan.writes(11) {
		t.Fatalf("plan = %+v, want exactly ledger 10", plan)
	}
}

func TestCoalesceLedgers(t *testing.T) {
	got := coalesceLedgers([]uint32{5, 6, 7, 60, 200, 201, 1000})
	want := []opsutil.RangeChunk{{From: 5, To: 60}, {From: 200, To: 201}, {From: 1000, To: 1000}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("coalesceLedgers = %v, want %v", got, want)
	}
	// 64 apart cannot share a 64-ledger object, so it streams separately.
	if got := coalesceLedgers([]uint32{100, 164}); len(got) != 2 {
		t.Fatalf("coalesceLedgers(100,164) = %v, want two ranges", got)
	}
}

func TestAssignRanges_EveryRangeOnceWithinParallel(t *testing.T) {
	ranges := []opsutil.RangeChunk{{From: 2, To: 2}, {From: 9, To: 9}, {From: 90, To: 91}}
	shares := assignRanges(ranges, 2)
	if len(shares) != 2 {
		t.Fatalf("%d shares, want 2", len(shares))
	}
	if got := slices.Concat(shares...); len(got) != len(ranges) {
		t.Fatalf("shares cover %v, want every range once", got)
	}
	if got := assignRanges(ranges, 8); len(got) != 3 {
		t.Fatalf("%d shares for 3 ranges at -parallel 8, want 3", len(got))
	}
}

func TestListModeAllowed(t *testing.T) {
	if err := listModeAllowed(true, true, false); err != nil {
		t.Fatalf("dry-run list refused: %v", err)
	}
	if err := listModeAllowed(false, false, true); err != nil {
		t.Fatalf("an ordinary run was refused by the list-mode check: %v", err)
	}
	for name, err := range map[string]error{
		"write":        listModeAllowed(true, false, false),
		"changes-only": listModeAllowed(true, true, true),
	} {
		if err == nil {
			t.Errorf("-list-upgrade-ledgers with %s was accepted", name)
		}
	}
}

// targetedLCM is a tx-less ledger; upgraded adds one upgrade carrying an account state/updated pair.
func targetedLCM(seq uint32, upgraded bool) sdkxdr.LedgerCloseMeta {
	v0 := &sdkxdr.LedgerCloseMetaV0{
		LedgerHeader: sdkxdr.LedgerHeaderHistoryEntry{Header: sdkxdr.LedgerHeader{LedgerSeq: sdkxdr.Uint32(seq), LedgerVersion: 19}},
	}
	if upgraded {
		key := sdkxdr.Uint256{1}
		entry := func(bal int64) *sdkxdr.LedgerEntry {
			return &sdkxdr.LedgerEntry{Data: sdkxdr.LedgerEntryData{
				Type:    sdkxdr.LedgerEntryTypeAccount,
				Account: &sdkxdr.AccountEntry{AccountId: sdkxdr.AccountId{Type: sdkxdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &key}, Balance: sdkxdr.Int64(bal)},
			}}
		}
		reserve := sdkxdr.Uint32(5_000_000)
		v0.UpgradesProcessing = []sdkxdr.UpgradeEntryMeta{{
			Upgrade: sdkxdr.LedgerUpgrade{Type: sdkxdr.LedgerUpgradeTypeLedgerUpgradeBaseReserve, NewBaseReserve: &reserve},
			Changes: sdkxdr.LedgerEntryChanges{
				{Type: sdkxdr.LedgerEntryChangeTypeLedgerEntryState, State: entry(100)},
				{Type: sdkxdr.LedgerEntryChangeTypeLedgerEntryUpdated, Updated: entry(90)},
			},
		}}
	}
	return sdkxdr.LedgerCloseMeta{V: 0, V0: v0}
}

// fakeGalexie streams targetedLCM for every requested range and records what each walk fetched and wrote.
type fakeGalexie struct {
	mu       sync.Mutex
	upgraded map[uint32]bool
	streamed []opsutil.RangeChunk
	written  []clickhouse.LedgerExtract
	flushes  int
}

func (f *fakeGalexie) stream(_ context.Context, _ ledgerstream.Config, from, to uint32, cb func(sdkxdr.LedgerCloseMeta) error) error {
	f.mu.Lock()
	f.streamed = append(f.streamed, opsutil.RangeChunk{From: from, To: to})
	f.mu.Unlock()
	for s := from; s <= to; s++ {
		if err := cb(targetedLCM(s, f.upgraded[s])); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeGalexie) Add(_ context.Context, e clickhouse.LedgerExtract) error {
	f.written = append(f.written, e)
	return nil
}
func (f *fakeGalexie) Flush(context.Context) error { f.flushes++; return nil }
func (f *fakeGalexie) Close(context.Context) error { return nil }

// changeIdentity is ledger_entry_changes' ReplacingMergeTree ORDER BY: rows sharing it collapse to one.
type changeIdentity struct {
	ledger  uint32
	txHash  string
	opIndex int32
	change  uint32
}

// collapse folds rows the way an identity-collapsed (FINAL) read does, keeping each identity's last row.
func collapse(rows []clickhouse.LedgerExtract) map[changeIdentity]clickhouse.LedgerEntryChangeRow {
	out := map[changeIdentity]clickhouse.LedgerEntryChangeRow{}
	for _, e := range rows {
		for _, c := range e.Changes {
			out[changeIdentity{c.LedgerSeq, c.TxHash, c.OpIndex, c.ChangeIndex}] = c
		}
	}
	return out
}

func runTargeted(t *testing.T, lake *fakeGalexie, list string) backfillPlan {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledgers.txt")
	if err := os.WriteFile(path, []byte(list), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := resolveBackfillPlan(0, 0, path, 1)
	if err != nil {
		t.Fatal(err)
	}
	var recorded int64
	w := backfillWorker{
		stream:      lake.stream,
		openSink:    func(context.Context) (extractSink, error) { return lake, nil },
		changesOnly: true, plan: plan, loss: &extractLoss{},
		logProgress: func(int, uint32, map[string]int) { recorded++ },
	}
	for i, share := range assignRanges(plan.ranges, 1) {
		if err := w.run(context.Background(), i, share); err != nil {
			t.Fatal(err)
		}
	}
	if err := plan.coverage(recorded, "bucket", false); err != nil {
		t.Fatalf("targeted run reported incomplete: %v", err)
	}
	if lerr := w.loss.err(); lerr != nil {
		t.Fatal(lerr)
	}
	return plan
}

// Only listed ledgers reach the Sink, though their object-mates are streamed; each listed upgrade ledger carries its
// upgrade rows; one Sink flush per worker.
func TestBackfillWorker_TargetedWritesOnlyListedLedgers(t *testing.T) {
	lake := &fakeGalexie{upgraded: map[uint32]bool{10: true, 40: true, 500: true}}
	runTargeted(t, lake, "40\n10\n500\n")

	if want := []opsutil.RangeChunk{{From: 10, To: 40}, {From: 500, To: 500}}; !reflect.DeepEqual(lake.streamed, want) {
		t.Fatalf("streamed %v, want %v (one fetch per shared object span)", lake.streamed, want)
	}
	var got []uint32
	for _, e := range lake.written {
		got = append(got, e.Ledger.LedgerSeq)
		if len(e.Txs)+len(e.Ops)+len(e.Events) != 0 {
			t.Errorf("ledger %d: -changes-only wrote non-change rows", e.Ledger.LedgerSeq)
		}
		upgradeRows := 0
		for _, c := range e.Changes {
			if c.LedgerSeq != e.Ledger.LedgerSeq {
				t.Errorf("ledger %d carried a row for ledger %d", e.Ledger.LedgerSeq, c.LedgerSeq)
			}
			if c.TxHash == "" && c.OpIndex == -2 {
				upgradeRows++
			}
		}
		if upgradeRows != 2 {
			t.Errorf("ledger %d: %d upgrade rows, want 2", e.Ledger.LedgerSeq, upgradeRows)
		}
	}
	if want := []uint32{10, 40, 500}; !reflect.DeepEqual(got, want) {
		t.Fatalf("wrote ledgers %v, want exactly %v", got, want)
	}
	if lake.flushes != 1 {
		t.Errorf("%d flushes, want 1 (one Sink per worker)", lake.flushes)
	}
}

// Re-running the list, or listing a ledger that already holds its upgrade rows, yields the same identities with the
// same payloads, so the collapsed table is unchanged and no unlisted ledger gains a row.
func TestBackfillWorker_TargetedRerunIsIdempotent(t *testing.T) {
	lake := &fakeGalexie{upgraded: map[uint32]bool{10: true, 12: true}}
	runTargeted(t, lake, "10\n12\n")
	first := collapse(lake.written)
	firstRows := len(lake.written)
	runTargeted(t, lake, "12\n10\n10\n")

	if len(lake.written) != 2*firstRows {
		t.Fatalf("second run wrote %d extracts, want %d", len(lake.written)-firstRows, firstRows)
	}
	again := collapse(lake.written)
	if !reflect.DeepEqual(first, again) {
		t.Fatalf("collapsed content changed on re-run:\nfirst %v\nagain %v", first, again)
	}
	for id := range again {
		if id.ledger != 10 && id.ledger != 12 {
			t.Errorf("unlisted ledger %d gained a row", id.ledger)
		}
	}
}

func TestListUpgradeLedgers_ReportsOnlyUpgradeLedgers(t *testing.T) {
	lake := &fakeGalexie{upgraded: map[uint32]bool{3: true, 17: true, 18: true}}
	var seen atomic.Int64
	plan := backfillPlan{ranges: opsutil.SplitRange(2, 30, 3), lo: 2, hi: 30}
	found, err := listUpgradeLedgers(context.Background(), lake.stream, ledgerstream.Config{},
		plan, 3, func(int, uint32, map[string]int) { seen.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	want := []upgradeLedger{{3, 1, 2}, {17, 1, 2}, {18, 1, 2}}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("listUpgradeLedgers = %v, want %v", found, want)
	}
	if seen := seen.Load(); seen != 29 {
		t.Fatalf("walked %d ledgers, want 29 (every ledger counts toward coverage)", seen)
	}
	var buf bytes.Buffer
	if err := writeLedgerList(&buf, found, plan.label(), "b"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "17 # upgrades=1 changes=2\n") {
		t.Fatalf("list line lacks the expected-row annotation:\n%s", buf.String())
	}
	back, err := readLedgerList(&buf)
	if err != nil || !reflect.DeepEqual(back, []uint32{3, 17, 18}) {
		t.Fatalf("list output does not round-trip as a -ledgers file: %v, %v", back, err)
	}
}

// Over a -ledgers list the walk reports and counts only listed ledgers, so it can annotate a header-derived list.
func TestListUpgradeLedgers_ListedOnly(t *testing.T) {
	lake := &fakeGalexie{upgraded: map[uint32]bool{3: true, 4: true, 9: true}}
	plan := backfillPlan{ranges: coalesceLedgers([]uint32{3, 9}), keep: map[uint32]struct{}{3: {}, 9: {}}, lo: 3, hi: 9}
	var seen atomic.Int64
	found, err := listUpgradeLedgers(context.Background(), lake.stream, ledgerstream.Config{}, plan, 1,
		func(int, uint32, map[string]int) { seen.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	if want := []upgradeLedger{{3, 1, 2}, {9, 1, 2}}; !reflect.DeepEqual(found, want) {
		t.Fatalf("listUpgradeLedgers = %v, want %v (ledger 4 is not listed)", found, want)
	}
	if err := plan.coverage(seen.Load(), "b", false); err != nil {
		t.Fatalf("coverage over the listed ledgers: %v", err)
	}
}

func TestTargetedCoverage_ShortListIsAHardError(t *testing.T) {
	plan := backfillPlan{keep: map[uint32]struct{}{5: {}, 9: {}}, lo: 5, hi: 9}
	if plan.coverage(1, "b", false) == nil {
		t.Fatal("1 of 2 listed ledgers written reported complete")
	}
	if plan.coverage(2, "b", true) == nil {
		t.Fatal("an interrupted targeted run reported complete")
	}
	if err := plan.coverage(2, "b", false); err != nil {
		t.Fatalf("complete list: %v", err)
	}
}
