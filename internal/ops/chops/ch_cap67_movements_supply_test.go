package chops

import (
	"slices"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41 "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

const (
	cap67Holder     = "GDUY7J7A33TQWOSOQGDO776GGLM3UQERL4J3SPT56F6YS4ID7MLDERI4"
	cap67USDCIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

// cap67SupplyEvent builds a CAP-67 [kind, holder, sep0011] supply event, or
// the bare [kind, holder] SEP-41 shape when sep0011 is "".
func cap67SupplyEvent(t *testing.T, kind, emitter, sep0011 string, body xdr.ScVal) events.Event {
	t.Helper()
	topics := []string{scval.MustEncodeSymbol(kind), encScVal(t, scAddr(t, cap67Holder))}
	if sep0011 != "" {
		topics = append(topics, scval.MustEncodeString(sep0011))
	}
	return events.Event{
		Type:           "contract",
		Ledger:         63_000_000,
		LedgerClosedAt: "2026-08-01T00:00:00Z",
		ContractID:     emitter,
		TxHash:         "bb11223344556677889900112233445566778899001122334455667788990011",
		OperationIndex: 1,
		EventIndex:     2,
		Topic:          topics,
		Value:          encScVal(t, body),
	}
}

// From P23 on a classic payment from an issuer is a CAP-67 `mint` and one to
// it a `burn`; the derive must carry both sides so the issuer's own feed
// shows them, framed as the pre-P23 archive frames a clawback.
func TestCap67SupplyMovement_SACIssuerSide(t *testing.T) {
	canonical.InstallNetworkPassphrase("")
	for _, tc := range []struct {
		kind     string
		from, to string
	}{
		{sep41supply.SymbolMint, cap67USDCIssuer, cap67Holder},
		{sep41supply.SymbolBurn, cap67Holder, cap67USDCIssuer},
		{sep41supply.SymbolClawback, cap67Holder, cap67USDCIssuer},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			ev := cap67SupplyEvent(t, tc.kind, cap67USDCSAC, cap67USDCName, scI128(5_000_000))
			m, ok := cap67EventMovement(sep41.NewUngatedDecoder(), sep41supply.NewUngatedDecoder(), &ev)
			if !ok {
				t.Fatal("cap67EventMovement returned ok=false for a valid SAC supply event")
			}
			if m.MovementKind != tc.kind || m.Provenance != clickhouse.ProvenanceCAP67Derived {
				t.Errorf("kind/provenance = %q/%q, want %q/cap67_derived", m.MovementKind, m.Provenance, tc.kind)
			}
			if m.Asset != "USDC-"+cap67USDCIssuer {
				t.Errorf("asset = %q, want the canonical USDC id", m.Asset)
			}
			if m.FromAddress != tc.from || m.ToAddress != tc.to {
				t.Errorf("from/to = %q/%q, want %q/%q", m.FromAddress, m.ToAddress, tc.from, tc.to)
			}
			if m.Amount == nil || m.Amount.Int64() != 5_000_000 {
				t.Errorf("amount = %v, want 5000000", m.Amount)
			}
			if m.OpIndex != 1 || m.LegIndex != 2 {
				t.Errorf("op/leg = %d/%d, want 1/2", m.OpIndex, m.LegIndex)
			}
			rows := clickhouse.FanOutAccountMovement(m)
			if len(rows) != 2 || !slices.ContainsFunc(rows, func(r clickhouse.AccountMovementRow) bool { return r.Address == cap67USDCIssuer }) {
				t.Errorf("fan-out %+v has no issuer row", rows)
			}
		})
	}
}

// The issuer side is asserted only for a verified SAC: a contract that names
// USDC in its topic is not USDC, and a custom token's admin is not the
// counterparty of record, so only the holder's row is derived.
func TestCap67SupplyMovement_NonSACIsHolderOnly(t *testing.T) {
	canonical.InstallNetworkPassphrase("")
	for _, tc := range []struct {
		name, kind, sep0011 string
		wantDir             clickhouse.AccountMovementDirection
	}{
		{"spoofed USDC mint", sep41supply.SymbolMint, cap67USDCName, clickhouse.AccountMovementReceived},
		{"bare SEP-41 mint", sep41supply.SymbolMint, "", clickhouse.AccountMovementReceived},
		{"spoofed USDC burn", sep41supply.SymbolBurn, cap67USDCName, clickhouse.AccountMovementSent},
		{"clawback", sep41supply.SymbolClawback, "TOKEN", clickhouse.AccountMovementSent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := cap67SupplyEvent(t, tc.kind, cap67NonSAC, tc.sep0011, scI128(7))
			m, ok := cap67EventMovement(sep41.NewUngatedDecoder(), sep41supply.NewUngatedDecoder(), &ev)
			if !ok {
				t.Fatal("cap67EventMovement returned ok=false")
			}
			if m.Asset != cap67NonSAC {
				t.Errorf("asset = %q, want the emitting contract", m.Asset)
			}
			rows := clickhouse.FanOutAccountMovement(m)
			if len(rows) != 1 || rows[0].Address != cap67Holder || rows[0].Direction != tc.wantDir || rows[0].Counterparty != "" {
				t.Errorf("fan-out = %+v, want one %s row for the holder with no counterparty", rows, tc.wantDir)
			}
		})
	}
}

// A muxed or memo-stamped mint carries its amount in a CAP-67 map body.
func TestCap67SupplyMovement_MapBody(t *testing.T) {
	canonical.InstallNetworkPassphrase("")
	amount := scI128(42)
	key := xdr.ScSymbol("amount")
	m := xdr.ScMap{{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &key}, Val: amount}}
	mp := &m
	body := xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
	ev := cap67SupplyEvent(t, sep41supply.SymbolMint, cap67USDCSAC, cap67USDCName, body)
	got, ok := cap67EventMovement(sep41.NewUngatedDecoder(), sep41supply.NewUngatedDecoder(), &ev)
	if !ok || got.Amount == nil || got.Amount.Int64() != 42 {
		t.Fatalf("map-bodied mint: ok=%v amount=%v, want 42", ok, got.Amount)
	}
}
