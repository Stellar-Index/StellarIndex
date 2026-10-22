package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Entry-change history. `ch-entry-history` pairs the pre- and post-images of the stellar.ledger_entry_changes rows that
// can carry a sponsorship transition and writes stellar.sponsorship_events from them. The account- and asset-keyed
// row types below are the decode-on-read shape served over the entry index; nothing writes them to the lake.

// EntryHistorySourceRow is one stellar.ledger_entry_changes row as the entry-history derive reads it.
type EntryHistorySourceRow struct {
	Ledger         uint32
	CloseTime      time.Time
	TxHash         string
	OpIndex        int32
	ChangeIndex    uint32
	IntraLedgerSeq uint32
	ChangeType     string
	EntryType      string
	KeyXDR         string
	EntryXDR       string
}

// EntryChangeRef is the change a history row records: its lake position, what happened, the differing decoded fields and the decoded entry as JSON; Fields carries the pre-image for a removal.
type EntryChangeRef struct {
	Ledger         uint32
	CloseTime      time.Time
	TxHash         string
	OpIndex        int32
	ChangeIndex    uint32
	IntraLedgerSeq uint32
	EntryType      string
	ChangeType     string
	Changed        []string
	Fields         string
}

// AccountEntryChange is one decoded entry change as seen by one account in one role (owner/sponsor/claimant); Balance is post-change (0 once removed).
type AccountEntryChange struct {
	EntryChangeRef
	Account string
	Role    string
	Asset   string
	Balance *big.Int
}

// AssetEntryChange is one decoded entry change as seen by one asset in one role; Account is the holder, seller or sponsor, empty for a liquidity pool.
type AssetEntryChange struct {
	EntryChangeRef
	Asset   string
	Role    string
	Account string
	Balance *big.Int
}

// SponsorshipEvent is one stellar.sponsorship_events row: an arrangement starting, being revoked (its entry lives on,
// unsponsored or under another sponsor) or ending with its entry or signer. Sponsored is empty for a claimable balance.
type SponsorshipEvent struct {
	Ledger         uint32
	CloseTime      time.Time
	TxHash         string
	OpIndex        int32
	ChangeIndex    uint32
	IntraLedgerSeq uint32
	Sponsor        string
	Sponsored      string
	Kind           string
	KeyID          string
	Reserves       uint8
	Event          string
}

// sponsorshipOps can change a signer's sponsor on an account that is not itself sponsored, so no image in the group
// carries the sponsored tail: a signer added inside a sponsorship sandwich, a signer sponsorship revoked or
// transferred, and a merge ending every signer's sponsorship.
const sponsorshipOps = `('OperationTypeSetOptions', 'OperationTypeRevokeSponsorship', 'OperationTypeAccountMerge')`

// sponsorshipCandidateQuery streams, in the source's sort-key order (each 'state' pre-image directly before its change),
// every classic entry change of an operation that can hold a sponsorship transition: a group with a sponsored image, or
// one of sponsorshipOps. That is a few percent of the window's rows. No FINAL: an unmerged duplicate re-derives a
// byte-identical event row, and the target is a ReplacingMergeTree on the same identity.
const sponsorshipCandidateQuery = `
	SELECT ledger_seq, close_time, tx_hash, op_index, change_index, intra_ledger_seq,
	       change_type, entry_type, key_xdr, entry_xdr
	FROM stellar.ledger_entry_changes
	WHERE ledger_seq BETWEEN ? AND ?
	  AND tx_hash != ''
	  AND entry_type IN ('account', 'trustline', 'offer', 'data', 'claimable_balance', 'liquidity_pool')
	  AND (tx_hash, op_index) IN (
	        SELECT tx_hash, op_index FROM stellar.ledger_entry_changes
	         WHERE ledger_seq BETWEEN ? AND ? AND tx_hash != '' AND entry_xdr != '' AND ` + sponsoredEntryPredicate + `
	        UNION ALL
	        SELECT tx_hash, toInt32(op_index) FROM stellar.operations
	         WHERE ledger_seq BETWEEN ? AND ? AND op_type IN ` + sponsorshipOps + `)
	ORDER BY ledger_seq, tx_hash, op_index, change_index
`

// ErrSponsorshipOperationsShortfall reports a refused derive over a window whose stellar.operations rows cover fewer
// ledgers than stellar.ledgers declares tx-bearing: the candidate read would silently miss signer transitions.
var ErrSponsorshipOperationsShortfall = errors.New("clickhouse: sponsorship window lacks stellar.operations coverage")

