package clickhouse

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"
)

// The side names the stellar.sponsorship_events column an account is matched on.
const (
	SponsorshipSideSponsor   = "sponsor"
	SponsorshipSideSponsored = "sponsored"
)

// SponsorshipKey identifies one arrangement: one sponsor paying the reserves of one entry, or one signer, of one
// account. It doubles as the keyset cursor of a page of arrangements.
type SponsorshipKey struct {
	Sponsor   string
	Sponsored string
	Kind      string
	KeyID     string
}

// IsSet reports whether the key is a continuation cursor.
func (k SponsorshipKey) IsSet() bool { return k != SponsorshipKey{} }

// SponsorshipEventRow is one lifecycle transition of an arrangement.
type SponsorshipEventRow struct {
	Ledger         uint32
	CloseTime      time.Time
	TxHash         string
	OpIndex        int32
	ChangeIndex    uint32
	IntraLedgerSeq uint32
	Event          string
}

// SponsorshipArrangement is one arrangement with its events in apply order, unmerged duplicates collapsed.
type SponsorshipArrangement struct {
	SponsorshipKey
	Reserves uint8
	Events   []SponsorshipEventRow
}

// SponsorshipTotals counts one side of an account's arrangements through a ledger; InForceReserves is the number
// the account entry's own num_sponsoring (sponsor side) or num_sponsored (sponsored side) counter must equal.
type SponsorshipTotals struct {
	InForce         uint64
	InForceReserves uint64
	Started         uint64
	Revoked         uint64
	Ended           uint64
}

type sponsorshipEventKey struct {
	ledger      uint32
	txHash      string
	opIndex     int32
	changeIndex uint32
	event       string
}

const sponsorshipEventCols = `sponsor, sponsored, kind, key_id, reserves, ledger, close_time, tx_hash, op_index,
	change_index, intra_ledger_seq, event`

// sponsorshipLastEvent picks the event applied last; the tuple orders events as the ledger applied them.
const sponsorshipLastEvent = `argMax(event, (ledger, intra_ledger_seq, change_index))`

func sponsorshipSideColumn(side string) (string, error) {
	switch side {
	case SponsorshipSideSponsor, SponsorshipSideSponsored:
		return side, nil
	}
	return "", fmt.Errorf("clickhouse: sponsorship side %q", side)
}

// sponsorshipWindowRows bounds one in-order read of an account's events; sponsorshipMaxWindows bounds a page, so an
// account whose arrangements are mostly over answers a short page with a resume key instead of scanning everything.
const (
	sponsorshipWindowRows = 20000
	sponsorshipMaxWindows = 8
)

// AccountSponsorships pages, in key order after `after`, the arrangements in which the account is the side's party,
// each with every event through maxLedger; inForceOnly keeps the arrangements whose last event is a start. Events
// are read in sort-key order a window at a time, which stops early, where a GROUP BY over the account would not. When
// the window budget runs out first, the page is short and the returned key is where the next page resumes. An
// absent table reads as none.
func (r *ExplorerReader) AccountSponsorships(ctx context.Context, account, side string, inForceOnly bool,
	after SponsorshipKey, maxLedger uint32, limit int,
) ([]SponsorshipArrangement, SponsorshipKey, error) {
	col, err := sponsorshipSideColumn(side)
	if err != nil {
		return nil, SponsorshipKey{}, err
	}
	if limit <= 0 {
		return nil, SponsorshipKey{}, nil
	}
	window := `SELECT ` + sponsorshipEventCols + ` FROM stellar.sponsorship_events
		WHERE ` + col + ` = ? AND ledger <= ? AND (sponsor, sponsored, kind, key_id) > (?, ?, ?, ?)
		ORDER BY sponsor, sponsored, kind, key_id, ledger LIMIT ?` + explorerScanSettings
	whole := `SELECT ` + sponsorshipEventCols + ` FROM stellar.sponsorship_events
		WHERE (sponsor, sponsored, kind, key_id) = (?, ?, ?, ?) AND ledger <= ?` + explorerScanSettings
	return pageSponsorships(after, limit, sponsorshipWindowRows, sponsorshipMaxWindows, inForceOnly,
		func(after SponsorshipKey) ([]SponsorshipArrangement, int, error) {
			return r.sponsorshipArrangements(ctx, window, account, maxLedger,
				after.Sponsor, after.Sponsored, after.Kind, after.KeyID, sponsorshipWindowRows)
		},
		func(k SponsorshipKey) ([]SponsorshipArrangement, error) {
			out, _, err := r.sponsorshipArrangements(ctx, whole, k.Sponsor, k.Sponsored, k.Kind, k.KeyID, maxLedger)
			return out, err
		})
}

