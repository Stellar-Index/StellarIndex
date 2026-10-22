package clickhouse

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// contractActivitySettledQuery is the ledger a contract's memoised activity base stops below: ~14h under the lake tip, rounded down to
// 100k ledgers so the split holds for days. max() is answered from part metadata.
const contractActivitySettledQuery = `SELECT toUInt32(greatest(intDiv(toInt64(max(ledger_seq)) - 10000, 100000) * 100000, 0))
	FROM stellar.ledgers`

// contractActivityRangeQuery counts one side of the split; the two sides are disjoint ledger ranges, so their uniqExact counts add.
const contractActivityRangeQuery = `SELECT min(close_time), max(close_time), toUInt64(uniqExact(ledger_seq))
	FROM stellar.contract_active_ledgers WHERE contract_id = ? AND ledger_seq `

const (
	// contractActivityBaseMaxAge bounds how long a row written late below the split (a backfill) can go uncounted.
	contractActivityBaseMaxAge  = 6 * time.Hour
	contractActivityBaseMemoMax = 8192
)

type contractActivitySpan struct {
	first, last time.Time
	total       uint64
}

// plus joins a span with the span directly above it.
func (a contractActivitySpan) plus(above contractActivitySpan) contractActivitySpan {
	switch {
	case a.total == 0:
		return above
	case above.total == 0:
		return a
	}
	return contractActivitySpan{first: a.first, last: above.last, total: a.total + above.total}
}

type contractActivityBase struct {
	settled uint32
	span    contractActivitySpan
	at      time.Time
}

type contractActivityMemo struct {
	mu sync.Mutex
	m  map[string]contractActivityBase
}

func (m *contractActivityMemo) get(id string, now time.Time) (contractActivityBase, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.m[id]
	return b, ok && now.Sub(b.at) < contractActivityBaseMaxAge
}

func (m *contractActivityMemo) put(id string, b contractActivityBase) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, held := m.m[id]; m.m == nil || (!held && len(m.m) >= contractActivityBaseMemoMax) {
		m.m = make(map[string]contractActivityBase)
	}
	m.m[id] = b
}

// contractActivityBounds is the contract's lifetime activity span: a memoised base below the settled ledger plus the live tail above
// it. The exact lifetime count reads every row (137M, ~1.7s for the XLM SAC); the tail reads under ~1M ledgers' worth.
func (r *ExplorerReader) contractActivityBounds(ctx context.Context, contractID string) (contractActivitySpan, error) {
	now := time.Now()
	base, ok := r.activityBases.get(contractID, now)
	if !ok {
		var settled uint32
		if err := r.conn.QueryRow(ctx, contractActivitySettledQuery).Scan(&settled); err != nil {
			return contractActivitySpan{}, fmt.Errorf("clickhouse: contract activity settled ledger: %w", err)
		}
		span, err := r.contractActivityRange(ctx, contractID, "< ?", settled)
		if err != nil {
			return contractActivitySpan{}, err
		}
		base = contractActivityBase{settled: settled, span: span, at: now}
		r.activityBases.put(contractID, base)
	}
	tail, err := r.contractActivityRange(ctx, contractID, ">= ?", base.settled)
	if err != nil {
		return contractActivitySpan{}, err
	}
	return base.span.plus(tail), nil
}

func (r *ExplorerReader) contractActivityRange(ctx context.Context, contractID, cmp string, settled uint32) (contractActivitySpan, error) {
	var s contractActivitySpan
	if err := r.conn.QueryRow(ctx, contractActivityRangeQuery+cmp, contractID, settled).Scan(&s.first, &s.last, &s.total); err != nil {
		return contractActivitySpan{}, fmt.Errorf("clickhouse: contract activity bounds: %w", err)
	}
	return s, nil
}
