package sac_balances

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// i128Parts builds the ScVal for an arbitrary (hi, lo) i128 — the fixture
// helpers above only reach int64, which is exactly the range where an
// int64(parts.Lo) truncation is invisible.
func i128Parts(hi int64, lo uint64) xdr.ScVal {
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}}
}

// balanceMap wraps an i128 in the SAC BalanceValue map shape. amountFirst
// flips field order: the decode is by map field NAME, never by position.
func balanceMap(amount xdr.ScVal, amountFirst bool) xdr.ScVal {
	amtSym, authSym, clbSym := xdr.ScSymbol("amount"), xdr.ScSymbol("authorized"), xdr.ScSymbol("clawback")
	trueB := true
	amt := xdr.ScMapEntry{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &amtSym}, Val: amount}
	auth := xdr.ScMapEntry{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &authSym}, Val: xdr.ScVal{Type: xdr.ScValTypeScvBool, B: &trueB}}
	clb := xdr.ScMapEntry{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &clbSym}, Val: xdr.ScVal{Type: xdr.ScValTypeScvBool, B: &trueB}}
	m := xdr.ScMap{auth, clb, amt}
	if amountFirst {
		m = xdr.ScMap{amt, auth, clb}
	}
	mp := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
}

// withChangeType re-homes an Updated change's entry under another
// live-entry variant (Created / Restored), which must decode identically.
func withChangeType(change xdr.LedgerEntryChange, typ xdr.LedgerEntryChangeType) xdr.LedgerEntryChange {
	entry := change.Updated
	switch typ {
	case xdr.LedgerEntryChangeTypeLedgerEntryCreated:
		return xdr.LedgerEntryChange{Type: typ, Created: entry}
	case xdr.LedgerEntryChangeTypeLedgerEntryRestored:
		return xdr.LedgerEntryChange{Type: typ, Restored: entry}
	default:
		return change
	}
}

// FuzzSACBalanceObservation drives the observer over the whole i128
// domain, both value shapes and every live-entry change variant. The
// balance lands in the SAC-wrapped supply component, so ADR-0003 applies:
// it must be the exact i128, never its low limb. IntraLedgerSeq and
// Ledger must survive too — the writer keeps the FINAL change per
// (contract, holder, ledger) by IntraLedgerSeq.
//
// Generative run:
// go test -run=^$ -fuzz=^FuzzSACBalanceObservation$ -fuzztime=60s -parallel=2 ./internal/sources/sac_balances
func FuzzSACBalanceObservation(f *testing.F) {
	f.Add(int64(0), uint64(987_654_321), uint8(0), uint8(0), uint32(7), uint32(1))
	f.Add(int64(0), uint64(555), uint8(1), uint8(1), uint32(0), uint32(2))
	f.Add(int64(0), uint64(0), uint8(2), uint8(2), uint32(1), uint32(3))
	f.Add(int64(0), uint64(1)<<63, uint8(0), uint8(0), uint32(9), uint32(4))
	f.Add(int64(0), ^uint64(0), uint8(1), uint8(1), uint32(1<<31), uint32(5))
	f.Add(int64(1), uint64(0), uint8(2), uint8(2), uint32(3), uint32(6))
	f.Add(int64(^uint64(0)>>1), ^uint64(0), uint8(0), uint8(1), ^uint32(0), ^uint32(0))
	f.Add(int64(-1), ^uint64(0), uint8(1), uint8(2), uint32(2), uint32(7))

	o, err := NewObserver(map[string]string{cSAC: "USDC:G..."})
	if err != nil {
		f.Fatalf("NewObserver: %v", err)
	}
	changeTypes := []xdr.LedgerEntryChangeType{
		xdr.LedgerEntryChangeTypeLedgerEntryUpdated,
		xdr.LedgerEntryChangeTypeLedgerEntryCreated,
		xdr.LedgerEntryChangeTypeLedgerEntryRestored,
	}

	f.Fuzz(func(t *testing.T, hi int64, lo uint64, shape, changeSel uint8, seq, ledger uint32) {
		want := new(big.Int).Lsh(big.NewInt(hi), 64)
		want.Add(want, new(big.Int).SetUint64(lo))

		amount := i128Parts(hi, lo)
		var val xdr.ScVal
		switch shape % 3 {
		case 0:
			val = amount
		case 1:
			val = balanceMap(amount, true)
		default:
			val = balanceMap(amount, false)
		}
		typ := changeTypes[int(changeSel)%len(changeTypes)]
		change := withChangeType(makeContractDataChange(t, cSAC, makeBalanceKey(t, gHolder), val), typ)

		if !o.Matches(change) {
			t.Fatalf("watched SAC balance change (%s, shape %d) did not match", typ, shape%3)
		}
		closedAt := time.Unix(1_700_000_000, 0).UTC()
		outs, err := o.Decode(dispatcher.LedgerEntryChangeContext{
			Ledger: ledger, ClosedAt: closedAt, Change: change, IntraLedgerSeq: seq,
		})
		if err != nil {
			t.Fatalf("Decode(%s, shape %d, hi=%d lo=%d): %v", typ, shape%3, hi, lo, err)
		}
		if len(outs) != 1 {
			t.Fatalf("Decode emitted %d observations, want 1", len(outs))
		}
		obs, ok := outs[0].(Observation)
		if !ok {
			t.Fatalf("Decode emitted %T, want Observation", outs[0])
		}
		if obs.Balance == nil || obs.Balance.Cmp(want) != 0 {
			t.Fatalf("Balance = %v, want %s (hi=%d lo=%d shape %d) — the i128 was not carried exactly",
				obs.Balance, want, hi, lo, shape%3)
		}
		if obs.IsRemoval {
			t.Fatalf("IsRemoval = true on a %s change", typ)
		}
		if obs.IntraLedgerSeq != seq {
			t.Fatalf("IntraLedgerSeq = %d, want %d — intra-ledger ordering lost", obs.IntraLedgerSeq, seq)
		}
		if obs.Ledger != ledger || !obs.ObservedAt.Equal(closedAt) {
			t.Fatalf("Ledger/ObservedAt = %d/%v, want %d/%v", obs.Ledger, obs.ObservedAt, ledger, closedAt)
		}
		if obs.ContractID != cSAC || obs.Holder != gHolder || obs.AssetKey != "USDC:G..." {
			t.Fatalf("identity = (%q, %q, %q), want (%q, %q, USDC:G...)", obs.ContractID, obs.Holder, obs.AssetKey, cSAC, gHolder)
		}
	})
}

