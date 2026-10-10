package band

import (
	"encoding/base64"
	"errors"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// ─── fixture helpers ─────────────────────────────────────────────

const (
	// adapterC is the mainnet StandardReference address per
	// docs/protocols/band.md. The decoder matches against it
	// but doesn't otherwise touch network — any valid C-strkey works.
	adapterC = "CCQXWMZVM3KRTXTUPTN53YHL272QGKF32L7XEDNZ2S6OSUFK3NFBGG5M"
)

// relayerG is a valid G-strkey generated from a fixed seed so we
// don't hard-code a checksum that could drift.
var relayerG = func() string {
	seed := [32]byte{
		0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
		0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x00,
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10,
	}
	s, err := strkey.Encode(strkey.VersionByteAccountID, seed[:])
	if err != nil {
		panic("strkey encode seed: " + err.Error())
	}
	return s
}()

// encodeAddressArg marshals a G-strkey as base64 SCVal::Address.
func encodeAddressArg(t *testing.T, g string) string {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, g)
	if err != nil {
		t.Fatalf("decode strkey: %v", err)
	}
	var pub xdr.Uint256
	copy(pub[:], raw)
	aid := xdr.AccountId{
		Type:    xdr.PublicKeyTypePublicKeyTypeEd25519,
		Ed25519: &pub,
	}
	addr := xdr.ScAddress{
		Type:      xdr.ScAddressTypeScAddressTypeAccount,
		AccountId: &aid,
	}
	sv := xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// encodeSymbolRatesArg marshals a Vec<(Symbol, u64)> as the base64
// SCVal wire form the relayer sends. Each (symbol, rate) entry is
// an ScvVec of length 2 — soroban-sdk's tuple serialization.
func encodeSymbolRatesArg(t *testing.T, pairs []struct {
	Symbol string
	Rate   uint64
},
) string {
	t.Helper()
	items := make([]xdr.ScVal, len(pairs))
	for i, p := range pairs {
		s := xdr.ScSymbol(p.Symbol)
		symSv := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &s}
		u := xdr.Uint64(p.Rate)
		rateSv := xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &u}
		tuple := xdr.ScVec{symSv, rateSv}
		pt := &tuple
		items[i] = xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pt}
	}
	outer := xdr.ScVec(items)
	po := &outer
	sv := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &po}
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal symbol_rates: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// encodeU64Arg marshals a u64 as base64 SCVal::U64.
func encodeU64Arg(t *testing.T, n uint64) string {
	t.Helper()
	u := xdr.Uint64(n)
	sv := xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &u}
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal u64: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// ─── tests ───────────────────────────────────────────────────────

func TestDecodeRelay_HappyPath(t *testing.T) {
	const resolveSec = uint64(1_745_000_000)
	const btcRateE9 = uint64(500_000_000_000_000) // $500k at E9
	const ethRateE9 = uint64(35_000_000_000_000)  // $35k at E9

	args := []string{
		encodeAddressArg(t, relayerG),
		encodeSymbolRatesArg(t, []struct {
			Symbol string
			Rate   uint64
		}{
			{"BTC", btcRateE9},
			{"ETH", ethRateE9},
		}),
		encodeU64Arg(t, resolveSec),
		encodeU64Arg(t, 1), // request_id
	}

	updates, err := decodeRelayArgs(FnRelay, args, adapterC,
		52_000_000, "abcd", 0, "", "", time.Now())
	if err != nil {
		t.Fatalf("decodeRelayArgs: %v", err)
	}
	if len(updates) != 2 {
		t.Fatalf("expected 2 updates, got %d", len(updates))
	}
	btc, _ := canonical.NewCryptoAsset("BTC")
	eth, _ := canonical.NewCryptoAsset("ETH")
	if !updates[0].Asset.Equal(btc) {
		t.Errorf("updates[0].Asset = %+v want BTC", updates[0].Asset)
	}
	if !updates[1].Asset.Equal(eth) {
		t.Errorf("updates[1].Asset = %+v want ETH", updates[1].Asset)
	}
	if updates[0].Price.BigInt().Cmp(new(big.Int).SetUint64(btcRateE9)) != 0 {
		t.Errorf("BTC price = %s want %d", updates[0].Price, btcRateE9)
	}
	if updates[0].Decimals != 9 {
		t.Errorf("decimals = %d want 9", updates[0].Decimals)
	}
	// Timestamp sourced from resolve_time (seconds), not close.
	if updates[0].Timestamp.Unix() != int64(resolveSec) {
		t.Errorf("timestamp %v != resolveSec %d", updates[0].Timestamp, resolveSec)
	}
	// Observer = `from` arg on relay().
	if updates[0].Observer != relayerG {
		t.Errorf("observer = %q want %q", updates[0].Observer, relayerG)
	}
	// OpIndex fan-out: slot 0, slot 1 under same base.
	if updates[0].OpIndex != 0 || updates[1].OpIndex != 1 {
		t.Errorf("OpIndex fan-out wrong: [%d, %d]", updates[0].OpIndex, updates[1].OpIndex)
	}
	// Quote = USD (Band single-symbol convention).
	usd, _ := canonical.NewFiatAsset("USD")
	if !updates[0].Quote.Equal(usd) {
		t.Errorf("quote = %+v want USD", updates[0].Quote)
	}
}

