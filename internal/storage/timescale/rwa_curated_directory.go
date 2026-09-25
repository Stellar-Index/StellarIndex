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

// rwa_curated_directory — a third party's list of which Stellar addresses
// are tokenized real-world assets, and that party's own price for each,
// cached for the RWA surface's CURATED arm (migration 0161). Synced by
// `stellarindex-ops curated-rwa-sync`.
//
// WHAT A ROW MEANS, precisely. "Curator X states that this address is a
// real-world asset issued by company Y, of subclass Z, and values one
// token at P dollars." That is a curation — an editorial table the
// curator publishes and maintains — and it is the entire content of the
// claim. No signature, no proof of control, no attestation, no market.
//
// The first curator is the Stellar team's public Dune uploads. Its
// figures are what the "RWAs on Stellar" dashboard publishes, and the
// whole reason this table exists is that a reader comparing that
// dashboard with this index could not see line by line why they differ.
// Rows admitted on this table are served under their own basis and their
// own total, beside the verified set and never inside it.
//
// TWO CLOCKS, TWO BOUNDS, the same shape as asset_listing_directory:
//
//   - RECOGNITION ages on `synced_at`, OUR clock, bounded by
//     [curatedRWARecognitionMaxAge].
//   - The PRICE ages on `priced_at`, the CURATOR's own day stamp,
//     bounded by [curatedRWAPriceMaxAge].
//
// Both are spliced into the reader's SQL, never applied by a caller, so
// there is no code path that reads a stale row as fresh.

// CuratedRWAEntry is one row as the curator published it.
type CuratedRWAEntry struct {
	// Address is the Stellar address as published: a Soroban contract
	// C-strkey (for a classic asset, its Stellar Asset Contract) or a
	// classic CODE-GISSUER pair. The join key; never the code.
	Address string
	// AssetCode and AssetIssuer are what the curator printed beside the
	// address. Display and reconciliation only.
	AssetCode   string
	AssetIssuer string
	// Company is the curator's issuing-entity label, served verbatim.
	Company string
	// AssetSubclass is the curator's instrument class, served verbatim
	// beside this index's own vocabulary and never mapped onto it.
	AssetSubclass string
	// PriceUSD is the curator's price per token as a DECIMAL STRING —
	// the literal the upload printed. Empty when the curator published
	// none, or when the one it published is past [curatedRWAPriceMaxAge].
	PriceUSD string
	// PricedAt is the curator's own stamp for that price. Zero exactly
	// when PriceUSD is empty.
	PricedAt time.Time
	// Source is the provenance the sync run recorded.
	Source string
}

// CuratedRWACensus is the storage layer's account of one curator's cache,
// taken in the same statement as the read so the counts and the rows
// come from ONE snapshot.
//
// The arithmetic closes as [ListingDirectoryCensus]'s does, and
// [CuratedRWACensus.Check] proves it:
//
//	Entries = Contracts + Classic   (migration 0161's CHECK makes the two
//	                                 forms exhaustive)
//	Priced <= Contracts, PricedClassic <= Classic
type CuratedRWACensus struct {
	// Entries counts rows inside the recognition bound, of either
	// address form.
	Entries int
	// Contracts counts recognised rows whose address is a C-strkey —
	// the only form the curated arm serves.
	Contracts int
	// Classic counts recognised rows whose address is a `CODE-GISSUER`
	// pair. The arm does not serve them, so this is the count that says
	// how much of the curator's list was left out.
	Classic int
	// Priced counts recognised CONTRACT rows whose price is inside its
	// bound.
	Priced int
	// PricedClassic is the same count over the CLASSIC rows, kept apart
	// so Priced keeps comparing against what the arm can serve.
	PricedClassic int
	// Stale counts rows PRESENT in the cache but past the recognition
	// bound — the fail-closed shrink. This is the number to watch: it is
	// the only evidence, from this side, that the sync has stopped.
	Stale int
}

