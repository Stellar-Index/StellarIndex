package claimable_balances

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// Generative runs: go test -run=^$ -fuzz=^FuzzXxx$ -fuzztime=60s ./internal/sources/claimable_balances

var bodyChangeTypes = []xdr.LedgerEntryChangeType{
	xdr.LedgerEntryChangeTypeLedgerEntryCreated,
	xdr.LedgerEntryChangeTypeLedgerEntryUpdated,
	xdr.LedgerEntryChangeTypeLedgerEntryRestored,
	xdr.LedgerEntryChangeTypeLedgerEntryState,
}

func fuzzCBAsset(native bool, code, issuer []byte) xdr.Asset {
	if native {
		return xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	}
	var pk xdr.Uint256
	copy(pk[:], issuer)
	aid := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pk}
	if len(code) <= 4 {
		var c xdr.AssetCode4
		copy(c[:], code)
		return xdr.Asset{Type: xdr.AssetTypeAssetTypeCreditAlphanum4, AlphaNum4: &xdr.AlphaNum4{AssetCode: c, Issuer: aid}}
	}
	var c xdr.AssetCode12
	copy(c[:], code)
	return xdr.Asset{Type: xdr.AssetTypeAssetTypeCreditAlphanum12, AlphaNum12: &xdr.AlphaNum12{AssetCode: c, Issuer: aid}}
}

// refAssetKey is the test's independent CODE:ISSUER reference.
func refAssetKey(t *testing.T, code, issuer []byte) string {
	t.Helper()
	var pk [32]byte
	copy(pk[:], issuer)
	g, err := strkey.Encode(strkey.VersionByteAccountID, pk[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	return string(bytes.TrimRight(code, "\x00")) + ":" + g
}

func fuzzCBEntry(ct xdr.LedgerEntryChangeType, id []byte, asset xdr.Asset, amount int64) xdr.LedgerEntryChange {
	var h xdr.Hash
	copy(h[:], id)
	entry := &xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeClaimableBalance,
		ClaimableBalance: &xdr.ClaimableBalanceEntry{
			BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &h},
			Asset:     asset,
			Amount:    xdr.Int64(amount),
		},
	}}
	ch := xdr.LedgerEntryChange{Type: ct}
	switch ct {
	case xdr.LedgerEntryChangeTypeLedgerEntryCreated:
		ch.Created = entry
	case xdr.LedgerEntryChangeTypeLedgerEntryUpdated:
		ch.Updated = entry
	case xdr.LedgerEntryChangeTypeLedgerEntryRestored:
		ch.Restored = entry
	case xdr.LedgerEntryChangeTypeLedgerEntryState:
		ch.State = entry
	}
	return ch
}

func fuzzCBRemoved(id []byte) xdr.LedgerEntryChange {
	var h xdr.Hash
	copy(h[:], id)
	return xdr.LedgerEntryChange{
		Type: xdr.LedgerEntryChangeTypeLedgerEntryRemoved,
		Removed: &xdr.LedgerKey{
			Type: xdr.LedgerEntryTypeClaimableBalance,
			ClaimableBalance: &xdr.LedgerKeyClaimableBalance{
				BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &h},
			},
		},
	}
}

