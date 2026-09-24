package trustlines

import (
	"bytes"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

func fuzzAccountID(seed byte) xdr.AccountId {
	var pub xdr.Uint256
	pub[0] = seed
	pub[31] = 0x5a
	return xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pub}
}

func fuzzTrustLineAsset(code string, issuer xdr.AccountId) xdr.TrustLineAsset {
	if len(code) <= 4 {
		var c [4]byte
		copy(c[:], code)
		return xdr.TrustLineAsset{Type: xdr.AssetTypeAssetTypeCreditAlphanum4, AlphaNum4: &xdr.AlphaNum4{AssetCode: c, Issuer: issuer}}
	}
	var c [12]byte
	copy(c[:], code)
	return xdr.TrustLineAsset{Type: xdr.AssetTypeAssetTypeCreditAlphanum12, AlphaNum12: &xdr.AlphaNum12{AssetCode: c, Issuer: issuer}}
}

func fuzzTrustlineChange(ct xdr.LedgerEntryChangeType, holder xdr.AccountId, asset xdr.TrustLineAsset, balance int64) xdr.LedgerEntryChange {
	if ct == xdr.LedgerEntryChangeTypeLedgerEntryRemoved {
		return xdr.LedgerEntryChange{Type: ct, Removed: &xdr.LedgerKey{
			Type:      xdr.LedgerEntryTypeTrustline,
			TrustLine: &xdr.LedgerKeyTrustLine{AccountId: holder, Asset: asset},
		}}
	}
	entry := &xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type:      xdr.LedgerEntryTypeTrustline,
		TrustLine: &xdr.TrustLineEntry{AccountId: holder, Asset: asset, Balance: xdr.Int64(balance), Limit: math.MaxInt64},
	}}
	c := xdr.LedgerEntryChange{Type: ct}
	switch ct {
	case xdr.LedgerEntryChangeTypeLedgerEntryCreated:
		c.Created = entry
	case xdr.LedgerEntryChangeTypeLedgerEntryUpdated:
		c.Updated = entry
	default:
		c.Restored = entry
	}
	return c
}

// refAssetKey is the reference CODE:ISSUER for a classic credit
// trustline asset: NUL padding stripped from the right only, issuer as
// its G-strkey. ok=false for native / pool-share / unknown variants.
func refAssetKey(a xdr.TrustLineAsset) (string, bool) {
	var code []byte
	var issuer xdr.AccountId
	switch a.Type {
	case xdr.AssetTypeAssetTypeCreditAlphanum4:
		code, issuer = a.AlphaNum4.AssetCode[:], a.AlphaNum4.Issuer
	case xdr.AssetTypeAssetTypeCreditAlphanum12:
		code, issuer = a.AlphaNum12.AssetCode[:], a.AlphaNum12.Issuer
	default:
		return "", false
	}
	return strings.TrimRight(string(code), "\x00") + ":" + issuer.Address(), true
}

// FuzzAssetKeyFromTrustLineAsset checks the watched-asset key: identity
// is (code, issuer) — the issuer bytes round-trip through the strkey, the
// code is exactly the unpadded asset code — and non-credit variants are
// rejected rather than keyed.
func FuzzAssetKeyFromTrustLineAsset(f *testing.F) {
	f.Add(uint8(1), []byte("USDC"), bytes.Repeat([]byte{7}, 32))
	f.Add(uint8(2), []byte("LONGCODE12AB"), bytes.Repeat([]byte{9}, 32))
	f.Add(uint8(2), []byte("A\x00B"), make([]byte, 32))
	f.Add(uint8(1), []byte{}, make([]byte, 32))
	f.Add(uint8(0), []byte("XLM"), make([]byte, 32))
	f.Add(uint8(3), []byte("POOL"), make([]byte, 32))

	f.Fuzz(func(t *testing.T, typ uint8, code, issuerBytes []byte) {
		var pub xdr.Uint256
		copy(pub[:], issuerBytes)
		issuer := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pub}
		var asset xdr.TrustLineAsset
		switch typ % 4 {
		case 0:
			asset = xdr.TrustLineAsset{Type: xdr.AssetTypeAssetTypeNative}
		case 1:
			var c [4]byte
			copy(c[:], code)
			asset = xdr.TrustLineAsset{Type: xdr.AssetTypeAssetTypeCreditAlphanum4, AlphaNum4: &xdr.AlphaNum4{AssetCode: c, Issuer: issuer}}
		case 2:
			var c [12]byte
			copy(c[:], code)
			asset = xdr.TrustLineAsset{Type: xdr.AssetTypeAssetTypeCreditAlphanum12, AlphaNum12: &xdr.AlphaNum12{AssetCode: c, Issuer: issuer}}
		default:
			asset = xdr.TrustLineAsset{Type: xdr.AssetTypeAssetTypePoolShare, LiquidityPoolId: &xdr.PoolId{}}
		}

		got, err := assetKeyFromTrustLineAsset(asset)
		want, ok := refAssetKey(asset)
		if !ok {
			if err == nil {
				t.Fatalf("%s asset keyed as %q, want an error", asset.Type, got)
			}
			return
		}
		if err != nil || got != want {
			t.Fatalf("key=%q err=%v, want %q", got, err, want)
		}
		sep := strings.LastIndexByte(got, ':')
		raw, derr := strkey.Decode(strkey.VersionByteAccountID, got[sep+1:])
		if derr != nil || !bytes.Equal(raw, pub[:]) {
			t.Fatalf("issuer %q does not round-trip to %x (err %v)", got[sep+1:], pub, derr)
		}
		if isClassicCreditAsset(asset.Type) != ok {
			t.Fatalf("isClassicCreditAsset(%s) disagrees with the keyer", asset.Type)
		}
	})
}

