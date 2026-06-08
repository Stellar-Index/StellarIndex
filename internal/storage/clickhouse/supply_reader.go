package clickhouse

import (
	"context"
	"fmt"
	"time"
)

// MintBurnFlow is one supply-affecting token event from the lake: a CAP-67
// classic or SEP-41 mint / burn / clawback. Supply per contract is
// Σmint − Σburn − Σclawback. The amount lives in DataXDR (a base64 i128 scval,
// or a map for some SEP-41 variants) — the caller decodes it.
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

// StreamMintBurnFlows streams the supply flows (mint/burn/clawback contract
// events) for [from,to] inclusive, ordered by ledger. Under the r1 archive's
// uniform V4 meta these include CAP-67 classic-asset issuance/destruction back
// to genesis AND SEP-41 token mint/burn/clawback — so Σ over all history gives
// total supply for EVERY token (baseline = 0 at asset/contract genesis), per
// docs/architecture/clickhouse-supply-from-ch.md.
//
// useFinal toggles FINAL: with it, ReplacingMergeTree parts dedup at read time
// (correct, but the all-history merge over 12 B rows is ~40× slower). Without
// it, the scan streams parts directly (fast) but double-counts the sample/
// validation re-run partitions 25/45/62 — acceptable for a quick all-token
// estimate (<0.2% error for tokens active across history; only test-tokens
// confined to those partitions inflate). The topic_0_sym IN filter keeps the
// scan to the ~570 M supply flows, not the ~12 B total contract_events.
func StreamMintBurnFlows(ctx context.Context, addr string, from, to uint32, useFinal bool, fn func(MintBurnFlow) error) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	final := ""
	if useFinal {
		final = "FINAL"
	}
	rows, err := conn.Query(ctx, fmt.Sprintf(`
		SELECT ledger_seq, close_time, contract_id, tx_hash, op_index, event_index, topic_0_sym, data_xdr
		FROM stellar.contract_events %s
		WHERE ledger_seq BETWEEN ? AND ?
		  AND topic_0_sym IN ('mint','burn','clawback')`, final), from, to)
	if err != nil {
		return fmt.Errorf("clickhouse: query mint/burn flows [%d,%d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()

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
