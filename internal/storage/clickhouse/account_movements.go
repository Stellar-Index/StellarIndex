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

// AccountMovementDirection discriminates which side of a two-party
// classic-asset movement one stellar.account_movements row represents
// (ADR-0048 D2).
type AccountMovementDirection string

const (
	AccountMovementSent     AccountMovementDirection = "sent"
	AccountMovementReceived AccountMovementDirection = "received"
	AccountMovementSelf     AccountMovementDirection = "self"
)

// AccountMovement is the storage-local, PRE-FAN-OUT shape of one
// ADR-0047-reconstructed classic movement. It deliberately mirrors
// classicmovements.Movement's fields rather than importing that type:
// internal/storage/ sits BELOW internal/sources/ in the repo's import
// direction (scripts/ci/lint-imports.sh's L/storage-below-compute
// rule forbids new storage->sources edges), the same reason
// timescale.ClassicMovementRow (the table this replaces, ADR-0048)
// doesn't import classicmovements either. The caller
// (stellarindex-ops classic-movements-backfill) converts.
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

	// FromAddress/ToAddress: "" means "not a real G-account for this
	// leg" (a claimable balance's escrow, a liquidity pool), NOT
	// "unknown" — every classicmovements decode path either resolves
	// a side or leaves it empty on purpose. See FanOutAccountMovement.
	FromAddress string
	ToAddress   string

	// Attributes is the kind-specific remainder, written straight to
	// migration 0105's `attributes` shape (as a JSON string here) —
	// same convention as timescale.ClassicMovementRow.Attributes.
	Attributes map[string]any
}

// AccountMovementRow is one stellar.account_movements row — the
// feed-shaped, per-participant fan-out of an AccountMovement
// (ADR-0048 D2). Address is the row's OWN participant; Counterparty
// is the other side, when known.
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

// FanOutAccountMovement expands one reconstructed movement into its
// stellar.account_movements row(s) — ADR-0048 D2's "two rows per
// movement, one per participant, direction discriminator" rule, with
// two documented exceptions driven directly by
// internal/sources/classicmovements' decode semantics:
//
//   - FromAddress == ToAddress (both non-empty): a degenerate
//     self-payment (Stellar allows a payment whose destination is its
//     own source). ONE row, direction=self — a sent+received pair
//     would otherwise be two rows IDENTICAL except for `direction`
//     for the very same address, which is redundant and would force
//     `direction` into the ORDER BY purely to avoid a false PK
//     collision between them.
//   - Exactly one of FromAddress/ToAddress is non-empty: the other
//     side isn't a real G-account for this leg (a claimable balance's
//     escrow at creation/claim/clawback time, or a liquidity pool's
//     leg). ONE row for the known side, Counterparty="". This is the
//     "acting side" rule (ADR-0048 D2 / the classic-movements-backfill
//     task doc): claimable_balance_create emits one 'sent' row for the
//     creator; claimable_balance_claim/clawback and each LP-withdraw
//     leg emit one 'received' row for the account; each LP-deposit leg
//     emits one 'sent' row for the depositor.
//
// Neither side known (both empty) is a defensive no-op (nil, zero
// rows) — every real classicmovements decode path populates at least
// one side; a caller hitting this indicates a decode-layer bug worth
// logging, not a legitimate zero-participant movement.
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