// sponsorshipOperationsCovered proves the operations side of the candidate read: every transaction holds at least one
// operation and the sink writes failed transactions' operations too, so each tx-bearing ledger must appear.
func sponsorshipOperationsCovered(ctx context.Context, conn driver.Conn, from, thru uint32) error {
	const q = `SELECT (SELECT uniqExact(ledger_seq) FROM stellar.ledgers WHERE ledger_seq BETWEEN ? AND ? AND tx_count > 0),
	                  (SELECT uniqExact(ledger_seq) FROM stellar.operations WHERE ledger_seq BETWEEN ? AND ?)`
	var txLedgers, opLedgers uint64
	if err := conn.QueryRow(ctx, q, from, thru, from, thru).Scan(&txLedgers, &opLedgers); err != nil {
		return fmt.Errorf("clickhouse: sponsorship window [%d,%d] operations coverage: %w", from, thru, err)
	}
	if opLedgers < txLedgers {
		return fmt.Errorf("%w: [%d,%d] holds operations for %d of %d tx-bearing ledgers", ErrSponsorshipOperationsShortfall, from, thru, opLedgers, txLedgers)
	}
	return nil
}

// StreamSponsorshipCandidates calls fn for every candidate entry change in [from, to], in (ledger_seq, tx_hash,
// op_index, change_index) order, after proving the window's operations coverage.
func StreamSponsorshipCandidates(ctx context.Context, addr string, from, to uint32, fn func(EntryHistorySourceRow) error) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := sponsorshipOperationsCovered(ctx, conn, from, to); err != nil {
		return err
	}
	rows, err := conn.Query(ctx, sponsorshipCandidateQuery, from, to, from, to, from, to)
	if err != nil {
		return fmt.Errorf("clickhouse: query sponsorship candidates [%d,%d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r EntryHistorySourceRow
		if err := rows.Scan(&r.Ledger, &r.CloseTime, &r.TxHash, &r.OpIndex, &r.ChangeIndex, &r.IntraLedgerSeq,
			&r.ChangeType, &r.EntryType, &r.KeyXDR, &r.EntryXDR); err != nil {
			return fmt.Errorf("clickhouse: scan sponsorship candidate: %w", err)
		}
		r.CloseTime = r.CloseTime.UTC()
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

// entryHistoryInsertChunk bounds one native INSERT batch.
const entryHistoryInsertChunk = 20_000

// InsertSponsorshipEvents writes one batch; retry-safe since the ReplacingMergeTree collapses a re-sent batch on merge.
func InsertSponsorshipEvents(ctx context.Context, addr string, rows []SponsorshipEvent) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	conn, err := openAccountMovementsWrite(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()

	var written int64
	for i := 0; i < len(rows); i += entryHistoryInsertChunk {
		end := min(i+entryHistoryInsertChunk, len(rows))
		if err := insertSponsorshipEvents(ctx, conn, rows[i:end]); err != nil {
			return written, err
		}
		written += int64(end - i)
	}
	return written, nil
}

func insertSponsorshipEvents(ctx context.Context, conn driver.Conn, rows []SponsorshipEvent) error {
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO stellar.sponsorship_events
		(sponsor, sponsored, kind, key_id, ledger, close_time, tx_hash, op_index, change_index, intra_ledger_seq, reserves, event)`)
	if err != nil {
		return fmt.Errorf("prepare sponsorship_events batch: %w", err)
	}
	for _, r := range rows {
		if err := batch.Append(r.Sponsor, r.Sponsored, r.Kind, r.KeyID, r.Ledger, r.CloseTime, r.TxHash, r.OpIndex,
			r.ChangeIndex, r.IntraLedgerSeq, r.Reserves, r.Event); err != nil {
			return fmt.Errorf("append sponsorship_events %s/%d/%s/%d/%d: %w", r.Sponsor, r.Ledger, r.TxHash, r.OpIndex, r.ChangeIndex, err)
		}
	}
	return wrapSend(batch.Send(), "sponsorship_events")
}

// ErrEntryHistoryHole reports a refused watermark advance over a window the lake does not hold in full; a delay, not a failure.
var ErrEntryHistoryHole = errors.New("clickhouse: entry-history window is not contiguous in the lake")

// ErrEntryHistorySkippedPrefix reports a refused advance whose window starts above watermark+1, which would claim never-derived ledgers as done.
var ErrEntryHistorySkippedPrefix = errors.New("clickhouse: entry-history window skips ledgers below it")

// ErrEntryHistoryEntryChangeShortfall reports a refused advance where entry changes were read for fewer ledgers than stellar.ledgers declares tx-bearing.
var ErrEntryHistoryEntryChangeShortfall = errors.New("clickhouse: entry-history window lacks ledger_entry_changes coverage")

// entryHistoryWindowCovered compares the tx-bearing ledgers the derive READ (non-empty tx_hash; seed rows carry none) with
// stellar.ledgers. A lake-state check after the read would pass once a concurrent ch-backfill filled the window though the derive saw none
// of those rows; only tx-bearing ledgers hold tx-scoped rows, so the counts compare as sets.
func entryHistoryWindowCovered(ctx context.Context, conn driver.Conn, name string, from, thru uint32, readTxLedgers uint64) error {
	const q = `SELECT uniqExact(ledger_seq) FROM stellar.ledgers WHERE ledger_seq BETWEEN ? AND ? AND tx_count > 0`
	cov := ECWindowCoverage{From: from, To: thru, ECCoveredTxLedgers: readTxLedgers}
	if err := conn.QueryRow(ctx, q, from, thru).Scan(&cov.TxLedgers); err != nil {
		return fmt.Errorf("clickhouse: %s window [%d,%d] tx-bearing ledgers: %w", name, from, thru, err)
	}
	if missing := cov.Missing(); missing > 0 {
		return fmt.Errorf("%w: %s [%d,%d] read entry changes for %d of %d tx-bearing ledgers",
			ErrEntryHistoryEntryChangeShortfall, name, from, thru, readTxLedgers, cov.TxLedgers)
	}
	return nil
}

// SponsorshipWatermark returns the highest ledger ch-entry-history has derived sponsorship events through (0 = never
// run, or watermark table not provisioned).
func SponsorshipWatermark(ctx context.Context, addr string) (uint32, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	return entryWatermarkOn(ctx, conn, sponsorshipWatermarkName)
}

// Its own name: a host whose entry_history row came from the retired payload derive must not skip those ledgers.
const sponsorshipWatermarkName = "sponsorship_events"

// ErrSponsorshipAheadOfEntryIndex reports a refused advance past the entry_index watermark: the account route proves
// in-force sponsorships against the index, so the events never cover a ledger the index does not.
var ErrSponsorshipAheadOfEntryIndex = errors.New("clickhouse: sponsorship window runs ahead of the entry_index watermark")

// entryWatermarkOn reads one named row of stellar.entry_history_watermark; max() collapses unmerged duplicates since a watermark only ever advances.
func entryWatermarkOn(ctx context.Context, conn driver.Conn, name string) (uint32, error) {
	const q = `SELECT max(thru_ledger) FROM stellar.entry_history_watermark WHERE name = ?`
	var wm uint32
	if err := conn.QueryRow(ctx, q, name).Scan(&wm); err != nil {
		if isSchemaAbsent(err) {
			observeWatermark(name, 0)
			return 0, nil
		}
		return 0, fmt.Errorf("clickhouse: %s watermark: %w", name, err)
	}
	observeWatermark(name, wm)
	return wm, nil
}

// observeWatermark exports a derived index's completeness watermark as last read; 0 (absent) pages, since every
// fast path gated on it is then serving its fallback.
func observeWatermark(name string, wm uint32) {
	obs.DerivedIndexWatermarkLedger.WithLabelValues(name).Set(float64(wm))
}

// SetSponsorshipWatermark records completion through `thru` only for a contiguous window the entry index already
// covers (an unproven advance would lose history for good); a window at or below the watermark is a no-op.
func SetSponsorshipWatermark(ctx context.Context, addr string, from, thru uint32) error {
	return advanceEntryWatermark(ctx, addr, sponsorshipWatermarkName, from, thru, func(ctx context.Context, conn driver.Conn) error {
		idx, err := entryWatermarkOn(ctx, conn, entryIndexWatermarkName)
		if err != nil {
			return err
		}
		if idx < thru {
			return fmt.Errorf("%w: [%d,%d] but the index stops at %d", ErrSponsorshipAheadOfEntryIndex, from, thru, idx)
		}
		return nil
	})
}

// advanceEntryWatermark is the shared advance: range sanity, no skipped prefix, lake contiguity, then the caller's proof.
func advanceEntryWatermark(ctx context.Context, addr, name string, from, thru uint32, proof func(context.Context, driver.Conn) error) error {
	if from == 0 || thru < from {
		return fmt.Errorf("clickhouse: %s watermark window [%d,%d] is not a ledger range (genesis is ledger 1)", name, from, thru)
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	wm, err := entryWatermarkOn(ctx, conn, name)
	if err != nil {
		return err
	}
	if thru <= wm {
		return nil
	}
	if wm > 0 && from > wm+1 {
		return fmt.Errorf("%w: %s [%d,%d] starts above watermark+1 (%d)", ErrEntryHistorySkippedPrefix, name, from, thru, wm+1)
	}
	present, err := windowLedgersPresent(ctx, conn, from, thru)
	if err != nil {
		return fmt.Errorf("clickhouse: %s window [%d,%d] contiguity: %w", name, from, thru, err)
	}
	if want := uint64(thru-from) + 1; present != want {
		return fmt.Errorf("%w: %s [%d,%d] holds %d of %d ledgers", ErrEntryHistoryHole, name, from, thru, present, want)
	}
	if err := proof(ctx, conn); err != nil {
		return err
	}
	const q = `INSERT INTO stellar.entry_history_watermark (name, thru_ledger) VALUES (?, ?)`
	if err := conn.Exec(ctx, q, name, thru); err != nil {
		return fmt.Errorf("clickhouse: set %s watermark %d: %w", name, thru, err)
	}
	return nil
}
