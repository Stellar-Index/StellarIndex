package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// MintBurnFlow is one supply-affecting token event (CAP-67 or SEP-41 mint/burn/clawback); the amount is in DataXDR (base64 scval), which the caller decodes.
type MintBurnFlow struct {
	Ledger     uint32
	CloseTime  time.Time
	ContractID string
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	Kind       string // "mint" | "burn" | "clawback"
	DataXDR    string
}

// StreamMintBurnFlows streams mint/burn/clawback events for [from,to] ordered by ledger; useFinal dedups ReplacingMergeTree parts (correct, ~40x slower), without it re-run partitions double-count (estimates only).
func StreamMintBurnFlows(ctx context.Context, addr string, from, to uint32, useFinal bool, fn func(MintBurnFlow) error) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.Query(ctx, mintBurnFlowsQuery(useFinal), from, to)
	if err != nil {
		return fmt.Errorf("clickhouse: query mint/burn flows [%d,%d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()
	return streamMintBurnFlowRows(rows, fn)
}

// mintBurnFlowsQuery is StreamMintBurnFlows' SQL, built here so its text stays assertable. TWO bind parameters (the ledger window's from
// + to); useFinal is a SHAPE toggle, so the two variants must differ ONLY by the FINAL keyword.
func mintBurnFlowsQuery(useFinal bool) string {
	final := ""
	if useFinal {
		final = "FINAL"
	}
	return fmt.Sprintf(`
		SELECT ledger_seq, close_time, contract_id, tx_hash, op_index, event_index, topic_0_sym, data_xdr
		FROM stellar.contract_events %s
		WHERE ledger_seq BETWEEN ? AND ?
		  AND topic_0_sym IN ('mint','burn','clawback')`, final)
}

// streamMintBurnFlowRows decodes an open mintBurnFlowsQuery result set and invokes fn once per row, in ClickHouse's order; split out to
// be drivable against a stub driver.Rows. fn's error aborts the stream and is returned VERBATIM.
func streamMintBurnFlowRows(rows driver.Rows, fn func(MintBurnFlow) error) error {
	for rows.Next() {
		var (
			ledger     uint32
			closeTime  time.Time
			contractID string
			txHash     string
			opIndex    uint32
			eventIndex uint32
			kind       string
			dataXDR    string
		)
		if err := rows.Scan(&ledger, &closeTime, &contractID, &txHash, &opIndex, &eventIndex, &kind, &dataXDR); err != nil {
			return fmt.Errorf("clickhouse: scan mint/burn flow: %w", err)
		}
		if err := fn(MintBurnFlow{
			Ledger: ledger, CloseTime: closeTime, ContractID: contractID,
			TxHash: txHash, OpIndex: opIndex, EventIndex: eventIndex, Kind: kind, DataXDR: dataXDR,
		}); err != nil {
			return err
		}
	}
	return rows.Err()
}
