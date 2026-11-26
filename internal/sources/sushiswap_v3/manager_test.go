package sushiswap_v3

import (
	"bufio"
	"errors"
	"math/big"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

type managerFixture struct {
	want, kind, txHash, data                         string
	ledger                                           uint32
	topics                                           []string
	tokenID, owner, recipient, liq, amount0, amount1 string
}

// loadManagerFixtures reads testdata/manager_events.txt; see its header for the columns.
func loadManagerFixtures(t *testing.T) []managerFixture {
	t.Helper()
	f, err := os.Open("testdata/manager_events.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []managerFixture
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		c := strings.Fields(line)
		if len(c) != 12 {
			t.Fatalf("fixture has %d columns, want 12: %.80s", len(c), line)
		}
		ledger, err := strconv.ParseUint(c[2], 10, 32)
		if err != nil {
			t.Fatalf("fixture ledger %q: %v", c[2], err)
		}
		out = append(out, managerFixture{
			want: c[0], kind: c[1], ledger: uint32(ledger), txHash: c[3], topics: strings.Split(c[4], ","), data: c[5],
			tokenID: c[6], owner: c[7], recipient: c[8], liq: c[9], amount0: c[10], amount1: c[11],
		})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func managerEvent(contractID string, topics []string, body string) events.Event {
	return events.Event{
		Type:                     "contract",
		Ledger:                   65_682_016,
		LedgerClosedAt:           "2026-11-25T10:00:00Z",
		ContractID:               contractID,
		TxHash:                   swapTxHash,
		InSuccessfulContractCall: true,
		Topic:                    topics,
		Value:                    body,
	}
}

func managerDecoder(opts ...contractid.Option) *Decoder {
	return newDecoder([]string{MainnetPositionManager}, opts...)
}

// amountCol renders an Amount the way the fixture spells it: "-" for a field not on the wire.
func amountCol(a canonical.Amount, present bool) string {
	if !present {
		return "-"
	}
	return a.String()
}

func strCol(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// TestManagerFixtures decodes every captured manager event to its exact columns, or refuses it.
func TestManagerFixtures(t *testing.T) {
	fixtures := loadManagerFixtures(t)
	if len(fixtures) < 15 {
		t.Fatalf("loaded %d fixtures, want at least the 15 census samples", len(fixtures))
	}
	d := managerDecoder()
	for _, fx := range fixtures {
		t.Run(fx.kind+"@"+strconv.FormatUint(uint64(fx.ledger), 10)+"/"+fx.txHash[:8], func(t *testing.T) {
			ev := managerEvent(MainnetPositionManager, fx.topics, fx.data)
			ev.Ledger, ev.TxHash = fx.ledger, fx.txHash
			if !d.Matches(ev) {
				t.Fatal("manager event not matched")
			}
			out, err := d.Decode(ev)
			if fx.want == "refuse" {
				if !errors.Is(err, ErrMalformedPayload) || len(out) != 0 {
					t.Fatalf("Decode = %v, %v; want no rows and ErrMalformedPayload", out, err)
				}
				return
			}
			if err != nil || len(out) != 1 {
				t.Fatalf("Decode = %v, %v; want one row", out, err)
			}
			got, ok := out[0].(ManagerEvent)
			if !ok {
				t.Fatalf("Decode emitted %T, want ManagerEvent", out[0])
			}
			if got.Kind != fx.kind || got.Ledger != fx.ledger || got.TxHash != fx.txHash || got.ContractID != MainnetPositionManager {
				t.Fatalf("identity = %s %d %s %s", got.Kind, got.Ledger, got.TxHash, got.ContractID)
			}
			for _, c := range []struct{ name, got, want string }{
				{"token_id", strconv.FormatUint(uint64(got.TokenID), 10), fx.tokenID},
				{"owner", strCol(got.Owner), fx.owner},
				{"recipient", strCol(got.Recipient), fx.recipient},
				{"liquidity", amountCol(got.Liquidity, got.HasLiquidity), fx.liq},
				{"amount0", amountCol(got.Amount0, got.HasAmounts), fx.amount0},
				{"amount1", amountCol(got.Amount1, got.HasAmounts), fx.amount1},
			} {
				if c.got != c.want {
					t.Errorf("%s = %s, want %s", c.name, c.got, c.want)
				}
			}
		})
	}
}

// scMap encodes a Symbol-keyed Map body; values are added in the order given.
func scMap(t *testing.T, kv ...any) string {
	t.Helper()
	var m xdr.ScMap
	for i := 0; i < len(kv); i += 2 {
		m = append(m, xdr.ScMapEntry{Key: scSym(kv[i].(string)), Val: kv[i+1].(xdr.ScVal)})
	}
	mp := &m
	b64, err := xdr.MarshalBase64(xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp})
	if err != nil {
		t.Fatal(err)
	}
	return b64
}

func scSym(s string) xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

func scU128(hi, lo uint64) xdr.ScVal {
	return xdr.ScVal{Type: xdr.ScValTypeScvU128, U128: &xdr.UInt128Parts{Hi: xdr.Uint64(hi), Lo: xdr.Uint64(lo)}}
}

func scU32(v uint32) xdr.ScVal {
	u := xdr.Uint32(v)
	return xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &u}
}

func scI128(hi int64, lo uint64) xdr.ScVal {
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}}
}

