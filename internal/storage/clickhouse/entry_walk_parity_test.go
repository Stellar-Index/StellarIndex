package clickhouse_test

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// This file is finding X1's remediation: the live dispatcher walker and the
// lake walker are two independent implementations of ONE contract ("the lake's
// rows match what the live LedgerEntryChangeDecoder hook sees"), and until now
// that contract was enforced only by a comment in each file saying it mirrored
// the other. Comment-coupling is what rotted last time — the dispatcher grew a
// failed-tx skip the lake never had (C2-023/C2-040) and both kept a per-tx
// phase order the chain does not use (C2-032), with the docs on both sides
// still claiming they agreed.
//
// It lives in package clickhouse_test (external) so it can import
// internal/dispatcher without a cycle, driving both walkers through their
// EXPORTED entry points — dispatcher.ProcessLedger and clickhouse.ExtractLedger
// — i.e. exactly what the indexer and the lake writer call.

const parityPassphrase = network.TestNetworkPassphrase

// parityAccount is the single account every fixture change touches, so the
// balance doubles as a readable position marker.
const parityAccount = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"

// parityChange is an Updated AccountEntry change carrying `balance`, which the
// assertions use to identify which change landed where.
func parityChange(balance int64) xdr.LedgerEntryChange {
	return xdr.LedgerEntryChange{
		Type: xdr.LedgerEntryChangeTypeLedgerEntryUpdated,
		Updated: &xdr.LedgerEntry{
			LastModifiedLedgerSeq: 100,
			Data: xdr.LedgerEntryData{
				Type: xdr.LedgerEntryTypeAccount,
				Account: &xdr.AccountEntry{
					AccountId: xdr.MustAddress(parityAccount),
					Balance:   xdr.Int64(balance),
				},
			},
		},
	}
}

// parityTx describes one transaction of the synthetic ledger by the balances
// it writes in each of the three ledger-wide phases.
type parityTx struct {
	seed         byte
	success      bool
	fee          []int64 // phase 1
	apply        []int64 // phase 2 (one operation)
	postApplyFee []int64 // phase 3 (P23 Soroban refund)
}

func toChanges(balances []int64) xdr.LedgerEntryChanges {
	out := make(xdr.LedgerEntryChanges, 0, len(balances))
	for _, b := range balances {
		out = append(out, parityChange(b))
	}
	return out
}

