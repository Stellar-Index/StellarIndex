package chops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type eiCapture struct {
	accounts []clickhouse.EntryIndexAccountRow
	assets   []clickhouse.EntryIndexAssetRow
	wm       []uint64
	// signerOps feeds the signer stream; signerWM records each signer watermark window and signerErr is its answer.
	signerOps []clickhouse.EntryIndexSignerOp
	signerWM  [][2]uint32
	signerErr error
	// removalsIn feeds the claimable-removal stream; claimables, removals and claimableWM capture the claimable index writes.
	removalsIn  []clickhouse.EntryIndexClaimableRemoval
	claimables  []clickhouse.EntryIndexClaimableRow
	removals    []clickhouse.EntryIndexClaimableRemoval
	claimableWM [][2]uint32
}

// eiStub serves the fixtures through the seams, filtered to each window's range, and captures every write.
func eiStub(t *testing.T, owners []clickhouse.EntryIndexOwnerRow, decode []clickhouse.EntryIndexDecodeRow, assets []clickhouse.EntryIndexAssetRow) *eiCapture {
	t.Helper()
	origO, origA, origD, origI, origS := streamEntryIndexOwners, streamEntryIndexAssets, streamEntryIndexDecodeRows, insertEntryIndex, setEntryIndexWatermark
	origSO, origSW := streamEntryIndexSignerOps, setSignerIndexWatermark
	origCR, origCI, origCW := streamClaimableRemovals, insertClaimableIndex, setClaimableIndexWatermark
	t.Cleanup(func() {
		streamEntryIndexOwners, streamEntryIndexAssets, streamEntryIndexDecodeRows, insertEntryIndex, setEntryIndexWatermark = origO, origA, origD, origI, origS
		streamEntryIndexSignerOps, setSignerIndexWatermark = origSO, origSW
		streamClaimableRemovals, insertClaimableIndex, setClaimableIndexWatermark = origCR, origCI, origCW
	})
	streamEntryIndexOwners = func(_ context.Context, _ string, from, to uint32, fn func(clickhouse.EntryIndexOwnerRow) error) error {
		for _, r := range owners {
			if r.Ledger >= from && r.Ledger <= to {
				if err := fn(r); err != nil {
					return err
				}
			}
		}
		return nil
	}
	streamEntryIndexDecodeRows = func(_ context.Context, _ string, from, to uint32, fn func(clickhouse.EntryIndexDecodeRow) error) error {
		for _, r := range decode {
			if r.Ledger >= from && r.Ledger <= to {
				if err := fn(r); err != nil {
					return err
				}
			}
		}
		return nil
	}
	streamEntryIndexAssets = func(_ context.Context, _ string, from, to uint32, fn func(clickhouse.EntryIndexAssetRow) error) error {
		for _, r := range assets {
			if r.Ledger >= from && r.Ledger <= to {
				if err := fn(r); err != nil {
					return err
				}
			}
		}
		return nil
	}
	got := &eiCapture{}
	streamEntryIndexSignerOps = func(_ context.Context, _ string, from, to uint32, fn func(clickhouse.EntryIndexSignerOp) error) error {
		for _, r := range got.signerOps {
			if r.Ledger >= from && r.Ledger <= to {
				if err := fn(r); err != nil {
					return err
				}
			}
		}
		return nil
	}
	setSignerIndexWatermark = func(_ context.Context, _ string, from, thru uint32) error {
		got.signerWM = append(got.signerWM, [2]uint32{from, thru})
		return got.signerErr
	}
	insertEntryIndex = func(_ context.Context, _ string, a []clickhouse.EntryIndexAccountRow, s []clickhouse.EntryIndexAssetRow) (int64, error) {
		got.accounts = append(got.accounts, a...)
		got.assets = append(got.assets, s...)
		return int64(len(a) + len(s)), nil
	}
	setEntryIndexWatermark = func(_ context.Context, _ string, _, _ uint32, readTxLedgers uint64) error {
		got.wm = append(got.wm, readTxLedgers)
		return nil
	}
	streamClaimableRemovals = func(_ context.Context, _ string, from, to uint32, fn func(clickhouse.EntryIndexClaimableRemoval) error) error {
		for _, r := range got.removalsIn {
			if r.Ledger >= from && r.Ledger <= to {
				if err := fn(r); err != nil {
					return err
				}
			}
		}
		return nil
	}
	insertClaimableIndex = func(_ context.Context, _ string, c []clickhouse.EntryIndexClaimableRow, r []clickhouse.EntryIndexClaimableRemoval) error {
		got.claimables = append(got.claimables, c...)
		got.removals = append(got.removals, r...)
		return nil
	}
	setClaimableIndexWatermark = func(_ context.Context, _ string, from, thru uint32) error {
		got.claimableWM = append(got.claimableWM, [2]uint32{from, thru})
		return nil
	}
	return got
}

