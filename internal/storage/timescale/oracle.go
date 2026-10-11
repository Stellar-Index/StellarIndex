package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Freshness convention for this file's Latest* readers: each is a
// bare "most recent row per key" pick (DISTINCT ON or ORDER BY ...
// LIMIT 1) with no age bound — the caller owns checking a returned
// row's Timestamp against its own staleness threshold. The one
// exception is LatestOracleStreams, which filters `ts` in SQL itself.

// InsertOracleUpdate writes one oracle observation. Idempotent on
// (source, ledger, tx_hash, op_index, ts).
func (s *Store) InsertOracleUpdate(ctx context.Context, u canonical.OracleUpdate) error {
	if err := u.Validate(); err != nil {
		return err
	}
	// Atomic + idempotent-corrective, mirroring InsertTrade: the oracle
	// row upsert and the per-source entry-tally bump (migration 0035)
	// are one statement. On conflict we DO UPDATE every value column plus
	// derive_generation, guarded by
	// `oracle_updates.derive_generation <= EXCLUDED.derive_generation`
	// (migration 0109): a re-derive with a higher-or-equal
	// generation lands its corrected price in place, while a lower
	// generation (a live gen-0 replay) can never revert a correction —
	// a plain `DO NOTHING` would silently discard corrected
	// re-derives. `xmax = 0` distinguishes a fresh insert from an
	// on-conflict update, and the `HAVING count(*) FILTER (WHERE inserted)
	// > 0` gate means a re-walked duplicate OR a re-derive update never
	// inflates the tally. Oracle ingest doesn't need the inserted count,
	// so this stays an Exec.
	const q = `
        WITH ins AS (
            INSERT INTO oracle_updates (
                source, contract_id,
                ledger, tx_hash, op_index, ts,
                asset, quote,
                price, decimals,
                confidence, observer, derive_generation,
                published_price
            ) VALUES (
                $1, NULLIF($2, ''),
                $3, $4, $5, $6,
                $7, $8,
                $9, $10,
                NULLIF($11, 0.0), NULLIF($12, ''), $13,
                $14
            )
            ON CONFLICT (source, ledger, tx_hash, op_index, ts) DO UPDATE SET
                contract_id       = EXCLUDED.contract_id,
                asset             = EXCLUDED.asset,
                quote             = EXCLUDED.quote,
                price             = EXCLUDED.price,
                decimals          = EXCLUDED.decimals,
                confidence        = EXCLUDED.confidence,
                observer          = EXCLUDED.observer,
                published_price   = EXCLUDED.published_price,
                derive_generation = EXCLUDED.derive_generation
              WHERE oracle_updates.derive_generation <= EXCLUDED.derive_generation
            RETURNING (xmax = 0) AS inserted
        )
        INSERT INTO source_entry_counts AS sec (source, entry_count, updated_at)
        SELECT $1, count(*) FILTER (WHERE inserted), now() FROM ins
        HAVING count(*) FILTER (WHERE inserted) > 0
        ON CONFLICT (source) DO UPDATE
          SET entry_count = sec.entry_count + EXCLUDED.entry_count,
              updated_at  = EXCLUDED.updated_at
    `
	_, err := s.db.ExecContext(ctx, q,
		u.Source, u.ContractID,
		u.Ledger, u.TxHash, u.OpIndex, u.Timestamp.UTC(),
		u.Asset.String(), u.Quote.String(),
		u.Price, int(u.Decimals),
		u.Confidence, u.Observer, s.deriveGeneration,
		publishedPriceArg(u.PublishedPrice),
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertOracleUpdate: %w", err)
	}
	return nil
}