// A future relay that appends an arg (e.g. `signer: Address` at index 4)
// must still decode the first four positionally.
func TestDecodeRelay_TrailingArgIgnored(t *testing.T) {
	const resolveSec = uint64(1_745_000_000)
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeSymbolRatesArg(t, []struct {
			Symbol string
			Rate   uint64
		}{{"BTC", 500_000_000_000_000}}),
		encodeU64Arg(t, resolveSec),
		encodeU64Arg(t, 1),
		encodeAddressArg(t, relayerG),
	}
	updates, err := decodeRelayArgs(FnRelay, args, adapterC,
		52_000_000, "abcd", 0, "", "", time.Now())
	if err != nil {
		t.Fatalf("decodeRelayArgs: %v", err)
	}
	if len(updates) != 1 || updates[0].Observer != relayerG || updates[0].Timestamp.Unix() != int64(resolveSec) {
		t.Fatalf("got %+v", updates)
	}
}

// TestDecodeRelay_ContractRelayerValidates pins that `relay` declares
// `from` as a Soroban Address, so the relayer may be a contract. Every
// decoded update must still pass OracleUpdate.Validate — the check the
// store runs before INSERT — or the whole batch's prices are lost.
func TestDecodeRelay_ContractRelayerValidates(t *testing.T) {
	raw, err := strkey.Decode(strkey.VersionByteContract, adapterC)
	if err != nil {
		t.Fatalf("decode adapter strkey: %v", err)
	}
	var cid xdr.ContractId
	copy(cid[:], raw)
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
	fromBytes, err := xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal from: %v", err)
	}
	args := []string{
		base64.StdEncoding.EncodeToString(fromBytes),
		encodeSymbolRatesArg(t, []struct {
			Symbol string
			Rate   uint64
		}{{"BTC", 78_313_029_743}, {"ETH", 3_500_000_000_000}}),
		encodeU64Arg(t, 1_745_000_000),
		encodeU64Arg(t, 1),
	}
	const txHash = "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"
	updates, err := decodeRelayArgs(FnRelay, args, adapterC,
		52_000_000, txHash, 0, "", "", time.Now())
	if err != nil {
		t.Fatalf("decodeRelayArgs: %v", err)
	}
	if len(updates) != 2 {
		t.Fatalf("expected 2 updates, got %d", len(updates))
	}
	for i, u := range updates {
		if u.Observer != adapterC {
			t.Errorf("updates[%d].Observer = %q want contract relayer %q", i, u.Observer, adapterC)
		}
		if err := u.Validate(); err != nil {
			t.Errorf("updates[%d].Validate() = %v; a contract relayer must not reject the price", i, err)
		}
	}
}

