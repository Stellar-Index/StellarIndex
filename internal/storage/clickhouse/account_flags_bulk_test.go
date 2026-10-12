package clickhouse

import (
	"context"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Real lake fixtures: both entries were read off the lake with bounded single-ledger SELECTs and are the two
// shapes the residue actually contains.

// residueGStrkey / residueEntryXDR: GB6DFVQB... (qstocks.org), merged away at ledger 62,822,338, flags =
// 10 (AUTH_REVOCABLE | AUTH_CLAWBACK), home_domain "qstocks.org". Modern shape: intra_ledger_seq
// populated.
const (
	residueGStrkey   = "GB6DFVQBQZX2Q747AQFAPYJ7NQI2H5WTF77AYN3PPZTTLGSO7FDETQQQ"
	residueRemovedAt = uint32(62822338)
	residueEntryXDR  = "A76XwgAAAAAAAAAAfDLWAYZvqH+fBAoH4T9sEaP20y/+DDdvfmc1mk75RkkAAAAAAS5tGQNQoIwAAAAWAAAAAAAAAAAAAAAKAAAAC3FzdG9ja3Mub3JnAAEAAAAAAAAAAAAAAQAAAAAAAAAAAAAAAAAAAAAAAAACAAAAAAAAAAAAAAAAAAAAAwAAAAADvpfCAAAAAGoche8AAAAA"
)

// legacyGStrkey / legacyEntryXDR: GA52ZQ7K..., merged away at ledger 38,137,083. Legacy shape: every row has
// intra_ledger_seq = 0, so the argMax tiebreak falls through to change_index. flags = 1, no home_domain.
const (
	legacyGStrkey   = "GA52ZQ7KV66MA3WATU4QD3CIZXZOI3RAOQF3Y7IADNHR6LSGFY7TUX55"
	legacyRemovedAt = uint32(38137083)
	legacyEntryXDR  = "AkXs+wAAAAAAAAAAO6zD6q+8wG7AnTkB7EjN8uRuIHQLvH0AG08fLkYuPzoAAAAAAq6fyAJF660AAAABAAAAAgAAAAAAAAABAAAAAAECAgIAAAACAAAAAD75+Q8tRcKY2v7ycwmmNp3dF2Hwl1kjkmow666NQ6RRAAAAAgAAAADpr9iSgM/C0GookE6opYjRd4gx3mv9KwPYo+3k3ZCLoAAAAAEAAAABAAAAAAAAAAAAAAAAAAAAAAAAAAIAAAAAAAAAAAAAAAIAAAAAAAAAAAAAAAAAAAAA"
)

// liveGStrkey / liveEntryXDR reuse the qstocks entry as a stand-in for a
// LIVE AccountEntry — the bytes are a real AccountEntry either way; only the
// table it is read from decides whether the reading is current.
const liveGStrkey = residueGStrkey

func mustAccountLedgerKey(t *testing.T, g string) string {
	t.Helper()
	k, err := accountKeyXDR(g)
	if err != nil {
		t.Fatalf("accountKeyXDR(%s): %v", g, err)
	}
	return k
}

// Query-shape classifiers for the two reads the last-known path makes.
func isRemovalLedgerRead(q string) bool {
	return strings.Contains(q, "stellar.ledger_entries_current") &&
		strings.Contains(q, "change_type = 'removed'")
}

func isLiveEntryRead(q string) bool {
	return strings.Contains(q, "stellar.ledger_entries_current") &&
		strings.Contains(q, "change_type != 'removed'")
}

func isPreImageRead(q string) bool {
	return strings.Contains(q, "stellar.ledger_entry_changes")
}

// TestRemovedAccountsLastKnownAuthFlags_ResolvesPreImageWithoutTheDeadDomain pins that a merged issuer's
// flags come from the removal-ledger pre-image, labelled historical, with the dead home_domain dropped (mask
// 10).
func TestRemovedAccountsLastKnownAuthFlags_ResolvesPreImageWithoutTheDeadDomain(t *testing.T) {
	key := mustAccountLedgerKey(t, residueGStrkey)
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		switch {
		case isRemovalLedgerRead(q):
			return &stubRows{data: [][]any{{key, residueRemovedAt}}}, nil
		case isPreImageRead(q):
			return &stubRows{data: [][]any{{key, residueEntryXDR, residueRemovedAt}}}, nil
		}
		t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}}
	r := &ExplorerReader{conn: conn}

	got, err := r.RemovedAccountsLastKnownAuthFlags(context.Background(), []string{residueGStrkey})
	if err != nil {
		t.Fatalf("RemovedAccountsLastKnownAuthFlags: %v", err)
	}
	f, ok := got[residueGStrkey]
	if !ok {
		t.Fatalf("issuer %s unresolved; got %#v", residueGStrkey, got)
	}
	if f.Required || !f.Revocable || f.Immutable || !f.Clawback {
		t.Errorf("flags = required:%v revocable:%v immutable:%v clawback:%v, want the fixture's mask 10 (revocable+clawback only)",
			f.Required, f.Revocable, f.Immutable, f.Clawback)
	}
	if f.Source != AuthFlagsSourceLastKnownBeforeRemoval {
		t.Errorf("source = %q, want %q — an unlabelled reading is served as the issuer's CURRENT policy",
			f.Source, AuthFlagsSourceLastKnownBeforeRemoval)
	}
	if f.AsOfLedger != residueRemovedAt {
		t.Errorf("as-of ledger = %d, want %d (the removal ledger)", f.AsOfLedger, residueRemovedAt)
	}
	// The entry really does carry "qstocks.org"; a merged account must not
	// keep advertising a domain nothing can check.
	if f.HomeDomain != "" {
		t.Errorf("home_domain = %q, want empty — a merged account's self-declared identity is not servable", f.HomeDomain)
	}
}

