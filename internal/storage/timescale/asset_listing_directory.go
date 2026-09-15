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

// asset_listing_directory — the cached Stellar slice of an independent
// price-aggregation platform's own per-coin platform→address map
// (migration 0160), written by `stellarindex-ops listing-sync` and read
// from cache.
//
// WHAT A ROW MEANS, precisely. "The platform that publishes a price for
// coin X states that X lives at this Stellar address." That is a
// CORROBORATION — a second, independent party having arrived at the same
// (address → instrument) pairing this index arrived at by another route —
// and it is the entire content of the claim. The upstream carries no
// signature, no proof of control of the address, and no undertaking that
// the pairing is right; a listing is an editorial and commercial decision.
//
// WHAT A MISSING ROW MEANS. "Not listed", and nothing else. Almost every
// asset on the network is not listed. There are no scam flags in this
// table and there must not be: this surface can only ever ADD
// corroboration, never withhold, demote or accuse. The in-repo curated
// scam list and the curated directory's tag vocabulary
// ([DirectoryScamFlagTags]) remain the only places a negative verdict
// lives.
//
// TWO CLOCKS, TWO BOUNDS. The table holds two facts that rot at
// completely different rates, and conflating them is the failure this
// file is shaped to prevent:
//
//   - RECOGNITION ages on `synced_at`, OUR clock, bounded by
//     [listingRecognitionMaxAge].
//   - The PRICE ages on `priced_at`, the PLATFORM's own published
//     `last_updated`, bounded by [listingPriceMaxAge].
//
// Both bounds are spliced into the reader's SQL, never applied by a
// caller's convention — the same discipline, and for the same reason, as
// [assetPriceSnapshotMaxAge] on the listing's price join. A bound that
// lives in a caller is a bound one new caller can forget.

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
	// PriceUSD is the platform's published USD price as a DECIMAL
	// STRING — the literal the upstream printed, carried unrounded from
	// the wire into a NUMERIC column per ADR-0003. Never a float64 at
	// any point: the upstream ships a JSON number, and a JSON number is
	// an IEEE-754 double the instant anything parses it as one.
	//
	// Empty when the platform published no price, and ALSO empty when
	// the price it published is past [listingPriceMaxAge] — from a
	// reader's position those two are the same state, "no price to
	// serve", and the reader must not be able to tell them apart by
	// accident and use the stale one.
	PriceUSD string
	// PricedAt is the PLATFORM's own publication time for that price.
	// Zero exactly when PriceUSD is empty — the two travel together or
	// not at all, which migration 0160 enforces with a table CHECK
	// rather than trusting the writer.
	PricedAt time.Time
	// Source names the aggregation platform the row came from. Upserts
	// and the prune are both scoped by it.
	Source string
}

// ListingDirectoryCensus is the storage layer's account of its own
// numbers: not just what it returned, but how large the cached set was
// and which bound removed the rest.
//
// It exists for the reason [DirectoryRWACensus] does. A reader handed
// only the survivors cannot answer the two questions that matter when
// the set looks wrong — how big was the population, and what shrank it —
// and a fail-closed bound whose effect is invisible is indistinguishable
// from an upstream that went quiet.
//
// The arithmetic closes, and [ListingDirectoryCensus.Check] proves it:
//
//	Entries = Contracts + Classic   (the table CHECK makes the two forms
//	                                 exhaustive)
//	Priced <= Contracts
type ListingDirectoryCensus struct {
	// Entries counts fresh rows of every address form — the recognised
	// population, before the contract/classic split.
	Entries int
	// Contracts counts fresh rows whose address is a C-strkey.
	Contracts int
	// Classic counts fresh rows whose address is a `CODE-GISSUER` pair.
	Classic int
	// Stale counts rows that are PRESENT in the cache but past
	// [listingRecognitionMaxAge] — the fail-closed shrink, counted so it
	// is visible.
	//
	// This is the number to watch. It is the only evidence, from the
	// read side, that the hourly sync has stopped: the recognised set
	// simply gets smaller, every read succeeds, and nothing anywhere
	// else says why. Stale rising toward Entries + Stale is a dead
	// sync, not a delisting.
	Stale int
	// Priced counts fresh CONTRACT rows carrying a price that is itself
	// inside [listingPriceMaxAge]. A fresh row with a frozen price
	// counts in Contracts and not here, which is the whole point of
	// separating the two clocks.
	Priced int
}

