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

// asset_listing_directory: the cached Stellar slice of an independent price-aggregation platform's
// address map, written by `stellarindex-ops listing-sync`.
//
// A ROW is a CORROBORATION that a second party reached the same (address -> instrument) pairing;
// it carries no proof of control. A MISSING ROW means "not listed", nothing else. There are no scam
// flags here and there must not be: this surface only ADDS corroboration, never withholds, demotes
// or accuses ([DirectoryScamFlagTags] is where negative verdicts live).
//
// TWO CLOCKS, TWO BOUNDS: RECOGNITION ages on `synced_at` (our clock, [listingRecognitionMaxAge]);
// the PRICE ages on `priced_at` (the platform's clock, [listingPriceMaxAge]). Conflating them is
// the failure this file prevents. Both bounds are spliced into the reader's SQL, as with
// [assetPriceSnapshotMaxAge], because a bound that lives in a caller is one a new caller can forget.

// ListingEntry is one row of the independent listing directory.
type ListingEntry struct {
	// Address is the Stellar address the platform published, in either
	// network form: a Soroban contract C-strkey, or a classic
	// `CODE-GISSUER` pair. The table's CHECK admits exactly those two.
	Address string
	// ListingID is the platform's opaque coin slug ("stellar",
	// "usd-coin"). Provenance and the price re-fetch key; never an
	// identity this index resolves an asset by.
	ListingID string
	// Symbol is the platform's display ticker, lowercase as published.
	// Not an asset code, not unique, never a resolution key.
	Symbol string
	// PriceUSD is the platform's USD price as the DECIMAL STRING the upstream printed, carried
	// unrounded into a NUMERIC column (ADR-0003). Never a float64: a JSON number is an IEEE-754
	// double the instant anything parses it as one.
	//
	// Empty when no price was published AND when it is past [listingPriceMaxAge]: both mean "no
	// price to serve", and a reader must not be able to tell them apart and use the stale one.
	PriceUSD string
	// PricedAt is the PLATFORM's own publication time for that price. Zero exactly when PriceUSD
	// is empty; a table CHECK enforces that the two travel together.
	PricedAt time.Time
	// Source names the aggregation platform the row came from. Upserts
	// and the prune are both scoped by it.
	Source string
}

// ListingDirectoryCensus is the storage layer's account of its own numbers: how large the cached
// set was and which bound removed the rest. As with [DirectoryRWACensus], a reader handed only the
// survivors cannot tell how big the population was or what shrank it, and a fail-closed bound whose
// effect is invisible looks like an upstream that went quiet.
//
// The arithmetic closes, and [ListingDirectoryCensus.Check] proves it:
//
//	Entries = Contracts + Classic   (the table CHECK makes the two forms exhaustive)
//	Priced <= Contracts
type ListingDirectoryCensus struct {
	// Entries counts fresh rows of every address form — the recognised
	// population, before the contract/classic split.
	Entries int
	// Contracts counts fresh rows whose address is a C-strkey.
	Contracts int
	// Classic counts fresh rows whose address is a `CODE-GISSUER` pair.
	Classic int
	// Stale counts rows PRESENT in the cache but past [listingRecognitionMaxAge], the fail-closed
	// shrink made visible. It is the only read-side evidence that the hourly sync has stopped:
	// the recognised set just gets smaller while every read succeeds. Stale rising toward
	// Entries + Stale is a dead sync, not a delisting.
	Stale int
	// Priced counts fresh CONTRACT rows whose price is itself inside [listingPriceMaxAge]. A fresh
	// row with a frozen price counts in Contracts and not here, which is why the clocks are split.
	Priced int
	// PricedClassic is the same count over the CLASSIC rows. [Store.ListingDirectoryByAddress]
	// serves both forms, so comparing its map size against Priced alone would compare against only
	// the contract half; it stays separate so (Priced <= Contracts) keeps its meaning.
	PricedClassic int
}