// TestManagerDecode_U128ExtremesKeepFullWidth: amounts and liquidity above 2^63 (and at 2^128-1) survive exactly.
func TestManagerDecode_U128ExtremesKeepFullWidth(t *testing.T) {
	maxU128 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	above63 := new(big.Int).Lsh(big.NewInt(1), 63)
	body := scMap(t,
		"amount0", scU128(^uint64(0), ^uint64(0)),
		"amount1", scU128(0, 1<<63),
		"liquidity", scU128(1, 0),
		"token_id", scU32(^uint32(0)),
	)
	out, err := managerDecoder().Decode(managerEvent(MainnetPositionManager, []string{TopicSymbolIncreaseLiquidity}, body))
	if err != nil || len(out) != 1 {
		t.Fatalf("Decode = %v, %v", out, err)
	}
	got := out[0].(ManagerEvent)
	if got.Amount0.BigInt().Cmp(maxU128) != 0 || got.Amount1.BigInt().Cmp(above63) != 0 {
		t.Fatalf("amounts = %s, %s; want %s, %s", got.Amount0, got.Amount1, maxU128, above63)
	}
	if got.Liquidity.String() != "18446744073709551616" || got.TokenID != ^uint32(0) {
		t.Fatalf("liquidity = %s token_id = %d", got.Liquidity, got.TokenID)
	}
}

// TestManagerDecode_RefusesOffShapeBodies: anything but the verified field set and types is a counted error, not a row.
func TestManagerDecode_RefusesOffShapeBodies(t *testing.T) {
	owner := "AAAAEgAAAAAAAAAAlc26trHUlkrHY5wXV1hadvxxhOch+B5n9PXGV9eNiYA="
	full := func(liqKey string, liq xdr.ScVal) string {
		return scMap(t, "amount0", scU128(0, 5), "amount1", scU128(0, 6), liqKey, liq, "token_id", scU32(7))
	}
	cases := []struct {
		name   string
		topics []string
		body   string
	}{
		{
			"increase with an extra field",
			[]string{TopicSymbolIncreaseLiquidity},
			scMap(t, "amount0", scU128(0, 5), "amount1", scU128(0, 6), "liquidity", scU128(0, 1), "token_id", scU32(7), "pool", scU32(1)),
		},
		{
			"increase missing liquidity",
			[]string{TopicSymbolIncreaseLiquidity},
			scMap(t, "amount0", scU128(0, 5), "amount1", scU128(0, 6), "token_id", scU32(7)),
		},
		{"decrease with signed liquidity", []string{TopicSymbolDecreaseLiquidity}, full("liquidity", scI128(0, 1))},
		{"collect with liquidity in place of recipient", []string{TopicSymbolCollect}, full("liquidity", scU128(0, 1))},
		{
			"collect body under increase",
			[]string{TopicSymbolIncreaseLiquidity},
			scMap(t, "amount0", scU128(0, 5), "amount1", scU128(0, 6), "recipient", scU32(1), "token_id", scU32(7)),
		},
		{
			"mint with a u64 token id",
			[]string{TopicSymbolMint, owner},
			scMap(t, "token_id", xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: new(xdr.Uint64)}),
		},
		{"mint carrying the pool's position body", []string{TopicSymbolMint, owner}, goldenMint},
		{"burn whose topic[1] is not an address", []string{TopicSymbolBurn, TopicSymbolBurn}, scMap(t, "token_id", scU32(7))},
		{"body is not a map", []string{TopicSymbolCollect}, "AAAAAwAAAAc="},
	}
	d := managerDecoder()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := d.Decode(managerEvent(MainnetPositionManager, c.topics, c.body))
			if !errors.Is(err, ErrMalformedPayload) || len(out) != 0 {
				t.Fatalf("Decode = %v, %v; want no rows and ErrMalformedPayload", out, err)
			}
		})
	}
}

