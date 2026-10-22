package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// AccountObservationSeed is the lake-derived latest AccountEntry of one account, seeding account_observations (ADR-0021); any later live observation supersedes it.
type AccountObservationSeed struct {
	// Found is false when the lake holds no AccountEntry row for the account:
	// never created, or dormant since before entry-change capture began (fill
	// the tail first with `stellarindex-ops state-snapshot`, account-state scope).
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

// LatestAccountEntrySeed reads the latest AccountEntry from the current-state projection; Found=false for an unknown account, a corrupt entry_xdr is an error.
func (r *ExplorerReader) LatestAccountEntrySeed(ctx context.Context, account string) (AccountObservationSeed, error) {
	const q = `SELECT entry_xdr, change_type, ledger_seq, close_time
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'account' AND key_xdr = ?
		LIMIT 1`
	key, err := accountKeyXDR(account)
	if err != nil {
		return AccountObservationSeed{}, err
	}
	var (
		entryXDR, changeType string
		ledgerSeq            uint32
		closeTime            time.Time
	)
	row := r.conn.QueryRow(ctx, q, key)
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
