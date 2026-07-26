package dispatcher

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// entryChangeSpy records every LedgerEntryChangeContext the dispatcher
// routes to it. Matches every change so the recorded slice is the exact
// walk order, including the seq stamped on each.
type entryChangeSpy struct {
	seen []LedgerEntryChangeContext
}

func (s *entryChangeSpy) Name() string { return "spy" }

func (s *entryChangeSpy) Matches(xdr.LedgerEntryChange) bool { return true }

func (s *entryChangeSpy) Decode(ctx LedgerEntryChangeContext) ([]consumer.Event, error) {
	s.seen = append(s.seen, ctx)
	return nil, nil
}

// ecWalkAccount is the account whose balance the two-phase ordering test
// follows across the ledger.
const ecWalkAccount = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"

// mkEntryWalkTx builds one transaction for a synthetic ledger: a distinct
// source account (so the envelope hashes differ), the given fee changes,
// and the given single-operation apply-phase changes. success drives the
// TransactionResultCode so the failed-tx path can be exercised.
func mkEntryWalkTx(
	t *testing.T,
	seed byte,
	success bool,
	feeChanges []xdr.LedgerEntryChange,
	applyChanges []xdr.LedgerEntryChange,
	ops ...xdr.Operation,
) (xdr.TransactionEnvelope, xdr.TransactionResultMeta) {
	t.Helper()

	var srcSeed [32]byte
	srcSeed[0] = seed
	for i := 1; i < 32; i++ {
		srcSeed[i] = byte(i)
	}
	srcMuxed, err := xdr.NewMuxedAccount(xdr.CryptoKeyTypeKeyTypeEd25519, xdr.Uint256(srcSeed))
	if err != nil {
		t.Fatalf("NewMuxedAccount: %v", err)
	}
	if ops == nil {
		ops = []xdr.Operation{}
	}
	envelope := xdr.TransactionEnvelope{
		Type: xdr.EnvelopeTypeEnvelopeTypeTx,
		V1: &xdr.TransactionV1Envelope{
			Tx: xdr.Transaction{
				SourceAccount: srcMuxed,
				Fee:           100,
				SeqNum:        1,
				Cond:          xdr.Preconditions{Type: xdr.PreconditionTypePrecondNone},
				Memo:          xdr.Memo{Type: xdr.MemoTypeMemoNone},
				Operations:    ops,
			},
		},
	}
	hash, err := network.HashTransactionInEnvelope(envelope, testPassphrase)
	if err != nil {
		t.Fatalf("hash envelope: %v", err)
	}

	code := xdr.TransactionResultCodeTxSuccess
	if !success {
		code = xdr.TransactionResultCodeTxFailed
	}
	emptyOpResults := make([]xdr.OperationResult, len(ops))
	for i := range emptyOpResults {
		emptyOpResults[i] = xdr.OperationResult{Code: xdr.OperationResultCodeOpInner}
	}
	proc := xdr.TransactionResultMeta{
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
		FeeProcessing: xdr.LedgerEntryChanges(feeChanges),
		TxApplyProcessing: xdr.TransactionMeta{
			V: 3,
			V3: &xdr.TransactionMetaV3{
				Operations: []xdr.OperationMeta{{Changes: applyChanges}},
			},
		},
	}
	return envelope, proc
}

// mkEntryWalkLedger assembles a LedgerCloseMeta from the given (envelope,
// processing) pairs, in tx-set order.
func mkEntryWalkLedger(seq uint32, envs []xdr.TransactionEnvelope, procs []xdr.TransactionResultMeta) xdr.LedgerCloseMeta {
	return xdr.LedgerCloseMeta{
		V: 1,
		V1: &xdr.LedgerCloseMetaV1{
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
							Type: xdr.TxSetComponentTypeTxsetCompTxsMaybeDiscountedFee,
							TxsMaybeDiscountedFee: &xdr.TxSetComponentTxsMaybeDiscountedFee{
								Txs: envs,
							},
						}},
					}},
				},
			},
			TxProcessing: procs,
		},
	}
}

