package aquarius

import (
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// Withdraw fixtures predate the 155a17b9 upgrade (the lake has no instance change to that hash before 64,578,395), so
// their directory names the capture date, not a WASM hash.
const clWithdrawDir = "concentrated-2026-04-29"

// A concentrated withdraw_liquidity body is [liquidity, amount0, amount1], all positive; the same-tx position_update
// delta is -liquidity. Values are hand-decoded from the XDR (i128 lo words), not read back.
func TestConcentratedWithdraw_fixturesCrossCheckAgainstSameTxEvents(t *testing.T) {
	cases := []struct {
		name, withdraw, position    string
		liquidity, amount0, amount1 string
	}{
		{
			"single-sided token1", "withdraw_liquidity_62341437_bf55f09c8439_1.json", "position_update_62341437_bf55f09c8439_3.json",
			"12196208501", "0", "9999999",
		}, // 0x02d6f35f75, 0, 0x98967f
		{
			"single-sided token0", "withdraw_liquidity_62342234_78c520b8817a_1.json", "position_update_62342234_78c520b8817a_3.json",
			"10173297756", "999999", "0",
		}, // 0x025e60345c, 0x0f423f, 0
		{
			"two-sided", "withdraw_liquidity_62342237_d3850b73a054_2.json", "position_update_62342237_d3850b73a054_4.json",
			"3234539544", "657233", "112207592",
		}, // 0xc0cb2818, 0x0a0751, 0x06b026e8
	}
	d := NewDecoder()
	for _, c := range cases {
		_, ev, _ := loadHashFixture(t, clWithdrawDir, c.withdraw)
		lq, ok := decodeOne(t, d, ev).(LiquidityEvent)
		if !ok || lq.Action != LiquidityWithdraw || !lq.Concentrated || len(lq.Amounts) != 2 || !lq.Shares.IsZero() {
			t.Fatalf("%s: decoded %+v, want a concentrated withdraw with 2 amounts and no shares", c.name, lq)
		}
		if lq.Liquidity.String() != c.liquidity || lq.Amounts[0].String() != c.amount0 || lq.Amounts[1].String() != c.amount1 {
			t.Errorf("%s: liquidity=%s amounts=%v, want %s [%s %s]", c.name, lq.Liquidity, lq.Amounts, c.liquidity, c.amount0, c.amount1)
		}
		_, puEv, _ := loadHashFixture(t, clWithdrawDir, c.position)
		pu, err := decodeRewardsEvent(&puEv, EventPositionUpdate, lq.ObservedAt)
		if err != nil {
			t.Fatal(err)
		}
		if want := new(big.Int).Neg(lq.Liquidity.BigInt()).String(); pu.Attributes["delta"] != want {
			t.Errorf("%s: same-tx position_update delta %v, want -liquidity %s", c.name, pu.Attributes["delta"], want)
		}
	}
}

// Between the pool's consecutive withdraws at 62,342,234 and 62,342,237 its reserves fall by exactly the second
// withdraw's amounts, which pins amount0/amount1 to token order.
func TestConcentratedWithdraw_reservesFallByAmounts(t *testing.T) {
	d := NewDecoder()
	reserves := func(name string) []*big.Int {
		_, ev, _ := loadHashFixture(t, clWithdrawDir, name)
		rv, ok := decodeOne(t, d, ev).(ReservesEvent)
		if !ok || len(rv.Reserves) != 2 {
			t.Fatalf("%s did not decode to a 2-leg ReservesEvent", name)
		}
		return []*big.Int{rv.Reserves[0].BigInt(), rv.Reserves[1].BigInt()}
	}
	before := reserves("update_reserves_62342234_78c520b8817a_2.json")
	after := reserves("update_reserves_62342237_d3850b73a054_3.json")
	_, ev, _ := loadHashFixture(t, clWithdrawDir, "withdraw_liquidity_62342237_d3850b73a054_2.json")
	lq, ok := decodeOne(t, d, ev).(LiquidityEvent)
	if !ok {
		t.Fatal("withdraw did not decode")
	}
	for i := range 2 {
		if drop := new(big.Int).Sub(before[i], after[i]); drop.Cmp(lq.Amounts[i].BigInt()) != 0 {
			t.Errorf("reserve[%d] fell by %s, withdraw amount%d is %s", i, drop, i, lq.Amounts[i])
		}
	}
}

// Every pool the router's add_pool announced (testdata census: ledger, pool, type) is in exactly the list its type
// names, the lists hold nothing else, and the decoder seeds each with the layout its announcement implies.
func TestRouterCensus_poolListsMatchAnnouncedTypes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "router_add_pool_census.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	d := NewDecoder()
	var classic, concentrated int
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			t.Fatalf("census row %q", line)
		}
		pool, typ := f[1], f[2]
		inClassic, inCL := slices.Contains(MainnetPools, pool), slices.Contains(MainnetConcentratedPools, pool)
		switch typ {
		case "constant", "stable":
			classic++
			if !inClassic || inCL {
				t.Errorf("%s (%s): in MainnetPools=%v MainnetConcentratedPools=%v", pool, typ, inClassic, inCL)
			}
		case "concentrated":
			concentrated++
			if inClassic || !inCL {
				t.Errorf("%s (%s): in MainnetPools=%v MainnetConcentratedPools=%v", pool, typ, inClassic, inCL)
			}
		default:
			t.Fatalf("census type %q", typ)
		}
		if got, want := d.reg.AttrsOf(pool)[AttrLiquidityLayout], LayoutForAnnouncement(typ); got != want {
			t.Errorf("%s (%s): seeded layout %q, want %q", pool, typ, got, want)
		}
	}
	if classic != len(MainnetPools) || concentrated != len(MainnetConcentratedPools) {
		t.Errorf("census %d classic / %d concentrated, lists hold %d / %d", classic, concentrated, len(MainnetPools), len(MainnetConcentratedPools))
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
