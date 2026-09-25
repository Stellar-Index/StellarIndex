package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pgarray"
)

// DirectoryEntry is one curated label for a Stellar address (G-account
// or C-contract strkey). Source of record is the MIT-licensed
// stellar-expert/public-directory repo, synced by `stellarindex-ops
// directory-sync` (migration 0136), shown with third-party attribution.
// NOT display-only: a scam-class tag withholds the issuer's price and
// demotes its listing rank (DirectoryScamFlagTags), and a recognition tag
// admits it to the RWA surface (DirectoryRecognisedContracts).
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
// Write it with `stellarindex-ops directory-override -clear-scam-flag -reason
// [-actor]` ([Store.ClearDirectoryScamFlag]); undo it with `-delete`
// ([Store.DeleteDirectoryOverride]), after which the next sync restores
// the upstream row.
//
// An override REPLACES the upstream label for that address rather than
// layering over it — one row per address is what keeps "price withheld",
// "demoted in the ranking" and "shows a Flagged pill" from ever
// disagreeing (see DirectoryScamFlagTags above), and a layered view
// would reintroduce exactly that split.
const DirectoryOperatorOverrideSource = "operator-override"

// ErrDirectoryNotScamFlagged is returned by [Store.ClearDirectoryScamFlag]
// when the row carries no scam-class tag; nothing was written, so a
// mistyped address cannot freeze an unflagged row away from its upstream.
var ErrDirectoryNotScamFlagged = errors.New("directory: row carries no scam-class tag")

// ErrDirectoryOverrideReasonRequired is returned by
// [Store.ClearDirectoryScamFlag] for a blank reason: lifting a safety
// gate without a recorded why leaves nothing to review.
var ErrDirectoryOverrideReasonRequired = errors.New("directory: an operator override needs a non-blank reason")

// ErrDirectoryOverrideOperatorRequired is returned by
// [Store.ClearDirectoryScamFlag] for a blank operator: a lifted safety
// gate with no named decider leaves a reviewer nobody to ask.
var ErrDirectoryOverrideOperatorRequired = errors.New("directory: an operator override needs a non-blank operator")

// DirectoryTagsWithoutScamFlags returns tags with every scam-class tag
// removed and every other tag kept, in order.
func DirectoryTagsWithoutScamFlags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if !isDirectoryScamFlagTag(t) {
			out = append(out, t)
		}
	}
	return out
}

// isDirectoryScamFlagTag matches trimmed and case-folded, the widest of
// the rules the consumers apply (pricingguard and rwa trim, the SQL
// predicates only fold), so a tag any of them would count is removed.
func isDirectoryScamFlagTag(tag string) bool {
	lt := strings.ToLower(strings.TrimSpace(tag))
	for _, f := range DirectoryScamFlagTags {
		if lt == f {
			return true
		}
	}
	return false
}

// DirectoryChurnLimit bounds how far ONE sync may move a source's
// snapshot: the rows it prunes and the addresses it newly scam-flags
// are each capped at max(Floor, ceil(Fraction × rows the source held
// before the sync)). A snapshot past either cap is refused whole with
// [ErrDirectoryChurnExceeded] and the table is left as it was.
//
// The upstream is an unpinned branch of a third-party repo, and a
// scam tag is not a label: it withholds the issuer's price and market
// cap (pricingguard.ScamGate). A hijacked, truncated or mis-generated
// snapshot could therefore prune the flags the gate withholds on, or
// flag thousands of issuers and withhold their prices, in one
// transaction with nothing failing. Real churn is a handful of rows a
// day; a legitimate mass change ships with `directory-sync
// -accept-churn`. The first sync of a source (nothing held yet) is
// unbounded by construction. Both fractions/floors are placeholders
// chosen conservatively, not measured upstream churn.
type DirectoryChurnLimit struct {
	Fraction float64
	Floor    int64
}