// accountMovementsDDL is the canonical stellar.account_movements
// definition, kept in sync with deploy/clickhouse/tier1_schema.sql
// (that file's copy is the one applied to r1 —
// `clickhouse-client < deploy/clickhouse/tier1_schema.sql`, an
// operator step, see docs/operations/self-hosting.md §4.5 — and the
// one the integration-test harness loads). This Go-side copy exists
// so EnsureAccountMovementsTable can defensively create the table on
// a fresh/older ClickHouse before the first backfill write, the same
// belt-and-suspenders pattern supply_flows.go uses.
//
// idx_cb_balance_id (added 2026-07-12, see FindClaimableBalanceCreates'
// doc comment) is part of this DDL so a FRESH install gets it from the
// start. `CREATE TABLE IF NOT EXISTS` does NOT retrofit an index onto
// an already-existing table, though — this only takes effect the first
// time EnsureAccountMovementsTable creates the table from scratch. r1's
// table already existed when the index was added, so it was applied
// there directly via a one-off `ALTER TABLE ... ADD INDEX` (mutation
// complete, not re-run by this DDL).
//
// proj_by_address (added 2026-07-16, BACKLOG #72) is the structural fix
// for `/v1/accounts/{g}/movements` timing out on extreme-volume addresses
// (e.g. an airdrop-sink address with 264M received payments): the reverse
// keyset read AccountMovements() issues (`WHERE address = ? ORDER BY
// ledger DESC, tx_hash DESC, op_index DESC, leg_index DESC LIMIT ?`) has
// to fan out across every one of PARTITION BY intDiv(ledger, 1000000)'s
// ~473 ledger-range partitions the address ever touched (~140 of them for
// the pathological address) — an equality filter on `address` prunes
// WITHIN a touched partition (address already leads this table's own
// ORDER BY) but cannot prune ACROSS partitions, since partitioning is by
// ledger range, not by address. The projection doesn't change that
// partitioning (a projection can't declare its own PARTITION BY — it's
// always co-partitioned with its parent table), so its win is orthogonal:
// MATERIALIZE PROJECTION forces a deterministic, one-time full compaction
// of the touched partitions' parts (vs. today's fragmented ~140/473,
// hostage to background-merge cadence contending with Phase-0's
// concurrent genesis-extension writes into old partitions — the ROADMAP's
// "partly self-heals as parts merge post-Phase-0" note), and its column
// list is narrower than the base table (drops `ingested_at`, which
// AccountMovements() never reads), shrinking bytes moved per touched
// partition. Same retrofit caveat as idx_cb_balance_id: `CREATE TABLE IF
// NOT EXISTS` does NOT retrofit a projection onto an already-existing
// table — this only takes effect on a fresh install. Applying it to r1's
// existing 6.76B-row table needs `ALTER TABLE ... ADD PROJECTION` +
// `MATERIALIZE PROJECTION`, a HEAVY operation deliberately NOT run by
// this DDL or by this change — see
// docs/operations/runbooks/account-movements-projection.md for the
// wrapped procedure + the Phase-0-free-window precondition.
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
		INDEX idx_cb_balance_id JSONExtractString(attributes, 'balance_id') TYPE bloom_filter(0.01) GRANULARITY 4,
		PROJECTION proj_by_address (
			SELECT address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
				movement_kind, provenance, asset, counterparty, amount, attributes
			ORDER BY (address, ledger, tx_hash, op_index, leg_index, direction)
		)
	) ENGINE = ReplacingMergeTree(ingested_at)
	PARTITION BY intDiv(ledger, 1000000)
	ORDER BY (address, ledger, tx_hash, op_index, leg_index, direction)`

// EnsureAccountMovementsTable creates stellar.account_movements if
// absent. Idempotent; classic-movements-backfill calls it at startup
// so the write path never races a missing table on a freshly deployed
// ClickHouse that hasn't had tier1_schema.sql re-applied yet.
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

// accountMovementsInsertChunk is how many ROWS (post-fan-out, not
// input movements — most movements fan out to 2 rows) accumulate
// before one native INSERT batch is sent. A caller may hand
// InsertAccountMovements an entire backfill window's decoded movement
// set (the task's "fat batches, >=10k rows" target); chunking here
// bounds any single INSERT payload independently of how large a
// window's total row count grows.
const accountMovementsInsertChunk = 20_000

// InsertAccountMovements fans out + batch-inserts movements into
// stellar.account_movements. Retry-safe: ReplacingMergeTree absorbs a
// duplicate re-send of an already-written window (the same idempotent
// re-derivation guarantee as every other ADR-0034 lake/serving
// writer) — a caller that fails partway through a multi-chunk send
// can simply retry the whole batch. Returns the number of ROWS sent
// (not deduped — unlike Postgres's ON CONFLICT ... RETURNING, a
// ClickHouse INSERT doesn't observe how many rows survive merge-time
// dedup; "landed" isn't directly measurable here the way
// BatchInsertClassicMovements' return value was).
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
	// Deterministic order (matches the table's own ORDER BY key) —
	// unlike the retired Postgres writer this isn't for row-lock
	// ordering (ClickHouse inserts don't take row locks), just for
	// reproducible batches in tests and logs.
	sortAccountMovementRows(rows)

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

// insertAccountMovementChunk sends one native batch (<=
// accountMovementsInsertChunk rows).
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

// marshalAccountMovementAttributes renders Attributes as the string
// to pass through to the driver — '{}' for nil/empty (matching the
// column DEFAULT), json.Marshal's output otherwise. Same convention
// as timescale's marshalClassicMovementAttributes.
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

// sortAccountMovementRows sorts by the table's exact ORDER BY key
// (address, ledger, tx_hash, op_index, leg_index, direction).
func sortAccountMovementRows(rows []AccountMovementRow) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := &rows[i], &rows[j]
		if a.Address != b.Address {
			return a.Address < b.Address
		}
		if a.Ledger != b.Ledger {
			return a.Ledger < b.Ledger
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

// openAccountMovementsWrite dials ClickHouse for
// InsertAccountMovements' batch INSERTs — the cheap-append write
// class (a finite execution ceiling), same shape as
// openParticipantWrite, kept as its own opener per this package's
// per-writer-file convention (participant_backfill.go, sink.go each
// define their own).
func openAccountMovementsWrite(ctx context.Context, addr string) (driver.Conn, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{Database: "stellar"},
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

// MaxAccountMovementLedger returns the highest ledger already present
// in stellar.account_movements within [from,to] inclusive — the
// ClickHouse-native resume point classic-movements-backfill uses in
// place of a Postgres-persisted cursor (ADR-0048 D2: "no Postgres in
// the loop"). found=false when nothing has been written in-range yet.
//
// No FINAL: max() over duplicate ReplacingMergeTree parts is correct
// without dedup (a duplicate row shares the same ledger value), so
// this stays a cheap, un-deduped read even over a large window.
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

// ClaimableBalanceCreateRow is one resolved claimable_balance_create
// movement's asset/amount/creator, keyed by balance_id in
// FindClaimableBalanceCreates' returned map.
type ClaimableBalanceCreateRow struct {
	Asset     string
	Amount    *big.Int
	CreatedBy string
}

// cbLookupExtTableChunkSize bounds how many balance_id hex strings go
// into a single external-table semijoin query (see
// FindClaimableBalanceCreates' doc comment for the full failure
// history). Unlike the retired inlined-IN-list chunk bound, this is
// NOT driven by a SQL-text-size ceiling — an external table sends ids
// as column data over the native protocol, not as inlined SQL
// literals, so there is no `max_query_size` exposure at any size. This
// bound exists purely as a safety ceiling on how large a single
// server-side hash-set (and driver-side Table.Append batch) gets built
// per query. 1,000,000 comfortably covers every real window observed
// so far (up to 1,393,786 ids, 2026-07-13) in one or two queries,
// while still capping worst-case per-query footprint if some future
// window is far larger.
const cbLookupExtTableChunkSize = 1_000_000

// chunkStrings splits ids into contiguous sub-slices of at most n
// elements each (the last sub-slice may be shorter). n<=0 is treated
// as "no chunking" (one sub-slice containing all of ids). Extracted
// as its own function so the chunking arithmetic (exact multiples,
// remainders, n larger than len(ids)) is unit-testable without a
// ClickHouse connection.
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

// FindClaimableBalanceCreates batch-resolves MANY pending claim/
// clawback refs' claimable_balance_create rows — the ADR-0048 D2
// ClickHouse-native replacement for the retired Postgres
// timescale.Store.FindClaimableBalanceCreate lookup (ADR-0047 Phase
// 3's cross-window correlation fallback tier; see
// classicmovements/dispatcher_adapter.go's Decoder doc for the full
// three-tier resolution: in-run index, this lookup, then unresolved).
// It replaces a now-removed single-ref FindClaimableBalanceCreate that
// classic-movements-backfill called once per pending ref, serially.
//
// 2026-07-12 finding: the claimable-balance-bot era (ledgers
// ~34M-40M) surfaces thousands of pending refs per window, and each
// serial per-ref lookup was a 6.5s full scan of
// stellar.account_movements' 973M rows — the drain was crawling.
// idx_cb_balance_id (this package's accountMovementsDDL and
// deploy/clickhouse/tier1_schema.sql; already applied to r1 via a
// one-off ALTER TABLE) is a bloom_filter skip-index on
// JSONExtractString(attributes, 'balance_id') that brought a single
// lookup to ~84ms (~77x). Batching on top of that turns an entire
// window's fallback resolution into (at most) a handful of queries
// regardless of how many refs it has, instead of one query per ref.
//
// 2026-07-13 finding #1 (fixed same day, superseded below): "a handful
// of queries" needed a floor. Batching into ONE query per window
// worked fine until the claimable-balance spam era (ledger ~49.3M)
// produced windows with over a million pending refs — the driver
// inlines the whole []string IN-list into the SQL text, and anything
// past ~3,400 ids blew ClickHouse's `max_query_size` (256 KiB
// default: `code: 62, Max query size exceeded`), failing the entire
// lookup and leaving every claim in that window's batch unresolved.
// The first fix chunked the inlined IN-list at 2,000 ids per query.
//
// 2026-07-13 finding #2 (this fix, ~1h after #1 shipped): chunking the
// inlined IN-list traded one failure mode for a worse one. With 2,000
// ids inlined per chunk, idx_cb_balance_id's bloom_filter(0.01) skip
// index stops helping and starts hurting: across the table's ~119k
// granules, the chance a granule's bloom filter reports a false
// positive for at least one of 2,000 probed ids is
// 1-(1-0.01)^2000 ≈ 1 (indistinguishable from certainty), so every
// chunk degenerates into a near-full parallel scan of the wide
// `attributes` column over all 973M rows — and that blew the
// connection's `max_memory_usage` (`code: 241, Query memory limit
// exceeded, would use 10.00 GiB`). Single-id point lookups were never
// affected (a 1-in-100 false-positive rate keeps a literal `= ?` or
// tiny `IN (?)` cheap: ~84ms, confirmed live) — only the batched path
// broke, because bloom-filter false-positive rate compounds with probe
// count, not because the index itself regressed.
//
// The terminal fix: ids are passed as a ClickHouse EXTERNAL TABLE
// (`clickhouse.WithExternalTable`, native-protocol side-channel, not
// SQL text) and matched via `JSONExtractString(...) IN cb_ids` — a
// hash-set semijoin whose cost is O(ids), not a function of granule
// count or bloom FPR. This intentionally does NOT use
// idx_cb_balance_id at all (an external table's contents aren't known
// at query-analysis time, so there's nothing for the skip index to
// prune against) — the `movement_kind = 'claimable_balance_create'`
// LowCardinality prewhere already scopes the scan to the ~2.1M
// cb-create rows before the semijoin runs, and a per-query
// `WithSettings` (max_threads=4, max_memory_usage=8 GiB,
// max_bytes_before_external_group_by=2 GiB) bounds the worst case.
// cbLookupExtTableChunkSize (1,000,000) still chunks the external
// table itself, purely as a footprint safety bound, not to dodge a
// text-size or index-FPR ceiling — see its doc comment.
//
// idx_cb_balance_id remains valuable and is NOT being removed: any
// TRUE point lookup (a literal `= ?` or a small hand-written `IN (?)`
// with a handful of ids) still benefits from it, and ClickHouse's
// skip-index pruning for such a query only fires when the WHERE
// predicate is textually IDENTICAL to the indexed expression — so the
// WHERE clause below MUST stay exactly
// `JSONExtractString(attributes, 'balance_id')` (not a rewritten
// equivalent: a CTE, a different function, a cast, …) even though
// THIS function's batched external-table query no longer benefits
// from that index itself. Any divergence silently falls back to a
// full scan with no query error to signal it, for either access
// pattern.
//
// The returned map contains ONLY found ids; a balance_id absent from
// it means no matching create row exists YET for it in what's been
// backfilled to ClickHouse so far — a genuine ADR-0047 D4
// recognizable-incompleteness signal (the create may be outside the
// range backfilled so far, or — rarely — same-ledger ordering noise),
// never a query failure. Callers must count + log misses, never guess
// an amount. Duplicate rows for the same balance_id (ReplacingMergeTree
// parts not yet merged, or the same id appearing in two different
// chunks) are identical by construction; the first one scanned wins.
// Empty input returns an empty, non-nil map without querying
// ClickHouse. ctx is checked between chunks so a caller's cancellation
// or deadline is honored without waiting for every remaining chunk.
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

