package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// explorerScanSettings is the per-query resource pin appended to every
// SCAN-SHAPED explorer read — any query whose cost is set by the size of a
// lake table (bloom-skip-index probes, window GROUP BYs, FINAL prefix scans)
// rather than by a primary-key point lookup.
//
// Why (measured on r1, 2026-07-29): at DEFAULT max_threads a
// ledger_entries_current probe fanned out over the post-D3 part layout to a
// 4.76 GiB peak — 40× the 89 MiB the IDENTICAL probe costs at
// max_threads = 4. The read amplification is thread scheduling (per-stream
// read buffers × many concurrent part ranges), not the table; pinning
// threads is the real lever. The explicit 8 GiB tracked ceiling
// (8589934592) makes a future part-layout shift fail the ONE query loudly
// instead of silently starving the shared host — same posture as
// classifyTTLLivenessBatch / boundedScanSettings / liveOfferScanSettings.
//
// Keyed point reads (PK-prefix lookups on (entry_type, key_xdr), per-ledger
// partition-pruned reads) are measured fast (0.08s) and deliberately do NOT
// carry this — they never fan out.
//
// The clause lives in SQL text, not clickhouse.WithSettings, following the
// cbLookupCreatesQuery precedent (settings observed to not reach the server
// in some driver paths) and so reader tests can pin it.
//
// The external-spill pair converts what would be a hard OOM at the 8 GiB
// cap into a slower disk-backed success: aggregation/sort state above 4 GB
// spills instead of dying. Added 2026-07-30 after the 30-day contracts
// directory GROUP BY (uniqExact over a 4-tuple, per contract, across
// ~470k ledgers) failed its detached refresh 23/23 times at the pin —
// measured on r1: OOM without spill, 55.7 s clean completion with it,
// comfortably inside the 3-minute detached budget. Spill only activates
// above its threshold, so the fast majority of scans are unaffected.
const explorerScanSettings = ` SETTINGS max_threads = 4, max_memory_usage = 8589934592,` +
	` max_bytes_before_external_group_by = 4000000000, max_bytes_before_external_sort = 4000000000`

// schemaProbeRetryAfter is how long a probe that got NO answer (a
// transport error, a deadline, or a ClickHouse error that is not a schema
// verdict) waits before querying again. Without it, every explorer read
// during a ClickHouse outage would add its own probe query — doubling load
// on the store exactly when it is least able to absorb it. Short enough
// that a recovered store is picked up within one window.
const schemaProbeRetryAfter = 5 * time.Second

// schemaProbe caches the answer to a "does this schema object exist"
// question — but ONLY once the server has actually answered one
// (C1-048, audit-2026-07-23). See [ExplorerReader.probeSchema] for why a
// sync.Once was the wrong primitive here.
type schemaProbe struct {
	mu      sync.Mutex
	settled bool      // an authoritative answer was received
	present bool      // meaningful only when settled
	retryAt time.Time // while now < retryAt, degrade without re-querying

	// retryAfter overrides [schemaProbeRetryAfter]. Zero uses the default;
	// tests set a negative value to disable the negative cache.
	retryAfter time.Duration
}

// schemaAbsentCodes are the ClickHouse error codes that constitute a
// DEFINITIVE "that schema object does not exist" answer. Nothing else does.
//
// This list is deliberately narrow and the asymmetry is deliberate too
// (C1-048, second review): a code we forgot to list costs one extra probe
// per retry window — a rounding error. A code we list that is NOT a schema
// verdict costs a PERMANENT silent degradation, because it latches the
// probe false for the process lifetime. ClickHouse raises exceptions for
// plain resource conditions — 159 TIMEOUT_EXCEEDED, 202
// TOO_MANY_SIMULTANEOUS_QUERIES, 241 MEMORY_LIMIT_EXCEEDED, 209
// SOCKET_TIMEOUT — and one such blip on lecVersionProbe would mean serving
// non-final intra-ledger balances forever. 1002 UNKNOWN_EXCEPTION is
// excluded for the same reason: it is a catch-all, not an answer.
var schemaAbsentCodes = map[int32]struct{}{
	8:  {}, // THERE_IS_NO_COLUMN
	16: {}, // NO_SUCH_COLUMN_IN_TABLE
	47: {}, // UNKNOWN_IDENTIFIER
	60: {}, // UNKNOWN_TABLE
	81: {}, // UNKNOWN_DATABASE
}

// isSchemaAbsent reports whether err is the server saying, definitively,
// that the probed column/table/database does not exist.
func isSchemaAbsent(err error) bool {
	var chErr *clickhouse.Exception
	if !errors.As(err, &chErr) {
		return false
	}
	_, ok := schemaAbsentCodes[chErr.Code]
	return ok
}

// ExplorerReader serves the network-explorer read path (ADR-0038) directly
// from the certified Tier-1 lake (ADR-0034): the full chain to genesis —
// ledgers, transactions, operations, contract events — lives in ClickHouse,
// not Postgres. Construct once at startup, reuse across requests, Close at
// shutdown. All reads are by immutable key (ledger_seq / tx_hash), so results
// are cacheable indefinitely.
//
// Phase A scope: ledger + transaction + operation + contract reads. Account
// state (balances) is Phase C and reads a different (to-be-populated) table.
type ExplorerReader struct {
	conn driver.Conn

	// tx-hash fast path (perf-todo §4): whether stellar.tx_hash_index
	// exists on this deployment. false → every hash lookup takes the
	// bloom-skip-index scan, exactly as before the index existed.
	txIndexProbe schemaProbe

	// contractLedgersProbe probes stellar.contract_active_ledgers (the
	// per-(contract, ledger) activity index,
	// deploy/clickhouse/contract_active_ledgers.sql). Present + non-empty
	// → ContractEventsRecent bounds its scan to the contract's active
	// ledgers (quiet-contract cold reads drop from ~9s to ms — site audit
	// 2026-08-07/08); absent → the unbounded reverse walk, exactly as
	// before the index existed. requireRows, like tx_hash_index: per-
	// contract emptiness is served as an authoritative "no events", so an
	// existing-but-empty index (MV dropped / TRUNCATE) must read as
	// index-unavailable, not as "no contract has events".
	contractLedgersProbe schemaProbe

	// instanceChangesProbe probes stellar.contract_instance_changes (the
	// per-contract instance-executable timeline,
	// deploy/clickhouse/contract_instance_changes.sql). Present +
	// non-empty → ContractCodeHistory reads the keyed timeline
	// (primary-key walk, ms) instead of the scan-shaped key_xdr
	// predicate over the whole changes log (8s+ cold, the last
	// persistent 503 in the 2026-08-09 route sweep). requireRows for the
	// same reason as the siblings: per-contract emptiness is served as
	// authoritative, so an existing-but-empty index must read as
	// unavailable.
	instanceChangesProbe schemaProbe

	// censusProbe probes stellar.contracts_census_daily (the day-keyed
	// per-contract event counts, deploy/clickhouse/
	// contracts_census_daily.sql). Present + non-empty → RecentContracts
	// sums day rows (sub-second) instead of the 40s uniqExact GROUP BY
	// over billions of contract_events rows. requireRows as with the
	// siblings.
	censusProbe schemaProbe

	// accountsStatsProbe probes stellar.accounts_stats (the /accounts hub
	// analytics rollup, deploy/clickhouse/accounts_stats_rollup.sql).
	// requireRows: an unpopulated rollup 503s the stats endpoint rather
	// than serving zeros as facts.
	accountsStatsProbe schemaProbe

	// holdersRollupProbe probes stellar.asset_holders_rollup (inventory
	// #4, deploy/clickhouse/asset_holders_rollup.sql). Present +
	// non-empty → AssetHolders serves keyed precomputed boards; absent →
	// the legacy two-FINAL-scans-per-request path. requireRows: a
	// never-exchanged empty rollup must read as unavailable, not as
	// "no asset has holders".
	holdersRollupProbe schemaProbe

	// cap67 movements watermark cache (see Cap67MovementsWatermark in
	// cap67_movements.go).
	cap67WMMu sync.Mutex
	cap67WM   uint32
	cap67WMAt time.Time

	// opsBySourceProbe probes whether stellar.ops_by_source (the slim
	// sourced-history projection, deploy/clickhouse/ops_by_source.sql)
	// exists. The account-history readers REFUSE without it — a silent
	// bloom-scan fallback would quietly restore the 6s sourced arm, and a
	// silent empty arm would hide the account's own transactions.
	opsBySourceProbe schemaProbe

	// accountActivityProbe probes stellar.account_activity (the per-account
	// activity watermark, deploy/clickhouse/account_activity.sql, #31).
	// Present + non-empty → AccountOperations bounds each keyset arm's
	// reverse primary-key resolve with the account's last-active ledger
	// (`ledger_seq <= ?`), so a long-idle account's page stops at its real
	// last activity instead of walking granules back from the tip (~4s
	// live for a 46d-idle account, 2026-08-24). Absent/empty → the
	// unbounded resolve, exactly as before the watermark existed. The
	// bound is a pure perf hint, never authority: a missing watermark row
	// falls back to the unbounded scan, and an available watermark is a
	// safe UPPER bound by construction (its MVs cover every account role
	// the query's key sources cover — see the tier1_schema.sql invariant
	// note; too-low would HIDE rows, which is the unacceptable direction).
	accountActivityProbe schemaProbe

	// lecVersionProbe probes whether stellar.ledger_entries_current carries a
	// `version` column — the (ledger_seq<<32)|intra_ledger_seq RMT version
	// that the D3 reproject introduces (deploy/clickhouse/
	// ledger_entries_current_intra_ledger_seq.sql). D3 is freeze-gated and
	// runs AFTER D2; until it lands, R1's table is still
	// ReplacingMergeTree(ledger_seq) with no such column. Queries that
	// tie-break same-ledger changes must use `version` where it exists
	// (C2-4c) and fall back to `ledger_seq` where it does not — otherwise
	// they 500 with "Unknown identifier `version`" (site-audit S3).
	lecVersionProbe schemaProbe

	// wealthCache backs AccountsByWealthCached. The wealth ranking is a
	// FINAL scan over 43.6M rows that cannot fit a request deadline
	// (site-audit S3); it is served from here and refreshed in the
	// background. Non-nil for every reader built by the constructors.
	wealthCache *accountsWealthCache

	// wealthRefreshErr, when set, is called with any error from the
	// detached wealth-ranking refresh. Optional — nil swallows the error
	// as before. Wired by the API so a persistently-failing refresh (which
	// would pin /v1/accounts on its 503 warming state) is visible in logs.
	wealthRefreshErr func(error)

	// stateCache + stateFlight back AccountStateCached. Account/issuer
	// detail reads scan the 4.2B-row current-state table under the bounded
	// serving profile, so concurrent detail requests contended into 8s
	// (site-audit follow-up); the cache serves repeats and cuts that load.
	stateCache  *accountStateCache
	stateFlight *perKeyFlight

	// ttlVerdicts fronts ClassifyTTLLiveness for SoroswapPairReserves'
	// archived-pair filter. The classification is a scan of the ~586M-row
	// ttl prefix that cannot run per request (route-sweep 2026-07-29:
	// GET /v1/pools/reserves 503'd on it); verdicts move on day/week
	// scales, so they are served stale-while-revalidate. Non-nil for every
	// reader built by the constructors.
	ttlVerdicts *ttlLivenessCache

	// refreshGate bounds concurrently-running detached cache refreshes
	// (account state here + the API-layer explorer caches via
	// DetachedRefreshGate) — see refresh_gate.go. Non-nil for every
	// reader built by the constructors; nil (test-built readers) admits
	// everything.
	refreshGate *RefreshGate
}

// SetWealthRefreshErrorHandler installs a callback for background
// refresh failures — the wealth ranking's AND the TTL-liveness verdict
// cache's (both share the visibility rationale: a persistently failing
// detached refresh silently pins its surface stale/warming). Call once at
// wiring time.
func (r *ExplorerReader) SetWealthRefreshErrorHandler(fn func(error)) {
	r.wealthRefreshErr = fn
	if r.ttlVerdicts != nil {
		r.ttlVerdicts.onErr = fn
	}
}

// NewExplorerReader dials ClickHouse (native protocol) with a request-sized
// pool and pings it, authenticating as the ops-batch user when
// STELLARINDEX_CLICKHOUSE_OPS_USER/_PASSWORD are set (ops_auth.go) and
// otherwise as CH's unauthenticated `default` user (empty username/password)
// — the pre-ADR-0048-D4 behavior. Every non-API caller (the aggregator's
// explorer reader, stellarindex-ops issuer-enrich / supply-seed) keeps
// calling this constructor unchanged.
func NewExplorerReader(ctx context.Context, addr string) (*ExplorerReader, error) {
	// Ops-batch identity from the environment (2026-08-28 r1 incident;
	// see ops_auth.go) — CH `default` user when unset, so every
	// non-API caller is byte-for-byte unchanged outside the ops env.
	auth, err := opsAuth()
	if err != nil {
		return nil, err
	}
	return NewExplorerReaderAuth(ctx, addr, auth.Username, auth.Password)
}

// NewExplorerReaderAuth is [NewExplorerReader] with an explicit CH
// username/password — ADR-0048 D4's serving-isolation profile. The API
// binary calls this with `storage.clickhouse_serving_user` /
// `clickhouse_serving_password_env` (internal/config's StorageConfig) so
// its per-request explorer reads (including GET /v1/accounts/{g}/movements,
// ADR-0048 D5) run under the dedicated `api_serving` CH settings profile
// (bounded threads/memory/execution-time, priority above merges and
// backfill inserts — configs/ansible/roles/archival-node/tasks/
// 20-clickhouse-serving-profile.yml) instead of the unbounded `default`
// user every other CH connection in this repo still uses. Both args empty
// is byte-for-byte the old NewExplorerReader behavior (clickhouse-go
// treats an empty Auth.Username as CH's `default` user).
func NewExplorerReaderAuth(ctx context.Context, addr, username, password string) (*ExplorerReader, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:        []string{addr},
		Auth:        clickhouse.Auth{Database: "stellar", Username: username, Password: password},
		Settings:    clickhouse.Settings{"max_execution_time": 30},
		DialTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second,
		// 8 -> 16 (2026-08-13): explorer pages fan out — one cold
		// contract page issues five concurrent reads — so a pool of 8
		// was barely one and a half visitors wide, and the detached
		// refresh gate (half the pool, see DefaultDetachedRefreshLimit)
		// was narrower than a single page. Each explorer scan is pinned
		// to max_threads = 4 and r1 has 20 cores at ~2 concurrent
		// queries idle, so this stays well inside the host.
		MaxOpenConns:    16,
		MaxIdleConns:    8,
		ConnMaxLifetime: time.Hour,
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse: open explorer reader %s: %w", addr, err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse: ping explorer reader %s: %w", addr, err)
	}
	return &ExplorerReader{
		conn:        conn,
		wealthCache: newAccountsWealthCache(),
		stateCache:  newAccountStateCache(),
		stateFlight: newPerKeyFlight(),
		refreshGate: NewRefreshGate(DefaultDetachedRefreshLimit),
		ttlVerdicts: newTTLLivenessCache(func(ctx context.Context, keys []string) (map[string]TTLLiveness, error) {
			// Verdicts are judged at the lake's tip AS OF compute time —
			// "current" means current relative to what the lake holds now.
			_, asOf, err := entryChangeLedgerBounds(ctx, conn)
			if err != nil {
				return nil, err
			}
			return ClassifyTTLLiveness(ctx, conn, keys, asOf)
		}),
	}, nil
}

