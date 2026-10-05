package phoenix

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// Real lake rows; see test/fixtures/phoenix/README.md "event-shapes/".
const eventShapesFixture = "lake_events.jsonl"

// nonPhoenixBond emits only Symbol-topic ("bond", …) events and must not
// be attributed to phoenix.
const nonPhoenixBond = "CBBUVHCEML7UE46XXZXLTMGKFMKX7KOC2XAKI3TW6WBQBKWMSARMU3YM"

type shapeRow struct {
	LedgerSeq  uint32   `json:"ledger_seq"`
	CloseTime  string   `json:"close_time"`
	TxHash     string   `json:"tx_hash"`
	OpIndex    int      `json:"op_index"`
	EventIndex int      `json:"event_index"`
	ContractID string   `json:"contract_id"`
	TopicsXDR  []string `json:"topics_xdr"`
	DataXDR    string   `json:"data_xdr"`
}

func loadShapeEvents(t *testing.T) []events.Event {
	t.Helper()
	path := filepath.Join("..", "..", "..", "test", "fixtures", "phoenix", "event-shapes", eventShapesFixture)
	f, err := os.Open(path) //nolint:gosec // repo-relative, test-only
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()
	var out []events.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var r shapeRow
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("fixture row: %v", err)
		}
		closedAt := ""
		if r.CloseTime != "" {
			closedAt = strings.Replace(r.CloseTime, " ", "T", 1) + "Z"
		}
		out = append(out, events.Event{
			Type:                     "contract",
			Ledger:                   r.LedgerSeq,
			LedgerClosedAt:           closedAt,
			ContractID:               r.ContractID,
			TxHash:                   r.TxHash,
			OperationIndex:           r.OpIndex,
			EventIndex:               r.EventIndex,
			InSuccessfulContractCall: true,
			Topic:                    r.TopicsXDR,
			Value:                    r.DataXDR,
		})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("fixture holds no rows")
	}
	return out
}

func shapeEvent(t *testing.T, evs []events.Event, ledger uint32, eventIndex int) events.Event {
	t.Helper()
	for _, ev := range evs {
		if ev.Ledger == ledger && ev.EventIndex == eventIndex {
			return ev
		}
	}
	t.Fatalf("fixture has no event at ledger %d index %d", ledger, eventIndex)
	return events.Event{}
}

func decodeOK(t *testing.T, d *Decoder, ev events.Event) []consumer.Event {
	t.Helper()
	if !d.Matches(ev) {
		t.Fatalf("ledger %d index %d: Matches = false", ev.Ledger, ev.EventIndex)
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("ledger %d index %d: Decode: %v", ev.Ledger, ev.EventIndex, err)
	}
	return out
}

func onlyStake(t *testing.T, out []consumer.Event) StakeChange {
	t.Helper()
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	se, ok := out[0].(StakeEvent)
	if !ok {
		t.Fatalf("got %T, want StakeEvent", out[0])
	}
	return se.Change
}

func onlyAdmin(t *testing.T, out []consumer.Event) AdminEvent {
	t.Helper()
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	ae, ok := out[0].(AdminEvent)
	if !ok {
		t.Fatalf("got %T, want AdminEvent", out[0])
	}
	return ae
}

func onlyLiquidity(t *testing.T, out []consumer.Event) LiquidityChange {
	t.Helper()
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	le, ok := out[0].(LiquidityEvent)
	if !ok {
		t.Fatalf("got %T, want LiquidityEvent", out[0])
	}
	return le.Change
}

// Every real row of a Phoenix contract is recognised by the ADR-0033
// oracle; the non-Phoenix bond contract's rows are not.
func TestEventShapes_RecognizedByDispatcher(t *testing.T) {
	disp := dispatcher.New(NewDecoder())
	for _, ev := range loadShapeEvents(t) {
		name, ok := disp.Recognize(ev)
		if ev.ContractID == nonPhoenixBond {
			if ok {
				t.Errorf("ledger %d: %s recognised as %q, want unrecognised", ev.Ledger, nonPhoenixBond, name)
			}
			continue
		}
		if !ok || name != SourceName {
			t.Errorf("ledger %d index %d (%s): Recognize = (%q, %v), want (%q, true)",
				ev.Ledger, ev.EventIndex, ev.ContractID, name, ok, SourceName)
		}
	}
}

