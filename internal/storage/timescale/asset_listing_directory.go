// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// asset_listing_directory: the cached Stellar slice of an independent price platform's address map,
// written by `stellarindex-ops listing-sync`.
//
// A ROW corroborates that a second party reached the same (address -> instrument) pairing; it is
// no proof of control. A MISSING ROW means only "not listed". This surface only ADDS
// corroboration; negative verdicts live in [DirectoryScamFlagTags].
//
// TWO CLOCKS, TWO BOUNDS: RECOGNITION ages on `synced_at` (ours, [listingRecognitionMaxAge]); the
// PRICE ages on `priced_at` (the platform's, [listingPriceMaxAge]). Both are spliced into the
// reader's SQL so a new caller cannot forget them.

// ListingEntry is one row of the independent listing directory.
type ListingEntry struct {
	// Address is the published Stellar address: a Soroban C-strkey or a classic `CODE-GISSUER`
	// pair, the only two forms the table CHECK admits.
	Address string
	// ListingID is the platform's opaque coin slug: provenance and the price re-fetch key, never
	// an identity this index resolves an asset by.
	ListingID string
	// Symbol is the platform's display ticker; not an asset code, not unique, never a resolution key.
	Symbol string
	// PriceUSD is the platform's USD price as the DECIMAL STRING the upstream printed, carried
	// unrounded into a NUMERIC column (ADR-0003); never a float64.
	//
	// Empty when no price was published AND when it is past [listingPriceMaxAge], so a reader
	// cannot tell the two apart and use the stale one.
	PriceUSD string
	// PricedAt is the PLATFORM's own publication time for that price. Zero exactly when PriceUSD
	// is empty; a table CHECK enforces that the two travel together.
	PricedAt time.Time
	// Source names the platform the row came from; upserts and the prune are scoped by it.
	Source string
}

// ListingDirectoryCensus is the storage layer's account of its own numbers: how large the cached set was and which bound removed the rest.
type ListingDirectoryCensus struct {
	// Entries counts fresh rows of every address form, before the contract/classic split.
	Entries int
	// Contracts counts fresh rows whose address is a C-strkey.
	Contracts int
	// Classic counts fresh rows whose address is a `CODE-GISSUER` pair.
	Classic int
	// Stale counts rows present but past [listingRecognitionMaxAge], the fail-closed shrink made
	// visible. It is the only read-side evidence of a stopped hourly sync, since reads still
	// succeed; Stale rising toward Entries + Stale is a dead sync, not a delisting.
	Stale int
	// Priced counts fresh CONTRACT rows whose price is inside [listingPriceMaxAge]. A fresh row
	// with a frozen price counts in Contracts and not here, which is why the clocks are split.
	Priced int
	// PricedClassic is the same count over CLASSIC rows, kept apart so (Priced <= Contracts) holds
	// while [Store.ListingDirectoryByAddress], which serves both forms, has a count to compare.
	PricedClassic int
}

// Check returns the reason the census does not balance, or "" when it does.
func (c ListingDirectoryCensus) Check() string {
	if got := c.Contracts + c.Classic; got != c.Entries {
		return fmt.Sprintf("address forms sum to %d, not Entries %d", got, c.Entries)
	}
	if c.Priced > c.Contracts {
		return fmt.Sprintf("Priced %d exceeds Contracts %d", c.Priced, c.Contracts)
	}
	if c.PricedClassic > c.Classic {
		return fmt.Sprintf("PricedClassic %d exceeds Classic %d", c.PricedClassic, c.Classic)
	}
	return ""
}

// listingRecognitionMaxAge is how long a cached row may be reused as RECOGNITION of an address,
// measured on `synced_at`. It is spliced into every read so the bound lives in SQL.
//
// 48 hours: mappings change at human cadence, so a too-TIGHT bound is the real cost (2 hours on an
// hourly sync flaps the admitted set on one rate-limited pass). 48h means 48 consecutive missed
// passes, a DEAD sync, shown as [ListingDirectoryCensus.Stale].
const listingRecognitionMaxAge = "48 hours"

