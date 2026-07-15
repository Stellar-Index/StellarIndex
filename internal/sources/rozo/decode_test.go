package rozo

import (
	"encoding/base64"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// ─── SDK-encode helpers for building well-formed fixtures ────────
// Patterns mirror internal/sources/soroswap/decode_test.go — the
// canonical SDK-encode shape across the source fleet.

func symbol(s string) xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

func i128(n *big.Int) xdr.ScVal {
	hi, lo := splitBigInt128(n)
	p := xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
}

func scMap(entries ...xdr.ScMapEntry) xdr.ScVal {
	m := xdr.ScMap(entries)
	pm := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pm}
}

func scString(s string) xdr.ScVal {
	v := xdr.ScString(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &v}
}

func makeContractStrkey(t *testing.T, seedByte byte) string {
	t.Helper()
	var raw [32]byte
	raw[0] = seedByte
	s, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	return s
}

func makeAccountStrkey(t *testing.T, seedByte byte) string {
	t.Helper()
	var raw [32]byte
	raw[0] = seedByte
	s, err := strkey.Encode(strkey.VersionByteAccountID, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	return s
}

func contractAddrFromStrkey(t *testing.T, strk string) xdr.ScVal {
	t.Helper()
	var cid xdr.ContractId
	raw, err := strkey.Decode(strkey.VersionByteContract, strk)
	if err != nil {
		t.Fatalf("strkey.Decode(%q): %v", strk, err)
	}
	copy(cid[:], raw)
	scAddr := xdr.ScAddress{
		Type:       xdr.ScAddressTypeScAddressTypeContract,
		ContractId: &cid,
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &scAddr}
}

func accountAddrFromStrkey(t *testing.T, strk string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, strk)
	if err != nil {
		t.Fatalf("strkey.Decode(%q): %v", strk, err)
	}
	var ed xdr.Uint256
	copy(ed[:], raw)
	scAccount := xdr.AccountId{
		Type:    xdr.PublicKeyTypePublicKeyTypeEd25519,
		Ed25519: &ed,
	}
	scAddr := xdr.ScAddress{
		Type:      xdr.ScAddressTypeScAddressTypeAccount,
		AccountId: &scAccount,
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &scAddr}
}