// cbLookupCreatesQuery matches chunk's ids against the external table
// (`cb_ids`, one `balance_id String` column) findClaimableBalanceCreatesChunk
// attaches via clickhouse.WithExternalTable — a hash-set semijoin, not
// an inlined IN-list. See FindClaimableBalanceCreates' doc comment for
// why the WHERE expression must stay textually exact.
// The SETTINGS clause lives in the SQL text, not clickhouse.WithSettings:
// observed live (2026-07-13) that per-query context settings did NOT reach
// the server when combined with WithExternalTable — the failing query ran
// at openRead's connection-level 10 GiB ceiling, not the requested 8 GB.
// use_skip_indexes=0 is load-bearing: evaluating idx_cb_balance_id (bloom,
// 1% FPR) against a multi-thousand-element IN-set matches essentially every
// granule (1-(1-0.01)^N → 1) and the index machinery itself blew the 10 GiB
// limit; a plain PREWHERE-on-movement_kind scan + hash probe streams within
// bounds (measured ~2.5 min over 695M cb-create rows at 4 threads).
const cbLookupCreatesQuery = `
	SELECT JSONExtractString(attributes, 'balance_id') AS balance_id, asset, amount, address
	FROM stellar.account_movements
	WHERE movement_kind = 'claimable_balance_create'
	  AND JSONExtractString(attributes, 'balance_id') IN cb_ids
	SETTINGS use_skip_indexes = 0, max_threads = 4, max_memory_usage = 8000000000`