// Check returns the reason the census does not balance, or "" when it
// does. Exported for the same reason [DirectoryRWACensus.Check] is: a
// surface publishing these counts should be able to declare its own
// accounting UNBALANCED rather than serve figures a reader would try,
// and fail, to reconcile.
func (c ListingDirectoryCensus) Check() string {
	if got := c.Contracts + c.Classic; got != c.Entries {
		return fmt.Sprintf("address forms sum to %d, not Entries %d", got, c.Entries)
	}
	if c.Priced > c.Contracts {
		return fmt.Sprintf("Priced %d exceeds Contracts %d", c.Priced, c.Contracts)
	}
	return ""
}

// listingRecognitionMaxAge is how long a cached row may be reused as
// RECOGNITION of an address — "the platform lists this address" —
// measured on `synced_at`, our own sync clock. Spliced into every read
// below so the bound is enforced in SQL, not by convention.
//
// 48 hours, and the value is argued rather than picked. An
// address→identity mapping does not rot by the hour: the platform's
// statement that a coin lives at an address changes when the platform
// relists or corrects it, which is a human-cadence event, not a market
// one. So the cost of a bound that is too TIGHT is real and one-sided —
// on an hourly sync a 2-hour bound closes the arm on a single failed
// pass, and the admitted set flaps in and out on nothing more than a
// rate limit. 48h is 48 consecutive missed passes before the arm closes,
// which is a sync that is DEAD rather than one having a bad afternoon,
// while still making an upstream CORRECTION visible within two days.
//
// It is deliberately much wider than [listingPriceMaxAge], because it
// bounds a different kind of fact. Widening it further trades away the
// only thing this bound buys — that a delisting or a correction
// propagates — for nothing, since the sync's own failure is already
// visible as [ListingDirectoryCensus.Stale].
const listingRecognitionMaxAge = "48 hours"

// listingPriceMaxAge is how long the PLATFORM's own published price may
// be reused, measured on `priced_at` — the platform's clock — and NOT on
// `synced_at`, ours.
//
// That distinction is the entire reason this constant is separate. A
// sync that succeeded five minutes ago proves that the FETCH is healthy;
// it proves nothing whatsoever about the price it fetched. If the
// upstream's price for a thinly traded asset froze two weeks ago, every
// hourly pass since has faithfully re-copied the same frozen number with
// a brand-new `synced_at`, and a bound measured on `synced_at` would
// launder it as fresh forever. This is the same distinction
// internal/divergence/coingecko.go's staleness gate draws (CS-089), and
// the same reason it rejects rather than trusts.
//
// A MISSING `priced_at` is likewise REJECTED, never waved through, for
// the reason that gate rejects an id absent from its own last-updated
// map: every request opts into the publication time, so its absence
// means the response did not honour the contract the bound rests on, and
// freshness is then not merely old but UNVERIFIABLE. Unverifiable
// freshness is the state a stale price is indistinguishable from.
//
// 24 hours because this price is corroboration, not a trading input —
// nothing in this repo quotes, settles or values a position from it, and
// the index computes its own prices from observed trades. A day-old
// aggregate is still a useful second opinion on what an asset is roughly
// worth; a week-old one is not. Tighter would blank the price on every
// illiquid listing, which is most of them.
const listingPriceMaxAge = "24 hours"

