package redstone

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// ─── #291: an unrepresentable ScString feed_id must not black out the batch ──
//
// RedStone feed_ids arrive as `ScString` — arbitrary bytes, unbounded
// length — not the `ScSymbol` the Reflector/Band raw path was written
// against. So `canonical.NewOracleRawAsset` CAN refuse one, and because
// write_prices batches every updated feed into ONE event, an
// event-level refusal takes all ~19 feeds dark until a code change.
// Refusal must therefore be per-SLOT: the unrepresentable slot is
// dropped (counted + WARN-logged), every sibling feed still lands, and
// op_index positions are unchanged (DAT-03).

// oneUnrepresentableFeedID returns the args + body for a three-feed
// batch whose middle feed_id is `bad`.
func threeFeedBatch(t *testing.T, middle string) *events.Event {
	t.Helper()
	body := encodeWritePricesBody(t, relayerG,
		[]*big.Int{
			big.NewInt(oneBTCAt8),
			big.NewInt(9_000_000),
			big.NewInt(oneETHAt8),
		}, 1, 2)
	return &events.Event{
		Topic: []string{TopicSymbolRedstone},
		Value: body,
		OpArgs: []string{
			encodeAddressArg(t, relayerG),
			encodeStringVecArg(t, []string{"BTC", middle, "ETH"}),
			encodePayloadArg(t),
		},
		ContractID: adapterC,
		Ledger:     63624934,
		TxHash:     "abcd",
	}
}

func TestDecode_UnrepresentableFeedID_SkipsSlotNotEvent(t *testing.T) {
	cases := []struct {
		name   string
		feedID string
	}{
		// A control byte: legal in an ScString, refused by the raw
		// validator's printable-ASCII charset.
		{"control byte", "BAD\x01FEED"},
		// Non-ASCII (UTF-8) — same class.
		{"non-ascii", "BTCü"},
		// Longer than the 64-byte raw cap.
		{"over 64 bytes", strings.Repeat("A", 65)},
		// Whitespace is outside 0x21-0x7E too.
		{"embedded space", "HAS SPACE"},
		// Empty string.
		{"empty", ""},
	}
	btc, _ := canonical.NewCryptoAsset("BTC")
	eth, _ := canonical.NewCryptoAsset("ETH")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Pre-flight: the case is only meaningful while the raw
			// validator genuinely refuses this feed_id.
			if _, err := canonical.NewOracleRawAsset(tc.feedID); err == nil {
				t.Fatalf("fixture %q is representable — pick another", tc.feedID)
			}
			ev := threeFeedBatch(t, tc.feedID)

			dropped := obs.SourceUnrepresentableSymbolsTotal.WithLabelValues(SourceName)
			before := testutil.ToFloat64(dropped)

			updates, err := decodeWritePrices(ev, time.Now())
			if err != nil {
				t.Fatalf("one unrepresentable feed_id must not fail the whole event: %v", err)
			}
			if len(updates) != 2 {
				t.Fatalf("expected 2 updates (BTC + ETH survive), got %d", len(updates))
			}
			if !updates[0].Asset.Equal(btc) {
				t.Errorf("updates[0].Asset = %s, want %s", updates[0].Asset, btc)
			}
			if !updates[1].Asset.Equal(eth) {
				t.Errorf("updates[1].Asset = %s, want %s", updates[1].Asset, eth)
			}
			// DAT-03: the surviving rows keep their ORIGINAL vector
			// slots — ETH stays at 2, it does not slide into the
			// dropped slot 1.
			if updates[0].OpIndex != 0 {
				t.Errorf("BTC OpIndex = %d, want 0", updates[0].OpIndex)
			}
			if updates[1].OpIndex != 2 {
				t.Errorf("ETH OpIndex = %d, want 2 (dropped slot 1 must not shift it)", updates[1].OpIndex)
			}
			if got := testutil.ToFloat64(dropped) - before; got != 1 {
				t.Errorf("stellarindex_source_unrepresentable_symbols_total{source=redstone} rose by %v, want 1", got)
			}
		})
	}
}

// The drop is NOT counted as an unknown-but-recorded symbol: that
// counter's contract is "recorded verbatim as raw:<symbol>", and this
// slot was recorded nowhere. Conflating them would send the operator
// hunting for raw rows that do not exist.
func TestDecode_UnrepresentableFeedID_NotCountedAsRawRecorded(t *testing.T) {
	ev := threeFeedBatch(t, "BAD\x01FEED")
	recorded := obs.SourceUnknownSymbolsTotal.WithLabelValues(SourceName)
	before := testutil.ToFloat64(recorded)
	if _, err := decodeWritePrices(ev, time.Now()); err != nil {
		t.Fatalf("decodeWritePrices: %v", err)
	}
	if got := testutil.ToFloat64(recorded) - before; got != 0 {
		t.Errorf("stellarindex_source_unknown_symbols_total{source=redstone} rose by %v, want 0 (nothing was recorded as raw:)", got)
	}
}

// A batch in which EVERY feed_id is unrepresentable records nothing,
// so it stays an honest decode error (ErrEmptyUpdates) rather than
// silently projecting an empty batch as a no-op.
func TestDecode_AllUnrepresentable_IsEmptyUpdates(t *testing.T) {
	body := encodeWritePricesBody(t, relayerG,
		[]*big.Int{big.NewInt(1), big.NewInt(2)}, 1, 2)
	ev := &events.Event{
		Topic: []string{TopicSymbolRedstone},
		Value: body,
		OpArgs: []string{
			encodeAddressArg(t, relayerG),
			encodeStringVecArg(t, []string{"BAD\x01ONE", "BAD\x02TWO"}),
			encodePayloadArg(t),
		},
		ContractID: adapterC,
		TxHash:     "abcd",
	}
	updates, err := decodeWritePrices(ev, time.Now())
	if !errors.Is(err, ErrEmptyUpdates) {
		t.Fatalf("err = %v, want ErrEmptyUpdates", err)
	}
	if len(updates) != 0 {
		t.Errorf("expected 0 updates, got %d", len(updates))
	}
}
