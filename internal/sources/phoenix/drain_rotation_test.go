package phoenix

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

func bufEvent(contract string, idx int) *events.Event {
	return &events.Event{ContractID: contract, TxHash: "tx", Ledger: 7, EventIndex: idx}
}

// A second action through the same contract in the same op must not
// overwrite the first action's filled slot; the open group rotates out.
func TestBuffer_RotatesOpenGroupOnSecondAction(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name  string
		first []string
		again string
		open  func(b *buffer) int
		step  func(b *buffer, e *events.Event, topic string) (int, error)
	}{
		{
			"provide",
			[]string{TopicSymbolPLSender, TopicSymbolPLTokenA},
			TopicSymbolPLSender,
			func(b *buffer) int { return len(b.pl) },
			func(b *buffer, e *events.Event, topic string) (int, error) {
				_, n, err := b.absorbProvideLiquidity(e, topic, now)
				return n, err
			},
		},
		{
			"withdraw",
			[]string{TopicSymbolWLSender, TopicSymbolWLSharesAmount},
			TopicSymbolWLSender,
			func(b *buffer) int { return len(b.wl) },
			func(b *buffer, e *events.Event, topic string) (int, error) {
				_, n, err := b.absorbWithdrawLiquidity(e, topic, now)
				return n, err
			},
		},
		{
			"bond",
			[]string{TopicSymbolStakeUser, TopicSymbolStakeToken},
			TopicSymbolStakeUser,
			func(b *buffer) int { return len(b.bond) },
			func(b *buffer, e *events.Event, topic string) (int, error) {
				_, n, err := b.absorbStake(e, topic, now, true)
				return n, err
			},
		},
		{
			"withdraw_rewards",
			[]string{TopicSymbolWRUser},
			TopicSymbolWRUser,
			func(b *buffer) int { return len(b.withdrawRewards) },
			func(b *buffer, e *events.Event, topic string) (int, error) {
				_, n, err := b.absorbWithdrawRewards(e, topic, now)
				return n, err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuffer()
			for i, topic := range tc.first {
				if n, err := tc.step(b, bufEvent("C", i), topic); err != nil || n != 0 {
					t.Fatalf("first action field %s: evicted=%d err=%v", topic, n, err)
				}
			}
			// Redelivery of an already-held field (same EventIndex) is not a rotation.
			if n, err := tc.step(b, bufEvent("C", 0), tc.first[0]); err != nil || n != 0 {
				t.Fatalf("redelivery: evicted=%d err=%v, want 0", n, err)
			}
			n, err := tc.step(b, bufEvent("C", 10), tc.again)
			if err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Errorf("evicted = %d, want 1 (first action's open group rotated out)", n)
			}
			if got := tc.open(b); got != 1 {
				t.Errorf("open groups = %d, want 1 (fresh generation)", got)
			}
		})
	}
}

// A late optional auto-unbonded event after the withdraw completed must not
// open an empty group that later ages out as a false orphan.
func TestBuffer_LateAutoUnbondedOpensNoGroup(t *testing.T) {
	b := newBuffer()
	now := time.Unix(1_700_000_000, 0)
	for i, topic := range []string{TopicSymbolWLSender, TopicSymbolWLSharesAmount, TopicSymbolWLReturnAmountA, TopicSymbolWLReturnAmountB} {
		done, _, err := b.absorbWithdrawLiquidity(bufEvent("C", i), topic, now)
		if err != nil {
			t.Fatal(err)
		}
		if (done != nil) != (i == 3) {
			t.Fatalf("field %d: completed=%v", i, done != nil)
		}
	}
	if _, _, err := b.absorbWithdrawLiquidity(bufEvent("C", 4), TopicSymbolWLAutoUnbonded, now); err != nil {
		t.Fatal(err)
	}
	if len(b.wl) != 0 {
		t.Errorf("open withdraw groups = %d, want 0", len(b.wl))
	}
}

// Drain at the end of a bounded stream emits the pre-upgrade 7-field swap
// that no later event is left to sweep, counts the rest as orphans, and
// leaves nothing buffered.
func TestDecoder_Drain_rescuesOpenSwapAndCountsOrphans(t *testing.T) {
	d := newTestDecoder()
	sender, sell, buy := makeC(t, 0x10), makeC(t, 0x20), makeC(t, 0x30)
	zero := i128Body(t, big.NewInt(0))
	fields := []struct{ topic, body string }{
		{TopicSymbolSender, addrBody(t, sender)},
		{TopicSymbolSellToken, addrBody(t, sell)},
		{TopicSymbolOfferAmount, i128Body(t, big.NewInt(1_000_000))},
		{TopicSymbolBuyToken, addrBody(t, buy)},
		{TopicSymbolReturnAmount, i128Body(t, big.NewInt(2_000_000))},
		{TopicSymbolSpreadAmount, zero},
		{TopicSymbolReferralFee, zero},
	}
	for _, f := range fields {
		if _, err := d.Decode(makeFieldEventAt(t, f.topic, f.body, "tx-7field", "2026-04-23T12:00:00Z")); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range fields[:2] {
		if _, err := d.Decode(makeFieldEventAt(t, f.topic, f.body, "tx-broken", "2026-04-23T12:00:01Z")); err != nil {
			t.Fatal(err)
		}
	}
	out := d.Drain()
	if len(out) != 1 {
		t.Fatalf("Drain emitted %d events, want 1 (the 7-field swap)", len(out))
	}
	if _, ok := out[0].(TradeEvent); !ok {
		t.Fatalf("drained event is %T, want TradeEvent", out[0])
	}
	if got := d.EvictedOrphans(); got != 1 {
		t.Errorf("EvictedOrphans = %d, want 1 (the 2-field group)", got)
	}
	if again := d.Drain(); len(again) != 0 || d.buf.size() != 0 {
		t.Errorf("second Drain emitted %d, buffered %d; want empty", len(again), d.buf.size())
	}
}
