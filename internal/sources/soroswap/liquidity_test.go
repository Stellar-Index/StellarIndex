package soroswap

import (
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Real mainnet deposit + withdraw bodies captured from the ClickHouse
// lake (deposit pair CDLMAKG5…, ledger 63,181,271; withdraw pair
// CA3PA2NY…, ledger 62,940,668). Proves sdkDecodeLiquidity extracts all
// five i128 fields + the `to` provider from the REAL on-chain ScvMap
// shape (not a synthetic body). deposit and withdraw share one struct.
const (
	realDepositBody  = "AAAAEQAAAAEAAAAGAAAADwAAAAhhbW91bnRfMAAAAAoAAAAAAAAAAAAAAAAM7ZpEAAAADwAAAAhhbW91bnRfMQAAAAoAAAAAAAAAAAAAAAnHZSQAAAAADwAAAAlsaXF1aWRpdHkAAAAAAAAKAAAAAAAAAAAAAAAAryjoaQAAAA8AAAANbmV3X3Jlc2VydmVfMAAAAAAAAAoAAAAAAAAAAAAAAAFFR+O9AAAADwAAAA1uZXdfcmVzZXJ2ZV8xAAAAAAAACgAAAAAAAAAAAAAA9gsmmTQAAAAPAAAAAnRvAAAAAAASAAAAAAAAAAB/8XxHTA1msG+0b3b1TJd6rspEiEzR8MYWBjCsXdFIow=="
	realWithdrawBody = "AAAAEQAAAAEAAAAGAAAADwAAAAhhbW91bnRfMAAAAAoAAAAAAAAAAAAAAAAGfa6bAAAADwAAAAhhbW91bnRfMQAAAAoAAAAAAAAAAAAABJMjWbR7AAAADwAAAAlsaXF1aWRpdHkAAAAAAAAKAAAAAAAAAAAAAAAFcwMiGgAAAA8AAAANbmV3X3Jlc2VydmVfMAAAAAAAAAoAAAAAAAAAAAAAAAAAAAAFAAAADwAAAA1uZXdfcmVzZXJ2ZV8xAAAAAAAACgAAAAAAAAAAAAAAAAADR4UAAAAPAAAAAnRvAAAAAAASAAAAAAAAAAAqgK2+TSm3Zi13f+1ZI/dUxFKl2W6gSs4aa+ABaSQiZw=="
)

func TestDecodeLiquidity_realBodies(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		amount0 string
		amount1 string
		liq     string
		r0      string
		r1      string
		to      string
	}{
		{
			name: "deposit", body: realDepositBody,
			amount0: "216898116", amount1: "42000000000", liq: "2938693737",
			r0: "5457306557", r1: "1056749033780",
			to: "GB77C7CHJQGWNMDPWRXXN5KMS55K5SSERBGND4GGCYDDBLC52FEKHUOR",
		},
		{
			name: "withdraw", body: realWithdrawBody,
			amount0: "108899995", amount1: "5029999785083", liq: "23404421658",
			r0: "5", r1: "214917",
			to: "GAVIBLN6JUU3OZRNO5762WJD65KMIUVF3FXKASWODJV6AALJEQRGPYOO",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := sdkDecodeLiquidity(tc.body)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if f.Amount0.String() != tc.amount0 {
				t.Errorf("amount_0 = %s, want %s", f.Amount0, tc.amount0)
			}
			if f.Amount1.String() != tc.amount1 {
				t.Errorf("amount_1 = %s, want %s", f.Amount1, tc.amount1)
			}
			if f.Liquidity.String() != tc.liq {
				t.Errorf("liquidity = %s, want %s", f.Liquidity, tc.liq)
			}
			if f.NewReserve0.String() != tc.r0 {
				t.Errorf("new_reserve_0 = %s, want %s", f.NewReserve0, tc.r0)
			}
			if f.NewReserve1.String() != tc.r1 {
				t.Errorf("new_reserve_1 = %s, want %s", f.NewReserve1, tc.r1)
			}
			if f.To != tc.to {
				t.Errorf("to = %s, want %s", f.To, tc.to)
			}
		})
	}
}

// A malformed body (missing a required field) must ERROR, not silently
// drop a leg — the every-event mission demands honest failure over a
// half-decoded row.
func TestDecodeLiquidity_missingFieldErrors(t *testing.T) {
	body := b64(t, scMap(
		xdr.ScMapEntry{Key: symbol("amount_0"), Val: i128(big.NewInt(1))},
		xdr.ScMapEntry{Key: symbol("amount_1"), Val: i128(big.NewInt(2))},
		// liquidity / reserves / to missing.
	))
	if _, err := sdkDecodeLiquidity(body); err == nil {
		t.Fatal("expected error for a body missing liquidity/reserves/to")
	}
}

