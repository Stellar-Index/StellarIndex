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
)

// ─── fixture helpers ─────────────────────────────────────────────

const (
	// adapterC is the mainnet StandardReference address per
	// docs/discovery/oracles/band.md. The decoder matches against it
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

// TestDecodeRelay_FarFutureResolveTimeClampsToClose confirms that a
// sentinel / garbage far-future resolve_time (the same overflow
// class as the soroswap-router deadline_ts) falls back to the ledger
// close time instead of stamping a year-99-billion timestamp that
// would overflow the timestamptz INSERT.
func TestDecodeRelay_FarFutureResolveTimeClampsToClose(t *testing.T) {
	const farFuture = uint64(3_000_000_000_000_000_000) // ~year 95 billion
	closedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	args := []string{
		encodeAddressArg(t, relayerG),
		encodeSymbolRatesArg(t, []struct {
			Symbol string
			Rate   uint64
		}{
			{"BTC", 500_000_000_000_000},
		}),
		encodeU64Arg(t, farFuture),
		encodeU64Arg(t, 1),
	}
	updates, err := decodeRelayArgs(FnRelay, args, adapterC,
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

// TestDecodeRelay_OverflowResolveTimeClampsToClose covers the u64 values
// ABOVE math.MaxInt64 (~9.2e18) that the FarFuture test (3e18) does not:
// these wrap NEGATIVE in the int64() cast and, pre-fix, stamped a
// far-PAST time (e.g. year -267,666,662,216 for 1e19, or 1969 for
// MaxUint64) that slipped past the old `ts.After(close+24h)` guard in
// both directions and overflowed the timestamptz INSERT.
func TestDecodeRelay_OverflowResolveTimeClampsToClose(t *testing.T) {
	closedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for name, resolve := range map[string]uint64{
		"justOverMaxInt64": uint64(math.MaxInt64) + 1,
		"1e19wrapsFarPast": 10_000_000_000_000_000_000,
		"maxUint64to1969":  math.MaxUint64,
	} {
		t.Run(name, func(t *testing.T) {
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

// Oracle capture-totality (PR-2): an unmapped symbol is RECORDED as a
// raw:<symbol> row at its own vector slot, not skipped. Before this
// change NOTACOIN was dropped (1 update) and BTC kept OpIndex 1; BTC
// STILL has OpIndex 1 — the raw row fills slot 0 (DAT-03: no existing
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

func TestDecodeRelay_EmptyRates_Rejects(t *testing.T) {
	args := []string{
		encodeAddressArg(t, relayerG),
		encodeSymbolRatesArg(t, nil),
		encodeU64Arg(t, 1_745_000_000),
		encodeU64Arg(t, 1),
	}
	_, err := decodeRelayArgs(FnRelay, args, adapterC,
		52_000_000, "abcd", 0, "", "", time.Now())
	if !errors.Is(err, ErrEmptyRates) {
		t.Errorf("expected ErrEmptyRates, got %v", err)
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

// A resolve_time beyond Band's own +1h acceptance window must clamp to
// the ledger close. relay() silently NO-OPs outside that window (the tx
// still succeeds), so recording the declared future time made our
// `ORDER BY ts DESC` latest-read serve a price the chain refused, for as
// long as the future stamp stayed ahead — up to a day under the shared
// helper's generic +24h ceiling (cold audit 2026-08-03).
func TestDecodeRelay_FutureResolveTimeBeyondContractWindowClampsToClose(t *testing.T) {
	closedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	for name, tc := range map[string]struct {
		offset    time.Duration
		wantClose bool
	}{
		"within window (30m)":       {30 * time.Minute, false},
		"just inside window (59m)":  {59 * time.Minute, false},
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
			if err != nil {
				t.Fatalf("decodeRelayArgs: %v", err)
			}
			if len(updates) != 1 {
				t.Fatalf("expected 1 update, got %d", len(updates))
			}
			got := updates[0].Timestamp.UTC()
			if tc.wantClose {
				if !got.Equal(closedAt) {
					t.Errorf("ts = %s, want the ledger close %s — a relay the contract "+
						"would reject must not be stamped in the future", got, closedAt)
				}
				return
			}
			if !got.Equal(time.Unix(int64(resolve), 0).UTC()) {
				t.Errorf("ts = %s, want the declared resolve_time (inside the contract's window)", got)
			}
		})
	}
}