// listingPriceMaxAge is how long the PLATFORM's price may be reused, measured on `priced_at` (its
// clock), NOT `synced_at`: a frozen price is re-copied with a new `synced_at` every hour, so that
// bound would launder it as fresh. A MISSING `priced_at` is REJECTED as unverifiable.
//
// 24 hours because this price is corroboration, not a trading input; tighter would blank most
// (illiquid) listings.
const listingPriceMaxAge = "24 hours"

// ListingPriceMaxFutureSkew is how far AHEAD of our clock a `priced_at` may sit and still count.
const ListingPriceMaxFutureSkew = 5 * time.Minute

const listingPriceMaxFutureSkew = "5 minutes"

// listingIsContractSQL and listingIsClassicSQL split the cached set by address form; they twin the
// table CHECK, so the census can treat the two as exhaustive.
//
// They are REGEXES, not `LIKE 'C%'`: a classic code may begin with C (`CETES-G...`), and a LIKE
// would put it in the contract arm while the census still balanced. Classic codes are
// case-SENSITIVE (MXNe, sUSD), hence `[A-Za-z0-9]`.
const (
	listingIsContractSQL = `address ~ '^C[A-Z2-7]{55}$'`
	listingIsClassicSQL  = `address ~ '^[A-Za-z0-9]{1,12}-G[A-Z2-7]{55}$'`
)

// listingRecognisedSQL is the recognition bound as a predicate. Both the census count and the row
// read use it, so they cannot disagree about who qualified.
const listingRecognisedSQL = `synced_at > now() - INTERVAL '` + listingRecognitionMaxAge + `'`

// listingPriceFreshSQL is the price bound as a predicate on the platform's clock. `IS NOT NULL`
// rejects a missing publication time explicitly, and the ceiling arm makes the floor a bound (see
// [ListingPriceMaxFutureSkew]).
const listingPriceFreshSQL = `price_usd IS NOT NULL
		   AND priced_at IS NOT NULL
		   AND priced_at > now() - INTERVAL '` + listingPriceMaxAge + `'
		   AND priced_at <= now() + INTERVAL '` + listingPriceMaxFutureSkew + `'`

// listingUpsertChunk bounds the multi-row upsert: 500 rows x 5 params = 2500 placeholders, far
// under Postgres's 65535 bind-param cap. Chunking exists because a third party sets the row count.
const listingUpsertChunk = 500

// ReplaceListingDirectory upserts the full entry set for one source and prunes that source's rows the upstream no longer carries.
func (s *Store) ReplaceListingDirectory(
	ctx context.Context, entries []ListingEntry, source string,
) (upserted, pruned int64, err error) {
	if source == "" {
		return 0, 0, errors.New("listing directory: source must be non-empty")
	}
	// An empty set means the fetch or parse broke, not a mass delisting; proceeding would prune the
	// whole source and report success. Same refusal as [ReplaceDirectory].
	if len(entries) == 0 {
		return 0, 0, errors.New("listing directory: refusing to sync an empty entry set (would prune the whole source)")
	}
	// Two coin ids can name one address, and equal addresses in one upsert chunk make Postgres
	// reject the WHOLE statement ("cannot affect row a second time"), so collapse last-wins first.
	entries = dedupListingEntriesByAddress(entries)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("listing directory: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	for start := 0; start < len(entries); start += listingUpsertChunk {
		end := min(start+listingUpsertChunk, len(entries))
		q, args := buildListingUpsert(entries[start:end], source)

		res, execErr := tx.ExecContext(ctx, q, args...)
		if execErr != nil {
			err = fmt.Errorf("listing directory: upsert chunk [%d:%d): %w", start, end, execErr)
			return 0, 0, err
		}
		n, _ := res.RowsAffected()
		upserted += n
	}

	res, execErr := tx.ExecContext(ctx, `
		DELETE FROM asset_listing_directory
		 WHERE source = $1 AND synced_at < now()`, source)
	if execErr != nil {
		err = fmt.Errorf("listing directory: prune: %w", execErr)
		return 0, 0, err
	}
	pruned, _ = res.RowsAffected()

	if err = tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("listing directory: commit: %w", err)
	}
	return upserted, pruned, nil
}

