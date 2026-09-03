package clickhouse

import (
	"hash/crc32"
	"math"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// accountEntry builds a real AccountEntry LedgerEntry at the given
// last-modified ledger and native balance.
func accountEntry(t *testing.T, addr string, lastModified uint32, balance int64) *xdr.LedgerEntry {
	t.Helper()
	return &xdr.LedgerEntry{
		LastModifiedLedgerSeq: xdr.Uint32(lastModified),
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeAccount,
			Account: &xdr.AccountEntry{
				AccountId: xdr.MustAddress(addr),
				Balance:   xdr.Int64(balance),
			},
		},
	}
}

// trustlineEntry builds a real TrustLineEntry LedgerEntry.
func trustlineEntry(t *testing.T, holder, code, issuer string, lastModified uint32, balance int64) *xdr.LedgerEntry {
	t.Helper()
	return &xdr.LedgerEntry{
		LastModifiedLedgerSeq: xdr.Uint32(lastModified),
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeTrustline,
			TrustLine: &xdr.TrustLineEntry{
				AccountId: xdr.MustAddress(holder),
				Asset:     xdr.MustNewCreditAsset(code, issuer).ToTrustLineAsset(),
				Balance:   xdr.Int64(balance),
			},
		},
	}
}

// TestSnapshotEntryRow_ChangeIndexIsPerKeyUniqueWithinALedger is the
// regression test for the single worst defect this function has had.
//
// The table is ReplacingMergeTree ORDER BY (ledger_seq, tx_hash, op_index,
// change_index), and snapshot rows all share tx_hash="" and op_index=-1. With
// a constant ChangeIndex, every snapshot entry that shares a ledger_seq
// collapses to ONE arbitrary survivor at merge time — the 2026-07-03 site
// audit measured over 55% of the 48M-entry Phase-C snapshot already destroyed,
// taking account-state, trustline, supply and wasm reads down with it.
//
// crc32(key_xdr) restores per-key uniqueness AND keeps a re-run idempotent
// (the same key re-derives the same index, so it replaces rather than
// duplicates). This test pins both halves.
func TestSnapshotEntryRow_ChangeIndexIsPerKeyUniqueWithinALedger(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	const ledger = 60_000_000

	// Three DIFFERENT keys modified in the SAME ledger — the exact shape that
	// collapsed.
	entries := []*xdr.LedgerEntry{
		accountEntry(t, testSource, ledger, 10_000_000),
		accountEntry(t, testDest, ledger, 20_000_000),
		trustlineEntry(t, testTrustor, "USDC", testIssuer, ledger, 30_000_000),
	}

	seen := make(map[uint32]string, len(entries))
	for _, e := range entries {
		row, ok := SnapshotEntryRow(e, at)
		if !ok {
			t.Fatalf("SnapshotEntryRow refused a well-formed entry: %+v", e)
		}
		if row.LedgerSeq != ledger {
			t.Fatalf("LedgerSeq = %d, want %d (the entry's LastModifiedLedgerSeq)", row.LedgerSeq, ledger)
		}
		if prev, clash := seen[row.ChangeIndex]; clash {
			t.Fatalf("ChangeIndex %d collides between key %q and key %q — same (ledger_seq, tx_hash, "+
				"op_index, change_index) means one row silently REPLACES the other at merge time",
				row.ChangeIndex, prev, row.KeyXDR)
		}
		seen[row.ChangeIndex] = row.KeyXDR

		// It is crc32 of the key, which is what makes a re-run idempotent
		// rather than duplicating.
		if want := crc32.ChecksumIEEE([]byte(row.KeyXDR)); row.ChangeIndex != want {
			t.Errorf("ChangeIndex = %d, want crc32(key_xdr) = %d", row.ChangeIndex, want)
		}
	}

	// Re-deriving the same entry must produce a byte-identical row, so an
	// interrupted backfill can simply be re-run over the same range.
	first, _ := SnapshotEntryRow(entries[0], at)
	again, _ := SnapshotEntryRow(entries[0], at)
	if first != again {
		t.Errorf("re-deriving the same entry produced a different row:\n%+v\n%+v", first, again)
	}
}

// TestSnapshotEntryRow_StampsTheTopOfSpaceIntraLedgerSentinel — a snapshot row
// is the authoritative reconstructed FINAL state of its entry as of
// LastModifiedLedgerSeq, so it must sit at the END of that ledger's
// intra-ledger order. ledger_entries_current resolves versions by
// (ledger_seq, intra_ledger_seq); a snapshot stamped anywhere below the top
// can be overwritten by a live per-ledger change for the same ledger, which
// silently reinstates a MID-ledger balance as current state.
//
// A re-snapshot stamps the SAME sentinel, which keeps it corrective (equal
// version, later part wins) rather than inert.
func TestSnapshotEntryRow_StampsTheTopOfSpaceIntraLedgerSentinel(t *testing.T) {
	row, ok := SnapshotEntryRow(accountEntry(t, testSource, 42, 7), time.Unix(0, 0).UTC())
	if !ok {
		t.Fatal("SnapshotEntryRow refused a well-formed account entry")
	}
	if row.IntraLedgerSeq != uint32(math.MaxUint32) {
		t.Errorf("IntraLedgerSeq = %d, want math.MaxUint32 (%d) — a lower value lets a live "+
			"same-ledger change overwrite the authoritative snapshot",
			row.IntraLedgerSeq, uint32(math.MaxUint32))
	}
	if seedIntraLedgerSeq != uint32(math.MaxUint32) {
		t.Errorf("seedIntraLedgerSeq = %d, want math.MaxUint32", seedIntraLedgerSeq)
	}
	// The other snapshot-shape constants the ORDER BY key depends on.
	if row.ChangeType != "state" {
		t.Errorf("ChangeType = %q, want \"state\"", row.ChangeType)
	}
	if row.OpIndex != -1 {
		t.Errorf("OpIndex = %d, want -1 (fee-meta / tx-level sentinel)", row.OpIndex)
	}
	if row.TxHash != "" {
		t.Errorf("TxHash = %q, want empty — a snapshot belongs to no transaction", row.TxHash)
	}
}