func eiXDR(t *testing.T, e xdr.LedgerEntry) string {
	t.Helper()
	s, err := xdr.MarshalBase64(e)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func eiRaw(t *testing.T, g string) string {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, g)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func eiClaimable(claimants ...string) xdr.LedgerEntry {
	cb := &xdr.ClaimableBalanceEntry{
		BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &xdr.Hash{1}},
		Asset:     xdr.MustNewCreditAsset("USDC", ehIssuer), Amount: 70,
	}
	for _, c := range claimants {
		cb.Claimants = append(cb.Claimants, xdr.Claimant{Type: xdr.ClaimantTypeClaimantTypeV0, V0: &xdr.ClaimantV0{
			Destination: xdr.MustAddress(c), Predicate: xdr.ClaimPredicate{Type: xdr.ClaimPredicateTypeClaimPredicateUnconditional},
		}})
	}
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeClaimableBalance, ClaimableBalance: cb}}
}

func eiFixtures(t *testing.T) ([]clickhouse.EntryIndexOwnerRow, []clickhouse.EntryIndexDecodeRow, []clickhouse.EntryIndexAssetRow) {
	t.Helper()
	sponsored := eiXDR(t, ehSponsored(ehAccount(ehBob, 10, 1), ehAlice))
	owners := []clickhouse.EntryIndexOwnerRow{
		{AccountID: ehAlice, Ledger: ehLedger}, {AccountID: ehBob, Ledger: ehLedger}, {AccountID: ehAlice, Ledger: ehLedger + 2},
	}
	decode := []clickhouse.EntryIndexDecodeRow{
		{Ledger: ehLedger, EntryType: "claimable_balance", KeyXDR: "cb-key", EntryXDR: eiXDR(t, eiClaimable(ehBob, ehAlice))},
		{Ledger: ehLedger + 1, EntryType: "account", EntryXDR: sponsored},
		{Ledger: ehLedger + 1, EntryType: "account", EntryXDR: sponsored}, // same sponsor twice in one ledger: one row
		{Ledger: ehLedger + 2, EntryType: "account", EntryXDR: sponsored}, // a new ledger: a new row
	}
	assets := []clickhouse.EntryIndexAssetRow{{Asset: "USDC-" + ehIssuer, EntryType: "trustline", Ledger: ehLedger, Created: 1, Updated: 2}}
	return owners, decode, assets
}

func TestRunEntryIndex_WriteCollectsEveryRoleAndProvesCoverage(t *testing.T) {
	owners, decode, assets := eiFixtures(t)
	got := eiStub(t, owners, decode, assets)
	st := &entryIndexStats{}
	r := &entryIndexRun{chAddr: "unused", window: 10}
	if err := runEntryIndex(context.Background(), r, ehLedger, ehLedger+2, st); err != nil {
		t.Fatal(err)
	}
	alice, bob := eiRaw(t, ehAlice), eiRaw(t, ehBob)
	want := []clickhouse.EntryIndexAccountRow{
		{Account: alice, Role: "owner", Ledger: ehLedger},
		{Account: bob, Role: "owner", Ledger: ehLedger},
		{Account: alice, Role: "owner", Ledger: ehLedger + 2},
		{Account: bob, Role: "claimant", Ledger: ehLedger},
		{Account: alice, Role: "claimant", Ledger: ehLedger},
		{Account: alice, Role: "sponsor", Ledger: ehLedger + 1},
		{Account: alice, Role: "sponsor", Ledger: ehLedger + 2},
	}
	if !slices.Equal(got.accounts, want) {
		t.Fatalf("account rows = %+v, want %+v", got.accounts, want)
	}
	if !slices.Equal(got.assets, assets) {
		t.Fatalf("asset rows = %+v, want %+v", got.assets, assets)
	}
	// Owner rows cover ledgers L and L+2 only: that count is what the watermark proof compares with stellar.ledgers.
	if !slices.Equal(got.wm, []uint64{2}) {
		t.Fatalf("watermark readTxLedgers = %v, want [2]", got.wm)
	}
	if st.accountRows != 7 || st.assetRows != 1 || st.decoded != 4 || st.skipped != 0 {
		t.Fatalf("stats = %+v", *st)
	}
}

