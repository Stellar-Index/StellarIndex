package timescale

import (
	"context"
	"database/sql"
	"time"
)

// LendingConfigChange is one Blend or BLNT pool admin event: a reserve-config step or a pool-level change.
type LendingConfigChange struct {
	Ts         time.Time
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	Kind       string
	Asset      string // "" for a pool-scoped event
	NewAdmin   string // set_admin only
	Attributes []byte // the event's attributes JSON
	// Metadata is the reserve config JSON: a queue_set_reserve's own, or for set_reserve the queue it applied.
	Metadata []byte
	Applied  bool // queue_set_reserve whose next step for the asset is set_reserve
}

// LendingConfigCursor is the keyset position of the last served LendingConfigChange.
type LendingConfigCursor struct {
	Ts         time.Time
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
}

// IsSet reports whether the cursor points past an already served row.
func (c LendingConfigCursor) IsSet() bool { return !c.Ts.IsZero() }

// lendingConfigHistorySQL reads both admin tables (a pool lives in one) through their kind indexes. The window runs over the
// pool's whole admin history before the keyset cut, so a queue on one page is judged by a step on another.
const lendingConfigHistorySQL = `
        WITH ev AS (
            SELECT ledger_close_time, ledger, tx_hash::text AS tx_hash, op_index, event_index, event_kind,
                   NULLIF(asset, '') AS asset, target AS new_admin, attributes
              FROM blend_admin
             WHERE contract_id = $1 AND event_kind IN (` + lendingConfigKinds + `)
            UNION ALL
            SELECT ledger_close_time, ledger, tx_hash::text, op_index, event_index, event_kind,
                   asset, attributes->>'new_admin', attributes
              FROM blnt_events
             WHERE contract_id = $1 AND contract_role = 'pool' AND event_kind IN (` + lendingConfigKinds + `)
        ), chained AS (
            SELECT ev.*,
                   LEAD(event_kind) OVER w AS next_kind,
                   LAG(event_kind) OVER w AS prev_kind,
                   LAG(attributes->'metadata') OVER w AS prev_metadata
              FROM ev
            WINDOW w AS (PARTITION BY CASE WHEN event_kind IN (` + reserveConfigKinds + `) THEN asset END
                         ORDER BY ledger_close_time, ledger, op_index, event_index)
        )
        SELECT ledger_close_time, ledger, tx_hash, op_index, event_index, event_kind,
               COALESCE(asset, ''), COALESCE(new_admin, ''), COALESCE(attributes::text, '{}'),
               COALESCE(CASE event_kind WHEN 'queue_set_reserve' THEN attributes->'metadata'
                                        WHEN 'set_reserve' THEN CASE WHEN prev_kind = 'queue_set_reserve' THEN prev_metadata END
                        END::text, ''),
               event_kind = 'queue_set_reserve' AND next_kind IS NOT DISTINCT FROM 'set_reserve'
          FROM chained
         WHERE (ledger_close_time, ledger, tx_hash, op_index, event_index) < ($2::timestamptz, $3::integer, $4::text, $5::integer, $6::integer)
         ORDER BY ledger_close_time DESC, ledger DESC, tx_hash DESC, op_index DESC, event_index DESC
         LIMIT $7`

const (
	reserveConfigKinds = `'queue_set_reserve','cancel_set_reserve','set_reserve'`
	lendingConfigKinds = reserveConfigKinds + `,'update_pool','set_status','set_admin'`
)

// endOfTime stands in for an unset cursor so one query serves the first page and the rest.
var endOfTime = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

// LendingConfigHistory returns a pool's reserve-config and pool-level admin events, newest first, at most limit rows after cur.
func (s *Store) LendingConfigHistory(ctx context.Context, pool string, limit int, cur LendingConfigCursor) ([]LendingConfigChange, error) {
	after := LendingConfigCursor{Ts: endOfTime}
	if cur.IsSet() {
		after = cur
	}
	args := []any{pool, after.Ts.UTC(), int64(after.Ledger), after.TxHash, int64(after.OpIndex), int64(after.EventIndex), limit}
	return queryFold(ctx, s.db, "LendingConfigHistory", lendingConfigHistorySQL, args, func(rows *sql.Rows) (LendingConfigChange, error) {
		var c LendingConfigChange
		var attrs, meta string
		err := rows.Scan(&c.Ts, &c.Ledger, &c.TxHash, &c.OpIndex, &c.EventIndex, &c.Kind, &c.Asset, &c.NewAdmin, &attrs, &meta, &c.Applied)
		c.Attributes = []byte(attrs)
		if meta != "" {
			c.Metadata = []byte(meta)
		}
		return c, err
	})
}