// pageSponsorships fills a page from windows of at most windowRows events each, read in key order after `after`. A
// full window's last arrangement may be cut short, so it is left to the next window, unless it is the window's only
// one, which is then read whole.
func pageSponsorships(after SponsorshipKey, limit, windowRows, maxWindows int, inForceOnly bool,
	window func(after SponsorshipKey) ([]SponsorshipArrangement, int, error),
	whole func(SponsorshipKey) ([]SponsorshipArrangement, error),
) ([]SponsorshipArrangement, SponsorshipKey, error) {
	var out []SponsorshipArrangement
	for range maxWindows {
		win, n, err := window(after)
		if err != nil {
			return nil, SponsorshipKey{}, err
		}
		full := n >= windowRows
		if full {
			if win, err = completeWindow(win, whole); err != nil {
				return nil, SponsorshipKey{}, err
			}
		}
		for _, a := range win {
			if inForceOnly && !sponsorshipInForce(a.Events) {
				continue
			}
			out = append(out, a)
			if len(out) == limit {
				return out, SponsorshipKey{}, nil
			}
		}
		if !full || len(win) == 0 {
			return out, SponsorshipKey{}, nil
		}
		after = win[len(win)-1].SponsorshipKey
	}
	return out, after, nil
}

// completeWindow drops a full window's last arrangement, whose events may run past it, or reads it whole when it
// is the window's only one.
func completeWindow(win []SponsorshipArrangement, whole func(SponsorshipKey) ([]SponsorshipArrangement, error),
) ([]SponsorshipArrangement, error) {
	if len(win) == 1 {
		return whole(win[0].SponsorshipKey)
	}
	return win[:len(win)-1], nil
}

