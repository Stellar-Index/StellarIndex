package dispatcher

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
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

// TestProcessLedger_FailedTxFeeChangesAreObserved pins that a FAILED transaction still debits its fee on chain,
// stellar-core commits that fee change, and the lake's
// clickhouse.extractEntryChanges has always recorded it. The live
// dispatcher must not `continue` past the whole tx before the entry-change
// walk, or the balance observer never sees the debit — the observed balance
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

// TestProcessLedger_FeePhasePrecedesApplyPhase pins that stellar-core charges the fee for EVERY transaction
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

// TestProcessLedger_BlockOrderDoesNotChangePositions: stellar-core lists a
// block's keys in hash-map order, so two exports of one ledger can disagree on
// it. Each key's IntraLedgerSeq must not depend on which export was read, or a
// re-derive from the other export renumbers what the guards compare.
func TestProcessLedger_BlockOrderDoesNotChangePositions(t *testing.T) {
	change := func(seed byte, balance int64) xdr.LedgerEntryChange {
		id, err := xdr.NewAccountId(xdr.PublicKeyTypePublicKeyTypeEd25519, xdr.Uint256{seed})
		if err != nil {
			t.Fatalf("NewAccountId: %v", err)
		}
		return xdr.LedgerEntryChange{
			Type: xdr.LedgerEntryChangeTypeLedgerEntryUpdated,
			Updated: &xdr.LedgerEntry{Data: xdr.LedgerEntryData{
				Type:    xdr.LedgerEntryTypeAccount,
				Account: &xdr.AccountEntry{AccountId: id, Balance: xdr.Int64(balance)},
			}},
		}
	}
	positions := func(block []xdr.LedgerEntryChange) map[int64]uint32 {
		env, proc := mkEntryWalkTx(t, 0x55, true, nil, block)
		spy := &entryChangeSpy{}
		d := New()
		d.AddEntryDecoder(spy)
		lcm := mkEntryWalkLedger(4244, []xdr.TransactionEnvelope{env}, []xdr.TransactionResultMeta{proc})
		if _, err := d.ProcessLedger(lcm, testPassphrase); err != nil {
			t.Fatalf("ProcessLedger: %v", err)
		}
		out := make(map[int64]uint32, len(spy.seen))
		for _, ctx := range spy.seen {
			out[int64(ctx.Change.Updated.Data.Account.Balance)] = ctx.IntraLedgerSeq
		}
		return out
	}

	a, b, c := change(0x0A, 1), change(0x0B, 2), change(0x0C, 3)
	one := positions([]xdr.LedgerEntryChange{a, b, c})
	two := positions([]xdr.LedgerEntryChange{c, a, b})
	if len(one) != 3 {
		t.Fatalf("expected 3 walked changes, got %v", one)
	}
	for bal, seq := range one {
		if two[bal] != seq {
			t.Errorf("change %d: IntraLedgerSeq %d from one export order, %d from another — positions must not depend on the export",
				bal, seq, two[bal])
		}
	}
}