// FuzzObserverDecode drives arbitrary LedgerEntryChange XDR through the
// observer. Properties: Matches fires exactly for a watched classic
// credit trustline in any change variant (same code under another issuer
// never matches); a matched change always decodes to one Observation
// with the holder, the watched key and the entry's int64 balance
// verbatim, and a removal carries a zero balance with IsRemoval set.
func FuzzObserverDecode(f *testing.F) {
	issuer, impostor := fuzzAccountID(0x10), fuzzAccountID(0x11)
	holder := fuzzAccountID(0x20)
	watchedAssets := []xdr.TrustLineAsset{fuzzTrustLineAsset("USDC", issuer), fuzzTrustLineAsset("LONGCODE12", issuer)}
	for _, ct := range []xdr.LedgerEntryChangeType{
		xdr.LedgerEntryChangeTypeLedgerEntryCreated,
		xdr.LedgerEntryChangeTypeLedgerEntryUpdated,
		xdr.LedgerEntryChangeTypeLedgerEntryRestored,
		xdr.LedgerEntryChangeTypeLedgerEntryRemoved,
	} {
		for _, asset := range append(watchedAssets[:2:2], fuzzTrustLineAsset("USDC", impostor), xdr.TrustLineAsset{Type: xdr.AssetTypeAssetTypeNative}) {
			for _, bal := range []int64{0, math.MaxInt64} {
				b, err := fuzzTrustlineChange(ct, holder, asset, bal).MarshalBinary()
				if err != nil {
					f.Fatal(err)
				}
				f.Add(b)
			}
		}
	}

	var keys []string
	for _, a := range watchedAssets {
		k, _ := refAssetKey(a)
		keys = append(keys, k)
	}
	obs, err := NewObserver(keys)
	if err != nil {
		f.Fatal(err)
	}
	closedAt := time.Unix(1_700_000_000, 0).UTC()

	f.Fuzz(func(t *testing.T, changeXDR []byte) {
		var change xdr.LedgerEntryChange
		if err := xdr.SafeUnmarshal(changeXDR, &change); err != nil {
			return
		}
		var (
			wantHolder xdr.AccountId
			asset      xdr.TrustLineAsset
			tl         *xdr.TrustLineEntry
			isTL       bool
		)
		switch change.Type {
		case xdr.LedgerEntryChangeTypeLedgerEntryRemoved:
			if k, ok := change.MustRemoved().GetTrustLine(); ok {
				wantHolder, asset, isTL = k.AccountId, k.Asset, true
			}
		case xdr.LedgerEntryChangeTypeLedgerEntryCreated, xdr.LedgerEntryChangeTypeLedgerEntryUpdated, xdr.LedgerEntryChangeTypeLedgerEntryRestored:
			var e *xdr.LedgerEntry
			switch change.Type {
			case xdr.LedgerEntryChangeTypeLedgerEntryCreated:
				e = change.Created
			case xdr.LedgerEntryChangeTypeLedgerEntryUpdated:
				e = change.Updated
			default:
				e = change.Restored
			}
			if v, ok := e.Data.GetTrustLine(); ok {
				tl = &v
				wantHolder, asset, isTL = v.AccountId, v.Asset, true
			}
		}
		wantKey, credit := refAssetKey(asset)
		wantMatch := isTL && credit && (wantKey == keys[0] || wantKey == keys[1])
		if got := obs.Matches(change); got != wantMatch {
			t.Fatalf("Matches=%v, want %v (key=%q)", got, wantMatch, wantKey)
		}
		if !wantMatch {
			return
		}

		evs, err := obs.Decode(dispatcher.LedgerEntryChangeContext{
			Ledger: 11, ClosedAt: closedAt, IntraLedgerSeq: 4, Change: change,
		})
		if err != nil {
			t.Fatalf("matched change failed to decode: %v", err)
		}
		if len(evs) != 1 {
			t.Fatalf("got %d events, want 1", len(evs))
		}
		o, ok := evs[0].(Observation)
		if !ok {
			t.Fatalf("event is %T, want Observation", evs[0])
		}
		if o.AccountID != wantHolder.Address() || o.AssetKey != wantKey || o.Ledger != 11 ||
			!o.ObservedAt.Equal(closedAt) || o.IntraLedgerSeq != 4 {
			t.Fatalf("bad observation %+v", o)
		}
		if tl == nil {
			if !o.IsRemoval || o.Balance.Sign() != 0 {
				t.Fatalf("removal observation %+v, want IsRemoval with zero balance", o)
			}
			return
		}
		if o.IsRemoval || o.Balance.Cmp(big.NewInt(int64(tl.Balance))) != 0 {
			t.Fatalf("observation removal=%v balance=%s, want false/%d", o.IsRemoval, o.Balance, tl.Balance)
		}
	})
}
