package aquarius

import (
	"errors"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Bodies are the lake's data_xdr for every event concentrated pool CABOSWXY... emitted (ledgers 65,675,307-65,678,793).
// The lake export carries no topics for these rows, so the deposit topics are built from the pool's token pair as the
// add_pool fixture announces it ([CAUIKL3I..., CBSJZEIO...]); the trade below pins CAUIKL3I... to reserve position 0.
const (
	clPool       = "CABOSWXYBLE2UJKYWITFRICR37D4UOB75CNXIH7VI5ZKKL526GFHXAUI"
	clToken0     = "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK"
	clToken0Blob = "AAAAEgAAAAEohS9owZhIjjRvsSEu1QKQU3Ycwk9FM5LjU5ggGwgl5w=="
	clToken1Blob = "AAAAEgAAAAFknJEO6L6hakXbFSbe6ZW6bdZtDfgJsVRGw6ZHiVM5HQ=="

	// ledger 65,675,307 event 5: [549667, 37766771, 8000]; update_reserves event 6: [37766771, 8000].
	clDeposit1Body  = "AAAAEAAAAAEAAAADAAAACgAAAAAAAAAAAAAAAAAIYyMAAAAKAAAAAAAAAAAAAAAAAkBGcwAAAAoAAAAAAAAAAAAAAAAAAB9A"
	clReserves1Body = "AAAAEAAAAAEAAAACAAAACgAAAAAAAAAAAAAAAAJARnMAAAAKAAAAAAAAAAAAAAAAAAAfQA=="
	// ledger 65,675,439 event 3: [5496676377, 377668139960, 80000000]; update_reserves event 4: [377705906731, 80008000].
	clDeposit2Body  = "AAAAEAAAAAEAAAADAAAACgAAAAAAAAAAAAAAAUegoBkAAAAKAAAAAAAAAAAAAABX7sZ7uAAAAAoAAAAAAAAAAAAAAAAExLQA"
	clReserves2Body = "AAAAEAAAAAEAAAACAAAACgAAAAAAAAAAAAAAV/EGwisAAAAKAAAAAAAAAAAAAAAABMTTQA=="
	// ledger 65,678,793 event 3: trade [sold 12635, bought 59576995, fee 13], token_out CAUIKL3I...; update_reserves
	// event 4: [377646329736, 80020629], so reserve[0] fell by exactly the 59576995 of CAUIKL3I... bought.
	clTradeBody     = "AAAAEAAAAAEAAAADAAAACgAAAAAAAAAAAAAAAAAAMVsAAAAKAAAAAAAAAAAAAAAAA40SowAAAAoAAAAAAAAAAAAAAAAAAAAN"
	clReserves3Body = "AAAAEAAAAAEAAAACAAAACgAAAAAAAAAAAAAAV+15r4gAAAAKAAAAAAAAAAAAAAAABMUElQ=="
)

func clEvent(topic0, body string, ledger uint32, eventIndex int, tokens ...string) events.Event {
	return events.Event{
		Type: "contract", ContractID: clPool, Ledger: ledger, TxHash: "cl", EventIndex: eventIndex,
		LedgerClosedAt: "2026-11-26T03:09:59Z", Topic: append([]string{topic0}, tokens...), Value: body,
	}
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

func reservesOf(t *testing.T, d *Decoder, body string, ledger uint32, idx int) []string {
	t.Helper()
	rv, ok := decodeOne(t, d, clEvent(TopicSymbolUpdateReserves, body, ledger, idx)).(ReservesEvent)
	if !ok {
		t.Fatal("update_reserves did not decode to a ReservesEvent")
	}
	return []string{rv.Reserves[0].String(), rv.Reserves[1].String()}
}

// A concentrated deposit_liquidity body is [liquidity, amount0, amount1]. The shares-last order would store the liquidity
// as token0's amount, amount0 as token1's and amount1 as shares.
func TestConcentratedDeposit_realBodiesDecodeAmountsAndLiquidity(t *testing.T) {
	_, addPool, _ := loadRouterFixture(t, "add_pool_65675307_a43b2fa30ab8.json")
	for name, d := range map[string]*Decoder{
		"announced live": func() *Decoder {
			d := NewDecoder(contractid.WithSeed(nil))
			if _, err := d.Decode(addPool); err != nil {
				t.Fatal(err)
			}
			return d
		}(),
		"in-code list after restart": NewDecoder(contractid.WithSeed([]string{clPool})),
	} {
		t.Run(name, func(t *testing.T) {
			cases := []struct {
				body, reserves   string
				ledger           uint32
				idx              int
				liq, a0, a1      string
				wantReserveAfter []string
			}{
				{clDeposit1Body, clReserves1Body, 65_675_307, 5, "549667", "37766771", "8000", []string{"37766771", "8000"}},
				{clDeposit2Body, clReserves2Body, 65_675_439, 3, "5496676377", "377668139960", "80000000", []string{"377705906731", "80008000"}},
			}
			sum := [2]*big.Int{new(big.Int), new(big.Int)}
			for _, c := range cases {
				lq, ok := decodeOne(t, d, clEvent(TopicSymbolDepositLiquidity, c.body, c.ledger, c.idx, clToken0Blob, clToken1Blob)).(LiquidityEvent)
				if !ok {
					t.Fatal("deposit did not decode to a LiquidityEvent")
				}
				if !lq.Concentrated || lq.Liquidity.String() != c.liq {
					t.Errorf("@%d concentrated=%v liquidity=%s, want true/%s", c.ledger, lq.Concentrated, lq.Liquidity, c.liq)
				}
				if len(lq.Amounts) != 2 || lq.Amounts[0].String() != c.a0 || lq.Amounts[1].String() != c.a1 {
					t.Fatalf("@%d amounts=%v, want [%s %s]", c.ledger, lq.Amounts, c.a0, c.a1)
				}
				if lq.Tokens[0] != clToken0 {
					t.Errorf("@%d token0=%s, want %s", c.ledger, lq.Tokens[0], clToken0)
				}
				if !lq.Shares.IsZero() {
					t.Errorf("@%d shares=%s, want unset (a concentrated pool mints no LP shares)", c.ledger, lq.Shares)
				}
				// The pool's own post-deposit reserves are the running sums of the decoded amounts.
				sum[0].Add(sum[0], lq.Amounts[0].BigInt())
				sum[1].Add(sum[1], lq.Amounts[1].BigInt())
				got := reservesOf(t, d, c.reserves, c.ledger, c.idx+1)
				if got[0] != sum[0].String() || got[1] != sum[1].String() || got[0] != c.wantReserveAfter[0] || got[1] != c.wantReserveAfter[1] {
					t.Errorf("@%d reserves=%v, running deposit sums=%v", c.ledger, got, sum)
				}
			}
			before := reservesOf(t, d, clReserves2Body, 65_675_439, 4)
			after := reservesOf(t, d, clReserves3Body, 65_678_793, 4)
			drop := new(big.Int).Sub(mustBig(t, before[0]), mustBig(t, after[0]))
			tr, ok := decodeOne(t, d, clEvent(TopicSymbolTrade, clTradeBody, 65_678_793, 3, clToken1Blob, clToken0Blob, clToken0Blob)).(TradeEvent)
			if !ok {
				t.Fatal("trade did not decode")
			}
			if drop.Cmp(tr.Trade.QuoteAmount.BigInt()) != 0 {
				t.Errorf("reserve[0] fell by %s, trade bought %s of token0", drop, tr.Trade.QuoteAmount)
			}
		})
	}
}

func TestConcentratedWithdraw_failsLoud(t *testing.T) {
	d := NewDecoder()
	ev := clEvent(TopicSymbolWithdrawLiquidity, clDeposit1Body, 65_700_000, 1, clToken0Blob, clToken1Blob)
	if !d.Matches(ev) {
		t.Fatal("concentrated pool not gated in")
	}
	out, err := d.Decode(ev)
	if !errors.Is(err, ErrUnverifiedLiquidityLayout) || out != nil {
		t.Fatalf("Decode(concentrated withdraw) = %v, %v; want nil, ErrUnverifiedLiquidityLayout", out, err)
	}
}

// A pool admitted without a known layout (a protocol_contracts row announced after the census ledger and not in the
// in-code list) must refuse its liquidity events rather than default to the shares-last order.
func TestLiquidity_unknownLayoutFailsLoud(t *testing.T) {
	pool := makeContractStrkey(t, 0x77)
	laterAddPool := events.Event{
		ContractID: MainnetRouter, Ledger: SharesLastThroughLedger + 1, Topic: []string{TopicSymbolAddPool},
		Value: encodeAddPoolBody(t, pool),
	}
	for name, d := range map[string]*Decoder{
		"warmed from protocol_contracts only": NewDecoder(contractid.WithSeed([]string{pool})),
		"announced after the census ledger without a type": func() *Decoder {
			d := NewDecoder()
			if _, err := d.Decode(laterAddPool); err != nil {
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

// A pool the router announced at or before the census ledger keeps the shares-last order whatever its add_pool body.
func TestLiquidity_poolAnnouncedBeforeCensusIsSharesLast(t *testing.T) {
	pool := makeContractStrkey(t, 0x78)
	d := NewDecoder()
	if _, err := d.Decode(events.Event{
		ContractID: MainnetRouter, Ledger: SharesLastThroughLedger, Topic: []string{TopicSymbolAddPool},
		Value: encodeAddPoolBody(t, pool),
	}); err != nil {
		t.Fatal(err)
	}
	lq, ok := decodeOne(t, d, events.Event{
		ContractID: pool, Ledger: 65_700_000, LedgerClosedAt: "2026-11-26T03:09:59Z",
		Topic: []string{TopicSymbolDepositLiquidity, realDepositTokenA, realDepositTokenB}, Value: realDepositBody,
	}).(LiquidityEvent)
	if !ok || lq.Concentrated || lq.Shares.IsZero() {
		t.Fatalf("decoded %+v, want a shares-last LiquidityEvent", lq)
	}
}

func TestLayoutForAnnouncement(t *testing.T) {
	cases := []struct {
		ledger   uint32
		poolType string
		want     string
	}{
		{65_675_307, "concentrated", LayoutConcentrated},
		{60_000_000, "concentrated", LayoutConcentrated},
		{SharesLastThroughLedger, "", LayoutSharesLast},
		{52_728_375, "anything", LayoutSharesLast},
		{SharesLastThroughLedger + 1, "", ""},
		{SharesLastThroughLedger + 1, "constant_product", ""},
		{0, "", ""},
	}
	for _, c := range cases {
		if got := LayoutForAnnouncement(c.ledger, c.poolType); got != c.want {
			t.Errorf("LayoutForAnnouncement(%d, %q) = %q, want %q", c.ledger, c.poolType, got, c.want)
		}
	}
}
