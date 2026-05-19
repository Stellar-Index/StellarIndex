package defindex

import (
	"context"
	"log/slog"

	"github.com/RatesEngine/rates-engine/internal/consumer"
)

// Sink consumes StrategyFlow events and persists them. For Phase A
// the sink is INFO-logging only — operators verify the dispatcher
// routes BlendStrategy events correctly via the journal, then
// Phase B (separate PR) wires:
//
//   - trades.routed_via tagging on same-tx Blend / Soroswap legs.
//   - aggregator_exposures rows from the periodic strategy-state
//     ticker.
//
// Why log-only first: the routed_via attribution path needs the
// router-attribution observer (a cross-cutting tx-batch hook,
// shared with the soroswap-router source) which doesn't exist
// yet. Better to ship the decoder + verify wire-shape on r1 with
// real traffic before tying the persist contract to a particular
// observer design.
type Sink struct {
	Logger *slog.Logger
}

// Persist implements consumer.Sink. Logs each strategy flow at INFO
// level with the call shape: contract, direction, from, amount. The
// pipeline's PersistEvents loop calls this once per dispatched
// Event.
func (s *Sink) Persist(_ context.Context, ev consumer.Event) error {
	ve, ok := ev.(Event)
	if !ok {
		return nil // not ours; pipeline already filtered but be defensive
	}
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("defindex strategy flow",
		"source", SourceName,
		"tx_hash", ve.Flow.TxHash,
		"ledger", ve.Flow.Ledger,
		"contract_id", ve.Flow.ContractID,
		"direction", string(ve.Flow.Direction),
		"from", ve.Flow.From,
		"amount", ve.Flow.Amount.String(),
	)
	return nil
}