func TestMainnetGatedSet_ExcludesNonPhoenixBond(t *testing.T) {
	if slices.Contains(MainnetGatedSet(), nonPhoenixBond) {
		t.Errorf("MainnetGatedSet contains %s", nonPhoenixBond)
	}
}

func TestEventShapes_CreateDistributionFlow(t *testing.T) {
	evs := loadShapeEvents(t)
	c := onlyStake(t, decodeOK(t, NewDecoder(), shapeEvent(t, evs, 53_329_394, 0)))
	if c.Action != EventActionCreateDistributionFlow || c.User != "" ||
		c.LPToken != "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32" ||
		c.Contract != "CABWEFVXUB3XWYPTWFETEGJR2WRGE2ZKYYLZDLV3EBUVFMOU4ENK4DJC" {
		t.Errorf("create_distribution_flow = %+v", c)
	}
}

func TestEventShapes_StakeMigration(t *testing.T) {
	evs := loadShapeEvents(t)
	d := NewDecoder()
	const user = "GDDHB3AWOIZ6ZZ4T3HBLV2UHCKM5TYDVRDO76BEJRKXBKD4S7DH342Z3"
	for i, want := range []string{StakeActionMigrationStarted, StakeActionMigrationQueried, StakeActionMigrationCompleted} {
		c := onlyStake(t, decodeOK(t, d, shapeEvent(t, evs, 53_586_195, i)))
		if c.Action != want || c.User != user || c.LPToken != "" || c.EventIndex != i {
			t.Errorf("event %d = %+v, want action %s user %s", i, c, want, user)
		}
	}
}

func TestEventShapes_FactoryConfigUpdated(t *testing.T) {
	evs := loadShapeEvents(t)
	d := NewDecoder()
	before := len(d.GatedContractSet())
	for _, ledger := range []uint32{63_293_663, 64_028_582} {
		ae := onlyAdmin(t, decodeOK(t, d, shapeEvent(t, evs, ledger, 0)))
		if ae.AdminAction != AdminActionFactoryConfigUpdated || ae.Pool != MainnetFactory || ae.Admin != "" {
			t.Errorf("ledger %d = %+v", ledger, ae)
		}
	}
	if after := len(d.GatedContractSet()); after != before {
		t.Errorf("config update changed the gated set: %d → %d", before, after)
	}
}

func TestEventShapes_BlendPoolSettings(t *testing.T) {
	evs := loadShapeEvents(t)
	d := NewDecoder()
	ae := onlyAdmin(t, decodeOK(t, d, shapeEvent(t, evs, 63_294_983, 0)))
	if ae.AdminAction != AdminActionBlendSetDelegate ||
		ae.Admin != "CDX6THURLR7KNL5PCJXLPHPI3ZD6NEY6AGNFTMYYDW4A6TQ2X724G3QZ" {
		t.Errorf("set_delegate = %+v", ae)
	}
	for i, want := range []struct{ action, value string }{
		{AdminActionBlendSetMinTradingA, "0"},
		{AdminActionBlendSetMinTradingB, "5000000000"},
	} {
		ae := onlyAdmin(t, decodeOK(t, d, shapeEvent(t, evs, 63_295_049, i)))
		if ae.AdminAction != want.action || ae.Value.String() != want.value || ae.Admin != "" {
			t.Errorf("event %d = %+v (value %s), want %s %s", i, ae, ae.Value.String(), want.action, want.value)
		}
	}
}

// toggle_trading on the factory-created pool CCPPPTDW… (WASM d54d01e0…),
// real lake bytes: ledger 64,030,690, tx 5de41f39…, op 0, event 0, body
// Bool(false). Recognised, zero rows; a non-Bool body is malformed.
func TestEventShapes_ToggleTradingRecognized(t *testing.T) {
	const pool = "CCPPPTDWJIWXQUQ2CN64S5JYQ7GYWVZIT7YWUUTH75HKIZX53Z2CE3XI"
	ev := events.Event{
		Type:                     "contract",
		Ledger:                   64_030_690,
		LedgerClosedAt:           "2026-08-19T19:37:49Z",
		ContractID:               pool,
		TxHash:                   "5de41f395bcacc6658b420678b8bc4478c895a7d6f1fbd8fe84c19835781bc6f",
		InSuccessfulContractCall: true,
		Topic:                    []string{"AAAADgAAAA50b2dnbGVfdHJhZGluZwAA", "AAAADgAAAAdlbmFibGVkAA=="},
		Value:                    "AAAAAAAAAAA=",
	}
	d := NewDecoder(contractid.WithSeed([]string{pool}))
	if name, ok := dispatcher.New(d).Recognize(ev); !ok || name != SourceName {
		t.Fatalf("Recognize = (%q, %v), want (%q, true)", name, ok, SourceName)
	}
	if out := decodeOK(t, d, ev); len(out) != 0 {
		t.Errorf("Decode emitted %d events, want 0", len(out))
	}
	if NewDecoder().Matches(ev) {
		t.Error("Matches = true for an unregistered emitter")
	}
	ev.Value = shapeEvent(t, loadShapeEvents(t), 63_295_049, 1).Value
	if _, err := d.Decode(ev); !errors.Is(err, ErrMalformedPayload) {
		t.Errorf("i128 body: err = %v, want ErrMalformedPayload", err)
	}
}

