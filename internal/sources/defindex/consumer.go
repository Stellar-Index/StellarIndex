package defindex

import (
	"context"
	"log/slog"
	"strings"

	"github.com/RatesEngine/rates-engine/internal/consumer"
)

// Sink consumes defindex flow events at both layers (StrategyFlow
// from the underlying Blend strategies, VaultFlow from the
// user-facing DeFindex vault wrappers) and persists them. For now
// the sink is INFO-logging only — operators verify the dispatcher
// routes events correctly via the journal, then a follow-up wires:
//
//   - A typed `defindex_flows` hypertable so events become
//     audit-queryable post-decode (currently the counter is the only
//     after-the-fact record; this is why the 2026-05-21 re-backfill
//     was needed to recover historical counts).
//   - trades.routed_via tagging on same-tx Blend / Soroswap legs.
//   - aggregator_exposures rows from the periodic strategy-state
//     ticker.
//
// Why log-only still: the routed_via attribution path needs the
// router-attribution observer (a cross-cutting tx-batch hook,
// shared with the soroswap-router source) which doesn't exist yet.
// Better to ship the decoder + verify wire-shape on r1 with real
// traffic before tying the persist contract to a particular
// observer design.
type Sink struct {
	Logger *slog.Logger
}

// Persist implements consumer.Sink. Logs each defindex flow at
// INFO level — strategy-layer and vault-layer flows get distinct
// "msg" tags so operators can grep either independently. The
// pipeline's PersistEvents loop calls this once per dispatched
// Event.
func (s *Sink) Persist(_ context.Context, ev consumer.Event) error {
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	switch e := ev.(type) {
	case Event:
		logger.Info("defindex strategy flow",
			"source", SourceName,
			"tx_hash", e.Flow.TxHash,
			"ledger", e.Flow.Ledger,
			"contract_id", e.Flow.ContractID,
			"direction", string(e.Flow.Direction),
			"from", e.Flow.From,
			"amount", e.Flow.Amount.String(),
		)
	case VaultEvent:
		amounts := make([]string, 0, len(e.Flow.Amounts))
		for _, a := range e.Flow.Amounts {
			amounts = append(amounts, a.String())
		}
		logger.Info("defindex vault flow",
			"source", SourceName,
			"tx_hash", e.Flow.TxHash,
			"ledger", e.Flow.Ledger,
			"contract_id", e.Flow.ContractID,
			"direction", string(e.Flow.Direction),
			"user", e.Flow.User,
			"amounts", strings.Join(amounts, ","),
			"df_tokens", e.Flow.DfTokens.String(),
		)
	}
	// Unknown event types: pipeline already filtered, no-op.
	return nil
}