// LatestOracleUpdateForAsset returns the most recent observation
// for an asset from the given source. Returns (nil, ErrNotFound) if
// no row matches. Unbounded in age — see the freshness convention
// note above InsertOracleUpdate.
func (s *Store) LatestOracleUpdateForAsset(ctx context.Context, source string, asset canonical.Asset) (*canonical.OracleUpdate, error) {
	const q = `
        SELECT source, COALESCE(contract_id, ''),
               ledger, tx_hash, op_index, ts,
               asset, quote,
               price, decimals,
               COALESCE(confidence, 0),
               COALESCE(observer, ''),
               published_price
          FROM oracle_updates
         WHERE source = $1
           AND asset  = $2
         ORDER BY ts DESC, ledger DESC
         LIMIT 1
    `
	var (
		u        canonical.OracleUpdate
		assetStr string
		quoteStr string
		decimals int
	)
	err := s.db.QueryRowContext(ctx, q, source, asset.String()).Scan(
		&u.Source, &u.ContractID,
		&u.Ledger, &u.TxHash, &u.OpIndex, &u.Timestamp,
		&assetStr, &quoteStr,
		&u.Price, &decimals,
		&u.Confidence,
		&u.Observer,
		publishedPriceDest{&u.PublishedPrice},
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("timescale: LatestOracleUpdateForAsset: %w", err)
	}

	parsedAsset, err := canonical.ParseAsset(assetStr)
	if err != nil {
		return nil, fmt.Errorf("timescale: asset %q: %w", assetStr, err)
	}
	parsedQuote, err := canonical.ParseAsset(quoteStr)
	if err != nil {
		return nil, fmt.Errorf("timescale: quote %q: %w", quoteStr, err)
	}
	u.Asset = parsedAsset
	u.Quote = parsedQuote
	u.Decimals = uint8(decimals)
	return &u, nil
}

// LatestOracleUpdatesForAsset returns the most-recent observation
// for asset from EVERY source that has observed it. Returns an
// empty slice + nil error when the asset has no observations.
//
// Optional filter: if sourceFilter != "", the result is restricted
// to that single source (equivalent to calling
// [LatestOracleUpdateForAsset] and wrapping in a 1-element slice,
// but with an empty slice instead of ErrNotFound for "none").
//
// Single-key wrapper around [LatestOracleUpdatesForAssets] —
// preserved for callers that haven't switched to the multi-key
// shape yet.
//
// Unbounded in age — see the freshness convention note above
// InsertOracleUpdate.
func (s *Store) LatestOracleUpdatesForAsset(ctx context.Context, asset canonical.Asset, sourceFilter string) ([]canonical.OracleUpdate, error) {
	return s.LatestOracleUpdatesForAssets(ctx, []canonical.Asset{asset}, sourceFilter)
}