// DefaultDirectoryChurnLimit is what [Store.ReplaceDirectory] applies:
// 5 % of the held rows, never below 100 (so a small table is not
// jammed by its own arithmetic).
var DefaultDirectoryChurnLimit = DirectoryChurnLimit{Fraction: 0.05, Floor: 100}

// DirectoryChurnUnbounded accepts any snapshot — the operator's
// explicit opt-in for a known upstream mass change.
var DirectoryChurnUnbounded = DirectoryChurnLimit{}

// ErrDirectoryChurnExceeded is returned (wrapped, with the numbers)
// when a snapshot would prune or newly flag more rows than the limit
// allows; nothing was written.
var ErrDirectoryChurnExceeded = errors.New("directory: snapshot exceeds the churn ceiling")

// ceiling is the row cap for a source that held `existing` rows, or
// ok=false when the sync is unbounded (no limit, or nothing held yet).
func (l DirectoryChurnLimit) ceiling(existing int64) (maxRows int64, ok bool) {
	if existing == 0 || (l.Fraction <= 0 && l.Floor <= 0) {
		return 0, false
	}
	return max(int64(math.Ceil(float64(existing)*l.Fraction)), l.Floor), true
}

// DirectorySyncResult is what one ReplaceDirectory run did.
type DirectorySyncResult struct {
	Upserted     int64
	Pruned       int64
	NewlyFlagged int64 // addresses of this source carrying a scam tag now that did not before
	Existing     int64 // rows the source held before the sync
	// Shadowed counts snapshot addresses another source (or an operator
	// override) owns: the sync leaves those rows alone, so without this
	// count a second source's shared addresses vanish from its result.
	Shadowed int64
}

// ReplaceDirectory is [Store.ReplaceDirectoryWithin] under
// [DefaultDirectoryChurnLimit].
func (s *Store) ReplaceDirectory(ctx context.Context, source string, entries []DirectoryEntry) (upserted, pruned int64, err error) {
	res, err := s.ReplaceDirectoryWithin(ctx, source, entries, DefaultDirectoryChurnLimit)
	return res.Upserted, res.Pruned, err
}