// Close releases the connection pool.
func (r *ExplorerReader) Close() error { return r.conn.Close() }

// LedgerHeader is one ledger header from stellar.ledgers. Hash fields are hex
// strings as stored. total_coins / fee_pool are XLM stroops (Int64 in the
// lake) — they exceed 2^53 so the API serialises them as strings (ADR-0003).
type LedgerHeader struct {
	Seq               uint32
	CloseTime         time.Time
	LedgerHash        string
	PrevHash          string
	ProtocolVersion   uint32
	TxCount           uint32
	OpCount           uint32
	SorobanEventCount uint32
	TotalCoins        int64
	FeePool           int64
	BaseFee           uint32
	BaseReserve       uint32
}

// TxSummary is one transaction summary from stellar.transactions. Memo is
// already decoded to a string at ingest; memo_type carries the discriminant.
type TxSummary struct {
	Seq            uint32
	CloseTime      time.Time
	TxHash         string
	TxIndex        uint32
	SourceAccount  string
	FeeCharged     int64
	MaxFee         int64
	OperationCount uint16
	Successful     bool
	ResultCode     int32
	MemoType       string
	Memo           string
}

const ledgerCols = `ledger_seq, close_time, ledger_hash, prev_hash, protocol_version,
	tx_count, op_count, soroban_event_count, total_coins, fee_pool, base_fee, base_reserve`

func scanLedger(rows driver.Rows) (LedgerHeader, error) {
	var l LedgerHeader
	err := rows.Scan(&l.Seq, &l.CloseTime, &l.LedgerHash, &l.PrevHash, &l.ProtocolVersion,
		&l.TxCount, &l.OpCount, &l.SorobanEventCount, &l.TotalCoins, &l.FeePool, &l.BaseFee, &l.BaseReserve)
	return l, err
}