func TestEventShapes_MapBodyLiquidity(t *testing.T) {
	evs := loadShapeEvents(t)
	d := NewDecoder()
	const sender = "GDXWIY7YU776ETADATRWVRBMXIIQQ2DAMSYE7BRVLXJE6CM55HXMRD6A"

	p := onlyLiquidity(t, decodeOK(t, d, shapeEvent(t, evs, 63_295_145, 4)))
	if p.Action != EventActionProvideLiquidity || p.Sender != sender ||
		p.TokenA != "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA" ||
		p.TokenB != "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75" ||
		p.AmountA.String() != "27300000000" || p.AmountB.String() != "5439074397" ||
		p.EventIndex != 4 || p.Pool != MainnetMapPools[0] {
		t.Errorf("provide_liquidity = %+v", p)
	}

	w := onlyLiquidity(t, decodeOK(t, d, shapeEvent(t, evs, 63_295_946, 4)))
	if w.Action != EventActionWithdrawLiquidity || w.Sender != sender ||
		w.SharesAmount.String() != "500000000" ||
		w.AmountA.String() != "1120182615" || w.AmountB.String() != "223177896" ||
		w.TokenA != "" || w.TokenB != "" || w.EventIndex != 4 {
		t.Errorf("withdraw_liquidity = %+v", w)
	}
}

// The earliest stake WASMs publish unbond as ("unbond","user") followed by
// ("bond","token") and ("bond","amount"); the three must reassemble into
// one unbond, not two orphaned groups.
func TestEventShapes_EarlyUnbondReassembles(t *testing.T) {
	evs := loadShapeEvents(t)
	d := NewDecoder()
	var got []consumer.Event
	for i := 1; i <= 3; i++ {
		got = append(got, decodeOK(t, d, shapeEvent(t, evs, 51_575_198, i))...)
	}
	c := onlyStake(t, got)
	if c.Action != EventActionUnbond ||
		c.User != "GC4WGKSMDHYDDMBWMWY6UB5F6ILGADT7LCFIJ7XWCSW5SPANE7TQURVH" ||
		c.LPToken != "CBTCSVZBJFGMW7E2LKFKRIUARUKZK2DTBUC7X5QPQJLXMAH42DB3ALE5" ||
		c.Amount.String() != "30455774762" || c.EventIndex != 1 {
		t.Errorf("early unbond = %+v (amount %s)", c, c.Amount.String())
	}
	if n := len(d.buf.bond) + len(d.buf.unbond); n != 0 {
		t.Errorf("%d stake groups left open", n)
	}

	// The current shape keeps all three fields under "unbond".
	d = NewDecoder()
	got = nil
	for i := 7; i <= 9; i++ {
		got = append(got, decodeOK(t, d, shapeEvent(t, evs, 53_344_280, i))...)
	}
	if c := onlyStake(t, got); c.Action != EventActionUnbond || c.Amount.String() != "252514322288" || c.EventIndex != 7 {
		t.Errorf("current unbond = %+v", c)
	}
}

// withOp copies ev into another op, optionally retitling topic[0].
func withOp(ev events.Event, op int, topic0 string) events.Event {
	ev.OperationIndex = op
	ev.Topic = slices.Clone(ev.Topic)
	if topic0 != "" {
		ev.Topic[0] = topic0
	}
	return ev
}

