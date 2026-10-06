package spectra

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

func sym(kind string) string { return scval.MustEncodeSymbol(kind) }

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

	w := decodeOneFuzz(t, fuzzEvent([]string{sym(EventWrap), cT, rT}, body, 1, 2))
	if w.Kind != EventWrap || w.Caller != caller || w.Receiver != receiver || w.Owner != "" {
		t.Errorf("wrap slots = %q %s/%s/%s", w.Kind, w.Caller, w.Receiver, w.Owner)
	}
	if w.Shares.String() != "1111" || w.VaultShares.String() != "2222" {
		t.Errorf("wrap shares/vault_shares = %s/%s, want 1111/2222", w.Shares, w.VaultShares)
	}

	u := decodeOneFuzz(t, fuzzEvent([]string{sym(EventUnwrap), cT, rT, oT}, body, 1, 2))
	if u.Kind != EventUnwrap || u.Caller != caller || u.Receiver != receiver || u.Owner != owner {
		t.Errorf("unwrap slots = %q %s/%s/%s", u.Kind, u.Caller, u.Receiver, u.Owner)
	}
	if u.Shares.String() != "1111" || u.VaultShares.String() != "2222" {
		t.Errorf("unwrap shares/vault_shares = %s/%s, want 1111/2222", u.Shares, u.VaultShares)
	}

	if _, err := NewDecoder().Decode(fuzzEvent([]string{sym(EventUnwrap), cT, rT}, body, 1, 2)); err == nil {
		t.Error("unwrap with 3 topics must error")
	}
}

// fuzzContract returns a distinct C-strkey per seed and its ScVal.
func fuzzContract(t *testing.T, seed byte) (string, xdr.ScVal) {
	t.Helper()
	var cid xdr.ContractId
	cid[0], cid[31] = seed, ^seed
	s, err := strkey.Encode(strkey.VersionByteContract, cid[:])
	if err != nil {
		t.Fatal(err)
	}
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
	return s, xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}
}

func u64Val(v uint64) xdr.ScVal {
	u := xdr.Uint64(v)
	return xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &u}
}

// The factory admits a PT, the PT admits its YT, and a YT transfer then
// carries the PT as its market. The attrs survive a restart; the IBT and
// a stranger's announcement admit nothing.
func TestGate_FactoryAnchoredDiscovery(t *testing.T) {
	t.Parallel()
	deployer, depT := fuzzAccount(t, 0x61)
	pt, ptV := fuzzContract(t, 0x71)
	yt, ytV := fuzzContract(t, 0x72)
	ibt, ibtV := fuzzContract(t, 0x73)
	depAddr, err := scval.Parse(depT)
	if err != nil {
		t.Fatal(err)
	}
	ptBody := fuzzB64(t, fuzzMap([]string{"deployer", "duration", "ibt", "pt"},
		[]xdr.ScVal{depAddr, u64Val(86_400), ibtV, ptV}))
	announce := fuzzEvent([]string{sym(EventPTDeployed)}, ptBody, 0, 6)
	ytEv := fuzzEvent([]string{sym(EventYTDeployed)}, fuzzB64(t, fuzzMap([]string{"address"}, []xdr.ScVal{ytV})), 0, 4)
	ytEv.ContractID = pt
	_, toT := fuzzAccount(t, 0x62)
	_, fromT := fuzzAccount(t, 0x63)
	move := fuzzEvent([]string{sym(EventTransfer), fromT, toT}, fuzzB64(t, fuzzI128(0, 5)), 0, 1)
	move.ContractID = yt
	deposit := loadFixture(t, blendHash, "deposit_ibt_63782614_5eaeea223b42_3_CBRT4E.json")
	deposit.ContractID = ibt

	var persisted []string
	saved := map[string]contractid.Attrs{}
	d := NewDecoder(contractid.WithAttrHook(func(child, parent string, _ uint32, a contractid.Attrs) {
		persisted = append(persisted, child+"<"+parent)
		saved[child] = a
	}))

	announce.ContractID = registryID // not the factory
	if d.Matches(announce) {
		t.Fatal("pt_deployed from the registry claimed")
	}
	if d.Matches(ytEv) {
		t.Fatal("yt_deployed from an unannounced PT claimed")
	}
	announce.ContractID = MainnetFactory
	if got := mustDecodeOneWith(t, d, announce); got.MarketPT != pt || got.IBT != ibt || got.Caller != deployer || got.DurationSeconds != 86_400 {
		t.Fatalf("pt_deployed = %+v", got)
	}
	if got := mustDecodeOneWith(t, d, ytEv); got.Role != RolePT || got.MarketPT != pt || got.YT != yt {
		t.Fatalf("yt_deployed = %+v", got)
	}
	if got := mustDecodeOneWith(t, d, move); got.Role != RoleYT || got.MarketPT != pt || got.Amount.String() != "5" {
		t.Fatalf("YT transfer = %+v", got)
	}
	if d.Matches(deposit) {
		t.Fatal("the IBT a pt_deployed names was admitted")
	}
	if want := []string{pt + "<" + MainnetFactory, yt + "<" + pt}; fmt.Sprint(persisted) != fmt.Sprint(want) {
		t.Fatalf("attr hook = %v, want %v", persisted, want)
	}

	restarted := NewDecoder(contractid.WithAttrSeed(saved))
	if got := mustDecodeOneWith(t, restarted, move); got.MarketPT != pt {
		t.Fatalf("after restart YT market = %q", got.MarketPT)
	}
	// A bare protocol_contracts row (no role attr) fails closed.
	bare := NewDecoder(contractid.WithSeed([]string{yt}))
	if bare.Matches(move) {
		t.Fatal("YT admitted without a role was claimed")
	}
}

