package clickhouse

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// OrderWithTxIndex runs evs through an index-stamping applyOrderer whose lookup is served from the extracted txs.
func OrderWithTxIndex(evs []events.Event, txs []TransactionRow, fn func(events.Event) error) error {
	idx := make(map[ledgerTx]uint32, len(txs))
	for _, tx := range txs {
		idx[ledgerTx{Ledger: tx.LedgerSeq, TxHash: tx.TxHash}] = tx.TxIndex
	}
	lookup := func(context.Context, []uint32) (map[ledgerTx]uint32, error) { return idx, nil }
	o := newApplyOrderer(context.Background(), lookup, fn)
	o.stampTxIndex = true
	for _, ev := range evs {
		if err := o.add(ev); err != nil {
			return err
		}
	}
	return o.flush()
}