// TestProcessLedger_PanickingDecoderLosesOnlyItsOwnEvent is the
// end-to-end shape of the incident: ProcessLedger must return nil (so
// the caller persists the cursor and the process keeps running) while
// the SIBLING source's event still decodes.
func TestProcessLedger_PanickingDecoderLosesOnlyItsOwnEvent(t *testing.T) {
	for _, tc := range []struct {
		name          string
		panicInMatch  bool
		panicInDecode bool
	}{
		{name: "panic_in_Decode", panicInDecode: true},
		{name: "panic_in_Matches", panicInMatch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op, _ := provInvokeOp(t, provContractA, "x")
			lcm := provLedger(t, op, []xdr.ContractEvent{
				provEvent("REDSTONE", provContractA), // routed to the broken decoder
				provEvent("REDSTONE", provContractB), // routed to the healthy one
			}, nil)

			strA, err := contractIDToStrkey(provContractA)
			if err != nil {
				t.Fatal(err)
			}
			strB, err := contractIDToStrkey(provContractB)
			if err != nil {
				t.Fatal(err)
			}

			broken := &panickyDecoder{
				name:          "broken-" + tc.name,
				contract:      strA,
				panicInMatch:  tc.panicInMatch,
				panicInDecode: tc.panicInDecode,
			}
			healthy := &provSpyDecoder{name: "healthy-" + tc.name, contract: strB}
			disp := New(broken, healthy) // broken registered FIRST
			before := testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(broken.name))

			outs, err := disp.ProcessLedger(lcm, testPassphrase)
			if err != nil {
				t.Fatalf("ProcessLedger = %v, want nil — one decoder's panic must not reject the whole ledger (that is the crash-loop)", err)
			}
			_ = outs
			if len(healthy.got) != 1 {
				t.Errorf("sibling decoder saw %d events, want 1 — the ledger walk must continue past the panic", len(healthy.got))
			}
			panics, _, decodeErrs := counters(t, disp, broken.name)
			if panics-before != 1 {
				t.Errorf("panic counter rose by %v, want 1", panics-before)
			}
			if decodeErrs != 1 {
				t.Errorf("Stats().DecodeErrors[%s] = %d, want 1 — the skip must be recorded, not silent", broken.name, decodeErrs)
			}
		})
	}
}

func TestProcessLedger_emptyLedgerYieldsNoOutputs(t *testing.T) {
	// A LedgerCloseMeta with no transactions should produce zero
	// outputs and no error. Validates that the reader construction
	// path doesn't trip on empty ledgers (common during Stellar's
	// quiet periods on testnet).
	lcm := emptyLedgerCloseMeta(t, 42)

	disp := New(&fakeDecoder{name: "unused", topic0: "zzz"})
	outs, err := disp.ProcessLedger(lcm, testPassphrase)
	if err != nil {
		t.Fatalf("ProcessLedger empty ledger: %v", err)
	}
	if len(outs) != 0 {
		t.Errorf("got %d outputs from empty ledger, want 0", len(outs))
	}
	// No events → no matches → no unmatched hits either.
	if got := disp.Stats().UnmatchedHits; got != 0 {
		t.Errorf("UnmatchedHits = %d, want 0", got)
	}
}

