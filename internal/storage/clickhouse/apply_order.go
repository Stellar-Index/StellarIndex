package clickhouse

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// applyOrderLedgerBatch is how many distinct ledgers accumulate before one
// tx_index lookup. Each lookup is a primary-key IN read on
// stellar.transactions (sort key (ledger_seq, tx_index)), so a batch costs
// one point query over at most this many ledgers' transactions.
const applyOrderLedgerBatch = 64

type ledgerTx struct {
	Ledger uint32
	TxHash string
}

// txIndexLookup resolves the apply order (0-based tx_index) of every
// transaction in the given ledgers, keyed by (ledger, tx hash).
type txIndexLookup func(ctx context.Context, ledgers []uint32) (map[ledgerTx]uint32, error)

// applyOrderer re-sorts streamed contract events within each ledger into
// transaction APPLY order. stellar.contract_events is sorted by
// (ledger_seq, tx_hash, ...), a lexical order: a pool's first trade whose
// tx hash sorts before its add_pool's would otherwise reach a live-grown
// registry first and be dropped. The SQL keeps the table's sort key
// so ClickHouse still streams in read order; the reorder is per ledger,
// buffered, and stable, so exact duplicate rows stay adjacent for the
// counting consumers that skip them by previous key.
type applyOrderer struct {
	ctx     context.Context
	lookup  txIndexLookup
	fn      func(events.Event) error
	buf     []events.Event
	ledgers []uint32
}

func newApplyOrderer(ctx context.Context, lookup txIndexLookup, fn func(events.Event) error) *applyOrderer {
	return &applyOrderer{ctx: ctx, lookup: lookup, fn: fn}
}

// add buffers one event. Input arrives ledger-ascending, so a batch is
// flushed only on a ledger boundary — a ledger is never split.
func (o *applyOrderer) add(ev events.Event) error {
	if n := len(o.ledgers); n == 0 || o.ledgers[n-1] != ev.Ledger {
		if n >= applyOrderLedgerBatch {
			if err := o.flush(); err != nil {
				return err
			}
		}
		o.ledgers = append(o.ledgers, ev.Ledger)
	}
	o.buf = append(o.buf, ev)
	return nil
}

// flush resolves the batch's tx_index values, stable-sorts by
// (ledger, tx_index) and forwards every event. A transaction missing from
// stellar.transactions fails the stream: its apply order is unknown, and
// guessing it is the bug this type exists to remove.
func (o *applyOrderer) flush() error {
	if len(o.buf) == 0 {
		return nil
	}
	idx, err := o.lookup(o.ctx, o.ledgers)
	if err != nil {
		return err
	}
	order := make([]uint32, len(o.buf))
	for i := range o.buf {
		k := ledgerTx{Ledger: o.buf[i].Ledger, TxHash: o.buf[i].TxHash}
		ti, ok := idx[k]
		if !ok {
			return fmt.Errorf("clickhouse: apply order unresolved: tx %s in ledger %d has contract_events but no stellar.transactions row", k.TxHash, k.Ledger)
		}
		order[i] = ti
	}
	perm := make([]int, len(o.buf))
	for i := range perm {
		perm[i] = i
	}
	sort.SliceStable(perm, func(a, b int) bool {
		ea, eb := &o.buf[perm[a]], &o.buf[perm[b]]
		if ea.Ledger != eb.Ledger {
			return ea.Ledger < eb.Ledger
		}
		return order[perm[a]] < order[perm[b]]
	})
	for _, p := range perm {
		if err := o.fn(o.buf[p]); err != nil {
			return err
		}
	}
	o.buf = o.buf[:0]
	o.ledgers = o.ledgers[:0]
	return nil
}

// connTxIndexLookup reads tx_index for every transaction in the given
// ledgers. Unmerged ReplacingMergeTree duplicates carry the same tx_index,
// so no FINAL is needed. A fee bump's inner hash maps too, in case an event
// row is keyed by it.
func connTxIndexLookup(conn driver.Conn) txIndexLookup {
	return func(ctx context.Context, ledgers []uint32) (map[ledgerTx]uint32, error) {
		rows, err := conn.Query(ctx, txIndexByLedgersQuery(ledgers))
		if err != nil {
			return nil, fmt.Errorf("clickhouse: tx_index lookup: %w", err)
		}
		defer func() { _ = rows.Close() }()
		out := make(map[ledgerTx]uint32)
		for rows.Next() {
			var (
				ledger      uint32
				hash, inner string
				ti          uint32
			)
			if err := rows.Scan(&ledger, &hash, &inner, &ti); err != nil {
				return nil, fmt.Errorf("clickhouse: scan tx_index: %w", err)
			}
			out[ledgerTx{ledger, hash}] = ti
			if inner != "" {
				out[ledgerTx{ledger, inner}] = ti
			}
		}
		return out, rows.Err()
	}
}

func txIndexByLedgersQuery(ledgers []uint32) string {
	parts := make([]string, len(ledgers))
	for i, l := range ledgers {
		parts[i] = strconv.FormatUint(uint64(l), 10)
	}
	return `SELECT ledger_seq, tx_hash, inner_tx_hash, tx_index
		FROM stellar.transactions
		WHERE ledger_seq IN (` + strings.Join(parts, ",") + `)`
}
