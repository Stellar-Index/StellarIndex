package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pgarray"
)

// DirectoryEntry is one curated address label synced from the stellar-expert directory; NOT display-only, as a scam-class tag withholds the issuer's price and demotes its rank.
type DirectoryEntry struct {
	Address string
	Name    string
	Domain  string
	Tags    []string
	Source  string
}

// DirectoryScamFlagTags is the single tag vocabulary marking an address scam-class; pricingguard, the /v1/assets ranking SQL and the explorer's DIRECTORY_SCAM_FLAG_TAGS must not drift from it.
var DirectoryScamFlagTags = []string{
	"malicious",
	"unsafe",
	"fraud",
	"scam",
	"hack",
	"phishing",
}

// directoryUpsertChunk bounds the multi-row upsert: 500 rows x 5 params stays well under
// Postgres's 65535 bind-param cap while keeping a full sync to tens of round trips.
const directoryUpsertChunk = 500

// DirectoryOperatorOverrideSource is the reserved `source` for operator-owned rows, the durable fix for a third-party false positive that ReplaceDirectory never touches or prunes.
const DirectoryOperatorOverrideSource = "operator-override"

// ErrDirectoryNotScamFlagged is returned by [Store.ClearDirectoryScamFlag] when the row carries no scam-class tag; nothing is written.
var ErrDirectoryNotScamFlagged = errors.New("directory: row carries no scam-class tag")

// ErrDirectoryOverrideReasonRequired is returned by [Store.ClearDirectoryScamFlag] for a blank reason: lifting a safety gate needs a recorded why.
var ErrDirectoryOverrideReasonRequired = errors.New("directory: an operator override needs a non-blank reason")

// ErrDirectoryOverrideOperatorRequired is returned by [Store.ClearDirectoryScamFlag] for a blank operator: a lifted safety gate needs a named decider.
var ErrDirectoryOverrideOperatorRequired = errors.New("directory: an operator override needs a non-blank operator")

// DirectoryTagsWithoutScamFlags returns tags minus every scam-class tag, order kept.
func DirectoryTagsWithoutScamFlags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if !isDirectoryScamFlagTag(t) {
			out = append(out, t)
		}
	}
	return out
}

// isDirectoryScamFlagTag matches the canonical form, the widest of the consumers' rules (the Go
// matchers trim, the SQL predicates only fold), so a tag any of them would count is removed.
func isDirectoryScamFlagTag(tag string) bool {
	lt := CanonicalDirectoryTag(tag)
	for _, f := range DirectoryScamFlagTags {
		if lt == f {
			return true
		}
	}
	return false
}

// CanonicalDirectoryTag is the stored form of one directory tag: trimmed and lowercased.
func CanonicalDirectoryTag(tag string) string {
	return strings.ToLower(strings.TrimSpace(tag))
}

// CanonicalDirectoryTags is the form every account_directory writer stores: canonical, deduplicated, order kept, never nil (tags is NOT NULL), so SQL predicates agree with the Go matchers.
func CanonicalDirectoryTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if c := CanonicalDirectoryTag(t); c != "" && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// DirectoryChurnLimit bounds how far ONE sync may move a source's snapshot.
type DirectoryChurnLimit struct {
	Fraction    float64
	Floor       int64
	UnflagFloor int64
}

// DefaultDirectoryChurnLimit is what [Store.ReplaceDirectory] applies: 5% of held rows (floor 100) and 5% of flagged addresses (floor 10) for un-flags.
var DefaultDirectoryChurnLimit = DirectoryChurnLimit{Fraction: 0.05, Floor: 100, UnflagFloor: 10}

// DirectoryChurnUnbounded accepts any snapshot: the operator's opt-in for a known mass change.
var DirectoryChurnUnbounded = DirectoryChurnLimit{}

// ErrDirectoryChurnExceeded is returned when a snapshot would prune, newly flag or un-flag more than the limit allows; nothing is written.
var ErrDirectoryChurnExceeded = errors.New("directory: snapshot exceeds the churn ceiling")

// ceiling is the row cap for a source that held `existing` rows, or ok=false when the sync is
// unbounded (no limit, or nothing held yet).
func (l DirectoryChurnLimit) ceiling(existing int64) (maxRows int64, ok bool) {
	if existing == 0 || l.unbounded() {
		return 0, false
	}
	return max(int64(math.Ceil(float64(existing)*l.Fraction)), l.Floor), true
}

// unflagCeiling is the un-flag cap for a source that held `flagged` scam-flagged addresses, or
// ok=false when nothing can be un-flagged or the sync is unbounded.
func (l DirectoryChurnLimit) unflagCeiling(flagged int64) (maxRows int64, ok bool) {
	if flagged == 0 || l.unbounded() {
		return 0, false
	}
	return max(int64(math.Ceil(float64(flagged)*l.Fraction)), l.UnflagFloor), true
}

func (l DirectoryChurnLimit) unbounded() bool {
	return l.Fraction <= 0 && l.Floor <= 0 && l.UnflagFloor <= 0
}

// DirectorySyncResult is what one ReplaceDirectory run did.
type DirectorySyncResult struct {
	Upserted     int64
	Pruned       int64
	NewlyFlagged int64 // addresses of this source carrying a scam tag now that did not before
	Unflagged    int64 // addresses of this source that carried a scam tag before and do not now (pruned or cleared)
	Existing     int64 // rows the source held before the sync
	// Shadowed counts snapshot addresses another source (or an operator override) owns. The sync
	// leaves those rows alone, and without this count they would vanish from its result.
	Shadowed int64
}