func b64(t *testing.T, sv xdr.ScVal) string {
	t.Helper()
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func splitBigInt128(n *big.Int) (hi int64, lo uint64) {
	twoTo64 := new(big.Int).Lsh(big.NewInt(1), 64)
	mask64 := new(big.Int).Sub(twoTo64, big.NewInt(1))
	if n.Sign() >= 0 {
		loBig := new(big.Int).And(n, mask64)
		hiBig := new(big.Int).Rsh(n, 64)
		return hiBig.Int64(), loBig.Uint64()
	}
	twoTo128 := new(big.Int).Lsh(big.NewInt(1), 128)
	u := new(big.Int).Add(twoTo128, n)
	loBig := new(big.Int).And(u, mask64)
	hiBig := new(big.Int).Rsh(u, 64)
	return int64(hiBig.Uint64()), loBig.Uint64()
}

// ─── Classify ────────────────────────────────────────────────────

func TestClassify_Payment(t *testing.T) {
	t.Parallel()
	from := makeAccountStrkey(t, 0x01)
	e := &events.Event{
		Topic: []string{TopicSymbolPayment, b64(t, accountAddrFromStrkey(t, from))},
	}
	if got := Classify(e); got != EventPayment {
		t.Errorf("Classify = %q, want %q", got, EventPayment)
	}
}

func TestClassify_Flush(t *testing.T) {
	t.Parallel()
	e := &events.Event{Topic: []string{TopicSymbolFlush}}
	if got := Classify(e); got != EventFlush {
		t.Errorf("Classify = %q, want %q", got, EventFlush)
	}
}

func TestClassify_UnknownTopic(t *testing.T) {
	t.Parallel()
	// An unrelated symbol (e.g., from another protocol that
	// happens to ride through the dispatcher). Classify must
	// return empty so the consumer skips it.
	other := b64(t, symbol("transfer"))
	e := &events.Event{Topic: []string{other}}
	if got := Classify(e); got != "" {
		t.Errorf("Classify on unknown topic = %q, want empty", got)
	}
}

func TestClassify_EmptyTopic(t *testing.T) {
	t.Parallel()
	e := &events.Event{Topic: nil}
	if got := Classify(e); got != "" {
		t.Errorf("Classify on empty topic = %q, want empty", got)
	}
}

// ─── DecodePayment ───────────────────────────────────────────────

func TestDecodePayment_HappyPath(t *testing.T) {
	t.Parallel()
	from := makeAccountStrkey(t, 0x11)
	dest := makeAccountStrkey(t, 0x22)
	body := b64(t, scMap(
		// ScMap fields are emitted alphabetically by the macro.
		xdr.ScMapEntry{Key: symbol("amount"), Val: i128(big.NewInt(12_345_678))},
		xdr.ScMapEntry{Key: symbol("destination"), Val: accountAddrFromStrkey(t, dest)},
		xdr.ScMapEntry{Key: symbol("from"), Val: accountAddrFromStrkey(t, from)},
		xdr.ScMapEntry{Key: symbol("memo"), Val: scString("binance-deposit-tag-987654")},
	))
	e := &events.Event{
		Type:                     "contract",
		Ledger:                   62_700_000,
		LedgerClosedAt:           "2026-05-20T13:30:00Z",
		ContractID:               MainnetPaymentContract,
		ID:                       "0001-rozo-fixture",
		OperationIndex:           0,
		TxHash:                   "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		InSuccessfulContractCall: true,
		Topic:                    []string{TopicSymbolPayment, b64(t, accountAddrFromStrkey(t, from))},
		Value:                    body,
	}
	got, err := DecodePayment(e)
	if err != nil {
		t.Fatalf("DecodePayment: %v", err)
	}
	if got.From != from {
		t.Errorf("From = %q, want %q", got.From, from)
	}
	if got.Destination != dest {
		t.Errorf("Destination = %q, want %q", got.Destination, dest)
	}
	if got.Amount != "12345678" {
		t.Errorf("Amount = %q, want \"12345678\"", got.Amount)
	}
	if got.Memo != "binance-deposit-tag-987654" {
		t.Errorf("Memo = %q", got.Memo)
	}
	if got.Ledger != 62_700_000 || got.OpIndex != 0 || got.ContractID != MainnetPaymentContract {
		t.Errorf("envelope fields not threaded; got %+v", got)
	}
}

// TestDecodePayment_LargeI128 catches the ADR-0003 invariant —
// USDC has 7 decimals so a >2^53 amount (e.g. a large institutional
// transfer above ~90 billion display USDC) MUST round-trip through
// the decoder as a *big.Int, not silently truncated to int64.
func TestDecodePayment_LargeI128(t *testing.T) {
	t.Parallel()
	big1 := new(big.Int)
	big1.SetString("123456789012345678901234567890", 10) // ~10^29, way above 2^53
	from := makeAccountStrkey(t, 0x11)
	dest := makeAccountStrkey(t, 0x22)
	body := b64(t, scMap(
		xdr.ScMapEntry{Key: symbol("amount"), Val: i128(big1)},
		xdr.ScMapEntry{Key: symbol("destination"), Val: accountAddrFromStrkey(t, dest)},
		xdr.ScMapEntry{Key: symbol("from"), Val: accountAddrFromStrkey(t, from)},
		xdr.ScMapEntry{Key: symbol("memo"), Val: scString("")},
	))
	e := &events.Event{Value: body}
	got, err := DecodePayment(e)
	if err != nil {
		t.Fatalf("DecodePayment: %v", err)
	}
	if got.Amount != big1.String() {
		t.Errorf("Amount round-trip lost precision: got %q, want %q", got.Amount, big1.String())
	}
}

func TestDecodePayment_MissingField(t *testing.T) {
	t.Parallel()
	from := makeAccountStrkey(t, 0x11)
	dest := makeAccountStrkey(t, 0x22)
	// Body missing `memo` — contract change would surface here.
	body := b64(t, scMap(
		xdr.ScMapEntry{Key: symbol("amount"), Val: i128(big.NewInt(1))},
		xdr.ScMapEntry{Key: symbol("destination"), Val: accountAddrFromStrkey(t, dest)},
		xdr.ScMapEntry{Key: symbol("from"), Val: accountAddrFromStrkey(t, from)},
	))
	e := &events.Event{Value: body}
	_, err := DecodePayment(e)
	if err == nil {
		t.Fatal("expected ErrMalformedBody, got nil")
	}
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("want ErrMalformedBody, got %v", err)
	}
	if !strings.Contains(err.Error(), "memo") {
		t.Errorf("error should name the missing field; got %v", err)
	}
}