// buildParityLedger assembles a protocol-23 LedgerCloseMetaV2 (V2 is required:
// PostTxApplyFeeChanges is only populated from lcmV2.TxProcessing[i].
// PostTxApplyFeeProcessing) whose transactions write the given per-phase
// balances.
func buildParityLedger(t *testing.T, seq uint32, txs []parityTx) xdr.LedgerCloseMeta {
	t.Helper()

	envs := make([]xdr.TransactionEnvelope, 0, len(txs))
	procs := make([]xdr.TransactionResultMetaV1, 0, len(txs))
	for _, spec := range txs {
		var srcSeed [32]byte
		srcSeed[0] = spec.seed
		for i := 1; i < 32; i++ {
			srcSeed[i] = byte(i)
		}
		muxed, err := xdr.NewMuxedAccount(xdr.CryptoKeyTypeKeyTypeEd25519, xdr.Uint256(srcSeed))
		if err != nil {
			t.Fatalf("NewMuxedAccount: %v", err)
		}
		env := xdr.TransactionEnvelope{
			Type: xdr.EnvelopeTypeEnvelopeTypeTx,
			V1: &xdr.TransactionV1Envelope{
				Tx: xdr.Transaction{
					SourceAccount: muxed,
					Fee:           100,
					SeqNum:        1,
					Cond:          xdr.Preconditions{Type: xdr.PreconditionTypePrecondNone},
					Memo:          xdr.Memo{Type: xdr.MemoTypeMemoNone},
					Operations:    []xdr.Operation{},
				},
			},
		}
		hash, err := network.HashTransactionInEnvelope(env, parityPassphrase)
		if err != nil {
			t.Fatalf("hash envelope: %v", err)
		}
		code := xdr.TransactionResultCodeTxSuccess
		if !spec.success {
			code = xdr.TransactionResultCodeTxFailed
		}
		emptyOpResults := []xdr.OperationResult{}
		envs = append(envs, env)
		procs = append(procs, xdr.TransactionResultMetaV1{
			Result: xdr.TransactionResultPair{
				TransactionHash: xdr.Hash(hash),
				Result: xdr.TransactionResult{
					FeeCharged: 100,
					Result: xdr.TransactionResultResult{
						Code:    code,
						Results: &emptyOpResults,
					},
				},
			},
			FeeProcessing: toChanges(spec.fee),
			TxApplyProcessing: xdr.TransactionMeta{
				V: 3,
				V3: &xdr.TransactionMetaV3{
					Operations: []xdr.OperationMeta{{Changes: toChanges(spec.apply)}},
				},
			},
			PostTxApplyFeeProcessing: toChanges(spec.postApplyFee),
		})
	}

	return xdr.LedgerCloseMeta{
		V: 2,
		V2: &xdr.LedgerCloseMetaV2{
			LedgerHeader: xdr.LedgerHeaderHistoryEntry{
				Header: xdr.LedgerHeader{
					LedgerSeq: xdr.Uint32(seq),
					ScpValue:  xdr.StellarValue{CloseTime: xdr.TimePoint(1_700_000_000)},
				},
			},
			TxSet: xdr.GeneralizedTransactionSet{
				V: 1,
				V1TxSet: &xdr.TransactionSetV1{
					Phases: []xdr.TransactionPhase{{
						V: 0,
						V0Components: &[]xdr.TxSetComponent{{
							Type:                  xdr.TxSetComponentTypeTxsetCompTxsMaybeDiscountedFee,
							TxsMaybeDiscountedFee: &xdr.TxSetComponentTxsMaybeDiscountedFee{Txs: envs},
						}},
					}},
				},
			},
			TxProcessing: procs,
		},
	}
}

// walkStep is the comparable projection of one walked change. Everything in it
// is derivable independently on BOTH sides, so neither walker's internal
// representation is privileged.
type walkStep struct {
	Seq     uint32
	TxHash  string
	OpIndex int32
	Balance int64
	KeyXDR  string
}

func (w walkStep) String() string {
	return fmt.Sprintf("seq=%d tx=%.8s op=%d balance=%d", w.Seq, w.TxHash, w.OpIndex, w.Balance)
}

// spyDecoder matches every change and records the walk as the live dispatcher
// produces it.
type spyDecoder struct{ steps []walkStep }

func (s *spyDecoder) Name() string { return "parity-spy" }

func (s *spyDecoder) Matches(xdr.LedgerEntryChange) bool { return true }

func (s *spyDecoder) Decode(ctx dispatcher.LedgerEntryChangeContext) ([]consumer.Event, error) {
	entry := ctx.Change.Updated
	if entry == nil || entry.Data.Account == nil {
		return nil, nil
	}
	key, err := entry.LedgerKey()
	if err != nil {
		return nil, err
	}
	raw, err := key.MarshalBinary()
	if err != nil {
		return nil, err
	}
	s.steps = append(s.steps, walkStep{
		Seq:     ctx.IntraLedgerSeq,
		TxHash:  ctx.TxHash,
		OpIndex: int32(ctx.OpIndex),
		Balance: int64(entry.Data.Account.Balance),
		KeyXDR:  base64.StdEncoding.EncodeToString(raw),
	})
	return nil, nil
}