// FuzzObserverLifecycle drives one claimable balance through a body
// change and the claim that follows it, over arbitrary asset shapes,
// amounts and watch sets. It asserts: Matches iff a watched credit
// asset; the emitted Balance is the exact int64 Amount as *big.Int
// (ADR-0003) with the change's id, ledger, time and position; STATE
// emits nothing; the claim zeroes the same asset; and a claim whose
// pre-image is from another ledger errors rather than being skipped.
func FuzzObserverLifecycle(f *testing.F) {
	iss := []byte{1, 2, 3}
	f.Add(uint8(0), []byte{0xaa}, false, []byte("USDC"), iss, int64(10_000_000), true, uint32(100), uint32(3))
	f.Add(uint8(1), []byte{0xbb}, false, []byte("EURCLONGCODE"), iss, int64(math.MaxInt64), true, uint32(1), uint32(0))
	f.Add(uint8(2), []byte{0xcc}, false, []byte("AQUA"), iss, int64(math.MaxInt32)+1, true, uint32(7), uint32(9))
	f.Add(uint8(3), []byte{0xdd}, false, []byte("yXLM"), iss, int64(1)<<53+1, true, uint32(8), uint32(1))
	f.Add(uint8(0), []byte{0xee}, true, []byte(nil), []byte(nil), int64(5), true, uint32(5), uint32(5))
	f.Add(uint8(1), []byte{0xff}, false, []byte("A\x00B"), iss, int64(5), false, uint32(5), uint32(5))

	closedAt := time.Unix(1_700_000_000, 0).UTC()
	f.Fuzz(func(t *testing.T, sel uint8, id []byte, native bool, code, issuer []byte,
		amount int64, watch bool, ledger, seq uint32,
	) {
		if len(code) > 12 || ledger == math.MaxUint32 {
			t.Skip()
		}
		ct := bodyChangeTypes[int(sel)%len(bodyChangeTypes)]
		asset := fuzzCBAsset(native, code, issuer)

		watched := []string{"SENTINEL:" + gIssuer}
		var key string
		invalidWatched := false
		if native {
			if _, err := assetKeyFromAsset(asset); !errors.Is(err, ErrUnsupportedClaimableAsset) {
				t.Fatalf("native key err = %v, want ErrUnsupportedClaimableAsset", err)
			}
		} else {
			key = refAssetKey(t, code, issuer)
			got, err := assetKeyFromAsset(asset)
			if err != nil || got != key {
				t.Fatalf("assetKeyFromAsset = %q, %v; want %q", got, err, key)
			}
			ca, cerr := canonical.AssetFromXDR(asset)
			if cerr == nil && ca.Code+":"+ca.Issuer != key {
				t.Fatalf("key %q disagrees with canonical %s:%s", key, ca.Code, ca.Issuer)
			}
			if watch {
				watched = append(watched, key)
				invalidWatched = cerr != nil
			}
		}
		o, err := NewObserver(watched)
		// A code stellar-core would reject can never be configured: the watch list must refuse it.
		if invalidWatched {
			if err == nil {
				t.Fatalf("NewObserver accepted non-canonical watched key %q", key)
			}
			return
		}
		if err != nil {
			t.Fatalf("NewObserver: %v", err)
		}
		isWatched := !native && watch

		change := fuzzCBEntry(ct, id, asset, amount)
		if got := o.Matches(change); got != isWatched {
			t.Fatalf("Matches = %v, want %v", got, isWatched)
		}
		removed := fuzzCBRemoved(id)
		if !isWatched {
			if o.Matches(removed) {
				t.Fatal("claim of an unwatched balance matched")
			}
			return
		}

		var h [32]byte
		copy(h[:], id)
		wantID := hex.EncodeToString(h[:])
		outs, err := o.Decode(dispatcher.LedgerEntryChangeContext{Ledger: ledger, ClosedAt: closedAt, Change: change, IntraLedgerSeq: seq})
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if ct == xdr.LedgerEntryChangeTypeLedgerEntryState {
			if len(outs) != 0 {
				t.Fatalf("STATE pre-image emitted %d observations", len(outs))
			}
		} else {
			if len(outs) != 1 {
				t.Fatalf("got %d observations, want 1", len(outs))
			}
			ob := outs[0].(Observation)
			if ob.Balance.Cmp(big.NewInt(amount)) != 0 {
				t.Fatalf("Balance = %s, want %d", ob.Balance, amount)
			}
			if ob.ClaimableID != wantID || ob.AssetKey != key || ob.Ledger != ledger ||
				!ob.ObservedAt.Equal(closedAt) || ob.IntraLedgerSeq != seq || ob.IsRemoval {
				t.Fatalf("observation context wrong: %+v", ob)
			}
		}

		if !o.Matches(removed) {
			t.Fatal("claim after a watched pre-image did not match")
		}
		rem, err := o.Decode(dispatcher.LedgerEntryChangeContext{Ledger: ledger, ClosedAt: closedAt, Change: removed, IntraLedgerSeq: seq + 1})
		if err != nil {
			t.Fatalf("Decode(removed): %v", err)
		}
		if len(rem) != 1 {
			t.Fatalf("claim emitted %d observations, want 1", len(rem))
		}
		ob := rem[0].(Observation)
		if ob.ClaimableID != wantID || ob.AssetKey != key || ob.Balance.Sign() != 0 || !ob.IsRemoval ||
			ob.Ledger != ledger || !ob.ObservedAt.Equal(closedAt) || ob.IntraLedgerSeq != seq+1 {
			t.Fatalf("removal observation wrong: %+v", ob)
		}

		if _, err := o.Decode(dispatcher.LedgerEntryChangeContext{Ledger: ledger + 1, ClosedAt: closedAt, Change: removed}); !errors.Is(err, ErrNotClaimable) {
			t.Fatalf("cross-ledger claim err = %v, want ErrNotClaimable", err)
		}
	})
}

// FuzzObserverDecodeXDR feeds arbitrary XDR LedgerEntryChanges to a
// fresh observer: Matches must never panic, a removal never matches an
// empty memo, and a matched body change decodes to exactly one
// observation carrying the entry's exact Amount.
func FuzzObserverDecodeXDR(f *testing.F) {
	iss := []byte{1, 2, 3}
	for i, ct := range bodyChangeTypes {
		asset := fuzzCBAsset(false, []byte("USDC"), iss)
		if i%2 == 1 {
			asset = fuzzCBAsset(false, []byte("EURCLONGCODE"), iss)
		}
		raw, err := fuzzCBEntry(ct, []byte{byte(i)}, asset, math.MaxInt64-int64(i)).MarshalBinary()
		if err != nil {
			f.Fatal(err)
		}
		f.Add(raw)
	}
	if raw, err := fuzzCBRemoved([]byte{1}).MarshalBinary(); err == nil {
		f.Add(raw)
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		var change xdr.LedgerEntryChange
		if err := xdr.SafeUnmarshal(raw, &change); err != nil {
			t.Skip()
		}
		o, err := NewObserver([]string{
			refAssetKey(t, []byte("USDC"), iss),
			refAssetKey(t, []byte("EURCLONGCODE"), iss),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !o.Matches(change) {
			return
		}
		if change.Type == xdr.LedgerEntryChangeTypeLedgerEntryRemoved {
			t.Fatal("removal matched an empty pre-image memo")
		}
		outs, err := o.Decode(dispatcher.LedgerEntryChangeContext{Ledger: 1, Change: change})
		if err != nil {
			t.Fatalf("Decode of a matched change: %v", err)
		}
		cb, _ := claimableFromChange(change)
		want := 1
		if change.Type == xdr.LedgerEntryChangeTypeLedgerEntryState {
			want = 0
		}
		if len(outs) != want {
			t.Fatalf("got %d observations, want %d", len(outs), want)
		}
		for _, ev := range outs {
			if ob := ev.(Observation); ob.Balance.Cmp(big.NewInt(int64(cb.Amount))) != 0 {
				t.Fatalf("Balance = %s, want %d", ob.Balance, cb.Amount)
			}
		}
	})
}