// accountBalanceChange is an Updated AccountEntry change for
// ecWalkAccount at the given balance — the shape the balance-observation
// decoders consume.
func accountBalanceChange(balance int64) xdr.LedgerEntryChange {
	return xdr.LedgerEntryChange{
		Type: xdr.LedgerEntryChangeTypeLedgerEntryUpdated,
		Updated: &xdr.LedgerEntry{
			Data: xdr.LedgerEntryData{
				Type: xdr.LedgerEntryTypeAccount,
				Account: &xdr.AccountEntry{
					AccountId: xdr.MustAddress(ecWalkAccount),
					Balance:   xdr.Int64(balance),
				},
			},
		},
	}
}

func balanceOf(c xdr.LedgerEntryChange) int64 {
	if c.Updated == nil || c.Updated.Data.Account == nil {
		return -1
	}
	return int64(c.Updated.Data.Account.Balance)
}

// TestProcessLedger_FailedTxFeeChangesAreObserved pins C2-023/C2-040
// (audit-2026-07-23): a FAILED transaction still debits its fee on chain,
// stellar-core commits that fee change, and the lake's
// clickhouse.extractEntryChanges has always recorded it. The live
// dispatcher used to `continue` past the whole tx before the entry-change
// walk, so the balance observer never saw the debit — the observed balance
// drifted above the on-chain balance by the fee, and the live path and the
// lake disagreed by exactly the failed-tx fee set (so an ADR-0034 re-derive
// could never reconcile).
func TestProcessLedger_FailedTxFeeChangesAreObserved(t *testing.T) {
	// Ledger: one successful tx, then one FAILED tx whose fee debits
	// ecWalkAccount from 1000 to 900.
	okEnv, okProc := mkEntryWalkTx(t, 0x11, true, nil, []xdr.LedgerEntryChange{accountBalanceChange(1000)})
	failEnv, failProc := mkEntryWalkTx(t, 0x22, false, []xdr.LedgerEntryChange{accountBalanceChange(900)}, nil)
	lcm := mkEntryWalkLedger(4242,
		[]xdr.TransactionEnvelope{okEnv, failEnv},
		[]xdr.TransactionResultMeta{okProc, failProc})

	spy := &entryChangeSpy{}
	d := New()
	d.AddEntryDecoder(spy)

	if _, err := d.ProcessLedger(lcm, testPassphrase); err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}

	var sawFailedFee bool
	for _, ctx := range spy.seen {
		if ctx.OpIndex == -1 && balanceOf(ctx.Change) == 900 {
			sawFailedFee = true
		}
	}
	if !sawFailedFee {
		t.Fatalf("the failed tx's fee change (balance 900) never reached the entry decoders; "+
			"got %d changes: %v — a failed tx's fee IS committed on chain and the lake records it",
			len(spy.seen), spy.seen)
	}
}

