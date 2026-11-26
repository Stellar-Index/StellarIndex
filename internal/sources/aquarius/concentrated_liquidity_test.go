package aquarius

import (
	"errors"
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Fixtures: every deposit_liquidity of concentrated pools CABOSWXY... and CBQ6AGQY... with the same-tx update_reserves,
// position_update and pool_state, plus CABOSWXY's one trade; both pools run WASM 155a17b9... at every capture ledger.
const concentratedPoolWasmHash = "155a17b9929ffb1f9e84bd6ef5c00a4d613c1ab5f4ad4c502d84515250cc2907"

const clPool = "CABOSWXYBLE2UJKYWITFRICR37D4UOB75CNXIH7VI5ZKKL526GFHXAUI"

func clFixture(t *testing.T, name string) events.Event {
	t.Helper()
	_, ev, _ := loadHashFixture(t, concentratedPoolWasmHash, name)
	return ev
}

func decodeOne(t *testing.T, d *Decoder, ev events.Event) consumer.Event {
	t.Helper()
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode(%s@%d): %v", classify(&ev), ev.Ledger, err)
	}
	if len(out) != 1 {
		t.Fatalf("Decode(%s@%d) emitted %d events, want 1", classify(&ev), ev.Ledger, len(out))
	}
	return out[0]
}

func reservesOf(t *testing.T, d *Decoder, name string) []*big.Int {
	t.Helper()
	rv, ok := decodeOne(t, d, clFixture(t, name)).(ReservesEvent)
	if !ok || len(rv.Reserves) != 2 {
		t.Fatalf("%s did not decode to a 2-leg ReservesEvent", name)
	}
	return []*big.Int{rv.Reserves[0].BigInt(), rv.Reserves[1].BigInt()}
}

type clDepositTx struct{ deposit, reserves, position string }

func symVal(s string) xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

// A concentrated deposit_liquidity body is [liquidity, amount0, amount1]; the shares-last order would store the liquidity
// as token0's amount, amount0 as token1's and amount1 as shares. Per transaction the pool's own events cross-check the
// decode: position_update's delta equals the liquidity, and update_reserves equals the running sums of the amounts.
func TestConcentratedDeposit_fixturesCrossCheckAgainstSameTxEvents(t *testing.T) {
	pools := map[string][]clDepositTx{
		clPool: {
			{"deposit_liquidity_65675307_a43b2fa30ab8_5.json", "update_reserves_65675307_a43b2fa30ab8_6.json", "position_update_65675307_a43b2fa30ab8_7.json"},
			{"deposit_liquidity_65675439_2bcc1e4ad220_3.json", "update_reserves_65675439_2bcc1e4ad220_4.json", "position_update_65675439_2bcc1e4ad220_5.json"},
		},
		"CBQ6AGQYE5HCDK7VIDUH6756IFPBTLF2HNQH57DAFMOHK3CVDH4CAMXO": {
			{"deposit_liquidity_65675280_bea153581781_5.json", "update_reserves_65675280_bea153581781_6.json", "position_update_65675280_bea153581781_7.json"},
			{"deposit_liquidity_65675489_c755184d1e95_3.json", "update_reserves_65675489_c755184d1e95_4.json", "position_update_65675489_c755184d1e95_5.json"},
		},
	}
	d := NewDecoder()
	for pool, txs := range pools {
		sum := []*big.Int{new(big.Int), new(big.Int)}
		for _, tx := range txs {
			lq, ok := decodeOne(t, d, clFixture(t, tx.deposit)).(LiquidityEvent)
			if !ok || lq.ContractID != pool {
				t.Fatalf("%s did not decode to a LiquidityEvent of %s", tx.deposit, pool)
			}
			if !lq.Concentrated || len(lq.Amounts) != 2 || !lq.Shares.IsZero() {
				t.Fatalf("%s: concentrated=%v amounts=%v shares=%s; want concentrated, 2 amounts, no shares",
					tx.deposit, lq.Concentrated, lq.Amounts, lq.Shares)
			}
			puEv := clFixture(t, tx.position)
			pu, err := decodeRewardsEvent(&puEv, EventPositionUpdate, lq.ObservedAt)
			if err != nil {
				t.Fatal(err)
			}
			if pu.Attributes["delta"] != lq.Liquidity.String() {
				t.Errorf("%s: liquidity %s != same-tx position_update delta %v", tx.deposit, lq.Liquidity, pu.Attributes["delta"])
			}
			sum[0].Add(sum[0], lq.Amounts[0].BigInt())
			sum[1].Add(sum[1], lq.Amounts[1].BigInt())
			if got := reservesOf(t, d, tx.reserves); got[0].Cmp(sum[0]) != 0 || got[1].Cmp(sum[1]) != 0 {
				t.Errorf("%s: update_reserves %v != running deposit sums %v", tx.deposit, got, sum)
			}
		}
	}
}

// The first deposit's values are hand-decoded from the XDR (i128 lo words 0x086323, 0x02404673, 0x1f40), not read back.
func TestConcentratedDeposit_handDecodedValues(t *testing.T) {
	lq, ok := decodeOne(t, NewDecoder(), clFixture(t, "deposit_liquidity_65675307_a43b2fa30ab8_5.json")).(LiquidityEvent)
	if !ok {
		t.Fatal("not a LiquidityEvent")
	}
	if lq.Liquidity.String() != "549667" || lq.Amounts[0].String() != "37766771" || lq.Amounts[1].String() != "8000" {
		t.Errorf("liquidity=%s amounts=%v, want 549667 [37766771 8000]", lq.Liquidity, lq.Amounts)
	}
	if lq.Tokens[0] != "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK" {
		t.Errorf("token0 = %s", lq.Tokens[0])
	}
}

