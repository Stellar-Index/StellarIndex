package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/pgarray"
)

// DirectoryEntry is one curated label for a Stellar address (G-account
// or C-contract strkey). Source of record is the MIT-licensed
// stellar-expert/public-directory repo, synced by `stellarindex-ops
// directory-sync` (migration 0136). Display-only with third-party
// attribution — never an input to verification or scam suppression.
type DirectoryEntry struct {
	Address string
	Name    string
	Domain  string
	Tags    []string
	Source  string
}

// DirectoryScamFlagTags is THE curated-directory tag vocabulary that
// marks an address as scam-class. It lives here — next to the
// account_directory table it reads from — because it now has THREE
// consumers that must not drift:
//
//  1. pricingguard.IsDirectoryScamFlagged (the price/market-cap
//     withholding gate + the API payload suppression),
//  2. the /v1/assets listing SQL, which ranks a flagged issuer's
//     assets BELOW every unflagged one (listingRankTierExpr), and
//  3. the explorer's DIRECTORY_SCAM_FLAG_TAGS in
//     web/explorer/src/lib/directory-tags.ts, which draws the
//     "⚠ Flagged" pill.
//
// Keeping one Go list means "shows a Flagged pill", "has its price
// withheld" and "is demoted in the ranking" can never disagree —
// a split between those three is exactly the drift that let a
// pill-bearing scam token rank #12 on the /assets page (#356).
// Matched case-insensitively; the frontend list is pinned equal by
// pricingguard's TestScamFlagTagSet_MatchesFrontend.
//
// Lowercase-ASCII by construction — mustSQLTextArrayLiteral (which
// inlines this list into the listing's ORDER BY) rejects anything
// else at package-init time.
var DirectoryScamFlagTags = []string{
	"malicious",
	"unsafe",
	"fraud",
	"scam",
	"hack",
	"phishing",
}

// directoryUpsertChunk bounds the multi-row upsert: 500 rows × 5
// params = 2500 placeholders, well under Postgres's 65535 bind-param
// cap while keeping the full 18.5k-entry sync to ~40 round trips.
const directoryUpsertChunk = 500

// DirectoryOperatorOverrideSource is the reserved `source` value for a
// row an OPERATOR owns instead of an upstream sync — the durable escape
// hatch for a third-party FALSE POSITIVE.
//
// The consequence of a wrong upstream tag is not cosmetic: a scam-class
// tag withholds the issuer's published price and market cap
// (pricingguard.ScamGate), demotes its assets below every unflagged one
// in the /v1/assets ranking (listingRankTierExpr) and draws the
// explorer's "⚠ Flagged" pill. Until this constant existed the only
// correction was `UPDATE account_directory SET tags = …` by hand, and
// the next daily `directory-sync` overwrote it — the fix held for hours,
// not until someone decided otherwise.
//
// Ownership is what makes it durable, and ownership is the `source`
// column: ReplaceDirectory only ever touches rows carrying ITS source
// (see buildDirectoryUpsert) and prunes only rows carrying its source,
// so a row owned by this one survives every sync of every upstream.
// Write it with [Store.UpsertDirectoryOverride]; undo it with
// [Store.DeleteDirectoryOverride], after which the next sync restores
// the upstream row.
//
// An override REPLACES the upstream label for that address rather than
// layering over it — one row per address is what keeps "price withheld",
// "demoted in the ranking" and "shows a Flagged pill" from ever
// disagreeing (see DirectoryScamFlagTags above), and a layered view
// would reintroduce exactly that split.
const DirectoryOperatorOverrideSource = "operator-override"