// TestRemovedAccountsLastKnownAuthFlags_LegacyLedgerResolvesByChangeIndex: on legacy rows intra_ledger_seq is
// 0, so change_index is the only tiebreak; the real pre-image must decode to AUTH_REQUIRED alone (mask 1), no
// domain.
func TestRemovedAccountsLastKnownAuthFlags_LegacyLedgerResolvesByChangeIndex(t *testing.T) {
	key := mustAccountLedgerKey(t, legacyGStrkey)
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		switch {
		case isRemovalLedgerRead(q):
			return &stubRows{data: [][]any{{key, legacyRemovedAt}}}, nil
		case isPreImageRead(q):
			return &stubRows{data: [][]any{{key, legacyEntryXDR, legacyRemovedAt}}}, nil
		}
		t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}}
	r := &ExplorerReader{conn: conn}

	got, err := r.RemovedAccountsLastKnownAuthFlags(context.Background(), []string{legacyGStrkey})
	if err != nil {
		t.Fatalf("RemovedAccountsLastKnownAuthFlags: %v", err)
	}
	f, ok := got[legacyGStrkey]
	if !ok {
		t.Fatalf("legacy issuer %s unresolved; got %#v", legacyGStrkey, got)
	}
	if !f.Required || f.Revocable || f.Immutable || f.Clawback {
		t.Errorf("flags = required:%v revocable:%v immutable:%v clawback:%v, want the fixture's mask 1 (required only)",
			f.Required, f.Revocable, f.Immutable, f.Clawback)
	}
	if f.AsOfLedger != legacyRemovedAt {
		t.Errorf("as-of ledger = %d, want %d", f.AsOfLedger, legacyRemovedAt)
	}
	if f.Source != AuthFlagsSourceLastKnownBeforeRemoval {
		t.Errorf("source = %q, want %q", f.Source, AuthFlagsSourceLastKnownBeforeRemoval)
	}
}

// TestRemovedAccountsLastKnownAuthFlags_PreImageReadIsLedgerScoped pins two rules: the pre-image read is
// scoped to the removal ledger (partition-pruned), not an unscoped key_xdr bloom scan; and the argMax
// tiebreak is the full (ledger_seq, intra_ledger_seq, change_index) key, as change_index restarts per tx.
func TestRemovedAccountsLastKnownAuthFlags_PreImageReadIsLedgerScoped(t *testing.T) {
	key := mustAccountLedgerKey(t, residueGStrkey)
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		switch {
		case isRemovalLedgerRead(q):
			return &stubRows{data: [][]any{{key, residueRemovedAt}}}, nil
		case isPreImageRead(q):
			return &stubRows{data: [][]any{{key, residueEntryXDR, residueRemovedAt}}}, nil
		}
		t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}}
	r := &ExplorerReader{conn: conn}
	if _, err := r.RemovedAccountsLastKnownAuthFlags(context.Background(), []string{residueGStrkey}); err != nil {
		t.Fatalf("RemovedAccountsLastKnownAuthFlags: %v", err)
	}

	var preImage string
	var preImageArgs []any
	for i, q := range conn.queries {
		if isPreImageRead(q) {
			preImage, preImageArgs = q, conn.args[i]
		}
	}
	if preImage == "" {
		t.Fatal("no read of stellar.ledger_entry_changes was issued")
	}
	if !strings.Contains(preImage, "WHERE ledger_seq IN (?)") {
		t.Errorf("pre-image read is not ledger-scoped — an unscoped key_xdr predicate falls back to the idx_lec_key_xdr bloom over the whole changes log:\n%s", preImage)
	}
	if !strings.Contains(preImage, "argMax(entry_xdr, (ledger_seq, intra_ledger_seq, change_index))") {
		t.Errorf("argMax tiebreak must be the full (ledger_seq, intra_ledger_seq, change_index) key; change_index alone restarts per transaction:\n%s", preImage)
	}
	if !strings.Contains(preImage, "change_type != 'removed'") {
		t.Errorf("pre-image read must ADMIT `state` rows — the state row before the removal IS the pre-image:\n%s", preImage)
	}
	// The removal ledger must actually be bound, not just named.
	if len(preImageArgs) == 0 {
		t.Fatalf("pre-image read bound no args")
	}
	ledgers, ok := preImageArgs[0].([]uint32)
	if !ok || len(ledgers) != 1 || ledgers[0] != residueRemovedAt {
		t.Errorf("first bound arg = %#v, want []uint32{%d} (the key's removal ledger)", preImageArgs[0], residueRemovedAt)
	}
}