// A real bond, in its own op or opened in the same op after an early
// unbond's user field, keeps its own fields.
func TestEventShapes_EarlyUnbondBesideBond(t *testing.T) {
	evs := loadShapeEvents(t)
	user := shapeEvent(t, evs, 51_575_198, 1)
	token := shapeEvent(t, evs, 51_575_198, 2)
	amount := shapeEvent(t, evs, 51_575_198, 3)
	bondUser := withOp(user, 0, TopicSymbolBond)

	t.Run("separate ops interleaved", func(t *testing.T) {
		d := NewDecoder()
		var got []consumer.Event
		for _, ev := range []events.Event{
			user, withOp(bondUser, 1, ""), token, withOp(token, 1, ""), amount, withOp(amount, 1, ""),
		} {
			got = append(got, decodeOK(t, d, ev)...)
		}
		if len(got) != 2 {
			t.Fatalf("got %d events, want 2", len(got))
		}
		byOp := map[int]string{}
		for _, e := range got {
			c := e.(StakeEvent).Change
			byOp[c.OpIndex] = c.Action
		}
		if byOp[0] != EventActionUnbond || byOp[1] != EventActionBond {
			t.Errorf("actions by op = %v, want op0 unbond, op1 bond", byOp)
		}
	})

	t.Run("bond opened in the same op", func(t *testing.T) {
		d := NewDecoder()
		var got []consumer.Event
		for _, ev := range []events.Event{user, bondUser, token, amount} {
			got = append(got, decodeOK(t, d, ev)...)
		}
		if c := onlyStake(t, got); c.Action != EventActionBond {
			t.Errorf("got %s, want bond", c.Action)
		}
		if len(d.buf.unbond) != 1 {
			t.Errorf("the unbond without token/amount must stay open, got %d open", len(d.buf.unbond))
		}
	})
}

// Topics alone are forgeable: each new shape is attributed only on the
// emitter its trust root allows, and only for its audited topic pairs.
func TestEventShapes_GatedOnEmitterAndTopicPair(t *testing.T) {
	evs := loadShapeEvents(t)
	d := NewDecoder()
	factoryCfg := shapeEvent(t, evs, 63_293_663, 0)
	blend := shapeEvent(t, evs, 63_294_983, 0)
	flow := shapeEvent(t, evs, 53_329_394, 0)
	migration := shapeEvent(t, evs, 53_586_195, 0)

	fromPool := factoryCfg
	fromPool.ContractID = MainnetMapPools[0]
	fromStranger := func(ev events.Event) events.Event { ev.ContractID = nonPhoenixBond; return ev }
	retopic := func(ev events.Event, t1 string) events.Event {
		ev.Topic = []string{ev.Topic[0], scval.MustEncodeString(t1)}
		return ev
	}
	for name, ev := range map[string]events.Event{
		"factory config from a gated pool":     fromPool,
		"blend_pool from a foreign contract":   fromStranger(blend),
		"distribution flow from a foreign one": fromStranger(flow),
		"migration from a foreign contract":    fromStranger(migration),
		"blend_pool unaudited setting":         retopic(blend, "set_fee"),
		"migration with the completed phrase":  retopic(migration, "Migration for user completed and stored: "),
		"create_distribution_flow other field": retopic(flow, "user"),
		"factory with another phrase":          retopic(factoryCfg, "Updated Admin"),
	} {
		if d.Matches(ev) {
			t.Errorf("%s: Matches = true", name)
		}
	}
}

func TestEventShapes_MalformedBodiesError(t *testing.T) {
	evs := loadShapeEvents(t)
	d := NewDecoder()
	addrBody := shapeEvent(t, evs, 63_294_983, 0).Value
	i128Body := shapeEvent(t, evs, 63_295_049, 1).Value

	minTrading := shapeEvent(t, evs, 63_295_049, 0)
	minTrading.Value = addrBody
	delegate := shapeEvent(t, evs, 63_294_983, 0)
	delegate.Value = i128Body
	migration := shapeEvent(t, evs, 53_586_195, 0)
	migration.Value = i128Body
	cfg := shapeEvent(t, evs, 63_293_663, 0)
	cfg.Value = addrBody
	provide := shapeEvent(t, evs, 63_295_145, 4)
	provide.Value = addrBody

	for name, ev := range map[string]events.Event{
		"min_trading with an address": minTrading,
		"set_delegate with an i128":   delegate,
		"migration with an i128":      migration,
		"factory config with a body":  cfg,
		"map provide with an address": provide,
	} {
		if _, err := d.Decode(ev); err == nil {
			t.Errorf("%s: Decode err = nil", name)
		}
	}
	if _, err := d.Decode(cfg); !errors.Is(err, ErrMalformedPayload) {
		t.Errorf("factory config err = %v, want ErrMalformedPayload", err)
	}
}
