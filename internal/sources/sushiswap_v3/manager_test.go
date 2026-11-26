package sushiswap_v3

import (
	"bufio"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"slices"
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
	if len(fixtures) < 23 {
		t.Fatalf("loaded %d fixtures, want at least the 15 post- and 8 pre-upgrade samples", len(fixtures))
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

// TestManagerWasmFixtures decodes every per-WASM-hash capture under test/fixtures/sushiswap_v3/<hash>/: each must be
// byte-identical to a testdata row (whose exact columns TestManagerFixtures asserts) and decode to one ManagerEvent.
func TestManagerWasmFixtures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "test", "fixtures", "sushiswap_v3", "*", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no per-hash fixtures: %v", err)
	}
	rows := loadManagerFixtures(t)
	d := managerDecoder()
	hashes := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var capture struct {
			ContractID string   `json:"contract_id"`
			WasmHash   string   `json:"wasm_hash"`
			Ledger     uint32   `json:"ledger"`
			TxHash     string   `json:"tx_hash"`
			Topics     []string `json:"topics"`
			Value      string   `json:"value"`
			EventName  string   `json:"event_name"`
		}
		if err := json.Unmarshal(raw, &capture); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if filepath.Base(filepath.Dir(f)) != capture.WasmHash || capture.ContractID != MainnetPositionManager {
			t.Fatalf("%s: wasm_hash %s / contract %s do not match its directory and the manager", f, capture.WasmHash, capture.ContractID)
		}
		matched := slices.ContainsFunc(rows, func(r managerFixture) bool {
			return r.want == "ok" && r.kind == capture.EventName && r.ledger == capture.Ledger && r.txHash == capture.TxHash &&
				r.data == capture.Value && slices.Equal(r.topics, capture.Topics)
		})
		if !matched {
			t.Errorf("%s: no byte-identical ok row in testdata/manager_events.txt", f)
			continue
		}
		ev := managerEvent(capture.ContractID, capture.Topics, capture.Value)
		ev.Ledger, ev.TxHash = capture.Ledger, capture.TxHash
		out, err := d.Decode(ev)
		if err != nil || len(out) != 1 {
			t.Fatalf("%s: Decode = %v, %v", f, out, err)
		}
		if got := out[0].(ManagerEvent); got.Kind != capture.EventName {
			t.Errorf("%s: decoded %s", f, got.Kind)
		}
		hashes[capture.WasmHash] = true
	}
	for _, h := range []string{
		"dae1af1c2863e907e6b58dc861bb7ae610c9b951d23af7902a6afc247b923b0a",
		"be969de545da89a04508e4952e94a53a77e76ecfa67d74b23fd56e0be42290d4",
	} {
		if !hashes[h] {
			t.Errorf("no captured manager event for attested hash %s", h)
		}
	}
}

// TestManagerAmounts_MatchThePoolSide anchors the manager's u128 scale on an independent event: the pre-upgrade
// decrease_liquidity at ledger 61,487,425 drove the pool's burn there (goldenBurn), whose owner is the manager.
func TestManagerAmounts_MatchThePoolSide(t *testing.T) {
	var dec managerFixture
	for _, fx := range loadManagerFixtures(t) {
		if fx.kind == EventDecreaseLiquidity && fx.ledger == 61_487_425 {
			dec = fx
		}
	}
	out, err := managerDecoder().Decode(managerEvent(MainnetPositionManager, dec.topics, dec.data))
	if err != nil || len(out) != 1 {
		t.Fatalf("manager decrease Decode = %v, %v", out, err)
	}
	m := out[0].(ManagerEvent)
	pool, err := decodePositionFields(EventBurn, goldenBurn)
	if err != nil {
		t.Fatal(err)
	}
	if pool.Owner != MainnetPositionManager {
		t.Fatalf("pool burn owner = %s, want the manager", pool.Owner)
	}
	for _, c := range []struct{ name, manager, pool string }{
		{"liquidity", m.Liquidity.String(), pool.Liquidity.String()},
		{"amount0", m.Amount0.String(), pool.Amount0.String()},
		{"amount1", m.Amount1.String(), pool.Amount1.String()},
	} {
		if c.manager != c.pool {
			t.Errorf("%s: manager %s, pool %s", c.name, c.manager, c.pool)
		}
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

// TestNewDecoder_ManagerIsLive: the production decoder gates the manager in, decodes its events and prefilters on it.
func TestNewDecoder_ManagerIsLive(t *testing.T) {
	d := NewDecoder()
	fx := loadManagerFixtures(t)[0]
	ev := managerEvent(MainnetPositionManager, fx.topics, fx.data)
	if !d.Matches(ev) {
		t.Fatal("the production decoder does not match a position-manager event")
	}
	if out, err := d.Decode(ev); err != nil || len(out) != 1 {
		t.Fatalf("Decode = %v, %v; want one ManagerEvent", out, err)
	}
	if !slices.Contains(d.GatedContractSet(), MainnetPositionManager) {
		t.Fatal("the manager is missing from the lake prefilter set")
	}
}