// RecentLedgers returns up to `limit` ledgers in descending sequence order. If
// beforeSeq > 0, only ledgers strictly below it are returned (keyset
// pagination — the next page descends from the previous page's last seq).
func (r *ExplorerReader) RecentLedgers(ctx context.Context, limit int, beforeSeq uint32) ([]LedgerHeader, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT ` + ledgerCols + ` FROM stellar.ledgers FINAL`
	args := []any{}
	if beforeSeq > 0 {
		// Bound the cursor page to the same tail-window width the tip
		// branch uses — without a lower bound this was the exact
		// unbounded whole-table FINAL merge the tip branch was rewritten
		// to avoid (O(table), caller-controlled via public ?before=).
		// Ledgers are contiguous genesis→tip, so a window ≥ 25× the max
		// page size can never truncate a legitimate page. Clamp at 0:
		// uint32 underflow near genesis would wrap and return nothing.
		lower := uint32(0)
		if beforeSeq > uint32(recentLedgersTailWindow) {
			lower = beforeSeq - uint32(recentLedgersTailWindow)
		}
		q += ` WHERE ledger_seq < ? AND ledger_seq >= ?`
		args = append(args, beforeSeq, lower)
	} else {
		// Tip query (the explorer's hot path — it backs "Total XLM", base fee
		// and the latest-ledger line). Without a lower bound this is
		// `FINAL ... ORDER BY ledger_seq DESC LIMIT n` across the WHOLE table,
		// so ClickHouse merges every part (490 of them / 124M rows) to return
		// n rows — O(table), not O(n), and it got worse as part count grew
		// during the backfill. Bounding to a tail window prunes to the newest
		// partition and makes it a small merge, exactly as NetworkThroughput
		// and OperationTypeStats already do. The window is a wide multiple of
		// the max page size so it can never truncate a legitimate first page.
		q += ` WHERE ledger_seq > (SELECT max(ledger_seq) FROM stellar.ledgers) - ?`
		args = append(args, uint32(recentLedgersTailWindow))
	}
	q += ` ORDER BY ledger_seq DESC LIMIT ?` + explorerScanSettings
	args = append(args, limit)

	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: recent ledgers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]LedgerHeader, 0, limit)
	for rows.Next() {
		l, err := scanLedger(rows)
		if err != nil {
			return nil, fmt.Errorf("clickhouse: scan ledger: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// LedgerBySeq returns a single ledger header. found=false (nil error) when the
// sequence is absent (out of range / not yet ingested).
func (r *ExplorerReader) LedgerBySeq(ctx context.Context, seq uint32) (LedgerHeader, bool, error) {
	q := `SELECT ` + ledgerCols + ` FROM stellar.ledgers FINAL WHERE ledger_seq = ? LIMIT 1`
	rows, err := r.conn.Query(ctx, q, seq)
	if err != nil {
		return LedgerHeader{}, false, fmt.Errorf("clickhouse: ledger %d: %w", seq, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return LedgerHeader{}, false, rows.Err()
	}
	l, err := scanLedger(rows)
	if err != nil {
		return LedgerHeader{}, false, fmt.Errorf("clickhouse: scan ledger %d: %w", seq, err)
	}
	return l, true, nil
}

// CloseTimeForLedger returns the on-chain close time of a single ledger from
// stellar.ledgers. found=false (nil error) when the sequence has no row (not
// yet ingested / out of range) — the caller decides how to treat the miss.
//
// This is the authoritative every-ledger close-time source the supply-snapshot
// ledger resolvers use to stamp a snapshot's ObservedAt (audit M4-callers): a
// re-derived HISTORICAL snapshot must carry the ledger's real close time, never
// the wall-clock write-time. Both callers fail closed on found=false rather
// than falling back to time.Now() (which is the very defect being removed).
//
// FINAL, like LedgerBySeq: stellar.ledgers is ReplacingMergeTree(ingested_at),
// so a re-ingested ledger leaves an un-merged duplicate part until a background
// merge; FINAL collapses it. A single-row point read stays cheap under FINAL.
func (r *ExplorerReader) CloseTimeForLedger(ctx context.Context, seq uint32) (time.Time, bool, error) {
	const q = `SELECT close_time FROM stellar.ledgers FINAL WHERE ledger_seq = ? LIMIT 1`
	rows, err := r.conn.Query(ctx, q, seq)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("clickhouse: close time for ledger %d: %w", seq, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return time.Time{}, false, rows.Err()
	}
	var closeTime time.Time
	if err := rows.Scan(&closeTime); err != nil {
		return time.Time{}, false, fmt.Errorf("clickhouse: scan close time for ledger %d: %w", seq, err)
	}
	return closeTime.UTC(), true, nil
}

// LatestLedgerAtOrBefore returns the newest stellar.ledgers row with
// ledger_seq <= maxSeq. The supply snapshot's AUTO ledger resolver uses it to
// clamp the live ingestion cursor to the lake's landed tip: the cursor
// (Postgres, realtime) leads stellar.ledgers (CH sink) by seconds, so the
// cursor's own row is routinely not landed yet when a timer-driven snapshot
// fires (r1 supply-snapshot failed every daily run on this race, 2026-08-22).
// FINAL for the same ReplacingMergeTree reason as CloseTimeForLedger; the
// primary key is ledger_seq so the descending point read stays cheap.
func (r *ExplorerReader) LatestLedgerAtOrBefore(ctx context.Context, maxSeq uint32) (uint32, time.Time, bool, error) {
	const q = `SELECT ledger_seq, close_time FROM stellar.ledgers FINAL
		WHERE ledger_seq <= ? ORDER BY ledger_seq DESC LIMIT 1`
	rows, err := r.conn.Query(ctx, q, maxSeq)
	if err != nil {
		return 0, time.Time{}, false, fmt.Errorf("clickhouse: latest ledger at or before %d: %w", maxSeq, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return 0, time.Time{}, false, rows.Err()
	}
	var (
		seq       uint32
		closeTime time.Time
	)
	if err := rows.Scan(&seq, &closeTime); err != nil {
		return 0, time.Time{}, false, fmt.Errorf("clickhouse: scan latest ledger at or before %d: %w", maxSeq, err)
	}
	return seq, closeTime.UTC(), true, nil
}

// LedgerTransactions returns the transactions in a ledger, ordered by tx_index.
func (r *ExplorerReader) LedgerTransactions(ctx context.Context, seq uint32, limit int) ([]TxSummary, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	const q = `SELECT ledger_seq, close_time, tx_hash, tx_index, source_account,
		fee_charged, max_fee, operation_count, successful, result_code, memo_type, memo
		FROM stellar.transactions FINAL WHERE ledger_seq = ? ORDER BY tx_index ASC LIMIT ?`
	rows, err := r.conn.Query(ctx, q, seq, limit)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: ledger %d txs: %w", seq, err)
	}
	defer func() { _ = rows.Close() }()
	return scanTxSummaries(rows)
}

// OpRow is one operation from stellar.operations. OpType is the lake's XDR
// enum string ("OperationTypePayment"); BodyXDR is the base64 body for
// read-time decode (internal/xdrjson). SourceAccount may be empty (the op
// inherits the transaction source).
type OpRow struct {
	Seq           uint32
	CloseTime     time.Time
	TxHash        string
	TxIndex       uint32
	OpIndex       uint32
	OpType        string
	SourceAccount string
	BodyXDR       string
}

const opCols = `ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr`

// opColsLight omits body_xdr — the large per-row column whose read dominates
// the query cost (a bare ledger_seq DESC LIMIT is ~40ms; adding body_xdr over
// this 24B-row / 2TiB table is ~600ms). Used by RecentOperations so the
// network-wide directory listing stays cheap; op_type still carries the type,
// and the per-ledger / detail views read the full body when they need it.
const opColsLight = `ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account`

func scanOps(rows driver.Rows) ([]OpRow, error) {
	var out []OpRow
	for rows.Next() {
		var o OpRow
		if err := rows.Scan(&o.Seq, &o.CloseTime, &o.TxHash, &o.TxIndex, &o.OpIndex,
			&o.OpType, &o.SourceAccount, &o.BodyXDR); err != nil {
			return nil, fmt.Errorf("clickhouse: scan op: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// scanOpsLight scans the opColsLight column set (no body_xdr; BodyXDR stays "").
func scanOpsLight(rows driver.Rows) ([]OpRow, error) {
	var out []OpRow
	for rows.Next() {
		var o OpRow
		if err := rows.Scan(&o.Seq, &o.CloseTime, &o.TxHash, &o.TxIndex, &o.OpIndex,
			&o.OpType, &o.SourceAccount); err != nil {
			return nil, fmt.Errorf("clickhouse: scan op (light): %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// RecentOperations returns the most-recent operations network-wide,
// newest first, keyset-paged by the composite (ledger_seq, tx_index,
// op_index) cursor. Backs the /v1/operations directory. Returns the
// LIGHT column set (opColsLight — no body_xdr); the returned
// OpRow.BodyXDR is always "". The directory is a summary listing; callers
// needing the decoded body use the per-ledger / per-tx paths.
//
// TWO-PASS, tail-window first (#444 / #332 F2, 2026-09-02). The old form
// carried NO lower bound on either arm, and the "cheap streamed reverse
// scan" the query's own comment claimed was refuted by ClickHouse's
// query_log on r1: the first page read 10.3M rows / 1.37 GiB in
// 1,788–1,875 ms, because with no ledger predicate every part in every
// partition is a candidate and the reverse read opens all of them.
//
// So the read is tried against a `recentLedgersTailWindow`-wide slice of
// ledgers FIRST — anchored at the tip for the first page, at the cursor for
// a continuation page — which prunes to the newest partition(s) (operations
// is PARTITION BY intDiv(ledger_seq, 1000000)). That bounded pass returns
// EXACTLY the same rows as the unbounded one whenever the window holds a
// full page, because a descending (ledger_seq, tx_index, op_index) page
// anchored at the window's top can only contain rows inside the window.
//
// When it comes back SHORT (fewer than `limit` rows) the window did not
// hold a page — a quiet network, a sparse historical region under a cursor,
// or a genuinely final page — so the read is REPEATED UNBOUNDED. Stopping
// short instead would truncate the listing and mint a next_cursor that
// skips real operations; falling back costs the old query only in the cases
// where the old query was the only one.
//
// A cursor page can be REFUSED: it carries a row budget, and a cursor whose
// read would exceed it returns ErrOperationsCursorTooDeep rather than a
// minutes-long scan (#484). Organic paging cannot reach one — the budget is
// ~18x the densest legitimate window — but `?cursor=` is publicly mintable,
// so the unservable case has to have an answer that is not "read 215 GiB".
func (r *ExplorerReader) RecentOperations(ctx context.Context, limit int, cur ExplorerCursor) ([]OpRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.recentOperationsPage(ctx, limit, cur, true)
	if err != nil {
		return nil, err
	}
	if len(rows) >= limit {
		return rows, nil
	}
	return r.recentOperationsPage(ctx, limit, cur, false)
}

// recentOperationsPage runs ONE RecentOperations pass, bounded to the tail
// window or not. Arg order mirrors recentOperationsQuery's clause order:
// the cursor tuple, then the window's lower bound, then the limit.
func (r *ExplorerReader) recentOperationsPage(ctx context.Context, limit int, cur ExplorerCursor, bounded bool) ([]OpRow, error) {
	q := recentOperationsQuery(cur.IsSet(), bounded)
	args := []any{}
	switch {
	case cur.IsSet():
		// Ledger binds TWICE — once to the index-usable `ledger_seq < ?`
		// arm and once to the `ledger_seq = ?` arm that confines the
		// tuple comparison to a single ledger (#484).
		args = append(args, cur.Ledger, cur.Ledger, cur.A, cur.B)
		if bounded {
			// Clamp at 0 — uint32 underflow near genesis would wrap and
			// return nothing (the RecentLedgers cursor branch's lesson).
			lower := uint32(0)
			if cur.Ledger > uint32(recentLedgersTailWindow) {
				lower = cur.Ledger - uint32(recentLedgersTailWindow)
			}
			args = append(args, lower)
		}
	case bounded:
		args = append(args, uint32(recentLedgersTailWindow))
	}
	args = append(args, limit)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		if isTooManyRows(err) {
			// The lake REFUSED the caller's cursor rather than serving
			// it (#484). Surface it as its own class so it is never
			// read as an internal fault or as retryable capacity.
			return nil, fmt.Errorf("clickhouse: recent operations from cursor %d.%d.%d: %w: %w",
				cur.Ledger, cur.A, cur.B, ErrOperationsCursorTooDeep, err)
		}
		return nil, fmt.Errorf("clickhouse: recent operations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanOpsLight(rows)
}

// recentOperationsQuery builds RecentOperations' SQL.
//
// LIMIT 1 BY the operations primary key (audit DAT-10): stellar.operations
// is ReplacingMergeTree(ingested_at); a re-ingested operation leaves an
// un-merged duplicate PART that is byte-identical to the original bar
// ingested_at (which opColsLight doesn't even select) until a background
// merge — without dedup this directory listing served the SAME operation
// twice. LIMIT 1 BY, not FINAL: FINAL would force ClickHouse to merge
// every overlapping part in the scanned range to answer this
// `ORDER BY … DESC LIMIT n` query. LIMIT 1 BY composes with the SAME ORDER
// BY the query already has, so it dedups inside the streamed read rather
// than triggering a merge.
//
// explorerScanSettings: a reverse tip read is cheap in TIME but its stream
// setup still fans out over the part layout at default threads (route-sweep
// 2026-07-29: /v1/operations was in the 8s-budget 503 class); pinning
// threads bounds the fan-out with no correctness change.
//
// The `bounded` arm carries the LOWER ledger bound that makes the read
// partition-pruned (#444 / #332 F2, 2026-09-02) — `> tip -
// recentLedgersTailWindow` on the first page, `>= cursor -
// recentLedgersTailWindow` on a cursor page. That is the same bound
// RecentLedgers takes on both of ITS arms, for the same measured reason:
// with no ledger predicate every part in every partition is a candidate,
// and r1's query_log measured the "cheap streamed reverse scan" this
// comment used to claim at 10.3M rows / 1.37 GiB / 1.8 s for one 50-row
// page. It is a PERFORMANCE bound only — RecentOperations re-runs the
// UNBOUNDED form whenever the bounded pass comes back short, so no page is
// ever truncated by it.
//
// The cursor arms carry recentOperationsCursorPredicate (index-prunable, and
// the reason that #444 bound now actually bites — see #484) plus
// recentOperationsCursorRowCeiling. Both are documented on their consts.
func recentOperationsQuery(hasCursor, bounded bool) string {
	q := `SELECT ` + opColsLight + ` FROM stellar.operations`
	switch {
	case hasCursor && bounded:
		q += ` WHERE ` + recentOperationsCursorPredicate + ` AND ledger_seq >= ?`
	case hasCursor:
		q += ` WHERE ` + recentOperationsCursorPredicate
	case bounded:
		q += ` WHERE ledger_seq > (SELECT max(ledger_seq) FROM stellar.operations) - ?`
	}
	q += ` ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC LIMIT 1 BY ledger_seq, tx_index, op_index LIMIT ?`
	q += explorerScanSettings
	if hasCursor {
		q += recentOperationsCursorRowCeiling
	}
	return q
}

// recentOperationsCursorPredicate is RecentOperations' keyset cursor
// comparison, written so ClickHouse's primary index can PRUNE on it (#484).
//
// It is EXACTLY equivalent to the tuple form it replaced —
// `(ledger_seq, tx_index, op_index) < (?, ?, ?)` — because lexicographic
// order on a product of totally-ordered sets IS
//
//	(L,T,O) < (l,t,o)  ⟺  L < l  ∨  (L = l ∧ (T,O) < (t,o))
//
// and this is literally that identity's first expansion step, with the inner
// comparison left as a tuple so it is not re-derived. All three columns are
// non-Nullable UInt32 (see the stellar.operations DDL), so there is no
// three-valued-logic case where the two forms could diverge, and
// ExplorerCursor.IsSet() guarantees l > 0 so the `ledger_seq < ?` arm cannot
// be asked about an underflowed bound.
//
// Why it matters (measured on r1's system.query_log, 2026-09-02): KeyCondition
// does NOT decompose a 3-column tuple comparison, so in the old form the ONLY
// index-usable predicate on a cursor page was #444's `ledger_seq >= lower` —
// which selects everything ABOVE the cursor, i.e. essentially the whole table.
// `EXPLAIN ESTIMATE` for `?cursor=5000000.0.0` selected 80 parts /
// 24,693,075,112 rows / 3,014,332 marks; the same page in this form selects
// 1 part / 4,157 rows / 1 mark. Executed, that is 2.86 BILLION rows / 32 GiB
// in 30 s and still unfinished (killed at the cap), versus 4,157 rows /
// 564 KiB / 5 ms. In the rewrite arm 1 constrains the leading key column to a
// half-open range and arm 2 pins it to a point, so their union is the
// index-usable `ledger_seq <= l`.
//
// The outer parentheses are load-bearing: SQL binds AND tighter than OR, so
// without them the `AND ledger_seq >= ?` window bound would attach to the
// equality arm alone and the query would return every operation below the
// cursor — a correctness bug on top of the scan it is meant to remove.
const recentOperationsCursorPredicate = `(ledger_seq < ? OR (ledger_seq = ? AND (tx_index, op_index) < (?, ?)))`

// recentOperationsCursorRowCeiling is the per-request row budget on a CURSOR
// page. `?cursor=` is publicly mintable dotted decimal on an unauthenticated
// route, so the cursor arms are the one place in this listing where a caller
// picks the size of the read; the ceiling makes a pathological pick REFUSED
// (ClickHouse code 158 TOO_MANY_ROWS, raised from the read pool in ~0.5 s)
// rather than served over minutes (#484).
//
// 200M is ~18x the densest legitimate window and ~123x below the pathological
// whole-table selection, both measured on r1 2026-09-02:
//   - the bounded cursor arm reads at most `recentLedgersTailWindow` ledgers;
//     the densest 5,000-ledger window on r1 holds 10.94M operations (mean
//     4.81M), so it cannot approach the ceiling without ~40,000 ops/ledger —
//     far beyond anything the protocol admits. Measured worst legitimate
//     cursor page (dense tip, limit 200): 524,288 rows.
//   - the UNBOUNDED cursor fallback reads [genesis, cursor]. Deep cursors are
//     cheap there (40,639 rows for cursor 5000000.0.0), but a near-tip cursor
//     would select the whole table — and that arm only runs when the tail
//     window came back short, which on a live network it does not. Refusing
//     is the fail-closed answer for exactly the shape the finding is about.
//
// read_overflow_mode is pinned to 'throw' rather than left to the default:
// a server profile that flipped it to 'break' would silently TRUNCATE the
// page and mint a next_cursor from the wrong last row, which is a data bug
// wearing a performance fix's clothes.
//
// Deliberately NOT applied to the two first-page arms. Measured on r1: the
// #444 unbounded first-page fallback (no predicate at all) announces 217.44M
// rows to the read pool and would be REFUSED at this ceiling. That arm is
// #444's correctness net for a quiet tip window, and it carries no
// caller-controlled input — so ceiling-ing it would convert "the network went
// quiet" into a hard error without closing any attacker-reachable path.
const recentOperationsCursorRowCeiling = `, max_rows_to_read = 200000000, read_overflow_mode = 'throw'`

// chTooManyRows is ClickHouse's TOO_MANY_ROWS — the code the server raises
// when a query would exceed max_rows_to_read under read_overflow_mode='throw'.
const chTooManyRows = 158

// ErrOperationsCursorTooDeep is returned when a cursor page trips
// recentOperationsCursorRowCeiling: the request was REFUSED by the lake, not
// failed by it. Callers should render it as a client error against the
// supplied `?cursor=` (the caller chose an unservable position), never as an
// internal fault or a retryable capacity signal — a retry of the identical
// cursor is refused identically.
var ErrOperationsCursorTooDeep = errors.New("clickhouse: operations cursor exceeds the per-request row budget")

// isTooManyRows reports whether err is the server refusing a query for its row
// budget (158). Mirrors isMemoryLimitExceeded's shape in sac_balance_seed.go.
func isTooManyRows(err error) bool {
	var chErr *clickhouse.Exception
	return errors.As(err, &chErr) && chErr.Code == chTooManyRows
}

// OpTypeCount is one op-type's count in the stats window.
type OpTypeCount struct {
	OpType string
	Count  int64
}

// opTypeStatsQuery is OperationTypeStats' SQL.
//
// FINAL: stellar.operations is ReplacingMergeTree(ingested_at); a re-ingested
// operation leaves an un-merged duplicate part that inflates count() until a
// merge (audit C2-12). Bounded by the ledger-window predicate.
//
// explorerScanSettings: this is a FINAL GROUP BY over a full day of the
// multi-billion-row operations table — the dominant cost behind the
// /v1/operations directory's first page and squarely in the thread-fan-out
// memory class the pin bounds.
const opTypeStatsQuery = `SELECT op_type, toInt64(count()) AS c
		FROM stellar.operations FINAL
		WHERE ledger_seq > (SELECT max(ledger_seq) FROM stellar.operations) - ?
		GROUP BY op_type
		ORDER BY c DESC` + explorerScanSettings

// OperationTypeStats returns the per-op-type operation counts over the
// most-recent `windowLedgers` ledgers (default ~24h at 5 s close
// time). Bounded to the table's tip via `ledger_seq > max - window`,
// so partition pruning keeps it to the last chunk(s). Sorted desc.
func (r *ExplorerReader) OperationTypeStats(ctx context.Context, windowLedgers uint32) ([]OpTypeCount, error) {
	if windowLedgers == 0 {
		windowLedgers = 17280 // ~24h at 5s ledger close
	}
	rows, err := r.conn.Query(ctx, opTypeStatsQuery, windowLedgers)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: operation type stats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []OpTypeCount
	for rows.Next() {
		var c OpTypeCount
		if err := rows.Scan(&c.OpType, &c.Count); err != nil {
			return nil, fmt.Errorf("clickhouse: scan op-type stat: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ThroughputBucket is one day's network throughput from stellar.ledgers.
type ThroughputBucket struct {
	Day     time.Time
	Ledgers int64
	Txs     int64
	Ops     int64
	Events  int64
	// End-of-day chain state, taken from the day's LAST ledger
	// (argMax over ledger_seq): the cumulative fee pool and total
	// XLM in stroops, and the protocol version in force. Stroop
	// totals exceed 2^53 so the API serialises them as strings
	// (ADR-0003). fee_pool is cumulative — daily fee burn is the
	// delta between consecutive complete days, computed by the
	// caller.
	FeePool         int64
	TotalCoins      int64
	ProtocolVersion uint32
	// Partial marks a bucket that does NOT cover a whole UTC day — in
	// practice only TODAY, which is still accumulating. Callers must render
	// it distinctly (dashed/faded) and EXCLUDE it from window totals, or the
	// chart shows a misleading drop at the right edge and the total
	// under-reports. Every other bucket is a complete day by construction
	// (see NetworkThroughput's day-aligned window).
	Partial bool
}

// ledgersPerDayPruningEstimate is a generous UPPER bound on ledgers closed per
// day (theoretical 5.0s cadence; the observed rate is ~14,950/day ≈ 5.78s).
// It is ONLY ever used to size a `ledger_seq >` predicate as a PARTITION-PRUNING
// HINT — never as a semantic window boundary. Overshooting is safe (it just
// scans a little wider); undershooting would silently truncate real data.
// Using it as the boundary was the 2026-07-21 chart bug: a ledger-count window
// lands mid-day, so the first toStartOfDay bucket was a partial day rendered as
// a real drop, and a "30 day" window actually spanned ~34.6 days.
const ledgersPerDayPruningEstimate = 17280

// recentLedgersTailWindow bounds the tip-page ledger query to a tail slice so
// its FINAL merge prunes to the newest partition(s) instead of scanning the
// whole table. 5000 is ~25x the max page size (200) — wide enough that a first
// page can never be truncated, narrow enough to stay inside one partition.
const recentLedgersTailWindow = 5000

// NetworkThroughput returns daily network throughput (ledger / tx / op
// / Soroban-event counts) for the most-recent `windowDays` UTC days,
// ascending by day. Exactly windowDays buckets: windowDays-1 COMPLETE days
// plus today, which is flagged Partial (still accumulating).
// windowDays defaults to 30, capped 365.
//
// The window is DAY-ALIGNED on close_time, not a ledger count. The old form
// bounded the range with `ledger_seq > max - windowDays*17280` alone, which
// lands on an arbitrary ledger MID-DAY: the earliest toStartOfDay bucket then
// covered only part of that day and rendered as a real throughput drop (at
// windowDays=90 the first bucket was ~20% of a day). It also silently
// mis-sized the window — 17280 is the theoretical 5.0s cadence but the real
// rate is ~14,950/day, so a "30 day" window actually spanned ~34.6 days and
// inflated every window total by ~15%.
//
// The ledger predicate is retained ONLY as a partition-pruning hint (sized
// generously so it can never clip a day the time predicate wants); close_time
// is the authoritative boundary.
func (r *ExplorerReader) NetworkThroughput(ctx context.Context, windowDays int) ([]ThroughputBucket, error) {
	if windowDays <= 0 || windowDays > 365 {
		windowDays = 30
	}
	// +2 days of slack so the pruning hint always covers the aligned window
	// even if the chain runs faster than the estimate.
	pruneLedgers := uint32(windowDays+2) * ledgersPerDayPruningEstimate
	// windowDays-1: the window spans windowDays buckets INCLUDING today.
	daysBack := uint32(windowDays - 1)
	const q = `SELECT toStartOfDay(close_time) AS day,
		toInt64(count())                  AS ledgers,
		toInt64(sum(tx_count))            AS txs,
		toInt64(sum(op_count))            AS ops,
		toInt64(sum(soroban_event_count)) AS events,
		-- End-of-day chain state: the value at the day's LAST ledger.
		-- argMax reads columns already materialised for this scan's
		-- granules — no extra predicate, no extra scan; the query stays
		-- the same bounded partition-pruned pass over the window.
		argMax(fee_pool, ledger_seq)         AS fee_pool,
		argMax(total_coins, ledger_seq)      AS total_coins,
		argMax(protocol_version, ledger_seq) AS protocol_version
		-- FINAL: stellar.ledgers is ReplacingMergeTree(ingested_at); without it
		-- an un-merged re-ingested ledger contributes TWO parts, so count() and
		-- every sum(*_count) double-count until a background merge (audit
		-- C2-12). FINAL is a no-op once merged. The ledger_seq predicate keeps
		-- the FINAL bounded to the recent partitions (pruning hint ONLY); the
		-- close_time predicate is the authoritative, day-aligned boundary.
		FROM stellar.ledgers FINAL
		WHERE ledger_seq > (SELECT max(ledger_seq) FROM stellar.ledgers) - ?
		  -- Day window anchored to the DATA's tip close_time, not now('UTC'):
		  -- deterministic for any two regions ingesting the same chain (the
		  -- multi-region plan's determinism contract) and equal to wall clock
		  -- within one ledger close when live.
		  AND close_time >= toStartOfDay((SELECT max(close_time) FROM stellar.ledgers)) - toIntervalDay(?)
		GROUP BY day
		ORDER BY day ASC`
	rows, err := r.conn.Query(ctx, q, pruneLedgers, daysBack)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: network throughput: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ThroughputBucket
	for rows.Next() {
		var b ThroughputBucket
		if err := rows.Scan(&b.Day, &b.Ledgers, &b.Txs, &b.Ops, &b.Events,
			&b.FeePool, &b.TotalCoins, &b.ProtocolVersion); err != nil {
			return nil, fmt.Errorf("clickhouse: scan throughput: %w", err)
		}
		out = append(out, b)
	}
	// Only the newest bucket — the one holding the tip — can be incomplete;
	// every earlier bucket is a whole UTC day because the window is
	// day-aligned to the tip's day. Data-derived (rows are day ASC), replacing
	// the old wall-clock comparison that made two regions disagree on the
	// Partial flag near a UTC day boundary.
	if len(out) > 0 {
		out[len(out)-1].Partial = true
	}
	return out, rows.Err()
}

// OperationsByLedger returns the operations in a ledger, ordered by
// (tx_index, op_index). Ledger-scoped → partition-pruned + fast (no tx_hash
// index needed).
func (r *ExplorerReader) OperationsByLedger(ctx context.Context, seq uint32, limit int) ([]OpRow, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	q := `SELECT ` + opCols + ` FROM stellar.operations FINAL
		WHERE ledger_seq = ? ORDER BY tx_index, op_index LIMIT ?`
	rows, err := r.conn.Query(ctx, q, seq, limit)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: ledger %d ops: %w", seq, err)
	}
	defer func() { _ = rows.Close() }()
	return scanOps(rows)
}

const txCols = `ledger_seq, close_time, tx_hash, tx_index, source_account,
	fee_charged, max_fee, operation_count, successful, result_code, memo_type, memo`

// ExplorerCursor is a composite keyset position for the descending explorer
// listings that can hold MANY rows per ledger (contract events, account
// txs/ops). A scalar ledger-only cursor silently drops the remainder of a
// ledger that straddles a page boundary (a busy AMM emits >limit events in one
// ledger; an MM submits >limit txs in one ledger); the full tuple makes paging
// exact. The zero value (Ledger==0) means "from the newest" (no cursor — first
// page). The A/B fields carry the 2nd/3rd ORDER BY columns and are interpreted
// per-listing: txs use (ledger, tx_index); ops use (ledger, tx_index,
// op_index); events use (ledger, op_index, event_index).
type ExplorerCursor struct {
	Ledger uint32 // ledger_seq — primary sort key (DESC)
	A      uint32 // 2nd sort col: tx_index (txs/ops) | op_index (events)
	B      uint32 // 3rd sort col: op_index (ops) | event_index (events); unused for txs
}

// IsSet reports whether the cursor points past the newest row (i.e. this is a
// continuation page, not the first page).
func (c ExplorerCursor) IsSet() bool { return c.Ledger > 0 }

// ContractEventsCursor is the keyset position for ContractEventsRecent. It
// carries the FULL row-identity tuple — (ledger_seq, tx_hash, op_index,
// event_index), the table's own ORDER BY key — because the 3-part
// (ledger, op_index, event_index) tuple is NOT unique: op_index/event_index
// are per-transaction (single-op txs dominate, so nearly every token event
// sits at (L, 0, 0)), and a strict `<` over the non-unique 3-tuple
// permanently skipped every never-served row that tied with a page's last
// row (cold audit 2026-08-03). Same shape as AccountMovements' 4-part
// cursor.
type ContractEventsCursor struct {
	Ledger     uint32 // ledger_seq — primary sort key (DESC)
	TxHash     string // 64-char hex — tie-break within a ledger (DESC, lexicographic)
	OpIndex    uint32
	EventIndex uint32
}

// IsSet reports whether the cursor points past the newest row.
func (c ContractEventsCursor) IsSet() bool { return c.Ledger > 0 }

// accountTransactionsQuery builds AccountTransactions' SQL.
//
// Two index-friendly arms UNION'd, NOT `source_account = ? OR … IN (…)`:
// an OR with a subquery defeats index use and full-scans the 23 B-row
// table. BOTH arms are now PK-shaped (2026-07-30): arm 1 (sourced) gets
// its (ledger, tx) keys from the slim stellar.ops_by_source projection
// (deploy/clickhouse/ops_by_source.sql — its tx-MV rows carry the
// sentinel op_index, its ops-MV rows real ones; the DISTINCT prefix read
// covers both, so a tx surfaces whether the account sourced the tx or any
// op in it); arm 2 (participant) via the account-prefixed
// operation_participants. The old arm 1 rode the source_account bloom
// skip-index over the whole table — granule-pruned but scan-shaped,
// measured 6.17 s vs 0.056 s for the participant arm on the SAME account
// (r1, 2026-07-30) — the whole reason these routes sat in the 8s 503
// class. DISTINCT dedups the rare tx that is BOTH sourced by the account
// AND has it as a non-source participant of one of its ops.
//
// The cursor tuple comparison is strictly older than the (ledger, tx_index)
// last served — never re-emits a served row, never skips an unserved one.
//
// Each arm carries its OWN `ORDER BY … LIMIT ?` (audit C-F1a) — without
// it only the outer query was bounded, so an account with a long history
// materialised EVERY transaction it ever touched before the outer LIMIT
// 50 threw all but a page away.
//
// INVARIANT that makes this exact, not an approximation: the union of two
// individually-top-N arms provably CONTAINS the union's top N. Any row in
// the true top N has at most N-1 distinct rows ahead of it across both
// arms, hence at most N-1 ahead of it WITHIN its own arm, so it survives
// that arm's top-N cut. The outer merge-sort + LIMIT then picks the same N
// rows it always did.
//
// The per-arm `LIMIT 1 BY ledger_seq, tx_index` is what keeps the cut
// exact rather than merely correct-order: stellar.transactions is
// ReplacingMergeTree, so an un-merged duplicate PART could otherwise
// consume slots in an arm's top-N and hand back a SHORT page while
// further rows existed (no row is lost — the keyset cursor still advances
// off the last served row — but a client that stops on a short page would
// truncate its own history). It also strengthens the outer DISTINCT,
// which only ever collapsed byte-identical duplicates.
//
// explorerScanSettings: arm 1 is a bloom-skip-index probe over the whole
// transactions table — granule-pruned but still scan-shaped, and the
// 40× thread-fan-out memory trap applies at default threads (route-sweep
// 2026-07-29: /v1/accounts/{g}/transactions was in the 8s 503 class).
func accountTransactionsQuery(hasCursor bool) string {
	cursorClause := ""
	if hasCursor {
		cursorClause = ` AND (ledger_seq, tx_index) < (?, ?)`
	}
	// TWO-PHASE (sub-second audit 2026-08-13): resolve the KEYSET in the
	// union, then hydrate the wide columns ONCE over the surviving ≤limit
	// keys. Selecting txCols inside both arms made each arm carry
	// memo/result_code/source_account/… through its own scan and sort of
	// stellar.transactions, and the outer DISTINCT then materialised both
	// wide sets. Measured on r1 (GATL account, 50 rows): wide-in-arms
	// 1.479s vs two-phase 0.219s — 6.7x, identical rows. The endpoint was
	// 1.5-2.0s and is now dominated by neither arm.
	//
	// The narrow arms keep their own LIMIT + LIMIT 1 BY (the per-arm
	// dedupe of a tx whose account appears in several of its operations),
	// and the hydration pass repeats LIMIT 1 BY as belt-and-braces
	// against duplicate lake parts.
	//
	// The KEYSET MERGE carries `LIMIT 1 BY` TOO, and it has to run BEFORE
	// the merge's own LIMIT (#290). The two arms legitimately overlap:
	// a tx the account SOURCED can also carry it as a NON-source
	// participant of one of that tx's operations (an op with its own
	// source_account naming the tx's source — batch/sponsored txs), so
	// both arms emit the SAME (ledger_seq, tx_index) key. With the dedupe
	// only in the hydration pass, every such key ate TWO of the merge's
	// LIMIT slots and the page came back SHORT — and the handler emits
	// next_cursor ONLY on a full page (internal/api/v1/explorer/
	// accounts.go, the documented `absent on the last page` contract), so
	// a client's history walk stopped there with older txs unreached:
	// SILENT TRUNCATION, not a cosmetic short page. Deduping at the merge
	// makes the keyset exactly min(limit, distinct keys older than the
	// cursor) — which is what "a short page means end of history" needs
	// in order to be true.
	//
	// The other way a page could be short — a key resolving to no
	// stellar.transactions row — cannot happen: Sink.Flush sends
	// transactions BEFORE operations/participants and stellar.ledgers
	// last (its ORDERING note), and ops_by_source is an MV fed by those
	// same inserts, so every key either arm can resolve already has its
	// hydration row durable.
	//
	// The arms page the ACCOUNT-KEYED tables directly (same rewrite as
	// accountOperationsQuery, 2026-08-28): resolving `(ledger_seq,
	// tx_index) IN (SELECT … FROM ops_by_source WHERE source_account = ?)`
	// over stellar.transactions prunes to one granule per key in the set
	// BEFORE the LIMIT, so a hot account's page cost its whole history.
	// `LIMIT 1 BY ledger_seq, tx_index` on the key table replaces the old
	// DISTINCT: it collapses the several ops_by_source rows one tx
	// contributes (tx-sentinel + per-op) to one key before the arm's LIMIT.
	return `SELECT ` + txCols + ` FROM stellar.transactions
		WHERE (ledger_seq, tx_index) IN (
		  SELECT ledger_seq, tx_index FROM (
		    (SELECT ledger_seq, tx_index FROM stellar.ops_by_source
		       WHERE source_account = ?` + cursorClause + `
		       ORDER BY ledger_seq DESC, tx_index DESC LIMIT 1 BY ledger_seq, tx_index LIMIT ?)
		    UNION ALL
		    (SELECT ledger_seq, tx_index FROM stellar.operation_participants
		       WHERE account = ?` + cursorClause + `
		       ORDER BY ledger_seq DESC, tx_index DESC LIMIT 1 BY ledger_seq, tx_index LIMIT ?)
		  ) ORDER BY ledger_seq DESC, tx_index DESC LIMIT 1 BY ledger_seq, tx_index LIMIT ?)
		ORDER BY ledger_seq DESC, tx_index DESC LIMIT 1 BY ledger_seq, tx_index LIMIT ?` + explorerScanSettings
}

// AccountTransactions returns transactions INVOLVING an account — both those
// it sourced (source/fee-payer) and those where it's a non-source participant
// in any operation (payment destination, trustor, merge target, …) — newest
// first, keyset-paged by the composite (ledger_seq, tx_index) cursor (ADR-0038
// Phase B). Sourced via the source_account skip-index; incoming via an
// account-prefixed lookup of stellar.operation_participants.
//
// Incoming coverage tracks the participant-index capture + backfill: live
// ingest fills operation_participants going forward, so a tx whose only link
// to the account predates participant capture surfaces once the historical
// re-derive lands.
func (r *ExplorerReader) AccountTransactions(ctx context.Context, account string, limit int, cur ExplorerCursor) ([]TxSummary, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if !r.opsBySourceAvailable(ctx) {
		return nil, errOpsBySourceMissing
	}
	var cursorArgs []any
	if cur.IsSet() {
		cursorArgs = []any{cur.Ledger, cur.A}
	}
	q := accountTransactionsQuery(cur.IsSet())
	args := []any{account}
	args = append(args, cursorArgs...)
	args = append(args, limit)
	args = append(args, account)
	args = append(args, cursorArgs...)
	args = append(args, limit)
	// Two placeholders trail the arms: the keyset LIMIT and the
	// hydration LIMIT (see accountTransactionsQuery).
	args = append(args, limit)
	args = append(args, limit)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: account %s txs: %w", account, err)
	}
	defer func() { _ = rows.Close() }()
	return scanTxSummaries(rows)
}

// accountOperationsQuery builds AccountOperations' SQL.
//
// UNION of two index-friendly arms (see accountTransactionsQuery for why an
// `OR … IN (…)` is wrong). Arm 1 (sourced) uses the source_account
// index; arm 2 (participant) matches operations on its PRIMARY KEY
// (ledger_seq, tx_index, op_index) via op-keys from the account-prefixed
// operation_participants. No DISTINCT needed for cross-arm overlap: an op
// is sourced XOR has the account as a NON-source participant
// (participants exclude the op's own source), so the arms never overlap.
//
// Each arm DOES carry its own `LIMIT 1 BY ledger_seq, tx_index, op_index`
// (audit DAT-10): stellar.operations is ReplacingMergeTree(ingested_at),
// so a re-ingested op leaves an un-merged duplicate PART — identical to
// the original bar ingested_at — until a background merge; unlike
// AccountTransactions' outer DISTINCT (which AccountTransactions'
// narrower, blob-free txCols makes cheap), opCols here carries body_xdr
// (KB-scale), so a DISTINCT comparing full wide rows is the wrong tool —
// LIMIT 1 BY only tracks the 3-column primary key and dedups per arm
// before the UNION ALL, cheaply.
//
// Each arm carries its OWN `ORDER BY … LIMIT ?` (audit C-F1a). This
// matters more here than on AccountTransactions: opCols carries body_xdr
// (KB-scale), and with only the OUTER query bounded a high-activity
// account materialised every op it ever sourced — blobs and all — before
// the outer LIMIT 50 discarded ~all of it (live-measured 5–6 s on an idle
// box). Bounded arms let each side read in reverse primary-key order and
// stop after N rows.
//
// INVARIANT: the union of two individually-top-N arms provably CONTAINS
// the union's top N — any row in the true top N has at most N-1 rows
// ahead of it across both arms, hence at most N-1 within its own arm, so
// it survives that arm's cut. The outer merge-sort + LIMIT then selects
// exactly the rows it selected before. The pre-existing per-arm
// `LIMIT 1 BY` (DAT-10) runs BEFORE the per-arm LIMIT, so each arm's N
// slots hold N DISTINCT primary keys — a duplicate un-merged part can't
// eat a slot and shorten the page.
//
// explorerScanSettings: same rationale as accountTransactionsQuery, with
// the wide body_xdr column raising the per-stream buffer stakes further.
//
// hasBound adds ` AND ledger_seq <= ?` to EACH arm's resolve over
// stellar.operations (#31): the arm reads `ORDER BY pk DESC LIMIT n`,
// which streams granules backwards FROM THE TIP until it accumulates the
// account's rows — for a long-idle account that walk covers every granule
// between the tip and its last activity (~4s live for a 46d-idle account,
// 2026-08-24; the scan that ate the 8s budget behind PR #155's symptom).
// The bound is the account's activity watermark
// (accountActivityWatermark, stellar.account_activity), so partition +
// primary-key pruning starts the reverse read AT the account's real last
// activity. EXACT, not an approximation: every key either arm can emit
// comes from a row whose insert also raised the watermark to >= its own
// ledger_seq (the watermark MVs fire on the same tables that feed
// ops_by_source and operation_participants — tier1_schema.sql documents
// the data-hiding invariant), so `ledger_seq <= watermark` can never
// exclude a returnable row. Callers must ONLY pass a bound derived from
// that watermark — an under-estimate silently HIDES history.
func accountOperationsQuery(hasCursor, hasBound bool) string {
	cursorClause := ""
	if hasCursor {
		cursorClause = ` AND (ledger_seq, tx_index, op_index) < (?, ?, ?)`
	}
	boundClause := ""
	if hasBound {
		boundClause = ` AND ledger_seq <= ?`
	}
	// TWO-PHASE, same as accountTransactionsQuery (sub-second audit
	// 2026-08-13) and with more at stake here: opCols carries body_xdr,
	// which opColsLight's own doc measures at ~600ms over this 24B-row /
	// 2TiB table. Carrying it through BOTH arms' scans and sorts paid
	// that twice; the keyset union below is narrow and the wide read
	// happens once over ≤limit keys.
	//
	// KEYSET FROM THE ACCOUNT-KEYED TABLES, NOT FROM stellar.operations
	// (r1 503s 2026-08-28): each arm used to re-resolve its keys over
	// stellar.operations — `WHERE pk IN (SELECT pk FROM ops_by_source
	// WHERE source_account = ?) ORDER BY pk DESC LIMIT n`. ClickHouse's
	// set-based index analysis DOES prune that to exact granules, but it
	// prunes to ONE GRANULE PER KEY IN THE SET, before the LIMIT: a hot
	// account's page cost its whole history in granules (11,925 sourced +
	// 26,064 participant keys × 8192-row granules ≈ 164–238 M rows /
	// ~8 s per page, live query_log) — the deadline-exceeded class, on a
	// page of 50. Both ops_by_source and operation_participants are
	// ORDER BY (account, ledger_seq, tx_index, op_index), so the cursor,
	// bound and LIMIT apply DIRECTLY on a primary-key-prefix range read
	// of the account's own rows, and stellar.operations is touched ONCE,
	// by the hydration pass, over ≤ 3×limit keys — the page is bounded by
	// the page size, not by the account's activity. Exactness: the
	// per-arm `LIMIT 1 BY` collapses an un-merged RMT duplicate in the
	// key table (both are ReplacingMergeTree) before the arm's LIMIT, and
	// the top-N-union invariant above holds unchanged since each arm's
	// keys are exactly the rows the old arm resolved.
	//
	// The merge below needs NO cross-arm dedupe (unlike
	// accountTransactionsQuery's, #290): these arms are disjoint at op
	// granularity — an op is sourced by the account XOR carries it as a
	// non-source participant, because operationParticipantRows excludes
	// the op's own resolved source (pinned by
	// TestOperationParticipantRows_SkipsSource). The full-history walk
	// asserts every non-final page of this listing is FULL
	// (test/integration/account_operations_pk_pruning_test.go), which is
	// what would catch a violation of that invariant.
	return `SELECT ` + opCols + ` FROM stellar.operations
		WHERE (ledger_seq, tx_index, op_index) IN (
		  SELECT ledger_seq, tx_index, op_index FROM (
		    (SELECT ledger_seq, tx_index, op_index FROM stellar.ops_by_source
		       WHERE source_account = ? AND op_index != 4294967295` + boundClause + cursorClause + `
		       ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC
		       LIMIT 1 BY ledger_seq, tx_index, op_index LIMIT ?)
		    UNION ALL
		    (SELECT ledger_seq, tx_index, op_index FROM stellar.operation_participants
		       WHERE account = ?` + boundClause + cursorClause + `
		       ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC
		       LIMIT 1 BY ledger_seq, tx_index, op_index LIMIT ?)
		  ) ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC LIMIT ?)
		ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC
		LIMIT 1 BY ledger_seq, tx_index, op_index LIMIT ?` + explorerScanSettings
}

// AccountOperations returns operations INVOLVING an account — both those it
// sourced (effective op source) and those where it's a non-source participant
// — newest first, keyset-paged by the composite (ledger_seq, tx_index,
// op_index) cursor (ADR-0038 Phase B). Sourced via the source_account
// skip-index on stellar.operations; incoming via an account-prefixed lookup of
// stellar.operation_participants. Incoming coverage tracks the participant-
// index capture + backfill (see AccountTransactions). When the account has
// an activity watermark (stellar.account_activity, #31) both arms' resolves
// are additionally bounded by `ledger_seq <= watermark`.
func (r *ExplorerReader) AccountOperations(ctx context.Context, account string, limit int, cur ExplorerCursor) ([]OpRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if !r.opsBySourceAvailable(ctx) {
		return nil, errOpsBySourceMissing
	}
	var cursorArgs []any
	if cur.IsSet() {
		cursorArgs = []any{cur.Ledger, cur.A, cur.B}
	}
	// The activity watermark bounds each arm's resolve (`ledger_seq <= ?`)
	// so a long-idle account's page stops at its real last activity —
	// exact, never row-hiding (see accountOperationsQuery). No watermark
	// (table absent, backfill pending, or the account has no row) → the
	// unbounded resolve, exactly as before the watermark existed.
	bound, hasBound := r.accountActivityWatermark(ctx, account)
	q := accountOperationsQuery(cur.IsSet(), hasBound)
	args := []any{account}
	if hasBound {
		args = append(args, bound)
	}
	args = append(args, cursorArgs...)
	args = append(args, limit)
	args = append(args, account)
	if hasBound {
		args = append(args, bound)
	}
	args = append(args, cursorArgs...)
	args = append(args, limit)
	args = append(args, limit)
	// Two trailing placeholders: the keyset LIMIT and the hydration
	// LIMIT (see accountOperationsQuery).
	args = append(args, limit)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: account %s ops: %w", account, err)
	}
	defer func() { _ = rows.Close() }()
	return scanOps(rows)
}

// accountOpTypeCountsQuery is AccountOperationTypeCounts' SQL.
//
// The SAME two index-friendly arms as accountOperationsQuery — sourced
// via the source_account skip-index, participant via an account-prefixed
// operation_participants lookup resolved on the operations PRIMARY KEY —
// UNION'd, NOT `source_account = ? OR … IN (…)`: an OR with a subquery
// defeats the source_account skip-index and full-scans the multi-billion-
// row table (see accountTransactionsQuery's doc comment). The arms never
// overlap (an op is sourced XOR has the account as a NON-source
// participant — participants exclude the op's own source), so the outer
// sum() over both arms counts every involving op exactly once.
//
// uniqExact over the 3-column primary key, not count(): stellar.operations
// is ReplacingMergeTree(ingested_at), so a re-ingested op leaves an
// un-merged duplicate PART until a background merge — a plain count()
// would inflate per-type totals (the aggregate twin of the per-row
// LIMIT 1 BY dedup accountOperationsQuery carries; FINAL is the wrong
// tool here for the same O(table)-merge reason recentOperationsQuery
// documents). uniqExact's state is bounded by the ACCOUNT's own op
// count, and the explorerScanSettings external-spill pair converts a
// whale account's aggregation state into a disk-backed success instead
// of an OOM (the same posture as the contracts-directory GROUP BY).
//
// Arm 1's keys come from the slim stellar.ops_by_source projection
// (2026-07-30, same rewrite as accountOperationsQuery — the bloom probe
// over the whole operations table measured 6.17s; the PK-prefixed slim
// read is ms). The sentinel op_index rows (tx-sourced) are excluded:
// this aggregate counts OPERATIONS the account sourced.
const accountOpTypeCountsQuery = `SELECT op_type, toInt64(sum(c)) AS n FROM (
		(SELECT op_type, uniqExact((ledger_seq, tx_index, op_index)) AS c
		   FROM stellar.operations
		  WHERE (ledger_seq, tx_index, op_index) IN (
		        SELECT ledger_seq, tx_index, op_index FROM stellar.ops_by_source
		        WHERE source_account = ? AND op_index != 4294967295)
		  GROUP BY op_type)
		UNION ALL
		(SELECT op_type, uniqExact((ledger_seq, tx_index, op_index)) AS c
		   FROM stellar.operations
		  WHERE (ledger_seq, tx_index, op_index) IN (
		        SELECT ledger_seq, tx_index, op_index FROM stellar.operation_participants WHERE account = ?)
		  GROUP BY op_type)
	) GROUP BY op_type ORDER BY n DESC` + explorerScanSettings

// AccountOperationTypeCounts returns the all-time per-op-type counts of
// operations INVOLVING an account — both those it sourced and those
// where it's a non-source participant — sorted by count desc. The
// aggregate variant of AccountOperations (same two arms, same coverage
// caveat: participant-side counts track the participant-index capture +
// backfill). Whole-history scan-shaped — callers run it under a
// detached stale-while-revalidate budget, never a request deadline.
func (r *ExplorerReader) AccountOperationTypeCounts(ctx context.Context, account string) ([]OpTypeCount, error) {
	if !r.opsBySourceAvailable(ctx) {
		return nil, errOpsBySourceMissing
	}
	rows, err := r.conn.Query(ctx, accountOpTypeCountsQuery, account, account)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: account %s op-type counts: %w", account, err)
	}
	defer func() { _ = rows.Close() }()
	var out []OpTypeCount
	for rows.Next() {
		var c OpTypeCount
		if err := rows.Scan(&c.OpType, &c.Count); err != nil {
			return nil, fmt.Errorf("clickhouse: scan account op-type count: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// TransactionByHash looks up a single transaction by its hex hash.
//
// Fast path (perf-todo §4): when stellar.tx_hash_index is available — it
// exists AND holds rows, see [ExplorerReader.txHashIndexAvailable] — the
// hash resolves to its ledger via the hash-ORDERED lookup table
// (primary-key binary search, µs) and the summary row is then read
// ledger-scoped (partition-pruned, sub-100ms). A per-hash MISS against
// that non-empty index is AUTHORITATIVE absence (2026-07-30 account-filter
// class audit; see the case comment below). Deployments without the index
// table — and deployments where the index EXISTS but is EMPTY (the
// MV-drop / truncation pathology; the availability probe treats that as
// index-unavailable) — take the pre-index behaviour: the tx_hash
// bloom-skip-index scan over stellar.transactions (~5s at 10.2B rows; the
// bloom prunes granules but cannot seek). found=false only after the scan
// also comes up empty.
func (r *ExplorerReader) TransactionByHash(ctx context.Context, hash string) (TxSummary, bool, error) {
	if r.txHashIndexAvailable(ctx) {
		tx, found, indexHit, err := r.txByHashIndexed(ctx, hash)
		switch {
		case err == nil && found:
			return tx, true, nil
		case err == nil && !indexHit:
			// The INDEX had no row — authoritative absence (2026-07-30
			// account-filter class audit): tx_hash_index covers
			// genesis→tip (20.78B rows from ledger 3, verified on r1 —
			// the old "pre-backfill history" caveat is stale; that
			// backfill completed). Falling through to the scan here
			// turned every unknown/garbage hash into an unauthenticated
			// bloom probe over the full 10.5B-row transactions table —
			// the same non-sort-key filter disease as the account-
			// history arms, plus a free DoS lever.
			return TxSummary{}, false, nil
		}
		// Index-path error, or an index/base INCONSISTENCY (index row
		// present, base row missing — a mis-seeded index): the scan is
		// still the availability-preserving answer for those.
	}
	return r.txByHashScan(ctx, hash)
}

// txHashIndexAvailable reports whether stellar.tx_hash_index is USABLE on
// this ClickHouse: the table exists AND holds at least one row.
//
// Row count matters here — unlike the other schema probes — because a
// per-hash index MISS is treated as an AUTHORITATIVE not-found (the DoS
// protection in TransactionByHash). Against an existing-but-EMPTY index
// (the MV-drop / TRUNCATE pathology: transactions keeps flowing, the index
// silently stops) that authority would turn EVERY hash lookup into a 404
// for real transactions. "An empty table is not a definitive answer" —
// the same convention as DailyActivityAvailable (protocol_reader.go):
// emptiness is treated as index-unavailable (scan path) and is NOT cached,
// so a later probe picks the index back up once it is repopulated; only
// table-absent (schema verdict) and non-empty (rows seen) settle. The
// complementary guard is the hourly tx_hash_index parity check — that
// catches PARTIAL index/base divergence; this probe catches total loss.
// A miss against a NON-EMPTY index remains authoritative.
func (r *ExplorerReader) txHashIndexAvailable(ctx context.Context) bool {
	return r.probeSchema(ctx, &r.txIndexProbe,
		`SELECT ledger_seq FROM stellar.tx_hash_index LIMIT 1`, true)
}

// contractLedgersIndexAvailable reports whether
// stellar.contract_active_ledgers is USABLE: exists AND non-empty (see the
// probe field doc for why emptiness must not settle). NOTE (audit
// W1-chrollup-3): a true verdict means the table has at least one row — it
// does NOT prove per-contract backfill coverage, so callers must treat an
// empty PER-CONTRACT walk as "unknown, fall back", never as authoritative
// "no rows for this contract" (see ContractEventsRecent).
func (r *ExplorerReader) contractLedgersIndexAvailable(ctx context.Context) bool {
	return r.probeSchema(ctx, &r.contractLedgersProbe,
		`SELECT ledger_seq FROM stellar.contract_active_ledgers LIMIT 1`, true)
}

// instanceChangesIndexAvailable reports whether
// stellar.contract_instance_changes is USABLE: exists AND non-empty (see
// the probe field doc for why emptiness must not settle).
func (r *ExplorerReader) instanceChangesIndexAvailable(ctx context.Context) bool {
	return r.probeSchema(ctx, &r.instanceChangesProbe,
		`SELECT ledger_seq FROM stellar.contract_instance_changes LIMIT 1`, true)
}

// censusAvailable reports whether stellar.contracts_census_daily is
// USABLE: exists AND non-empty (see the probe field doc).
func (r *ExplorerReader) censusAvailable(ctx context.Context) bool {
	return r.probeSchema(ctx, &r.censusProbe,
		`SELECT day FROM stellar.contracts_census_daily LIMIT 1`, true)
}

// ContractActivitySummary is the per-contract liveness card (page
// insight program unit 1): lifetime bounds + a daily activity series,
// all key-pruned reads off contract_active_ledgers (µs–ms class).
type ContractActivitySummary struct {
	FirstSeen          time.Time
	LastSeen           time.Time
	ActiveLedgersTotal uint64
	Daily              []ContractActivityDay
}

// ContractActivityDay is one day of the activity series. ActiveLedgers
// counts DISTINCT ledgers the contract emitted ≥1 event in — uniqExact
// on ledger_seq so un-merged ReplacingMergeTree duplicate parts (an
// overlapping backfill window re-inserts the same (contract, ledger)
// keys) do not inflate the count, matching the sibling reader
// contractActiveLedgers' SELECT DISTINCT on this same table (audit CHQ-2).
type ContractActivityDay struct {
	Date          time.Time
	ActiveLedgers uint64
}

// ContractActivitySummaryFor reads the contract's activity card.
// ok=false when the active-ledgers index isn't usable (probe) — callers
// omit the card rather than fabricating one.
func (r *ExplorerReader) ContractActivitySummaryFor(ctx context.Context, contractID string, days int) (ContractActivitySummary, bool, error) {
	if !r.contractLedgersIndexAvailable(ctx) {
		return ContractActivitySummary{}, false, nil
	}
	if days <= 0 || days > 365 {
		days = 30
	}
	var s ContractActivitySummary
	if err := r.conn.QueryRow(ctx, `
		SELECT min(close_time), max(close_time), toUInt64(uniqExact(ledger_seq))
		FROM stellar.contract_active_ledgers WHERE contract_id = ?`,
		contractID).Scan(&s.FirstSeen, &s.LastSeen, &s.ActiveLedgersTotal); err != nil {
		return ContractActivitySummary{}, false, fmt.Errorf("clickhouse: contract activity bounds: %w", err)
	}
	if s.ActiveLedgersTotal == 0 {
		return s, true, nil
	}
	rows, err := r.conn.Query(ctx, `
		SELECT toDate(close_time) AS d, toUInt64(uniqExact(ledger_seq))
		FROM stellar.contract_active_ledgers
		WHERE contract_id = ? AND close_time >= now() - INTERVAL ? DAY
		GROUP BY d ORDER BY d`, contractID, days)
	if err != nil {
		return ContractActivitySummary{}, false, fmt.Errorf("clickhouse: contract activity series: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var d ContractActivityDay
		if err := rows.Scan(&d.Date, &d.ActiveLedgers); err != nil {
			return ContractActivitySummary{}, false, fmt.Errorf("clickhouse: scan activity day: %w", err)
		}
		s.Daily = append(s.Daily, d)
	}
	return s, true, rows.Err()
}

// contractActiveLedgers returns the contract's most recent active ledgers
// (descending), at most n, optionally bounded to <= before (cursor pages —
// inclusive: the boundary ledger can still hold events older than the
// cursor tuple). Primary-key reverse walk on the narrow index: µs-class.
// DISTINCT collapses un-merged RMT duplicate rows.
func (r *ExplorerReader) contractActiveLedgers(ctx context.Context, contractID string, before uint32, n int) ([]uint32, error) {
	q := `SELECT DISTINCT ledger_seq FROM stellar.contract_active_ledgers WHERE contract_id = ?`
	args := []any{contractID}
	if before > 0 {
		q += ` AND ledger_seq <= ?`
		args = append(args, before)
	}
	q += ` ORDER BY ledger_seq DESC LIMIT ?`
	args = append(args, n)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: contract %s active ledgers: %w", contractID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []uint32
	for rows.Next() {
		var l uint32
		if err := rows.Scan(&l); err != nil {
			return nil, fmt.Errorf("clickhouse: scan active ledger: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// errOpsBySourceMissing — the sourced-history projection has not been
// provisioned on this ClickHouse. Fail-loud by design (same contract as
// ttl_live_until): the pre-projection bloom-scan arm is DELETED, and a
// silent fallback or empty arm would either restore the 6-second read or
// hide the account's own history.
var errOpsBySourceMissing = errors.New(
	"clickhouse: stellar.ops_by_source does not exist — apply deploy/clickhouse/ops_by_source.sql " +
		"(table + both MVs, then its Step-2 windowed backfills) before serving account history; " +
		"there is no scan fallback")

// opsBySourceAvailable reports whether stellar.ops_by_source exists.
func (r *ExplorerReader) opsBySourceAvailable(ctx context.Context) bool {
	return r.probeSchema(ctx, &r.opsBySourceProbe,
		`SELECT ledger_seq FROM stellar.ops_by_source LIMIT 1`, false)
}

// accountActivityAvailable reports whether stellar.account_activity (the
// per-account activity watermark, deploy/clickhouse/account_activity.sql)
// is usable. requireRows: an existing-but-EMPTY watermark (table created,
// MVs live, backfill not yet run, no traffic since) can bound nothing
// anyway — treat it as unavailable now, re-probe after the retry window,
// like the sibling requireRows probes.
func (r *ExplorerReader) accountActivityAvailable(ctx context.Context) bool {
	return r.probeSchema(ctx, &r.accountActivityProbe,
		`SELECT last_ledger FROM stellar.account_activity LIMIT 1`, true)
}

// accountActivityWatermark returns the account's last-active ledger from
// stellar.account_activity — the exact upper bound AccountOperations'
// arms use to stop their reverse primary-key resolves at the account's
// real last activity instead of walking granules back from the tip (#31).
//
// max(last_ledger), never a bare row read or FINAL: the table is
// ReplacingMergeTree(last_ledger) fed by three MVs, so an account can
// hold several un-merged rows and only the MAX is a safe bound — a lower
// row would HIDE history (the invariant tier1_schema.sql documents).
// ok=false — table absent/empty (probe), the account has no row (max()
// over zero rows is 0), or any read error — degrades to the unbounded
// scan: a missing watermark may only ever cost performance, never rows.
func (r *ExplorerReader) accountActivityWatermark(ctx context.Context, account string) (uint32, bool) {
	if !r.accountActivityAvailable(ctx) {
		return 0, false
	}
	var last uint32
	err := r.conn.QueryRow(ctx,
		`SELECT max(last_ledger) FROM stellar.account_activity WHERE account_id = ?`,
		account).Scan(&last)
	if err != nil || last == 0 {
		return 0, false
	}
	return last, true
}

// ledgerEntriesVersioned reports whether stellar.ledger_entries_current has
// a `version` column (the post-D3 RMT version). false → pre-D3 schema;
// callers use ledger_seq as the version key. See schemaProbe.
func (r *ExplorerReader) ledgerEntriesVersioned(ctx context.Context) bool {
	return r.probeSchema(ctx, &r.lecVersionProbe,
		`SELECT version FROM stellar.ledger_entries_current LIMIT 1`, false)
}

// probeSchema answers "does this schema object exist" and CACHES ONLY A
// DEFINITIVE ANSWER (C1-048, audit-2026-07-23).
//
// These probes used to be a plain sync.Once, so the FIRST call's outcome
// was final for the process lifetime. A transient ClickHouse error at that
// instant — a restart mid-deploy, a connection reset, a request-context
// deadline — latched the probe to false and silently degraded every
// subsequent read for the life of the process, with no error, no metric,
// and no self-heal short of a restart. That is the worst shape a fallback
// can have: correct-but-slower forever, triggered by a blip.
//
// "Definitive" means the server gave a SCHEMA VERDICT:
//
//   - no error                          → the object exists (cache true);
//   - an exception in [schemaAbsentCodes] → the object does not exist
//     (cache false).
//
// Everything else — transport errors, context deadline/cancel, and any
// OTHER ClickHouse exception (resource limits, timeouts, overload) — means
// we never got an answer about the schema. Nothing is cached; the caller
// degrades for this call only and a later call re-probes, rate-limited by
// [schemaProbeRetryAfter] so an outage doesn't turn every read into an
// extra query.
//
// requireRows tightens "exists" to "exists AND is non-empty" — for probes
// whose caller derives AUTHORITY from the object (txHashIndexAvailable:
// an index miss is a definitive 404, which an empty index must never
// grant). An existing-but-EMPTY object is treated like a non-answer:
// unavailable NOW, not cached (it may be backfilled/repopulated later),
// re-probed after the retry window — the same "empty table is not a
// definitive answer" convention as DailyActivityAvailable. Only a
// schema-absent verdict or an observed row settles a requireRows probe.
//
// The query runs OUTSIDE the mutex. sync.Mutex is not context-aware, so
// holding it across a network round-trip would queue every concurrent
// reader behind one slow probe and serialise the whole explorer read path
// (C1-048, second review). The cost is that concurrent first-callers may
// each issue a probe until one settles — bounded, and each is a LIMIT 1.
func (r *ExplorerReader) probeSchema(ctx context.Context, p *schemaProbe, query string, requireRows bool) bool {
	p.mu.Lock()
	if p.settled {
		present := p.present
		p.mu.Unlock()
		return present
	}
	if !p.retryAt.IsZero() && time.Now().Before(p.retryAt) {
		// A recent probe got no answer; don't pile on.
		p.mu.Unlock()
		return false
	}
	p.mu.Unlock()

	rows, err := r.conn.Query(ctx, query)
	empty := false
	if err == nil {
		if requireRows {
			empty = !rows.Next()
			if rerr := rows.Err(); rerr != nil {
				// Row iteration died — no verdict on emptiness either.
				err, empty = rerr, false
			}
		}
		_ = rows.Close()
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.settled {
		// A concurrent probe already got the authoritative answer.
		return p.present
	}
	switch {
	case err == nil && !empty:
		p.settled, p.present = true, true
		return true
	case isSchemaAbsent(err):
		p.settled, p.present = true, false
		return false
	default:
		retryAfter := p.retryAfter
		if retryAfter == 0 {
			retryAfter = schemaProbeRetryAfter
		}
		if retryAfter > 0 {
			p.retryAt = time.Now().Add(retryAfter)
		}
		return false
	}
}

// txByHashIndexed is the two-step fast path: hash → ledger_seq via the
// ordered index, then the ledger-scoped summary read. found=false on an
// index miss (the caller falls back to the scan).
// indexHit distinguishes the two "not found" kinds: false = the INDEX had
// no row for the hash (authoritative absence — the index covers
// genesis→tip); true+!found = the index pointed at a ledger whose base row
// is missing (an index/base inconsistency the caller may still resolve via
// the scan).
func (r *ExplorerReader) txByHashIndexed(ctx context.Context, hash string) (tx TxSummary, found, indexHit bool, err error) {
	rows, err := r.conn.Query(ctx,
		`SELECT ledger_seq FROM stellar.tx_hash_index WHERE tx_hash = ? LIMIT 1`, hash)
	if err != nil {
		return TxSummary{}, false, false, fmt.Errorf("clickhouse: tx index %s: %w", hash, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return TxSummary{}, false, false, rows.Err()
	}
	var seq uint32
	if err := rows.Scan(&seq); err != nil {
		return TxSummary{}, false, true, fmt.Errorf("clickhouse: scan tx index: %w", err)
	}
	tx, found, err = r.txByLedgerAndHash(ctx, seq, hash)
	return tx, found, true, err
}

// txByLedgerAndHash reads the authoritative stellar.transactions row for a
// KNOWN (ledger_seq, tx_hash) pair. Both txByHashIndexed and txByHashScan's
// second step call this once they know which ledger the hash lives in.
//
// FINAL, not `ORDER BY ingested_at DESC LIMIT 1` (audit DAT-10, "may return
// the WRONG row"): ingested_at is `DateTime` — ONE-SECOND resolution — so a
// batch that re-ingests many rows within the same wall-clock second (a
// decode-bug-fix backfill re-deriving a ledger range, not just an idempotent
// retry) leaves two ReplacingMergeTree parts for the same (ledger_seq,
// tx_index) key that TIE on the version column `ORDER BY ingested_at DESC`
// sorts by. A plain SELECT has no way to break that tie correctly — it only
// sees the tied ingested_at values, not true insertion order — so it could
// silently keep serving the STALE pre-fix row. FINAL resolves the tie
// correctly: ClickHouse's ReplacingMergeTree merge keeps the row that was
// PHYSICALLY inserted last among version ties, using real insertion order
// that isn't exposed to a bare SELECT.
//
// This stays cheap: `ledger_seq = ?` is a partition-pruning + primary-key
// prefix predicate (PARTITION BY intDiv(ledger_seq, 1000000), ORDER BY
// (ledger_seq, tx_index)), so FINAL only ever merges the handful of parts
// that touch this one ledger — the same "single-row point read stays cheap
// under FINAL" reasoning as LedgerBySeq / CloseTimeForLedger above. tx_hash
// is globally unique, so at most one row can match.
func (r *ExplorerReader) txByLedgerAndHash(ctx context.Context, seq uint32, hash string) (TxSummary, bool, error) {
	q := `SELECT ` + txCols + ` FROM stellar.transactions FINAL
		WHERE ledger_seq = ? AND tx_hash = ?`
	rows, err := r.conn.Query(ctx, q, seq, hash)
	if err != nil {
		return TxSummary{}, false, fmt.Errorf("clickhouse: tx %s in ledger %d: %w", hash, seq, err)
	}
	defer func() { _ = rows.Close() }()
	out, err := scanTxSummaries(rows)
	if err != nil || len(out) == 0 {
		return TxSummary{}, false, err
	}
	return out[0], true, nil
}

// txByHashScan is the pre-index lookup, in two steps:
//
//  1. Locate WHICH ledger the hash lives in via the tx_hash bloom
//     skip-index (the table is ORDER BY (ledger_seq, tx_index), so without
//     the index this would full-scan). NOT FINAL — FINAL would defeat the
//     skip-index over the WHOLE table (this is the un-scoped fallback path;
//     unlike step 2 below, there is no known ledger yet to bound it to).
//     Only ledger_seq is read here, and that is safe even from an un-merged
//     duplicate part: a transaction executes in exactly one historical
//     ledger, and that fact never changes on re-ingest — every candidate row
//     for this tx_hash agrees on ledger_seq.
//
//     The `ORDER BY ingested_at DESC` is therefore a NO-OP on real data (one
//     ledger, so nothing to order), and is kept deliberately for the case the
//     lake can hold but the network cannot: the same hash recorded against two
//     different ledger_seq values (a mis-seeded or cross-network backfill).
//     There, "most recently ingested wins" is the long-documented behaviour
//     this reader has always had, and integration coverage pins it. Dropping
//     it made the choice arbitrary — whichever granule the skip-index happened
//     to return first. Note this ordering is NOT what resolves the duplicate-
//     row case: `ingested_at` is DateTime (1s), so it cannot break a
//     same-second tie. That is step 2's job, via FINAL.
//
//  2. Read the authoritative row via txByLedgerAndHash, now that step 1
//     narrowed the read to one ledger — cheap FINAL, deterministic, correct
//     even on an ingested_at tie (audit DAT-10; see txByLedgerAndHash).
//
// found=false when the hash is unknown (step 1 comes up empty).
func (r *ExplorerReader) txByHashScan(ctx context.Context, hash string) (TxSummary, bool, error) {
	const seqQ = `SELECT ledger_seq FROM stellar.transactions WHERE tx_hash = ? ORDER BY ingested_at DESC LIMIT 1`
	rows, err := r.conn.Query(ctx, seqQ, hash)
	if err != nil {
		return TxSummary{}, false, fmt.Errorf("clickhouse: tx %s: %w", hash, err)
	}
	if !rows.Next() {
		err := rows.Err()
		_ = rows.Close()
		return TxSummary{}, false, err
	}
	var seq uint32
	scanErr := rows.Scan(&seq)
	_ = rows.Close()
	if scanErr != nil {
		return TxSummary{}, false, fmt.Errorf("clickhouse: scan tx %s ledger: %w", hash, scanErr)
	}
	return r.txByLedgerAndHash(ctx, seq, hash)
}

// OperationsByTx returns a transaction's operations, ledger-scoped (so
// partition-pruned + fast — the caller passes the ledger from TransactionByHash).
//
// FINAL (audit DAT-10): ledger+tx_hash-scoped, so bounded to one partition
// and a primary-key prefix on ledger_seq — cheap, same reasoning as the
// sibling OperationsByLedger's FINAL just above. Without it, a re-ingested
// op left an un-merged duplicate part and this tx-detail view showed the
// operation twice.
func (r *ExplorerReader) OperationsByTx(ctx context.Context, seq uint32, hash string) ([]OpRow, error) {
	q := `SELECT ` + opCols + ` FROM stellar.operations FINAL
		WHERE ledger_seq = ? AND tx_hash = ? ORDER BY op_index`
	rows, err := r.conn.Query(ctx, q, seq, hash)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: tx %s ops: %w", hash, err)
	}
	defer func() { _ = rows.Close() }()
	return scanOps(rows)
}

// OperationResultsByTx returns op_index → result_code for a transaction
// (ledger-scoped; operation_results is ORDER BY (ledger_seq, tx_hash, op_index)
// so this is a primary-key point lookup).
func (r *ExplorerReader) OperationResultsByTx(ctx context.Context, seq uint32, hash string) (map[uint32]int32, error) {
	const q = `SELECT op_index, result_code FROM stellar.operation_results
		WHERE ledger_seq = ? AND tx_hash = ?`
	rows, err := r.conn.Query(ctx, q, seq, hash)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: tx %s op results: %w", hash, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[uint32]int32{}
	for rows.Next() {
		var idx uint32
		var code int32
		if err := rows.Scan(&idx, &code); err != nil {
			return nil, fmt.Errorf("clickhouse: scan op result: %w", err)
		}
		out[idx] = code
	}
	return out, rows.Err()
}

// TxOutcome is a transaction's applied verdict + result code. It stamps the
// operation LIST views (account history, ledger op list, /operations
// directory) so a failed transaction's operations are marked FAILED with a
// reason rather than shown as though they applied.
type TxOutcome struct {
	Successful bool
	ResultCode int32
}

// TxOutcomesByHash batch-reads the applied verdict + result code for a page's
// worth of transactions, keyed by tx_hash. The operation list views fetch
// operations WITHOUT their parent transaction, so they call this to stamp each
// op with its tx's outcome (transaction_successful + the tx result).
//
// The predicate is the EXACT SET of ledgers the page's operations sit in —
// never a [lo,hi] range across them. That distinction is the whole cost of
// this query, because an operation-list page is not contiguous: a sparse
// account's 50 most recent operations can straddle millions of ledgers, so a
// range predicate makes the scan scale with how IDLE the account is instead
// of with the page size. `ledger_seq` is the leading primary-key column AND
// the partition key (PARTITION BY intDiv(ledger_seq, 1000000), ORDER BY
// (ledger_seq, tx_index)), so an IN-set of ~50 values prunes to ~50 point
// ranges; a range predicate selects every granule between them and leaves the
// tx_hash bloom skip-index as the only filter — which cannot carry it: at
// bloom_filter(0.01), probing 50 hashes against ~124k candidate granules
// false-positives on ~1-(1-0.01)^50 ≈ 39% of them. Measured on r1
// (2026-09-03, cold, use_query_condition_cache=0, three real 50-op pages of
// idle accounts): the range form read 1.69-2.04 BILLION rows / 122-147 GiB
// and did not finish inside 60s; the exact-set form reads 319k-508k rows /
// 24-38 MiB in 32-61 ms — same rows, byte-identical. Passing the ledger set
// is also why this is safe to leave on the request path at all: cost is now
// bounded by the caller's page size, which ParseLimit bounds.
//
// The set is lossless by construction: an operation's parent transaction is
// in the operation's own ledger, so the ledgers of a page's ops are exactly
// the ledgers of their txs.
//
// FINAL stays. stellar.transactions is ReplacingMergeTree(ingested_at) and the
// duplicates are real, not theoretical — r1's partition 63 carries 183.5M
// duplicated (ledger_seq, tx_index) key-groups. It is deliberately NOT rewritten
// to `ORDER BY ingested_at DESC LIMIT 1 BY tx_hash`: `ingested_at` is DateTime
// (ONE-SECOND resolution), so a re-ingest batch that rewrites many rows within
// one wall-clock second TIES on the version column and a bare SELECT has no way
// to break that tie — it would silently serve the STALE pre-fix verdict. That is
// audit DAT-10, and txByLedgerAndHash above documents the same trap on this same
// table. FINAL resolves the tie using real insertion order. It is cheap here for
// exactly the reason it is cheap there: once ledger_seq is a primary-key point
// set, FINAL only merges the handful of parts touching those ledgers, so there
// is no PrimaryKeyExpand blow-up (the expand is what makes FINAL ruinous over a
// bloom-probed WIDE range: 32,930 skip-index granules re-expanded to 74,938).
// Measured premium of keeping it, same pages: 32-61 ms vs 17-18 ms without.
// Correctness at 2x of ~40 ms is the right trade; a stale verdict is not.
//
// Empty input returns an empty (non-nil) map.
//
// The SQL is a package-level const so explorer_tx_outcomes_test.go can pin the
// two clauses whose loss is silent and expensive (the ledger IN-set, and FINAL).
const txOutcomesByHashQuery = `SELECT tx_hash, successful, result_code FROM stellar.transactions FINAL
		WHERE ledger_seq IN (?) AND tx_hash IN (?)`

func (r *ExplorerReader) TxOutcomesByHash(ctx context.Context, ledgers []uint32, hashes []string) (map[string]TxOutcome, error) {
	if len(hashes) == 0 || len(ledgers) == 0 {
		return map[string]TxOutcome{}, nil
	}
	rows, err := r.conn.Query(ctx, txOutcomesByHashQuery, ledgers, hashes)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: tx outcomes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]TxOutcome, len(hashes))
	for rows.Next() {
		var hash string
		var ok uint8
		var code int32
		if err := rows.Scan(&hash, &ok, &code); err != nil {
			return nil, fmt.Errorf("clickhouse: scan tx outcome: %w", err)
		}
		out[hash] = TxOutcome{Successful: ok != 0, ResultCode: code}
	}
	return out, rows.Err()
}

// ContractActivityRow is a contract event for the contract-activity view
// (GET /v1/contracts/{c}). Ordered most-recent-first.
type ContractActivityRow struct {
	Seq        uint32
	CloseTime  time.Time
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	EventType  string
	Topic0Sym  string
	// TopicsDisplay / DataDisplay are human-readable renderings of the
	// event's remaining topics + data payload (S-016: rows showed only
	// topic_0 — 'transfer' fifty times with no amounts or parties).
	TopicsDisplay []string
	DataDisplay   string
}

// contractEventsRecentQuery builds ContractEventsRecent's SQL.
//
// LIMIT 1 BY the contract_events primary key (audit W4-storage-1, same class as
// DAT-10): stellar.contract_events is ReplacingMergeTree(ingested_at), so a
// re-ingested event (ch-live-catchup heal / ch-rebuild / partial-flush retry —
// all documented legitimate dup-part states) leaves an un-merged duplicate PART,
// byte-identical bar ingested_at, until a background merge — and without dedup
// this per-row activity feed served the SAME event TWICE. LIMIT 1 BY, not FINAL:
// FINAL would defeat the contract_id bloom skip-index and force ClickHouse to
// merge every overlapping part for this bloom-probed, no-lower-bound scan — the
// same O(table) trap recentOperationsQuery documents. LIMIT 1 BY dedups on the
// table's full ORDER BY tuple (ledger_seq, tx_hash, op_index, event_index) and
// composes with the existing ORDER BY, so it stays a cheap keyset/tip scan; run
// BEFORE the page-size LIMIT (ClickHouse clause order) it also stops an un-merged
// duplicate part from eating a page slot, mirroring the union-arm dedup.
//
// explorerScanSettings: the contract_id predicate rides a bloom skip-index
// over the billions-row contract_events table — granule-pruned but
// scan-shaped, and reading the wide topics_xdr/data_xdr columns per
// surviving granule is exactly the per-stream-buffer × part-fan-out product
// the pin bounds (route-sweep 2026-07-29: /v1/contracts/{id} was in the 8s
// 503 class).
// contractEventsRecentQuery deliberately carries NO `LIMIT 1 BY`: that
// clause disables ClickHouse's reverse read-in-order early exit, turning
// the busy-contract first page into an O(all-events-of-contract) sort —
// measured 16.3s vs 0.16s (100×) for a 17.9M-event contract on r1
// (2026-08-06, the CCW5IBJ7… soroswap-router 503). The W4-storage-1 RMT
// duplicate-part dedup still happens — in Go, in contractEventsScan: the
// full ORDER BY tuple makes duplicate row-identities ADJACENT in the
// stream, so adjacent-row collapse is exact. The page over-fetches
// contractEventsDedupHeadroom rows so dedup can collapse and still fill
// the page; a duplicate storm beyond that falls back to
// contractEventsRecentDedupQuery (in-CH dedup, slow, correct).
func contractEventsRecentQuery(hasCursor, hasLedgerSet bool) string {
	q := `SELECT ledger_seq, close_time, tx_hash, op_index, event_index, event_type, topic_0_sym,
			topics_xdr, data_xdr
		FROM stellar.contract_events WHERE contract_id = ?`
	if hasCursor {
		// Full row-identity tuple — see ContractEventsCursor: the 3-part
		// (ledger_seq, op_index, event_index) predicate skipped tied rows.
		q += ` AND (ledger_seq, tx_hash, op_index, event_index) < (?, ?, ?, ?)`
	}
	if hasLedgerSet {
		// Active-ledger bound from contract_active_ledgers — prunes the
		// scan to the granules of ledgers the contract actually touched,
		// which is what makes QUIET contracts fast (the reverse
		// read-in-order early exit already covers busy ones).
		q += ` AND ledger_seq IN (?)`
	}
	return q + ` ORDER BY ledger_seq DESC, tx_hash DESC, op_index DESC, event_index DESC` +
		` LIMIT ?` + explorerScanSettings
}

// contractEventsRecentDedupQuery is the legacy in-ClickHouse dedup shape —
// the correctness fallback when a duplicate storm eats the whole Go-side
// headroom. Slow on busy contracts (see contractEventsRecentQuery); only
// ever issued when the fast path provably could not fill the page.
func contractEventsRecentDedupQuery(hasCursor, hasLedgerSet bool) string {
	q := `SELECT ledger_seq, close_time, tx_hash, op_index, event_index, event_type, topic_0_sym,
			topics_xdr, data_xdr
		FROM stellar.contract_events WHERE contract_id = ?`
	if hasCursor {
		q += ` AND (ledger_seq, tx_hash, op_index, event_index) < (?, ?, ?, ?)`
	}
	if hasLedgerSet {
		q += ` AND ledger_seq IN (?)`
	}
	return q + ` ORDER BY ledger_seq DESC, tx_hash DESC, op_index DESC, event_index DESC` +
		` LIMIT 1 BY ledger_seq, tx_hash, op_index, event_index LIMIT ?` + explorerScanSettings
}

// contractEventsDedupHeadroom is the fast path's over-fetch beyond the
// requested page size. RMT duplicate parts exist only for rows whose
// parts haven't merged yet (a recent-ingest sliver), so duplicates on any
// given page are rare and few — 100 extra rows of headroom covers real
// merge windows by orders of magnitude while keeping the worst-case fetch
// at 600 rows.
const contractEventsDedupHeadroom = 100

// ContractEventsRecent returns a contract's most-recent events, descending.
// Relies on the contract_id bloom skip-index for quiet contracts and on
// reverse read-in-order early exit for busy ones — which is why the fast
// query carries NO FINAL and NO LIMIT 1 BY (both disable one of those two
// paths; see contractEventsRecentQuery). The RMT duplicate-part
// over-count (audit W4-storage-1) is collapsed by adjacent-row dedup in
// contractEventsScan, with an in-CH dedup fallback when the headroom is
// exhausted. A set cursor keyset-pages to older events by the full
// row-identity composite (ledger_seq, tx_hash, op_index, event_index) — a
// contract can emit many events in one ledger (and across many single-op
// txs that all tie at op_index=0/event_index=0), so anything less than
// the full tuple drops rows at a page boundary.
func (r *ExplorerReader) ContractEventsRecent(ctx context.Context, contractID string, limit int, cur ContractEventsCursor) ([]ContractActivityRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	fetch := limit + contractEventsDedupHeadroom

	// Active-ledger bound (contract_active_ledgers): when the index is
	// usable AND the per-contract walk is NON-EMPTY, prune the events read
	// to those ledgers — every event the page can serve lives in them (≥1
	// event per active ledger), so the bound is lossless for both the page
	// and its cursor. An EMPTY walk is NOT treated as authoritative "no
	// events" (audit W1-chrollup-3): the availability probe is a LIMIT-1
	// table-global emptiness check that cannot see PARTIAL backfill
	// coverage, so an applied-but-still-backfilling index can hold zero
	// rows for a quiet contract whose events do exist in contract_events.
	// Trusting an empty walk there served confidently-wrong "no events".
	// Both an empty walk and an index error therefore fall through to the
	// unbounded contract_events read (the source of truth) — correctness
	// over speed; the bloom skip-index keeps a genuinely-eventless contract
	// cheap anyway.
	var ledgers []uint32
	if r.contractLedgersIndexAvailable(ctx) {
		if ls, lerr := r.contractActiveLedgers(ctx, contractID, cur.Ledger, fetch); lerr == nil && len(ls) > 0 {
			ledgers = ls
		}
	}

	out, raw, err := r.contractEventsScan(ctx, contractEventsRecentQuery(cur.IsSet(), ledgers != nil), contractID, limit, fetch, cur, ledgers)
	if err != nil {
		return nil, err
	}
	if len(out) < limit && raw == fetch {
		// Duplicate storm: the over-fetch was ALL consumed and dedup still
		// couldn't fill the page — the only case where a short page would
		// be a lie (the handler's next_cursor emission keys on a FULL
		// page). Fall back to the in-ClickHouse dedup shape.
		out, _, err = r.contractEventsScan(ctx, contractEventsRecentDedupQuery(cur.IsSet(), ledgers != nil), contractID, limit, limit, cur, ledgers)
	}
	return out, err
}

// contractEventsScan issues one contract-events page query, collapsing
// adjacent duplicate row-identities (exact under the full ORDER BY tuple)
// and keeping at most `keep` rows. Returns the raw pre-dedup row count so
// the caller can distinguish "data exhausted" from "headroom exhausted".
// Display decoding runs only for kept rows.
func (r *ExplorerReader) contractEventsScan(ctx context.Context, q, contractID string, keep, fetch int, cur ContractEventsCursor, ledgers []uint32) ([]ContractActivityRow, int, error) {
	args := []any{contractID}
	if cur.IsSet() {
		args = append(args, cur.Ledger, cur.TxHash, cur.OpIndex, cur.EventIndex)
	}
	if ledgers != nil {
		args = append(args, ledgers)
	}
	args = append(args, fetch)

	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("clickhouse: contract %s events: %w", contractID, err)
	}
	defer func() { _ = rows.Close() }()
	var (
		out  []ContractActivityRow
		raw  int
		last ContractActivityRow
	)
	for rows.Next() {
		var e ContractActivityRow
		var topicsB64 []string
		var dataB64 string
		if err := rows.Scan(&e.Seq, &e.CloseTime, &e.TxHash, &e.OpIndex, &e.EventIndex, &e.EventType, &e.Topic0Sym,
			&topicsB64, &dataB64); err != nil {
			return nil, 0, fmt.Errorf("clickhouse: scan contract event: %w", err)
		}
		raw++
		if len(out) > 0 && e.Seq == last.Seq && e.TxHash == last.TxHash &&
			e.OpIndex == last.OpIndex && e.EventIndex == last.EventIndex {
			continue // un-merged RMT duplicate part (W4-storage-1)
		}
		last = e
		if len(out) == keep {
			// Page already full — keep draining rows.Next() only to count
			// raw (cheap: the fetch LIMIT bounds it), not to decode.
			continue
		}
		// Skip topic[0] (already surfaced as Topic0Sym) and render the
		// rest for display; decode failures degrade to omission.
		for i, t := range topicsB64 {
			if i == 0 {
				continue
			}
			if d := scval.DisplayB64(t); d != "" {
				e.TopicsDisplay = append(e.TopicsDisplay, d)
			}
		}
		e.DataDisplay = scval.DisplayB64(dataB64)
		out = append(out, e)
	}
	return out, raw, rows.Err()
}

// ContractDirectoryRow is one row of the contracts directory: a contract
// ranked by recent on-chain event activity.
type ContractDirectoryRow struct {
	ContractID string
	Events     int64
	LastLedger uint32
	LastSeen   time.Time
}

// recentContractsQuery is RecentContracts' SQL — see that method's doc for
// the uniqExact-vs-count() and no-FINAL rationale.
//
// explorerScanSettings: a multi-day GROUP BY over the billions-row
// contract_events table — the single heaviest read behind the /v1/contracts
// directory and squarely in the thread-fan-out memory class the pin bounds.
// Latency for this one is fixed by the directory's stale-serving cache (the
// scan runs detached, off the request deadline); the pin is the host-safety
// half of that fix.
const recentContractsQuery = `SELECT contract_id,
		       toInt64(uniqExact((ledger_seq, tx_hash, op_index, event_index))) AS events,
		       max(ledger_seq) AS last_ledger, max(close_time) AS last_seen
		FROM stellar.contract_events
		WHERE ledger_seq >= ?
		GROUP BY contract_id
		ORDER BY events DESC
		LIMIT ?` + explorerScanSettings

// RecentContracts returns the most active contracts by contract-event count
// within [sinceLedger, tip] — the contracts directory (GET /v1/contracts).
// Window-scoped so the GROUP BY stays bounded (contract_events is billions of
// rows all-time); the caller derives sinceLedger from the tip.
//
// The event count is uniqExact over the PRIMARY KEY, not count() (audit
// DAT-10). stellar.contract_events is ReplacingMergeTree(ingested_at) and a
// partially-succeeded flush is retried over the same range — by design, the
// writes are idempotent under RMT — so the table legitimately holds duplicate
// un-merged parts until a background merge collapses them. A bare count()
// therefore inflated a contract's event tally and MIS-RANKED this directory,
// and it did so worst exactly where it matters: this query is window-scoped to
// recent ledgers, which is where un-merged retries concentrate.
//
// Still NOT FINAL, deliberately: FINAL would defeat the contract_id bloom
// index and force a merge of every overlapping part. uniqExact over the PK
// tuple costs one aggregate state per contract and leaves the scan shape
// untouched — the same "dedup without FINAL" trade the operations-family
// readers took with LIMIT 1 BY (1bac345f). max() needs no such treatment: it
// is idempotent over duplicates and was already correct.
func (r *ExplorerReader) RecentContracts(ctx context.Context, limit int, sinceLedger uint32) ([]ContractDirectoryRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// Census-first (inventory #26 item 2): sum precomputed day rows —
	// sub-second against ~tens of millions of narrow rows — instead of
	// the 40s uniqExact GROUP BY over billions of contract_events. Day
	// resolution: the window floor rounds DOWN to the start of
	// sinceLedger's UTC day, so a ranking window is up to one day wider
	// than the exact ledger floor — immaterial for an activity ranking,
	// and the serving tail is at most one rollup cadence (30 min) stale.
	if r.censusAvailable(ctx) {
		if out, ok, err := r.recentContractsFromCensus(ctx, limit, sinceLedger); err != nil {
			return nil, err
		} else if ok {
			return out, nil
		}
		// !ok: sinceLedger predates the census coverage — fall through
		// to the exact scan.
	}
	rows, err := r.conn.Query(ctx, recentContractsQuery, sinceLedger, limit)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: recent contracts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ContractDirectoryRow
	for rows.Next() {
		var c ContractDirectoryRow
		if err := rows.Scan(&c.ContractID, &c.Events, &c.LastLedger, &c.LastSeen); err != nil {
			return nil, fmt.Errorf("clickhouse: scan contract directory row: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ContractEdgeRow is one edge of a contract's interaction map: another
// contract that emitted events in the same transactions as the subject.
type ContractEdgeRow struct {
	ContractID string
	SharedTxs  int64
}

// contractInteractionsQuery is ContractInteractions' SQL — see that method's
// body for the uniqExact / subjectTxCap rationale.
//
// explorerScanSettings: both halves (the subject's bloom-probed tx-set
// collection and the outer window scan matching those txs) are scan-shaped
// over contract_events; the pin bounds their combined thread fan-out
// (route-sweep 2026-07-29: /v1/contracts/{id}/interactions was in the 8s
// 503 class).
const contractInteractionsQuery = `SELECT contract_id, toInt64(uniqExact(tx_hash)) AS shared
		FROM stellar.contract_events
		WHERE ledger_seq >= ?
		  AND contract_id != ?
		  AND (ledger_seq, tx_hash) IN (
		      SELECT DISTINCT ledger_seq, tx_hash FROM stellar.contract_events
		      WHERE contract_id = ? AND ledger_seq >= ?
		      ORDER BY ledger_seq DESC
		      LIMIT ?
		  )
		GROUP BY contract_id
		ORDER BY shared DESC
		LIMIT ?` + explorerScanSettings

// ContractInteractions returns the contracts that co-occur with contractID
// in the same transactions, ranked by shared-tx count — the contract
// interaction map (GET /v1/contracts/{id}/interactions). Co-occurrence in a
// tx is a strong proxy for a cross-contract call (Soroban invokes nest within
// one InvokeHostFunction op, so the callee's events land in the caller's tx).
//
// Implemented as an IN-subquery (the inner query rides the contract_id bloom
// index to collect the subject's (ledger_seq, tx_hash) set; the outer scan
// finds the other contracts in those txs) rather than a self-join, which
// ClickHouse would materialise more expensively. Window-scoped via
// sinceLedger to bound both halves.
func (r *ExplorerReader) ContractInteractions(ctx context.Context, contractID string, limit int, sinceLedger uint32) ([]ContractEdgeRow, uint32, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	// Anchor the window to the contract's OWN recent activity, not to
	// wall-clock days (sub-second audit 2026-08-13). Both halves of this
	// query scale with the ledger SPAN they cover, and over the default
	// 90 days a busy contract cost 3-6s — the slowest panel left on the
	// contract page once the /wasm scan was bounded.
	//
	// This narrows what a busy contract reports, so it is a deliberate
	// choice and not a free win. It is consistent with what the endpoint
	// already does — subjectTxCap has always truncated busy contracts to
	// a recent sample — and the ranking is what the panel is for: at 500
	// ledgers the top edges came back in the SAME order as the full
	// window, with proportionally smaller counts (0.705s vs 3.017s).
	// Quiet contracts, which are most of them, have fewer active ledgers
	// than the cap and so are untouched by this and keep the full window.
	//
	// The effective floor is RETURNED so the response's since_ledger
	// describes the window actually served rather than the one asked
	// for.
	const activeLedgerWindow = 500
	if r.contractLedgersIndexAvailable(ctx) {
		if ls, err := r.contractActiveLedgers(ctx, contractID, 0, activeLedgerWindow); err == nil && len(ls) == activeLedgerWindow {
			// contractActiveLedgers is newest-first, so the last entry is
			// the oldest ledger inside the cap. Never widen the window.
			if oldest := ls[len(ls)-1]; oldest > sinceLedger {
				sinceLedger = oldest
			}
		}
	}
	// Cap the subject's transaction set to its most-recent 50k DISTINCT
	// (ledger, tx) rows. Without this, a mega-contract (a SAC / AMM router
	// with tens of millions of events in the window) builds an enormous IN
	// set and the probe times out. The inner subquery is SELECT DISTINCT
	// (ledger_seq, tx_hash) so the cap counts TRANSACTIONS, not event rows
	// (audit CHQ-1): a contract emitting ~10-20 events/tx would otherwise
	// consume the cap in event-space and sample only ~2.5-5k txs drawn from
	// the newest handful of ledgers. With DISTINCT the cap is a rich, bounded
	// sample of 50k recent txs, matching the comment and the shared_txs name.
	const subjectTxCap = 50_000
	// uniqExact(tx_hash), not count(), and this fixes TWO defects at once.
	//
	// DAT-10: contract_events is ReplacingMergeTree, so a retried partial
	// flush leaves duplicate un-merged rows that count() double-counted —
	// concentrated in exactly this query's recent window.
	//
	// DAT-11 #50: the column is named shared_txs and the API serves it as
	// shared_txs, but count() counted co-occurring EVENTS, not transactions.
	// A callee emitting 20 events in one shared tx scored 20. Counting
	// distinct tx_hash makes the number mean what its name has always
	// claimed, and is inherently duplicate-proof — a duplicated row carries
	// the same tx_hash and collapses on its own.
	//
	// Served values will DROP for busy pairs. That is the correction: the old
	// figure was an event count wearing a transaction count's name.
	rows, err := r.conn.Query(ctx, contractInteractionsQuery, sinceLedger, contractID, contractID, sinceLedger, subjectTxCap, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("clickhouse: contract %s interactions: %w", contractID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []ContractEdgeRow
	for rows.Next() {
		var e ContractEdgeRow
		if err := rows.Scan(&e.ContractID, &e.SharedTxs); err != nil {
			return nil, 0, fmt.Errorf("clickhouse: scan contract edge: %w", err)
		}
		out = append(out, e)
	}
	return out, sinceLedger, rows.Err()
}

// EventSummary is a lightweight contract-event row for the tx-detail view.
type EventSummary struct {
	OpIndex    uint32
	EventIndex uint32
	ContractID string
	EventType  string
	Topic0Sym  string
}

// EventsByTx returns a transaction's contract events (ledger-scoped — fast;
// contract_events is ORDER BY (ledger_seq, tx_hash, op_index, event_index)).
//
// FINAL (audit W4-storage-1, same class as DAT-10): ledger+tx_hash-scoped, so
// bounded to one partition and a primary-key prefix on ledger_seq — cheap, the
// same reasoning as the byte-twin OperationsByTx's FINAL. Without it, a
// re-ingested event left an un-merged duplicate ReplacingMergeTree part and this
// tx-detail view showed the event twice.
func (r *ExplorerReader) EventsByTx(ctx context.Context, seq uint32, hash string) ([]EventSummary, error) {
	const q = `SELECT op_index, event_index, contract_id, event_type, topic_0_sym
		FROM stellar.contract_events FINAL
		WHERE ledger_seq = ? AND tx_hash = ? ORDER BY op_index, event_index`
	rows, err := r.conn.Query(ctx, q, seq, hash)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: tx %s events: %w", hash, err)
	}
	defer func() { _ = rows.Close() }()
	var out []EventSummary
	for rows.Next() {
		var e EventSummary
		if err := rows.Scan(&e.OpIndex, &e.EventIndex, &e.ContractID, &e.EventType, &e.Topic0Sym); err != nil {
			return nil, fmt.Errorf("clickhouse: scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanTxSummaries(rows driver.Rows) ([]TxSummary, error) {
	var out []TxSummary
	for rows.Next() {
		var t TxSummary
		var ok uint8
		if err := rows.Scan(&t.Seq, &t.CloseTime, &t.TxHash, &t.TxIndex, &t.SourceAccount,
			&t.FeeCharged, &t.MaxFee, &t.OperationCount, &ok, &t.ResultCode, &t.MemoType, &t.Memo); err != nil {
			return nil, fmt.Errorf("clickhouse: scan tx: %w", err)
		}
		t.Successful = ok != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// censusHeadMaxLag is how stale the census HEAD (max(day)) may be before
// recentContractsFromCensus stops trusting it and falls through to the
// exact scan (audit W1-explorer-perf-2). One day of slack absorbs the UTC
// rollover window (before the day's first rollup, the freshest row is
// still yesterday's) while catching a genuinely stalled rollup timer,
// whose cadence is ~30 min.
const censusHeadMaxLag = 24 * time.Hour

// recentContractsCensusQuery sums the day-keyed census over the window.
// The day floor is resolved from sinceLedger via a pruned PK lookup on
// stellar.ledgers.
const recentContractsCensusQuery = `SELECT contract_id,
		       toInt64(sum(events)) AS events,
		       max(last_ledger) AS last_ledger, max(last_seen) AS last_seen
		FROM stellar.contracts_census_daily
		WHERE day >= ?
		GROUP BY contract_id
		ORDER BY events DESC
		LIMIT ?`

// recentContractsFromCensus serves the directory census from
// contracts_census_daily. ok=false when the census doesn't cover
// sinceLedger's day (caller falls back to the exact scan).
func (r *ExplorerReader) recentContractsFromCensus(ctx context.Context, limit int, sinceLedger uint32) ([]ContractDirectoryRow, bool, error) {
	// Resolve the window floor's UTC day from the ledger sequence
	// (pruned read on the ledgers PK, ms).
	dayRows, err := r.conn.Query(ctx,
		`SELECT toDate(close_time) FROM stellar.ledgers WHERE ledger_seq >= ? ORDER BY ledger_seq ASC LIMIT 1`,
		sinceLedger)
	if err != nil {
		return nil, false, fmt.Errorf("clickhouse: census day floor: %w", err)
	}
	var floor time.Time
	haveFloor := dayRows.Next()
	if haveFloor {
		if err := dayRows.Scan(&floor); err != nil {
			_ = dayRows.Close()
			return nil, false, fmt.Errorf("clickhouse: scan census day floor: %w", err)
		}
	}
	if cerr := dayRows.Close(); cerr != nil {
		return nil, false, cerr
	}
	if err := dayRows.Err(); err != nil {
		return nil, false, err
	}
	if !haveFloor {
		return nil, false, nil
	}

	// Coverage check: the census must reach back to the floor day (a
	// backfill still climbing toward the floor must not serve a
	// truncated tail) AND its HEAD must be fresh (audit
	// W1-explorer-perf-2). The old gate checked min(day) only — a
	// lower-bound test — so a stalled census-rollup timer (max(day)
	// frozen days ago) still passed the floor check and served a ranking
	// missing the last N days of activity, stamped as current because the
	// fast query itself kept succeeding. today() is read from the SAME
	// server as the census to avoid app/DB clock skew.
	covRows, err := r.conn.Query(ctx,
		`SELECT min(day), max(day), today() FROM stellar.contracts_census_daily`)
	if err != nil {
		return nil, false, fmt.Errorf("clickhouse: census coverage: %w", err)
	}
	var minDay, maxDay, chToday time.Time
	if covRows.Next() {
		if err := covRows.Scan(&minDay, &maxDay, &chToday); err != nil {
			_ = covRows.Close()
			return nil, false, err
		}
	}
	if cerr := covRows.Close(); cerr != nil {
		return nil, false, cerr
	}
	if err := covRows.Err(); err != nil {
		return nil, false, err
	}
	if minDay.After(floor) {
		return nil, false, nil
	}
	// Head-freshness gate: the rollup writes today's (partial) row every
	// cadence, so a healthy head is today or — right after UTC rollover,
	// before the day's first rollup — yesterday. A head older than that
	// means the timer has stalled; fall through to the exact scan
	// (always fresh) rather than serve a silently-stale ranking. The exact
	// scan is slower, but a stalled rollup is a degraded state that must
	// surface, not be papered over with days-old data.
	if chToday.Sub(maxDay) > censusHeadMaxLag {
		return nil, false, nil
	}

	rows, err := r.conn.Query(ctx, recentContractsCensusQuery, floor, limit)
	if err != nil {
		return nil, false, fmt.Errorf("clickhouse: recent contracts (census): %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ContractDirectoryRow
	for rows.Next() {
		var row ContractDirectoryRow
		if err := rows.Scan(&row.ContractID, &row.Events, &row.LastLedger, &row.LastSeen); err != nil {
			return nil, false, fmt.Errorf("clickhouse: scan recent contracts (census): %w", err)
		}
		out = append(out, row)
	}
	return out, true, rows.Err()
}
