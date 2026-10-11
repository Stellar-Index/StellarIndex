package accounts

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// Errors returned by extractObservation. Callers classify via
// errors.Is.
var (
	// ErrNotAccountEntry means the change touched a non-AccountEntry. Matches pre-filters
	// these, so Decode should never see one; the guard stays anyway.
	ErrNotAccountEntry = errors.New("accounts: change is not an AccountEntry")
)

// accountIDFromChange returns the G-strkey of the AccountEntry a change references, for any
// change variant, or [ErrNotAccountEntry]. It is the cheap pre-filter for [Observer.Matches],
// so it extracts no balance or home domain.
func accountIDFromChange(change xdr.LedgerEntryChange) (string, error) {
	switch change.Type {
	case xdr.LedgerEntryChangeTypeLedgerEntryCreated:
		if change.Created == nil || change.Created.Data.Type != xdr.LedgerEntryTypeAccount {
			return "", ErrNotAccountEntry
		}
		return strkeyFromAccountID(change.Created.Data.Account.AccountId)
	case xdr.LedgerEntryChangeTypeLedgerEntryUpdated:
		if change.Updated == nil || change.Updated.Data.Type != xdr.LedgerEntryTypeAccount {
			return "", ErrNotAccountEntry
		}
		return strkeyFromAccountID(change.Updated.Data.Account.AccountId)
	case xdr.LedgerEntryChangeTypeLedgerEntryRestored:
		if change.Restored == nil || change.Restored.Data.Type != xdr.LedgerEntryTypeAccount {
			return "", ErrNotAccountEntry
		}
		return strkeyFromAccountID(change.Restored.Data.Account.AccountId)
	case xdr.LedgerEntryChangeTypeLedgerEntryRemoved:
		if change.Removed == nil || change.Removed.Type != xdr.LedgerEntryTypeAccount {
			return "", ErrNotAccountEntry
		}
		return strkeyFromAccountID(change.Removed.Account.AccountId)
	}
	return "", fmt.Errorf("%w: unknown change type %d", ErrNotAccountEntry, change.Type)
}

// extractObservation builds an [Observation] from a change on a watched AccountEntry.
// Removed changes yield IsRemoval=true with zeroed balance/home_domain, meaning the account no
// longer exists at that ledger. The caller filters by watched set; a mismatch errors rather
// than being silently coerced.
func extractObservation(change xdr.LedgerEntryChange) (Observation, error) {
	switch change.Type {
	case xdr.LedgerEntryChangeTypeLedgerEntryCreated:
		return obsFromAccountEntry(change.Created.Data.Account, false)
	case xdr.LedgerEntryChangeTypeLedgerEntryUpdated:
		return obsFromAccountEntry(change.Updated.Data.Account, false)
	case xdr.LedgerEntryChangeTypeLedgerEntryRestored:
		return obsFromAccountEntry(change.Restored.Data.Account, false)
	case xdr.LedgerEntryChangeTypeLedgerEntryRemoved:
		// Removed carries only the LedgerKey (no AccountEntry body).
		// Use the key's AccountId to populate AccountID; balance +
		// home_domain are unknown post-removal, hence zeroed.
		id, err := strkeyFromAccountID(change.Removed.Account.AccountId)
		if err != nil {
			return Observation{}, err
		}
		return Observation{
			AccountID: id,
			Balance:   big.NewInt(0),
			IsRemoval: true,
		}, nil
	}
	return Observation{}, fmt.Errorf("%w: unknown change type %d", ErrNotAccountEntry, change.Type)
}

// obsFromAccountEntry turns the post-change AccountEntry into an
// [Observation]. AccountID is derived from the entry's own
// AccountId field.
func obsFromAccountEntry(entry *xdr.AccountEntry, removal bool) (Observation, error) {
	if entry == nil {
		return Observation{}, errors.New("accounts: nil AccountEntry passed to obsFromAccountEntry")
	}
	id, err := strkeyFromAccountID(entry.AccountId)
	if err != nil {
		return Observation{}, err
	}
	return Observation{
		AccountID:  id,
		Balance:    big.NewInt(int64(entry.Balance)),
		HomeDomain: string(entry.HomeDomain),
		Flags:      uint32(entry.Flags),
		SeqNum:     int64(entry.SeqNum),
		IsRemoval:  removal,
	}, nil
}

// strkeyFromAccountID encodes an xdr.AccountId to its G-strkey
// representation. Fails when the account is not Ed25519 (we don't
// observe muxed accounts at the AccountEntry level — the entry's
// AccountId is always an Ed25519 PK).
func strkeyFromAccountID(aid xdr.AccountId) (string, error) {
	pk, ok := aid.GetEd25519()
	if !ok {
		return "", fmt.Errorf("accounts: AccountId is not Ed25519 (type=%d)", aid.Type)
	}
	return strkey.Encode(strkey.VersionByteAccountID, pk[:])
}
