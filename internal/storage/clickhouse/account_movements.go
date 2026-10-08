package clickhouse

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// AccountMovementDirection says which side of a two-party movement a row represents.
type AccountMovementDirection string

const (
	AccountMovementSent     AccountMovementDirection = "sent"
	AccountMovementReceived AccountMovementDirection = "received"
	AccountMovementSelf     AccountMovementDirection = "self"
)

// AccountMovement is the pre-fan-out movement. It mirrors classicmovements.Movement
// instead of importing it: internal/storage sits below internal/sources
// (lint-imports.sh L/storage-below-compute).
type AccountMovement struct {
	MovementKind    string
	Provenance      string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	LegIndex        uint32
	Asset           string
	Amount          *big.Int

	// FromAddress/ToAddress: "" means "not a real G-account for this leg" (claimable
	// balance escrow, liquidity pool), not "unknown".
	FromAddress string
	ToAddress   string

	// Attributes is the kind-specific remainder, as a JSON string.
	Attributes map[string]any
}

// AccountMovementRow is one per-participant row; Counterparty is the other side, when known.
type AccountMovementRow struct {
	Address         string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	LegIndex        uint32
	Direction       AccountMovementDirection
	MovementKind    string
	Provenance      string
	Asset           string
	Counterparty    string
	Amount          *big.Int
	Attributes      map[string]any
}

// FanOutAccountMovement expands one movement into its account_movements row(s).
// Two exceptions to "one row per participant": From==To (both set) is one
// direction=self row, so no two rows differ only by `direction`; exactly one side
// set (claimable-balance escrow, LP or path-payment leg) is one row for the known
// side with Counterparty="". Neither side set returns nil.
func FanOutAccountMovement(m AccountMovement) []AccountMovementRow {
	base := AccountMovementRow{
		Ledger:          m.Ledger,
		LedgerCloseTime: m.LedgerCloseTime,
		TxHash:          m.TxHash,
		OpIndex:         m.OpIndex,
		LegIndex:        m.LegIndex,
		MovementKind:    m.MovementKind,
		Provenance:      m.Provenance,
		Asset:           m.Asset,
		Amount:          m.Amount,
		Attributes:      m.Attributes,
	}
	switch {
	case m.FromAddress != "" && m.ToAddress != "" && m.FromAddress == m.ToAddress:
		row := base
		row.Address = m.FromAddress
		row.Direction = AccountMovementSelf
		return []AccountMovementRow{row}
	case m.FromAddress != "" && m.ToAddress != "":
		sent := base
		sent.Address = m.FromAddress
		sent.Direction = AccountMovementSent
		sent.Counterparty = m.ToAddress
		received := base
		received.Address = m.ToAddress
		received.Direction = AccountMovementReceived
		received.Counterparty = m.FromAddress
		return []AccountMovementRow{sent, received}
	case m.FromAddress != "":
		row := base
		row.Address = m.FromAddress
		row.Direction = AccountMovementSent
		return []AccountMovementRow{row}
	case m.ToAddress != "":
		row := base
		row.Address = m.ToAddress
		row.Direction = AccountMovementReceived
		return []AccountMovementRow{row}
	default:
		return nil
	}
}

// accountMovementsDDL mirrors deploy/clickhouse/tier1_schema.sql, which is what gets
// applied; this copy lets EnsureAccountMovementsTable create the table on a fresh
// ClickHouse. CREATE TABLE IF NOT EXISTS does not retrofit idx_cb_balance_id onto
// an existing table.
const accountMovementsDDL = `
	CREATE TABLE IF NOT EXISTS stellar.account_movements (
		address           String,
		ledger            UInt32,
		ledger_close_time DateTime64(0, 'UTC'),
		tx_hash           String,
		op_index          UInt32,
		leg_index         UInt32,
		direction         LowCardinality(String),
		movement_kind     LowCardinality(String),
		provenance        LowCardinality(String),
		asset             String,
		counterparty      String DEFAULT '',
		amount            Int128,
		attributes        String DEFAULT '{}',
		ingested_at       DateTime DEFAULT now(),
		INDEX idx_cb_balance_id JSONExtractString(attributes, 'balance_id') TYPE bloom_filter(0.01) GRANULARITY 4
	) ENGINE = ReplacingMergeTree(ingested_at)
	PARTITION BY intDiv(ledger, 1000000)
	ORDER BY (address, ledger, tx_hash, op_index, leg_index, direction)`