// TestEntryWalkParity_DispatcherAndLakeAgree drives ONE synthetic multi-phase
// ledger through both walkers and asserts they emit an IDENTICAL (entry, seq)
// sequence.
//
// The fixture is chosen so every historical divergence between the two would
// break it:
//
//   - tx2 is FAILED with a committed fee debit — the dispatcher used to skip
//     the whole tx before its entry-change walk while the lake walked it
//     (C2-023/C2-040), so the two disagreed by exactly the failed-tx fee set;
//   - tx1 has both an apply-phase change and a later tx's fee change competing
//     for the same key — the per-tx walk ranked the fee last (C2-032);
//   - both txs carry PostTxApplyFeeChanges — the P23 Soroban refund phase
//     (R-A01-1), which neither walker read.
func TestEntryWalkParity_DispatcherAndLakeAgree(t *testing.T) {
	lcm := buildParityLedger(t, 4711, []parityTx{
		{seed: 0x71, success: true, fee: []int64{990}, apply: []int64{500}, postApplyFee: []int64{540}},
		{seed: 0x72, success: false, fee: []int64{980}, postApplyFee: []int64{985}},
	})

	// ── Live path ────────────────────────────────────────────────────────
	spy := &spyDecoder{}
	d := dispatcher.New()
	d.AddEntryDecoder(spy)
	if _, err := d.ProcessLedger(lcm, parityPassphrase); err != nil {
		t.Fatalf("dispatcher.ProcessLedger: %v", err)
	}

	// ── Lake path ────────────────────────────────────────────────────────
	ext, err := clickhouse.ExtractLedger(lcm, parityPassphrase)
	if err != nil {
		t.Fatalf("clickhouse.ExtractLedger: %v", err)
	}
	lake := make([]walkStep, 0, len(ext.Changes))
	for _, row := range ext.Changes {
		lake = append(lake, walkStep{
			Seq:     row.IntraLedgerSeq,
			TxHash:  row.TxHash,
			OpIndex: row.OpIndex,
			Balance: row.Balance,
			KeyXDR:  row.KeyXDR,
		})
	}

	// ── Parity ───────────────────────────────────────────────────────────
	if len(spy.steps) != len(lake) {
		t.Fatalf("walk LENGTH differs: dispatcher %d changes, lake %d\n  dispatcher: %v\n  lake:       %v",
			len(spy.steps), len(lake), spy.steps, lake)
	}
	for i := range spy.steps {
		if spy.steps[i] != lake[i] {
			t.Errorf("walk step %d differs:\n  dispatcher: %s\n  lake:       %s",
				i, spy.steps[i], lake[i])
		}
	}

	// The sequence must also be the CHAIN's order, or the two could agree on
	// something equally wrong. Phase 1 (both fees) → phase 2 (the apply
	// change) → phase 3 (both refunds).
	wantBalances := []int64{990, 980, 500, 540, 985}
	if len(spy.steps) != len(wantBalances) {
		t.Fatalf("expected %d changes across the three phases, got %d: %v",
			len(wantBalances), len(spy.steps), spy.steps)
	}
	for i, want := range wantBalances {
		if spy.steps[i].Balance != want {
			t.Errorf("position %d = balance %d, want %d — the ledger-wide phase order is "+
				"fee(all txs) → apply(all txs) → post-apply fee(all txs); got %v",
				i, spy.steps[i].Balance, want, spy.steps)
		}
		if spy.steps[i].Seq != uint32(i) {
			t.Errorf("position %d carries IntraLedgerSeq %d, want %d (dense, monotonic)",
				i, spy.steps[i].Seq, i)
		}
	}
}