// pt_deployed naming a hand-kept non-PT is refused rather than re-roled.
func TestGate_PTDeployedCannotReroleCuratedContract(t *testing.T) {
	t.Parallel()
	ev := loadFixture(t, factoryHash, "pt_deployed_factory_63782624_dc730b2a132e_6_CC4ZVR.json")
	sv, err := scval.Parse(ev.Value)
	if err != nil {
		t.Fatal(err)
	}
	m, err := scval.AsMap(sv)
	if err != nil {
		t.Fatal(err)
	}
	ytAddr, err := scval.Parse(loadFixture(t, ptHash, "yt_deployed_pt_63782624_dc730b2a132e_4_CAAOR5.json").Value)
	if err != nil {
		t.Fatal(err)
	}
	ytm, _ := scval.AsMap(ytAddr)
	ytv, _ := scval.MustMapField(ytm, "address")
	for i := range m {
		if *m[i].Key.Sym == "pt" {
			m[i].Val = ytv
		}
	}
	sm := xdr.ScMap(m)
	pm := &sm
	ev.Value = fuzzB64(t, xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pm})
	if _, err := NewDecoder().Decode(ev); !errors.Is(err, ErrMalformedPayload) {
		t.Fatalf("pt_deployed naming a curated YT: err = %v, want ErrMalformedPayload", err)
	}
}

// A registry *_change to an id outside the hand-kept set is loud.
func TestRegistryChange_UnlistedIsAnError(t *testing.T) {
	t.Parallel()
	_, otherV := fuzzContract(t, 0x81)
	for _, name := range []string{
		"factory_change_registry_63778155_79683f438c5d_0_CCUGRA.json",
		"router_change_registry_63778178_df72289e24e9_0_CCUGRA.json",
		"limit_order_engine_change_registry_63780164_ae5298ae3817_0_CCUGRA.json",
	} {
		ev := loadFixture(t, registryHsh, name)
		ev.Value = fuzzB64(t, fuzzMap([]string{"new"}, []xdr.ScVal{otherV}))
		if _, err := NewDecoder().Decode(ev); !errors.Is(err, ErrUnlistedInfrastructure) {
			t.Errorf("%s to an unlisted id: err = %v", name, err)
		}
	}
	h := xdr.ScBytes(make([]byte, 32))
	ev := loadFixture(t, registryHsh, "pt_wasm_hash_change_registry_63778118_ff6b9aca835e_0_CCUGRA.json")
	ev.Value = fuzzB64(t, fuzzMap([]string{"new"}, []xdr.ScVal{{Type: xdr.ScValTypeScvBytes, Bytes: &h}}))
	if _, err := NewDecoder().Decode(ev); !errors.Is(err, ErrUnlistedInfrastructure) {
		t.Errorf("pt_wasm_hash_change to an unknown hash: err = %v", err)
	}
}

// SEP-41 transfer body: bare i128 or CAP-67 map {amount, to_muxed_id}; a
// negative amount or a map without amount is refused.
func TestTransfer_BodyShapes(t *testing.T) {
	t.Parallel()
	base := loadFixture(t, ptHash, "transfer_pt_63784450_db4f7aa725b8_1_CAAOR5.json")
	muxed := xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: func() *xdr.Uint64 { v := xdr.Uint64(9); return &v }()}
	wide := fuzzI128(1, 0) // 2^64: wider than int64
	cases := []struct {
		name string
		body xdr.ScVal
		want string // "" = refused
	}{
		{"cap67 map", fuzzMap([]string{"amount", "to_muxed_id"}, []xdr.ScVal{wide, muxed}), "18446744073709551616"},
		{"bare i128 above int64", wide, "18446744073709551616"},
		{"negative", fuzzI128(-1, ^uint64(0)), ""},
		{"map without amount", fuzzMap([]string{"to_muxed_id"}, []xdr.ScVal{muxed}), ""},
		{"u64 not i128", muxed, ""},
	}
	for _, c := range cases {
		ev := base
		ev.Value = fuzzB64(t, c.body)
		out, err := NewDecoder().Decode(ev)
		if c.want == "" {
			if err == nil {
				t.Errorf("%s: decoded %+v, want refusal", c.name, out)
			}
			continue
		}
		if err != nil || len(out) != 1 || out[0].(Event).Amount.String() != c.want {
			t.Errorf("%s: out=%+v err=%v, want amount %s", c.name, out, err, c.want)
		}
	}
}

// An order id that is not bytes32 is refused.
func TestOrderID_MustBeBytes32(t *testing.T) {
	t.Parallel()
	ev := loadFixture(t, engineHash, "order_filled_order_engine_63784324_2b7c7486b916_4_CCKNOC.json")
	short := xdr.ScBytes(make([]byte, 31))
	ev.Topic = append([]string(nil), ev.Topic...)
	ev.Topic[1] = fuzzB64(t, xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &short})
	if _, err := NewDecoder().Decode(ev); !errors.Is(err, ErrMalformedPayload) {
		t.Fatalf("31-byte order id: err = %v", err)
	}
}