// listingIsContractSQL and listingIsClassicSQL split the cached set by
// address form, and they are the exact twins of migration 0160's CHECK —
// which is what lets the census arithmetic treat the two as exhaustive.
//
// They are REGEXES and not `LIKE 'C%'` / `LIKE '%-%'` shortcuts, and
// that is load-bearing rather than fussy: a classic asset code may
// itself begin with C, and two live upstream rows do exactly that
// (`C1USD-GDCDFF6Z…` and `CETES-GCRYUGD5…`). A `LIKE 'C%'` contract test
// would classify both as Soroban contracts, put them in the contract arm
// of every reader, and the census would still balance while doing it.
//
// The classic arm's code class is `[A-Za-z0-9]`, not `[A-Z0-9]`: Stellar
// asset codes are case-SENSITIVE alphanumerics and four live rows carry
// a lowercase letter (MXNe, sUSD, yUSDC, yETH). It is the same
// expression asset_volume_character_rollup.go uses for a classic id.
const (
	listingIsContractSQL = `address ~ '^C[A-Z2-7]{55}$'`
	listingIsClassicSQL  = `address ~ '^[A-Za-z0-9]{1,12}-G[A-Z2-7]{55}$'`
)

// listingRecognisedSQL is the recognition bound as a predicate: a row
// this sync wrote inside [listingRecognitionMaxAge]. Spelled once and
// used by BOTH the census count and the row read, so the two can never
// disagree about who qualified.
const listingRecognisedSQL = `synced_at > now() - INTERVAL '` + listingRecognitionMaxAge + `'`

// listingPriceFreshSQL is the price bound as a predicate, on the
// platform's clock. The `IS NOT NULL` arm is not redundant with the
// interval comparison — it is the explicit rejection of a missing
// publication time, stated so that reading this predicate answers the
// question rather than leaving it to SQL's NULL semantics.
const listingPriceFreshSQL = `price_usd IS NOT NULL
		   AND priced_at IS NOT NULL
		   AND priced_at > now() - INTERVAL '` + listingPriceMaxAge + `'`

// listingUpsertChunk bounds the multi-row upsert: 500 rows × 5 params =
// 2500 placeholders, far under Postgres's 65535 bind-param cap. The
// Stellar slice is ~50 rows today so one chunk covers it many times
// over; the chunking exists because the row count is set by how many
// assets a third party chooses to list, which is not a number this repo
// controls.
const listingUpsertChunk = 500