// TestDecodeForceRelay_FarFutureResolveTimeClampsToClose confirms that a
// sentinel / garbage far-future resolve_time (the same overflow
// class as the soroswap-router deadline_ts) falls back to the ledger
// close time instead of stamping a year-99-billion timestamp that
// would overflow the timestamptz INSERT. force_relay (unconditional
// admin path, no resolve_time acceptance window) still clamps; relay()
// drops instead — see TestDecodeRelay_FutureResolveTimeBeyondContractWindowIsDropped.
func TestDecodeForceRelay_FarFutureResolveTimeClampsToClose(t *testing.T) {
	const farFuture = uint64(3_000_000_000_000_000_000) // ~year 95 billion
	closedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	args := []string{
		encodeSymbolRatesArg(t, []struct {
			Symbol string
			Rate   uint64
		}{
			{"BTC", 500_000_000_000_000},
		}),
		encodeU64Arg(t, farFuture),
		encodeU64Arg(t, 1),
	}
	updates, err := decodeRelayArgs(FnForceRelay, args, adapterC,
		52_000_000, "abcd", 0, "", "", closedAt)
	if err != nil {
		t.Fatalf("decodeRelayArgs: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	if !updates[0].Timestamp.Equal(closedAt) {
		t.Errorf("Timestamp = %v, want ledger close %v (far-future resolve_time should clamp)",
			updates[0].Timestamp, closedAt)
	}
}

// TestDecodeForceRelay_OverflowResolveTimeClampsToClose covers the u64
// values ABOVE math.MaxInt64 (~9.2e18) that the FarFuture test (3e18)
// does not: these wrap NEGATIVE in the int64() cast and, unguarded,
// stamped a far-PAST time (e.g. year -267,666,662,216 for 1e19, or
// 1969 for MaxUint64) that slipped past the old `ts.After(close+24h)`
// guard in both directions and overflowed the timestamptz INSERT.
func TestDecodeForceRelay_OverflowResolveTimeClampsToClose(t *testing.T) {
	closedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for name, resolve := range map[string]uint64{
		"justOverMaxInt64": uint64(math.MaxInt64) + 1,
		"1e19wrapsFarPast": 10_000_000_000_000_000_000,
		"maxUint64to1969":  math.MaxUint64,
	} {
		t.Run(name, func(t *testing.T) {
			args := []string{
				encodeSymbolRatesArg(t, []struct {
					Symbol string
					Rate   uint64
				}{
					{"BTC", 500_000_000_000_000},
				}),
				encodeU64Arg(t, resolve),
				encodeU64Arg(t, 1),
			}
			updates, err := decodeRelayArgs(FnForceRelay, args, adapterC,
				52_000_000, "abcd", 0, "", "", closedAt)
			if err != nil {
				t.Fatalf("decodeRelayArgs: %v", err)
			}
			if len(updates) != 1 {
				t.Fatalf("expected 1 update, got %d", len(updates))
			}
			if !updates[0].Timestamp.Equal(closedAt) {
				t.Errorf("Timestamp = %v, want ledger close %v (>MaxInt64 resolve_time must clamp, not wrap)",
					updates[0].Timestamp, closedAt)
			}
		})
	}
}

func TestDecodeForceRelay_HappyPath(t *testing.T) {
	// force_relay has 3 args (no `from`). Observer should fall back
	// to opSource (here a G-strkey we pass directly).
	const resolveSec = uint64(1_745_000_100)
	args := []string{
		encodeSymbolRatesArg(t, []struct {
			Symbol string
			Rate   uint64
		}{
			{"XLM", 120_000_000},
		}),
		encodeU64Arg(t, resolveSec),
		encodeU64Arg(t, 42),
	}
	updates, err := decodeRelayArgs(FnForceRelay, args, adapterC,
		52_000_001, "ef01", 0, relayerG /*opSource*/, "" /*txSource*/, time.Now())
	if err != nil {
		t.Fatalf("decodeRelayArgs: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	if updates[0].Observer != relayerG {
		t.Errorf("force_relay observer = %q want %q (opSource fallback)",
			updates[0].Observer, relayerG)
	}
}

func TestDecodeRelay_USDSymbolSkipped(t *testing.T) {
	// USD is special-cased in Band's storage (always 1@E9, relayer
	// writes rejected). Mixed-payload: USD skipped, BTC lands.
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeSymbolRatesArg(t, []struct {
			Symbol string
			Rate   uint64
		}{
			{"USD", 1_000_000_000}, // should be skipped
			{"BTC", 500_000_000_000_000},
		}),
		encodeU64Arg(t, 1_745_000_000),
		encodeU64Arg(t, 1),
	}
	updates, err := decodeRelayArgs(FnRelay, args, adapterC,
		52_000_000, "abcd", 0, "", "", time.Now())
	if err != nil {
		t.Fatalf("decodeRelayArgs: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("expected 1 update (BTC only), got %d", len(updates))
	}
	btc, _ := canonical.NewCryptoAsset("BTC")
	if !updates[0].Asset.Equal(btc) {
		t.Errorf("updates[0].Asset = %+v want BTC", updates[0].Asset)
	}
	// OpIndex preserves slot 1 (USD was at slot 0).
	if updates[0].OpIndex != 1 {
		t.Errorf("OpIndex = %d want 1 (USD skipped at slot 0)", updates[0].OpIndex)
	}
}

// Oracle capture-totality: an unmapped symbol is RECORDED as a
// raw:<symbol> row at its own vector slot, not skipped. NOTACOIN yields
// a raw row and BTC keeps OpIndex 1: the raw row fills slot 0 (no existing
// row moves).
func TestDecodeRelay_UnknownSymbolRecordedAsRaw(t *testing.T) {
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeSymbolRatesArg(t, []struct {
			Symbol string
			Rate   uint64
		}{
			{"NOTACOIN", 999},
			{"BTC", 500_000_000_000_000},
		}),
		encodeU64Arg(t, 1_745_000_000),
		encodeU64Arg(t, 1),
	}
	updates, err := decodeRelayArgs(FnRelay, args, adapterC,
		52_000_000, "abcd", 0, "", "", time.Now())
	if err != nil {
		t.Fatalf("decodeRelayArgs: %v", err)
	}
	if len(updates) != 2 {
		t.Fatalf("expected 2 updates (raw:NOTACOIN + BTC), got %d", len(updates))
	}
	raw, _ := canonical.NewOracleRawAsset("NOTACOIN")
	if !updates[0].Asset.Equal(raw) || updates[0].Asset.IsMapped() {
		t.Errorf("updates[0].Asset = %s want %s (unmapped)", updates[0].Asset, raw)
	}
	if updates[0].OpIndex != 0 {
		t.Errorf("updates[0].OpIndex = %d want 0 (raw row holds slot 0)", updates[0].OpIndex)
	}
	if updates[0].Price.BigInt().Uint64() != 999 {
		t.Errorf("updates[0].Price = %s want 999 (recorded verbatim)", updates[0].Price)
	}
	usd, _ := canonical.NewFiatAsset("USD")
	if !updates[0].Quote.Equal(usd) {
		t.Errorf("updates[0].Quote = %s want fiat:USD", updates[0].Quote)
	}
	btc, _ := canonical.NewCryptoAsset("BTC")
	if !updates[1].Asset.Equal(btc) {
		t.Errorf("updates[1].Asset = %s want BTC", updates[1].Asset)
	}
	if updates[1].OpIndex != 1 {
		t.Errorf("updates[1].OpIndex = %d want 1 (BTC keeps its pre-totality slot)", updates[1].OpIndex)
	}
}

// All-unknown vector: rows, not ErrEmptyRates. USD and rate-0 slots
// are STILL skipped (contract rejects the USD write; a zero rate is
// not a price) — totality is about symbol mapping only.
func TestDecodeRelay_AllUnknownRecordedAsRaw_USDAndZeroStillSkipped(t *testing.T) {
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeSymbolRatesArg(t, []struct {
			Symbol string
			Rate   uint64
		}{
			{"USD", 1_000_000_000}, // slot 0: contract special-case, skipped
			{"NOTACOIN", 999},      // slot 1: raw
			{"ALSONOTACOIN", 0},    // slot 2: rate 0, skipped
			{"DOGEMOON", 5},        // slot 3: raw
		}),
		encodeU64Arg(t, 1_745_000_000),
		encodeU64Arg(t, 1),
	}
	updates, err := decodeRelayArgs(FnRelay, args, adapterC,
		52_000_000, "abcd", 0, "", "", time.Now())
	if err != nil {
		t.Fatalf("all-unknown relay must decode to raw rows, got error: %v", err)
	}
	if len(updates) != 2 {
		t.Fatalf("expected 2 raw updates, got %d", len(updates))
	}
	want := []struct {
		code    string
		opIndex uint32
	}{{"NOTACOIN", 1}, {"DOGEMOON", 3}}
	for i, w := range want {
		raw, _ := canonical.NewOracleRawAsset(w.code)
		if !updates[i].Asset.Equal(raw) {
			t.Errorf("updates[%d].Asset = %s want %s", i, updates[i].Asset, raw)
		}
		if updates[i].OpIndex != w.opIndex {
			t.Errorf("updates[%d].OpIndex = %d want %d", i, updates[i].OpIndex, w.opIndex)
		}
	}
}

