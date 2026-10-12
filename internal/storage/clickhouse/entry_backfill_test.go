package clickhouse

import (
	"hash/crc32"
	"math"
	"strings"
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

// TestSnapshotEntryRow_ChangeIndexIsPerKeyUniqueWithinALedger: snapshot rows share tx_hash and op_index, so
// ChangeIndex must be per-key unique (crc32(key_xdr)) or the ReplacingMergeTree collapses them. It must also
// be idempotent on re-run.
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

// TestSnapshotEntryRow_StampsTheTopOfSpaceIntraLedgerSentinel: a snapshot is the FINAL state at its ledger,
// so it must sit at the END of the intra-ledger order, or a live change in that ledger overwrites it in
// ledger_entries_current. A re-snapshot stamps the same sentinel, staying corrective (later part wins).
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

// TestSnapshotEntryRow_PopulatesTheQueryableColumns: owner/asset/balance are denormalised
// columns the account-state, asset-holder and supply readers query instead of decoding entry_xdr.
// A snapshot leaving them empty is invisible to those reads while entry_xdr looks captured.
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

// TestResolveChangeIndexCollisions_ReassignsCollidingRows: crc32(key) is 32 bits, so different keys in one
// ledger_seq/tx_hash/op_index group can collide on ChangeIndex; unresolved, one row silently replaces the
// other.
func TestResolveChangeIndexCollisions_ReassignsCollidingRows(t *testing.T) {
	rows := []LedgerEntryChangeRow{
		{LedgerSeq: 60_000_000, ChangeIndex: 42, KeyXDR: "keyA"},
		{LedgerSeq: 60_000_000, ChangeIndex: 42, KeyXDR: "keyB"},
		{LedgerSeq: 60_000_000, ChangeIndex: 42, KeyXDR: "keyC"},
		// A different ledger_seq sharing the same colliding value must be
		// left alone — collision is scoped to the ORDER BY group, not global.
		{LedgerSeq: 60_000_001, ChangeIndex: 42, KeyXDR: "keyD"},
	}
	if err := resolveChangeIndexCollisions(rows); err != nil {
		t.Fatalf("resolveChangeIndexCollisions: %v", err)
	}

	seenInLedger := map[uint32]map[uint32]string{}
	for _, r := range rows {
		if seenInLedger[r.LedgerSeq] == nil {
			seenInLedger[r.LedgerSeq] = map[uint32]string{}
		}
		if prev, clash := seenInLedger[r.LedgerSeq][r.ChangeIndex]; clash {
			t.Fatalf("ledger_seq %d: ChangeIndex %d still collides between %q and %q after resolution",
				r.LedgerSeq, r.ChangeIndex, prev, r.KeyXDR)
		}
		seenInLedger[r.LedgerSeq][r.ChangeIndex] = r.KeyXDR
	}
	if got := rows[3].ChangeIndex; got != 42 {
		t.Errorf("keyD (lone occupant of its own ledger_seq) ChangeIndex = %d, want unchanged 42", got)
	}
}

// TestResolveChangeIndexCollisions_DeterministicRegardlessOfInputOrder pins idempotency: a resumed
// backfill re-derives the same rows in any order, so resolution must depend only on the SET of colliding
// keys, or a resume assigns different indexes and duplicates instead of replacing.
func TestResolveChangeIndexCollisions_DeterministicRegardlessOfInputOrder(t *testing.T) {
	base := []LedgerEntryChangeRow{
		{LedgerSeq: 70_000_000, ChangeIndex: 7, KeyXDR: "AAA"},
		{LedgerSeq: 70_000_000, ChangeIndex: 7, KeyXDR: "BBB"},
		{LedgerSeq: 70_000_000, ChangeIndex: 7, KeyXDR: "CCC"},
	}

	forward := append([]LedgerEntryChangeRow(nil), base...)
	if err := resolveChangeIndexCollisions(forward); err != nil {
		t.Fatalf("resolveChangeIndexCollisions (forward): %v", err)
	}

	reversed := []LedgerEntryChangeRow{base[2], base[1], base[0]}
	if err := resolveChangeIndexCollisions(reversed); err != nil {
		t.Fatalf("resolveChangeIndexCollisions (reversed): %v", err)
	}

	want := make(map[string]uint32, len(forward))
	for _, r := range forward {
		want[r.KeyXDR] = r.ChangeIndex
	}
	for _, r := range reversed {
		if want[r.KeyXDR] != r.ChangeIndex {
			t.Errorf("key %q resolved to ChangeIndex %d in forward order, %d in reverse order — "+
				"a resumed backfill would duplicate this row instead of replacing it",
				r.KeyXDR, want[r.KeyXDR], r.ChangeIndex)
		}
	}
}

// TestSnapshotEntryRow_RefusesAnEntryWithNoDerivableKey: ok=false means skip this entry, never abort the
// backfill (e.g. a LedgerEntryType the pinned SDK lacks). A keyless row with ok=true would be counted as
// written yet be invisible to key_xdr lookups.
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

// Tests for ReplacingMergeTree reads: a read with neither FINAL nor a dedup over-counts un-merged duplicates.
// StreamEntryChanges and CountOpScopedEntryChanges dial a real connection via openRead, so stubConn cannot
// drive them; their queries are package-level consts so the SQL text is testable.

func TestStreamEntryChangesQuery_UsesFinal(t *testing.T) {
	if !strings.Contains(streamEntryChangesQuery, "stellar.ledger_entry_changes FINAL") {
		t.Fatalf("streamEntryChangesQuery = %q, want `stellar.ledger_entry_changes FINAL`", streamEntryChangesQuery)
	}
	// The per-op grouping order callers rely on (see StreamEntryChanges' doc
	// comment) must survive the fix unchanged.
	if !strings.Contains(streamEntryChangesQuery, "ORDER BY ledger_seq, tx_hash, op_index, change_index") {
		t.Fatalf("streamEntryChangesQuery = %q, want the original per-op ORDER BY preserved", streamEntryChangesQuery)
	}
}

func TestCountOpScopedEntryChangesQuery_UsesUniqExact(t *testing.T) {
	if !strings.Contains(countOpScopedEntryChangesQuery, "uniqExact(ledger_seq, tx_hash, op_index, change_index)") {
		t.Fatalf("countOpScopedEntryChangesQuery = %q, want a uniqExact(...) primary-key dedup, not count()", countOpScopedEntryChangesQuery)
	}
	if strings.Contains(countOpScopedEntryChangesQuery, "SELECT count()") {
		t.Fatalf("countOpScopedEntryChangesQuery = %q, must not use bare count() (over-counts un-merged duplicate parts)", countOpScopedEntryChangesQuery)
	}
}