// ReplaceListingDirectory upserts the full entry set for one source and
// prunes the rows of that source the upstream no longer carries.
//
// Everything runs in one transaction, for the reason [ReplaceDirectory]
// does: now() is transaction-stable in Postgres, so every upserted row
// lands with an identical `synced_at` and the prune is exactly "same
// source, older synced_at". A partial failure rolls the whole sync back
// — the table never holds a half-applied upstream snapshot, and never a
// snapshot whose rows disagree about when they were synced (which would
// put some of them the wrong side of the recognition bound).
func (s *Store) ReplaceListingDirectory(
	ctx context.Context, entries []ListingEntry, source string,
) (upserted, pruned int64, err error) {
	if source == "" {
		return 0, 0, errors.New("listing directory: source must be non-empty")
	}
	// An empty set means the fetch or the parse upstream broke, not that
	// the platform delisted every Stellar asset at once. Proceeding would
	// prune every row of this source and present the result as a
	// successful sync — the fail-closed shrink turned into a wipe, with
	// nothing to distinguish it from a genuine delisting. Fail loudly
	// instead; an operator emptying a source on purpose can DELETE
	// directly. Identical refusal, identical reasoning, to
	// [ReplaceDirectory]'s.
	if len(entries) == 0 {
		return 0, 0, errors.New("listing directory: refusing to sync an empty entry set (would prune the whole source)")
	}
	// Two upstream coin ids can name one Stellar address — a relisting, a
	// duplicate record, a wrapped form catalogued twice. Two rows with an
	// equal address in one multi-row upsert chunk make Postgres reject
	// the WHOLE statement ("ON CONFLICT DO UPDATE command cannot affect
	// row a second time"), which would freeze the sync until the upstream
	// happened to fix itself. Collapse duplicates last-wins before
	// chunking, exactly as [dedupDirectoryEntriesByAddress] does for the
	// curated directory (RA-3).
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

// dedupListingEntriesByAddress collapses entries sharing an Address to a
// single entry, last-wins (the later occurrence in upstream order
// overrides the earlier), preserving the original relative order of the
// survivors. A single multi-row upsert chunk may contain each conflict
// key at most once, so this is what keeps a duplicate-address upstream
// from aborting [ReplaceListingDirectory]'s whole transaction.
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

// buildListingUpsert renders one multi-row upsert statement for a chunk.
// Per row: 5 positional params (address, listing_id, symbol, price_usd,
// priced_at); the shared source param sits once at position
// len(chunk)*5+1 and every row references it. synced_at is now() —
// transaction-stable, which is what [ReplaceListingDirectory]'s prune
// relies on.
//
// The price and time placeholders carry EXPLICIT `::numeric` /
// `::timestamptz` casts. An untyped bind parameter next to a value
// Postgres has to infer a type for is a whole bug class in this tree —
// the same discipline the assets cursor's `$n::numeric` / `$n::int`
// placeholders apply — and here it also documents, in the statement
// itself, that the price arrives as a decimal STRING and is parsed by
// Postgres, never by a Go float.
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

		// A price and its publication time are bound together or both
		// NULL — never one without the other. The table CHECK refuses the
		// mixed row, so this is the writer agreeing with the schema
		// rather than relying on it to catch a mistake.
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
// The price columns come back through a CASE on [listingPriceFreshSQL]
// rather than through a WHERE: past the price bound the row is STILL
// RECOGNISED and is still returned, simply unpriced. Putting the price
// bound in the WHERE would silently delete the address from the
// recognised set every time a thinly traded listing's price went quiet —
// two independent facts collapsed into one, with the wrong one winning.
//
// `price_usd::text` hands the caller the stored decimal verbatim. Nothing
// on this path converts to a float: the value crossed the wire as a
// decimal string, is stored as NUMERIC, and comes back as the same
// digits.
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
// One statement with FILTER aggregates rather than five round trips, for
// the reason [directoryRWACensus] takes the same shape: the buckets have
// to come from ONE snapshot. Counts taken across separate statements can
// straddle a sync — or, worse here, straddle the moving `now()` the two
// staleness bounds are measured against — and fail to reconcile through
// no fault of the arithmetic.
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
		                     AND ` + listingPriceFreshSQL + `)              AS priced
		  FROM asset_listing_directory`

// ListingDirectoryContracts returns the recognised CONTRACT rows of the
// cached listing directory, together with the census of the whole cache.
//
// Three properties, each deliberate:
//
//   - Only rows inside [listingRecognitionMaxAge]. The bound is in the
//     SQL; a caller cannot forget it.
//   - Only C-strkey addresses. The classic rows are cached and counted
//     ([ListingDirectoryCensus.Classic]) but not returned here — a
//     classic asset already has an issuer this index knows by another
//     route, so the contract arm is where an address would otherwise go
//     unrecognised. The count stays so the caller can see how much of
//     the cache this read is NOT looking at.
//   - PriceUSD / PricedAt are filled only when the platform's own
//     publication time is inside [listingPriceMaxAge]. Past it the entry
//     comes back with an empty price and a zero time, still recognised.
//
// There is no scan cap. The recognised set is bounded by how many
// Stellar assets a third party has chosen to list (~50 today), the prune
// keeps the table at that size, and [ListingDirectoryCensus.Contracts]
// is the number a caller compares len(entries) against — a cap with no
// truncation signal would be worse than none.
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
		// Both or neither, matching what the table CHECK stores and what
		// [ListingEntry] documents: a caller testing PriceUSD != "" can
		// rely on PricedAt being set, and vice versa.
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
		&c.Entries, &c.Contracts, &c.Classic, &c.Stale, &c.Priced,
	)
	if err != nil {
		return ListingDirectoryCensus{}, fmt.Errorf("listing directory: census: %w", err)
	}
	return c, nil
}