func TestDecodeRelay_EmptyRates_IsNoOp(t *testing.T) {
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeSymbolRatesArg(t, nil),
		encodeU64Arg(t, 1_745_000_000),
		encodeU64Arg(t, 1),
	}
	updates, err := decodeRelayArgs(FnRelay, args, adapterC,
		52_000_000, "abcd", 0, "", "", time.Now())
	if err != nil || updates != nil {
		t.Errorf("empty symbol_rates: got (%v, %v), want (nil, nil) no-op", updates, err)
	}
}

// The empty-vector no-op must not mask a malformed call.
func TestDecodeRelay_EmptyRates_MalformedResolveTimeRejects(t *testing.T) {
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeSymbolRatesArg(t, nil),
		encodeAddressArg(t, relayerG),
		encodeU64Arg(t, 1),
	}
	_, err := decodeRelayArgs(FnRelay, args, adapterC,
		52_000_000, "abcd", 0, "", "", time.Now())
	if !errors.Is(err, ErrMalformedArgs) {
		t.Errorf("expected ErrMalformedArgs, got %v", err)
	}
}

func TestDecodeRelay_TooFewArgs_Malformed(t *testing.T) {
	// relay requires 4 args; supply only 2.
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeSymbolRatesArg(t, []struct {
			Symbol string
			Rate   uint64
		}{{"BTC", 1}}),
	}
	_, err := decodeRelayArgs(FnRelay, args, adapterC,
		52_000_000, "abcd", 0, "", "", time.Now())
	if !errors.Is(err, ErrMalformedArgs) {
		t.Errorf("expected ErrMalformedArgs, got %v", err)
	}
}

