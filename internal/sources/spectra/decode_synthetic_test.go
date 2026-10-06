package spectra

import (
	"encoding/base64"
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

func i128Want(hi int64, lo uint64) *big.Int {
	v := new(big.Int).Lsh(big.NewInt(hi), 64)
	return v.Add(v, new(big.Int).SetUint64(lo))
}

func fuzzI128(hi int64, lo uint64) xdr.ScVal {
	p := xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
}

func fuzzB64(t *testing.T, sv xdr.ScVal) string {
	t.Helper()
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func fuzzMap(names []string, vals []xdr.ScVal) xdr.ScVal {
	m := make(xdr.ScMap, len(names))
	for i := range names {
		sym := xdr.ScSymbol(names[i])
		m[i] = xdr.ScMapEntry{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}, Val: vals[i]}
	}
	pm := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pm}
}

// fuzzAccount returns a distinct G-strkey per seed and its topic encoding.
func fuzzAccount(t *testing.T, seed byte) (string, string) {
	t.Helper()
	var pub xdr.Uint256
	pub[0], pub[31] = seed, ^seed
	s, err := strkey.Encode(strkey.VersionByteAccountID, pub[:])
	if err != nil {
		t.Fatal(err)
	}
	aid := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pub}
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}
	return s, fuzzB64(t, xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr})
}

func fuzzEvent(topics []string, body string, op, ev uint16) events.Event {
	return events.Event{
		Type:           "contract",
		ContractID:     wrapperID,
		Ledger:         64716536,
		LedgerClosedAt: "2026-10-01T15:27:42Z",
		TxHash:         "f7d5d536ff4d0000000000000000000000000000000000000000000000000000",
		OperationIndex: int(op),
		EventIndex:     int(ev),
		Topic:          topics,
		Value:          body,
	}
}

func decodeOneFuzz(t *testing.T, ev events.Event) Event {
	t.Helper()
	return mustDecodeOne(t, ev)
}

// Synthetic: distinct caller/receiver/owner and shares != vault_shares, so a
// swapped topic index or body field fails. Unwrap's shape is from source, not
// a captured fixture.
func TestSynthetic_wrapUnwrapFieldSlots(t *testing.T) {
	t.Parallel()
	caller, cT := fuzzAccount(t, 0x41)
	receiver, rT := fuzzAccount(t, 0x42)
	owner, oT := fuzzAccount(t, 0x43)
	body := fuzzB64(t, fuzzMap([]string{"shares", "vault_shares"},
		[]xdr.ScVal{fuzzI128(0, 1_111), fuzzI128(0, 2_222)}))

	w := decodeOneFuzz(t, fuzzEvent([]string{TopicSymbolWrap, cT, rT}, body, 1, 2))
	if w.Kind != EventWrap || w.Caller != caller || w.Receiver != receiver || w.Owner != "" {
		t.Errorf("wrap slots = %q %s/%s/%s", w.Kind, w.Caller, w.Receiver, w.Owner)
	}
	if w.Shares.String() != "1111" || w.VaultShares.String() != "2222" {
		t.Errorf("wrap shares/vault_shares = %s/%s, want 1111/2222", w.Shares, w.VaultShares)
	}

	u := decodeOneFuzz(t, fuzzEvent([]string{TopicSymbolUnwrap, cT, rT, oT}, body, 1, 2))
	if u.Kind != EventUnwrap || u.Caller != caller || u.Receiver != receiver || u.Owner != owner {
		t.Errorf("unwrap slots = %q %s/%s/%s", u.Kind, u.Caller, u.Receiver, u.Owner)
	}
	if u.Shares.String() != "1111" || u.VaultShares.String() != "2222" {
		t.Errorf("unwrap shares/vault_shares = %s/%s, want 1111/2222", u.Shares, u.VaultShares)
	}

	if _, err := NewDecoder().Decode(fuzzEvent([]string{TopicSymbolUnwrap, cT, rT}, body, 1, 2)); err == nil {
		t.Error("unwrap with 3 topics must error")
	}
}