func TestDecodePayment_WrongTopLevelKind(t *testing.T) {
	t.Parallel()
	// Pass a non-Map ScVal as the body (a bare i128). Soroban
	// contract upgrade that returns the wrong shape would surface
	// here as the early-fail signal.
	body := b64(t, i128(big.NewInt(42)))
	e := &events.Event{Value: body}
	_, err := DecodePayment(e)
	if err == nil {
		t.Fatal("expected decode error on non-Map body")
	}
	if !strings.Contains(err.Error(), "not a map") {
		t.Errorf("error should mention 'not a map'; got %v", err)
	}
}

// ─── DecodeFlush ─────────────────────────────────────────────────

func TestDecodeFlush_HappyPath(t *testing.T) {
	t.Parallel()
	usdc := makeContractStrkey(t, 0x33) // USDC SAC contract
	dest := makeAccountStrkey(t, 0x44)
	body := b64(t, scMap(
		xdr.ScMapEntry{Key: symbol("amount"), Val: i128(big.NewInt(99_999))},
		xdr.ScMapEntry{Key: symbol("destination"), Val: accountAddrFromStrkey(t, dest)},
		xdr.ScMapEntry{Key: symbol("token"), Val: contractAddrFromStrkey(t, usdc)},
	))
	e := &events.Event{
		Ledger:         62_700_001,
		LedgerClosedAt: "2026-05-20T13:30:05Z",
		ContractID:     MainnetPaymentContract,
		TxHash:         "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe",
		Topic:          []string{TopicSymbolFlush},
		Value:          body,
	}
	got, err := DecodeFlush(e)
	if err != nil {
		t.Fatalf("DecodeFlush: %v", err)
	}
	if got.Token != usdc {
		t.Errorf("Token = %q, want %q", got.Token, usdc)
	}
	if got.Destination != dest {
		t.Errorf("Destination = %q, want %q", got.Destination, dest)
	}
	if got.Amount != "99999" {
		t.Errorf("Amount = %q", got.Amount)
	}
}

func TestDecodeFlush_MissingField(t *testing.T) {
	t.Parallel()
	usdc := makeContractStrkey(t, 0x33)
	// Body missing 'destination' — contract drift surfacing.
	body := b64(t, scMap(
		xdr.ScMapEntry{Key: symbol("amount"), Val: i128(big.NewInt(1))},
		xdr.ScMapEntry{Key: symbol("token"), Val: contractAddrFromStrkey(t, usdc)},
	))
	e := &events.Event{Value: body}
	_, err := DecodeFlush(e)
	if err == nil {
		t.Fatal("expected ErrMalformedBody")
	}
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("want ErrMalformedBody, got %v", err)
	}
	if !strings.Contains(err.Error(), "destination") {
		t.Errorf("error should name the missing field; got %v", err)
	}
}

// ─── Topic constants are encoded once at init ────────────────────

func TestTopicSymbolPayment_StableEncoding(t *testing.T) {
	t.Parallel()
	// Lock the on-wire bytes — a re-encoded value MUST be
	// byte-identical to the package-init constant. Drift here
	// means scval's encoder changed and every classify() call
	// would silently miss matches.
	want := b64(t, symbol(EventPayment))
	if TopicSymbolPayment != want {
		t.Errorf("TopicSymbolPayment drift: pkg = %q, re-encoded = %q", TopicSymbolPayment, want)
	}
}

func TestTopicSymbolFlush_StableEncoding(t *testing.T) {
	t.Parallel()
	want := b64(t, symbol(EventFlush))
	if TopicSymbolFlush != want {
		t.Errorf("TopicSymbolFlush drift: pkg = %q, re-encoded = %q", TopicSymbolFlush, want)
	}
}