func TestDecoder_MatchesOnlyRelayFunctions(t *testing.T) {
	d := NewDecoder(adapterC)
	if !d.Matches(adapterC, "relay") {
		t.Error("expected match on relay")
	}
	if !d.Matches(adapterC, "force_relay") {
		t.Error("expected match on force_relay")
	}
	if d.Matches(adapterC, "get_ref_data") {
		t.Error("get_ref_data should not match (read-only)")
	}
	if d.Matches("CWRONGADDRESS3333333333333333333333333333333333333333333", "relay") {
		t.Error("wrong contract should not match")
	}
}

// A resolve_time beyond Band's own +1h acceptance window must be
// dropped, not clamped to the ledger close: relay() silently NO-OPs
// outside that window (the tx still succeeds), so writing the update
// anyway — even stamped at closedAt — let a rate the chain never
// applied win our `ORDER BY ts DESC` latest-read (the
// clamp alone still leaves that window open for up to one relay interval).
func TestDecodeRelay_FutureResolveTimeBeyondContractWindowIsDropped(t *testing.T) {
	closedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	for name, tc := range map[string]struct {
		offset   time.Duration
		wantDrop bool
	}{
		"within window (30m)":      {30 * time.Minute, false},
		"just inside window (59m)": {59 * time.Minute, false},
		"last accepted second":     {time.Hour - time.Second, false},
		// relay() applies only while resolve_time < close + 3600, so
		// the edge itself is a rejected (no-op) relay.
		"exactly at window edge":    {time.Hour, true},
		"beyond window (2h)":        {2 * time.Hour, true},
		"far beyond, inside helper": {23 * time.Hour, true},
	} {
		t.Run(name, func(t *testing.T) {
			resolve := uint64(closedAt.Add(tc.offset).Unix())
			args := []string{
				encodeAddressArg(t, relayerG),
				encodeSymbolRatesArg(t, []struct {
					Symbol string
					Rate   uint64
				}{
					{"BTC", 500_000_000_000_000},
				}),
				encodeU64Arg(t, resolve),
				encodeU64Arg(t, 1),
			}
			updates, err := decodeRelayArgs(FnRelay, args, adapterC,
				52_000_000, "abcd", 0, "", "", closedAt)
			if tc.wantDrop {
				if !errors.Is(err, ErrEmptyRates) {
					t.Fatalf("decodeRelayArgs error = %v, want ErrEmptyRates — a relay the "+
						"contract would reject must not be written at all", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeRelayArgs: %v", err)
			}
			if len(updates) != 1 {
				t.Fatalf("expected 1 update, got %d", len(updates))
			}
			got := updates[0].Timestamp.UTC()
			if !got.Equal(time.Unix(int64(resolve), 0).UTC()) {
				t.Errorf("ts = %s, want the declared resolve_time (inside the contract's window)", got)
			}
		})
	}
}

// decodeRelayArgs has many reject paths — existing tests cover
// happy/USD-skip/unknown-symbol/empty-rates/too-few-args. This file
// pins the remaining structural rejects so a malformed Band
// invocation can't slip through to the storage layer:
//   - force_relay with too few args
//   - unknown function name (returns ErrNotBandCall — guards the
//     ContractCallDecoder routing seam)
//   - resolve_time pre-epoch fallback to ledger close time

func TestDecodeForceRelay_TooFewArgs_Malformed(t *testing.T) {
	// force_relay needs 3 args; pass 1.
	args := []string{
		encodeSymbolRatesArg(t, []struct {
			Symbol string
			Rate   uint64
		}{{"BTC", 1}}),
	}
	_, err := decodeRelayArgs(FnForceRelay, args, adapterC,
		52_000_000, "abcd", 0, "", "", time.Now())
	if !errors.Is(err, ErrMalformedArgs) {
		t.Errorf("expected ErrMalformedArgs, got %v", err)
	}
}

func TestDecodeRelayArgs_UnknownFunction_NotBandCall(t *testing.T) {
	// A future Band ABI extension — or a misrouted call — must NOT
	// be decoded as relay/force_relay. ErrNotBandCall is the marker
	// the dispatcher uses to keep dispatching downstream.
	_, err := decodeRelayArgs("get_ref_data", nil, adapterC,
		52_000_000, "abcd", 0, "", "", time.Now())
	if !errors.Is(err, ErrNotBandCall) {
		t.Errorf("expected ErrNotBandCall for unknown function, got %v", err)
	}
}

func TestDecodeRelay_PreEpochResolveTimeIsDropped(t *testing.T) {
	// resolve_time=0 is well below Band's own resolve_time < close+OFFSET
	// acceptance window's floor of sanity (pre-2001) — the contract's
	// relay() would silently no-op the call on-chain even though the tx
	// succeeds. Clamping it to closedAt and still writing it (the old
	// behaviour) let a rate the chain never applied win the latest-read
	// ORDER BY ts DESC. relay() must drop the update instead.
	closedAt := time.Unix(1_745_000_500, 0).UTC()
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeSymbolRatesArg(t, []struct {
			Symbol string
			Rate   uint64
		}{{"BTC", 50_000_000_000_000}}),
		encodeU64Arg(t, 0), // pre-epoch — the contract would no-op
		encodeU64Arg(t, 1),
	}
	_, err := decodeRelayArgs(FnRelay, args, adapterC,
		52_000_000, "abcd", 0, "", "", closedAt)
	if !errors.Is(err, ErrEmptyRates) {
		t.Fatalf("decodeRelayArgs error = %v, want ErrEmptyRates (relay() would no-op)", err)
	}
}

func TestDecodeForceRelay_PreEpochResolveTimeFallsBackToClosedAt(t *testing.T) {
	// force_relay is the unconditional admin path — it has no
	// resolve_time acceptance window to mirror, so a garbage
	// resolve_time still clamps to ledger close rather than being
	// dropped.
	closedAt := time.Unix(1_745_000_500, 0).UTC()
	args := []string{
		encodeSymbolRatesArg(t, []struct {
			Symbol string
			Rate   uint64
		}{{"BTC", 50_000_000_000_000}}),
		encodeU64Arg(t, 0), // pre-epoch — triggers fallback, not a drop
		encodeU64Arg(t, 1),
	}
	updates, err := decodeRelayArgs(FnForceRelay, args, adapterC,
		52_000_000, "abcd", 0, "", "", closedAt)
	if err != nil {
		t.Fatalf("decodeRelayArgs: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("got %d updates, want 1", len(updates))
	}
	if !updates[0].Timestamp.Equal(closedAt) {
		t.Errorf("Timestamp = %v, want closedAt %v (force_relay pre-epoch resolve_time should fall back)",
			updates[0].Timestamp, closedAt)
	}
}

func TestDecodeRelay_FutureResolveTimeBeyondOffsetIsDropped(t *testing.T) {
	// resolve_time >= close+OFFSET (1h) is outside relay()'s own
	// acceptance window even though it's well inside
	// canonical.SafeUnixSeconds's looser 24h ceiling — must still drop.
	closedAt := time.Unix(1_745_000_500, 0).UTC()
	future := uint64(closedAt.Add(2 * time.Hour).Unix())
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeSymbolRatesArg(t, []struct {
			Symbol string
			Rate   uint64
		}{{"BTC", 50_000_000_000_000}}),
		encodeU64Arg(t, future),
		encodeU64Arg(t, 1),
	}
	_, err := decodeRelayArgs(FnRelay, args, adapterC,
		52_000_000, "abcd", 0, "", "", closedAt)
	if !errors.Is(err, ErrEmptyRates) {
		t.Fatalf("decodeRelayArgs error = %v, want ErrEmptyRates (future resolve_time beyond Band's OFFSET)", err)
	}
}

func TestDecodeRelay_MalformedSymbolRatesArg_Rejected(t *testing.T) {
	// Pass a non-Vec for symbol_rates — the parse step itself
	// succeeds but AsVec must reject. Surface as ErrMalformedArgs
	// so dispatcher's drop-stat counter increments rather than
	// crashing the ledger pass.
	notAVec := xdr.ScSymbol("not-a-vec")
	bogusSv := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &notAVec}
	bogusBytes, err := bogusSv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	bogus := base64.StdEncoding.EncodeToString(bogusBytes)

	args := []string{
		encodeAddressArg(t, relayerG),
		bogus, // symbol_rates not a Vec
		encodeU64Arg(t, 1_745_000_000),
		encodeU64Arg(t, 1),
	}
	_, err = decodeRelayArgs(FnRelay, args, adapterC,
		52_000_000, "abcd", 0, "", "", time.Now())
	if !errors.Is(err, ErrMalformedArgs) {
		t.Errorf("expected ErrMalformedArgs for non-Vec symbol_rates, got %v", err)
	}
}

