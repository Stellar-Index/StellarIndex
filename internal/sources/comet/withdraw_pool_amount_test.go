package comet

import (
	"errors"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// A withdraw burns a strictly positive BPT count: comet_liquidity CHECKs
// pool_amount_in > 0 (migration 0042) and InsertCometLiquidity refuses
// anything else with an unclassified error, which the sink retries as an
// infrastructure fault forever. The decoder must reject it instead.
func TestDecodeWithdraw_nonPositivePoolAmountInRejected(t *testing.T) {
	caller := accountStrkeyFromSeed(t, 0x50)
	token := contractStrkeyFromSeed(t, 0x51)
	for _, pool := range []*big.Int{big.NewInt(0), big.NewInt(-1)} {
		ev := events.Event{
			Topic:          []string{TopicSymbolPool, TopicSymbolWithdraw},
			Value:          encodeWithdrawBody(t, caller, token, big.NewInt(700_000), pool),
			Ledger:         52_000_401,
			TxHash:         "feed05",
			LedgerClosedAt: "2026-05-26T12:00:00Z",
		}
		out, err := NewDecoder().WithoutMetrics().Decode(ev)
		if !errors.Is(err, ErrNonPositiveAmounts) {
			t.Errorf("pool_amount_in=%s: got (%v, %v), want ErrNonPositiveAmounts", pool, out, err)
		}
	}
}