// ─── real-lake golden frames (base64 SCVal) ──────────────────────
//
// ROADMAP #89 residual: payment_event's BODY decode had never been
// verified against real on-chain bytes (only the topic SHAPE —
// topic_count=1 — was lake-verified on 2026-07-09; see the Payment
// doc comment in events.go). Captured read-only from the ClickHouse
// raw lake (stellar.contract_events) on r1, scoped to the four
// MainnetPaymentContracts, topic_0_sym = 'payment_event'
// (2026-07-10). Same base64-capture convention as
// internal/sources/blend_backstop/decode_test.go's goldenFrames.
//
// Verdict: DecodePayment agrees with the real wire shape exactly —
// alphabetical ScMap {amount, destination, from, memo}, all four
// fields present, i128 amount, both from/destination as ACCOUNT
// (G-strkey) addresses. No decoder change was needed; this locks
// the verified-correct shape against regression.
var rozoGoldenPaymentFrames = map[string]struct {
	contractID string
	ledger     uint32
	txHash     string
	topic0     string
	data       string
	wantFrom   string
	wantDest   string
	wantAmount string
	wantMemo   string
}{
	// Ledger 60829397 — destination is a bridge-out address that is
	// NOT one of the documented MainnetRelayerAccounts (pay() can
	// target any G-address the caller supplies).
	"ledger_60829397": {
		contractID: "CCRLTS3CMJHYHFD7MYRBJPNW6R3LCXNDO2B6TK6AS6FSXAHR6GBMGLRE",
		ledger:     60_829_397,
		txHash:     "e73744c150314a8618ece9b6ac0f49f2af6cf8ac3c634a7ae98bf8d1d3efa4c6",
		topic0:     "AAAADwAAAA1wYXltZW50X2V2ZW50AAAA",
		data:       "AAAAEQAAAAEAAAAEAAAADwAAAAZhbW91bnQAAAAAAAoAAAAAAAAAAAAAAAAAD0JAAAAADwAAAAtkZXN0aW5hdGlvbgAAAAASAAAAAAAAAAC74NxNJZfkyMo49xT06YtUUiYd2PeMbkwfrq/h40f6GAAAAA8AAAAEZnJvbQAAABIAAAAAAAAAAPVMuB2JbIWeb4L8Ic1on67yGzC6b8baAv5p8YiXV1soAAAADwAAAARtZW1vAAAADgAAABVwYXltZW50XzE3Njg3NTQ1NDMyMTIAAAA=",
		wantFrom:   "GD2UZOA5RFWILHTPQL6CDTLIT6XPEGZQXJX4NWQC7ZU7DCEXK5NSQ2GH",
		wantDest:   "GC56BXCNEWL6JSGKHD3RJ5HJRNKFEJQ53D3YY3SMD6XK7YPDI75BQ7FD",
		wantAmount: "1000000",
		wantMemo:   "payment_1768754543212",
	},
	// Ledger 60829399 — same `from` (a relayer-fed source account),
	// destination IS the first MainnetRelayerAccounts entry.
	"ledger_60829399": {
		contractID: "CAC5SKP5FJT2ZZ7YLV4UCOM6Z5SQCCVPZWHLLLVQNQG2RWWOOSP3IYRL",
		ledger:     60_829_399,
		txHash:     "d3193380289f82bc29c538e6a0289268336bdd663862ef2ea87987990da186ae",
		topic0:     "AAAADwAAAA1wYXltZW50X2V2ZW50AAAA",
		data:       "AAAAEQAAAAEAAAAEAAAADwAAAAZhbW91bnQAAAAAAAoAAAAAAAAAAAAAAAAAD0JAAAAADwAAAAtkZXN0aW5hdGlvbgAAAAASAAAAAAAAAAAGNGBVjrPj89i2Sd2tAn99kLSUpPUAG5A8rVRgnOHl+wAAAA8AAAAEZnJvbQAAABIAAAAAAAAAAPVMuB2JbIWeb4L8Ic1on67yGzC6b8baAv5p8YiXV1soAAAADwAAAARtZW1vAAAADgAAABVwYXltZW50XzE3Njg3NTQ1NDMyMTIAAAA=",
		wantFrom:   "GD2UZOA5RFWILHTPQL6CDTLIT6XPEGZQXJX4NWQC7ZU7DCEXK5NSQ2GH",
		wantDest:   "GADDIYCVR2Z6H46YWZE53LICP56ZBNEUUT2QAG4QHSWVIYE44HS7W3XY",
		wantAmount: "1000000",
		wantMemo:   "payment_1768754543212",
	},
	// Ledger 60837217 — a smaller amount + a distinct "memo_<ts>"
	// tag shape (vs. the "payment_<ts>" shape above), exercising a
	// different real memo pattern.
	"ledger_60837217": {
		contractID: "CAC5SKP5FJT2ZZ7YLV4UCOM6Z5SQCCVPZWHLLLVQNQG2RWWOOSP3IYRL",
		ledger:     60_837_217,
		txHash:     "e4114983c577e6f8ae8047fb02508b36857d5a2881b49283561e84bff5a5c41a",
		topic0:     "AAAADwAAAA1wYXltZW50X2V2ZW50AAAA",
		data:       "AAAAEQAAAAEAAAAEAAAADwAAAAZhbW91bnQAAAAAAAoAAAAAAAAAAAAAAAAAB6EgAAAADwAAAAtkZXN0aW5hdGlvbgAAAAASAAAAAAAAAAAGNGBVjrPj89i2Sd2tAn99kLSUpPUAG5A8rVRgnOHl+wAAAA8AAAAEZnJvbQAAABIAAAAAAAAAAPVMuB2JbIWeb4L8Ic1on67yGzC6b8baAv5p8YiXV1soAAAADwAAAARtZW1vAAAADgAAABJtZW1vXzE3Njg3OTg1OTk4MjkAAA==",
		wantFrom:   "GD2UZOA5RFWILHTPQL6CDTLIT6XPEGZQXJX4NWQC7ZU7DCEXK5NSQ2GH",
		wantDest:   "GADDIYCVR2Z6H46YWZE53LICP56ZBNEUUT2QAG4QHSWVIYE44HS7W3XY",
		wantAmount: "500000",
		wantMemo:   "memo_1768798599829",
	},
}

