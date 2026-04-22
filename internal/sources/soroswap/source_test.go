package soroswap

import (
	"math/big"
	"testing"
	"time"

	"github.com/RatesEngine/rates-engine/internal/canonical"
	"github.com/RatesEngine/rates-engine/internal/stellarrpc"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		topic0 string
		want   string
	}{
		{TopicSymbolSwap, EventSwap},
		{TopicSymbolSync, EventSync},
		{TopicSymbolDeposit, EventDeposit},
		{TopicSymbolWithdraw, EventWithdraw},
		{TopicSymbolNewPair, EventNewPair},
		{"AAAAANotAThing", ""},
	}
	for _, tc := range cases {
		e := &stellarrpc.Event{Topic: []string{tc.topic0}}
		if got := classify(e); got != tc.want {
			t.Errorf("classify(%q) = %q, want %q", tc.topic0, got, tc.want)
		}
	}

	// Empty topic list.
	if got := classify(&stellarrpc.Event{}); got != "" {
		t.Errorf("empty topic: got %q", got)
	}
}

func TestBufferAbsorbsAndPairs(t *testing.T) {
	buf := newBuffer()
	pair := "CABC...XYZ"
	base := event(EventSwap, pair, 100, "tx1", 0, TopicSymbolSwap)
	companion := event(EventSync, pair, 100, "tx1", 0, TopicSymbolSync)

	// Swap first → nothing complete.
	if got := buf.absorb(base, EventSwap); len(got) != 0 {
		t.Fatalf("absorb(swap): expected 0 completes, got %d", len(got))
	}

	// Sync completes the pair.
	completed := buf.absorb(companion, EventSync)
	if len(completed) != 1 {
		t.Fatalf("absorb(sync): expected 1 complete, got %d", len(completed))
	}
	if !completed[0].Complete() {
		t.Fatal("returned pair reports itself incomplete")
	}
	if len(buf.m) != 0 {
		t.Fatalf("buffer should be empty after pairing, has %d", len(buf.m))
	}
}

func TestBufferOrphanSyncStaysBuffered(t *testing.T) {
	buf := newBuffer()
	// A sync-only event (no preceding swap in the same group) is
	// held in the buffer as Sync-set-but-Swap-nil. It's NOT emitted
	// as a complete pair; orphans() surfaces it.
	completed := buf.absorb(event(EventSync, "CXYZ...", 42, "tx2", 0, TopicSymbolSync), EventSync)
	if len(completed) != 0 {
		t.Fatalf("orphan sync should not complete; got %d", len(completed))
	}
	orphans := buf.orphans()
	if len(orphans) != 1 {
		t.Fatalf("expected 1 orphan, got %d", len(orphans))
	}
	if orphans[0].Complete() {
		t.Fatal("orphan should not report Complete")
	}
}

func TestBufferSeparatesByGroupKey(t *testing.T) {
	// Two distinct (ledger, tx_hash, op_index) triples should not
	// collide. This guards against the naive "key on contract_id
	// only" bug.
	buf := newBuffer()
	swapA := event(EventSwap, "CAAA", 1, "txA", 0, TopicSymbolSwap)
	swapB := event(EventSwap, "CBBB", 1, "txB", 0, TopicSymbolSwap)
	syncA := event(EventSync, "CAAA", 1, "txA", 0, TopicSymbolSync)

	buf.absorb(swapA, EventSwap)
	buf.absorb(swapB, EventSwap)
	completed := buf.absorb(syncA, EventSync)
	if len(completed) != 1 {
		t.Fatalf("expected only A to complete, got %d", len(completed))
	}
	if completed[0].TxHash != "txA" {
		t.Fatalf("wrong pair completed: %s", completed[0].TxHash)
	}
	// B still buffered.
	if len(buf.m) != 1 {
		t.Fatalf("B should still be buffered, buffer len %d", len(buf.m))
	}
}

func TestDecodeSwap_withFakeDecoder(t *testing.T) {
	// Install a fake decoder that yields a deterministic trade for
	// the swap's Value blob. This exercises the direction-
	// determination branch without the real XDR codec.
	prev1, prev2 := decodeSwapAmounts, decodeSwapOutAmounts
	defer func() { decodeSwapAmounts, decodeSwapOutAmounts = prev1, prev2 }()

	decodeSwapAmounts = func(_ string) (canonical.Amount, canonical.Amount, error) {
		return canonical.NewAmount(big.NewInt(100)), canonical.NewAmount(big.NewInt(0)), nil
	}
	decodeSwapOutAmounts = func(_ string) (canonical.Amount, canonical.Amount, error) {
		return canonical.NewAmount(big.NewInt(0)), canonical.NewAmount(big.NewInt(42)), nil
	}

	xlm := canonical.NativeAsset()
	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}

	r := RawPair{
		Ledger: 100, TxHash: "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe", OpIndex: 0,
		ClosedAt: time.Now().UTC().Truncate(time.Second),
		Swap:     &stellarrpc.Event{Value: "stub"},
		Sync:     &stellarrpc.Event{Value: "stub"},
	}

	trade, err := decodeSwap(r, xlm, usdc)
	if err != nil {
		t.Fatalf("decodeSwap: %v", err)
	}
	if trade.Source != SourceName {
		t.Errorf("source = %q", trade.Source)
	}
	// amount0_in=100 > 0 and amount1_out=42 > 0 → base=token0 (xlm), quote=token1 (usdc).
	if !trade.Pair.Base.Equal(xlm) || !trade.Pair.Quote.Equal(usdc) {
		t.Errorf("pair direction wrong: %+v", trade.Pair)
	}
	if trade.BaseAmount.Cmp(canonical.NewAmount(big.NewInt(100))) != 0 {
		t.Errorf("base amount = %s", trade.BaseAmount)
	}
	if trade.QuoteAmount.Cmp(canonical.NewAmount(big.NewInt(42))) != 0 {
		t.Errorf("quote amount = %s", trade.QuoteAmount)
	}
}

func TestDecodeSwap_incompleteErrors(t *testing.T) {
	_, err := decodeSwap(RawPair{Swap: &stellarrpc.Event{}, Sync: nil}, canonical.NativeAsset(), canonical.NativeAsset())
	if err == nil {
		t.Fatal("expected error for incomplete pair")
	}
}

func TestSource_NameAndNewBasics(t *testing.T) {
	s := New(nil, WithPollInterval(500*time.Millisecond))
	if s.Name() != SourceName {
		t.Errorf("Name = %q, want %q", s.Name(), SourceName)
	}
	if s.pollInterval != 500*time.Millisecond {
		t.Errorf("pollInterval = %v", s.pollInterval)
	}
	// Health starts zero.
	if h := s.Health(); h.Connected || !h.LastEvent.IsZero() || h.LagLedgers != 0 {
		t.Errorf("initial health: %+v", h)
	}
}

// helpers

func event(kind string, contract string, ledger uint32, tx string, op int, topic0 string) *stellarrpc.Event {
	return &stellarrpc.Event{
		Type:           "contract",
		Ledger:         ledger,
		LedgerClosedAt: time.Now().UTC().Format(time.RFC3339),
		ContractID:     contract,
		TxHash:         tx,
		OperationIndex: op,
		Topic:          []string{topic0},
		Value:          "stub",
	}
}