// LatestOracleUpdatesForAssets is the multi-key variant — returns
// the most-recent observation per (source, quote) across the union
// of the supplied asset keys. The DISTINCT ON (source, quote) pick
// keeps the observation with the highest (ts, ledger) per that pair,
// regardless of which input key it matched.
//
// quote is INCLUDED in the distinct key rather than collapsed to one
// row per source: a single source can publish the SAME base asset
// against two different quotes as two independent live feeds (e.g.
// Redstone's EUROC/EUR and EUROC/USD — see feeds.go). Collapsing on
// source alone silently kept whichever quote happened to publish most
// recently, which is a live-feed coin flip, not a "latest reading".
// Callers that want exactly one row disambiguate with quoteFilter (the
// v1 handler's `?quote=`); an empty quoteFilter returns every live
// quote so the caller can see the ambiguity rather than have it hidden.
//
// Use case: the v1 handler calls this with a translation list —
// e.g. user-facing `native` expands to `[native, crypto:XLM]`
// because Reflector publishes XLM under the global crypto ticker
// rather than the per-network "native" form.
//
// Unbounded in age — see the freshness convention note above
// InsertOracleUpdate.
func (s *Store) LatestOracleUpdatesForAssets(ctx context.Context, assets []canonical.Asset, sourceFilter string) ([]canonical.OracleUpdate, error) {
	if len(assets) == 0 {
		return nil, nil
	}
	keys := make([]string, len(assets))
	for i, a := range assets {
		keys[i] = a.String()
	}
	// Aggregate-then-fetch instead of DISTINCT ON over every matching row:
	// max(ts) per (source, asset, quote) — the compress_segmentby key — is
	// answered from compressed-batch metadata, and the lateral fetches one
	// stream's newest rows by exact ts. The DISTINCT ON form sorted the
	// asset's whole history (318,908 rows, 541 ms on r1).
	// OFFSET 0 keeps the lateral a parameterised nested loop; flattened,
	// the planner hash-joins against a full scan of the open chunk.
	const q = `
        WITH latest AS (
            SELECT source, asset, quote, max(ts) AS ts
              FROM oracle_updates
             WHERE asset = ANY($1)
               AND ($2 = '' OR source = $2)
             GROUP BY source, asset, quote
        )
        SELECT DISTINCT ON (o.source, o.quote)
               o.source, COALESCE(o.contract_id, ''),
               o.ledger, o.tx_hash, o.op_index, o.ts,
               o.asset, o.quote,
               o.price, o.decimals,
               COALESCE(o.confidence, 0),
               COALESCE(o.observer, ''),
               o.published_price
          FROM latest l
          CROSS JOIN LATERAL (
                SELECT * FROM oracle_updates u
                 WHERE u.asset = ANY($1)
                   AND u.source = l.source AND u.asset = l.asset
                   AND u.quote = l.quote AND u.ts = l.ts
                OFFSET 0
          ) o
         ORDER BY o.source, o.quote, o.ts DESC, o.ledger DESC
    `
	rows, err := s.db.QueryContext(ctx, q, keys, sourceFilter)
	if err != nil {
		return nil, fmt.Errorf("timescale: LatestOracleUpdatesForAssets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []canonical.OracleUpdate
	for rows.Next() {
		var (
			u        canonical.OracleUpdate
			assetStr string
			quoteStr string
			decimals int
		)
		if err := rows.Scan(
			&u.Source, &u.ContractID,
			&u.Ledger, &u.TxHash, &u.OpIndex, &u.Timestamp,
			&assetStr, &quoteStr,
			&u.Price, &decimals,
			&u.Confidence, &u.Observer,
			publishedPriceDest{&u.PublishedPrice},
		); err != nil {
			return nil, fmt.Errorf("timescale: LatestOracleUpdatesForAsset scan: %w", err)
		}
		parsedAsset, err := canonical.ParseAsset(assetStr)
		if err != nil {
			return nil, fmt.Errorf("timescale: asset %q: %w", assetStr, err)
		}
		parsedQuote, err := canonical.ParseAsset(quoteStr)
		if err != nil {
			return nil, fmt.Errorf("timescale: quote %q: %w", quoteStr, err)
		}
		u.Asset = parsedAsset
		u.Quote = parsedQuote
		u.Decimals = uint8(decimals)
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LatestOracleUpdatesForAsset rows: %w", err)
	}
	return out, nil
}

// LatestAggregatorPricesForPair returns the most-recent
// oracle_updates row per source for the given (base, quote) across
// the supplied source list — the seam Phase 1.3's `aggregator_avg`
// price-authority tier reads. Caller passes the aggregator-class
// source names (typically every Source where
// `external.Registry[name].Class == external.ClassAggregator`);
// the storage layer doesn't repeat that classification.
//
// Returns rows in the order Postgres scans them; callers that want
// alphabetical or weighted order sort client-side. An empty source
// list returns (nil, nil) — caller handled the "no aggregators
// configured" case.
//
// The query uses DISTINCT ON (source) over (source, ts DESC,
// ledger DESC), so each source contributes its single most-recent
// observation in the (base, quote) pair. Unbounded in age — see the
// freshness convention note above InsertOracleUpdate.
// unbounded-latest-ok: the latest observation per source is served with its age; a floor would drop quiet sources.
func (s *Store) LatestAggregatorPricesForPair(ctx context.Context, base, quote canonical.Asset, sources []string) ([]canonical.OracleUpdate, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	const q = `
        SELECT DISTINCT ON (source)
               source, COALESCE(contract_id, ''),
               ledger, tx_hash, op_index, ts,
               asset, quote,
               price, decimals,
               COALESCE(confidence, 0),
               COALESCE(observer, ''),
               published_price
          FROM oracle_updates
         WHERE asset  = $1
           AND quote  = $2
           AND source = ANY($3)
         ORDER BY source, ts DESC, ledger DESC
    `
	rows, err := s.db.QueryContext(ctx, q,
		base.String(), quote.String(), sources)
	if err != nil {
		return nil, fmt.Errorf("timescale: LatestAggregatorPricesForPair: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []canonical.OracleUpdate
	for rows.Next() {
		var (
			u        canonical.OracleUpdate
			assetStr string
			quoteStr string
			decimals int
		)
		if err := rows.Scan(
			&u.Source, &u.ContractID,
			&u.Ledger, &u.TxHash, &u.OpIndex, &u.Timestamp,
			&assetStr, &quoteStr,
			&u.Price, &decimals,
			&u.Confidence, &u.Observer,
			publishedPriceDest{&u.PublishedPrice},
		); err != nil {
			return nil, fmt.Errorf("timescale: LatestAggregatorPricesForPair scan: %w", err)
		}
		parsedAsset, err := canonical.ParseAsset(assetStr)
		if err != nil {
			return nil, fmt.Errorf("timescale: asset %q: %w", assetStr, err)
		}
		parsedQuote, err := canonical.ParseAsset(quoteStr)
		if err != nil {
			return nil, fmt.Errorf("timescale: quote %q: %w", quoteStr, err)
		}
		u.Asset = parsedAsset
		u.Quote = parsedQuote
		u.Decimals = uint8(decimals)
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LatestAggregatorPricesForPair rows: %w", err)
	}
	return out, nil
}

// LatestOracleObservation returns the single most-recent
// oracle_updates row for `source` whose asset matches ANY of
// baseKeys AND whose quote matches ANY of quoteKeys. The key-set
// shape exists for XLM's dual identity (`native` vs `crypto:XLM`)
// — the divergence package's on-chain oracle references expand a
// canonical pair into both forms before calling here.
//
// Returns (nil, nil) — NOT ErrNotFound — when no row matches:
// this method implements internal/divergence.OracleReader, whose
// contract maps "no observation" to ErrAssetUnsupported without
// importing this package's sentinel. Empty inputs are treated as
// "no match" for the same reason.
//
// Unbounded in age — see the freshness convention note above
// InsertOracleUpdate.
func (s *Store) LatestOracleObservation(ctx context.Context, source string, baseKeys, quoteKeys []string) (*canonical.OracleUpdate, error) {
	if source == "" || len(baseKeys) == 0 || len(quoteKeys) == 0 {
		return nil, nil
	}
	const q = `
        SELECT source, COALESCE(contract_id, ''),
               ledger, tx_hash, op_index, ts,
               asset, quote,
               price, decimals,
               COALESCE(confidence, 0),
               COALESCE(observer, ''),
               published_price
          FROM oracle_updates
         WHERE source = $1
           AND asset  = ANY($2)
           AND quote  = ANY($3)
         ORDER BY ts DESC, ledger DESC
         LIMIT 1
    `
	var (
		u        canonical.OracleUpdate
		assetStr string
		quoteStr string
		decimals int
	)
	err := s.db.QueryRowContext(ctx, q,
		source, baseKeys, quoteKeys).Scan(
		&u.Source, &u.ContractID,
		&u.Ledger, &u.TxHash, &u.OpIndex, &u.Timestamp,
		&assetStr, &quoteStr,
		&u.Price, &decimals,
		&u.Confidence, &u.Observer,
		publishedPriceDest{&u.PublishedPrice},
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("timescale: LatestOracleObservation: %w", err)
	}
	parsedAsset, err := canonical.ParseAsset(assetStr)
	if err != nil {
		return nil, fmt.Errorf("timescale: asset %q: %w", assetStr, err)
	}
	parsedQuote, err := canonical.ParseAsset(quoteStr)
	if err != nil {
		return nil, fmt.Errorf("timescale: quote %q: %w", quoteStr, err)
	}
	u.Asset = parsedAsset
	u.Quote = parsedQuote
	u.Decimals = uint8(decimals)
	return &u, nil
}

// CountOracleUpdates returns the row count in oracle_updates.
// Diagnostic helper, not for production hot paths.
func (s *Store) CountOracleUpdates(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM oracle_updates -- totality: includes unmapped`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("timescale: CountOracleUpdates: %w", err)
	}
	return n, nil
}

// LatestOracleStreams returns one row per (source, asset, quote)
// triple — the most-recent observation in the trailing 7d window.
// Backs the /v1/oracles/streams listing (the second table on the
// explorer's /oracles page). Sources with no observation in the
// window are absent from the result.
//
// 7d window matches the "live stream" semantic — observations
// older than that signal a dead feed and shouldn't surface as
// "current" on the page; the historical trail still lives in the
// hypertable.
func (s *Store) LatestOracleStreams(ctx context.Context) ([]canonical.OracleUpdate, error) {
	const q = `
        SELECT DISTINCT ON (source, asset, quote)
               source, COALESCE(contract_id, ''),
               ledger, tx_hash, op_index, ts,
               asset, quote,
               price, decimals,
               COALESCE(confidence, 0),
               COALESCE(observer, ''),
               published_price
          FROM oracle_updates -- totality: includes unmapped
         WHERE ts > NOW() - INTERVAL '7 days'
         ORDER BY source, asset, quote, ts DESC
    `
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: LatestOracleStreams: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []canonical.OracleUpdate
	dropped := 0
	for rows.Next() {
		var u canonical.OracleUpdate
		var assetStr, quoteStr string
		var decimals int16
		if err := rows.Scan(
			&u.Source, &u.ContractID,
			&u.Ledger, &u.TxHash, &u.OpIndex, &u.Timestamp,
			&assetStr, &quoteStr,
			&u.Price, &decimals,
			&u.Confidence, &u.Observer,
			publishedPriceDest{&u.PublishedPrice},
		); err != nil {
			return nil, fmt.Errorf("timescale: LatestOracleStreams scan: %w", err)
		}
		u.Decimals = uint8(decimals)
		// A parse failure still drops the row — there is nothing sane to
		// serve for an asset we cannot name — but it is never SILENT.
		// A bare `continue` with no log, metric or error would make a row
		// whose stored canonical text the running binary could not parse
		// vanish from /v1/oracle/streams and the explorer's /oracles
		// page with zero signal.
		//
		// The likeliest cause is also the worst time for silence: the
		// documented remediation for a mislabelled oracle row is an
		// operator-run raw SQL UPDATE against this very column, which has
		// no CHECK constraint. A typo would have deleted the row from the
		// served surface rather than erroring, and the operator would see
		// it disappear and reasonably conclude the relabel worked.
		a, err := canonical.ParseAsset(assetStr)
		if err != nil {
			obs.OracleStreamRowsUnparsedTotal.WithLabelValues(u.Source, "asset").Inc()
			dropped++
			continue
		}
		u.Asset = a
		qa, err := canonical.ParseAsset(quoteStr)
		if err != nil {
			obs.OracleStreamRowsUnparsedTotal.WithLabelValues(u.Source, "quote").Inc()
			dropped++
			continue
		}
		u.Quote = qa
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LatestOracleStreams rows: %w", err)
	}
	if dropped > 0 {
		slog.Warn("timescale: LatestOracleStreams: rows dropped for unparseable asset/quote", "dropped", dropped, "returned", len(out))
	}
	return out, nil
}

// OracleDayPoint is one day bucket of one oracle's observations of one
// asset, read off the `oracle_prices_1d` continuous aggregate
// (migration 0034).
//
// Price is the bucket's CLOSING observation (`last(price, ts)`) — the
// same reduction the live snapshot applies when it takes the most recent
// row per stream, applied at a day's grain. The bucket carries no
// timestamp of its own, so two sources' closing observations within one
// day cannot be ordered against each other; a caller choosing between
// sources must do so on some other ground and say which it chose.
type OracleDayPoint struct {
	// Bucket is the UTC day (time_bucket('1 day', ts)).
	Bucket time.Time
	Source string
	Asset  canonical.Asset
	// Price is the day's closing value at Decimals scale, exactly as
	// stored — never normalised here (ADR-0003; the raw integer and its
	// scale travel together and the caller scales once).
	Price    *big.Int
	Decimals uint8
	// Observations is how many publications the oracle made in the day.
	// A bucket built from ONE observation and one built from a thousand
	// are different evidence for the same closing figure, and a surface
	// that publishes the figure should be able to say which it had.
	Observations int64
}

// DailyOraclePrices returns the daily closing observation for each
// (source, asset) among `assets`, denominated in `quote`, within the
// inclusive bucket range [from, to] — ascending by bucket, then asset,
// then source.
//
// This is the first read of the `oracle_prices_*` family; the rest of this
// package reads raw `oracle_updates`. The CAGG fits because the question is a
// HISTORY over a fixed grain, and it carries no retention policy
// (migration 0034), so the series reaches back as far as the oracle has
// published.
//
// There is deliberately NO carry-in row of the kind
// [Store.DailyCirculatingSupply] returns. A supply is a stock whose last
// reading stays true until something moves it; a price is an observation of a
// quantity that moves on its own, so the most recent bucket BEFORE the window
// says nothing about any day inside it. A caller wanting a value on a day the
// oracle was silent has to report the silence, not fill it.
//
// Unmapped `raw:` assets are dropped from `assets`, and the SQL refuses
// raw: rows too (an unvalidated Asset can still stringify to "raw:…"): they
// are record-layer only and must not reach a price surface. An empty `assets`
// (after that drop) returns (nil, nil): no keys is not a query.
func (s *Store) DailyOraclePrices(
	ctx context.Context,
	assets []canonical.Asset,
	quote canonical.Asset,
	from, to time.Time,
) ([]OracleDayPoint, error) {
	keys := make([]string, 0, len(assets))
	for _, a := range assets {
		if a.IsMapped() {
			keys = append(keys, a.String())
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	const q = `
        SELECT bucket, source, asset, last_price::text, last_decimals, observation_count
          FROM oracle_prices_1d
         WHERE asset = ANY($1)
           AND asset NOT LIKE 'raw:%'
           AND quote = $2
           AND bucket >= $3
           AND bucket <= $4
         ORDER BY bucket ASC, asset ASC, source ASC
    `
	rows, err := s.db.QueryContext(ctx, q, keys, quote.String(), from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("timescale: DailyOraclePrices: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]OracleDayPoint, 0, 512)
	for rows.Next() {
		var (
			p        OracleDayPoint
			assetStr string
			priceStr string
			decimals int
		)
		if err := rows.Scan(&p.Bucket, &p.Source, &assetStr, &priceStr, &decimals, &p.Observations); err != nil {
			return nil, fmt.Errorf("timescale: DailyOraclePrices scan: %w", err)
		}
		asset, err := canonical.ParseAsset(assetStr)
		if err != nil {
			return nil, fmt.Errorf("timescale: DailyOraclePrices asset %q: %w", assetStr, err)
		}
		price, ok := new(big.Int).SetString(priceStr, 10)
		if !ok {
			return nil, fmt.Errorf("timescale: DailyOraclePrices parse price %q", priceStr)
		}
		p.Bucket = p.Bucket.UTC()
		p.Asset = asset
		p.Price = price
		p.Decimals = uint8(decimals)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: DailyOraclePrices rows: %w", err)
	}
	return out, nil
}

// publishedPriceArg binds OracleUpdate.PublishedPrice: nil is SQL NULL.
func publishedPriceArg(p *canonical.Amount) any {
	if p == nil {
		return nil
	}
	return p.String()
}

// publishedPriceDest scans the nullable published_price NUMERIC; NULL
// leaves the field nil (canonical.Amount.Scan refuses NULL by design).
type publishedPriceDest struct{ dst **canonical.Amount }

func (d publishedPriceDest) Scan(src any) error {
	if src == nil {
		*d.dst = nil
		return nil
	}
	var a canonical.Amount
	if err := a.Scan(src); err != nil {
		return fmt.Errorf("published_price: %w", err)
	}
	*d.dst = &a
	return nil
}