// TestSnapshotEntryRow_PopulatesTheQueryableColumns — owner/asset/balance are
// denormalised columns the account-state, asset-holder and supply readers
// query directly instead of decoding entry_xdr. A snapshot that left them
// empty would be invisible to every one of those reads while entry_xdr
// suggested the data was captured.
func TestSnapshotEntryRow_PopulatesTheQueryableColumns(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()

	acct, ok := SnapshotEntryRow(accountEntry(t, testSource, 100, 987_654_321), at)
	if !ok {
		t.Fatal("SnapshotEntryRow refused an account entry")
	}
	if acct.EntryType != "account" {
		t.Errorf("EntryType = %q, want \"account\"", acct.EntryType)
	}
	if acct.AccountID != testSource {
		t.Errorf("AccountID = %q, want %q", acct.AccountID, testSource)
	}
	if acct.Asset != "" {
		t.Errorf("Asset = %q, want empty for an account entry", acct.Asset)
	}
	if acct.Balance != 987_654_321 {
		t.Errorf("Balance = %d, want 987654321 stroops", acct.Balance)
	}
	if acct.CloseTime != at {
		t.Errorf("CloseTime = %s, want %s", acct.CloseTime, at)
	}

	tl, ok := SnapshotEntryRow(trustlineEntry(t, testTrustor, "USDC", testIssuer, 101, 555), at)
	if !ok {
		t.Fatal("SnapshotEntryRow refused a trustline entry")
	}
	if tl.EntryType != "trustline" {
		t.Errorf("EntryType = %q, want \"trustline\"", tl.EntryType)
	}
	if tl.AccountID != testTrustor {
		t.Errorf("AccountID = %q, want the HOLDER %q", tl.AccountID, testTrustor)
	}
	if want := "USDC-" + testIssuer; tl.Asset != want {
		t.Errorf("Asset = %q, want the canonical %q", tl.Asset, want)
	}
	if tl.Balance != 555 {
		t.Errorf("Balance = %d, want 555", tl.Balance)
	}

	// key_xdr and entry_xdr must round-trip: the readers' key_xdr lookups
	// match on this exact text, and entry_xdr is the archived truth.
	var key xdr.LedgerKey
	if err := xdr.SafeUnmarshalBase64(tl.KeyXDR, &key); err != nil {
		t.Fatalf("key_xdr does not decode: %v", err)
	}
	if key.Type != xdr.LedgerEntryTypeTrustline {
		t.Errorf("decoded key type = %v, want Trustline", key.Type)
	}
	var entry xdr.LedgerEntry
	if err := xdr.SafeUnmarshalBase64(tl.EntryXDR, &entry); err != nil {
		t.Fatalf("entry_xdr does not decode: %v", err)
	}
	if entry.Data.TrustLine.Balance != 555 {
		t.Errorf("round-tripped entry balance = %d, want 555", entry.Data.TrustLine.Balance)
	}
}

// TestSnapshotEntryRow_RefusesAnEntryWithNoDerivableKey — ok=false means "skip
// this one entry", never "abort the backfill". The reachable case is a
// LedgerEntryType this build's SDK does not know (a future protocol adding an
// entry type ahead of our pinned go-stellar-sdk): xdr.LedgerEntryData.LedgerKey
// returns "unknown ledger entry type N" for it.
//
// Returning a zero row with ok=true instead would insert a KEYLESS row into
// ledger_entry_changes — invisible to every key_xdr lookup, but counted by the
// backfill as written, so the snapshot would report full coverage of entries it
// had silently dropped.
func TestSnapshotEntryRow_RefusesAnEntryWithNoDerivableKey(t *testing.T) {
	unknown := &xdr.LedgerEntry{
		LastModifiedLedgerSeq: 1,
		Data:                  xdr.LedgerEntryData{Type: xdr.LedgerEntryType(9999)},
	}
	row, ok := SnapshotEntryRow(unknown, time.Unix(0, 0).UTC())
	if ok {
		t.Fatalf("SnapshotEntryRow accepted an entry with no derivable LedgerKey: %+v", row)
	}
	if row != (LedgerEntryChangeRow{}) {
		t.Errorf("row = %+v, want the zero value on refusal", row)
	}
}