// TestProcessLedger_EvictedKeysReachTheEntryDecoders pins that Soroban state archival is the one way a ledger entry
// leaves the live state WITHOUT a transaction touching it: when its TTL
// lapses, core evicts it at ledger close and reports it only in the
// LedgerCloseMeta's evicted-keys list. The three-phase walk reads
// transaction meta, so it could never see one.
//
// The consequence was not cosmetic. The decoders handled Restored — the
// other half of the same lifecycle — so an archived SAC balance's last
// write stood as the holder's current balance forever and the served supply
// component stayed permanently above the truth with no path to
// self-correct.
func TestProcessLedger_EvictedKeysReachTheEntryDecoders(t *testing.T) {
	env, proc := mkEntryWalkTx(t, 0x55, true, nil, []xdr.LedgerEntryChange{accountBalanceChange(1000)})
	lcm := mkEntryWalkLedger(4244,
		[]xdr.TransactionEnvelope{env},
		[]xdr.TransactionResultMeta{proc})
	dataKey := evictedContractDataKey(0x9e)
	lcm.V1.EvictedKeys = []xdr.LedgerKey{evictedTTLKey(0x9e), dataKey}

	spy := &entryChangeSpy{}
	d := New()
	d.AddEntryDecoder(spy)

	if _, err := d.ProcessLedger(lcm, testPassphrase); err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}

	var (
		evicted   *LedgerEntryChangeContext
		sawTTLKey bool
		maxTxSeq  uint32
		txChanges int
	)
	for i := range spy.seen {
		ctx := spy.seen[i]
		if ctx.Change.Type != xdr.LedgerEntryChangeTypeLedgerEntryRemoved || ctx.Change.Removed == nil {
			txChanges++
			if ctx.IntraLedgerSeq > maxTxSeq {
				maxTxSeq = ctx.IntraLedgerSeq
			}
			continue
		}
		switch ctx.Change.Removed.Type {
		case xdr.LedgerEntryTypeContractData:
			evicted = &spy.seen[i]
		case xdr.LedgerEntryTypeTtl:
			sawTTLKey = true
		default:
			t.Errorf("unexpected removed key type %v", ctx.Change.Removed.Type)
		}
	}

	if evicted == nil {
		t.Fatalf("the ledger's evicted contract-data key never reached the entry decoders; "+
			"saw %d changes — an evicted SAC balance then stays in the served supply forever",
			len(spy.seen))
	}
	if got, want := *evicted.Change.Removed.ContractData, *dataKey.ContractData; got.Contract != want.Contract ||
		got.Durability != want.Durability {
		t.Errorf("evicted change carries key %+v, want %+v", got, want)
	}
	if !sawTTLKey {
		t.Error("the paired TTL key was dropped; the walk must dispatch the eviction list as core reports it " +
			"and let each decoder's Matches discard what it does not watch")
	}
	if evicted.TxHash != "" || evicted.OpIndex != -1 {
		t.Errorf("evicted change stamped tx_hash=%q op_index=%d, want \"\" / -1 — "+
			"eviction is applied at ledger close and belongs to no transaction",
			evicted.TxHash, evicted.OpIndex)
	}
	if txChanges == 0 {
		t.Fatal("no transaction-phase change was walked; the fixture is not exercising the ordering assertion")
	}
	if evicted.IntraLedgerSeq <= maxTxSeq {
		t.Errorf("evicted change has IntraLedgerSeq %d <= the last transaction-phase change at %d: "+
			"a balance written earlier in this same ledger would win the last-writer-wins upsert "+
			"and the eviction would be discarded", evicted.IntraLedgerSeq, maxTxSeq)
	}
}

// TestProcessLedger_NoEvictionsEmitsNothingExtra keeps the negative half
// honest: a ledger with an empty eviction list must walk exactly the
// transaction-phase changes it always did. The eviction phase is additive,
// which is also why it does not renumber IntraLedgerSeq (see
// [EntryWalkVersion]).
func TestProcessLedger_NoEvictionsEmitsNothingExtra(t *testing.T) {
	env, proc := mkEntryWalkTx(t, 0x66, true,
		[]xdr.LedgerEntryChange{accountBalanceChange(990)},
		[]xdr.LedgerEntryChange{accountBalanceChange(500)})
	lcm := mkEntryWalkLedger(4245,
		[]xdr.TransactionEnvelope{env},
		[]xdr.TransactionResultMeta{proc})

	spy := &entryChangeSpy{}
	d := New()
	d.AddEntryDecoder(spy)

	if _, err := d.ProcessLedger(lcm, testPassphrase); err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}
	if len(spy.seen) != 2 {
		t.Fatalf("walked %d changes, want exactly the 2 transaction-phase changes", len(spy.seen))
	}
	for i, ctx := range spy.seen {
		if got := balanceOf(ctx.Change); got != []int64{990, 500}[i] {
			t.Errorf("change %d = balance %d, want %d (order unchanged by the eviction phase)",
				i, got, []int64{990, 500}[i])
		}
		if ctx.IntraLedgerSeq != uint32(i) {
			t.Errorf("change %d has IntraLedgerSeq %d, want %d", i, ctx.IntraLedgerSeq, i)
		}
	}
}