// ReplaceDirectory upserts the full entry set for one source and
// prunes rows of that source the upstream no longer carries.
// Everything runs in one transaction: now() is transaction-stable in
// Postgres, so every upserted row lands with an identical synced_at
// and the prune is simply "same source, older synced_at". A partial
// failure rolls the whole sync back — the table never holds a
// half-applied upstream snapshot.
func (s *Store) ReplaceDirectory(ctx context.Context, source string, entries []DirectoryEntry) (upserted, pruned int64, err error) {
	if source == "" {
		return 0, 0, errors.New("directory: source must be non-empty")
	}
	// A sync may never run AS the operator: it would adopt every
	// operator override into an upstream snapshot and then prune the
	// ones that snapshot omits — i.e. silently delete the corrections
	// this source exists to protect.
	if source == DirectoryOperatorOverrideSource {
		return 0, 0, fmt.Errorf("directory: %q is the reserved operator-override source and cannot be synced", source)
	}
	// An empty snapshot means the fetch/parse upstream broke, not that
	// the directory emptied — proceeding would prune every row of this
	// source. Fail loudly instead; an operator emptying a source on
	// purpose can DELETE directly.
	if len(entries) == 0 {
		return 0, 0, errors.New("directory: refusing to sync an empty entry set (would prune the whole source)")
	}
	// Entries are keyed on the JSON body's `address` field, not the tar
	// filename, so a careless/malicious upstream can ship two files that
	// declare the same address. Two rows with an equal address in one
	// multi-row upsert chunk make Postgres reject the whole statement
	// ("ON CONFLICT DO UPDATE command cannot affect row a second time"),
	// which would abort the entire day's sync (RA-3). Collapse duplicate
	// addresses last-wins before chunking so one bad pair can't freeze
	// the sync.
	entries = dedupDirectoryEntriesByAddress(entries)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("directory: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	for start := 0; start < len(entries); start += directoryUpsertChunk {
		end := min(start+directoryUpsertChunk, len(entries))
		q, args := buildDirectoryUpsert(entries[start:end], source)

		res, execErr := tx.ExecContext(ctx, q, args...)
		if execErr != nil {
			err = fmt.Errorf("directory: upsert chunk [%d:%d): %w", start, end, execErr)
			return 0, 0, err
		}
		n, _ := res.RowsAffected()
		upserted += n
	}

	res, execErr := tx.ExecContext(ctx, `
		DELETE FROM account_directory
		 WHERE source = $1 AND synced_at < now()`, source)
	if execErr != nil {
		err = fmt.Errorf("directory: prune: %w", execErr)
		return 0, 0, err
	}
	pruned, _ = res.RowsAffected()

	if err = tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("directory: commit: %w", err)
	}
	return upserted, pruned, nil
}

// dedupDirectoryEntriesByAddress collapses entries sharing an Address
// to a single entry, last-wins (the later occurrence in upstream
// tar-iteration order overrides the earlier), preserving the original
// relative order of the surviving entries. A single multi-row upsert
// chunk can contain each conflict key at most once, so this is what
// keeps a duplicate-address upstream from aborting ReplaceDirectory's
// whole transaction (RA-3).
func dedupDirectoryEntriesByAddress(entries []DirectoryEntry) []DirectoryEntry {
	idx := make(map[string]int, len(entries))
	out := make([]DirectoryEntry, 0, len(entries))
	for _, e := range entries {
		if i, seen := idx[e.Address]; seen {
			out[i] = e // last-wins: overwrite the earlier row in place
			continue
		}
		idx[e.Address] = len(out)
		out = append(out, e)
	}
	return out
}

// buildDirectoryUpsert renders one multi-row upsert statement for a
// chunk. Per row: 4 positional params (address, name, domain, tags);
// the shared source param sits once at position len(chunk)*4+1 and
// every row references it. synced_at is now() — transaction-stable,
// which is what ReplaceDirectory's prune step relies on.
//
// The conflict arm is OWNERSHIP-SCOPED: `WHERE account_directory.source
// = EXCLUDED.source`, so a sync updates only the rows it owns and
// `source` itself is never rewritten. That is migration 0136's stated
// contract ("scoped by `source` so a future second directory source can
// coexist without the syncs deleting each other's rows"), which the
// unconditional `source = EXCLUDED.source` arm quietly broke in two
// ways: a second upstream would STEAL every shared address from the
// first (whose prune, `WHERE source = $1`, then no longer sees them,
// while the thief's prune eventually deletes them), and — the reason
// this was found — an operator's hand-held correction was adopted into
// the upstream snapshot and overwritten within 24 hours, so there was
// no durable override for a false-positive scam flag at all. Rows the
// chunk conflicts with but does not own are left untouched (no error);
// they simply do not count towards ReplaceDirectory's `upserted`.
func buildDirectoryUpsert(chunk []DirectoryEntry, source string) (string, []any) {
	var (
		sb   strings.Builder
		args = make([]any, 0, len(chunk)*4+1)
	)
	sb.WriteString(`
		INSERT INTO account_directory (address, name, domain, tags, source, synced_at)
		VALUES `)
	srcParam := len(chunk)*4 + 1
	for i, e := range chunk {
		if i > 0 {
			sb.WriteString(", ")
		}
		base := i * 4
		fmt.Fprintf(&sb, "($%d, $%d, $%d, $%d, $%d, now())",
			base+1, base+2, base+3, base+4, srcParam)
		args = append(args, e.Address, e.Name, e.Domain, e.Tags)
	}
	sb.WriteString(`
		ON CONFLICT (address) DO UPDATE SET
		    name = EXCLUDED.name,
		    domain = EXCLUDED.domain,
		    tags = EXCLUDED.tags,
		    synced_at = EXCLUDED.synced_at
		 WHERE account_directory.source = EXCLUDED.source`)
	args = append(args, source)
	return sb.String(), args
}