// findClaimableBalanceCreatesChunk runs cbLookupCreatesQuery against
// one chunk (<= cbLookupExtTableChunkSize ids), passed as a
// server-side external table rather than inlined SQL, and merges
// matches into out. Building the ext.Table is per-chunk (the driver
// has no reset/reuse API for one), which is cheap relative to the
// query itself.
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

	// Query bounds are in cbLookupCreatesQuery's SQL-text SETTINGS
	// clause — see its doc comment for why WithSettings cannot be
	// trusted next to WithExternalTable.
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

// AccountMovementVerifyCounts maps movement_kind -> the number of
// DISTINCT movements (not rows — a two-participant movement is 2 rows
// sharing one (tx_hash, op_index, leg_index) identity) currently in
// stellar.account_movements for a ledger window.
type AccountMovementVerifyCounts map[string]uint64

// VerifyAccountMovementsWindow recounts [from,to] from
// stellar.account_movements, grouped by movement_kind, collapsing
// each movement's 1-2 fan-out rows back to one count via
// uniqExact(tx_hash, op_index, leg_index) — tx_hash is unique
// network-wide (stellar.tx_hash_index's ORDER BY tx_hash precedent),
// so this needs no `ledger` in the tuple beyond the WHERE-scope.
//
// This is classic-movements-backfill's -verify mode: a cheap,
// window-scoped reconciliation of "ops decoded this run" against
// "movements now visible in ClickHouse" (ADR-0047 D4 applied to the
// CH write target) — NOT the full ADR-0033 substrate/recognition/
// projection machinery, which doesn't apply to a historical-only,
// non-projected write path like this one.
//
// No FINAL: uniqExact over duplicate ReplacingMergeTree parts is
// still exact (an identical duplicate row is the identical tuple, so
// it doesn't inflate the distinct count) — the same reasoning
// StreamClassicOps' NO-FINAL note documents for this table family.
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