// The pool's one trade leaves its reserve of the bought token (token0) lower by exactly the bought amount.
func TestConcentratedTrade_reserveMovesByBoughtAmount(t *testing.T) {
	d := NewDecoder()
	tr, ok := decodeOne(t, d, clFixture(t, "trade_65678793_fdcd61113af4.json")).(TradeEvent)
	if !ok {
		t.Fatal("trade did not decode")
	}
	before := reservesOf(t, d, "update_reserves_65675439_2bcc1e4ad220_4.json")
	after := reservesOf(t, d, "update_reserves_65678793_fdcd61113af4_4.json")
	if drop := new(big.Int).Sub(before[0], after[0]); drop.Cmp(tr.Trade.QuoteAmount.BigInt()) != 0 {
		t.Errorf("reserve[0] fell by %s, trade bought %s of token0", drop, tr.Trade.QuoteAmount)
	}
}

// A pool that learns its layout from a live add_pool decodes the same as one typed by the in-code list.
func TestConcentratedDeposit_layoutFromLiveAddPool(t *testing.T) {
	_, addPool, _ := loadRouterFixture(t, "add_pool_65675307_a43b2fa30ab8.json")
	d := &Decoder{reg: contractid.New(contractid.WithFactories([]string{MainnetRouter}))}
	if _, err := d.Decode(addPool); err != nil {
		t.Fatal(err)
	}
	lq, ok := decodeOne(t, d, clFixture(t, "deposit_liquidity_65675307_a43b2fa30ab8_5.json")).(LiquidityEvent)
	if !ok || !lq.Concentrated || lq.Liquidity.String() != "549667" {
		t.Fatalf("decoded %+v, want the concentrated layout", lq)
	}
}

func TestConcentratedWithdraw_failsLoud(t *testing.T) {
	ev := clFixture(t, "deposit_liquidity_65675307_a43b2fa30ab8_5.json")
	ev.Topic[0] = TopicSymbolWithdrawLiquidity
	d := NewDecoder()
	if !d.Matches(ev) {
		t.Fatal("concentrated pool not gated in")
	}
	if out, err := d.Decode(ev); !errors.Is(err, ErrUnverifiedLiquidityLayout) || out != nil {
		t.Fatalf("Decode(concentrated withdraw) = %v, %v; want nil, ErrUnverifiedLiquidityLayout", out, err)
	}
}

// A pool admitted without a known layout (a protocol_contracts row not in either in-code list, or an add_pool with a
// type name other than "concentrated") must refuse its liquidity events rather than default to the shares-last order.
func TestLiquidity_unknownLayoutFailsLoud(t *testing.T) {
	pool := makeContractStrkey(t, 0x77)
	otherType := events.Event{
		ContractID: MainnetRouter, Ledger: 52_728_375, Topic: []string{TopicSymbolAddPool},
		Value: marshalB64(t, vecVal(decodeB64ScVal(t, encodeContractAddrFromStrkey(t, pool)), symVal("constant_product"))),
	}
	for name, d := range map[string]*Decoder{
		"warmed from protocol_contracts only": NewDecoder(contractid.WithSeed([]string{pool})),
		"announced with another type name": func() *Decoder {
			d := NewDecoder()
			if _, err := d.Decode(otherType); err != nil {
				t.Fatal(err)
			}
			return d
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			for _, topic0 := range []string{TopicSymbolDepositLiquidity, TopicSymbolWithdrawLiquidity} {
				ev := events.Event{
					ContractID: pool, Ledger: 65_700_000, LedgerClosedAt: "2026-11-26T03:09:59Z",
					Topic: []string{topic0, realDepositTokenA, realDepositTokenB}, Value: realDepositBody,
				}
				if !d.Matches(ev) {
					t.Fatal("pool not gated in")
				}
				if out, err := d.Decode(ev); !errors.Is(err, ErrUnknownLiquidityLayout) || out != nil {
					t.Errorf("Decode = %v, %v; want nil, ErrUnknownLiquidityLayout", out, err)
				}
			}
		})
	}
}

// A curated pool keeps the shares-last order.
func TestLiquidity_curatedPoolIsSharesLast(t *testing.T) {
	lq, ok := decodeOne(t, NewDecoder(), events.Event{
		ContractID: MainnetPools[0], Ledger: 65_700_000, LedgerClosedAt: "2026-11-26T03:09:59Z",
		Topic: []string{TopicSymbolDepositLiquidity, realDepositTokenA, realDepositTokenB}, Value: realDepositBody,
	}).(LiquidityEvent)
	if !ok || lq.Concentrated || lq.Shares.String() != "30000000000" {
		t.Fatalf("decoded %+v, want a shares-last LiquidityEvent", lq)
	}
}

func TestLayoutForAnnouncement(t *testing.T) {
	for typ, want := range map[string]string{
		"concentrated": LayoutConcentrated, "constant": LayoutSharesLast, "stable": LayoutSharesLast,
		"": "", "constant_product": "", "Stable": "",
	} {
		if got := LayoutForAnnouncement(typ); got != want {
			t.Errorf("LayoutForAnnouncement(%q) = %q, want %q", typ, got, want)
		}
	}
}
