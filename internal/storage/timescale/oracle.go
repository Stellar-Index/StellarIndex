package timescale

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// Freshness convention for this file's Latest* readers: each is a bare "most recent row per key"
// pick with no age bound, so the caller checks a returned row's Timestamp against its own
// staleness threshold. The exception is LatestOracleStreams, which filters `ts` in SQL.

// InsertOracleUpdate writes one oracle observation.
func (s *Store) InsertOracleUpdate(ctx context.Context, u canonical.OracleUpdate) error {
	if err := u.Validate(); err != nil {
		return err
	}
	// Atomic and idempotent-corrective, mirroring InsertTrade: the upsert and the per-source
	// entry-tally bump are one statement. On conflict the value columns update, guarded by
	// `derive_generation <= EXCLUDED.derive_generation`, so a re-derive lands its corrected price
	// while a lower generation (a live gen-0 replay) never reverts it. `xmax = 0` separates a
	// fresh insert from an update, and the `HAVING count(*) FILTER (WHERE inserted) > 0` gate
	// keeps duplicates and re-derives from inflating the tally.
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

// LatestOracleUpdateForAsset returns the most recent observation for an asset from the given source.
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

// LatestOracleUpdatesForAsset returns the newest observation for asset from EVERY source that observed it (empty slice and nil error if none), optionally restricted by a non-empty sourceFilter; wraps [LatestOracleUpdatesForAssets].
func (s *Store) LatestOracleUpdatesForAsset(ctx context.Context, asset canonical.Asset, sourceFilter string) ([]canonical.OracleUpdate, error) {
	return s.LatestOracleUpdatesForAssets(ctx, []canonical.Asset{asset}, sourceFilter)
}

// LatestOracleUpdatesForAssets returns the newest observation per (source, quote) across the union of the supplied alias-expanded asset keys (e.g. `native` and `crypto:XLM`), highest (ts, ledger) winning.
func (s *Store) LatestOracleUpdatesForAssets(ctx context.Context, assets []canonical.Asset, sourceFilter string) ([]canonical.OracleUpdate, error) {
	if len(assets) == 0 {
		return nil, nil
	}
	keys := make([]string, len(assets))
	for i, a := range assets {
		keys[i] = a.String()
	}
	recent, err := s.queryOracleLatest(ctx, oracleLatestRecentQuery, keys, sourceFilter)
	if err != nil {
		return nil, err
	}
	allTime, err := s.oracleLatestAllTime(ctx, keys, sourceFilter)
	if err != nil {
		return nil, err
	}
	return mergeOracleLatest(recent, allTime), nil
}

// oracleLatestQueryTmpl is aggregate-then-fetch instead of DISTINCT ON over every matching row:
// max(ts) per (source, asset, quote) — the compress_segmentby key — is answered from
// compressed-batch metadata, and the lateral fetches one stream's newest rows by exact ts. The
// DISTINCT ON form sorted the asset's whole history (318,908 rows, 541 ms on r1).
// OFFSET 0 keeps the lateral a parameterised nested loop; flattened, the planner hash-joins
// against a full scan of the open chunk. %[1]s names the CTE; %[2]s and %[3]s bound ts on both reads.
// unbounded-latest-ok: the all-time instance must reach a stream silent for months; its age is served.
const oracleLatestQueryTmpl = `
        WITH %[1]s AS (
            SELECT source, asset, quote, max(ts) AS ts
              FROM oracle_updates
             WHERE asset = ANY($1)
               AND ($2 = '' OR source = $2) %[2]s
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
          FROM %[1]s l
          CROSS JOIN LATERAL (
                SELECT * FROM oracle_updates u
                 WHERE u.asset = ANY($1)
                   AND u.source = l.source AND u.asset = l.asset
                   AND u.quote = l.quote AND u.ts = l.ts %[3]s
                OFFSET 0
          ) o
         ORDER BY o.source, o.quote, o.ts DESC, o.ledger DESC
    `

var (
	// The all-time read plans and scans every chunk (~100 ms on r1's 96); bounding ts lets
	// TimescaleDB exclude all but the newest few (~15 ms).
	oracleLatestAllTimeQuery = fmt.Sprintf(oracleLatestQueryTmpl, "latest", "", "")
	oracleLatestRecentQuery  = fmt.Sprintf(oracleLatestQueryTmpl, "recent",
		"AND ts > now() - interval '48 hours'", "AND u.ts > now() - interval '48 hours'")
)

// oracleLatestAllTimeTTL bounds how long a re-derive of a stream silent for 48 h can go unseen:
// only such streams take their row from the all-time read, and live writes always land in the
// recent window, so nothing else can change that read's answer.
const (
	oracleLatestAllTimeTTL        = time.Minute
	oracleLatestAllTimeMaxEntries = 4096
)

type oracleAllTimeEntry struct {
	rows       []canonical.OracleUpdate
	at         time.Time
	refreshing bool
}

// oracleLatestAllTime serves the all-time read from a per-key cache, refreshing an expired entry
// in the background so a request pays for the full scan only on a key's first read.
func (s *Store) oracleLatestAllTime(ctx context.Context, keys []string, sourceFilter string) ([]canonical.OracleUpdate, error) {
	sorted := slices.Clone(keys)
	slices.Sort(sorted)
	key := strings.Join(sorted, "|") + "|" + sourceFilter

	s.oracleAllTimeMu.Lock()
	e, ok := s.oracleAllTime[key]
	if ok && !e.refreshing && time.Since(e.at) > oracleLatestAllTimeTTL {
		e.refreshing = true
		go s.refreshOracleLatestAllTime(context.WithoutCancel(ctx), key, keys, sourceFilter)
	}
	s.oracleAllTimeMu.Unlock()
	if ok {
		return e.rows, nil
	}
	rows, err := s.queryOracleLatest(ctx, oracleLatestAllTimeQuery, keys, sourceFilter)
	if err != nil {
		return nil, err
	}
	s.storeOracleLatestAllTime(key, rows)
	return rows, nil
}

func (s *Store) refreshOracleLatestAllTime(parent context.Context, key string, keys []string, sourceFilter string) {
	stored := false
	defer func() {
		if rec := recover(); rec != nil {
			worker.Report(nil, "oracle-latest-all-time-refresh", rec)
		}
		if !stored {
			s.oracleAllTimeMu.Lock()
			if e, ok := s.oracleAllTime[key]; ok {
				e.refreshing = false
			}
			s.oracleAllTimeMu.Unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	rows, err := s.queryOracleLatest(ctx, oracleLatestAllTimeQuery, keys, sourceFilter)
	if err != nil {
		slog.Warn("timescale: oracle all-time latest refresh failed", "key", key, "err", err)
		return
	}
	s.storeOracleLatestAllTime(key, rows)
	stored = true
}

func (s *Store) storeOracleLatestAllTime(key string, rows []canonical.OracleUpdate) {
	s.oracleAllTimeMu.Lock()
	defer s.oracleAllTimeMu.Unlock()
	if s.oracleAllTime == nil || len(s.oracleAllTime) >= oracleLatestAllTimeMaxEntries {
		s.oracleAllTime = map[string]*oracleAllTimeEntry{}
	}
	s.oracleAllTime[key] = &oracleAllTimeEntry{rows: rows, at: time.Now()}
}

// mergeOracleLatest keeps, per (source, quote), the row with the highest (ts, ledger) — the
// DISTINCT ON pick of the all-time query — ordered by (source, quote) as that query returns them.
func mergeOracleLatest(recent, allTime []canonical.OracleUpdate) []canonical.OracleUpdate {
	best := make(map[[2]string]canonical.OracleUpdate, len(allTime))
	for _, u := range slices.Concat(allTime, recent) {
		k := [2]string{u.Source, u.Quote.String()}
		cur, ok := best[k]
		if !ok || u.Timestamp.After(cur.Timestamp) || (u.Timestamp.Equal(cur.Timestamp) && u.Ledger > cur.Ledger) {
			best[k] = u
		}
	}
	out := make([]canonical.OracleUpdate, 0, len(best))
	for _, u := range best {
		out = append(out, u)
	}
	slices.SortFunc(out, func(a, b canonical.OracleUpdate) int {
		return cmp.Or(strings.Compare(a.Source, b.Source), strings.Compare(a.Quote.String(), b.Quote.String()))
	})
	return out
}

func (s *Store) queryOracleLatest(ctx context.Context, q string, keys []string, sourceFilter string) ([]canonical.OracleUpdate, error) {
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

// LatestAggregatorPricesForPair returns the newest oracle_updates row per source for (base, quote) across the given aggregator-class sources, for the `aggregator_avg` tier; empty list gives (nil, nil). unbounded-latest-ok: the latest observation per source is served with its age; a floor would drop quiet sources.
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

// LatestOracleObservation returns the newest oracle_updates row for `source` matching ANY of baseKeys and ANY of quoteKeys (XLM's dual identity), or (nil, nil) rather than ErrNotFound when nothing matches.
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

// CountOracleUpdates returns the oracle_updates row count; a diagnostic, not for hot paths.
func (s *Store) CountOracleUpdates(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM oracle_updates -- totality: includes unmapped`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("timescale: CountOracleUpdates: %w", err)
	}
	return n, nil
}

// LatestOracleStreams returns the newest observation per (source, asset, quote) in the trailing 7d window for /v1/oracles/streams; the window is the live-stream definition, so a dead feed does not surface as current.
func (s *Store) LatestOracleStreams(ctx context.Context) ([]canonical.OracleUpdate, error) {
	// One (asset, quote, ts DESC) index probe per stream instead of sorting the whole 7-day window
	// (~275k rows for ~190 streams on r1: 490 ms -> 80 ms, same rows).
	const q = `
        WITH streams AS (
            SELECT DISTINCT source, asset, quote
              FROM oracle_updates -- totality: includes unmapped
             WHERE ts > NOW() - INTERVAL '7 days'
        )
        SELECT l.source, COALESCE(l.contract_id, ''),
               l.ledger, l.tx_hash, l.op_index, l.ts,
               l.asset, l.quote,
               l.price, l.decimals,
               COALESCE(l.confidence, 0),
               COALESCE(l.observer, ''),
               l.published_price
          FROM streams s
         CROSS JOIN LATERAL (
            SELECT * FROM oracle_updates o
             WHERE o.source = s.source AND o.asset = s.asset AND o.quote = s.quote
               AND o.ts > NOW() - INTERVAL '7 days'
             ORDER BY o.ts DESC
             LIMIT 1
         ) l
         ORDER BY l.source, l.asset, l.quote
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
		// A parse failure drops the row (there is nothing sane to serve for an asset we cannot
		// name) but is never silent. The likeliest cause is an operator's raw SQL UPDATE of this
		// unconstrained column to relabel a row; a typo would make the row vanish and look like
		// a successful relabel.
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

// OracleDayPoint is one day bucket of one oracle's observations of one asset from `oracle_prices_1d`; Price is the bucket's CLOSING observation, and closes of two sources within a day cannot be ordered.
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
	// Observations is how many publications the oracle made in the day. A bucket built from ONE observation and one built from
	// a thousand are different evidence for the same closing figure, and a surface that publishes the figure should be able to
	// say which it had.
	Observations int64
}

// DailyOraclePrices returns the daily closing observation per (source, asset) among `assets` in `quote` over the inclusive bucket range [from, to]; no carry-in row, so a silent day is silence, and `raw:` assets are dropped and refused by the SQL.
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