// AccountMovementFilter narrows an AccountMovements read (ADR-0048
// D5's GET /v1/accounts/{g}/movements). Zero-value fields mean "no
// filter" on that dimension.
type AccountMovementFilter struct {
	Kind      string                   // movement_kind exact match; "" = any
	Direction AccountMovementDirection // exact match; "" = any
	Asset     string                   // canonical asset id exact match; "" = any
}

// AccountMovementCursor is the keyset position for AccountMovements
// pagination (ADR-0048 D5) — descending (ledger, tx_hash, op_index,
// leg_index), the table's ORDER BY suffix after the fixed `address`
// equality filter. Zero value (Ledger==0) means "from the newest"
// (first page) — same IsSet/Ledger==0 sentinel convention as
// ExplorerCursor above.
type AccountMovementCursor struct {
	Ledger   uint32
	TxHash   string
	OpIndex  uint32
	LegIndex uint32
}

// IsSet reports whether the cursor points past the newest row (a
// continuation page, not the first).
func (c AccountMovementCursor) IsSet() bool { return c.Ledger > 0 }

const accountMovementCols = `ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
	movement_kind, provenance, asset, counterparty, amount, attributes`

// AccountMovements returns one address's movement feed from
// stellar.account_movements (ADR-0048 D2/D5), newest first, keyset-
// paged by the composite (ledger, tx_hash, op_index, leg_index)
// cursor. `address` is an equality filter on the table's ORDER BY
// PREFIX, so this is a single contiguous primary-key range scan — the
// exact property ADR-0048 D1 designed this table for (unlike
// AccountTransactions/AccountOperations above, which UNION two arms
// against the raw lake's source_account/participant split, this needs
// no UNION).
//
// internal/api/v1/explorer/movements.go merges this CH-native
// pre-P23 archive with timescale.Store.ListSEP41TransfersByAddress's
// post-P23 Postgres tail to serve the full GET
// /v1/accounts/{g}/movements feed (ADR-0048 D5).
//
// No FINAL: same acceptable-eventual-consistency posture as
// AccountOperations/AccountTransactions above — a duplicate row from
// an in-flight re-derive merge is a rare, benign visual repeat (it
// disappears on the next background ReplacingMergeTree merge), not a
// correctness issue, and FINAL's read-time dedup cost isn't worth
// paying on every paginated request.
func (r *ExplorerReader) AccountMovements(ctx context.Context, address string, limit int, cur AccountMovementCursor, filter AccountMovementFilter) ([]AccountMovementRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 25
	}
	var sb strings.Builder
	sb.WriteString("SELECT " + accountMovementCols + " FROM stellar.account_movements WHERE address = ?")
	args := []any{address}
	if filter.Kind != "" {
		sb.WriteString(" AND movement_kind = ?")
		args = append(args, filter.Kind)
	}
	if filter.Direction != "" {
		sb.WriteString(" AND direction = ?")
		args = append(args, string(filter.Direction))
	}
	if filter.Asset != "" {
		sb.WriteString(" AND asset = ?")
		args = append(args, filter.Asset)
	}
	if cur.IsSet() {
		sb.WriteString(" AND (ledger, tx_hash, op_index, leg_index) < (?, ?, ?, ?)")
		args = append(args, cur.Ledger, cur.TxHash, cur.OpIndex, cur.LegIndex)
	}
	sb.WriteString(" ORDER BY ledger DESC, tx_hash DESC, op_index DESC, leg_index DESC LIMIT ?")
	args = append(args, limit)

	rows, err := r.conn.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: account %s movements: %w", address, err)
	}
	defer func() { _ = rows.Close() }()
	return scanAccountMovementRows(rows, address)
}

// scanAccountMovementRows scans accountMovementCols rows into
// AccountMovementRow values, stamping Address (not itself selected —
// every row already matches the query's address filter).
func scanAccountMovementRows(rows driver.Rows, address string) ([]AccountMovementRow, error) {
	var out []AccountMovementRow
	for rows.Next() {
		var row AccountMovementRow
		var direction, attrs string
		var amt *big.Int
		if err := rows.Scan(
			&row.Ledger, &row.LedgerCloseTime, &row.TxHash, &row.OpIndex, &row.LegIndex,
			&direction, &row.MovementKind, &row.Provenance, &row.Asset, &row.Counterparty,
			&amt, &attrs,
		); err != nil {
			return nil, fmt.Errorf("clickhouse: scan account movement row: %w", err)
		}
		row.Address = address
		row.Direction = AccountMovementDirection(direction)
		row.Amount = amt
		if attrs != "" && attrs != "{}" {
			if uerr := json.Unmarshal([]byte(attrs), &row.Attributes); uerr != nil {
				return nil, fmt.Errorf("clickhouse: unmarshal account movement attributes: %w", uerr)
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
