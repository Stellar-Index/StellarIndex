package timescale

import (
	"fmt"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// tradeUpsertColumns is the column order of tradeValuesRow; the tagger columns (routed_via, signer, tx_index) are first-wins and never listed.
const tradeUpsertColumns = `source, ledger, tx_hash, op_index, ts, base_asset, quote_asset,
                base_amount, quote_amount, usd_volume, maker, taker, derive_generation, usd_basis`

// tradeValuesRow binds one trade; the casts type the VALUES CTE, which, unlike INSERT ... VALUES, takes no types from trades.
const tradeValuesRow = "$%d::text, $%d::integer, $%d::char(64), $%d::integer, $%d::timestamptz, $%d::text, $%d::text, " +
	"$%d::numeric, $%d::numeric, $%d::numeric, NULLIF($%d::text, ''), NULLIF($%d::text, ''), $%d::bigint, NULLIF($%d::text, '')"

// tradeUpsertWrites is true when upserting incoming over stored would change the row: a higher generation, or an equal one with a
// changed value (an incoming NULL usd_volume keeps the stored pair, per the SET list).
func tradeUpsertWrites(stored, incoming string) string {
	return strings.NewReplacer("{s}", stored, "{i}", incoming).Replace(`({s}.derive_generation < {i}.derive_generation
                 OR ({s}.derive_generation = {i}.derive_generation AND (
                        ({s}.base_asset, {s}.quote_asset, {s}.base_amount, {s}.quote_amount, {s}.maker, {s}.taker)
                          IS DISTINCT FROM
                        ({i}.base_asset, {i}.quote_asset, {i}.base_amount, {i}.quote_amount, {i}.maker, {i}.taker)
                     OR ({i}.usd_volume IS NOT NULL AND
                         ({s}.usd_volume, {s}.usd_basis) IS DISTINCT FROM ({i}.usd_volume, {i}.usd_basis)))))`)
}

// tradeUpsertSkipNoOp selects the rows of v the upsert would change. TimescaleDB decompresses a compressed chunk's batch for the
// ON CONFLICT check whatever the DO UPDATE WHERE says, so a no-op must not reach it. LATERAL ... LIMIT 1 keeps a per-row probe
// (segmentby index + bloom filter, about 1 ms on r1's compressed chunks) where NOT EXISTS hash-anti-joins the whole hypertable,
// and the constant ts bounds exclude chunks at plan time.
func tradeUpsertSkipNoOp(minTS, maxTS int) string {
	//nolint:gosec // G201: SQL built only from compile-time fragments; values are bind params.
	return fmt.Sprintf(`SELECT v.* FROM v LEFT JOIN LATERAL (
                SELECT 1 AS hit FROM trades t
                 WHERE t.ts >= $%d::timestamptz AND t.ts <= $%d::timestamptz
                   AND t.source = v.source AND t.ledger = v.ledger AND t.tx_hash = v.tx_hash
                   AND t.op_index = v.op_index AND t.ts = v.ts
                   AND t.base_asset = v.base_asset AND t.quote_asset = v.quote_asset
                   AND NOT %s
                 LIMIT 1) noop ON true
             WHERE noop.hit IS NULL`, minTS, maxTS, tradeUpsertWrites("t", "v"))
}

// tradeTSBounds is the [min, max] trade timestamp of a non-empty batch.
func tradeTSBounds(rows []canonical.Trade) (time.Time, time.Time) {
	lo, hi := rows[0].Timestamp.UTC(), rows[0].Timestamp.UTC()
	for _, t := range rows[1:] {
		ts := t.Timestamp.UTC()
		if ts.Before(lo) {
			lo = ts
		}
		if ts.After(hi) {
			hi = ts
		}
	}
	return lo, hi
}
