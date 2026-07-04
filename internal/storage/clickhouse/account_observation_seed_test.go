package clickhouse

import (
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

const seedTestAccount = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// mustAccountEntryXDR builds a base64 LedgerEntry holding an
// AccountEntry with the supplied fields — the shape
// ledger_entries_current stores in entry_xdr.
func mustAccountEntryXDR(t *testing.T, account string, balance int64, seqNum int64, flags uint32, homeDomain string) string {
	t.Helper()
	var accountID xdr.AccountId
	if err := accountID.SetAddress(account); err != nil {
		t.Fatalf("SetAddress: %v", err)
	}
	le := xdr.LedgerEntry{
		LastModifiedLedgerSeq: 123,
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeAccount,
			Account: &xdr.AccountEntry{
				AccountId:  accountID,
				Balance:    xdr.Int64(balance),
				SeqNum:     xdr.SequenceNumber(seqNum),
				Flags:      xdr.Uint32(flags),
				HomeDomain: xdr.String32(homeDomain),
				Thresholds: xdr.Thresholds{1, 0, 0, 0},
			},
		},
	}
	out, err := xdr.MarshalBase64(le)
	if err != nil {
		t.Fatalf("MarshalBase64: %v", err)
	}
	return out
}

// TestAccountSeedFromRow_Decodes — the seed decode path used by
// `stellarindex-ops supply seed-observations` (ADR-0021 dormant-
// account bootstrap) must carry balance / home_domain / flags /
// seq_num faithfully from the stored AccountEntry XDR.
func TestAccountSeedFromRow_Decodes(t *testing.T) {
	closeTime := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	entry := mustAccountEntryXDR(t, seedTestAccount, 5_000_000_000, 42, 0x1, "stellar.org")

	seed, err := accountSeedFromRow(seedTestAccount, entry, "updated", 62_000_000, closeTime)
	if err != nil {
		t.Fatalf("accountSeedFromRow: %v", err)
	}
	if !seed.Found || seed.Removed {
		t.Fatalf("Found=%v Removed=%v, want Found=true Removed=false", seed.Found, seed.Removed)
	}
	if seed.Balance != 5_000_000_000 {
		t.Errorf("Balance = %d, want 5000000000", seed.Balance)
	}
	if seed.SeqNum != 42 {
		t.Errorf("SeqNum = %d, want 42", seed.SeqNum)
	}
	if seed.Flags != 1 {
		t.Errorf("Flags = %d, want 1", seed.Flags)
	}
	if seed.HomeDomain != "stellar.org" {
		t.Errorf("HomeDomain = %q, want stellar.org", seed.HomeDomain)
	}
	if seed.LedgerSeq != 62_000_000 {
		t.Errorf("LedgerSeq = %d, want 62000000", seed.LedgerSeq)
	}
	if !seed.CloseTime.Equal(closeTime) {
		t.Errorf("CloseTime = %v, want %v", seed.CloseTime, closeTime)
	}
}

// TestAccountSeedFromRow_Removed — a trailing 'removed' change means
// the account merged away; the seeder must see Removed=true and NOT
// fabricate balances (entry_xdr may be empty for removals).
func TestAccountSeedFromRow_Removed(t *testing.T) {
	seed, err := accountSeedFromRow(seedTestAccount, "", "removed", 62_000_001, time.Now().UTC())
	if err != nil {
		t.Fatalf("accountSeedFromRow: %v", err)
	}
	if !seed.Found || !seed.Removed {
		t.Fatalf("Found=%v Removed=%v, want both true", seed.Found, seed.Removed)
	}
}

// TestAccountSeedFromRow_CorruptXDRErrors — unlike the explorer's
// degrade-to-empty policy, the seeder persists into the served tier,
// so a corrupt entry must ERROR rather than silently seed nothing.
func TestAccountSeedFromRow_CorruptXDRErrors(t *testing.T) {
	if _, err := accountSeedFromRow(seedTestAccount, "not-base64-xdr!", "updated", 1, time.Now().UTC()); err == nil {
		t.Fatal("expected error for corrupt entry_xdr")
	}
}

// TestAccountSeedFromRow_WrongEntryTypeErrors — a trustline entry
// under an account_id filter would indicate a query bug; refuse it.
func TestAccountSeedFromRow_WrongEntryTypeErrors(t *testing.T) {
	tl := xdr.LedgerEntry{
		LastModifiedLedgerSeq: 1,
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeTtl,
			Ttl:  &xdr.TtlEntry{KeyHash: xdr.Hash{}, LiveUntilLedgerSeq: 1},
		},
	}
	raw, err := xdr.MarshalBase64(tl)
	if err != nil {
		t.Fatalf("MarshalBase64: %v", err)
	}
	if _, err := accountSeedFromRow(seedTestAccount, raw, "updated", 1, time.Now().UTC()); err == nil {
		t.Fatal("expected error for non-account entry type")
	}
}