// TestEntryWalkParity_PostApplyFeeRefundIsWalked isolates the P23 phase-3
// half (R-A01-1) so a regression there names itself instead of surfacing as a
// generic parity mismatch. A Soroban fee refund is the LAST thing that touches
// the fee-source account in a P23 ledger, so dropping it publishes the
// PRE-REFUND balance as that account's ledger-final state.
func TestEntryWalkParity_PostApplyFeeRefundIsWalked(t *testing.T) {
	lcm := buildParityLedger(t, 4712, []parityTx{
		{seed: 0x81, success: true, fee: []int64{900}, apply: []int64{400}, postApplyFee: []int64{460}},
	})

	spy := &spyDecoder{}
	d := dispatcher.New()
	d.AddEntryDecoder(spy)
	if _, err := d.ProcessLedger(lcm, parityPassphrase); err != nil {
		t.Fatalf("dispatcher.ProcessLedger: %v", err)
	}
	ext, err := clickhouse.ExtractLedger(lcm, parityPassphrase)
	if err != nil {
		t.Fatalf("clickhouse.ExtractLedger: %v", err)
	}

	const refund = int64(460)
	findLast := func(steps []walkStep) walkStep { return steps[len(steps)-1] }

	if len(spy.steps) != 3 {
		t.Fatalf("dispatcher walked %d changes, want 3 (fee, apply, post-apply refund): %v",
			len(spy.steps), spy.steps)
	}
	if got := findLast(spy.steps); got.Balance != refund {
		t.Errorf("dispatcher's LAST change is balance %d, want the P23 refund %d — "+
			"the pre-refund balance would be published as the ledger-final state", got.Balance, refund)
	}
	if len(ext.Changes) != 3 {
		t.Fatalf("lake extracted %d changes, want 3 (fee, apply, post-apply refund)", len(ext.Changes))
	}
	if got := ext.Changes[len(ext.Changes)-1]; got.Balance != refund {
		t.Errorf("lake's LAST row is balance %d, want the P23 refund %d", got.Balance, refund)
	}
	// op_index -1 marks it tx-level, like the fee phase it mirrors.
	if got := ext.Changes[len(ext.Changes)-1]; got.OpIndex != -1 {
		t.Errorf("post-apply fee row op_index = %d, want -1 (a tx-level change)", got.OpIndex)
	}
}

// evictionSpy records the walk's Removed changes — the shape the dispatcher's
// eviction phase dispatches — keyed so they compare against lake rows.
type evictionSpy struct{ steps []walkStep }

func (s *evictionSpy) Name() string { return "parity-eviction-spy" }

func (s *evictionSpy) Matches(c xdr.LedgerEntryChange) bool { return c.Removed != nil }

func (s *evictionSpy) Decode(ctx dispatcher.LedgerEntryChangeContext) ([]consumer.Event, error) {
	raw, err := ctx.Change.Removed.MarshalBinary()
	if err != nil {
		return nil, err
	}
	s.steps = append(s.steps, walkStep{
		Seq:     ctx.IntraLedgerSeq,
		TxHash:  ctx.TxHash,
		OpIndex: int32(ctx.OpIndex),
		KeyXDR:  base64.StdEncoding.EncodeToString(raw),
	})
	return nil, nil
}