// EnsureAccountMovementsTable creates stellar.account_movements if absent (idempotent).
func EnsureAccountMovementsTable(ctx context.Context, addr string) error {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.Exec(ctx, accountMovementsDDL); err != nil {
		return fmt.Errorf("clickhouse: ensure account_movements: %w", err)
	}
	return nil
}

// accountMovementsInsertChunk is the ROW count (post-fan-out) per native INSERT,
// bounding payload size however large the caller's window is.
const accountMovementsInsertChunk = 20_000

// InsertAccountMovements fans out and batch-inserts movements; a retry of the
// whole batch is safe because ReplacingMergeTree absorbs duplicates.
// Rows go out in ledger order, so a partial send is complete below max(ledger),
// which is what makes max(ledger) a sound resume point for classic-movements-backfill.
// Returns rows sent, not rows surviving dedup.
func InsertAccountMovements(ctx context.Context, addr string, movements []AccountMovement) (int64, error) {
	if len(movements) == 0 {
		return 0, nil
	}
	var rows []AccountMovementRow
	for _, m := range movements {
		rows = append(rows, FanOutAccountMovement(m)...)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	// Ledger-ordered: decides what a partially-sent multi-chunk batch leaves behind.
	sortAccountMovementRowsForInsert(rows)

	conn, err := openAccountMovementsWrite(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()

	var written int64
	for i := 0; i < len(rows); i += accountMovementsInsertChunk {
		end := i + accountMovementsInsertChunk
		if end > len(rows) {
			end = len(rows)
		}
		if err := insertAccountMovementChunk(ctx, conn, rows[i:end]); err != nil {
			return written, fmt.Errorf("clickhouse: InsertAccountMovements: chunk [%d,%d): %w", i, end, err)
		}
		written += int64(end - i)
	}
	return written, nil
}

// insertAccountMovementChunk sends one native batch.
func insertAccountMovementChunk(ctx context.Context, conn driver.Conn, rows []AccountMovementRow) error {
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO stellar.account_movements
		(address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
		 movement_kind, provenance, asset, counterparty, amount, attributes)`)
	if err != nil {
		return fmt.Errorf("prepare account_movements batch: %w", err)
	}
	for _, r := range rows {
		amt := r.Amount
		if amt == nil {
			amt = big.NewInt(0)
		}
		attrs, aerr := marshalAccountMovementAttributes(r.Attributes)
		if aerr != nil {
			return fmt.Errorf("marshal attributes %s/%s/%d/%d: %w", r.Address, r.TxHash, r.OpIndex, r.LegIndex, aerr)
		}
		if err := batch.Append(
			r.Address, r.Ledger, r.LedgerCloseTime, r.TxHash, r.OpIndex, r.LegIndex, string(r.Direction),
			r.MovementKind, r.Provenance, r.Asset, r.Counterparty, amt, attrs,
		); err != nil {
			return fmt.Errorf("append %s/%s/%d/%d/%s: %w", r.Address, r.TxHash, r.OpIndex, r.LegIndex, r.Direction, err)
		}
	}
	return wrapSend(batch.Send(), "account_movements")
}

// marshalAccountMovementAttributes returns '{}' for nil/empty, matching the column DEFAULT.
func marshalAccountMovementAttributes(attrs map[string]any) (string, error) {
	if len(attrs) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(attrs)
	if err != nil {
		return "", fmt.Errorf("marshal attributes: %w", err)
	}
	return string(b), nil
}

// sortAccountMovementRowsForInsert orders by LEDGER first, then the remaining ORDER BY
// columns. ClickHouse has no transaction across chunks and resume checkpoints on
// max(ledger), so ledger-first makes every ledger below the highest written one
// complete; address-first would silently skip addresses past a failure point.
func sortAccountMovementRowsForInsert(rows []AccountMovementRow) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := &rows[i], &rows[j]
		if a.Ledger != b.Ledger {
			return a.Ledger < b.Ledger
		}
		if a.Address != b.Address {
			return a.Address < b.Address
		}
		if a.TxHash != b.TxHash {
			return a.TxHash < b.TxHash
		}
		if a.OpIndex != b.OpIndex {
			return a.OpIndex < b.OpIndex
		}
		if a.LegIndex != b.LegIndex {
			return a.LegIndex < b.LegIndex
		}
		return a.Direction < b.Direction
	})
}

// openAccountMovementsWrite dials the cheap-append write class; one opener per writer file.
func openAccountMovementsWrite(ctx context.Context, addr string) (driver.Conn, error) {
	// Identity from the environment; see ops_auth.go.
	auth, err := chAuth()
	if err != nil {
		return nil, err
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: auth,
		Settings: clickhouse.Settings{
			"max_execution_time": 300,
		},
		DialTimeout:     10 * time.Second,
		MaxOpenConns:    2,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Hour,
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse: open write %s: %w", addr, err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse: ping write %s: %w", addr, err)
	}
	return conn, nil
}

// MaxAccountMovementLedger returns the highest ledger in [from,to], the resume point (found=false if none).
// No FINAL: duplicates share the same ledger value, so max() is exact.
func MaxAccountMovementLedger(ctx context.Context, addr string, from, to uint32) (ledger uint32, found bool, err error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = conn.Close() }()
	var cnt, hi uint64
	if err := conn.QueryRow(ctx,
		`SELECT toUInt64(count()), toUInt64(max(ledger)) FROM stellar.account_movements WHERE ledger BETWEEN ? AND ?`,
		from, to).Scan(&cnt, &hi); err != nil {
		return 0, false, fmt.Errorf("clickhouse: max account_movements ledger [%d,%d]: %w", from, to, err)
	}
	if cnt == 0 {
		return 0, false, nil
	}
	return uint32(hi), true, nil
}

// MinAccountMovementLedger returns the lowest ledger in [from,to]. -resume trusts
// MaxAccountMovementLedger only when this equals from, otherwise a widened -from
// would jump past the earlier, never-processed range.
func MinAccountMovementLedger(ctx context.Context, addr string, from, to uint32) (ledger uint32, found bool, err error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = conn.Close() }()
	var cnt, lo uint64
	if err := conn.QueryRow(ctx,
		`SELECT toUInt64(count()), toUInt64(min(ledger)) FROM stellar.account_movements WHERE ledger BETWEEN ? AND ?`,
		from, to).Scan(&cnt, &lo); err != nil {
		return 0, false, fmt.Errorf("clickhouse: min account_movements ledger [%d,%d]: %w", from, to, err)
	}
	if cnt == 0 {
		return 0, false, nil
	}
	return uint32(lo), true, nil
}

// ClaimableBalanceCreateRow is one resolved claimable_balance_create (asset/amount/creator).
type ClaimableBalanceCreateRow struct {
	Asset     string
	Amount    *big.Int
	CreatedBy string
}

// cbLookupExtTableChunkSize caps ids per external-table semijoin query. Ids travel as
// column data, so there is no max_query_size exposure; the cap only bounds the
// server-side hash set and driver batch.
const cbLookupExtTableChunkSize = 1_000_000

// chunkStrings splits ids into sub-slices of at most n; n<=0 means no chunking.
func chunkStrings(ids []string, n int) [][]string {
	if len(ids) == 0 {
		return nil
	}
	if n <= 0 || n >= len(ids) {
		return [][]string{ids}
	}
	chunks := make([][]string, 0, (len(ids)+n-1)/n)
	for i := 0; i < len(ids); i += n {
		end := i + n
		if end > len(ids) {
			end = len(ids)
		}
		chunks = append(chunks, ids[i:end])
	}
	return chunks
}

// FindClaimableBalanceCreates batch-resolves pending claim/clawback refs to their create rows, keyed by balance_id.
// Ids go in a ClickHouse external table and match via a hash-set semijoin: an
// inlined IN-list exceeds max_query_size and compounds the bloom filter's
// false-positive rate into a full scan of the wide attributes column. The query
// runs with use_skip_indexes=0, so idx_cb_balance_id is unused here.
// The map holds only found ids; a miss means no create row yet (ADR-0047 D4
// incompleteness), so callers must count and log it, never guess an amount.
// Duplicate rows are identical, first wins. Empty input returns an empty non-nil map.
func FindClaimableBalanceCreates(ctx context.Context, addr string, balanceIDHexes []string) (map[string]ClaimableBalanceCreateRow, error) {
	out := make(map[string]ClaimableBalanceCreateRow, len(balanceIDHexes))
	if len(balanceIDHexes) == 0 {
		return out, nil
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	for _, chunk := range chunkStrings(balanceIDHexes, cbLookupExtTableChunkSize) {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("clickhouse: FindClaimableBalanceCreates(%d ids): %w", len(balanceIDHexes), err)
		}
		if err := findClaimableBalanceCreatesChunk(ctx, conn, chunk, out); err != nil {
			return nil, fmt.Errorf("clickhouse: FindClaimableBalanceCreates(%d ids): %w", len(balanceIDHexes), err)
		}
	}
	return out, nil
}

// cbLookupCreatesQuery matches chunk ids against the `cb_ids` external table.
// SETTINGS is in the SQL text because per-query WithSettings did not reach the
// server alongside WithExternalTable. use_skip_indexes=0 is load-bearing: the
// bloom index against a large IN-set matches every granule and blew the memory limit.
const cbLookupCreatesQuery = `
	SELECT JSONExtractString(attributes, 'balance_id') AS balance_id, asset, amount, address
	FROM stellar.account_movements
	WHERE movement_kind = 'claimable_balance_create'
	  AND JSONExtractString(attributes, 'balance_id') IN cb_ids
	SETTINGS use_skip_indexes = 0, max_threads = 4, max_memory_usage = 8000000000`

// findClaimableBalanceCreatesChunk runs the query for one chunk via a server-side
// external table and merges matches into out.
func findClaimableBalanceCreatesChunk(ctx context.Context, conn driver.Conn, chunk []string, out map[string]ClaimableBalanceCreateRow) error {
	tbl, terr := ext.NewTable("cb_ids", ext.Column("balance_id", "String"))
	if terr != nil {
		return fmt.Errorf("build cb_ids external table: %w", terr)
	}
	for _, id := range chunk {
		if aerr := tbl.Append(id); aerr != nil {
			return fmt.Errorf("append cb_ids row: %w", aerr)
		}
	}

	// Query bounds are in cbLookupCreatesQuery's SETTINGS clause (see there).
	qctx := clickhouse.Context(ctx, clickhouse.WithExternalTable(tbl))

	rows, qerr := conn.Query(qctx, cbLookupCreatesQuery)
	if qerr != nil {
		return qerr
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var balanceID, asset, createdBy string
		var amt *big.Int
		if err := rows.Scan(&balanceID, &asset, &amt, &createdBy); err != nil {
			return fmt.Errorf("scan row: %w", err)
		}
		if _, dup := out[balanceID]; dup {
			continue // ReplacingMergeTree pre-merge duplicate, or a repeat across chunks — identical by construction; first wins.
		}
		if amt == nil {
			amt = big.NewInt(0)
		}
		out[balanceID] = ClaimableBalanceCreateRow{Asset: asset, Amount: amt, CreatedBy: createdBy}
	}
	return rows.Err()
}

// AccountMovementVerifyCounts maps movement_kind to the count of DISTINCT movements
// (a two-participant movement is 2 rows sharing one identity).
type AccountMovementVerifyCounts map[string]uint64

// VerifyAccountMovementsWindow recounts [from,to] by movement_kind via
// uniqExact(tx_hash, op_index, leg_index), collapsing fan-out rows. No FINAL:
// identical duplicates do not inflate a distinct count.
func VerifyAccountMovementsWindow(ctx context.Context, addr string, from, to uint32) (AccountMovementVerifyCounts, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.Query(ctx, `
		SELECT movement_kind, uniqExact(tx_hash, op_index, leg_index) AS n
		FROM stellar.account_movements
		WHERE ledger BETWEEN ? AND ?
		GROUP BY movement_kind`, from, to)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: verify account_movements window [%d,%d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()

	out := AccountMovementVerifyCounts{}
	for rows.Next() {
		var kind string
		var n uint64
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, fmt.Errorf("clickhouse: scan verify row [%d,%d]: %w", from, to, err)
		}
		out[kind] = n
	}
	return out, rows.Err()
}

// AccountMovementFilter narrows an AccountMovements read; zero values mean no filter.
type AccountMovementFilter struct {
	Kind      string                   // movement_kind exact match; "" = any
	Direction AccountMovementDirection // exact match; "" = any
	Asset     string                   // canonical asset id exact match; "" = any

	// MaxLedger is the inclusive ledger ceiling, read only when HasMaxLedger is set.
	// It is a SQL predicate, not a Go post-filter: trimming after LIMIT can empty the
	// page and suppress next_cursor, stranding all pre-watermark history.
	MaxLedger uint32
	// HasMaxLedger is the explicit set-signal: 0 is a reachable ceiling (genesis floor
	// with no watermark) and must serve nothing, which a `MaxLedger > 0` sentinel would break.
	HasMaxLedger bool
}

// AccountMovementCursor is the descending (ledger, tx_hash, op_index, leg_index)
// keyset position; Ledger==0 means first page.
type AccountMovementCursor struct {
	Ledger   uint32
	TxHash   string
	OpIndex  uint32
	LegIndex uint32
}

// IsSet reports whether this is a continuation page.
func (c AccountMovementCursor) IsSet() bool { return c.Ledger > 0 }

const accountMovementCols = `ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
	movement_kind, provenance, asset, counterparty, amount, attributes`

// accountMovementsQuery builds the SQL for the filter dimensions and cursor presence.
// Keyed read: `address = ?` is a contiguous primary-key range, but it still
// carries explorerScanSettings because un-merged parts during backfill fan out
// per-part stream setup.
func accountMovementsQuery(filter AccountMovementFilter, hasCursor bool) string {
	return accountMovementsSQL(filter, hasCursor, true)
}

// accountMovementsWindowQuery reads in sort-key order with ingested_at appended and
// no LIMIT 1 BY, so the read stops early; the caller keeps the newest version per key.
func accountMovementsWindowQuery(filter AccountMovementFilter, hasCursor bool) string {
	return accountMovementsSQL(filter, hasCursor, false)
}

func accountMovementsSQL(filter AccountMovementFilter, hasCursor, exactDedup bool) string {
	var sb strings.Builder
	cols := accountMovementCols
	if !exactDedup {
		cols += ", ingested_at"
	}
	sb.WriteString("SELECT " + cols + " FROM stellar.account_movements WHERE address = ?")
	if filter.Kind != "" {
		sb.WriteString(" AND movement_kind = ?")
	}
	if filter.Direction != "" {
		sb.WriteString(" AND direction = ?")
	}
	if filter.Asset != "" {
		sb.WriteString(" AND asset = ?")
	}
	if filter.HasMaxLedger {
		sb.WriteString(" AND ledger <= ?")
	}
	if hasCursor {
		// The leading `ledger <= ?` lets the primary index cut at the cursor; a tuple alone is not pruned.
		sb.WriteString(" AND ledger <= ? AND (ledger, tx_hash, op_index, leg_index) < (?, ?, ?, ?)")
	}
	// LIMIT 1 BY is the read-time dedup: an un-merged ReplacingMergeTree duplicate would
	// eat a LIMIT slot and shift the keyset cursor. Cheaper than FINAL.
	// ingested_at DESC keeps the newest version of a re-derived key.
	if exactDedup {
		sb.WriteString(" ORDER BY ledger DESC, tx_hash DESC, op_index DESC, leg_index DESC, ingested_at DESC LIMIT 1 BY ledger, tx_hash, op_index, leg_index LIMIT ?")
	} else {
		sb.WriteString(" ORDER BY ledger DESC, tx_hash DESC, op_index DESC, leg_index DESC LIMIT ?")
	}
	sb.WriteString(explorerScanSettings)
	return sb.String()
}

// AccountMovements returns one address's movement feed, newest first, keyset-paged.
// `address` is an equality on the ORDER BY prefix: one contiguous range, no UNION.
// The explorer merges this archive with the Postgres tail; its ledger ceiling travels
// in filter.MaxLedger/HasMaxLedger so it applies before LIMIT, never as a post-read trim.
// No FINAL: the page is read as a window in sort-key order (LIMIT 1 BY would stop
// the early exit), newest ingested_at per key is kept in Go, and a window that
// cannot prove a full page falls back to accountMovementsQuery.
func (r *ExplorerReader) AccountMovements(ctx context.Context, address string, limit int, cur AccountMovementCursor, filter AccountMovementFilter) ([]AccountMovementRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 25
	}
	args := []any{address}
	if filter.Kind != "" {
		args = append(args, filter.Kind)
	}
	if filter.Direction != "" {
		args = append(args, string(filter.Direction))
	}
	if filter.Asset != "" {
		args = append(args, filter.Asset)
	}
	if filter.HasMaxLedger {
		args = append(args, filter.MaxLedger)
	}
	if cur.IsSet() {
		args = append(args, cur.Ledger, cur.Ledger, cur.TxHash, cur.OpIndex, cur.LegIndex)
	}
	window := windowRows(limit, windowFactorKeys)
	if page, ok, err := r.accountMovementsWindowed(ctx, address, limit, window, cur, filter, append(args, window)); err != nil || ok {
		return page, err
	}
	return r.queryAccountMovements(ctx, address, accountMovementsQuery(filter, cur.IsSet()), append(args, limit))
}

func (r *ExplorerReader) queryAccountMovements(ctx context.Context, address, q string, args []any) ([]AccountMovementRow, error) {
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: account %s movements: %w", address, err)
	}
	defer func() { _ = rows.Close() }()
	return scanAccountMovementRows(rows, address)
}

type movementKey struct {
	ledger, opIndex, legIndex uint32
	txHash                    string
}

// accountMovementsWindowed serves a page from one sort-key-order window, keeping the
// newest ingested_at per key. ok=false means it could not prove a full page.
func (r *ExplorerReader) accountMovementsWindowed(ctx context.Context, address string, limit, window int, cur AccountMovementCursor, filter AccountMovementFilter, args []any) ([]AccountMovementRow, bool, error) {
	rows, err := r.conn.Query(ctx, accountMovementsWindowQuery(filter, cur.IsSet()), args...)
	if err != nil {
		return nil, false, fmt.Errorf("clickhouse: account %s movements: %w", address, err)
	}
	defer func() { _ = rows.Close() }()
	versioned, err := scanAccountMovementVersions(rows, address)
	if err != nil {
		return nil, false, err
	}
	deduped, ok := dedupWindow(versioned, window, limit,
		func(v accountMovementVersion) movementKey {
			return movementKey{v.row.Ledger, v.row.OpIndex, v.row.LegIndex, v.row.TxHash}
		},
		func(a, b accountMovementVersion) bool { return a.ingestedAt.After(b.ingestedAt) })
	if !ok {
		return nil, false, nil
	}
	if len(deduped) > limit {
		deduped = deduped[:limit]
	}
	out := make([]AccountMovementRow, len(deduped))
	for i, v := range deduped {
		out[i] = v.row
	}
	return out, true, nil
}

type accountMovementVersion struct {
	row        AccountMovementRow
	ingestedAt time.Time
}

func scanAccountMovementVersions(rows driver.Rows, address string) ([]accountMovementVersion, error) {
	var out []accountMovementVersion
	for rows.Next() {
		var v accountMovementVersion
		if err := scanAccountMovementRow(rows, address, &v.row, &v.ingestedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// scanAccountMovementRows scans rows into AccountMovementRow, stamping Address (not selected).
func scanAccountMovementRows(rows driver.Rows, address string) ([]AccountMovementRow, error) {
	var out []AccountMovementRow
	for rows.Next() {
		var row AccountMovementRow
		if err := scanAccountMovementRow(rows, address, &row, nil); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// scanAccountMovementRow scans one row; a non-nil version also receives ingested_at.
func scanAccountMovementRow(rows driver.Rows, address string, row *AccountMovementRow, version *time.Time) error {
	var direction, attrs string
	var amt *big.Int
	dest := []any{
		&row.Ledger, &row.LedgerCloseTime, &row.TxHash, &row.OpIndex, &row.LegIndex,
		&direction, &row.MovementKind, &row.Provenance, &row.Asset, &row.Counterparty,
		&amt, &attrs,
	}
	if version != nil {
		dest = append(dest, version)
	}
	if err := rows.Scan(dest...); err != nil {
		return fmt.Errorf("clickhouse: scan account movement row: %w", err)
	}
	row.Address = address
	row.Direction = AccountMovementDirection(direction)
	row.Amount = amt
	if attrs != "" && attrs != "{}" {
		if uerr := json.Unmarshal([]byte(attrs), &row.Attributes); uerr != nil {
			return fmt.Errorf("clickhouse: unmarshal account movement attributes: %w", uerr)
		}
	}
	return nil
}
