package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/lib/pq"
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

// directoryUpsertChunk bounds the multi-row upsert: 500 rows × 5
// params = 2500 placeholders, well under lib/pq's 65535 cap while
// keeping the full 18.5k-entry sync to ~40 round trips.
const directoryUpsertChunk = 500

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
		args = append(args, e.Address, e.Name, e.Domain, pq.Array(e.Tags))
	}
	sb.WriteString(`
		ON CONFLICT (address) DO UPDATE SET
		    name = EXCLUDED.name,
		    domain = EXCLUDED.domain,
		    tags = EXCLUDED.tags,
		    source = EXCLUDED.source,
		    synced_at = EXCLUDED.synced_at`)
	args = append(args, source)
	return sb.String(), args
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
		&e.Address, &e.Name, &e.Domain, pq.Array(&e.Tags), &e.Source)
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
	rows, err := s.db.QueryContext(ctx, q, pq.Array(addresses))
	if err != nil {
		return nil, fmt.Errorf("directory: batch lookup: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]DirectoryEntry, len(addresses))
	for rows.Next() {
		var e DirectoryEntry
		if err := rows.Scan(&e.Address, &e.Name, &e.Domain, pq.Array(&e.Tags), &e.Source); err != nil {
			return nil, fmt.Errorf("directory: batch scan: %w", err)
		}
		out[e.Address] = e
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("directory: batch rows: %w", err)
	}
	return out, nil
}