// keep the canonical import live — used implicitly by the
// happy-path test in this same package via shared types.
var _ = canonical.NewFiatAsset

// oracle_updates carries ts in its primary key, so a decoder change that
// shifts the ts of an already-stored event makes a re-derive INSERT a second
// row instead of conflicting. These goldens pin the exact ts per input for
// both entry points; a failure here means a ts-derivation change needs its
// own cleanup run (see "Re-deriving a timestamp" in
// docs/architecture/ingest-pipeline.md).
func TestDecodeRelayArgs_TimestampGolden(t *testing.T) {
	closedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	sec := func(d time.Duration) uint64 { return uint64(closedAt.Add(d).Unix()) }

	type tc struct {
		resolve  uint64
		want     time.Time
		wantDrop bool
	}
	cases := map[string]map[string]tc{
		FnRelay: {
			"past resolve_time kept": {1_745_000_000, time.Unix(1_745_000_000, 0), false},
			"equal to close kept":    {sec(0), closedAt, false},
			"close+3599s kept":       {sec(time.Hour - time.Second), closedAt.Add(time.Hour - time.Second), false},
			"close+3600s dropped":    {sec(time.Hour), time.Time{}, true},
			"close+24h dropped":      {sec(24 * time.Hour), time.Time{}, true},
			"zero dropped":           {0, time.Time{}, true},
			"pre-2001 dropped":       {999_999_999, time.Time{}, true},
			"at 2001 floor kept":     {1_000_000_000, time.Unix(1_000_000_000, 0), false},
			"u64 max dropped":        {^uint64(0), time.Time{}, true},
		},
		FnForceRelay: {
			"past resolve_time kept":      {1_745_000_000, time.Unix(1_745_000_000, 0), false},
			"close+3599s kept":            {sec(time.Hour - time.Second), closedAt.Add(time.Hour - time.Second), false},
			"close+3600s clamps to close": {sec(time.Hour), closedAt, false},
			"close+24h clamps to close":   {sec(24 * time.Hour), closedAt, false},
			"zero clamps to close":        {0, closedAt, false},
			"pre-2001 clamps to close":    {999_999_999, closedAt, false},
			"at 2001 floor kept":          {1_000_000_000, time.Unix(1_000_000_000, 0), false},
			"u64 max clamps to close":     {^uint64(0), closedAt, false},
		},
	}

	for fn, group := range cases {
		for name, c := range group {
			t.Run(fn+"/"+name, func(t *testing.T) {
				rates := encodeSymbolRatesArg(t, []struct {
					Symbol string
					Rate   uint64
				}{{"BTC", 500_000_000_000_000}})
				var args []string
				if fn == FnRelay {
					args = []string{encodeAddressArg(t, relayerG), rates, encodeU64Arg(t, c.resolve), encodeU64Arg(t, 1)}
				} else {
					args = []string{rates, encodeU64Arg(t, c.resolve), encodeU64Arg(t, 1)}
				}
				got, err := decodeRelayArgs(fn, args, adapterC, 52_000_000, "abcd", 0, "", "", closedAt)
				if c.wantDrop {
					if err == nil && len(got) != 0 {
						t.Fatalf("expected the relay to be dropped, got %d rows (ts %s)", len(got), got[0].Timestamp)
					}
					return
				}
				if err != nil {
					t.Fatalf("decodeRelayArgs: %v", err)
				}
				if len(got) != 1 {
					t.Fatalf("got %d updates, want 1", len(got))
				}
				if !got[0].Timestamp.Equal(c.want) {
					t.Errorf("ts = %s, want %s", got[0].Timestamp, c.want)
				}
			})
		}
	}
}