// dedupListingEntriesByAddress collapses entries sharing an Address, last wins, keeping survivor
// order. An upsert chunk may hold each conflict key once, or the transaction aborts.
func dedupListingEntriesByAddress(entries []ListingEntry) []ListingEntry {
	idx := make(map[string]int, len(entries))
	out := make([]ListingEntry, 0, len(entries))
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

// buildListingUpsert renders one multi-row upsert: 5 params per row (address, listing_id, symbol,
// price_usd, priced_at) plus one shared source param at len(chunk)*5+1. synced_at is the
// transaction-stable now() the prune relies on.
//
// The price and time placeholders carry EXPLICIT `::numeric` / `::timestamptz` casts, as untyped
// binds fail type inference; the price is a decimal STRING parsed by Postgres, never a float.
func buildListingUpsert(chunk []ListingEntry, source string) (string, []any) {
	var (
		sb   strings.Builder
		args = make([]any, 0, len(chunk)*5+1)
	)
	sb.WriteString(`
		INSERT INTO asset_listing_directory
		    (address, listing_id, symbol, price_usd, priced_at, source, synced_at)
		VALUES `)
	srcParam := len(chunk)*5 + 1
	for i, e := range chunk {
		if i > 0 {
			sb.WriteString(", ")
		}
		base := i * 5
		fmt.Fprintf(&sb, "($%d, $%d, $%d, $%d::numeric, $%d::timestamptz, $%d, now())",
			base+1, base+2, base+3, base+4, base+5, srcParam)

		// Price and publication time bind together or both NULL; the table CHECK refuses a mixed row.
		var (
			price    sql.NullString
			pricedAt sql.NullTime
		)
		if e.PriceUSD != "" && !e.PricedAt.IsZero() {
			price = sql.NullString{String: e.PriceUSD, Valid: true}
			pricedAt = sql.NullTime{Time: e.PricedAt.UTC(), Valid: true}
		}
		args = append(args, e.Address, e.ListingID, e.Symbol, price, pricedAt)
	}
	sb.WriteString(`
		ON CONFLICT (address) DO UPDATE SET
		    listing_id = EXCLUDED.listing_id,
		    symbol     = EXCLUDED.symbol,
		    price_usd  = EXCLUDED.price_usd,
		    priced_at  = EXCLUDED.priced_at,
		    source     = EXCLUDED.source,
		    synced_at  = EXCLUDED.synced_at`)
	args = append(args, source)
	return sb.String(), args
}

// listingDirectoryContractsSQL reads the recognised CONTRACT rows.
//
// The price columns use a CASE on [listingPriceFreshSQL], not a WHERE: past the price bound the
// row is STILL RECOGNISED, just unpriced. `price_usd::text` returns the NUMERIC digits verbatim.
const listingDirectoryContractsSQL = `
		SELECT address, listing_id, symbol,
		       CASE WHEN ` + listingPriceFreshSQL + `
		            THEN price_usd::text END AS price_usd,
		       CASE WHEN ` + listingPriceFreshSQL + `
		            THEN priced_at END       AS priced_at,
		       source
		  FROM asset_listing_directory
		 WHERE ` + listingRecognisedSQL + `
		   AND ` + listingIsContractSQL + `
		 ORDER BY address`

// listingDirectoryCensusSQL counts every bucket in one pass.
//
// One statement with FILTER aggregates so the buckets share ONE snapshot; separate statements can
// straddle a sync or the moving `now()` and fail to reconcile.
const listingDirectoryCensusSQL = `
		SELECT
		  count(*) FILTER (WHERE ` + listingRecognisedSQL + `)              AS entries,
		  count(*) FILTER (WHERE ` + listingRecognisedSQL + `
		                     AND ` + listingIsContractSQL + `)              AS contracts,
		  count(*) FILTER (WHERE ` + listingRecognisedSQL + `
		                     AND ` + listingIsClassicSQL + `)               AS classic,
		  count(*) FILTER (WHERE NOT (` + listingRecognisedSQL + `))        AS stale,
		  count(*) FILTER (WHERE ` + listingRecognisedSQL + `
		                     AND ` + listingIsContractSQL + `
		                     AND ` + listingPriceFreshSQL + `)              AS priced,
		  count(*) FILTER (WHERE ` + listingRecognisedSQL + `
		                     AND ` + listingIsClassicSQL + `
		                     AND ` + listingPriceFreshSQL + `)              AS priced_classic
		  FROM asset_listing_directory`

// ListingDirectoryContracts returns the recognised CONTRACT (C-strkey) rows of the listing directory cache within [listingRecognitionMaxAge], plus the census of the whole cache; classic rows are only counted.
func (s *Store) ListingDirectoryContracts(
	ctx context.Context,
) ([]ListingEntry, ListingDirectoryCensus, error) {
	census, err := s.listingDirectoryCensus(ctx)
	if err != nil {
		return nil, ListingDirectoryCensus{}, err
	}

	rows, err := s.db.QueryContext(ctx, listingDirectoryContractsSQL)
	if err != nil {
		return nil, ListingDirectoryCensus{}, fmt.Errorf("listing directory: contracts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]ListingEntry, 0, 16)
	for rows.Next() {
		var (
			e        ListingEntry
			price    sql.NullString
			pricedAt sql.NullTime
		)
		if err := rows.Scan(&e.Address, &e.ListingID, &e.Symbol, &price, &pricedAt, &e.Source); err != nil {
			return nil, ListingDirectoryCensus{}, fmt.Errorf("listing directory: scan contract: %w", err)
		}
		// Both or neither, so a caller testing PriceUSD != "" can rely on PricedAt being set.
		if price.Valid && pricedAt.Valid {
			e.PriceUSD = price.String
			e.PricedAt = pricedAt.Time.UTC()
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, ListingDirectoryCensus{}, fmt.Errorf("listing directory: contract rows: %w", err)
	}
	return out, census, nil
}

// listingDirectoryCensus counts the whole cached set, bucketed as the reader narrows it.
func (s *Store) listingDirectoryCensus(ctx context.Context) (ListingDirectoryCensus, error) {
	var c ListingDirectoryCensus
	err := s.db.QueryRowContext(ctx, listingDirectoryCensusSQL).Scan(
		&c.Entries, &c.Contracts, &c.Classic, &c.Stale, &c.Priced, &c.PricedClassic,
	)
	if err != nil {
		return ListingDirectoryCensus{}, fmt.Errorf("listing directory: census: %w", err)
	}
	return c, nil
}

// listingDirectoryByAddressSQL is [listingDirectoryContractsSQL] without the address-form
// predicate: a classic asset can be listed under EITHER its `CODE-GISSUER` id or its SAC C-strkey,
// and a reader seeing one form misses whichever the platform published. Same two staleness bounds.
const listingDirectoryByAddressSQL = `
		SELECT address, listing_id, symbol,
		       CASE WHEN ` + listingPriceFreshSQL + `
		            THEN price_usd::text END AS price_usd,
		       CASE WHEN ` + listingPriceFreshSQL + `
		            THEN priced_at END       AS priced_at,
		       source
		  FROM asset_listing_directory
		 WHERE ` + listingRecognisedSQL + `
		 ORDER BY address`

// ListingDirectoryByAddress returns every recognised row of the cached listing directory keyed by its exact Stellar address, plus the census of the whole cache.
func (s *Store) ListingDirectoryByAddress(
	ctx context.Context,
) (map[string]ListingEntry, ListingDirectoryCensus, error) {
	census, err := s.listingDirectoryCensus(ctx)
	if err != nil {
		return nil, ListingDirectoryCensus{}, err
	}

	rows, err := s.db.QueryContext(ctx, listingDirectoryByAddressSQL)
	if err != nil {
		return nil, ListingDirectoryCensus{}, fmt.Errorf("listing directory: by address: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]ListingEntry, 64)
	for rows.Next() {
		var (
			e        ListingEntry
			price    sql.NullString
			pricedAt sql.NullTime
		)
		if err := rows.Scan(&e.Address, &e.ListingID, &e.Symbol, &price, &pricedAt, &e.Source); err != nil {
			return nil, ListingDirectoryCensus{}, fmt.Errorf("listing directory: scan row: %w", err)
		}
		// Both or neither, exactly as [ListingDirectoryContracts] does.
		if price.Valid && pricedAt.Valid {
			e.PriceUSD = price.String
			e.PricedAt = pricedAt.Time.UTC()
		}
		out[e.Address] = e
	}
	if err := rows.Err(); err != nil {
		return nil, ListingDirectoryCensus{}, fmt.Errorf("listing directory: rows: %w", err)
	}
	return out, census, nil
}