// Check returns the reason the census does not balance, or "" when it
// does.
func (c CuratedRWACensus) Check() string {
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

// curatedRWARecognitionMaxAge is how long a cached row may be reused as
// "the curator names this address". The sync runs daily; 48 hours means
// one missed run is survivable and two are not.
const curatedRWARecognitionMaxAge = "48 hours"

// curatedRWAPriceMaxAge is how long the curator's own price may be served
// after the day it was stamped. Uploaded price tables are refreshed on
// the curator's schedule, not ours, and the one behind the first curator
// was last updated seven days before this file was written — so the bound
// is a week, and a row past it comes back recognised and UNPRICED rather
// than disappearing.
const curatedRWAPriceMaxAge = "7 days"

const curatedRWARecognisedSQL = `synced_at > now() - INTERVAL '` + curatedRWARecognitionMaxAge + `'`

const curatedRWAPriceFreshSQL = `price_usd IS NOT NULL
		   AND priced_at IS NOT NULL
		   AND priced_at > now() - INTERVAL '` + curatedRWAPriceMaxAge + `'`

// curatedRWAUpsertChunk bounds the multi-row upsert: 500 rows × 9 params
// = 4500 placeholders, far under Postgres's 65535 bind-param cap.
const curatedRWAUpsertChunk = 500

// ReplaceCuratedRWADirectory makes the cache for one curator equal to
// `entries`: every entry is upserted with a fresh synced_at, and every
// row of that curator the run did NOT touch is pruned.
//
// An empty set is refused rather than applied. A run that fetched nothing
// cannot distinguish "the curator lists nothing" from "the fetch failed",
// and applying it would prune the whole curator into silence.
func (s *Store) ReplaceCuratedRWADirectory(
	ctx context.Context, curator string, entries []CuratedRWAEntry, source string,
) (upserted, pruned int64, err error) {
	if curator == "" {
		return 0, 0, errors.New("curated rwa directory: curator must be non-empty")
	}
	if source == "" {
		return 0, 0, errors.New("curated rwa directory: source must be non-empty")
	}
	if len(entries) == 0 {
		return 0, 0, errors.New("curated rwa directory: refusing to sync an empty entry set (would prune the whole curator)")
	}
	entries = dedupCuratedRWAEntries(entries)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("curated rwa directory: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	for start := 0; start < len(entries); start += curatedRWAUpsertChunk {
		end := min(start+curatedRWAUpsertChunk, len(entries))
		q, args := buildCuratedRWAUpsert(curator, entries[start:end], source)
		res, execErr := tx.ExecContext(ctx, q, args...)
		if execErr != nil {
			err = fmt.Errorf("curated rwa directory: upsert chunk [%d:%d): %w", start, end, execErr)
			return 0, 0, err
		}
		n, _ := res.RowsAffected()
		upserted += n
	}

	res, execErr := tx.ExecContext(ctx, `
		DELETE FROM rwa_curated_directory
		 WHERE curator = $1 AND synced_at < now()`, curator)
	if execErr != nil {
		err = fmt.Errorf("curated rwa directory: prune: %w", execErr)
		return 0, 0, err
	}
	pruned, _ = res.RowsAffected()

	if err = tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("curated rwa directory: commit: %w", err)
	}
	return upserted, pruned, nil
}

// dedupCuratedRWAEntries keeps the LAST entry per address. A curator's
// upload can carry an address twice; the multi-row upsert would then hit
// its own ON CONFLICT target twice in one statement, which Postgres
// refuses.
func dedupCuratedRWAEntries(entries []CuratedRWAEntry) []CuratedRWAEntry {
	idx := make(map[string]int, len(entries))
	out := make([]CuratedRWAEntry, 0, len(entries))
	for _, e := range entries {
		if i, ok := idx[e.Address]; ok {
			out[i] = e
			continue
		}
		idx[e.Address] = len(out)
		out = append(out, e)
	}
	return out
}

// buildCuratedRWAUpsert renders one multi-row INSERT … ON CONFLICT for a
// chunk. The price and its time travel as a pair: an empty price binds
// NULL for both, so the table's CHECK holds by construction rather than
// by the caller remembering it.
func buildCuratedRWAUpsert(curator string, entries []CuratedRWAEntry, source string) (string, []any) {
	var b strings.Builder
	b.WriteString(`
		INSERT INTO rwa_curated_directory
		    (curator, address, asset_code, asset_issuer, company, asset_subclass,
		     price_usd, priced_at, source, synced_at)
		VALUES `)
	args := make([]any, 0, len(entries)*9)
	for i, e := range entries {
		if i > 0 {
			b.WriteString(", ")
		}
		base := i * 9
		fmt.Fprintf(&b, "($%d, $%d, $%d, $%d, $%d, $%d, NULLIF($%d, '')::numeric, $%d::timestamptz, $%d, now())",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9)
		var pricedAt any
		if e.PriceUSD != "" && !e.PricedAt.IsZero() {
			pricedAt = e.PricedAt.UTC()
		} else {
			e.PriceUSD = ""
		}
		args = append(args, curator, e.Address, e.AssetCode, e.AssetIssuer, e.Company, e.AssetSubclass,
			e.PriceUSD, pricedAt, source)
	}
	b.WriteString(`
		ON CONFLICT (curator, address) DO UPDATE SET
		    asset_code     = EXCLUDED.asset_code,
		    asset_issuer   = EXCLUDED.asset_issuer,
		    company        = EXCLUDED.company,
		    asset_subclass = EXCLUDED.asset_subclass,
		    price_usd      = EXCLUDED.price_usd,
		    priced_at      = EXCLUDED.priced_at,
		    source         = EXCLUDED.source,
		    synced_at      = now()`)
	return b.String(), args
}