// TestProcessLedger_UpgradesAreCountedAndLogged pins that a ledger's
// upgrade entries are observed (counted, logged with the ledger) without
// reaching any entry decoder.
func TestProcessLedger_UpgradesAreCountedAndLogged(t *testing.T) {
	env, proc := mkEntryWalkTx(t, 0x56, true, nil, []xdr.LedgerEntryChange{accountBalanceChange(1000)})
	lcm := mkEntryWalkLedger(4245,
		[]xdr.TransactionEnvelope{env},
		[]xdr.TransactionResultMeta{proc})
	ver, reserve := xdr.Uint32(30), xdr.Uint32(5000000)
	lcm.V1.UpgradesProcessing = []xdr.UpgradeEntryMeta{
		{Upgrade: xdr.LedgerUpgrade{Type: xdr.LedgerUpgradeTypeLedgerUpgradeVersion, NewLedgerVersion: &ver}},
		{Upgrade: xdr.LedgerUpgrade{Type: xdr.LedgerUpgradeTypeLedgerUpgradeBaseReserve, NewBaseReserve: &reserve}},
	}

	var buf bytes.Buffer
	spy := &entryChangeSpy{}
	d := New()
	d.SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	d.AddEntryDecoder(spy)

	if _, err := d.ProcessLedger(lcm, testPassphrase); err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}
	if got := d.Stats().LedgerUpgradeEntries; got != 2 {
		t.Errorf("Stats().LedgerUpgradeEntries = %d, want 2", got)
	}
	if log := buf.String(); !strings.Contains(log, "ledger=4245") || !strings.Contains(log, "LedgerUpgradeVersion") {
		t.Errorf("want an upgrade log naming ledger 4245 and its types, got %q", log)
	}
	if len(spy.seen) != 1 {
		t.Errorf("decoders saw %d changes, want only the 1 transaction change", len(spy.seen))
	}
}

// TestProcessLedger_UpgradesCountedWithoutEntryDecoders pins that upgrade
// observation does not depend on any entry decoder being registered.
func TestProcessLedger_UpgradesCountedWithoutEntryDecoders(t *testing.T) {
	env, proc := mkEntryWalkTx(t, 0x57, true, nil, []xdr.LedgerEntryChange{accountBalanceChange(1000)})
	lcm := mkEntryWalkLedger(4246,
		[]xdr.TransactionEnvelope{env},
		[]xdr.TransactionResultMeta{proc})
	ver := xdr.Uint32(30)
	lcm.V1.UpgradesProcessing = []xdr.UpgradeEntryMeta{
		{Upgrade: xdr.LedgerUpgrade{Type: xdr.LedgerUpgradeTypeLedgerUpgradeVersion, NewLedgerVersion: &ver}},
	}

	d := New()
	if _, err := d.ProcessLedger(lcm, testPassphrase); err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}
	if got := d.Stats().LedgerUpgradeEntries; got != 1 {
		t.Errorf("Stats().LedgerUpgradeEntries = %d, want 1", got)
	}
}

// ─── OpArgs provenance gate + lazy state-write enrichment ────────────
//
// The op's top-level InvokeContract args belong to the CALLEE of that
// top-level call. Pre-gate, ProcessLedger attached them to EVERY event
// the op produced — including events emitted by OTHER contracts reached
// as sub-invocations. That let a wrapper contract call
// adapter.write_prices with the genuine signed payload while its own
// (attacker-chosen) top-level args were handed to the adapter-event
// decoder as if they were the write_prices args — feed-attribution
// steering. These tests pin the gate at the ProcessLedger level: args
// attach exactly when invoked-contract == emitting-contract.

// provContracts: A is the invoked (callee) contract, B a sub-invoked
// contract emitting in the same op.
var (
	provContractA = xdr.ContractId{0xA1, 0x01}
	provContractB = xdr.ContractId{0xB2, 0x02}
)

// provSpyDecoder records every event routed to it. wantWrites opts the
// decoder into the StateWriteKeyConsumer enrichment for `contract`.
type provSpyDecoder struct {
	name       string
	contract   string
	wantWrites bool
	got        []events.Event
}

func (p *provSpyDecoder) Name() string { return p.name }
func (p *provSpyDecoder) Matches(ev events.Event) bool {
	return ev.ContractID == p.contract
}

func (p *provSpyDecoder) Decode(ev events.Event) ([]consumer.Event, error) {
	p.got = append(p.got, ev)
	return nil, nil
}

