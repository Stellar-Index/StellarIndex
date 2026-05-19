package defindex

import (
	"encoding/base64"
	"errors"
	"math/big"
	"testing"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/RatesEngine/rates-engine/internal/events"
)

// TestClassify_depositWithdraw covers the topic-byte equality path —
// ensures topic[0] = ScvString("BlendStrategy") + topic[1] in
// {deposit, withdraw} is the only thing the decoder picks up.
// Verifies the byte-equality constants line up with the SDK encoder.
func TestClassify_depositWithdraw(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		topic     []string
		wantClass string
	}{
		{
			name:      "deposit",
			topic:     []string{TopicPrefixStrategy, TopicSymbolDeposit},
			wantClass: EventDeposit,
		},
		{
			name:      "withdraw",
			topic:     []string{TopicPrefixStrategy, TopicSymbolWithdraw},
			wantClass: EventWithdraw,
		},
		{
			name:      "wrong prefix (SoroswapPair)",
			topic:     []string{mustB64String(t, "SoroswapPair"), TopicSymbolDeposit},
			wantClass: "",
		},
		{
			name:      "prefix as Symbol not String",
			topic:     []string{mustB64Symbol(t, "BlendStrategy"), TopicSymbolDeposit},
			wantClass: "",
		},
		{
			name:      "harvest (not Phase A)",
			topic:     []string{TopicPrefixStrategy, mustB64Symbol(t, "harvest")},
			wantClass: "",
		},
		{
			name:      "single-element topic",
			topic:     []string{TopicPrefixStrategy},
			wantClass: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := &events.Event{Topic: tc.topic}
			got := classify(ev)
			if got != tc.wantClass {
				t.Errorf("classify = %q, want %q", got, tc.wantClass)
			}
		})
	}
}

// TestDecodeFlow_deposit covers the happy-path decode of a deposit
// event with an account (G-strkey) `from`. Verifies amount
// preservation (no truncation per ADR-0003) and address round-trip.
func TestDecodeFlow_deposit(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Type:           "contract",
		Ledger:         60_000_000,
		LedgerClosedAt: "2026-05-14T10:30:00Z",
		ContractID:     "CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP",
		OperationIndex: 2,
		TxHash:         "abc123",
		Topic:          []string{TopicPrefixStrategy, TopicSymbolDeposit},
		Value: mustB64(t, mapSCVal(t,
			mapEntry(t, "from", addrSCVal(makeAccountAddress(t, 0xAA))),
			mapEntry(t, "amount", i128SCVal(big.NewInt(123_456_789_000))),
		)),
	}
	flow, err := decodeFlow(ev, EventDeposit)
	if err != nil {
		t.Fatalf("decodeFlow: %v", err)
	}
	if flow.Source != SourceName {
		t.Errorf("Source = %q, want %q", flow.Source, SourceName)
	}
	if flow.Direction != DirectionDeposit {
		t.Errorf("Direction = %q, want deposit", flow.Direction)
	}
	if flow.From == "" || flow.From[0] != 'G' {
		t.Errorf("From = %q, want a G-strkey account address", flow.From)
	}
	if got, want := flow.Amount.String(), "123456789000"; got != want {
		t.Errorf("Amount = %q, want %q (no truncation)", got, want)
	}
	if flow.Ledger != 60_000_000 || flow.OpIndex != 2 || flow.TxHash != "abc123" {
		t.Errorf("header fields not preserved: %+v", flow)
	}
}

// TestDecodeFlow_withdrawFromContract covers the withdraw branch
// AND the real-world case where `from` is the vault/router
// *contract* (a C-strkey), not an end-user account — exactly what
// scan-soroban-events observed on mainnet. The body shape is
// identical to deposit; only Direction differs.
func TestDecodeFlow_withdrawFromContract(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Type:           "contract",
		Ledger:         60_000_001,
		LedgerClosedAt: "2026-05-14T10:31:00Z",
		ContractID:     "CC5CE6MWISDXT3MLNQ7R3FVILFVFEIH3COWGH45GJKL6BD2ZHF7F7JVI",
		Topic:          []string{TopicPrefixStrategy, TopicSymbolWithdraw},
		Value: mustB64(t, mapSCVal(t,
			mapEntry(t, "from", addrSCVal(makeContractAddress(t, 0xBB))),
			mapEntry(t, "amount", i128SCVal(big.NewInt(29_999_999))),
		)),
	}
	flow, err := decodeFlow(ev, EventWithdraw)
	if err != nil {
		t.Fatalf("decodeFlow: %v", err)
	}
	if flow.Direction != DirectionWithdraw {
		t.Errorf("Direction = %q, want withdraw", flow.Direction)
	}
	if flow.From == "" || flow.From[0] != 'C' {
		t.Errorf("From = %q, want a C-strkey contract address", flow.From)
	}
	if got, want := flow.Amount.String(), "29999999"; got != want {
		t.Errorf("Amount = %q, want %q", got, want)
	}
}