// sponsorshipArrangements runs an event query in key order and groups its rows into arrangements, each with its
// events deduplicated and in apply order; it also returns the raw row count.
func (r *ExplorerReader) sponsorshipArrangements(ctx context.Context, q string, args ...any) ([]SponsorshipArrangement, int, error) {
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		if absentReadsEmpty("sponsorship_events", err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("clickhouse: account sponsorships: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SponsorshipArrangement
	n := 0
	seen := map[sponsorshipEventKey]struct{}{}
	for rows.Next() {
		n++
		var k SponsorshipKey
		var reserves uint8
		var e SponsorshipEventRow
		if err := rows.Scan(&k.Sponsor, &k.Sponsored, &k.Kind, &k.KeyID, &reserves, &e.Ledger, &e.CloseTime,
			&e.TxHash, &e.OpIndex, &e.ChangeIndex, &e.IntraLedgerSeq, &e.Event); err != nil {
			return nil, 0, fmt.Errorf("clickhouse: account sponsorships scan: %w", err)
		}
		if len(out) == 0 || out[len(out)-1].SponsorshipKey != k {
			out = append(out, SponsorshipArrangement{SponsorshipKey: k, Reserves: reserves})
			clear(seen)
		}
		ek := sponsorshipEventKey{e.Ledger, e.TxHash, e.OpIndex, e.ChangeIndex, e.Event}
		if _, dup := seen[ek]; dup {
			continue
		}
		seen[ek] = struct{}{}
		a := &out[len(out)-1]
		a.Events = append(a.Events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("clickhouse: account sponsorships: %w", err)
	}
	for i := range out {
		slices.SortStableFunc(out[i].Events, func(a, b SponsorshipEventRow) int {
			return cmp.Or(cmp.Compare(a.Ledger, b.Ledger), cmp.Compare(a.IntraLedgerSeq, b.IntraLedgerSeq),
				cmp.Compare(a.ChangeIndex, b.ChangeIndex))
		})
	}
	return out, n, nil
}

// sponsorshipInForce reports whether the last event applied, of events in apply order, is a start.
func sponsorshipInForce(events []SponsorshipEventRow) bool {
	return len(events) > 0 && events[len(events)-1].Event == "started"
}

// SponsorshipTotals counts the account's arrangements on one side through maxLedger: the completed buckets plus the
// tip bucket read live, or every event when the buckets do not yet reach maxLedger's bucket.
func (r *ExplorerReader) SponsorshipTotals(ctx context.Context, account, side string, maxLedger uint32) (SponsorshipTotals, error) {
	col, err := sponsorshipSideColumn(side)
	if err != nil {
		return SponsorshipTotals{}, err
	}
	if t, ok, err := r.sponsorshipTotalsFromBuckets(ctx, account, side, col, maxLedger); ok || err != nil {
		return t, err
	}
	return r.sponsorshipTotalsFromEvents(ctx, account, col, maxLedger)
}

// sponsorshipTotalsFromBuckets reports !ok when the buckets cannot answer exactly: unfilled below the tip bucket,
// absent, or summing negative (events out of order across buckets), each of which the event count still answers.
func (r *ExplorerReader) sponsorshipTotalsFromBuckets(ctx context.Context, account, side, col string, maxLedger uint32) (SponsorshipTotals, bool, error) {
	cut := maxLedger / SponsorshipTotalsBucketLedgers
	q := `SELECT twm, bf + tf, bfr + tfr, bs + ts, br + tr, be + te
		FROM (SELECT max(thru_ledger) AS twm FROM stellar.entry_history_watermark WHERE name = ?) AS w
		CROSS JOIN (SELECT sum(in_force) AS bf, sum(in_force_reserves) AS bfr, sum(started) AS bs, sum(revoked) AS br, sum(ended) AS be
			FROM stellar.sponsorship_totals_buckets FINAL WHERE account = ? AND side = ? AND bucket < ?) AS b
		CROSS JOIN (SELECT sum(d) AS tf, sum(d * toInt64(res)) AS tfr, sum(ns) AS ts, sum(nr) AS tr, sum(ne) AS te
			FROM (` + fmt.Sprintf(sponsorshipBucketDeltas, col+" = ? AND") + `)) AS t` + explorerScanSettings
	var (
		twm       uint32
		f, fr     int64
		s, rv, en uint64
	)
	err := r.conn.QueryRow(ctx, q, sponsorshipTotalsWatermarkName, account, side, cut, account, cut*SponsorshipTotalsBucketLedgers, maxLedger).
		Scan(&twm, &f, &fr, &s, &rv, &en)
	if err != nil {
		if isSchemaAbsent(err) {
			return SponsorshipTotals{}, false, nil
		}
		return SponsorshipTotals{}, false, fmt.Errorf("clickhouse: sponsorship totals buckets: %w", err)
	}
	if uint64(twm)+1 < uint64(cut)*SponsorshipTotalsBucketLedgers || f < 0 || fr < 0 {
		return SponsorshipTotals{}, false, nil
	}
	return SponsorshipTotals{InForce: uint64(f), InForceReserves: uint64(fr), Started: s, Revoked: rv, Ended: en}, true, nil
}

// sponsorshipTotalsFromEvents counts every event through maxLedger. Unmerged duplicates of an event share its
// identity, so events are counted with uniqExact; an absent table reads as zero.
func (r *ExplorerReader) sponsorshipTotalsFromEvents(ctx context.Context, account, col string, maxLedger uint32) (SponsorshipTotals, error) {
	q := `SELECT countIf(last_event = 'started'), sumIf(res, last_event = 'started'),
			sum(n_started), sum(n_revoked), sum(n_ended)
		FROM (
			SELECT ` + sponsorshipLastEvent + ` AS last_event, max(reserves) AS res,
				uniqExactIf((ledger, tx_hash, op_index, change_index), event = 'started') AS n_started,
				uniqExactIf((ledger, tx_hash, op_index, change_index), event = 'revoked') AS n_revoked,
				uniqExactIf((ledger, tx_hash, op_index, change_index), event = 'ended') AS n_ended
			FROM stellar.sponsorship_events
			WHERE ` + col + ` = ? AND ledger <= ?
			GROUP BY sponsor, sponsored, kind, key_id)` + explorerScanSettings
	var t SponsorshipTotals
	if err := r.conn.QueryRow(ctx, q, account, maxLedger).Scan(&t.InForce, &t.InForceReserves, &t.Started, &t.Revoked, &t.Ended); err != nil {
		if absentReadsEmpty("sponsorship_events", err) {
			return SponsorshipTotals{}, nil
		}
		return SponsorshipTotals{}, fmt.Errorf("clickhouse: sponsorship totals: %w", err)
	}
	return t, nil
}