func (p *provSpyDecoder) StateWriteContracts() []string {
	if !p.wantWrites {
		return nil
	}
	return []string{p.contract}
}

// provEvent builds one capture-eligible contract event for cid.
func provEvent(sym string, cid xdr.ContractId) xdr.ContractEvent {
	s := xdr.ScSymbol(sym)
	u := xdr.Uint32(7)
	return xdr.ContractEvent{
		Type:       xdr.ContractEventTypeContract,
		ContractId: &cid,
		Body: xdr.ContractEventBody{
			V: 0,
			V0: &xdr.ContractEventV0{
				Topics: []xdr.ScVal{{Type: xdr.ScValTypeScvSymbol, Sym: &s}},
				Data:   xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &u},
			},
		},
	}
}

// provInvokeOp builds an InvokeHostFunction op whose top-level call
// targets `cid` with one string arg.
func provInvokeOp(t *testing.T, cid xdr.ContractId, arg string) (xdr.Operation, string) {
	t.Helper()
	s := xdr.ScString(arg)
	op := xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypeInvokeHostFunction,
			InvokeHostFunctionOp: &xdr.InvokeHostFunctionOp{
				HostFunction: xdr.HostFunction{
					Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
					InvokeContract: &xdr.InvokeContractArgs{
						ContractAddress: xdr.ScAddress{
							Type:       xdr.ScAddressTypeScAddressTypeContract,
							ContractId: &cid,
						},
						FunctionName: "write_prices",
						Args:         []xdr.ScVal{{Type: xdr.ScValTypeScvString, Str: &s}},
					},
				},
			},
		},
	}
	raw, err := op.Body.InvokeHostFunctionOp.HostFunction.InvokeContract.Args[0].MarshalBinary()
	if err != nil {
		t.Fatalf("marshal arg: %v", err)
	}
	return op, base64.StdEncoding.EncodeToString(raw)
}

// provLedger assembles a one-tx LCM: the tx carries `op`, succeeds, and
// its V4 meta stamps `evs` on the operation along with `changes`.
func provLedger(t *testing.T, op xdr.Operation, evs []xdr.ContractEvent, changes []xdr.LedgerEntryChange) xdr.LedgerCloseMeta {
	t.Helper()
	var srcSeed [32]byte
	srcSeed[0] = 0x77
	srcMuxed, err := xdr.NewMuxedAccount(xdr.CryptoKeyTypeKeyTypeEd25519, xdr.Uint256(srcSeed))
	if err != nil {
		t.Fatalf("NewMuxedAccount: %v", err)
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
				Operations:    []xdr.Operation{op},
			},
		},
	}
	hash, err := network.HashTransactionInEnvelope(envelope, testPassphrase)
	if err != nil {
		t.Fatalf("hash envelope: %v", err)
	}
	opResults := []xdr.OperationResult{{Code: xdr.OperationResultCodeOpInner}}
	proc := xdr.TransactionResultMeta{
		Result: xdr.TransactionResultPair{
			TransactionHash: xdr.Hash(hash),
			Result: xdr.TransactionResult{
				FeeCharged: 100,
				Result: xdr.TransactionResultResult{
					Code:    xdr.TransactionResultCodeTxSuccess,
					Results: &opResults,
				},
			},
		},
		TxApplyProcessing: xdr.TransactionMeta{
			V: 4,
			V4: &xdr.TransactionMetaV4{
				Operations: []xdr.OperationMetaV2{{
					Changes: changes,
					Events:  evs,
				}},
			},
		},
	}
	return mkEntryWalkLedger(9_000_001, []xdr.TransactionEnvelope{envelope}, []xdr.TransactionResultMeta{proc})
}