// ReplaceDirectoryWithin upserts the full entry set for one source and
// prunes rows of that source the upstream no longer carries, refusing
// the whole snapshot if it moves more than `limit` allows.
// Everything runs in one transaction: now() is transaction-stable in
// Postgres, so every upserted row lands with an identical synced_at
// and the prune is simply "same source, older synced_at". A partial
// failure — or a churn refusal, judged after the prune — rolls the
// whole sync back; the table never holds a half-applied upstream
// snapshot.
func (s *Store) ReplaceDirectoryWithin(ctx context.Context, source string, entries []DirectoryEntry, limit DirectoryChurnLimit) (res DirectorySyncResult, err error) {
	if source == "" {
		return res, errors.New("directory: source must be non-empty")
	}
	// A sync may never run AS the operator: it would adopt every
	// operator override into an upstream snapshot and then prune the
	// ones that snapshot omits — i.e. silently delete the corrections
	// this source exists to protect.
	if source == DirectoryOperatorOverrideSource {
		return res, fmt.Errorf("directory: %q is the reserved operator-override source and cannot be synced", source)
	}
	// An empty snapshot means the fetch/parse upstream broke, not that
	// the directory emptied — proceeding would prune every row of this
	// source. Fail loudly instead; an operator emptying a source on
	// purpose can DELETE directly.
	if len(entries) == 0 {
		return res, errors.New("directory: refusing to sync an empty entry set (would prune the whole source)")
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
		return res, fmt.Errorf("directory: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	before, err := snapshotDirectoryChurn(ctx, tx, source)
	if err != nil {
		return res, err
	}
	res.Existing = before.rows

	for start := 0; start < len(entries); start += directoryUpsertChunk {
		end := min(start+directoryUpsertChunk, len(entries))
		q, args := buildDirectoryUpsert(entries[start:end], source)

		r, execErr := tx.ExecContext(ctx, q, args...)
		if execErr != nil {
			err = fmt.Errorf("directory: upsert chunk [%d:%d): %w", start, end, execErr)
			return res, err
		}
		n, _ := r.RowsAffected()
		res.Upserted += n
	}
	// A conflict row the ownership arm skips is not counted as affected.
	res.Shadowed = int64(len(entries)) - res.Upserted

	r, execErr := tx.ExecContext(ctx, `
		DELETE FROM account_directory
		 WHERE source = $1 AND synced_at < now()`, source)
	if execErr != nil {
		err = fmt.Errorf("directory: prune: %w", execErr)
		return res, err
	}
	res.Pruned, _ = r.RowsAffected()

	// Judged after the prune, inside the transaction: a refusal is a
	// rollback, so the table is exactly as it was.
	if res.NewlyFlagged, err = before.newlyFlagged(ctx, tx, source); err != nil {
		return res, err
	}
	if err = before.check(limit, res); err != nil {
		return res, err
	}

	if err = tx.Commit(); err != nil {
		return res, fmt.Errorf("directory: commit: %w", err)
	}
	return res, nil
}

// directoryChurn is a source's pre-sync state, the base a
// [DirectoryChurnLimit] is judged against.
type directoryChurn struct {
	rows    int64
	flagged map[string]struct{} // addresses carrying a scam-class tag before the sync
}

func snapshotDirectoryChurn(ctx context.Context, tx *sql.Tx, source string) (*directoryChurn, error) {
	c := &directoryChurn{flagged: map[string]struct{}{}}
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM account_directory WHERE source = $1`, source).Scan(&c.rows); err != nil {
		return nil, fmt.Errorf("directory: count before sync: %w", err)
	}
	if err := scanDirectoryFlaggedAddresses(ctx, tx, source, func(addr string) { c.flagged[addr] = struct{}{} }); err != nil {
		return nil, err
	}
	return c, nil
}

// newlyFlagged counts the source's scam-flagged addresses AFTER the
// upsert + prune that were not flagged before it. Run after the prune,
// every remaining row of the source is from this snapshot.
func (c *directoryChurn) newlyFlagged(ctx context.Context, tx *sql.Tx, source string) (int64, error) {
	var n int64
	err := scanDirectoryFlaggedAddresses(ctx, tx, source, func(addr string) {
		if _, was := c.flagged[addr]; !was {
			n++
		}
	})
	return n, err
}

func (c *directoryChurn) check(limit DirectoryChurnLimit, res DirectorySyncResult) error {
	maxRows, bounded := limit.ceiling(c.rows)
	if !bounded {
		return nil
	}
	if res.Pruned > maxRows {
		return fmt.Errorf("%w: prunes %d of %d rows (ceiling %d)", ErrDirectoryChurnExceeded, res.Pruned, c.rows, maxRows)
	}
	if res.NewlyFlagged > maxRows {
		return fmt.Errorf("%w: newly scam-flags %d addresses of %d rows (ceiling %d)", ErrDirectoryChurnExceeded, res.NewlyFlagged, c.rows, maxRows)
	}
	return nil
}

// scanDirectoryFlaggedAddresses visits every address of `source` that
// carries a scam-class tag, through the same predicate the listing
// rank and the RWA census use (directoryScamTaggedSQL), so "counts as
// newly flagged" can never disagree with "has its price withheld".
func scanDirectoryFlaggedAddresses(ctx context.Context, tx *sql.Tx, source string, visit func(addr string)) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT address FROM account_directory
		 WHERE source = $1 AND `+directoryScamTaggedSQL, source)
	if err != nil {
		return fmt.Errorf("directory: flagged addresses: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var addr string
		if err := rows.Scan(&addr); err != nil {
			return fmt.Errorf("directory: flagged addresses: %w", err)
		}
		visit(addr)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("directory: flagged addresses: %w", err)
	}
	return nil
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
// chunk conflicts with but does not own are left untouched (no error)
// and reported as [DirectorySyncResult.Shadowed]: the first source to
// hold an address keeps it, and the other's prune never sees it.
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

// ClearDirectoryScamFlag corrects a third-party FALSE POSITIVE: it takes
// the address's row over as [DirectoryOperatorOverrideSource] with every
// scam-class tag removed and the name, domain and every other tag kept,
// so no `directory-sync` updates or prunes it until the override is
// deleted.
//
// Keeping the other tags is the point. The row feeds four readers: the
// price gate, the /v1/assets rank tier, the explorer's flag pill and the
// RWA recognition funnel, which admits an issuer on a recognition tag
// such as `issuer` (directoryHasIssuingTagSQL). Replacing the tag set
// wholesale clears the flag and silently drops the issuer from that
// funnel.
//
// operator and reason are stored in override_by (migration 0177) and
// override_reason (0170) so the lifted flag can be reviewed; a blank
// either is refused before anything is read.
//
// found=false (no error) means the address has no row. A row with no
// scam-class tag is refused with [ErrDirectoryNotScamFlagged].
func (s *Store) ClearDirectoryScamFlag(ctx context.Context, address, operator, reason string) (before, after DirectoryEntry, found bool, err error) {
	if strings.TrimSpace(operator) == "" {
		return before, after, false, ErrDirectoryOverrideOperatorRequired
	}
	if strings.TrimSpace(reason) == "" {
		return before, after, false, ErrDirectoryOverrideReasonRequired
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return before, after, false, fmt.Errorf("directory: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	// FOR UPDATE: a concurrent sync must not rewrite the tags between
	// this read and the takeover.
	err = tx.QueryRowContext(ctx, `
		SELECT address, name, domain, tags, source
		  FROM account_directory
		 WHERE address = $1
		   FOR UPDATE`, address).Scan(
		&before.Address, &before.Name, &before.Domain, pgarray.Strings(&before.Tags), &before.Source)
	if errors.Is(err, sql.ErrNoRows) {
		return before, after, false, nil
	}
	if err != nil {
		return before, after, false, fmt.Errorf("directory: lock %s: %w", address, err)
	}
	after = before
	after.Tags = DirectoryTagsWithoutScamFlags(before.Tags)
	after.Source = DirectoryOperatorOverrideSource
	if len(after.Tags) == len(before.Tags) {
		return before, after, true, fmt.Errorf("%w: %s (tags %v)", ErrDirectoryNotScamFlagged, address, before.Tags)
	}
	if _, err = tx.ExecContext(ctx, `
		UPDATE account_directory
		   SET tags = $2, source = $3, override_reason = $4, override_by = $5, synced_at = now()
		 WHERE address = $1`, address, after.Tags, after.Source, reason, strings.TrimSpace(operator)); err != nil {
		return before, after, true, fmt.Errorf("directory: clear scam flag %s: %w", address, err)
	}
	if err = tx.Commit(); err != nil {
		return before, after, true, fmt.Errorf("directory: commit: %w", err)
	}
	return before, after, true, nil
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

// DirectoryScamFlaggedClassicAssets lists every registered classic asset
// whose issuer account carries a scam-class tag ([DirectoryScamFlagTags]).
// It is what lets the scam pricing gate recognise a flagged issuer's
// asset under its SAC contract id, which carries no issuer of its own.
// Rows are returned as stored; the caller validates them.
func (s *Store) DirectoryScamFlaggedClassicAssets(ctx context.Context) ([]canonical.Asset, error) {
	q := `
		SELECT ca.code, ca.issuer_g_strkey
		  FROM classic_assets ca
		 WHERE ca.issuer_g_strkey IN (
		       SELECT address FROM account_directory
		        WHERE ` + directoryIsAccountSQL + `
		          AND ` + directoryScamTaggedSQL + `)`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("directory: scam-flagged classic assets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []canonical.Asset
	for rows.Next() {
		a := canonical.Asset{Type: canonical.AssetClassic}
		if err := rows.Scan(&a.Code, &a.Issuer); err != nil {
			return nil, fmt.Errorf("directory: scan scam-flagged classic asset: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("directory: scam-flagged classic asset rows: %w", err)
	}
	return out, nil
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