// UpsertDirectoryOverride records an operator-owned label for one
// address, taking the row over from whichever upstream currently owns
// it. The row carries [DirectoryOperatorOverrideSource], so from this
// point no `directory-sync` of any upstream updates or prunes it — the
// correction holds until an operator removes it, which is the whole
// point (a hand-edited upstream-owned row was reverted by the next
// daily sync).
//
// Tags are written verbatim: pass the corrected set. Dropping the
// scam-class tags (timescale.DirectoryScamFlagTags) is what un-withholds
// the issuer's price and market cap, restores its listing rank tier and
// clears the explorer's flag pill — all three read this one row, so they
// move together.
//
// The `address ~ '^[GC][A-Z2-7]{55}$'` CHECK from migration 0136 is the
// validator; a malformed address is a database error, not a silent
// no-op row.
func (s *Store) UpsertDirectoryOverride(ctx context.Context, e DirectoryEntry) error {
	const q = `
		INSERT INTO account_directory (address, name, domain, tags, source, synced_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (address) DO UPDATE SET
		    name = EXCLUDED.name,
		    domain = EXCLUDED.domain,
		    tags = EXCLUDED.tags,
		    source = EXCLUDED.source,
		    synced_at = EXCLUDED.synced_at`
	tags := e.Tags
	if tags == nil {
		tags = []string{}
	}
	if _, err := s.db.ExecContext(ctx, q,
		e.Address, e.Name, e.Domain, tags, DirectoryOperatorOverrideSource,
	); err != nil {
		return fmt.Errorf("directory: upsert override %s: %w", e.Address, err)
	}
	return nil
}

// DeleteDirectoryOverride removes an operator override, reporting
// whether one was there. It is scoped to
// [DirectoryOperatorOverrideSource] so it can never delete an
// upstream-owned row by mistake. The next `directory-sync` re-inserts
// the upstream label for the address, so this is the documented undo.
func (s *Store) DeleteDirectoryOverride(ctx context.Context, address string) (bool, error) {
	const q = `
		DELETE FROM account_directory
		 WHERE address = $1 AND source = $2`
	res, err := s.db.ExecContext(ctx, q, address, DirectoryOperatorOverrideSource)
	if err != nil {
		return false, fmt.Errorf("directory: delete override %s: %w", address, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DirectoryEntryByAddress returns the label for one address, with
// ok=false (not an error) when the address isn't in the directory —
// the overwhelmingly common case for arbitrary explorer lookups.
func (s *Store) DirectoryEntryByAddress(ctx context.Context, address string) (DirectoryEntry, bool, error) {
	const q = `
		SELECT address, name, domain, tags, source
		  FROM account_directory
		 WHERE address = $1`
	var e DirectoryEntry
	err := s.db.QueryRowContext(ctx, q, address).Scan(
		&e.Address, &e.Name, &e.Domain, pgarray.Strings(&e.Tags), &e.Source)
	if errors.Is(err, sql.ErrNoRows) {
		return DirectoryEntry{}, false, nil
	}
	if err != nil {
		return DirectoryEntry{}, false, fmt.Errorf("directory: lookup %s: %w", address, err)
	}
	return e, true, nil
}

// DirectoryEntriesByAddresses resolves a batch of addresses in one
// query, returning only the ones present. Caller bounds the batch
// (the API layer caps it) — this method just passes the list through.
func (s *Store) DirectoryEntriesByAddresses(ctx context.Context, addresses []string) (map[string]DirectoryEntry, error) {
	if len(addresses) == 0 {
		return map[string]DirectoryEntry{}, nil
	}
	const q = `
		SELECT address, name, domain, tags, source
		  FROM account_directory
		 WHERE address = ANY($1)`
	rows, err := s.db.QueryContext(ctx, q, addresses)
	if err != nil {
		return nil, fmt.Errorf("directory: batch lookup: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]DirectoryEntry, len(addresses))
	for rows.Next() {
		var e DirectoryEntry
		if err := rows.Scan(&e.Address, &e.Name, &e.Domain, pgarray.Strings(&e.Tags), &e.Source); err != nil {
			return nil, fmt.Errorf("directory: batch scan: %w", err)
		}
		out[e.Address] = e
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("directory: batch rows: %w", err)
	}
	return out, nil
}