func TestRunEntryIndex_WindowsAdvanceTheWatermarkEach(t *testing.T) {
	owners, decode, assets := eiFixtures(t)
	got := eiStub(t, owners, decode, assets)
	r := &entryIndexRun{chAddr: "unused", window: 2}
	if err := runEntryIndex(context.Background(), r, ehLedger, ehLedger+2, &entryIndexStats{}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.wm, []uint64{1, 1}) {
		t.Fatalf("watermark readTxLedgers per window = %v, want [1 1]", got.wm)
	}
	if len(got.accounts) != 7 {
		t.Fatalf("account rows across windows = %d, want 7", len(got.accounts))
	}
}

func TestRunEntryIndex_DryRunCountsWithoutWriting(t *testing.T) {
	owners, decode, assets := eiFixtures(t)
	got := eiStub(t, owners, decode, assets)
	st := &entryIndexStats{}
	r := &entryIndexRun{chAddr: "unused", window: 10, dryRun: true}
	if err := runEntryIndex(context.Background(), r, ehLedger, ehLedger+2, st); err != nil {
		t.Fatal(err)
	}
	if len(got.accounts) != 0 || len(got.assets) != 0 || len(got.wm) != 0 {
		t.Fatalf("dry run wrote %d account rows, %d asset rows, %d watermarks", len(got.accounts), len(got.assets), len(got.wm))
	}
	if st.accountRows != 7 || st.assetRows != 1 {
		t.Fatalf("dry-run stats = %+v, want 7 account rows and 1 asset row", *st)
	}
}

func TestRunEntryIndex_UndecodableEntryIsSkippedAndBudgeted(t *testing.T) {
	owners, decode, assets := eiFixtures(t)
	decode = append(decode, clickhouse.EntryIndexDecodeRow{Ledger: ehLedger, EntryType: "claimable_balance", EntryXDR: "AAAA"})
	got := eiStub(t, owners, decode, assets)
	st := &entryIndexStats{}
	if err := runEntryIndex(context.Background(), &entryIndexRun{chAddr: "unused", window: 10}, ehLedger, ehLedger+2, st); err != nil {
		t.Fatal(err)
	}
	if st.skipped != 1 || len(got.accounts) != 7 {
		t.Fatalf("skipped = %d, account rows = %d; want 1 skipped and the 7 good rows", st.skipped, len(got.accounts))
	}
	if err := enforceDecodeBudget("ch-entry-index", st.skipped, 0); err == nil {
		t.Fatal("a zero decode budget must fail the run after a skip")
	}
}

func TestChEntryIndex_RejectsEmptyWindowAndFloor(t *testing.T) {
	if err := chEntryIndex([]string{"-window", "0"}); err == nil {
		t.Fatal("-window 0 accepted")
	}
	if err := chEntryIndex([]string{"-floor-ledger", "0"}); err == nil {
		t.Fatal("-floor-ledger 0 accepted")
	}
}