// TestManagerMatches_ForeignContractRejected: the manager's topics carry no identity; only the manager's own events match.
func TestManagerMatches_ForeignContractRejected(t *testing.T) {
	d := managerDecoder()
	owner := "AAAAEgAAAAAAAAAAlc26trHUlkrHY5wXV1hadvxxhOch+B5n9PXGV9eNiYA="
	for _, fx := range loadManagerFixtures(t) {
		if d.Matches(managerEvent(foreignContract, fx.topics, fx.data)) {
			t.Fatalf("a foreign contract emitting manager %s was claimed", fx.kind)
		}
	}
	if d.Matches(managerEvent(foreignContract, []string{TopicSymbolMint, owner}, scMap(t, "token_id", scU32(1)))) {
		t.Fatal("a foreign two-topic mint was claimed")
	}
}

// TestManagerMatches_PoolAndManagerRolesDoNotCross: same-named topics are told apart by contract role, both ways, even when the
// manager is also in the warmed registry (the curated-set warm puts it there).
func TestManagerMatches_PoolAndManagerRolesDoNotCross(t *testing.T) {
	d := managerDecoder(contractid.WithSeed([]string{MainnetPositionManager}))
	owner := "AAAAEgAAAAAAAAAAlc26trHUlkrHY5wXV1hadvxxhOch+B5n9PXGV9eNiYA="
	inc := scMap(t, "amount0", scU128(0, 5), "amount1", scU128(0, 6), "liquidity", scU128(0, 1), "token_id", scU32(7))

	for name, ev := range map[string]events.Event{
		"pool increase_liquidity":   managerEvent(mainPool, []string{TopicSymbolIncreaseLiquidity}, inc),
		"pool decrease_liquidity":   managerEvent(mainPool, []string{TopicSymbolDecreaseLiquidity}, inc),
		"pool two-topic mint":       managerEvent(mainPool, []string{TopicSymbolMint, owner}, scMap(t, "token_id", scU32(7))),
		"manager one-topic mint":    managerEvent(MainnetPositionManager, []string{TopicSymbolMint}, goldenMint),
		"manager one-topic burn":    managerEvent(MainnetPositionManager, []string{TopicSymbolBurn}, goldenBurn),
		"manager swap":              managerEvent(MainnetPositionManager, []string{TopicSymbolSwap}, goldenSwapSellToken0),
		"manager pool_created":      managerEvent(MainnetPositionManager, []string{TopicSymbolPoolCreated}, goldenPoolCreated),
		"manager init":              managerEvent(MainnetPositionManager, []string{TopicSymbolInit}, goldenMint),
		"manager two-topic collect": managerEvent(MainnetPositionManager, []string{TopicSymbolCollect, owner}, inc),
	} {
		if d.Matches(ev) {
			t.Errorf("%s was claimed", name)
		}
	}

	var collect managerFixture
	for _, fx := range loadManagerFixtures(t) {
		if fx.kind == EventCollect {
			collect = fx
			break
		}
	}
	out, err := d.Decode(managerEvent(MainnetPositionManager, collect.topics, collect.data))
	if err != nil || len(out) != 1 {
		t.Fatalf("manager collect Decode = %v, %v", out, err)
	}
	if _, ok := out[0].(ManagerEvent); !ok {
		t.Fatalf("manager collect decoded as %T, want ManagerEvent", out[0])
	}
}

// TestManagerLifecycleIsRecognisedZeroRows: the manager's upgrade / migration events are gated and project nothing.
func TestManagerLifecycleIsRecognisedZeroRows(t *testing.T) {
	d := managerDecoder()
	for _, topic := range []string{TopicSymbolUpgraded, TopicSymbolMigrated} {
		ev := managerEvent(MainnetPositionManager, []string{topic}, "AAAAAQ==")
		if !d.Matches(ev) {
			t.Fatalf("manager lifecycle %s not matched", topic)
		}
		if out, err := d.Decode(ev); err != nil || len(out) != 0 {
			t.Fatalf("Decode = %v, %v; want zero rows, no error", out, err)
		}
	}
}

// TestManagerGatedContractSet: a configured manager is in the lake prefilter set, once, beside the pools.
func TestManagerGatedContractSet(t *testing.T) {
	set := managerDecoder(contractid.WithSeed([]string{MainnetPositionManager})).GatedContractSet()
	n := 0
	for _, id := range set {
		if id == MainnetPositionManager {
			n++
		}
	}
	if n != 1 || len(set) != len(MainnetGatedSet())+len(MainnetFactories)+1 {
		t.Fatalf("manager appears %d times in a %d-entry set", n, len(set))
	}
}

// TestNewDecoder_ManagerNotLiveUntilAudited pins the shipped state: the manager role is empty, so production ignores it.
func TestNewDecoder_ManagerNotLiveUntilAudited(t *testing.T) {
	fx := loadManagerFixtures(t)[0]
	if NewDecoder().Matches(managerEvent(MainnetPositionManager, fx.topics, fx.data)) {
		t.Fatal("the production decoder matched a manager event before its WASM hashes are audited")
	}
}