// Check returns the reason the census does not balance, or "" when it does. It is exported so a
// surface can declare its accounting UNBALANCED rather than serve figures nobody can reconcile.
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
// measured on `synced_at` (our sync clock). It is spliced into every read so the bound lives in
// SQL, not convention.
//
// 48 hours: an address-to-identity mapping changes at human cadence (relist, correction), so a
// too-TIGHT bound is the real cost. On an hourly sync a 2-hour bound closes the arm on one failed
// pass and the admitted set flaps on a rate limit. 48h means 48 consecutive missed passes, a DEAD
// sync, while an upstream CORRECTION still shows within two days. It is deliberately wider than
// [listingPriceMaxAge], which bounds a different kind of fact; a dead sync shows as
// [ListingDirectoryCensus.Stale].
const listingRecognitionMaxAge = "48 hours"

// listingPriceMaxAge is how long the PLATFORM's published price may be reused, measured on
// `priced_at` (the platform's clock), NOT `synced_at` (ours).
//
// A recent sync proves the FETCH is healthy, not that the price is fresh: a frozen price is
// re-copied every hour with a new `synced_at`, so a `synced_at` bound would launder it as fresh
// forever. A MISSING `priced_at` is REJECTED because freshness is then unverifiable.
//
// 24 hours because this price is corroboration, not a trading input; tighter would blank the
// price on most (illiquid) listings.
const listingPriceMaxAge = "24 hours"

// ListingPriceMaxFutureSkew is how far AHEAD of our clock a `priced_at` may sit and still count.
// Without a ceiling a future timestamp voids the 24-hour floor (a price stamped 2099 stays "fresh"
// for decades). It only absorbs NTP clock skew; the ingest sync rejects past it too, and
// [listingPriceMaxFutureSkew] is its SQL spelling.
const ListingPriceMaxFutureSkew = 5 * time.Minute

const listingPriceMaxFutureSkew = "5 minutes"

// listingIsContractSQL and listingIsClassicSQL split the cached set by address form; they are the
// exact twins of the table CHECK, which is what lets the census treat the two as exhaustive.
//
// They are REGEXES, not `LIKE 'C%'` shortcuts, on purpose: a classic asset code may itself begin
// with C (`C1USD-G...`, `CETES-G...`), and a LIKE would put those in the contract arm of every
// reader while the census still balanced. The classic code class is `[A-Za-z0-9]` because Stellar
// asset codes are case-SENSITIVE (MXNe, sUSD, yUSDC), matching asset_volume_character_rollup.go.
const (
	listingIsContractSQL = `address ~ '^C[A-Z2-7]{55}$'`
	listingIsClassicSQL  = `address ~ '^[A-Za-z0-9]{1,12}-G[A-Z2-7]{55}$'`
)

// listingRecognisedSQL is the recognition bound as a predicate. Both the census count and the row
// read use it, so they cannot disagree about who qualified.
const listingRecognisedSQL = `synced_at > now() - INTERVAL '` + listingRecognitionMaxAge + `'`

// listingPriceFreshSQL is the price bound as a predicate on the platform's clock. The `IS NOT NULL`
// arm explicitly rejects a missing publication time rather than leaving it to NULL semantics, and
// the ceiling arm is what makes the floor a bound: see [ListingPriceMaxFutureSkew].
const listingPriceFreshSQL = `price_usd IS NOT NULL
		   AND priced_at IS NOT NULL
		   AND priced_at > now() - INTERVAL '` + listingPriceMaxAge + `'
		   AND priced_at <= now() + INTERVAL '` + listingPriceMaxFutureSkew + `'`

// listingUpsertChunk bounds the multi-row upsert: 500 rows x 5 params = 2500 placeholders, far
// under Postgres's 65535 bind-param cap. Chunking exists because a third party sets the row count.
const listingUpsertChunk = 500

