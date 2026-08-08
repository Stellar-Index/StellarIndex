package chops

import (
	"encoding/base64"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

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

func TestCap67AssetName(t *testing.T) {
	for _, tc := range []struct {
		sep0011 string
		want    string
	}{
		{"native", "native"},
		{"USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
		// Malformed sep0011 falls back to the emitting contract id —
		// honest identity, never a fabricated classic id.
		{"not a real asset name", "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"},
		// 3-topic (no sep0011): pure Soroban token → contract id.
		{"", "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"},
	} {
		ev := cap67TransferEvent(t, tc.sep0011)
		if got := cap67AssetName(&ev); got != tc.want {
			t.Errorf("sep0011=%q: asset = %q, want %q", tc.sep0011, got, tc.want)
		}
	}
}