// TestGolden_Payment_LakeBytes pins DecodePayment against real
// mainnet lake bytes across both the original and the 2026-07-09-
// admitted 4th contract's sibling deployments, and across both
// observed memo naming conventions. This is the ROADMAP #89 residual
// verification: prior to this test, the body field-name mapping
// (amount/destination/from/memo) was documented from the contract
// source but never round-tripped against a captured real event.
func TestGolden_Payment_LakeBytes(t *testing.T) {
	t.Parallel()
	for name, f := range rozoGoldenPaymentFrames {
		f := f
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := &events.Event{
				Type:           "contract",
				ContractID:     f.contractID,
				Ledger:         f.ledger,
				LedgerClosedAt: "2026-05-27T00:00:00Z",
				TxHash:         f.txHash,
				Topic:          []string{f.topic0},
				Value:          f.data,
			}
			got, err := DecodePayment(e)
			if err != nil {
				t.Fatalf("DecodePayment: %v", err)
			}
			if got.From != f.wantFrom {
				t.Errorf("From = %q, want %q", got.From, f.wantFrom)
			}
			if got.Destination != f.wantDest {
				t.Errorf("Destination = %q, want %q", got.Destination, f.wantDest)
			}
			if got.Amount != f.wantAmount {
				t.Errorf("Amount = %q, want %q", got.Amount, f.wantAmount)
			}
			if got.Memo != f.wantMemo {
				t.Errorf("Memo = %q, want %q", got.Memo, f.wantMemo)
			}
			// Also drive the dispatcher-facing Classify() + envelope
			// path so the golden bytes exercise the same route
			// production ingest takes.
			if kind := Classify(e); kind != EventPayment {
				t.Errorf("Classify = %q, want %q", kind, EventPayment)
			}
		})
	}
}

// TestGolden_FlushEvent_NeverObserved documents that flush_event
// (the admin sweep path) has never fired on mainnet across any of
// the four gated v1 Payment contracts as of the 2026-07-10 lake
// census (topic_0_sym = 'flush_event' returns zero rows) — so
// DecodeFlush's body shape remains UNVERIFIED against real bytes,
// unlike DecodePayment above. This is the honest counterpart to the
// blend_backstop precedent (rw_zone_remove: synthetic-from-source,
// unverified) rather than a silent gap.
func TestGolden_FlushEvent_NeverObserved(t *testing.T) {
	t.Parallel()
	t.Log("flush_event: 0 real occurrences across all 4 MainnetPaymentContracts " +
		"as of the 2026-07-10 ClickHouse lake census (ROADMAP #89) — DecodeFlush " +
		"is exercised only by the synthetic fixtures in TestDecodeFlush_HappyPath / " +
		"TestDecodeFlush_MissingField above, not real lake bytes")
}