// ReplaceListingDirectory upserts the full entry set for one source and prunes that source's rows
// the upstream no longer carries.
//
// One transaction, as in [ReplaceDirectory]: now() is transaction-stable, so every row lands with an
// identical `synced_at` and the prune is exactly "same source, older synced_at". A partial failure
// rolls back, so the table never holds a half-applied snapshot or rows that disagree about their
// sync time (which would put some on the wrong side of the recognition bound).
func (s *Store) ReplaceListingDirectory(
	ctx context.Context, entries []ListingEntry, source string,
) (upserted, pruned int64, err error) {
	if source == "" {
		return 0, 0, errors.New("listing directory: source must be non-empty")
	}
	// An empty set means the fetch or parse broke, not that every Stellar asset was delisted at
	// once. Proceeding would prune the whole source and report a successful sync. An operator
	// emptying a source on purpose can DELETE directly. Same refusal as [ReplaceDirectory].
	if len(entries) == 0 {
		return 0, 0, errors.New("listing directory: refusing to sync an empty entry set (would prune the whole source)")
	}
	// Two upstream coin ids can name one address. Equal addresses in one upsert chunk make Postgres
	// reject the WHOLE statement ("cannot affect row a second time") and freeze the sync, so
	// collapse last-wins before chunking, as [dedupDirectoryEntriesByAddress] does.
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

// dedupListingEntriesByAddress collapses entries sharing an Address, last wins, preserving survivor
// order. One upsert chunk may hold each conflict key at most once, so this keeps a duplicate-address
// upstream from aborting [ReplaceListingDirectory]'s transaction.
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

// buildListingUpsert renders one multi-row upsert for a chunk: 5 positional params per row
// (address, listing_id, symbol, price_usd, priced_at) plus one shared source param at
// len(chunk)*5+1. synced_at is the transaction-stable now() that [ReplaceListingDirectory]'s prune
// relies on.
//
// The price and time placeholders carry EXPLICIT `::numeric` / `::timestamptz` casts: an untyped
// bind parameter whose type Postgres must infer is a bug class here, and the cast also documents
// that the price arrives as a decimal STRING parsed by Postgres, never a Go float.
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
// The price columns go through a CASE on [listingPriceFreshSQL], not a WHERE: past the price bound
// the row is STILL RECOGNISED and returned unpriced. A WHERE would delete the address from the
// recognised set whenever a thin listing's price went quiet, collapsing two independent facts.
// `price_usd::text` returns the stored NUMERIC digits verbatim; nothing converts to a float.
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
// One statement with FILTER aggregates, as in [directoryRWACensus], so the buckets come from ONE
// snapshot. Separate statements can straddle a sync or the moving `now()` the staleness bounds use,
// and fail to reconcile through no fault of the arithmetic.
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

// ListingDirectoryContracts returns the recognised CONTRACT rows of the cached listing directory
// plus the census of the whole cache.
//
//   - Only rows inside [listingRecognitionMaxAge]; the bound is in the SQL.
//   - Only C-strkey addresses. Classic rows are cached and counted ([ListingDirectoryCensus.Classic])
//     but not returned, since a classic asset's issuer is known by another route; the count shows
//     how much of the cache this read is NOT looking at.
//   - PriceUSD / PricedAt are filled only inside [listingPriceMaxAge]; past it the entry is still
//     recognised, with an empty price and zero time.
//
// There is no scan cap: the set is the few dozen Stellar assets a third party listed, and
// [ListingDirectoryCensus.Contracts] is what a caller compares len(entries) against.
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

// listingDirectoryCensus counts the whole cached set, bucketed the way
// the reader narrows it.
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

// listingDirectoryByAddressSQL reads EVERY recognised row, of either address form, for the surface
// that binds a verified-catalogue asset to a listing price.
//
// It is [listingDirectoryContractsSQL] without the address-form predicate, and that is the point:
// a classic asset can be listed under EITHER its `CODE-GISSUER` id or the C-strkey of the SAC its
// (code, issuer) derives, and a reader seeing one form misses whichever the platform published
// (EURC and AQUA appeared by classic id, USDC and XLM by SAC address only). The same two staleness
// bounds apply.
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

// ListingDirectoryByAddress returns every recognised row of the cached listing directory keyed by
// its exact Stellar address, plus the census of the whole cache.
//
// The map key is the ADDRESS AS PUBLISHED, with no code, symbol or case folding. That is the safety
// property callers rest on: PYUSD, USDT, USDC and even XLM are each worn by impersonating issuers,
// so a lookup by code would hand a real instrument's price to whoever minted the ticker. A strkey,
// or a code bound to its issuer's G-address, cannot be collided into.
//
// Same properties as [ListingDirectoryContracts]: the recognition bound is in the SQL, a row past
// the PRICE bound comes back recognised but unpriced, and there is no scan cap because the set is
// bounded by what a third party listed and the census is what len() is compared against.
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