// TestEntryWalkParity_EvictedKeysAreRecordedByBothWalkers: an entry whose TTL
// lapses is reported only in the LCM's evicted-keys list, never in tx meta.
// The lake must record a deleted (temporary) entry as a `removed` row at the
// eviction ledger, at the same position the live dispatcher walks it, or
// ledger_entries_current keeps the entry's last write as live. A persistent
// entry or contract code is archived, not deleted, so the lake writes no row
// and its last live value stays current.
func TestEntryWalkParity_EvictedKeysAreRecordedByBothWalkers(t *testing.T) {
	const ledger = 4713
	lcm := buildParityLedger(t, ledger, []parityTx{
		{seed: 0x91, success: true, fee: []int64{700}, apply: []int64{650}, postApplyFee: []int64{660}},
	})
	cid := xdr.ContractId{0xE1}
	dataKey := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityTemporary,
		},
	}
	persistentKey := dataKey
	persistentKey.ContractData = &xdr.LedgerKeyContractData{
		Contract:   dataKey.ContractData.Contract,
		Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
		Durability: xdr.ContractDataDurabilityPersistent,
	}
	codeKey := xdr.LedgerKey{Type: xdr.LedgerEntryTypeContractCode, ContractCode: &xdr.LedgerKeyContractCode{Hash: xdr.Hash{0xE3}}}
	ttlKey := xdr.LedgerKey{Type: xdr.LedgerEntryTypeTtl, Ttl: &xdr.LedgerKeyTtl{KeyHash: xdr.Hash{0xE2}}}
	lcm.V2.EvictedKeys = []xdr.LedgerKey{persistentKey, dataKey, codeKey, ttlKey}

	spy := &evictionSpy{}
	d := dispatcher.New()
	d.AddEntryDecoder(spy)
	if _, err := d.ProcessLedger(lcm, parityPassphrase); err != nil {
		t.Fatalf("dispatcher.ProcessLedger: %v", err)
	}
	ext, err := clickhouse.ExtractLedger(lcm, parityPassphrase)
	if err != nil {
		t.Fatalf("clickhouse.ExtractLedger: %v", err)
	}
	if ext.EvictedKeysUnreadable != 0 {
		t.Fatalf("EvictedKeysUnreadable = %d on a readable list", ext.EvictedKeysUnreadable)
	}

	var lake []walkStep
	var removed []clickhouse.LedgerEntryChangeRow
	var lastTxSeq uint32
	for _, row := range ext.Changes {
		if row.ChangeType != "removed" {
			lastTxSeq = max(lastTxSeq, row.IntraLedgerSeq)
			continue
		}
		removed = append(removed, row)
		lake = append(lake, walkStep{Seq: row.IntraLedgerSeq, TxHash: row.TxHash, OpIndex: row.OpIndex, KeyXDR: row.KeyXDR})
	}

	archived := map[string]bool{}
	for _, k := range []xdr.LedgerKey{persistentKey, codeKey} {
		raw, err := k.MarshalBinary()
		if err != nil {
			t.Fatalf("marshal archived key: %v", err)
		}
		archived[base64.StdEncoding.EncodeToString(raw)] = true
	}
	var deleted []walkStep
	for _, st := range spy.steps {
		if !archived[st.KeyXDR] {
			deleted = append(deleted, st)
		}
	}
	if len(spy.steps) != 4 || len(deleted) != 2 || len(lake) != 2 {
		t.Fatalf("evicted keys walked: dispatcher %d (%d deleted), lake %d, want 4 (2) and 2\n  dispatcher: %v\n  lake:       %v",
			len(spy.steps), len(deleted), len(lake), spy.steps, lake)
	}
	for i := range deleted {
		if deleted[i] != lake[i] {
			t.Errorf("eviction step %d differs:\n  dispatcher: %s key=%s\n  lake:       %s key=%s",
				i, deleted[i], deleted[i].KeyXDR, lake[i], lake[i].KeyXDR)
		}
	}

	wantTypes := []string{"contract_data", "ttl"}
	for i, row := range removed {
		if row.LedgerSeq != ledger || row.TxHash != "" || row.OpIndex != -1 {
			t.Errorf("eviction row %d at (ledger %d, tx %q, op %d), want (%d, \"\", -1)",
				i, row.LedgerSeq, row.TxHash, row.OpIndex, ledger)
		}
		if row.EntryType != wantTypes[i] || row.EntryXDR != "" {
			t.Errorf("eviction row %d entry_type=%q entry_xdr empty=%v, want %q with no entry",
				i, row.EntryType, row.EntryXDR == "", wantTypes[i])
		}
		// Rows share (ledger_seq, tx_hash, op_index), the RMT sort-key prefix,
		// so change_index alone keeps them from collapsing into one at merge.
		if row.ChangeIndex != uint32(i) {
			t.Errorf("eviction row %d change_index = %d, want %d", i, row.ChangeIndex, i)
		}
		if row.IntraLedgerSeq <= lastTxSeq {
			t.Errorf("eviction row %d intra_ledger_seq %d must follow every tx-phase change (last %d)",
				i, row.IntraLedgerSeq, lastTxSeq)
		}
	}
}