// ReplaceDirectory is [Store.ReplaceDirectoryWithin] under [DefaultDirectoryChurnLimit].
func (s *Store) ReplaceDirectory(ctx context.Context, source string, entries []DirectoryEntry) (upserted, pruned int64, err error) {
	res, err := s.ReplaceDirectoryWithin(ctx, source, entries, DefaultDirectoryChurnLimit)
	return res.Upserted, res.Pruned, err
}

// ReplaceDirectoryWithin upserts one source's full entry set and prunes rows it no longer carries in one transaction, refusing the whole snapshot if churn exceeds `limit`.
func (s *Store) ReplaceDirectoryWithin(ctx context.Context, source string, entries []DirectoryEntry, limit DirectoryChurnLimit) (res DirectorySyncResult, err error) {
	if source == "" {
		return res, errors.New("directory: source must be non-empty")
	}
	// A sync running AS the operator would adopt every override into an upstream snapshot and
	// prune the ones it omits, silently deleting the corrections.
	if source == DirectoryOperatorOverrideSource {
		return res, fmt.Errorf("directory: %q is the reserved operator-override source and cannot be synced", source)
	}
	// An empty snapshot means the upstream fetch broke, not that the directory emptied; proceeding
	// would prune every row of this source. Emptying on purpose is a direct DELETE.
	if len(entries) == 0 {
		return res, errors.New("directory: refusing to sync an empty entry set (would prune the whole source)")
	}
	// Upstream can ship two files declaring one address, and equal addresses in one multi-row
	// upsert make Postgres abort the sync ("cannot affect row a second time"), so collapse
	// duplicates last-wins before chunking.
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

	// Judged after the prune inside the transaction, so a refusal rolls back to the prior table.
	if res.NewlyFlagged, res.Unflagged, err = before.flagDelta(ctx, tx, source); err != nil {
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

// directoryChurn is a source's pre-sync state, the base a [DirectoryChurnLimit] is judged against.
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

// flagDelta counts addresses flagged after the upsert and prune that were not before, and those
// flagged before that no longer are. After the prune every remaining row is from this snapshot.
func (c *directoryChurn) flagDelta(ctx context.Context, tx *sql.Tx, source string) (newly, cleared int64, err error) {
	var after []string
	err = scanDirectoryFlaggedAddresses(ctx, tx, source, func(addr string) { after = append(after, addr) })
	newly, cleared = c.tally(after)
	return newly, cleared, err
}

// tally splits the post-sync flagged set against the pre-sync one. A pruned flagged row counts as
// cleared too, since its price is un-withheld just the same.
func (c *directoryChurn) tally(after []string) (newly, cleared int64) {
	var kept int64
	for _, addr := range after {
		if _, was := c.flagged[addr]; was {
			kept++
		} else {
			newly++
		}
	}
	return newly, int64(len(c.flagged)) - kept
}

func (c *directoryChurn) check(limit DirectoryChurnLimit, res DirectorySyncResult) error {
	flagged := int64(len(c.flagged))
	if maxUnflag, bounded := limit.unflagCeiling(flagged); bounded && res.Unflagged > maxUnflag {
		return fmt.Errorf("%w: un-flags %d of %d scam-flagged addresses (ceiling %d)", ErrDirectoryChurnExceeded, res.Unflagged, flagged, maxUnflag)
	}
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

// scanDirectoryFlaggedAddresses visits `source`'s scam-tagged addresses via directoryScamTaggedSQL, the predicate the listing rank and
// RWA census use, so "newly flagged" agrees with "price withheld".
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

// dedupDirectoryEntriesByAddress collapses duplicate Addresses (last wins, order kept): an upsert chunk may hold each conflict key once.
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

// buildDirectoryUpsert renders a multi-row upsert: 4 params per row, shared source at len(chunk)*4+1; synced_at is transaction-stable
// now(). The conflict arm is ownership-scoped (source = EXCLUDED.source) so a second upstream cannot steal shared addresses or overwrite
// an operator correction; unowned rows are counted as [DirectorySyncResult.Shadowed].
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
		args = append(args, e.Address, e.Name, e.Domain, CanonicalDirectoryTags(e.Tags))
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

// ClearDirectoryScamFlag corrects a third-party false positive by taking the row over as [DirectoryOperatorOverrideSource] with scam-class tags removed and other tags kept, so RWA recognition survives.
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
	// FOR UPDATE: a concurrent sync must not rewrite the tags between this read and the takeover.
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
	kept := DirectoryTagsWithoutScamFlags(before.Tags)
	after.Tags = CanonicalDirectoryTags(kept)
	after.Source = DirectoryOperatorOverrideSource
	if len(kept) == len(before.Tags) {
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

// DeleteDirectoryOverride removes an operator override, reporting whether one existed; scoped to [DirectoryOperatorOverrideSource] so it never deletes an upstream-owned row.
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

// DirectoryEntryByAddress returns the label for one address; ok=false (not an error) when absent, the common case for arbitrary explorer lookups.
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

// DirectoryScamFlaggedClassicAssets lists registered classic assets whose issuer carries a scam-class tag, so the pricing gate can recognise them under their issuer-less SAC id; rows are unvalidated.
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

// DirectoryEntriesByAddresses resolves a batch of addresses in one query, returning only those present.
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
