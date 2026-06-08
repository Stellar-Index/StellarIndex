package clickhouse

import (
	"context"
	"fmt"
)

// MintBurnFlow is one supply-affecting token event from the lake: a CAP-67
// classic or SEP-41 mint / burn / clawback. Supply per contract is
// Σmint − Σburn − Σclawback. The amount lives in DataXDR (a base64 i128 scval,
// or a map for some SEP-41 variants) — the caller decodes it.
type MintBurnFlow struct {
	Ledger     uint32
	ContractID string
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
// FINAL dedups the ReplacingMergeTree parts (the sample/validation re-run
// partitions 25/45/62 carry duplicate events that would double-count supply).
// The topic_0_sym IN filter keeps the scan to the ~570 M supply flows, not the
// ~12 B total contract_events.
func StreamMintBurnFlows(ctx context.Context, addr string, from, to uint32, fn func(MintBurnFlow) error) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.Query(ctx, `
		SELECT ledger_seq, contract_id, topic_0_sym, data_xdr
		FROM stellar.contract_events FINAL
		WHERE ledger_seq BETWEEN ? AND ?
		  AND topic_0_sym IN ('mint','burn','clawback')
		ORDER BY ledger_seq`, from, to)
	if err != nil {
		return fmt.Errorf("clickhouse: query mint/burn flows [%d,%d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			ledger     uint32
			contractID string
			kind       string
			dataXDR    string
		)
		if err := rows.Scan(&ledger, &contractID, &kind, &dataXDR); err != nil {
			return fmt.Errorf("clickhouse: scan mint/burn flow: %w", err)
		}
		if err := fn(MintBurnFlow{Ledger: ledger, ContractID: contractID, Kind: kind, DataXDR: dataXDR}); err != nil {
			return err
		}
	}
	return rows.Err()
}
