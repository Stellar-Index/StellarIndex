package chops

import (
	"encoding/base64"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	sep41 "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
)

func encScVal(t *testing.T, sv xdr.ScVal) string {
	t.Helper()
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal scval: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func scAddr(t *testing.T, g string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, g)
	if err != nil {
		t.Fatalf("decode %q: %v", g, err)
	}
	var pub xdr.Uint256
	copy(pub[:], raw)
	aid := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pub}
	a := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &a}
}

func scI128(n int64) xdr.ScVal {
	p := xdr.Int128Parts{Hi: 0, Lo: xdr.Uint64(n)} //nolint:gosec // test literal
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
}

// cap67TransferEvent builds a decodable CAP-67 transfer event — the
// 4-topic classic-asset shape when sep0011 != "", else the 3-topic pure
// Soroban-token shape.
func cap67TransferEvent(t *testing.T, sep0011 string) events.Event {
	t.Helper()
	topics := []string{
		scval.MustEncodeSymbol("transfer"),
		encScVal(t, scAddr(t, "GDUY7J7A33TQWOSOQGDO776GGLM3UQERL4J3SPT56F6YS4ID7MLDERI4")),
		encScVal(t, scAddr(t, "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")),
	}
	if sep0011 != "" {
		topics = append(topics, scval.MustEncodeString(sep0011))
	}
	return events.Event{
		Type:           "contract",
		Ledger:         63_000_000,
		LedgerClosedAt: "2026-08-01T00:00:00Z",
		ContractID:     "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		TxHash:         "aa11223344556677889900112233445566778899001122334455667788990011",
		Topic:          topics,
		Value:          encScVal(t, scI128(12_345_678)),
	}
}

// TestCap67MovementFromEvent_NativeClassic — the founding case
// (inventory #1): a native-XLM CAP-67 transfer, emitted by the
// deliberately-unwatched native SAC, must decode into a movement with
// asset "native" and cap67 provenance.
func TestCap67MovementFromEvent_NativeClassic(t *testing.T) {
	dec := sep41.NewUngatedDecoder()
	ev := cap67TransferEvent(t, "native")

	m, ok := cap67MovementFromEvent(dec, &ev)
	if !ok {
		t.Fatal("cap67MovementFromEvent returned ok=false for a valid native transfer")
	}
	if m.Asset != "native" {
		t.Errorf("Asset = %q, want native", m.Asset)
	}
	if m.Provenance != "cap67_derived" {
		t.Errorf("Provenance = %q, want cap67_derived", m.Provenance)
	}
	if m.MovementKind != "transfer" {
		t.Errorf("MovementKind = %q, want transfer", m.MovementKind)
	}
	if m.FromAddress == "" || m.ToAddress == "" || m.Amount == nil {
		t.Fatalf("incomplete movement: from=%q to=%q amount=%v", m.FromAddress, m.ToAddress, m.Amount)
	}
	if m.Amount.Int64() != 12_345_678 {
		t.Errorf("Amount = %s, want 12345678", m.Amount)
	}
	// The ungated decoder must never be dispatcher-wireable by accident.
	if dec.Matches(ev) {
		t.Error("NewUngatedDecoder().Matches returned true — it must stay un-wireable in gated paths")
	}
}

const (
	cap67NativeSAC = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	cap67USDCSAC   = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	cap67USDCName  = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	// A WASM contract that is nobody's SAC (the Blend pool fixture id).
	cap67NonSAC = "CDVQVKOY2YSXS2IC7KN6MNASSHPAO7UN2UR2ON4OI2SKMFJNVAMDX6DP"
)

func TestCap67AssetName(t *testing.T) {
	canonical.InstallNetworkPassphrase("")
	for _, tc := range []struct {
		emitter string
		sep0011 string
		want    string
	}{
		{cap67NativeSAC, "native", "native"},
		{cap67USDCSAC, cap67USDCName, "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
		// Malformed sep0011 falls back to the emitting contract id —
		// honest identity, never a fabricated classic id.
		{cap67NativeSAC, "not a real asset name", cap67NativeSAC},
		// 3-topic (no sep0011): pure Soroban token → contract id.
		{cap67NativeSAC, "", cap67NativeSAC},
	} {
		ev := cap67TransferEvent(t, tc.sep0011)
		ev.ContractID = tc.emitter
		if got := cap67AssetName(&ev); got != tc.want {
			t.Errorf("emitter=%s sep0011=%q: asset = %q, want %q", tc.emitter, tc.sep0011, got, tc.want)
		}
	}
}

// TestCap67AssetName_SpoofedTopicRendersAsEmitter — any contract can emit a
// 4-topic transfer whose trailing topic names a trusted asset. The label is
// trusted only when the emitter IS that asset's SAC; otherwise the row is the
// emitting contract's own token, never the asset it impersonates.
func TestCap67AssetName_SpoofedTopicRendersAsEmitter(t *testing.T) {
	canonical.InstallNetworkPassphrase("")
	for _, tc := range []struct {
		name, emitter, sep0011 string
	}{
		{"non-SAC claims USDC", cap67NonSAC, cap67USDCName},
		{"non-SAC claims native", cap67NonSAC, "native"},
		{"native SAC claims USDC", cap67NativeSAC, cap67USDCName},
		{"USDC SAC claims native", cap67USDCSAC, "native"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := cap67TransferEvent(t, tc.sep0011)
			ev.ContractID = tc.emitter
			m, ok := cap67MovementFromEvent(sep41.NewUngatedDecoder(), &ev)
			if !ok {
				t.Fatal("cap67MovementFromEvent returned ok=false")
			}
			if m.Asset != tc.emitter {
				t.Errorf("asset = %q, want the emitting contract %q", m.Asset, tc.emitter)
			}
		})
	}
}

// TestCap67AssetName_NetworkAware — the SAC cross-check derives against the
// -network the job runs with: on testnet the pubnet native SAC is not XLM's
// SAC, and the testnet one is.
func TestCap67AssetName_NetworkAware(t *testing.T) {
	pass, err := cap67NetworkPassphrase("testnet")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { canonical.InstallNetworkPassphrase("") })
	canonical.InstallNetworkPassphrase(pass)
	testnetNativeSAC, err := canonical.NativeAsset().SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	if testnetNativeSAC == cap67NativeSAC {
		t.Fatal("testnet native SAC equals pubnet's — passphrase not applied")
	}
	for emitter, want := range map[string]string{
		testnetNativeSAC: "native",
		cap67NativeSAC:   cap67NativeSAC,
	} {
		ev := cap67TransferEvent(t, "native")
		ev.ContractID = emitter
		if got := cap67AssetName(&ev); got != want {
			t.Errorf("emitter=%s: asset = %q, want %q", emitter, got, want)
		}
	}
}

func TestCap67NetworkPassphrase(t *testing.T) {
	for name, want := range map[string]string{
		"pubnet":    canonical.PubnetPassphrase,
		"testnet":   canonical.TestnetPassphrase,
		"futurenet": canonical.FuturenetPassphrase,
	} {
		if got, err := cap67NetworkPassphrase(name); err != nil || got != want {
			t.Errorf("%s: got (%q, %v), want %q", name, got, err, want)
		}
	}
	if _, err := cap67NetworkPassphrase("mainnet"); err == nil {
		t.Error("unknown network accepted; want an error")
	}
}