// TestProcessLedger_FeePhasePrecedesApplyPhase pins C2-032
// (audit-2026-07-23). stellar-core charges the fee for EVERY transaction
// in the tx set before applying ANY of them, so on chain every fee change
// precedes every apply-phase change. The per-tx walk (tx1 fee, tx1 apply,
// tx2 fee, ...) gave tx2's FEE-phase balance a HIGHER IntraLedgerSeq than
// tx1's APPLY-phase balance for the same account — and IntraLedgerSeq is
// exactly the tiebreak that makes the FINAL intra-ledger change win the
// balance upsert. The result was a fee-phase balance published as the
// ledger-final balance.
func TestProcessLedger_FeePhasePrecedesApplyPhase(t *testing.T) {
	// One account, touched twice in one ledger:
	//   tx1 fee   → balance 990   (fee phase)
	//   tx1 apply → balance 500   (apply phase — the ledger-FINAL balance)
	//   tx2 fee   → balance 980   (fee phase, but a LATER tx)
	// On chain the order is 990, 980, 500 — so 500 must rank last.
	tx1Env, tx1Proc := mkEntryWalkTx(t, 0x33, true,
		[]xdr.LedgerEntryChange{accountBalanceChange(990)},
		[]xdr.LedgerEntryChange{accountBalanceChange(500)})
	tx2Env, tx2Proc := mkEntryWalkTx(t, 0x44, true,
		[]xdr.LedgerEntryChange{accountBalanceChange(980)}, nil)
	lcm := mkEntryWalkLedger(4243,
		[]xdr.TransactionEnvelope{tx1Env, tx2Env},
		[]xdr.TransactionResultMeta{tx1Proc, tx2Proc})

	spy := &entryChangeSpy{}
	d := New()
	d.AddEntryDecoder(spy)

	if _, err := d.ProcessLedger(lcm, testPassphrase); err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}

	seqOf := make(map[int64]uint32, 3)
	for _, ctx := range spy.seen {
		seqOf[balanceOf(ctx.Change)] = ctx.IntraLedgerSeq
	}
	for _, want := range []int64{990, 980, 500} {
		if _, ok := seqOf[want]; !ok {
			t.Fatalf("balance %d never reached the entry decoders; saw %v", want, seqOf)
		}
	}
	// The chain's commit order, expressed as the exact IntraLedgerSeq the
	// ReplacingMergeTree version folds in.
	if seqOf[990] != 0 || seqOf[980] != 1 || seqOf[500] != 2 {
		t.Errorf("IntraLedgerSeq = {tx1fee(990):%d, tx2fee(980):%d, tx1apply(500):%d}, "+
			"want {0, 1, 2} — all fee changes precede all apply-phase changes on chain",
			seqOf[990], seqOf[980], seqOf[500])
	}
	if seqOf[500] <= seqOf[980] {
		t.Errorf("tx1's apply-phase balance (500) has IntraLedgerSeq %d <= tx2's fee-phase balance (980) at %d: "+
			"FINAL dedup would publish the fee-phase balance as the ledger-final balance",
			seqOf[500], seqOf[980])
	}
}

// TestProcessLedger_FailedTxStillSkipsPriceSignal keeps the half of the
// pre-existing behaviour that is correct: a failed transaction's
// operations produced no effect, so no op/event/contract-call decoder may
// fire for it. Only its committed entry changes are observed.
func TestProcessLedger_FailedTxStillSkipsPriceSignal(t *testing.T) {
	// A ManageSellOffer op that a decoder watches for. It is inside a
	// FAILED tx, so it produced no effect and must never be routed.
	offerOp := xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypeManageSellOffer,
			ManageSellOfferOp: &xdr.ManageSellOfferOp{
				Selling: xdr.MustNewNativeAsset(),
				Buying:  xdr.MustNewCreditAsset("USDC", ecWalkAccount),
				Amount:  100,
				Price:   xdr.Price{N: 1, D: 1},
			},
		},
	}
	failEnv, failProc := mkEntryWalkTx(t, 0x55, false,
		[]xdr.LedgerEntryChange{accountBalanceChange(900)}, nil, offerOp)
	lcm := mkEntryWalkLedger(4244,
		[]xdr.TransactionEnvelope{failEnv},
		[]xdr.TransactionResultMeta{failProc})

	op := &fakeOpDecoder{name: "ops", matchTyp: xdr.OperationTypeManageSellOffer}
	spy := &entryChangeSpy{}
	d := New()
	d.AddOpDecoder(op)
	d.AddEntryDecoder(spy)

	if _, err := d.ProcessLedger(lcm, testPassphrase); err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}
	if op.calls != 0 {
		t.Errorf("op decoder decoded %d ops from a FAILED tx, want 0 — failed ops have no effect", op.calls)
	}
	if len(spy.seen) != 1 {
		t.Errorf("entry decoder saw %d changes, want 1 (the failed tx's committed fee debit)", len(spy.seen))
	}
}
