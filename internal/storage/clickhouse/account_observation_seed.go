package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// AccountObservationSeed is the lake-derived snapshot of one account's
// latest AccountEntry, shaped for seeding the served tier's
// account_observations hypertable (ADR-0021). Motivation: the live
// AccountEntry observer only writes a row when the account CHANGES
// after the observer started — a dormant SDF reserve account never
// emits a LedgerEntryChange, so the LCM reserve-balance reader would
// fall back to the operator-static config forever. Seeding from
// ledger_entries_current (latest entry per key, ReplacingMergeTree)
// gives the live reader its bootstrap row at the account's true
// last-modified ledger; any later live observation supersedes it via
// the reader's at-or-before-ledger ordering.
type AccountObservationSeed struct {
	// Found is false when the lake holds no AccountEntry row for the
	// account — never created, or dormant since before the lake's
	// entry-change capture began (run `stellarindex-ops state-snapshot`
	// with the account-state scope to fill the dormant tail from a
	// history-archive checkpoint first).
	Found bool

	// Removed is true when the latest change merged the account away.
	Removed bool

	AccountID  string
	LedgerSeq  uint32    // the entry's LastModifiedLedgerSeq (from the change row)
	CloseTime  time.Time // close time of that ledger (UTC)
	Balance    int64     // native XLM, stroops (from the decoded AccountEntry)
	HomeDomain string
	Flags      uint32
	SeqNum     int64
}

// LatestAccountEntrySeed reads the latest AccountEntry for one account
// from the current-state projection and decodes the fields the
// account_observations hypertable carries. Returns Found=false (no
// error) for an unknown account; a corrupt stored entry_xdr IS an
// error here (unlike the explorer's degrade-to-empty policy) because
// the caller is about to persist the row into the served tier —
// silently seeding nothing would masquerade as "account dormant".
func (r *ExplorerReader) LatestAccountEntrySeed(ctx context.Context, account string) (AccountObservationSeed, error) {
	const q = `SELECT entry_xdr, change_type, ledger_seq, close_time
		FROM stellar.ledger_entries_current FINAL
		WHERE account_id = ? AND entry_type = 'account'
		LIMIT 1`
	var (
		entryXDR, changeType string
		ledgerSeq            uint32
		closeTime            time.Time
	)
	row := r.conn.QueryRow(ctx, q, account)
	if err := row.Scan(&entryXDR, &changeType, &ledgerSeq, &closeTime); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AccountObservationSeed{}, nil
		}
		return AccountObservationSeed{}, fmt.Errorf("clickhouse: account entry seed %s: %w", account, err)
	}
	return accountSeedFromRow(account, entryXDR, changeType, ledgerSeq, closeTime)
}

// accountSeedFromRow decodes one ledger_entries_current row into an
// AccountObservationSeed. Split from the query for testability.
func accountSeedFromRow(account, entryXDR, changeType string, ledgerSeq uint32, closeTime time.Time) (AccountObservationSeed, error) {
	if changeType == "removed" {
		return AccountObservationSeed{Found: true, Removed: true, AccountID: account, LedgerSeq: ledgerSeq, CloseTime: closeTime.UTC()}, nil
	}
	var le xdr.LedgerEntry
	if err := xdr.SafeUnmarshalBase64(entryXDR, &le); err != nil {
		return AccountObservationSeed{}, fmt.Errorf("clickhouse: decode account entry for %s at ledger %d: %w", account, ledgerSeq, err)
	}
	acc, ok := le.Data.GetAccount()
	if !ok {
		return AccountObservationSeed{}, fmt.Errorf("clickhouse: entry for %s at ledger %d is %s, not an AccountEntry", account, ledgerSeq, le.Data.Type.String())
	}
	return AccountObservationSeed{
		Found:      true,
		AccountID:  account,
		LedgerSeq:  ledgerSeq,
		CloseTime:  closeTime.UTC(),
		Balance:    int64(acc.Balance),
		HomeDomain: string(acc.HomeDomain),
		Flags:      uint32(acc.Flags),
		SeqNum:     int64(acc.SeqNum),
	}, nil
}
