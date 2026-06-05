package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/RatesEngine/rates-engine/internal/events"
)

// StreamContractEvents is the Phase-4 input adapter (ADR-0034): it reads
// stellar.contract_events for [from,to] inclusive, ordered by
// (ledger_seq, tx_hash, op_index, event_index) — the dispatcher's natural
// emission order — and invokes fn for each row reconstructed as an
// events.Event.
//
// The CH columns are a byte-identical serialization of events.Event: topics,
// value, and op-args are all base64(scval.MarshalBinary), exactly as the
// production dispatcher writes them (internal/dispatcher.contractEventToEventsEvent
// at dispatcher.go:881/:907 vs the extractor's eventRow at extract.go:181/:206).
// So the existing protocol decoders consume these events verbatim — no
// re-encoding, no galexie re-touch.
//
// FINAL dedups concurrent/duplicate ReplacingMergeTree parts at read time.
// Callers re-projecting all history should window [from,to] (e.g. per 1M-ledger
// partition) so the streamed result set stays bounded in memory.
//
// Note: ID and TransactionIndex are left zero — the CH lake keys events by
// (ledger, tx_hash, op_index, event_index) and decoders use TxHash, not the
// RPC-shape ID/tx-index. If a future decoder needs tx_index, add it to the
// contract_events schema + extractor first.
func StreamContractEvents(ctx context.Context, addr string, from, to uint32, fn func(events.Event) error) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.Query(ctx, `
		SELECT ledger_seq, close_time, tx_hash, op_index, event_index,
		       contract_id, event_type, topics_xdr, data_xdr, op_args_xdr,
		       in_successful_call
		FROM stellar.contract_events FINAL
		WHERE ledger_seq BETWEEN ? AND ?
		ORDER BY ledger_seq, tx_hash, op_index, event_index`, from, to)
	if err != nil {
		return fmt.Errorf("clickhouse: query contract_events [%d,%d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			ledger     uint32
			closeTime  time.Time
			txHash     string
			opIndex    uint32
			eventIndex uint32
			contractID string
			eventType  string
			topics     []string
			dataXDR    string
			opArgs     []string
			inSucc     uint8
		)
		if err := rows.Scan(&ledger, &closeTime, &txHash, &opIndex, &eventIndex,
			&contractID, &eventType, &topics, &dataXDR, &opArgs, &inSucc); err != nil {
			return fmt.Errorf("clickhouse: scan contract_event: %w", err)
		}
		ev := events.Event{
			Type:                     eventType,
			Ledger:                   ledger,
			LedgerClosedAt:           closeTime.UTC().Format(time.RFC3339),
			ContractID:               contractID,
			OperationIndex:           int(opIndex),
			EventIndex:               int(eventIndex),
			TxHash:                   txHash,
			InSuccessfulContractCall: inSucc != 0,
			Topic:                    topics,
			Value:                    dataXDR,
			OpArgs:                   opArgs,
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
	return rows.Err()
}