// makeLiquidityEvent builds a pair-contract deposit/withdraw
// events.Event with the six #[contracttype] fields, for the
// adapter-level tests.
func makeLiquidityEvent(t *testing.T, pair, action string, amt0, amt1, liq, r0, r1 *big.Int, to string) events.Event {
	t.Helper()
	sym := TopicSymbolDeposit
	if action == EventWithdraw {
		sym = TopicSymbolWithdraw
	}
	body := b64(t, scMap(
		xdr.ScMapEntry{Key: symbol("amount_0"), Val: i128(amt0)},
		xdr.ScMapEntry{Key: symbol("amount_1"), Val: i128(amt1)},
		xdr.ScMapEntry{Key: symbol("liquidity"), Val: i128(liq)},
		xdr.ScMapEntry{Key: symbol("new_reserve_0"), Val: i128(r0)},
		xdr.ScMapEntry{Key: symbol("new_reserve_1"), Val: i128(r1)},
		xdr.ScMapEntry{Key: symbol("to"), Val: contractAddrFromStrkey(t, to)},
	))
	return events.Event{
		Topic:          []string{TopicPrefixPair, sym},
		Value:          body,
		Ledger:         52_000_002,
		TxHash:         "liqtx0",
		OperationIndex: 0,
		EventIndex:     0,
		LedgerClosedAt: "2026-04-23T12:00:02Z",
		ContractID:     pair,
	}
}

// A seeded pair's deposit resolves token identities and emits exactly
// one LiquidityEvent carrying every field.
func TestDecoder_Decode_depositEmitsLiquidityEvent(t *testing.T) {
	d := NewDecoder()
	token0 := makeContractStrkey(t, 0x10)
	token1 := makeContractStrkey(t, 0x11)
	pair := makeContractStrkey(t, 0x20)
	a0, _ := canonical.NewSorobanAsset(token0)
	a1, _ := canonical.NewSorobanAsset(token1)
	d.SeedPair(pair, a0, a1)
	provider := makeContractStrkey(t, 0x99)

	dep := makeLiquidityEvent(t, pair, EventDeposit,
		big.NewInt(111), big.NewInt(222), big.NewInt(333),
		big.NewInt(4444), big.NewInt(5555), provider)

	out, err := d.Decode(dep)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 event, got %d", len(out))
	}
	le, ok := out[0].(LiquidityEvent)
	if !ok {
		t.Fatalf("want LiquidityEvent, got %T", out[0])
	}
	if le.Action != EventDeposit {
		t.Errorf("action = %q, want deposit", le.Action)
	}
	if le.Token0 != a0.String() || le.Token1 != a1.String() {
		t.Errorf("tokens = (%s,%s), want (%s,%s)", le.Token0, le.Token1, a0, a1)
	}
	if le.Amount0.String() != "111" || le.Amount1.String() != "222" || le.Liquidity.String() != "333" {
		t.Errorf("amounts = (%s,%s,%s), want (111,222,333)", le.Amount0, le.Amount1, le.Liquidity)
	}
	if le.NewReserve0.String() != "4444" || le.NewReserve1.String() != "5555" {
		t.Errorf("reserves = (%s,%s), want (4444,5555)", le.NewReserve0, le.NewReserve1)
	}
	if le.To != provider {
		t.Errorf("to = %s, want %s", le.To, provider)
	}
}

// An UNSEEDED pair still emits the liquidity row — with empty token
// identities — rather than dropping the event. This is the every-event
// guarantee: token resolution is best-effort, the event is not.
func TestDecoder_Decode_withdrawUnseededPairStillEmits(t *testing.T) {
	d := NewDecoder()
	pair := makeContractStrkey(t, 0x21) // never SeedPair'd
	provider := makeContractStrkey(t, 0x98)

	wd := makeLiquidityEvent(t, pair, EventWithdraw,
		big.NewInt(9), big.NewInt(8), big.NewInt(7),
		big.NewInt(6), big.NewInt(5), provider)

	out, err := d.Decode(wd)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 event even for an unseeded pair, got %d", len(out))
	}
	le := out[0].(LiquidityEvent)
	if le.Action != EventWithdraw {
		t.Errorf("action = %q, want withdraw", le.Action)
	}
	if le.Token0 != "" || le.Token1 != "" {
		t.Errorf("tokens = (%q,%q), want empty (pair unseeded)", le.Token0, le.Token1)
	}
	if le.Amount0.String() != "9" || le.Liquidity.String() != "7" {
		t.Errorf("amounts wrong: %+v", le)
	}
}