func TestProcessLedger_OpArgsAttachOnlyToCalleeEvents(t *testing.T) {
	op, argB64 := provInvokeOp(t, provContractA, "feed-vec-sentinel")
	lcm := provLedger(t, op, []xdr.ContractEvent{
		provEvent("REDSTONE", provContractA), // emitted by the callee
		provEvent("REDSTONE", provContractB), // emitted by a sub-invoked contract
	}, nil)

	strA, err := contractIDToStrkey(provContractA)
	if err != nil {
		t.Fatal(err)
	}
	strB, err := contractIDToStrkey(provContractB)
	if err != nil {
		t.Fatal(err)
	}
	spyA := &provSpyDecoder{name: "spyA", contract: strA}
	spyB := &provSpyDecoder{name: "spyB", contract: strB}
	disp := New(spyA, spyB)
	if _, err := disp.ProcessLedger(lcm, testPassphrase); err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}

	if len(spyA.got) != 1 || len(spyB.got) != 1 {
		t.Fatalf("routed events: A=%d B=%d, want 1 each", len(spyA.got), len(spyB.got))
	}
	// Callee's event carries the call's args…
	if len(spyA.got[0].OpArgs) != 1 || spyA.got[0].OpArgs[0] != argB64 {
		t.Errorf("callee event OpArgs = %v, want [%s]", spyA.got[0].OpArgs, argB64)
	}
	// …and the foreign (sub-invoked) contract's event carries NONE:
	// the top-level args describe a call into A, not into B. Handing
	// them to B's decoder is the feed-steering vector this gate closes.
	if len(spyB.got[0].OpArgs) != 0 {
		t.Errorf("foreign-contract event OpArgs = %v, want none (args belong to the callee)", spyB.got[0].OpArgs)
	}
}

func TestProcessLedger_StateWriteKeysOnlyForDeclaredConsumers(t *testing.T) {
	op, _ := provInvokeOp(t, provContractA, "x")
	// Both contracts change one of their own contract-data entries in
	// the op; only A's decoder declares StateWriteKeyConsumer interest.
	changes := []xdr.LedgerEntryChange{
		swkChange(xdr.LedgerEntryChangeTypeLedgerEntryCreated, swkDataEntry(provContractA, "FEED_A", 1)),
		swkChange(xdr.LedgerEntryChangeTypeLedgerEntryCreated, swkDataEntry(provContractB, "FEED_B", 2)),
	}
	lcm := provLedger(t, op, []xdr.ContractEvent{
		provEvent("REDSTONE", provContractA),
		provEvent("REDSTONE", provContractB),
	}, changes)

	strA, err := contractIDToStrkey(provContractA)
	if err != nil {
		t.Fatal(err)
	}
	strB, err := contractIDToStrkey(provContractB)
	if err != nil {
		t.Fatal(err)
	}
	spyA := &provSpyDecoder{name: "spyA", contract: strA, wantWrites: true}
	spyB := &provSpyDecoder{name: "spyB", contract: strB} // no interest declared
	disp := New(spyA, spyB)
	if _, err := disp.ProcessLedger(lcm, testPassphrase); err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}

	if len(spyA.got) != 1 || len(spyB.got) != 1 {
		t.Fatalf("routed events: A=%d B=%d, want 1 each", len(spyA.got), len(spyB.got))
	}
	// Declared consumer gets its own contract's changed keys.
	if len(spyA.got[0].StateWriteKeys) != 1 {
		t.Fatalf("A StateWriteKeys = %v, want exactly FEED_A's key", spyA.got[0].StateWriteKeys)
	}
	var lk xdr.LedgerKey
	if err := xdr.SafeUnmarshalBase64(spyA.got[0].StateWriteKeys[0], &lk); err != nil {
		t.Fatalf("key round-trip: %v", err)
	}
	if cd, ok := lk.GetContractData(); !ok {
		t.Fatalf("key type = %v, want contract data", lk.Type)
	} else if s, _ := cd.Key.GetStr(); string(s) != "FEED_A" {
		t.Errorf("key = %q, want FEED_A", s)
	}
	// Undeclared decoder's event carries nil — "unknown", by contract.
	if spyB.got[0].StateWriteKeys != nil {
		t.Errorf("B StateWriteKeys = %v, want nil (no StateWriteKeyConsumer declared)", spyB.got[0].StateWriteKeys)
	}
}