// curatedRWAByAddressSQL serves every recognised row of one curator. The
// price columns come back NULL when the curator's own stamp is past its
// bound, so a stale price reads as an absence and never as a value.
const curatedRWAByAddressSQL = `
		SELECT address, asset_code, asset_issuer, company, asset_subclass,
		       CASE WHEN ` + curatedRWAPriceFreshSQL + `
		            THEN price_usd::text END AS price_usd,
		       CASE WHEN ` + curatedRWAPriceFreshSQL + `
		            THEN priced_at END       AS priced_at,
		       source
		  FROM rwa_curated_directory
		 WHERE curator = $1
		   AND ` + curatedRWARecognisedSQL + `
		 ORDER BY address`

// curatedRWACensusSQL counts every bucket in one pass, for one curator.
// The address-form predicates are the listing directory's: migration
// 0161's CHECK is the same pair of regexes as 0160's.
const curatedRWACensusSQL = `
		SELECT
		  count(*) FILTER (WHERE ` + curatedRWARecognisedSQL + `)          AS entries,
		  count(*) FILTER (WHERE ` + curatedRWARecognisedSQL + `
		                     AND ` + listingIsContractSQL + `)             AS contracts,
		  count(*) FILTER (WHERE ` + curatedRWARecognisedSQL + `
		                     AND ` + listingIsClassicSQL + `)              AS classic,
		  count(*) FILTER (WHERE ` + curatedRWARecognisedSQL + `
		                     AND ` + listingIsContractSQL + `
		                     AND ` + curatedRWAPriceFreshSQL + `)          AS priced,
		  count(*) FILTER (WHERE ` + curatedRWARecognisedSQL + `
		                     AND ` + listingIsClassicSQL + `
		                     AND ` + curatedRWAPriceFreshSQL + `)          AS priced_classic,
		  count(*) FILTER (WHERE NOT (` + curatedRWARecognisedSQL + `))    AS stale
		  FROM rwa_curated_directory
		 WHERE curator = $1`

// CuratedRWADirectoryByAddress returns one curator's recognised rows keyed
// by exact address, with the census of that curator's whole cache.
//
// The map key is the ADDRESS AS PUBLISHED and nothing else. A lookup by
// code would hand a curator's price to whichever account minted the
// ticker first; a 56-character strkey, or a code bound to its issuer's
// G-address, cannot be collided into.
func (s *Store) CuratedRWADirectoryByAddress(
	ctx context.Context, curator string,
) (map[string]CuratedRWAEntry, CuratedRWACensus, error) {
	if curator == "" {
		return nil, CuratedRWACensus{}, errors.New("curated rwa directory: curator must be non-empty")
	}

	// REPEATABLE READ, not the pool default: under READ COMMITTED each
	// statement in a transaction still takes its OWN snapshot, so merely
	// wrapping the two reads in a BeginTx would not deliver the "ONE
	// snapshot" this method documents. Same isolation, same reason, as
	// [Store.restampTradesUSDVolume]'s before-image/update pair.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, CuratedRWACensus{}, fmt.Errorf("curated rwa directory: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var census CuratedRWACensus
	if err := tx.QueryRowContext(ctx, curatedRWACensusSQL, curator).Scan(
		&census.Entries, &census.Contracts, &census.Classic,
		&census.Priced, &census.PricedClassic, &census.Stale); err != nil {
		return nil, CuratedRWACensus{}, fmt.Errorf("curated rwa directory: census: %w", err)
	}

	rows, err := tx.QueryContext(ctx, curatedRWAByAddressSQL, curator)
	if err != nil {
		return nil, CuratedRWACensus{}, fmt.Errorf("curated rwa directory: by address: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]CuratedRWAEntry, 64)
	for rows.Next() {
		var (
			e        CuratedRWAEntry
			price    sql.NullString
			pricedAt sql.NullTime
		)
		if err := rows.Scan(&e.Address, &e.AssetCode, &e.AssetIssuer, &e.Company, &e.AssetSubclass,
			&price, &pricedAt, &e.Source); err != nil {
			return nil, CuratedRWACensus{}, fmt.Errorf("curated rwa directory: scan row: %w", err)
		}
		// Both or neither, matching what the table CHECK stores.
		if price.Valid && pricedAt.Valid {
			e.PriceUSD = price.String
			e.PricedAt = pricedAt.Time.UTC()
		}
		out[e.Address] = e
	}
	if err := rows.Err(); err != nil {
		return nil, CuratedRWACensus{}, fmt.Errorf("curated rwa directory: rows: %w", err)
	}
	return out, census, nil
}
