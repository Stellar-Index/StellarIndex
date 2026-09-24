package liquidity_pools

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

// Generative runs: go test -run=^$ -fuzz=^FuzzXxx$ -fuzztime=60s ./internal/sources/liquidity_pools

var bodyChangeTypes = []xdr.LedgerEntryChangeType{
	xdr.LedgerEntryChangeTypeLedgerEntryCreated,
	xdr.LedgerEntryChangeTypeLedgerEntryUpdated,
	xdr.LedgerEntryChangeTypeLedgerEntryRestored,
	xdr.LedgerEntryChangeTypeLedgerEntryState,
}

func fuzzLPAsset(native bool, code, issuer []byte) xdr.Asset {
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

// refAssetKey is the test's independent CODE:ISSUER reference for a
// credit asset: the code with trailing NUL padding stripped, then the
// issuer's G-strkey.
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

func fuzzLPEntry(ct xdr.LedgerEntryChangeType, pool []byte, a, b xdr.Asset, ra, rb int64) xdr.LedgerEntryChange {
	var pid xdr.PoolId
	copy(pid[:], pool)
	entry := &xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeLiquidityPool,
		LiquidityPool: &xdr.LiquidityPoolEntry{
			LiquidityPoolId: pid,
			Body: xdr.LiquidityPoolEntryBody{
				Type: xdr.LiquidityPoolTypeLiquidityPoolConstantProduct,
				ConstantProduct: &xdr.LiquidityPoolEntryConstantProduct{
					Params:   xdr.LiquidityPoolConstantProductParameters{AssetA: a, AssetB: b, Fee: 30},
					ReserveA: xdr.Int64(ra),
					ReserveB: xdr.Int64(rb),
				},
			},
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

func fuzzLPRemoved(pool []byte) xdr.LedgerEntryChange {
	var pid xdr.PoolId
	copy(pid[:], pool)
	return xdr.LedgerEntryChange{
		Type: xdr.LedgerEntryChangeTypeLedgerEntryRemoved,
		Removed: &xdr.LedgerKey{
			Type:          xdr.LedgerEntryTypeLiquidityPool,
			LiquidityPool: &xdr.LedgerKeyLiquidityPool{LiquidityPoolId: pid},
		},
	}
}

// FuzzObserverLifecycle drives one pool through a body change and the
// removal that follows it, over arbitrary asset shapes, reserves and
// watch sets. It asserts the served-supply contract end to end:
// Matches iff a credit side is watched; one observation per watched side
// carrying that side's exact reserve as *big.Int (ADR-0003) and the
// change's position; nothing from a STATE pre-image; the removal
// zeroes exactly the sides the pre-image named; and a removal whose
// pre-image is from another ledger is an error, never a silent skip.
func FuzzObserverLifecycle(f *testing.F) {
	iss := []byte{1, 2, 3}
	f.Add(uint8(1), []byte{0xaa}, false, []byte("USDC"), iss, true, []byte(nil), []byte(nil), int64(10_000_000), int64(5), true, false, uint32(100), uint32(3))
	f.Add(uint8(0), []byte{0xbb}, false, []byte("USDC"), iss, false, []byte("EURCLONGCODE"), []byte{9}, int64(math.MaxInt64), int64(math.MaxInt32)+1, true, true, uint32(1), uint32(0))
	f.Add(uint8(3), []byte{0xcc}, false, []byte("AQUA"), iss, false, []byte("AQUA"), iss, int64(1)<<53+1, int64(0), true, true, uint32(7), uint32(9))
	f.Add(uint8(2), []byte{0xdd}, true, []byte(nil), []byte(nil), false, []byte("yXLM"), iss, int64(0), int64(-1), false, true, uint32(math.MaxUint32-1), uint32(1))
	f.Add(uint8(1), []byte{0xee}, false, []byte("A\x00B"), iss, true, []byte(nil), []byte(nil), int64(42), int64(42), false, false, uint32(5), uint32(5))

	closedAt := time.Unix(1_700_000_000, 0).UTC()
	f.Fuzz(func(t *testing.T, sel uint8, pool []byte,
		nativeA bool, codeA, issA []byte,
		nativeB bool, codeB, issB []byte,
		ra, rb int64, watchA, watchB bool, ledger, seq uint32,
	) {
		if len(codeA) > 12 || len(codeB) > 12 || ledger == math.MaxUint32 {
			t.Skip()
		}
		ct := bodyChangeTypes[int(sel)%len(bodyChangeTypes)]
		a, b := fuzzLPAsset(nativeA, codeA, issA), fuzzLPAsset(nativeB, codeB, issB)

		watched := []string{"SENTINEL:" + gIssuerA}
		type side struct {
			key     string
			reserve int64
		}
		var want []side
		invalidWatched := false
		for _, s := range []struct {
			native, watch bool
			code, iss     []byte
			asset         xdr.Asset
			reserve       int64
		}{{nativeA, watchA, codeA, issA, a, ra}, {nativeB, watchB, codeB, issB, b, rb}} {
			if s.native {
				if _, err := assetKeyFromAsset(s.asset); !errors.Is(err, ErrUnsupportedLPAsset) {
					t.Fatalf("native side key err = %v, want ErrUnsupportedLPAsset", err)
				}
				continue
			}
			key := refAssetKey(t, s.code, s.iss)
			got, err := assetKeyFromAsset(s.asset)
			if err != nil || got != key {
				t.Fatalf("assetKeyFromAsset = %q, %v; want %q", got, err, key)
			}
			// Parity with the canonical asset the watch list is canonicalised through.
			ca, cerr := canonical.AssetFromXDR(s.asset)
			if cerr == nil && ca.Code+":"+ca.Issuer != key {
				t.Fatalf("key %q disagrees with canonical %s:%s", key, ca.Code, ca.Issuer)
			}
			if s.watch {
				watched = append(watched, key)
				invalidWatched = invalidWatched || cerr != nil
			}
		}
		o, err := NewObserver(watched)
		// A code stellar-core would reject can never be configured: the watch list must refuse it.
		if invalidWatched {
			if err == nil {
				t.Fatalf("NewObserver accepted non-canonical watched keys %q", watched)
			}
			return
		}
		if err != nil {
			t.Fatalf("NewObserver: %v", err)
		}
		set := map[string]bool{}
		for _, k := range watched {
			set[k] = true
		}
		for _, s := range []struct {
			native bool
			code   []byte
			iss    []byte
			res    int64
		}{{nativeA, codeA, issA, ra}, {nativeB, codeB, issB, rb}} {
			if s.native {
				continue
			}
			if k := refAssetKey(t, s.code, s.iss); set[k] {
				want = append(want, side{k, s.res})
			}
		}

		change := fuzzLPEntry(ct, pool, a, b, ra, rb)
		if got := o.Matches(change); got != (len(want) > 0) {
			t.Fatalf("Matches = %v, want %v (watched sides %v)", got, len(want) > 0, want)
		}
		removed := fuzzLPRemoved(pool)
		if len(want) == 0 {
			if o.Matches(removed) {
				t.Fatal("removal of an unwatched pool matched")
			}
			return
		}

		var pid [32]byte
		copy(pid[:], pool)
		wantPool := hex.EncodeToString(pid[:])
		outs, err := o.Decode(dispatcher.LedgerEntryChangeContext{Ledger: ledger, ClosedAt: closedAt, Change: change, IntraLedgerSeq: seq})
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if ct == xdr.LedgerEntryChangeTypeLedgerEntryState {
			if len(outs) != 0 {
				t.Fatalf("STATE pre-image emitted %d observations", len(outs))
			}
		} else {
			if len(outs) != len(want) {
				t.Fatalf("got %d observations, want %d", len(outs), len(want))
			}
			for i, ev := range outs {
				ob := ev.(Observation)
				if ob.AssetKey != want[i].key || ob.Balance.Cmp(big.NewInt(want[i].reserve)) != 0 {
					t.Fatalf("obs[%d] = %s %s, want %s %d", i, ob.AssetKey, ob.Balance, want[i].key, want[i].reserve)
				}
				if ob.PoolID != wantPool || ob.Ledger != ledger || !ob.ObservedAt.Equal(closedAt) || ob.IntraLedgerSeq != seq || ob.IsRemoval {
					t.Fatalf("obs[%d] context wrong: %+v", i, ob)
				}
			}
		}

		if !o.Matches(removed) {
			t.Fatal("removal after a watched pre-image did not match")
		}
		rem, err := o.Decode(dispatcher.LedgerEntryChangeContext{Ledger: ledger, ClosedAt: closedAt, Change: removed, IntraLedgerSeq: seq + 1})
		if err != nil {
			t.Fatalf("Decode(removed): %v", err)
		}
		if len(rem) != len(want) {
			t.Fatalf("removal emitted %d observations, want %d", len(rem), len(want))
		}
		for i, ev := range rem {
			ob := ev.(Observation)
			if ob.AssetKey != want[i].key || ob.Balance.Sign() != 0 || !ob.IsRemoval ||
				ob.PoolID != wantPool || ob.Ledger != ledger || !ob.ObservedAt.Equal(closedAt) || ob.IntraLedgerSeq != seq+1 {
				t.Fatalf("removal obs[%d] wrong: %+v", i, ob)
			}
		}

		// The memo is ledger-scoped: the same removal one ledger later has
		// no attributable pre-image and must surface as a decode error.
		if o.Matches(removed) {
			if _, err := o.Decode(dispatcher.LedgerEntryChangeContext{Ledger: ledger + 1, ClosedAt: closedAt, Change: removed}); !errors.Is(err, ErrUnsupportedLPType) {
				t.Fatalf("cross-ledger removal err = %v, want ErrUnsupportedLPType", err)
			}
		}
	})
}

// FuzzObserverDecodeXDR feeds arbitrary XDR LedgerEntryChanges to a
// fresh observer: Matches must never panic, a removal can never match
// an empty memo, and any matched body change decodes to observations
// whose balances are exactly the reserve of the side they name.
func FuzzObserverDecodeXDR(f *testing.F) {
	iss := []byte{1, 2, 3}
	usdc := fuzzLPAsset(false, []byte("USDC"), iss)
	long := fuzzLPAsset(false, []byte("EURCLONGCODE"), iss)
	for i, ct := range bodyChangeTypes {
		raw, err := fuzzLPEntry(ct, []byte{byte(i)}, usdc, long, math.MaxInt64, 12_345).MarshalBinary()
		if err != nil {
			f.Fatal(err)
		}
		f.Add(raw)
	}
	if raw, err := fuzzLPRemoved([]byte{1}).MarshalBinary(); err == nil {
		f.Add(raw)
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		var change xdr.LedgerEntryChange
		if err := xdr.SafeUnmarshal(raw, &change); err != nil {
			t.Skip()
		}
		watchedKeys := []string{
			refAssetKey(t, []byte("USDC"), iss),
			refAssetKey(t, []byte("EURCLONGCODE"), iss),
		}
		o, err := NewObserver(watchedKeys)
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
		lp, _ := lpFromChange(change)
		cp := lp.Body.ConstantProduct
		reserveOf := map[string][]int64{}
		for _, s := range []struct {
			a xdr.Asset
			r xdr.Int64
		}{{cp.Params.AssetA, cp.ReserveA}, {cp.Params.AssetB, cp.ReserveB}} {
			if k, err := assetKeyFromAsset(s.a); err == nil {
				reserveOf[k] = append(reserveOf[k], int64(s.r))
			}
		}
		for _, ev := range outs {
			ob := ev.(Observation)
			ok := false
			for _, r := range reserveOf[ob.AssetKey] {
				ok = ok || ob.Balance.Cmp(big.NewInt(r)) == 0
			}
			if !ok {
				t.Fatalf("observation %s=%s matches no side's reserve %v", ob.AssetKey, ob.Balance, reserveOf)
			}
		}
	})
}

// A ConstantProduct-typed body with no ConstantProduct arm (only
// constructible in memory, not over the wire) must be skipped, not
// dereferenced.
func TestObserver_MatchesNilConstantProductIsFalse(t *testing.T) {
	o, err := NewObserver([]string{"USDC:" + gIssuerA})
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	change := xdr.LedgerEntryChange{
		Type: xdr.LedgerEntryChangeTypeLedgerEntryUpdated,
		Updated: &xdr.LedgerEntry{Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeLiquidityPool,
			LiquidityPool: &xdr.LiquidityPoolEntry{Body: xdr.LiquidityPoolEntryBody{
				Type: xdr.LiquidityPoolTypeLiquidityPoolConstantProduct,
			}},
		}},
	}
	if o.Matches(change) {
		t.Fatal("Matches = true for a body with no ConstantProduct arm")
	}
}
