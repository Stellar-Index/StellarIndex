package clickhouse

import (
	"context"
	"fmt"
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

// AccountSponsorships pages, in key order after `after`, the arrangements in which the account is the side's party,
// each with every event through maxLedger; inForceOnly keeps the arrangements whose last event is a start. An
// absent table reads as none.
func (r *ExplorerReader) AccountSponsorships(ctx context.Context, account, side string, inForceOnly bool,
	after SponsorshipKey, maxLedger uint32, limit int,
) ([]SponsorshipArrangement, error) {
	col, err := sponsorshipSideColumn(side)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	having := ``
	if inForceOnly {
		having = ` HAVING ` + sponsorshipLastEvent + ` = 'started'`
	}
	q := `SELECT ` + sponsorshipEventCols + ` FROM stellar.sponsorship_events
		WHERE ` + col + ` = ? AND ledger <= ? AND (sponsor, sponsored, kind, key_id) IN (
			SELECT sponsor, sponsored, kind, key_id FROM stellar.sponsorship_events
			WHERE ` + col + ` = ? AND ledger <= ? AND (sponsor, sponsored, kind, key_id) > (?, ?, ?, ?)
			GROUP BY sponsor, sponsored, kind, key_id` + having + `
			ORDER BY sponsor, sponsored, kind, key_id LIMIT ?)
		ORDER BY sponsor, sponsored, kind, key_id, ledger, intra_ledger_seq, change_index` + explorerScanSettings
	rows, err := r.conn.Query(ctx, q, account, maxLedger, account, maxLedger,
		after.Sponsor, after.Sponsored, after.Kind, after.KeyID, limit)
	if err != nil {
		if absentReadsEmpty("sponsorship_events", err) {
			return nil, nil
		}
		return nil, fmt.Errorf("clickhouse: account sponsorships: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SponsorshipArrangement
	seen := map[sponsorshipEventKey]struct{}{}
	for rows.Next() {
		var k SponsorshipKey
		var reserves uint8
		var e SponsorshipEventRow
		if err := rows.Scan(&k.Sponsor, &k.Sponsored, &k.Kind, &k.KeyID, &reserves, &e.Ledger, &e.CloseTime,
			&e.TxHash, &e.OpIndex, &e.ChangeIndex, &e.IntraLedgerSeq, &e.Event); err != nil {
			return nil, fmt.Errorf("clickhouse: account sponsorships scan: %w", err)
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
		return nil, fmt.Errorf("clickhouse: account sponsorships: %w", err)
	}
	return out, nil
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