// TestObserver_RemovalCarriesIntraLedgerSeq: an eviction/deletion must keep
// its within-ledger position, or a same-ledger delete-then-recreate can be
// resolved to the wrong final state.
func TestObserver_RemovalCarriesIntraLedgerSeq(t *testing.T) {
	o, _ := NewObserver(map[string]string{cSAC: "USDC:G..."})
	cid := mustContractID(t, cSAC)
	change := xdr.LedgerEntryChange{
		Type: xdr.LedgerEntryChangeTypeLedgerEntryRemoved,
		Removed: &xdr.LedgerKey{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.LedgerKeyContractData{
				Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: (*xdr.ContractId)(&cid)},
				Key:        makeBalanceKey(t, gHolder),
				Durability: xdr.ContractDataDurabilityPersistent,
			},
		},
	}
	outs, err := o.Decode(dispatcher.LedgerEntryChangeContext{Ledger: 42, Change: change, IntraLedgerSeq: 17})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	obs := outs[0].(Observation)
	if !obs.IsRemoval || obs.Balance.Sign() != 0 {
		t.Fatalf("removal = (%v, %s), want (true, 0)", obs.IsRemoval, obs.Balance)
	}
	if obs.IntraLedgerSeq != 17 || obs.Ledger != 42 {
		t.Fatalf("IntraLedgerSeq/Ledger = %d/%d, want 17/42", obs.IntraLedgerSeq, obs.Ledger)
	}
}

// TestObserver_DecodeRejectsUnwatchedContract: Decode must not trust that
// Matches ran — an unwatched contract has no asset_key, and emitting one
// with an empty key would write an orphan supply slice.
func TestObserver_DecodeRejectsUnwatchedContract(t *testing.T) {
	const cOther = "CAAQCAIBAEAQCAIBAEAQCAIBAEAQCAIBAEAQCAIBAEAQCAIBAEAQC526"
	o, _ := NewObserver(map[string]string{cSAC: "USDC:G..."})
	change := makeContractDataChange(t, cOther, makeBalanceKey(t, gHolder), makeI128Val(1))
	if o.Matches(change) {
		t.Fatalf("Matches accepted unwatched contract %s", cOther)
	}
	outs, err := o.Decode(dispatcher.LedgerEntryChangeContext{Ledger: 1, Change: change})
	if !errors.Is(err, ErrNotSACBalance) {
		t.Fatalf("Decode(unwatched) = (%v, %v), want ErrNotSACBalance", outs, err)
	}
}

// TestObserver_DecodeRejectsForeignBalanceShapes: a balance value that is
// neither i128 nor a map with an i128 `amount` must be refused, not read
// as zero or as a different width.
func TestObserver_DecodeRejectsForeignBalanceShapes(t *testing.T) {
	o, _ := NewObserver(map[string]string{cSAC: "USDC:G..."})
	u128 := xdr.ScVal{Type: xdr.ScValTypeScvU128, U128: &xdr.UInt128Parts{Lo: 5}}
	u64v := xdr.Uint64(5)
	u64 := xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &u64v}
	for name, val := range map[string]xdr.ScVal{
		"bare u128":        u128,
		"bare u64":         u64,
		"map amount u128":  balanceMap(u128, true),
		"map amount u64":   balanceMap(u64, false),
		"void (no amount)": {Type: xdr.ScValTypeScvVoid},
	} {
		change := makeContractDataChange(t, cSAC, makeBalanceKey(t, gHolder), val)
		outs, err := o.Decode(dispatcher.LedgerEntryChangeContext{Ledger: 1, Change: change})
		if !errors.Is(err, ErrUnknownValShape) {
			t.Errorf("%s: Decode = (%v, %v), want ErrUnknownValShape", name, outs, err)
		}
	}
}