// TestRemovedAccountsLastKnownAuthFlags_RefusesAPreImageFromAnotherLedger: ledger IN-lists are batched
// across the slice, so a key active in another issuer's removal ledger could leak its state here. A row
// whose as_of is not the key's own removal ledger is refused, not mis-attributed.
func TestRemovedAccountsLastKnownAuthFlags_RefusesAPreImageFromAnotherLedger(t *testing.T) {
	key := mustAccountLedgerKey(t, residueGStrkey)
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		switch {
		case isRemovalLedgerRead(q):
			return &stubRows{data: [][]any{{key, residueRemovedAt}}}, nil
		case isPreImageRead(q):
			// Same key, but the aggregate landed on a DIFFERENT ledger.
			return &stubRows{data: [][]any{{key, residueEntryXDR, residueRemovedAt - 1}}}, nil
		}
		t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}}
	r := &ExplorerReader{conn: conn}

	got, err := r.RemovedAccountsLastKnownAuthFlags(context.Background(), []string{residueGStrkey})
	if err != nil {
		t.Fatalf("RemovedAccountsLastKnownAuthFlags: %v", err)
	}
	if f, ok := got[residueGStrkey]; ok {
		t.Errorf("resolved %s from a foreign ledger (as-of %d, removed at %d): %#v",
			residueGStrkey, f.AsOfLedger, residueRemovedAt, f)
	}
}

// TestRemovedAccountsLastKnownAuthFlags_LiveAccountStaysUnresolved — a key
// with no `removed` row is not this path's business, and must not be invented
// into a last-known reading.
func TestRemovedAccountsLastKnownAuthFlags_LiveAccountStaysUnresolved(t *testing.T) {
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		if isRemovalLedgerRead(q) {
			return &stubRows{}, nil
		}
		t.Fatalf("must not read the changes log when nothing was removed: %s", q)
		return nil, nil
	}}
	r := &ExplorerReader{conn: conn}

	got, err := r.RemovedAccountsLastKnownAuthFlags(context.Background(), []string{residueGStrkey})
	if err != nil {
		t.Fatalf("RemovedAccountsLastKnownAuthFlags: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %#v, want no readings for a key the projection never recorded as removed", got)
	}
}

// TestBulkAccountAuthFlags_StampsLiveProvenance: the live path must label its own answers, or
// "unlabelled" would mean both "live entry" and "unknown" and the persisted column is untrustworthy.
func TestBulkAccountAuthFlags_StampsLiveProvenance(t *testing.T) {
	key := mustAccountLedgerKey(t, liveGStrkey)
	const liveAt = uint32(64100000)
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		if isLiveEntryRead(q) {
			return &stubRows{data: [][]any{{key, residueEntryXDR, liveAt}}}, nil
		}
		t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}}
	r := &ExplorerReader{conn: conn}

	got, err := r.BulkAccountAuthFlags(context.Background(), []string{liveGStrkey})
	if err != nil {
		t.Fatalf("BulkAccountAuthFlags: %v", err)
	}
	f, ok := got[liveGStrkey]
	if !ok {
		t.Fatalf("live issuer unresolved; got %#v", got)
	}
	if f.Source != AuthFlagsSourceLive {
		t.Errorf("source = %q, want %q", f.Source, AuthFlagsSourceLive)
	}
	if f.AsOfLedger != liveAt {
		t.Errorf("as-of ledger = %d, want %d (the entry's last-modified ledger)", f.AsOfLedger, liveAt)
	}
	if f.HomeDomain != "qstocks.org" {
		t.Errorf("home_domain = %q, want %q — the live path keeps the domain", f.HomeDomain, "qstocks.org")
	}
}
