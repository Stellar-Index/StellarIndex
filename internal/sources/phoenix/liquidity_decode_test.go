package phoenix

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

const (
	plPool    = "CDPL000000000000000000000000000000000000000000000000A"
	wlPool    = "CDWL000000000000000000000000000000000000000000000000B"
	stakeC    = "CDSTAKE000000000000000000000000000000000000000000000C"
	plTokenA  = "CDTKNA000000000000000000000000000000000000000000000D"
	plTokenB  = "CDTKNB000000000000000000000000000000000000000000000E"
	plSender  = "GPLSENDER0000000000000000000000000000000000000000000F"
	plTxHash  = "feedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfee0"
	wlTxHash  = "feedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfee1"
	bondTx    = "feedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfee2"
	unbondTx  = "feedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfee3"
	lpTokenC  = "CDLPTKN0000000000000000000000000000000000000000000000G"
	stakeUser = "GPLBONDR0000000000000000000000000000000000000000000000H"
)

// ─── classifyAny ─────────────────────────────────────────────────

// installAddressI128Fakes swaps the SDK decoders for deterministic
// fakes — same pattern as TestDecodeSwap_happyPath. The fakes
// interpret a body string as either an address tag ("addr:<C>") or
// an i128 decimal ("i128:<n>"), so test data is human-readable in
// the test source.
func installAddressI128Fakes(t *testing.T) (restore func()) {
	t.Helper()
	prevAddr, prevAsset, prevI128 := decodeAddress, decodeAsset, decodeI128
	decodeAddress = func(v string) (string, error) {
		if len(v) > 5 && v[:5] == "addr:" {
			return v[5:], nil
		}
		t.Fatalf("fake decodeAddress: unexpected body %q", v)
		return "", nil
	}
	decodeI128 = func(v string) (canonical.Amount, error) {
		if len(v) > 5 && v[:5] == "i128:" {
			n := new(big.Int)
			if _, ok := n.SetString(v[5:], 10); !ok {
				t.Fatalf("fake decodeI128: bad number %q", v)
			}
			return canonical.NewAmount(n), nil
		}
		t.Fatalf("fake decodeI128: unexpected body %q", v)
		return canonical.NewAmount(big.NewInt(0)), nil
	}
	decodeAsset = prevAsset // unused for liquidity / stake paths
	return func() {
		decodeAddress, decodeAsset, decodeI128 = prevAddr, prevAsset, prevI128
	}
}

func plClosedAt() time.Time { return time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC) }

func plField(topic1, value, txHash string) events.Event {
	return events.Event{
		Topic:          []string{TopicSymbolProvideLiquidity, topic1},
		Value:          value,
		Ledger:         62_500_000,
		TxHash:         txHash,
		OperationIndex: 0,
		LedgerClosedAt: plClosedAt().Format(time.RFC3339),
		ContractID:     plPool,
	}
}

func wlField(topic1, value, txHash string) events.Event {
	return events.Event{
		Topic:          []string{TopicSymbolWithdrawLiquidity, topic1},
		Value:          value,
		Ledger:         62_500_000,
		TxHash:         txHash,
		OperationIndex: 0,
		LedgerClosedAt: plClosedAt().Format(time.RFC3339),
		ContractID:     wlPool,
	}
}

func bondField(topic1, value, txHash string) events.Event {
	return events.Event{
		Topic:          []string{TopicSymbolBond, topic1},
		Value:          value,
		Ledger:         62_500_001,
		TxHash:         txHash,
		OperationIndex: 0,
		LedgerClosedAt: plClosedAt().Format(time.RFC3339),
		ContractID:     stakeC,
	}
}

func unbondField(topic1, value, txHash string) events.Event {
	return events.Event{
		Topic:          []string{TopicSymbolUnbond, topic1},
		Value:          value,
		Ledger:         62_500_002,
		TxHash:         txHash,
		OperationIndex: 0,
		LedgerClosedAt: plClosedAt().Format(time.RFC3339),
		ContractID:     stakeC,
	}
}

// ─── provide_liquidity completes on 5th field ─────────────────────

func TestLiquidityEvent_implementsConsumerEvent(t *testing.T) {
	le := LiquidityEvent{}
	if le.EventKind() != "phoenix.liquidity" {
		t.Errorf("EventKind() = %q", le.EventKind())
	}
	if le.Source() != SourceName {
		t.Errorf("Source() = %q", le.Source())
	}
	var _ consumer.Event = le
}

func TestStakeEvent_implementsConsumerEvent(t *testing.T) {
	se := StakeEvent{}
	if se.EventKind() != "phoenix.stake" {
		t.Errorf("EventKind() = %q", se.EventKind())
	}
	if se.Source() != SourceName {
		t.Errorf("Source() = %q", se.Source())
	}
	var _ consumer.Event = se
}

// ─── Incompleteness / orphan guards ─────────────────────────────

func TestDecodeProvideLiquidity_incomplete(t *testing.T) {
	r := &RawProvideLiquidity{Sender: &events.Event{}}
	if _, err := decodeProvideLiquidity(r); err == nil {
		t.Fatal("expected ErrIncompleteLiquidity")
	}
}

func TestDecodeWithdrawLiquidity_incomplete(t *testing.T) {
	r := &RawWithdrawLiquidity{Sender: &events.Event{}}
	if _, err := decodeWithdrawLiquidity(r); err == nil {
		t.Fatal("expected ErrIncompleteLiquidity")
	}
}

func TestDecodeStake_incomplete(t *testing.T) {
	r := &RawStake{User: &events.Event{}}
	if _, err := decodeStake(r); err == nil {
		t.Fatal("expected ErrIncompleteStake")
	}
}