func eiSetOptions(t *testing.T, signer *xdr.Signer) string {
	t.Helper()
	s, err := xdr.MarshalBase64(xdr.OperationBody{Type: xdr.OperationTypeSetOptions, SetOptionsOp: &xdr.SetOptionsOp{Signer: signer}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func eiEd25519Signer(t *testing.T, g string, weight uint32) *xdr.Signer {
	t.Helper()
	var k xdr.Uint256
	copy(k[:], eiRaw(t, g))
	return &xdr.Signer{Key: xdr.SignerKey{Type: xdr.SignerKeyTypeSignerKeyTypeEd25519, Ed25519: &k}, Weight: xdr.Uint32(weight)}
}

func TestRunEntryIndex_SignerRowsComeFromAddedEd25519Signers(t *testing.T) {
	got := eiStub(t, nil, nil, nil)
	addBob := eiSetOptions(t, eiEd25519Signer(t, ehBob, 1))
	hashX := xdr.Uint256{7}
	bump, err := xdr.MarshalBase64(xdr.OperationBody{Type: xdr.OperationTypeBumpSequence, BumpSequenceOp: &xdr.BumpSequenceOp{BumpTo: 1}})
	if err != nil {
		t.Fatal(err)
	}
	got.signerOps = []clickhouse.EntryIndexSignerOp{
		{Ledger: ehLedger, BodyXDR: addBob},
		{Ledger: ehLedger, BodyXDR: addBob},                                          // same key twice in one ledger: one row
		{Ledger: ehLedger, BodyXDR: eiSetOptions(t, eiEd25519Signer(t, ehAlice, 0))}, // removal: no row
		{Ledger: ehLedger, BodyXDR: eiSetOptions(t, &xdr.Signer{Key: xdr.SignerKey{Type: xdr.SignerKeyTypeSignerKeyTypeHashX, HashX: &hashX}, Weight: 1})},
		{Ledger: ehLedger, BodyXDR: eiSetOptions(t, nil)}, // thresholds-only set_options
		{Ledger: ehLedger + 1, BodyXDR: bump},
		{Ledger: ehLedger + 1, BodyXDR: "not-xdr"},
		{Ledger: ehLedger + 2, BodyXDR: addBob},
	}
	st := &entryIndexStats{}
	r := &entryIndexRun{chAddr: "unused", window: 2}
	if err := runEntryIndex(context.Background(), r, ehLedger, ehLedger+2, st); err != nil {
		t.Fatal(err)
	}
	bob := eiRaw(t, ehBob)
	want := []clickhouse.EntryIndexAccountRow{{Account: bob, Role: "signer", Ledger: ehLedger}, {Account: bob, Role: "signer", Ledger: ehLedger + 2}}
	if !slices.Equal(got.accounts, want) {
		t.Fatalf("account rows = %+v, want %+v", got.accounts, want)
	}
	if want := [][2]uint32{{ehLedger, ehLedger + 1}, {ehLedger + 2, ehLedger + 2}}; !slices.Equal(got.signerWM, want) {
		t.Fatalf("signer watermark windows = %v, want %v", got.signerWM, want)
	}
	if st.skipped != 1 {
		t.Fatalf("skipped = %d, want 1 (the undecodable body)", st.skipped)
	}
}

func TestRunEntryIndex_SignerWatermarkWithoutHistoryKeepsTheBuildGoing(t *testing.T) {
	got := eiStub(t, nil, nil, nil)
	got.signerErr = fmt.Errorf("wrapped: %w", clickhouse.ErrEntryHistorySkippedPrefix)
	r := &entryIndexRun{chAddr: "unused", window: 10}
	if err := runEntryIndex(context.Background(), r, ehLedger, ehLedger+2, &entryIndexStats{}); err != nil {
		t.Fatalf("a live-only build must not fail on the signer watermark: %v", err)
	}
	if len(got.wm) != 1 {
		t.Fatalf("entry watermark calls = %d, want 1", len(got.wm))
	}

	got.signerErr, got.wm = errors.New("clickhouse down"), nil
	if err := runEntryIndex(context.Background(), r, ehLedger, ehLedger+2, &entryIndexStats{}); err == nil {
		t.Fatal("a real signer watermark error must fail the window")
	}
	if len(got.wm) != 0 {
		t.Fatal("the entry watermark must not advance past a window whose signer watermark failed")
	}
}

// eiSignersOnly stubs the range and both watermark reads for execSigners: the signer watermark is whatever the last
// set call recorded, and each entry_index read returns the next value of entryWMs (the timer moving on meanwhile).
func eiSignersOnly(t *testing.T, got *eiCapture, entryWMs ...uint32) {
	t.Helper()
	origR, origE, origS := resolveEntryIndexRange, entryIndexWatermark, signerIndexWatermark
	t.Cleanup(func() { resolveEntryIndexRange, entryIndexWatermark, signerIndexWatermark = origR, origE, origS })
	signerIndexWatermark = func(context.Context, string) (uint32, error) {
		if len(got.signerWM) == 0 {
			return 0, nil
		}
		return got.signerWM[len(got.signerWM)-1][1], nil
	}
	resolveEntryIndexRange = func(ctx context.Context, addr, _ string, from, _, floor uint32, readWM func(context.Context, string) (uint32, error)) (uint32, uint32, error) {
		if from != 0 {
			return from, ehLedger + 100, nil
		}
		wm, _ := readWM(ctx, addr)
		return max(wm+1, floor), ehLedger + 100, nil
	}
	entryIndexWatermark = func(context.Context, string) (uint32, error) {
		v := entryWMs[0]
		if len(entryWMs) > 1 {
			entryWMs = entryWMs[1:]
		}
		return v, nil
	}
}

func TestExecSigners_WalksOnlySignersUntilTheEntryWatermark(t *testing.T) {
	owners, decode, assets := eiFixtures(t)
	got := eiStub(t, owners, decode, assets)
	got.signerOps = []clickhouse.EntryIndexSignerOp{
		{Ledger: ehLedger, BodyXDR: eiSetOptions(t, eiEd25519Signer(t, ehBob, 1))},
		{Ledger: ehLedger + 3, BodyXDR: eiSetOptions(t, eiEd25519Signer(t, ehBob, 2))},
	}
	// The timer finishes a window while the first pass runs: the walk goes round again to meet it.
	eiSignersOnly(t, got, ehLedger+1, ehLedger+3, ehLedger+3)
	r := &entryIndexRun{chAddr: "unused", window: 10, floor: ehLedger, signersOnly: true}
	if err := r.execRole(context.Background(), "signer role", signerIndexWatermark); err != nil {
		t.Fatal(err)
	}
	bob := eiRaw(t, ehBob)
	want := []clickhouse.EntryIndexAccountRow{{Account: bob, Role: "signer", Ledger: ehLedger}, {Account: bob, Role: "signer", Ledger: ehLedger + 3}}
	if !slices.Equal(got.accounts, want) {
		t.Fatalf("account rows = %+v, want only the signer rows %+v", got.accounts, want)
	}
	if len(got.assets) != 0 || len(got.wm) != 0 {
		t.Fatalf("signers-only wrote %d asset rows and %d entry watermarks; want neither", len(got.assets), len(got.wm))
	}
	if want := [][2]uint32{{ehLedger, ehLedger + 1}, {ehLedger + 2, ehLedger + 3}}; !slices.Equal(got.signerWM, want) {
		t.Fatalf("signer watermark windows = %v, want %v", got.signerWM, want)
	}
}

func TestExecSigners_RefusedSignerWatermarkFails(t *testing.T) {
	got := eiStub(t, nil, nil, nil)
	got.signerErr = fmt.Errorf("wrapped: %w", clickhouse.ErrEntryHistorySkippedPrefix)
	eiSignersOnly(t, got, ehLedger+2)
	r := &entryIndexRun{chAddr: "unused", window: 10, floor: ehLedger, signersOnly: true}
	if err := r.execRole(context.Background(), "signer role", signerIndexWatermark); err == nil {
		t.Fatal("a signers-only walk exists to set the signer watermark; a refusal must fail it")
	}
}

func TestRunEntryIndex_ClaimableIndexRowsPerClaimantAndRemovals(t *testing.T) {
	owners, decode, assets := eiFixtures(t)
	got := eiStub(t, owners, decode, assets)
	got.removalsIn = []clickhouse.EntryIndexClaimableRemoval{{KeyXDR: "cb-gone", Ledger: ehLedger + 2}}
	r := &entryIndexRun{chAddr: "unused", window: 10}
	if err := runEntryIndex(context.Background(), r, ehLedger, ehLedger+2, &entryIndexStats{}); err != nil {
		t.Fatal(err)
	}
	entry := decode[0].EntryXDR
	want := []clickhouse.EntryIndexClaimableRow{
		{Account: eiRaw(t, ehBob), KeyXDR: "cb-key", Ledger: ehLedger, EntryXDR: entry},
		{Account: eiRaw(t, ehAlice), KeyXDR: "cb-key", Ledger: ehLedger, EntryXDR: entry},
	}
	if !slices.Equal(got.claimables, want) {
		t.Fatalf("claimable rows = %+v, want one per claimant %+v", got.claimables, want)
	}
	if !slices.Equal(got.removals, got.removalsIn) {
		t.Fatalf("removals = %+v, want %+v", got.removals, got.removalsIn)
	}
	if want := [][2]uint32{{ehLedger, ehLedger + 2}}; !slices.Equal(got.claimableWM, want) {
		t.Fatalf("claimable watermark windows = %v, want %v", got.claimableWM, want)
	}
}

func TestRunEntryIndex_ClaimablesOnlyWritesOnlyTheClaimableIndex(t *testing.T) {
	owners, decode, assets := eiFixtures(t)
	got := eiStub(t, owners, decode, assets)
	got.removalsIn = []clickhouse.EntryIndexClaimableRemoval{{KeyXDR: "cb-gone", Ledger: ehLedger + 1}}
	r := &entryIndexRun{chAddr: "unused", window: 10, claimablesOnly: true}
	if err := runEntryIndex(context.Background(), r, ehLedger, ehLedger+2, &entryIndexStats{}); err != nil {
		t.Fatal(err)
	}
	if len(got.accounts) != 0 || len(got.assets) != 0 || len(got.wm) != 0 || len(got.signerWM) != 0 {
		t.Fatalf("claimables-only wrote %d account rows, %d asset rows, %d entry and %d signer watermarks; want none",
			len(got.accounts), len(got.assets), len(got.wm), len(got.signerWM))
	}
	if len(got.claimables) != 2 || len(got.removals) != 1 {
		t.Fatalf("claimables-only wrote %d claimable rows and %d removals, want 2 and 1", len(got.claimables), len(got.removals))
	}
	if want := [][2]uint32{{ehLedger, ehLedger + 2}}; !slices.Equal(got.claimableWM, want) {
		t.Fatalf("claimable watermark windows = %v, want %v", got.claimableWM, want)
	}
}