// Exactly opIndexFanoutStride symbol_rates fit one call's OpIndex block;
// one more would spill into the next operation's block and is refused.
func TestDecodeRelay_FanoutStrideEdges(t *testing.T) {
	closedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	decode := func(n int) (int, error) {
		pairs := make([]struct {
			Symbol string
			Rate   uint64
		}, n)
		for i := range pairs {
			pairs[i].Symbol, pairs[i].Rate = "BTC", uint64(i+1)
		}
		args := []string{
			encodeAddressArg(t, relayerG),
			encodeSymbolRatesArg(t, pairs),
			encodeU64Arg(t, uint64(closedAt.Unix())),
			encodeU64Arg(t, 1),
		}
		updates, err := decodeRelayArgs(FnRelay, args, adapterC, 52_000_000, "abcd", 1, "", "", closedAt)
		if err == nil {
			if last := updates[len(updates)-1].OpIndex; last != 2*opIndexFanoutStride-1 {
				t.Errorf("last OpIndex = %d, want %d", last, 2*opIndexFanoutStride-1)
			}
		}
		return len(updates), err
	}
	if n, err := decode(opIndexFanoutStride); err != nil || n != opIndexFanoutStride {
		t.Fatalf("%d pairs: got (%d, %v), want all decoded", opIndexFanoutStride, n, err)
	}
	if _, err := decode(opIndexFanoutStride + 1); err == nil {
		t.Fatalf("%d pairs decoded; must be refused", opIndexFanoutStride+1)
	}
}