// TestDecodeFlow_missingField covers the malformed-input path. A
// body missing `amount` must return ErrMalformedPayload, not panic
// on a nil-deref.
func TestDecodeFlow_missingField(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     "CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP",
		LedgerClosedAt: "2026-05-14T10:30:00Z",
		Topic:          []string{TopicPrefixStrategy, TopicSymbolDeposit},
		Value: mustB64(t, mapSCVal(t,
			mapEntry(t, "from", addrSCVal(makeAccountAddress(t, 0xAA))),
			// no amount
		)),
	}
	_, err := decodeFlow(ev, EventDeposit)
	if !errors.Is(err, ErrMalformedPayload) {
		t.Errorf("err = %v, want ErrMalformedPayload", err)
	}
}

// TestDecodeFlow_badKind defends the defensive default branch — a
// kind classify() would never return must still error cleanly.
func TestDecodeFlow_badKind(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		LedgerClosedAt: "2026-05-14T10:30:00Z",
		Topic:          []string{TopicPrefixStrategy, TopicSymbolDeposit},
		Value:          mustB64(t, mapSCVal(t)),
	}
	_, err := decodeFlow(ev, "rebalance")
	if !errors.Is(err, ErrUnknownEvent) {
		t.Errorf("err = %v, want ErrUnknownEvent", err)
	}
}

// ─── SCVal builders for tests ─────────────────────────────────
// Mirrored from internal/sources/soroswap_router/decode_test.go —
// keeping per-package builders rather than DRYing into a shared
// test helper because the test-time graph stays small + the
// builders are pure Go (no production dependencies to manage).

func i128SCVal(n *big.Int) sdkxdr.ScVal {
	abs := new(big.Int).Set(n)
	if abs.Sign() < 0 {
		abs.Neg(abs)
	}
	bytes := abs.Bytes()
	for len(bytes) < 16 {
		bytes = append([]byte{0}, bytes...)
	}
	hi := int64(0)
	for i := 0; i < 8; i++ {
		hi = (hi << 8) | int64(bytes[i])
	}
	lo := uint64(0)
	for i := 8; i < 16; i++ {
		lo = (lo << 8) | uint64(bytes[i])
	}
	if n.Sign() < 0 {
		hi = ^hi
		lo = ^lo + 1
		if lo == 0 {
			hi++
		}
	}
	return sdkxdr.ScVal{
		Type: sdkxdr.ScValTypeScvI128,
		I128: &sdkxdr.Int128Parts{
			Hi: sdkxdr.Int64(hi),
			Lo: sdkxdr.Uint64(lo),
		},
	}
}

func addrSCVal(addr sdkxdr.ScAddress) sdkxdr.ScVal {
	return sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvAddress, Address: &addr}
}

func makeAccountAddress(t *testing.T, fillByte byte) sdkxdr.ScAddress {
	t.Helper()
	var ed25519 sdkxdr.Uint256
	for i := range ed25519 {
		ed25519[i] = fillByte
	}
	acct := sdkxdr.AccountId{
		Type:    sdkxdr.PublicKeyTypePublicKeyTypeEd25519,
		Ed25519: &ed25519,
	}
	return sdkxdr.ScAddress{Type: sdkxdr.ScAddressTypeScAddressTypeAccount, AccountId: &acct}
}

func makeContractAddress(t *testing.T, fillByte byte) sdkxdr.ScAddress {
	t.Helper()
	var cid sdkxdr.ContractId
	for i := range cid {
		cid[i] = fillByte
	}
	return sdkxdr.ScAddress{Type: sdkxdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
}

func mapEntry(t *testing.T, key string, val sdkxdr.ScVal) sdkxdr.ScMapEntry {
	t.Helper()
	sym := sdkxdr.ScSymbol(key)
	keySv := sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvSymbol, Sym: &sym}
	return sdkxdr.ScMapEntry{Key: keySv, Val: val}
}

func mapSCVal(t *testing.T, entries ...sdkxdr.ScMapEntry) sdkxdr.ScVal {
	t.Helper()
	m := sdkxdr.ScMap(entries)
	pm := &m
	return sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvMap, Map: &pm}
}

func mustB64(t *testing.T, sv sdkxdr.ScVal) string {
	t.Helper()
	bs, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal scval: %v", err)
	}
	return base64.StdEncoding.EncodeToString(bs)
}

func mustB64String(t *testing.T, s string) string {
	t.Helper()
	xs := sdkxdr.ScString(s)
	sv := sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvString, Str: &xs}
	return mustB64(t, sv)
}

func mustB64Symbol(t *testing.T, s string) string {
	t.Helper()
	sym := sdkxdr.ScSymbol(s)
	sv := sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvSymbol, Sym: &sym}
	return mustB64(t, sv)
}