// ─── consumer.go ──────────────────────────────────────────────────

func TestUpdateEvent_implementsConsumerEvent(t *testing.T) {
	ue := UpdateEvent{}
	if got := ue.EventKind(); got != "band.update" {
		t.Errorf("EventKind() = %q, want \"band.update\"", got)
	}
	if got := ue.Source(); got != SourceName {
		t.Errorf("Source() = %q, want %q", got, SourceName)
	}
	var _ consumer.Event = ue
}

// ─── dispatcher_adapter.go ────────────────────────────────────────

func TestDecoder_Name(t *testing.T) {
	if got := NewDecoder(adapterC).Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
}

func TestDecoder_Decode_RoutesToDecodeRelayArgs(t *testing.T) {
	// End-to-end through the adapter: build a relay() call's args,
	// hand them to Decoder.Decode via a ContractCallContext, and
	// verify the resulting UpdateEvent slice carries the expected
	// observations. Effectively the same shape as decode_test.go's
	// TestDecodeRelay_HappyPath but exercises the adapter's
	// out-array packing.
	const resolveSec = uint64(1_745_000_000)
	const btcRateE9 = uint64(500_000_000_000_000)

	args := []string{
		encodeAddressArg(t, relayerG),
		encodeSymbolRatesArg(t, []struct {
			Symbol string
			Rate   uint64
		}{
			{"BTC", btcRateE9},
		}),
		encodeU64Arg(t, resolveSec),
		encodeU64Arg(t, 42),
	}
	ctx := dispatcher.ContractCallContext{
		Ledger:       52_000_000,
		ClosedAt:     time.Now().UTC(),
		TxHash:       "abcd",
		ContractID:   adapterC,
		FunctionName: FnRelay,
		Args:         args,
	}
	d := NewDecoder(adapterC)
	out, err := d.Decode(ctx)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	ue, ok := out[0].(UpdateEvent)
	if !ok {
		t.Fatalf("expected UpdateEvent, got %T", out[0])
	}
	if ue.Update.Source != SourceName {
		t.Errorf("Update.Source = %q, want %q", ue.Update.Source, SourceName)
	}
}

func TestDecoder_Decode_MalformedArgsReturnsError(t *testing.T) {
	d := NewDecoder(adapterC)
	ctx := dispatcher.ContractCallContext{
		Ledger:       52_000_000,
		ClosedAt:     time.Now().UTC(),
		TxHash:       "abcd",
		ContractID:   adapterC,
		FunctionName: FnRelay,
		Args:         []string{"not-base64"}, // too few args + invalid encoding
	}
	if _, err := d.Decode(ctx); err == nil {
		t.Error("expected decode error on malformed args, got nil")
	}
}

func TestDecoder_Decode_EmptyRatesIsNoOp(t *testing.T) {
	// A decode error here would count the call undecodable and blind the
	// ledger's completeness verdict; an empty batch has nothing to project.
	d := NewDecoder(adapterC)
	ctx := dispatcher.ContractCallContext{
		Ledger:       52_000_000,
		ClosedAt:     time.Unix(1_745_000_000, 0).UTC(),
		TxHash:       "abcd",
		ContractID:   adapterC,
		FunctionName: FnRelay,
		Args: []string{
			encodeAddressArg(t, relayerG),
			encodeSymbolRatesArg(t, nil),
			encodeU64Arg(t, 1_745_000_000),
			encodeU64Arg(t, 1),
		},
	}
	out, err := d.Decode(ctx)
	if err != nil {
		t.Fatalf("Decode(empty symbol_rates) = %v, want a nil-error no-op", err)
	}
	if len(out) != 0 {
		t.Fatalf("got %d events, want 0", len(out))
	}
}
