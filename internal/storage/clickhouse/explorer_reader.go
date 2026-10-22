package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"golang.org/x/sync/singleflight"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// explorerScanSettings pins resources on every SCAN-SHAPED explorer read (keyed point reads skip it). Per-stream read buffers across part
// ranges amplify memory at default max_threads (4.76 GiB vs 89 MiB at 4); the 8 GiB ceiling fails one query loudly if the part layout
// shifts, and the spill pair turns an OOM above 4 GB into a slower success. SQL text, not clickhouse.WithSettings: settings do not reach
// the server on some driver paths.
const explorerScanSettings = ` SETTINGS max_threads = 4, max_memory_usage = 8589934592,` +
	` max_bytes_before_external_group_by = 4000000000, max_bytes_before_external_sort = 4000000000`

// schemaProbeRetryAfter is how long a probe that got NO answer (transport error, deadline, non-schema error) waits before querying again,
// so an outage is not met with a probe per explorer read.
const schemaProbeRetryAfter = 5 * time.Second

// schemaProbeLease is how long a requireRows probe's POSITIVE verdict is trusted. Rows can vanish (TRUNCATE, DROP+recreate) and callers
// derive authority from it (an index miss is a 404), so it is a lease, never a latch; renewal is lazy, only while the surface is read.
const schemaProbeLease = 30 * time.Second

// schemaProbeStaleLeases bounds how long an EXPIRED positive lease is honoured while renewal gets NO answer (2 min at the default lease):
// dropping it at once would push readers onto the slow/503 arm just when the store refuses queries; unbounded would re-create the latch.
const schemaProbeStaleLeases = 4

// schemaProbe caches a "does this schema object exist" answer, but only once the
// server has answered. See ExplorerReader.probeSchema for which verdicts latch and which are leased.
type schemaProbe struct {
	name    string // the `probe` metric label; assigned in [newExplorerReader]
	mu      sync.Mutex
	settled bool      // an authoritative answer was received
	present bool      // meaningful only when settled
	retryAt time.Time // while now < retryAt, answer from cache without re-querying

	// confirmedAt is when a probe last observed the object usable. It anchors the
	// positive lease; non-zero also means this process has seen it (see schemaProbe.record).
	confirmedAt time.Time

	// retryAfter overrides [schemaProbeRetryAfter]. Zero uses the default;
	// tests set a negative value to disable the negative cache.
	retryAfter time.Duration

	// lease overrides [schemaProbeLease]; zero uses the default. now
	// overrides time.Now. Both exist for tests.
	lease time.Duration
	now   func() time.Time
}

func (p *schemaProbe) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *schemaProbe) leaseLength() time.Duration {
	if p.lease != 0 {
		return p.lease
	}
	return schemaProbeLease
}

// leaseExpired reports whether a settled verdict must be re-confirmed.
// Only a requireRows POSITIVE verdict is leased. Callers hold p.mu.
func (p *schemaProbe) leaseExpired(now time.Time, requireRows bool) bool {
	return requireRows && p.present && !now.Before(p.confirmedAt.Add(p.leaseLength()))
}

// staleVerdict is the answer while an expired positive lease cannot be renewed: true inside the [schemaProbeStaleLeases] bound, false past
// it or for a probe with no positive verdict. Callers hold p.mu.
func (p *schemaProbe) staleVerdict(now time.Time) bool {
	return p.settled && p.present &&
		now.Before(p.confirmedAt.Add(schemaProbeStaleLeases*p.leaseLength()))
}

// armRetry starts the back-off window after a probe that did not end in a
// usable verdict. Callers hold p.mu.
func (p *schemaProbe) armRetry(now time.Time) {
	retryAfter := p.retryAfter
	if retryAfter == 0 {
		retryAfter = schemaProbeRetryAfter
	}
	if retryAfter > 0 {
		p.retryAt = now.Add(retryAfter)
	}
}

// cached returns the verdict to serve WITHOUT querying, or ok=false when this caller must probe: nothing settled yet, or a positive lease
// has run out and the back-off window is not holding probes off.
func (p *schemaProbe) cached(requireRows bool) (verdict, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	if p.settled && !p.leaseExpired(now, requireRows) {
		return p.present, true
	}
	if !p.retryAt.IsZero() && now.Before(p.retryAt) {
		// A recent probe got no usable answer; don't pile on.
		return p.staleVerdict(now), true
	}
	return false, false
}

// record folds one probe outcome into the cache and returns the verdict
// for the caller that ran it. empty is meaningful only when err == nil.
func (p *schemaProbe) record(err error, empty, requireRows bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	if p.settled && !p.leaseExpired(now, requireRows) {
		// A concurrent probe already got (or renewed) the answer.
		return p.present
	}
	switch {
	case err == nil && !empty:
		p.settled, p.present, p.confirmedAt = true, true, now
		p.retryAt = time.Time{}
		return true
	case isSchemaAbsent(err) && p.confirmedAt.IsZero():
		// Never seen by this process: a deployment shape, latched.
		p.settled, p.present = true, false
		return false
	case err == nil || isSchemaAbsent(err):
		// The server ANSWERED and the object is not usable now (empty, or vanished after
		// being seen, e.g. DROP+recreate): revoke any positive verdict at once and latch
		// nothing, so a later probe picks the object back up.
		p.settled, p.present = false, false
		p.armRetry(now)
		return false
	default:
		// No answer about the object at all.
		p.armRetry(now)
		return p.staleVerdict(now)
	}
}

// schemaAbsentCodes are the ClickHouse error codes that DEFINITIVELY mean "that schema object does not exist". Keep it narrow: an unlisted
// code costs one extra probe, but a listed non-schema code (159 TIMEOUT_EXCEEDED, 202, 241, 209, or the 1002 catch-all) latches the probe
// false for the process lifetime.
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

var absentEmptyTables = []string{"account_entry_ledgers", "asset_entry_ledgers", "sponsorship_events"}

func init() {
	for _, t := range absentEmptyTables {
		obs.ExplorerSchemaAbsentEmptyTotal.WithLabelValues(t)
	}
}

// absentReadsEmpty reports whether err is a missing-schema answer that the caller serves as an empty result,
// counting it so an unbuilt index pages instead of reading as "no activity".
func absentReadsEmpty(table string, err error) bool {
	if !isSchemaAbsent(err) {
		return false
	}
	obs.ExplorerSchemaAbsentEmptyTotal.WithLabelValues(table).Inc()
	return true
}

// ExplorerReader serves the network-explorer reads (ADR-0038) from the certified lake (ADR-0034); construct once and reuse, reads are by immutable key and cacheable.
type ExplorerReader struct {
	conn driver.Conn

	// Whether stellar.tx_hash_index exists; false means every hash lookup takes the bloom-skip-index scan.
	txIndexProbe schemaProbe

	// txCoverageProbe probes stellar.tx_hash_index_coverage, written when ch-txindex-backfill
	// finishes genesis to tip. Without a row the index is not proof of coverage, so a miss must not be a 404.
	txCoverageProbe schemaProbe

	// opTypesCoverageProbe probes stellar.account_op_types_coverage, written after the account_op_types backfill is verified
	// (deploy/clickhouse/account_op_types.sql Step 5). Without a row the table is partial and op-type counts take the join.
	opTypesCoverageProbe schemaProbe

	// opTypeRollup caches the stellar.account_op_type_counts read plan; no plan means the indexed query.
	opTypeRollup opTypeRollupCache
	// movementRollup caches the stellar.account_movement_counts read plan (the movements partition directory).
	movementRollup opTypeRollupCache

	// contractLedgersProbe probes stellar.contract_active_ledgers. Present and non-empty bounds ContractEventsRecent to the contract's
	// active ledgers; absent means the unbounded reverse walk. requireRows: per-contract emptiness is served as an authoritative "no
	// events", so an existing-but-empty index must read as unavailable.
	contractLedgersProbe schemaProbe

	// instanceChangesProbe probes stellar.contract_instance_changes. Present and non-empty
	// means ContractCodeHistory reads the keyed timeline instead of the scan-shaped key_xdr
	// predicate. requireRows, as with the siblings: emptiness is served as authoritative.
	instanceChangesProbe schemaProbe

	// instanceKeyProbe probes whether contract_instance_changes has the tx-keyed shape
	// (tx_hash + intra_ledger_seq). Without it, intra_ledger_seq-ordered reads would 500,
	// so they fall back to change_index order.
	instanceKeyProbe schemaProbe

	// censusProbe probes stellar.contracts_census_daily. Present and non-empty means
	// RecentContracts sums day rows instead of a uniqExact GROUP BY over contract_events. requireRows.
	censusProbe schemaProbe

	// accountsStatsProbe probes stellar.accounts_stats. requireRows: an unpopulated
	// rollup 503s rather than serving zeros as facts.
	accountsStatsProbe schemaProbe

	// accountCreatorsProbe probes stellar.account_creators_rollup. requireRows: an empty
	// board would read as "nobody has created an account".
	accountCreatorsProbe schemaProbe

	// accountSponsorsProbe probes stellar.account_sponsors_rollup. requireRows: an empty
	// board would read as "nobody has ever sponsored an account".
	accountSponsorsProbe schemaProbe

	// accountCreatorEdgesProbe / accountSponsorEdgesProbe probe the graph edge tables.
	// requireRows: an empty edge table would make /graph claim "created by nobody and
	// sponsored nobody", a claim rather than an absence; the handler 503s instead.
	accountCreatorEdgesProbe schemaProbe
	accountSponsorEdgesProbe schemaProbe

	// holdersRollupProbe probes stellar.asset_holders_rollup. Present and non-empty means
	// AssetHolders serves keyed precomputed boards; absent means the legacy FINAL-scan path.
	// requireRows: an empty rollup must read as unavailable, not "no asset has holders".
	holdersRollupProbe schemaProbe

	// sacVerdicts memoises SACClassicAssetName answers an instance proved for good.
	sacVerdicts sacVerdictMemo
	// activityBases memoises each contract's activity span below a settled ledger.
	activityBases contractActivityMemo

	// cap67 movements coverage cache (see Cap67MovementsWatermark): cap67WMErr/At
	// negatively cache a failed read; cap67WMFlight is non-nil while a read is in flight.
	cap67WMMu     sync.Mutex
	cap67Cov      Cap67Coverage
	cap67WMAt     time.Time
	cap67WMErr    error
	cap67WMErrAt  time.Time
	cap67WMFlight chan struct{}

	// protocolDaily memoises the protocol-page reads over contract_events_daily; see memoProtocolDaily.
	protocolDaily protocolDailyMemo

	// opsBySourceProbe probes stellar.ops_by_source. The account-history readers REFUSE
	// without it: a bloom-scan fallback would restore the slow sourced arm and an empty
	// arm would hide the account's own transactions.
	opsBySourceProbe schemaProbe
	// opsBySourceProofProbe probes for ch-index-proof's watermark row. The readers serve without it (non-empty is all
	// they can check per request, and there is no fallback); its absent answer is what pages.
	opsBySourceProofProbe schemaProbe

	// accountActivityProbe probes stellar.account_activity. Present and non-empty bounds AccountOperations' reverse primary-key resolve at
	// the account's last-active ledger (`ledger_seq <= ?`); absent means unbounded. The bound is only a perf hint, and an available
	// watermark is a safe UPPER bound by construction (too low would HIDE rows).
	accountActivityProbe schemaProbe

	// lecVersionProbe probes whether ledger_entries_current has the `version` column
	// ((ledger_seq<<32)|intra_ledger_seq). Same-ledger tie-break queries use it where it
	// exists and fall back to `ledger_seq` otherwise, or they 500 on "Unknown identifier `version`".
	lecVersionProbe schemaProbe

	// wealthCache backs AccountsByWealthCached: the wealth ranking is a FINAL scan that
	// cannot fit a request deadline, so it is served from here and refreshed in the background.
	wealthCache *accountsWealthCache

	// wealthRefreshErr, when set, receives errors from the detached wealth refresh so a
	// persistently failing one (pinning /v1/accounts on 503) is visible in logs.
	wealthRefreshErr func(error)

	// stateCache + stateFlight back AccountStateCached: detail reads scan the current-state
	// table under the bounded profile, so concurrent requests contended; the cache absorbs repeats.
	stateCache  *accountStateCache
	stateFlight *perKeyFlight

	// ttlVerdicts fronts ClassifyTTLLiveness for SoroswapPairReserves' archived-pair filter.
	// The classification scans the ttl prefix and cannot run per request, so verdicts are served stale-while-revalidate.
	ttlVerdicts *ttlLivenessCache

	// refreshGate bounds concurrent detached cache refreshes (see refresh_gate.go); nil in test-built readers admits everything.
	refreshGate *RefreshGate

	// disasmCache backs buildWasmDisassembly: the wabt fork/exec for a wasm hash is paid
	// once per process. Nil-safe (permanent miss).
	disasmCache *wasmDisasmCache

	// moduleCache and wasmFlight back ContractWasm's per-hash stage: concurrent cold
	// requests for one hash share a single blob read and wabt run.
	moduleCache *wasmModuleCache
	wasmFlight  singleflight.Group
}

// SetWealthRefreshErrorHandler installs a callback for background refresh failures (wealth ranking and TTL verdicts) that would otherwise pin a surface stale; call once at wiring.
func (r *ExplorerReader) SetWealthRefreshErrorHandler(fn func(error)) {
	r.wealthRefreshErr = fn
	if r.ttlVerdicts != nil {
		r.ttlVerdicts.onErr = fn
	}
}

// NewExplorerReader dials ClickHouse with a request-sized pool and pings it, using the environment's identity ([chAuth]).
func NewExplorerReader(ctx context.Context, addr string) (*ExplorerReader, error) {
	return NewExplorerReaderAuth(ctx, addr, "", "")
}

// NewExplorerReaderAuth is NewExplorerReader with an explicit username/password for the API's serving-isolation profile (ADR-0048 D4); both empty resolves the environment's identity.
func NewExplorerReaderAuth(ctx context.Context, addr, username, password string) (*ExplorerReader, error) {
	auth, err := authOrEnv(username, password)
	if err != nil {
		return nil, err
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:        []string{addr},
		Auth:        auth,
		Settings:    clickhouse.Settings{"max_execution_time": 30},
		DialTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second,
		// Explorer pages fan out (one cold contract page issues five reads) and the detached
		// refresh gate takes half the pool, so 16 keeps that gate wider than one page.
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
	return newExplorerReader(conn), nil
}

// newExplorerReader wires a reader over an open connection. Every schemaProbe is named
// here (its metric label); TestNewExplorerReader_EverySchemaProbeIsNamed fails on an omission.
func newExplorerReader(conn driver.Conn) *ExplorerReader {
	return &ExplorerReader{
		conn:                     conn,
		txIndexProbe:             schemaProbe{name: "tx_hash_index"},
		txCoverageProbe:          schemaProbe{name: "tx_hash_index_coverage"},
		opTypesCoverageProbe:     schemaProbe{name: "account_op_types_coverage"},
		contractLedgersProbe:     schemaProbe{name: "contract_active_ledgers"},
		instanceChangesProbe:     schemaProbe{name: "contract_instance_changes"},
		instanceKeyProbe:         schemaProbe{name: "contract_instance_changes_tx_key"},
		censusProbe:              schemaProbe{name: "contracts_census_daily"},
		accountsStatsProbe:       schemaProbe{name: "accounts_stats"},
		accountCreatorsProbe:     schemaProbe{name: "account_creators_rollup"},
		accountSponsorsProbe:     schemaProbe{name: "account_sponsors_rollup"},
		accountCreatorEdgesProbe: schemaProbe{name: "account_creator_edges"},
		accountSponsorEdgesProbe: schemaProbe{name: "account_sponsor_edges"},
		holdersRollupProbe:       schemaProbe{name: "asset_holders_rollup"},
		opsBySourceProbe:         schemaProbe{name: "ops_by_source"},
		opsBySourceProofProbe:    schemaProbe{name: "ops_by_source_proof"},
		accountActivityProbe:     schemaProbe{name: "account_activity"},
		lecVersionProbe:          schemaProbe{name: "ledger_entries_current_version"},
		wealthCache:              newAccountsWealthCache(),
		stateCache:               newAccountStateCache(),
		stateFlight:              newPerKeyFlight(),
		refreshGate:              NewRefreshGate(DefaultDetachedRefreshLimit),
		disasmCache:              newWasmDisasmCache(),
		moduleCache:              newWasmModuleCache(),
		ttlVerdicts: newTTLLivenessCache(func(ctx context.Context, keys []string) (map[string]TTLLiveness, error) {
			// Verdicts are judged at the lake's tip as of compute time.
			_, asOf, err := entryChangeLedgerBounds(ctx, conn)
			if err != nil {
				return nil, err
			}
			return ClassifyTTLLiveness(ctx, conn, keys, asOf)
		}),
	}
}

// Close releases the connection pool.
func (r *ExplorerReader) Close() error { return r.conn.Close() }

// LedgerHeader is one stellar.ledgers row; total_coins and fee_pool are XLM stroops above 2^53, serialised as strings by the API (ADR-0003).
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

// TxSummary is one stellar.transactions row; Memo is decoded at ingest, memo_type is the discriminant.
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
	// Fee-bump outer layer; FeeAccount is "" on a non-fee-bump tx and on older fee bumps.
	InnerTxHash     string
	FeeAccount      string
	FeeBumpFee      int64
	InnerResultCode int32
}

const ledgerCols = `ledger_seq, close_time, ledger_hash, prev_hash, protocol_version,
	tx_count, op_count, soroban_event_count, total_coins, fee_pool, base_fee, base_reserve`

func scanLedger(rows driver.Rows) (LedgerHeader, error) {
	var l LedgerHeader
	err := rows.Scan(&l.Seq, &l.CloseTime, &l.LedgerHash, &l.PrevHash, &l.ProtocolVersion,
		&l.TxCount, &l.OpCount, &l.SorobanEventCount, &l.TotalCoins, &l.FeePool, &l.BaseFee, &l.BaseReserve)
	return l, err
}

// RecentLedgers returns up to `limit` ledgers descending; beforeSeq > 0 returns only ledgers strictly below it (keyset).
func (r *ExplorerReader) RecentLedgers(ctx context.Context, limit int, beforeSeq uint32) ([]LedgerHeader, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT ` + ledgerCols + ` FROM stellar.ledgers FINAL`
	args := []any{}
	// A cursor page reads ~64 ledgers per row (one granule per part for the limit-1 probes FirstCapturedClosedAfter issues),
	// never more than the tail window: without a lower bound this is a whole-table FINAL merge, caller-controlled via ?before=.
	window := uint32(min(recentLedgersTailWindow, 64*limit))
	if beforeSeq > 0 {
		// A short page (a lake hole wider than the window) is re-read wider below. Clamp at 0 to avoid uint32 underflow.
		lower := uint32(0)
		if beforeSeq > window {
			lower = beforeSeq - window
		}
		q += ` WHERE ledger_seq < ? AND ledger_seq >= ?`
		args = append(args, beforeSeq, lower)
	} else {
		// Tip query (the hot path): bound to a tail window so FINAL merges only the newest
		// partition instead of every part. The window is a wide multiple of the max page size
		// so it cannot truncate a legitimate first page.
		q += ` WHERE ledger_seq > (SELECT max(ledger_seq) FROM stellar.ledgers) - ?`
		args = append(args, uint32(recentLedgersTailWindow))
	}
	q += ` ORDER BY ledger_seq DESC LIMIT ?` + explorerScanSettings
	args = append(args, limit)

	out, err := r.queryLedgers(ctx, q, args, limit)
	if err != nil || len(out) == limit || (beforeSeq > 0 && beforeSeq <= window) {
		return out, err
	}
	return r.recentLedgersWidened(ctx, limit, beforeSeq)
}

// recentLedgersWidened re-reads a short page over geometrically wider windows: the lake is not contiguous (a dropped live extract leaves
// no ledgers row until gap-scan heals it). The top is clamped to the real tip so widening spans only a genuine hole.
func (r *ExplorerReader) recentLedgersWidened(ctx context.Context, limit int, beforeSeq uint32) ([]LedgerHeader, error) {
	var hi uint32
	if err := r.conn.QueryRow(ctx, `SELECT max(ledger_seq) FROM stellar.ledgers`).Scan(&hi); err != nil {
		return nil, fmt.Errorf("clickhouse: recent ledgers tip: %w", err)
	}
	if beforeSeq > 0 && beforeSeq-1 < hi {
		hi = beforeSeq - 1
	}
	q := `SELECT ` + ledgerCols + ` FROM stellar.ledgers FINAL WHERE ledger_seq <= ? AND ledger_seq >= ?` +
		` ORDER BY ledger_seq DESC LIMIT ?` + explorerScanSettings
	for window := uint64(4 * recentLedgersTailWindow); ; window *= 4 {
		lower := uint32(0)
		if uint64(hi) > window {
			lower = hi - uint32(window)
		}
		out, err := r.queryLedgers(ctx, q, []any{hi, lower, limit}, limit)
		if err != nil || len(out) == limit || lower == 0 {
			return out, err
		}
	}
}

func (r *ExplorerReader) queryLedgers(ctx context.Context, q string, args []any, limit int) ([]LedgerHeader, error) {
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

// LedgerBySeq returns one ledger header; found=false (nil error) when absent.
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

// CloseTimeForLedger returns a ledger's close time (found=false, nil error when absent) with FINAL, since stellar.ledgers is a ReplacingMergeTree; callers fail closed on a miss rather than using time.Now().
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

// LatestLedgerLookbackLedgers is how far below maxSeq LatestLedgerAtOrBefore looks and the single definition of the supply snapshot's stalled-lake bound; widening it widens the scan.
const LatestLedgerLookbackLedgers = 512

// latestLedgerLookbackFloor is that window's lower bound, saturating at 0 to avoid uint32 wraparound.
func latestLedgerLookbackFloor(maxSeq uint32) uint32 {
	if maxSeq < LatestLedgerLookbackLedgers {
		return 0
	}
	return maxSeq - LatestLedgerLookbackLedgers
}

// latestLedgerAtOrBeforeQuery reads the newest landed ledger inside that window. The LOWER bound is load-bearing: stellar.ledgers is
// PARTITION BY intDiv(ledger_seq, 1000000), so `ledger_seq <= X` alone prunes no partition below X. FINAL stays and is cheap here
// (duplicates concentrate at the tip), and close_time is stamped as a snapshot's ObservedAt, so it must be the newest-ingested version.
const latestLedgerAtOrBeforeQuery = `SELECT ledger_seq, close_time FROM stellar.ledgers FINAL
	WHERE ledger_seq BETWEEN ? AND ? ORDER BY ledger_seq DESC LIMIT 1`

// LatestLedgerAtOrBefore returns the newest ledgers row in [maxSeq-LatestLedgerLookbackLedgers, maxSeq]; found=false means an empty, gapped or stalled lake, which callers fail closed on.
func (r *ExplorerReader) LatestLedgerAtOrBefore(ctx context.Context, maxSeq uint32) (uint32, time.Time, bool, error) {
	rows, err := r.conn.Query(ctx, latestLedgerAtOrBeforeQuery, latestLedgerLookbackFloor(maxSeq), maxSeq)
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
	const q = `SELECT ` + txCols + `
		FROM stellar.transactions FINAL WHERE ledger_seq = ? ORDER BY tx_index ASC LIMIT ?`
	rows, err := r.conn.Query(ctx, q, seq, limit)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: ledger %d txs: %w", seq, err)
	}
	defer func() { _ = rows.Close() }()
	return scanTxSummaries(rows)
}

// OpRow is one stellar.operations row; OpType is the XDR enum string, BodyXDR the base64 body for read-time decode, SourceAccount may be empty (inherits the tx source).
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

// opColsLight omits body_xdr, whose read dominates cost (~40ms vs ~600ms on the
// operations table). RecentOperations uses it for the cheap directory listing.
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

// scanOpsLight scans opColsLight (BodyXDR stays "").
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

// RecentOperations returns recent operations network-wide, newest first, keyset-paged by (ledger_seq, tx_index, op_index) with BodyXDR always ""; a cursor page can be refused with ErrOperationsCursorTooDeep.
func (r *ExplorerReader) RecentOperations(ctx context.Context, limit int, cur ExplorerCursor) ([]OpRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.recentOperationsPage(ctx, limit, cur, true, nil)
	if err != nil {
		return nil, err
	}
	if len(rows) >= limit {
		return rows, nil
	}
	return r.recentOperationsPage(ctx, limit, cur, false, nil)
}

// recentOperationsPage runs ONE pass (tail-bounded or not, optionally by opTypes): it reads a small window in sort-key order and dedups
// adjacent duplicates in Go; a window that cannot prove a full page falls back to the exact LIMIT 1 BY query.
func (r *ExplorerReader) recentOperationsPage(ctx context.Context, limit int, cur ExplorerCursor, bounded bool, opTypes []string) ([]OpRow, error) {
	window := windowRows(limit, windowFactorKeys)
	typed := len(opTypes) > 0
	rows, err := r.queryRecentOperations(ctx, recentOperationsSQL(cur.IsSet(), bounded, false, typed), cur, bounded, opTypes, window)
	if err != nil {
		return nil, err
	}
	deduped, ok := dedupWindow(rows, window, limit, opRowKey, nil)
	if ok {
		if len(deduped) > limit {
			deduped = deduped[:limit]
		}
		return deduped, nil
	}
	exactFallback("recent_operations")
	return r.queryRecentOperations(ctx, recentOperationsSQL(cur.IsSet(), bounded, true, typed), cur, bounded, opTypes, limit)
}

func opRowKey(o OpRow) [3]uint32 { return [3]uint32{o.Seq, o.TxIndex, o.OpIndex} }

// queryRecentOperations runs one recentOperationsQuery-shaped statement for `n` rows.
// Arg order mirrors clause order: cursor tuple, window lower bound, op-type list, n.
func (r *ExplorerReader) queryRecentOperations(ctx context.Context, q string, cur ExplorerCursor, bounded bool, opTypes []string, n int) ([]OpRow, error) {
	args := []any{}
	switch {
	case cur.IsSet():
		// Ledger binds TWICE: once to the index-usable `ledger_seq < ?` arm and once to the
		// `ledger_seq = ?` arm that confines the tuple comparison to one ledger.
		args = append(args, cur.Ledger, cur.Ledger, cur.A, cur.B)
		if bounded {
			args = append(args, tailWindowFloor(cur.Ledger))
		}
	case bounded:
		args = append(args, uint32(recentLedgersTailWindow))
	}
	if len(opTypes) > 0 {
		args = append(args, opTypes)
	}
	args = append(args, n)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		if isTooManyRows(err) {
			// The lake REFUSED the cursor; surface it as its own class, never as an internal fault or retryable capacity.
			return nil, fmt.Errorf("clickhouse: recent operations from cursor %d.%d.%d: %w: %w",
				cur.Ledger, cur.A, cur.B, ErrOperationsCursorTooDeep, err)
		}
		return nil, fmt.Errorf("clickhouse: recent operations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanOpsLight(rows)
}

// recentOperationsQuery builds the exact-dedup fallback SQL. LIMIT 1 BY the primary key, not FINAL: stellar.operations is a
// ReplacingMergeTree and an un-merged duplicate would be served twice, while FINAL would merge every overlapping part. The `bounded` lower
// ledger bound is a partition-pruning bound only.
func recentOperationsQuery(hasCursor, bounded bool) string {
	return recentOperationsSQL(hasCursor, bounded, true, false)
}

func recentOperationsSQL(hasCursor, bounded, exactDedup, typed bool) string {
	var conds []string
	switch {
	case hasCursor && bounded:
		conds = append(conds, recentOperationsCursorPredicate, `ledger_seq >= ?`)
	case hasCursor:
		conds = append(conds, recentOperationsCursorPredicate)
	case bounded:
		conds = append(conds, `ledger_seq > (SELECT max(ledger_seq) FROM stellar.operations) - ?`)
	}
	if typed {
		conds = append(conds, `op_type IN (?)`)
	}
	q := `SELECT ` + opColsLight + ` FROM stellar.operations`
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, ` AND `)
	}
	q += ` ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC`
	if exactDedup {
		q += ` LIMIT 1 BY ledger_seq, tx_index, op_index`
	}
	q += ` LIMIT ?` + explorerScanSettings
	if hasCursor {
		q += recentOperationsCursorRowCeiling
	}
	return q
}

// recentOperationsCursorPredicate is `(ledger_seq, tx_index, op_index) < (?, ?, ?)` expanded as L < l OR (L = l AND (T,O) < (t,o)):
// KeyCondition does not decompose a 3-column tuple compare, so the tuple form would scan the whole table. The outer parentheses are
// load-bearing: AND binds tighter than OR, so without them `AND ledger_seq >= ?` would attach to the equality arm alone and return every
// operation below the cursor.
const recentOperationsCursorPredicate = `(ledger_seq < ? OR (ledger_seq = ? AND (tx_index, op_index) < (?, ?)))`

// recentOperationsCursorRowCeiling is the row budget on a CURSOR page: `?cursor=` is publicly mintable, so a pathological pick is refused
// (TOO_MANY_ROWS, 158). read_overflow_mode is 'throw': 'break' would truncate the page and mint a next_cursor from the wrong row.
const recentOperationsCursorRowCeiling = `, max_rows_to_read = 200000000, read_overflow_mode = 'throw'`

// chTooManyRows is ClickHouse's TOO_MANY_ROWS (max_rows_to_read under read_overflow_mode='throw').
const chTooManyRows = 158

// ErrOperationsCursorTooDeep means a cursor page tripped recentOperationsCursorRowCeiling; render as a client error on `?cursor=`, never retryable (the same cursor is refused identically).
var ErrOperationsCursorTooDeep = errors.New("clickhouse: operations cursor exceeds the per-request row budget")

// isTooManyRows reports the server refusing a query for its row budget (158); mirrors isMemoryLimitExceeded.
func isTooManyRows(err error) bool {
	var chErr *clickhouse.Exception
	return errors.As(err, &chErr) && chErr.Code == chTooManyRows
}

// OpTypeCount is one op-type's count in the stats window.
type OpTypeCount struct {
	OpType string
	Count  int64
}

// opTypeStatsQuery is OperationTypeStats' SQL. FINAL: stellar.operations is ReplacingMergeTree(ingested_at) and an un-merged duplicate
// inflates count(). explorerScanSettings because this FINAL GROUP BY over a day of operations is in the thread-fan-out memory class.
const opTypeStatsQuery = `SELECT op_type, toInt64(count()) AS c
		FROM stellar.operations FINAL
		WHERE ledger_seq > (SELECT max(ledger_seq) FROM stellar.operations) - ?
		GROUP BY op_type
		ORDER BY c DESC` + explorerScanSettings

// OperationTypeStats returns per-op-type counts over the most-recent `windowLedgers` ledgers (default ~24h), bounded at the table's tip for partition pruning, sorted descending.
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
	// End-of-day chain state from the day's LAST ledger (argMax over ledger_seq): the
	// cumulative fee pool and total XLM in stroops (exceed 2^53, serialised as strings,
	// ADR-0003) and the protocol version. Daily fee burn is the delta between days, computed by the caller.
	FeePool         int64
	TotalCoins      int64
	ProtocolVersion uint32
	// Partial marks a bucket not covering a whole UTC day (only TODAY). Callers must render
	// it distinctly and EXCLUDE it from window totals, or the chart drops at the right edge.
	Partial bool
}

// ledgersPerDayPruningEstimate is a generous UPPER bound on ledgers per day (5.0s cadence; observed ~14,950/day), only a PARTITION-PRUNING
// HINT, never a window boundary: overshooting scans wider, undershooting silently truncates, and as the boundary it would span more days
// than asked.
const ledgersPerDayPruningEstimate = 17280

// recentLedgersTailWindow bounds the tip-page query to a tail slice so FINAL prunes to
// the newest partition(s). 5000 is ~25x the max page size: one read fills a hole-free page.
const recentLedgersTailWindow = 5000

// NetworkThroughput returns daily throughput for the most-recent `windowDays` UTC days (default 30, cap 365), ascending, day-aligned on close_time with today Partial.
func (r *ExplorerReader) NetworkThroughput(ctx context.Context, windowDays int) ([]ThroughputBucket, error) {
	if windowDays <= 0 || windowDays > 365 {
		windowDays = 30
	}
	// +2 days of slack so the pruning hint always covers the aligned window.
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
		-- every sum(*_count) double-count until a background merge.
		-- FINAL is a no-op once merged. The ledger_seq predicate keeps
		-- the FINAL bounded to the recent partitions (pruning hint ONLY); the
		-- close_time predicate is the authoritative, day-aligned boundary.
		FROM stellar.ledgers FINAL
		WHERE ledger_seq > (SELECT max(ledger_seq) FROM stellar.ledgers) - ?
		  -- Day window anchored to the DATA's tip close_time, not now('UTC'):
		  -- deterministic for any two regions ingesting the same chain
		  -- (ADR-0050 §4: byte-identical closed buckets) and equal to wall clock
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
	// Only the newest bucket (holding the tip) can be incomplete; earlier buckets are whole
	// days. Data-derived (rows are day ASC), so replicas with the same tip agree on Partial
	// near a UTC day boundary, unlike a wall-clock comparison.
	if len(out) > 0 {
		out[len(out)-1].Partial = true
	}
	return out, rows.Err()
}

// OperationsByLedger returns a ledger's operations ordered by (tx_index, op_index); ledger-scoped, so partition-pruned.
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
	fee_charged, max_fee, operation_count, successful, result_code, memo_type, memo,
	inner_tx_hash, fee_account, fee_bump_fee, inner_result_code`

// ExplorerCursor is a composite keyset position for descending listings with many rows per ledger (A/B carry the 2nd/3rd ORDER BY columns); a ledger-only cursor would drop the rest of a straddling ledger, and Ledger==0 means first page.
type ExplorerCursor struct {
	Ledger uint32 // ledger_seq — primary sort key (DESC)
	A      uint32 // 2nd sort col: tx_index (txs/ops) | op_index (events)
	B      uint32 // 3rd sort col: op_index (ops) | event_index (events); unused for txs
}

// IsSet reports whether this is a continuation page.
func (c ExplorerCursor) IsSet() bool { return c.Ledger > 0 }

// ContractEventsCursor is the keyset position for ContractEventsRecent, carrying the FULL row identity (ledger_seq, tx_hash, op_index, event_index) since the 3-part tuple is not unique.
type ContractEventsCursor struct {
	Ledger     uint32 // ledger_seq — primary sort key (DESC)
	TxHash     string // 64-char hex — tie-break within a ledger (DESC, lexicographic)
	OpIndex    uint32
	EventIndex uint32
}

// IsSet reports whether the cursor points past the newest row.
func (c ContractEventsCursor) IsSet() bool { return c.Ledger > 0 }

// Account listings resolve a page keyset from two account-keyed arms, merge it in Go, then hydrate the wide columns once over the
// surviving keys. Sourced arm: stellar.ops_by_source (tx-MV rows carry the sentinel op_index). Participant arm:
// stellar.operation_participants, limited to transactions that succeeded or that the account itself sourced (participantKeys). Each arm
// is a primary-key-prefix range, so the wide tables see only point lookups. mergeKeysDesc drops cross-arm duplicates BEFORE cutting to
// `limit`: a duplicate eating a slot would serve a short page read as end of history.

// sourcedTxKeysExactQuery is the sourced tx arm's LIMIT 1 BY form: exact but O(account history), so it runs only when the windowed read
// cannot prove a full page. The leading `ledger_seq <= ?` is redundant but required: KeyCondition does not prune on a tuple comparison.
func sourcedTxKeysExactQuery(hasCursor bool) string {
	cursorClause := ""
	if hasCursor {
		cursorClause = ` AND ledger_seq <= ? AND (ledger_seq, tx_index) < (?, ?)`
	}
	return `SELECT ledger_seq, tx_index FROM stellar.ops_by_source
		WHERE source_account = ?` + cursorClause + `
		ORDER BY ledger_seq DESC, tx_index DESC LIMIT 1 BY ledger_seq, tx_index LIMIT ?` + explorerScanSettings
}

// AccountTransactions returns transactions involving an account (sourced, or SUCCESSFUL as a non-source participant, since failed txs still write participant rows), newest first; `resume` is the next cursor when the query budget ran out mid-page.
func (r *ExplorerReader) AccountTransactions(ctx context.Context, account string, limit int, cur ExplorerCursor) (_ []TxSummary, resume ExplorerCursor, _ error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if !r.opsBySourceAvailable(ctx) {
		return nil, resume, errOpsBySourceMissing
	}
	sourced, err := r.sourcedTxKeys(ctx, account, limit, cur)
	if err != nil {
		return nil, resume, fmt.Errorf("clickhouse: account %s txs: %w", account, err)
	}
	var from *accountTxKey
	if cur.IsSet() {
		from = &accountTxKey{cur.Ledger, cur.A}
	}
	ceiling := uint32(math.MaxUint32)
	if from != nil {
		ceiling = from.ledger
	}
	fixed, floor, hasFloor := r.accountParticipantPartitions(ctx, account, ceiling)
	part, frontier, err := newestFirstParticipantKeys(ctx, r.conn, account, limit, txParticipantArm, fixed, nil, from, floor, hasFloor)
	if err != nil {
		return nil, resume, fmt.Errorf("clickhouse: account %s txs: %w", account, err)
	}
	if frontier != nil {
		sourced = notOlderThan(sourced, *frontier, accountTxKey.after)
	}
	keys := mergeKeysDesc(append(sourced, part...), limit, accountTxKey.after)
	if frontier != nil && len(keys) < limit {
		resume = ExplorerCursor{Ledger: frontier.ledger, A: frontier.txIndex}
	}
	if len(keys) == 0 {
		return nil, resume, nil
	}
	// FINAL: ingested_at has one-second resolution, so a same-second re-derive can leave two
	// RMT parts a bare SELECT cannot order (see txByLedgerAndHash). Cheap: a bounded IN over PK points.
	q := `SELECT ` + txCols + ` FROM stellar.transactions FINAL
		WHERE (ledger_seq, tx_index) IN (` + tupleList(keys, func(k accountTxKey) []uint32 { return []uint32{k.ledger, k.txIndex} }) + `)
		ORDER BY ledger_seq DESC, tx_index DESC LIMIT ?` + explorerScanSettings + `, do_not_merge_across_partitions_select_final = 1`
	rows, err := r.conn.Query(ctx, q, limit)
	if err != nil {
		return nil, resume, fmt.Errorf("clickhouse: account %s txs: %w", account, err)
	}
	defer func() { _ = rows.Close() }()
	txs, err := scanTxSummaries(rows)
	return txs, resume, err
}

// accountTxKey is one transaction's (ledger_seq, tx_index) listing key.
type accountTxKey struct{ ledger, txIndex uint32 }

func (k accountTxKey) after(o accountTxKey) bool {
	return k.ledger > o.ledger || (k.ledger == o.ledger && k.txIndex > o.txIndex)
}

func scanTxKey(rows driver.Rows) (accountTxKey, error) {
	var k accountTxKey
	err := rows.Scan(&k.ledger, &k.txIndex)
	return k, err
}

// sourcedTxKeys is the sourced arm's exact top `limit` keys: a bounded window read in sort-key order, widened up to windowWidenings
// times, falling back to sourcedTxKeysExactQuery when no window can prove a full page.
func (r *ExplorerReader) sourcedTxKeys(ctx context.Context, account string, limit int, cur ExplorerCursor) ([]accountTxKey, error) {
	window := windowRows(limit, windowFactorTxArm)
	cursorClause := ""
	var cursorArgs []any
	if cur.IsSet() {
		cursorClause = ` AND ledger_seq <= ? AND (ledger_seq, tx_index) < (?, ?)`
		cursorArgs = []any{cur.Ledger, cur.Ledger, cur.A}
	}
	args := append([]any{account}, cursorArgs...)
	for i := 0; i <= windowWidenings; i, window = i+1, window*windowGrowth {
		keys, ok, err := windowedKeyRead(ctx, r.conn,
			`SELECT ledger_seq, tx_index FROM stellar.ops_by_source WHERE source_account = ?`+cursorClause+
				` ORDER BY ledger_seq DESC, tx_index DESC LIMIT ?`+explorerScanSettings,
			args, window, limit, scanTxKey)
		if err != nil || ok {
			return keys, err
		}
	}
	exactFallback("sourced_tx_keys")
	args = append(append([]any{account}, cursorArgs...), limit)
	return queryKeys(ctx, r.conn, sourcedTxKeysExactQuery(cur.IsSet()), args, scanTxKey)
}

// queryKeys runs a key query and scans every row.
func queryKeys[K any](ctx context.Context, conn driver.Conn, q string, args []any, scan func(driver.Rows) (K, error)) ([]K, error) {
	rows, err := conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: key read: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []K
	for rows.Next() {
		k, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("clickhouse: scan key: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: key read: %w", err)
	}
	return out, nil
}

// windowedKeyRead runs one windowed key read (q's final `LIMIT ?` binds to window, after args) and collapses adjacent duplicate keys;
// ok=false when the window cannot prove `limit` distinct keys.
func windowedKeyRead[K comparable](ctx context.Context, conn driver.Conn, q string, args []any, window, limit int,
	scan func(driver.Rows) (K, error),
) ([]K, bool, error) {
	raw, err := queryKeys(ctx, conn, q, append(args[:len(args):len(args)], window), scan)
	if err != nil {
		return nil, false, err
	}
	keys, ok := dedupWindow(raw, window, limit, func(k K) K { return k }, nil)
	return keys, ok, nil
}

// mergeKeysDesc concatenates arm key lists, orders newest-first, drops cross-arm duplicates, keeps the first n.
func mergeKeysDesc[K comparable](keys []K, n int, after func(a, b K) bool) []K {
	sort.SliceStable(keys, func(i, j int) bool { return after(keys[i], keys[j]) })
	out := keys[:0:0]
	for i, k := range keys {
		if i > 0 && keys[i-1] == k {
			continue
		}
		out = append(out, k)
		if len(out) == n {
			break
		}
	}
	return out
}

// notOlderThan keeps keys at or newer than f: past an arm's scan frontier the other
// arm's keys cannot be ordered against keys not yet read.
func notOlderThan[K comparable](keys []K, f K, after func(a, b K) bool) []K {
	out := keys[:0:0]
	for _, k := range keys {
		if !after(f, k) {
			out = append(out, k)
		}
	}
	return out
}

// participantArm describes one listing's stellar.operation_participants key.
type participantArm[K comparable] struct {
	cols, order string
	older       func(K) (string, []any) // strictly-older-than-k predicate (keyset cursor)
	scan        func(driver.Rows) (K, error)
	tx          func(K) accountTxKey
}

var (
	txParticipantArm = participantArm[accountTxKey]{
		cols:  "ledger_seq, tx_index",
		order: "ledger_seq DESC, tx_index DESC",
		older: func(k accountTxKey) (string, []any) {
			return ` AND ledger_seq <= ? AND (ledger_seq, tx_index) < (?, ?)`, []any{k.ledger, k.ledger, k.txIndex}
		},
		scan: scanTxKey,
		tx:   func(k accountTxKey) accountTxKey { return k },
	}
	opParticipantArm = participantArm[accountOpKey]{
		cols:  "ledger_seq, tx_index, op_index",
		order: "ledger_seq DESC, tx_index DESC, op_index DESC",
		older: func(k accountOpKey) (string, []any) {
			return ` AND ledger_seq <= ? AND (ledger_seq, tx_index, op_index) < (?, ?, ?)`,
				[]any{k.ledger, k.ledger, k.txIndex, k.opIndex}
		},
		scan: scanOpKey,
		tx:   func(k accountOpKey) accountTxKey { return accountTxKey{k.ledger, k.txIndex} },
	}
)

// participantQueryBudget caps the queries one participantKeys call issues (a page
// normally costs two), bounding what planted failed txs can make a page cost.
const participantQueryBudget = 16

// participantKeys returns the account's newest `limit` participant keys older than `from` whose tx is visible (visibleParticipantKeys);
// filtering inside the arm avoids short pages at hydration. If the budget runs out, frontier is the oldest key scanned: keys are exact at
// or newer than it and the caller resumes strictly below. `fixed` carries predicates every read must keep (the activity-watermark bound).
func participantKeys[K comparable](ctx context.Context, conn driver.Conn, account string, limit int,
	arm participantArm[K], fixed string, fixedArgs []any, from *K,
) (keys []K, frontier *K, err error) {
	var out []K
	window := windowRows(limit, windowFactorKeys)
	for budget := participantQueryBudget; ; window *= 2 {
		// The read, the need-sized first lookup and the further lookups must fit the budget.
		window = min(window, (budget-2)*visibilityChunk)
		if window <= 0 {
			return out, from, nil
		}
		q := `SELECT ` + arm.cols + ` FROM stellar.operation_participants WHERE account = ?` + fixed
		args := append([]any{account}, fixedArgs...)
		if from != nil {
			c, a := arm.older(*from)
			q += c
			args = append(args, a...)
		}
		raw, err := queryKeys(ctx, conn, q+` ORDER BY `+arm.order+` LIMIT ?`+explorerScanSettings, append(args, window), arm.scan)
		if err != nil {
			return nil, nil, err
		}
		// Adjacent rows repeat a key (several ops of one tx, un-merged RMT parts); the strict cursor skips repeats past the window.
		var keys []K
		for i, k := range raw {
			if i == 0 || raw[i-1] != k {
				keys = append(keys, k)
			}
		}
		kept, lookups, err := visibleParticipantKeys(ctx, conn, account, keys, arm.tx, limit-len(out))
		if err != nil {
			return nil, nil, err
		}
		budget -= 1 + lookups
		out = append(out, kept[:min(len(kept), limit-len(out))]...)
		if len(out) >= limit || len(raw) < window {
			return out, nil, nil
		}
		from = &raw[len(raw)-1]
	}
}

// newestFirstParticipantKeys runs participantKeys over the newest partition first (newestPartitionFloor) and widens only when that
// cannot fill the page; a run-out budget's frontier lies above the floor, so that answer stands too.
func newestFirstParticipantKeys[K comparable](ctx context.Context, conn driver.Conn, account string, limit int,
	arm participantArm[K], fixed string, fixedArgs []any, from *K, floor uint32, hasFloor bool,
) ([]K, *K, error) {
	if hasFloor {
		bounded := fixed + ` AND ledger_seq >= ` + strconv.FormatUint(uint64(floor), 10)
		keys, frontier, err := participantKeys(ctx, conn, account, limit, arm, bounded, fixedArgs, from)
		if err != nil || len(keys) >= limit || frontier != nil {
			return keys, frontier, err
		}
	}
	return participantKeys(ctx, conn, account, limit, arm, fixed, fixedArgs, from)
}

// visibleTxPredicate is the stellar.transactions filter a participant row's tx must pass; its placeholder binds the listed account.
const visibleTxPredicate = `(successful = 1 OR source_account = ?)`

// visibilityChunk is fixed, never derived from free slots: a nearly full page must not turn a run of failed txs into one query per key.
const visibilityChunk = 500

// visibleParticipantKeys keeps, in order, keys whose tx succeeded or was sourced by the account (its own failed txs stay in its history),
// via point lookups on the transactions primary key; it reports how many it ran. The first covers only `need` txs, later ones
// visibilityChunk. No FINAL: successful and source_account are ledger facts, identical across un-merged versions of a key.
func visibleParticipantKeys[K comparable](ctx context.Context, conn driver.Conn, account string, keys []K, tx func(K) accountTxKey, need int) (_ []K, lookups int, _ error) {
	seen := make(map[accountTxKey]struct{}, len(keys))
	var txs []accountTxKey
	for _, k := range keys {
		t := tx(k)
		if _, dup := seen[t]; !dup {
			seen[t] = struct{}{}
			txs = append(txs, t)
		}
	}
	visible := make(map[accountTxKey]struct{}, len(txs))
	chunk := max(need, 1)
	for len(txs) > 0 {
		n := min(len(txs), chunk)
		chunk = visibilityChunk
		q := `SELECT ledger_seq, tx_index FROM stellar.transactions
		WHERE (ledger_seq, tx_index) IN (` + tupleList(txs[:n], func(t accountTxKey) []uint32 { return []uint32{t.ledger, t.txIndex} }) + `)
		  AND ` + visibleTxPredicate + explorerScanSettings
		ok, err := queryKeys(ctx, conn, q, []any{account}, scanTxKey)
		if err != nil {
			return nil, lookups, err
		}
		lookups++
		for _, k := range ok {
			visible[k] = struct{}{}
		}
		txs = txs[n:]
		nVisible := 0
		for _, k := range keys {
			if _, v := visible[tx(k)]; v {
				nVisible++
			}
		}
		if nVisible >= need {
			break
		}
	}
	out := keys[:0:0]
	for _, k := range keys {
		if _, v := visible[tx(k)]; v {
			out = append(out, k)
		}
	}
	return out, lookups, nil
}

// sourcedOpKeysExactQuery is the sourced op arm's LIMIT 1 BY form (exact, O(account history)); sentinel op_index rows (tx-sourced) are
// excluded. hasBound adds `AND ledger_seq <= ?`, the account's activity watermark (accountActivityWatermark), so an idle account's reverse
// read starts at its last activity. Pass ONLY a watermark-derived bound: an under-estimate HIDES history.
func sourcedOpKeysExactQuery(hasCursor, hasBound bool) string {
	clauses := ""
	if hasBound {
		clauses += ` AND ledger_seq <= ?`
	}
	if hasCursor {
		clauses += ` AND ledger_seq <= ? AND (ledger_seq, tx_index, op_index) < (?, ?, ?)`
	}
	return `SELECT ledger_seq, tx_index, op_index FROM stellar.ops_by_source
		WHERE source_account = ? AND op_index != 4294967295` + clauses + `
		ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC
		LIMIT 1 BY ledger_seq, tx_index, op_index LIMIT ?` + explorerScanSettings
}

// AccountOperations returns operations involving an account (effective op source, or non-source participant of a succeeded/self-sourced tx), newest first, keyset-paged; resume as in AccountTransactions.
func (r *ExplorerReader) AccountOperations(ctx context.Context, account string, limit int, cur ExplorerCursor) (_ []OpRow, resume ExplorerCursor, _ error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if !r.opsBySourceAvailable(ctx) {
		return nil, resume, errOpsBySourceMissing
	}
	// No watermark (table absent, backfill pending, or no row for the account): unbounded read.
	bound, hasBound := r.accountActivityWatermark(ctx, account)
	sourced, err := r.sourcedOpKeys(ctx, account, limit, cur, bound, hasBound)
	if err != nil {
		return nil, resume, fmt.Errorf("clickhouse: account %s ops: %w", account, err)
	}
	ceiling := uint32(math.MaxUint32)
	if cur.IsSet() {
		ceiling = cur.Ledger
	}
	if hasBound {
		ceiling = min(ceiling, bound)
	}
	fixed, floor, hasFloor := r.accountParticipantPartitions(ctx, account, ceiling)
	var fixedArgs []any
	if hasBound {
		fixed, fixedArgs = fixed+` AND ledger_seq <= ?`, []any{bound}
	}
	var from *accountOpKey
	if cur.IsSet() {
		from = &accountOpKey{cur.Ledger, cur.A, cur.B}
	}
	part, frontier, err := newestFirstParticipantKeys(ctx, r.conn, account, limit, opParticipantArm, fixed, fixedArgs, from, floor, hasFloor)
	if err != nil {
		return nil, resume, fmt.Errorf("clickhouse: account %s ops: %w", account, err)
	}
	if frontier != nil {
		sourced = notOlderThan(sourced, *frontier, accountOpKey.after)
	}
	keys := mergeKeysDesc(append(sourced, part...), limit, accountOpKey.after)
	if frontier != nil && len(keys) < limit {
		resume = ExplorerCursor{Ledger: frontier.ledger, A: frontier.txIndex, B: frontier.opIndex}
	}
	if len(keys) == 0 {
		return nil, resume, nil
	}
	// FINAL for AccountTransactions' tie-break reason; opCols carries body_xdr, read only here, once.
	q := `SELECT ` + opCols + ` FROM stellar.operations FINAL
		WHERE (ledger_seq, tx_index, op_index) IN (` + tupleList(keys, func(k accountOpKey) []uint32 { return []uint32{k.ledger, k.txIndex, k.opIndex} }) + `)
		ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC LIMIT ?` + explorerScanSettings + `, do_not_merge_across_partitions_select_final = 1`
	rows, err := r.conn.Query(ctx, q, limit)
	if err != nil {
		return nil, resume, fmt.Errorf("clickhouse: account %s ops: %w", account, err)
	}
	defer func() { _ = rows.Close() }()
	ops, err := scanOps(rows)
	return ops, resume, err
}

// accountOpKey is one operation's (ledger_seq, tx_index, op_index) key.
type accountOpKey struct{ ledger, txIndex, opIndex uint32 }

func (k accountOpKey) after(o accountOpKey) bool {
	if k.ledger != o.ledger {
		return k.ledger > o.ledger
	}
	if k.txIndex != o.txIndex {
		return k.txIndex > o.txIndex
	}
	return k.opIndex > o.opIndex
}

func scanOpKey(rows driver.Rows) (accountOpKey, error) {
	var k accountOpKey
	err := rows.Scan(&k.ledger, &k.txIndex, &k.opIndex)
	return k, err
}

// sourcedOpKeys is sourcedTxKeys for operations.
func (r *ExplorerReader) sourcedOpKeys(ctx context.Context, account string, limit int, cur ExplorerCursor, bound uint32, hasBound bool) ([]accountOpKey, error) {
	window := windowRows(limit, windowFactorKeys)
	clauses := ""
	var extra []any
	if hasBound {
		clauses += ` AND ledger_seq <= ?`
		extra = append(extra, bound)
	}
	if cur.IsSet() {
		clauses += ` AND ledger_seq <= ? AND (ledger_seq, tx_index, op_index) < (?, ?, ?)`
		extra = append(extra, cur.Ledger, cur.Ledger, cur.A, cur.B)
	}
	args := append([]any{account}, extra...)
	keys, ok, err := windowedKeyRead(ctx, r.conn,
		`SELECT ledger_seq, tx_index, op_index FROM stellar.ops_by_source WHERE source_account = ? AND op_index != 4294967295`+clauses+
			` ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC LIMIT ?`+explorerScanSettings,
		args, window, limit, scanOpKey)
	if err != nil || ok {
		return keys, err
	}
	exactFallback("sourced_op_keys")
	args = append(append([]any{account}, extra...), limit)
	return queryKeys(ctx, r.conn, sourcedOpKeysExactQuery(cur.IsSet(), hasBound), args, scanOpKey)
}

// accountOpTypeCountsQuery is AccountOperationTypeCounts' SQL: the SAME two arms as AccountOperations, UNION'd, NOT `source_account = ? OR
// ... IN (...)` (an OR with a subquery defeats index use); the arms never overlap, so the outer sum() counts each op once. uniqExact over
// the 3-column primary key, not count() (un-merged duplicates inflate) and not FINAL (O(table) merge). The spill pair in
// explorerScanSettings covers whale accounts.
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
		    AND (ledger_seq, tx_index) IN (
		        SELECT ledger_seq, tx_index FROM stellar.transactions
		         WHERE (ledger_seq, tx_index) IN (
		               SELECT ledger_seq, tx_index FROM stellar.operation_participants WHERE account = ?)
		           AND ` + visibleTxPredicate + `)
		  GROUP BY op_type)
	) GROUP BY op_type ORDER BY n DESC` + explorerScanSettings

// accountOpTypeCountsIndexedQuery reads the same counts from stellar.account_op_types, keyed on account and holding only visible rows.
// count() FINAL collapses the RMT on its full key; uniqExact over a whale's rows breaks the 8 GB cap. Partitions are disjoint ledger
// ranges, so FINAL need not merge across them.
const accountOpTypeCountsIndexedQuery = `SELECT op_type, toInt64(count()) AS n
		FROM stellar.account_op_types FINAL
		WHERE account = ?
		GROUP BY op_type ORDER BY n DESC` + explorerScanSettings + `, do_not_merge_across_partitions_select_final = 1`

// AccountOperationTypeCounts returns all-time per-op-type counts of operations involving an account, by count desc. Without the
// account_op_types coverage marker it is a whole-history join, so run it under a detached stale-while-revalidate budget.
func (r *ExplorerReader) AccountOperationTypeCounts(ctx context.Context, account string) ([]OpTypeCount, error) {
	q, args := accountOpTypeCountsIndexedQuery, []any{account}
	if r.accountOpTypesCovered(ctx) {
		if plan, ok := r.planFor(ctx, OpTypeCountsRollup); ok {
			q, args = opTypeCountsRollupQuery(plan), []any{account, account}
		}
	} else {
		if !r.opsBySourceAvailable(ctx) {
			return nil, errOpsBySourceMissing
		}
		q, args = accountOpTypeCountsQuery, []any{account, account, account, account}
	}
	rows, err := r.conn.Query(ctx, q, args...)
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

// TransactionByHash looks up one transaction by hex hash; a miss is authoritative absence only when tx_hash_index is covered (completion marker required), otherwise the slow bloom scan runs.
func (r *ExplorerReader) TransactionByHash(ctx context.Context, hash string) (TxSummary, bool, error) {
	if r.txHashIndexAvailable(ctx) && r.txHashIndexCovered(ctx) {
		tx, found, indexHit, err := r.txByHashIndexed(ctx, hash)
		switch {
		case err == nil && found:
			return tx, true, nil
		case err == nil && !indexHit:
			// The index had no row: authoritative absence, since the coverage marker vouches for
			// genesis to tip. Falling through to the scan would make every garbage hash a bloom probe over the full table.
			return TxSummary{}, false, nil
		}
		// Index-path error, or index/base inconsistency (index row, base row missing): the scan is the availability-preserving answer.
	}
	return r.txByHashScan(ctx, hash)
}

// txHashIndexAvailable reports whether tx_hash_index exists AND holds a row: a per-hash miss is authoritative not-found, so an
// existing-but-EMPTY index (MV dropped or TRUNCATEd) would 404 every real hash. "Holds rows" is a lease ([schemaProbeLease]). This catches
// total loss; the parity check catches partial divergence.
func (r *ExplorerReader) txHashIndexAvailable(ctx context.Context) bool {
	return r.probeSchema(ctx, &r.txIndexProbe,
		`SELECT ledger_seq FROM stellar.tx_hash_index LIMIT 1`, true)
}

// txHashIndexCovered reports whether ch-txindex-backfill recorded a completed genesis-to-tip run;
// without it a non-empty index would turn historical hashes into wrong 404s.
func (r *ExplorerReader) txHashIndexCovered(ctx context.Context) bool {
	return r.probeSchema(ctx, &r.txCoverageProbe,
		`SELECT covered_to FROM stellar.tx_hash_index_coverage LIMIT 1`, true)
}

// accountOpTypesCovered reports whether the account_op_types backfill was verified and marked; until then the table misses history.
func (r *ExplorerReader) accountOpTypesCovered(ctx context.Context) bool {
	return r.probeSchema(ctx, &r.opTypesCoverageProbe,
		`SELECT covered_to FROM stellar.account_op_types_coverage LIMIT 1`, true)
}

// contractLedgersIndexAvailable reports whether contract_active_ledgers is USABLE (exists, non-empty), not that it covers a contract:
// without the genesis watermark treat an empty PER-CONTRACT walk as "unknown, fall back", never "no rows" (see ContractEventsRecent).
func (r *ExplorerReader) contractLedgersIndexAvailable(ctx context.Context) bool {
	return r.probeSchema(ctx, &r.contractLedgersProbe,
		`SELECT ledger_seq FROM stellar.contract_active_ledgers LIMIT 1`, true)
}

// instanceChangesIndexAvailable reports whether contract_instance_changes is USABLE (exists, non-empty).
func (r *ExplorerReader) instanceChangesIndexAvailable(ctx context.Context) bool {
	return r.probeSchema(ctx, &r.instanceChangesProbe,
		`SELECT ledger_seq FROM stellar.contract_instance_changes LIMIT 1`, true)
}

// instanceChangesTxKeyed reports whether contract_instance_changes carries tx_hash + intra_ledger_seq (see instanceKeyProbe).
func (r *ExplorerReader) instanceChangesTxKeyed(ctx context.Context) bool {
	return r.probeSchema(ctx, &r.instanceKeyProbe,
		`SELECT tx_hash, intra_ledger_seq FROM stellar.contract_instance_changes LIMIT 1`, false)
}

// censusAvailable reports whether contracts_census_daily is USABLE (exists, non-empty).
func (r *ExplorerReader) censusAvailable(ctx context.Context) bool {
	return r.probeSchema(ctx, &r.censusProbe,
		`SELECT day FROM stellar.contracts_census_daily LIMIT 1`, true)
}

// ContractActivitySummary is the per-contract liveness card: lifetime bounds plus a daily series, all key-pruned reads off contract_active_ledgers.
type ContractActivitySummary struct {
	FirstSeen          time.Time
	LastSeen           time.Time
	ActiveLedgersTotal uint64
	Daily              []ContractActivityDay
}

// ContractActivityDay is one day of the series; ActiveLedgers is uniqExact on ledger_seq so un-merged ReplacingMergeTree duplicates do not inflate it.
type ContractActivityDay struct {
	Date          time.Time
	ActiveLedgers uint64
}

// ContractActivitySummaryFor reads the contract's activity card; ok=false when the index is not usable, so callers omit the card rather than fabricate one.
// sinceLedger > 0 must be at or below the first ledger of the series window: close_time is not in the key, so only it prunes the series read.
func (r *ExplorerReader) ContractActivitySummaryFor(ctx context.Context, contractID string, days int, sinceLedger uint32) (ContractActivitySummary, bool, error) {
	if !r.contractLedgersIndexAvailable(ctx) {
		return ContractActivitySummary{}, false, nil
	}
	if days <= 0 || days > 365 {
		days = 30
	}
	b, err := r.contractActivityBounds(ctx, contractID)
	if err != nil {
		return ContractActivitySummary{}, false, err
	}
	s := ContractActivitySummary{FirstSeen: b.first, LastSeen: b.last, ActiveLedgersTotal: b.total}
	if s.ActiveLedgersTotal == 0 {
		return s, true, nil
	}
	rows, err := r.conn.Query(ctx, `
		SELECT toDate(close_time) AS d, toUInt64(uniqExact(ledger_seq))
		FROM stellar.contract_active_ledgers
		WHERE contract_id = ? AND ledger_seq >= ? AND close_time >= now() - INTERVAL ? DAY
		GROUP BY d ORDER BY d`, contractID, sinceLedger, days)
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

// contractActiveLedgers returns the contract's newest active ledgers (descending), at most n, optionally <= before (inclusive: the
// boundary ledger can hold events older than the cursor tuple). Primary-key reverse walk; DISTINCT collapses un-merged RMT duplicates.
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

// errOpsBySourceMissing: the sourced-history projection is not provisioned or populated.
// Fail-loud by design: a silent fallback or empty arm would restore the slow read or hide the account's own history.
var errOpsBySourceMissing = errors.New(
	"clickhouse: stellar.ops_by_source does not exist or is empty — apply deploy/clickhouse/ops_by_source.sql " +
		"(table + both MVs, then its Step-2 windowed backfills) before serving account history; " +
		"there is no scan fallback")

// opsBySourceAvailable reports whether ops_by_source exists AND holds rows: an empty
// sourced arm reads as "sourced nothing", so an unfed or TRUNCATEd projection must refuse.
func (r *ExplorerReader) opsBySourceAvailable(ctx context.Context) bool {
	ok := r.probeSchema(ctx, &r.opsBySourceProbe,
		`SELECT ledger_seq FROM stellar.ops_by_source LIMIT 1`, true)
	if ok {
		r.probeSchema(ctx, &r.opsBySourceProofProbe, opsBySourceProofProbeQuery, true)
	}
	return ok
}

const opsBySourceProofProbeQuery = `SELECT thru_ledger FROM stellar.entry_history_watermark WHERE name = '` + OpsBySourceProofName + `' AND thru_ledger > 0 LIMIT 1`

// accountActivityAvailable reports whether stellar.account_activity is usable.
// requireRows: an empty watermark bounds nothing, so it reads as unavailable and re-probes.
func (r *ExplorerReader) accountActivityAvailable(ctx context.Context) bool {
	return r.probeSchema(ctx, &r.accountActivityProbe,
		`SELECT last_ledger FROM stellar.account_activity LIMIT 1`, true)
}

// accountActivityWatermark returns the account's last-active ledger, the upper bound for AccountOperations' reverse resolves.
// max(last_ledger), never a bare row or FINAL: the table is ReplacingMergeTree(last_ledger) fed by three MVs, so only the MAX is safe (a
// lower row would HIDE history). ok=false degrades to the unbounded scan: a missing watermark may cost performance, never rows.
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

// ledgerEntriesVersioned reports whether ledger_entries_current has a `version` column;
// false means the older schema, and callers use ledger_seq as the version key.
func (r *ExplorerReader) ledgerEntriesVersioned(ctx context.Context) bool {
	return r.probeSchema(ctx, &r.lecVersionProbe,
		`SELECT version FROM stellar.ledger_entries_current LIMIT 1`, false)
}

// probeSchema answers "does this schema object exist" and caches only a definitive answer, so a transient error cannot latch a probe false
// for the process lifetime: no error caches true; an exception in [schemaAbsentCodes] caches false (unless already seen); anything else
// caches nothing and re-probes wait [schemaProbeRetryAfter]. requireRows tightens "exists" to "exists AND non-empty" for probes that derive
// authority (an index miss is a 404): rows renew the [schemaProbeLease]; empty revokes at once; schema-absent revokes without latching; no
// answer honours the old verdict for [schemaProbeStaleLeases]. The query runs OUTSIDE the mutex (not context-aware); concurrent first
// callers may each probe.
func (r *ExplorerReader) probeSchema(ctx context.Context, p *schemaProbe, query string, requireRows bool) bool {
	if verdict, ok := p.cached(requireRows); ok {
		return verdict
	}

	rows, err := r.conn.Query(ctx, query)
	empty := false
	if err == nil {
		if requireRows {
			empty = !rows.Next()
			if rerr := rows.Err(); rerr != nil {
				// Row iteration died: no verdict on emptiness either.
				err, empty = rerr, false
			}
		}
		_ = rows.Close()
	}
	verdict := p.record(err, empty, requireRows)
	p.observe(err, verdict)
	return verdict
}

// observe exports one probe outcome. Only an ANSWER moves the gauge; a non-answer is counted and the last answer stands.
func (p *schemaProbe) observe(err error, verdict bool) {
	if err != nil && !isSchemaAbsent(err) {
		obs.CHSchemaProbeUnansweredTotal.WithLabelValues(p.name).Inc()
		return
	}
	present := 0.0
	if verdict {
		present = 1
	}
	obs.CHSchemaProbePresent.WithLabelValues(p.name).Set(present)
}

// txByHashIndexed is the two-step fast path: hash to ledger_seq via the ordered index, then the ledger-scoped read. indexHit=false means
// the INDEX had no row (authoritative absence); true with !found means it pointed at a ledger with no base row (inconsistency the caller
// may resolve via the scan).
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

// txByLedgerAndHash reads the authoritative transactions row for a KNOWN (ledger_seq, tx_hash). FINAL, not `ORDER BY ingested_at DESC
// LIMIT 1`: ingested_at is 1 s DateTime, so a re-ingest within one second TIES on the version column and a plain SELECT could serve the
// STALE row. FINAL stays cheap: `ledger_seq = ?` is a partition and primary-key prefix predicate. A hash (outer or fee-bump inner) is
// globally unique.
func (r *ExplorerReader) txByLedgerAndHash(ctx context.Context, seq uint32, hash string) (TxSummary, bool, error) {
	q := `SELECT ` + txCols + ` FROM stellar.transactions FINAL
		WHERE ledger_seq = ? AND (tx_hash = ? OR inner_tx_hash = ?)`
	rows, err := r.conn.Query(ctx, q, seq, hash, hash)
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

// txByHashScan is the lookup without the hash index, in two steps. 1. Find WHICH ledger holds the hash via the tx_hash bloom skip-index;
// NOT FINAL (it would defeat the skip-index with no ledger to bound it); only ledger_seq is read, safe from duplicates since a tx executes
// in one ledger. `ORDER BY ingested_at DESC` only makes a hash recorded against two ledgers resolve to the newest. 2. Read the row via
// txByLedgerAndHash. found=false when step 1 is empty.
func (r *ExplorerReader) txByHashScan(ctx context.Context, hash string) (TxSummary, bool, error) {
	for _, seqQ := range txSeqScanQueries {
		seq, ok, err := r.txSeqByScan(ctx, seqQ, hash)
		if err != nil {
			return TxSummary{}, false, err
		}
		if ok {
			return r.txByLedgerAndHash(ctx, seq, hash)
		}
	}
	return TxSummary{}, false, nil
}

// txSeqScanQueries are step 1's probes: the outer hash, then a fee bump's inner hash
// (own skip-index idx_tx_inner_hash; one OR-ed predicate would prune on neither).
var txSeqScanQueries = [...]string{
	`SELECT ledger_seq FROM stellar.transactions WHERE tx_hash = ? ORDER BY ingested_at DESC LIMIT 1`,
	`SELECT ledger_seq FROM stellar.transactions WHERE inner_tx_hash = ? ORDER BY ingested_at DESC LIMIT 1`,
}

func (r *ExplorerReader) txSeqByScan(ctx context.Context, seqQ, hash string) (uint32, bool, error) {
	rows, err := r.conn.Query(ctx, seqQ, hash)
	if err != nil {
		return 0, false, fmt.Errorf("clickhouse: tx %s: %w", hash, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return 0, false, rows.Err()
	}
	var seq uint32
	if err := rows.Scan(&seq); err != nil {
		return 0, false, fmt.Errorf("clickhouse: scan tx %s ledger: %w", hash, err)
	}
	return seq, true, nil
}

// OperationsByTx returns a transaction's operations, ledger-scoped; FINAL is cheap here (one partition, ledger_seq prefix) and stops an un-merged duplicate showing an op twice.
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

// OperationResultsByTx returns op_index to result for a transaction: a primary-key point lookup on (ledger_seq, tx_hash, op_index).
func (r *ExplorerReader) OperationResultsByTx(ctx context.Context, seq uint32, hash string) (map[uint32]OpResult, error) {
	const q = `SELECT op_index, result_code, result_xdr FROM stellar.operation_results
		WHERE ledger_seq = ? AND tx_hash = ?`
	rows, err := r.conn.Query(ctx, q, seq, hash)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: tx %s op results: %w", hash, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[uint32]OpResult{}
	for rows.Next() {
		var idx uint32
		var res OpResult
		if err := rows.Scan(&idx, &res.Code, &res.ResultXDR); err != nil {
			return nil, fmt.Errorf("clickhouse: scan op result: %w", err)
		}
		out[idx] = res
	}
	return out, rows.Err()
}

// OpResult is one operation_results row: the OUTER code plus the base64 OperationResult whose inner code says why an op_inner operation failed.
type OpResult struct {
	Code      int32
	ResultXDR string
}

// TxOutcome is a transaction's applied verdict and result code, stamped on operation list views so failed transactions' operations are marked FAILED.
type TxOutcome struct {
	Successful bool
	ResultCode int32
}

// TxOutcomesByHash batch-reads verdict + result code by tx_hash. The predicate is the EXACT SET of the page's ledgers, never a [lo,hi]
// range (a sparse account's page can straddle millions of ledgers, leaving the high-false-positive tx_hash bloom as the only filter);
// lossless, as an op's parent tx is in its own ledger. FINAL stays (duplicates and ingested_at ties, see txByLedgerAndHash): a point set
// merges few parts. Empty input returns an empty non-nil map. A const so explorer_tx_outcomes_test.go can pin the ledger IN-set and FINAL.
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

// ContractActivityRow is a contract event for the contract-activity view, most recent first.
type ContractActivityRow struct {
	Seq        uint32
	CloseTime  time.Time
	TxHash     string
	OpIndex    uint32
	EventIndex uint32
	EventType  string
	Topic0Sym  string
	// TopicsDisplay / DataDisplay are human-readable renderings of the remaining topics and data payload.
	TopicsDisplay []string
	DataDisplay   string
}

// contractEventsCursorClause is the keyset predicate both contract-events shapes share. KeyCondition does not prune a tuple comparison, so
// the redundant `ledger_seq <= ?` stops a deep page reading every granule above the cursor.
const contractEventsCursorClause = ` AND ledger_seq <= ? AND (ledger_seq, tx_hash, op_index, event_index) < (?, ?, ?, ?)`

// contractEventsRecentQuery builds the fast-path SQL. Neither FINAL (defeats the contract_id bloom skip-index) nor LIMIT 1 BY (disables
// the reverse read-in-order early exit). Un-merged duplicates collapse in Go by contractEventsScan; the page over-fetches
// contractEventsDedupHeadroom rows and a duplicate storm beyond that falls back to contractEventsRecentDedupQuery. explorerScanSettings:
// scan-shaped over wide topics_xdr/data_xdr.
func contractEventsRecentQuery(hasCursor, hasLedgerSet bool) string {
	q := `SELECT ledger_seq, close_time, tx_hash, op_index, event_index, event_type, topic_0_sym,
			topics_xdr, data_xdr
		FROM stellar.contract_events WHERE contract_id = ?`
	if hasCursor {
		// Full row-identity tuple (see ContractEventsCursor); the 3-part tuple skipped tied rows.
		q += contractEventsCursorClause
	}
	if hasLedgerSet {
		// Active-ledger bound from contract_active_ledgers: prunes to the granules the contract
		// touched, which makes QUIET contracts fast (reverse read-in-order covers busy ones).
		q += ` AND ledger_seq IN (?)`
	}
	return q + ` ORDER BY ledger_seq DESC, tx_hash DESC, op_index DESC, event_index DESC` +
		` LIMIT ?` + explorerScanSettings
}

// contractEventsRecentDedupQuery is the in-ClickHouse dedup correctness fallback for a
// duplicate storm; slow on busy contracts, issued only when the fast path could not fill the page.
func contractEventsRecentDedupQuery(hasCursor, hasLedgerSet bool) string {
	q := `SELECT ledger_seq, close_time, tx_hash, op_index, event_index, event_type, topic_0_sym,
			topics_xdr, data_xdr
		FROM stellar.contract_events WHERE contract_id = ?`
	if hasCursor {
		q += contractEventsCursorClause
	}
	if hasLedgerSet {
		q += ` AND ledger_seq IN (?)`
	}
	return q + ` ORDER BY ledger_seq DESC, tx_hash DESC, op_index DESC, event_index DESC` +
		` LIMIT 1 BY ledger_seq, tx_hash, op_index, event_index LIMIT ?` + explorerScanSettings
}

// contractEventsDedupHeadroom is the fast path's over-fetch. RMT duplicates exist only
// for not-yet-merged parts, so they are rare and few; 100 extra rows bounds the worst fetch at 600.
const contractEventsDedupHeadroom = 100

// ContractEventsRecent returns a contract's newest events, descending, with NO FINAL (duplicates collapse in contractEventsScan); a set cursor pages by the full row identity (ledger_seq, tx_hash, op_index, event_index).
func (r *ExplorerReader) ContractEventsRecent(ctx context.Context, contractID string, limit int, cur ContractEventsCursor) ([]ContractActivityRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	fetch := limit + contractEventsDedupHeadroom

	// Active-ledger bound: when the index is usable AND the per-contract walk is NON-EMPTY, prune to those ledgers (lossless: each holds at
	// least one event). Without a genesis watermark an EMPTY walk is NOT "no events": the probe is table-global and cannot see PARTIAL
	// backfill, so an empty walk or index error falls through to the unbounded read (the source of truth).
	var ledgers []uint32
	walked := false
	if r.contractLedgersIndexAvailable(ctx) {
		if ls, lerr := r.contractActiveLedgers(ctx, contractID, cur.Ledger, fetch); lerr == nil {
			walked = true
			if len(ls) > 0 {
				ledgers = ls
			}
		}
	}
	if walked && ledgers == nil && r.contractLedgersIndexComplete(ctx) {
		return nil, nil
	}

	out, raw, err := r.contractEventsScan(ctx, contractEventsRecentQuery(cur.IsSet(), ledgers != nil), contractID, limit, fetch, cur, ledgers)
	if err != nil {
		return nil, err
	}
	if ledgers != nil && len(out) < limit && !r.contractLedgersIndexComplete(ctx) {
		// A short bounded page cannot be told from a walk truncated by a partial backfill, and a
		// short page ends pagination, so re-read from contract_events.
		ledgers = nil
		out, raw, err = r.contractEventsScan(ctx, contractEventsRecentQuery(cur.IsSet(), false), contractID, limit, fetch, cur, nil)
		if err != nil {
			return nil, err
		}
	}
	if len(out) < limit && raw == fetch {
		// Duplicate storm: the over-fetch was ALL consumed and dedup still could not fill the
		// page, the only case where a short page would lie (next_cursor keys on a FULL page).
		exactFallback("contract_events")
		out, _, err = r.contractEventsScan(ctx, contractEventsRecentDedupQuery(cur.IsSet(), ledgers != nil), contractID, limit, limit, cur, ledgers)
	}
	return out, err
}

// contractLedgersIndexComplete reports whether ch-contract-ledgers-backfill proved contract_active_ledgers complete from genesis (its MV
// covers later ledgers), so a per-contract walk is exact; absent or unreadable answers false.
func (r *ExplorerReader) contractLedgersIndexComplete(ctx context.Context) bool {
	wm, err := r.genesisWatermark(ctx, ContractActiveLedgersTable)
	return err == nil && wm > 0
}

// contractEventsScan issues one contract-events page query, collapsing adjacent duplicate row-identities (exact under the full ORDER BY
// tuple), keeping at most `keep`. It returns the raw row count so the caller can tell "data exhausted" from "headroom exhausted".
func (r *ExplorerReader) contractEventsScan(ctx context.Context, q, contractID string, keep, fetch int, cur ContractEventsCursor, ledgers []uint32) ([]ContractActivityRow, int, error) {
	args := []any{contractID}
	if cur.IsSet() {
		args = append(args, cur.Ledger, cur.Ledger, cur.TxHash, cur.OpIndex, cur.EventIndex)
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
			continue // un-merged RMT duplicate part
		}
		last = e
		if len(out) == keep {
			// Page full: keep draining rows.Next() only to count raw (the fetch LIMIT bounds it), not to decode.
			continue
		}
		// Skip topic[0] (already Topic0Sym) and render the rest; decode failures degrade to omission.
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

// ContractDirectoryRow is one row of the contracts directory, ranked by recent event activity.
type ContractDirectoryRow struct {
	ContractID string
	Events     int64
	LastLedger uint32
	LastSeen   time.Time
}

// recentContractsQuery is RecentContracts' SQL (see its doc for uniqExact vs count() and no FINAL). explorerScanSettings: a multi-day
// GROUP BY over contract_events, the heaviest read behind the directory; the stale-serving cache fixes latency, the pin is the host-safety
// half.
const recentContractsQuery = `SELECT contract_id,
		       toInt64(uniqExact((ledger_seq, tx_hash, op_index, event_index))) AS events,
		       max(ledger_seq) AS last_ledger, max(close_time) AS last_seen
		FROM stellar.contract_events
		WHERE ledger_seq >= ?
		GROUP BY contract_id
		ORDER BY events DESC
		LIMIT ?` + explorerScanSettings

// RecentContracts returns the most active contracts by event count in [sinceLedger, tip] (GET /v1/contracts), counting uniqExact over the PRIMARY KEY (not count(), which duplicates would inflate and mis-rank) and not FINAL.
func (r *ExplorerReader) RecentContracts(ctx context.Context, limit int, sinceLedger uint32) ([]ContractDirectoryRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// Census-first: sum precomputed day rows instead of the 40s uniqExact GROUP BY. The window
	// floor rounds DOWN to the start of sinceLedger's UTC day, so the ranking window can be up
	// to a day wider than exact; immaterial for ranking.
	if r.censusAvailable(ctx) {
		if out, ok, err := r.recentContractsFromCensus(ctx, limit, sinceLedger); err != nil {
			return nil, err
		} else if ok {
			return out, nil
		}
		// !ok: sinceLedger predates census coverage; fall through to the exact scan.
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

// ContractEdgeRow is one edge of a contract's interaction map: another contract that emitted events in the same transactions as the subject.
type ContractEdgeRow struct {
	ContractID string
	SharedTxs  int64
}

// contractInteractionsQuery is ContractInteractions' SQL (uniqExact / subjectTxCap rationale in that method). explorerScanSettings: the
// subject's bloom-probed tx-set collection and the outer window scan are both scan-shaped over contract_events.
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

// ContractInteractions returns contracts co-occurring with contractID in the same transactions, ranked by shared-tx count, window-scoped via sinceLedger; co-occurrence is only a proxy for a cross-contract call.
func (r *ExplorerReader) ContractInteractions(ctx context.Context, contractID string, limit int, sinceLedger uint32) ([]ContractEdgeRow, uint32, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	// Anchor the window to the contract's OWN recent activity, not wall-clock days: both halves scale with ledger SPAN. This narrows what a
	// busy contract reports, consistent with subjectTxCap truncating to a recent sample; quiet contracts keep the full window. The
	// effective floor is RETURNED so since_ledger describes the window served.
	const activeLedgerWindow = 500
	if r.contractLedgersIndexAvailable(ctx) {
		if ls, err := r.contractActiveLedgers(ctx, contractID, 0, activeLedgerWindow); err == nil && len(ls) == activeLedgerWindow {
			// contractActiveLedgers is newest-first, so the last entry is the oldest ledger inside the cap. Never widen the window.
			if oldest := ls[len(ls)-1]; oldest > sinceLedger {
				sinceLedger = oldest
			}
		}
	}
	// Cap the subject's tx set to its most recent 50k DISTINCT (ledger, tx) rows, else a
	// mega-contract builds an enormous IN set and times out. DISTINCT (ledger_seq, tx_hash)
	// makes the cap count TRANSACTIONS, not event rows (10-20 events/tx would shrink the sample).
	const subjectTxCap = 50_000
	// uniqExact(tx_hash), not count(): contract_events is ReplacingMergeTree, so retried flushes leave duplicates that count() double-counts,
	// and count() would count co-occurring EVENTS while the column and API say shared_txs. Distinct tx_hash is duplicate-proof, so served
	// values drop for busy pairs.
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

// EventsByTx returns a transaction's contract events, ledger-scoped; FINAL is cheap here (one partition, ledger_seq prefix) and stops an un-merged duplicate showing an event twice.
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
			&t.FeeCharged, &t.MaxFee, &t.OperationCount, &ok, &t.ResultCode, &t.MemoType, &t.Memo,
			&t.InnerTxHash, &t.FeeAccount, &t.FeeBumpFee, &t.InnerResultCode); err != nil {
			return nil, fmt.Errorf("clickhouse: scan tx: %w", err)
		}
		t.Successful = ok != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// censusHeadMaxLag is how stale the census HEAD (max(day)) may be before recentContractsFromCensus falls through to the exact scan: one
// day absorbs the UTC rollover while catching a stalled ~30 min rollup timer.
const censusHeadMaxLag = 24 * time.Hour

// recentContractsCensusQuery sums the day-keyed census over the window; the day floor comes
// from sinceLedger via a pruned PK lookup on stellar.ledgers.
const recentContractsCensusQuery = `SELECT contract_id,
		       toInt64(sum(events)) AS events,
		       max(last_ledger) AS last_ledger, max(last_seen) AS last_seen
		FROM stellar.contracts_census_daily
		WHERE day >= ?
		GROUP BY contract_id
		ORDER BY events DESC
		LIMIT ?`

// recentContractsFromCensus serves the directory from contracts_census_daily; ok=false when
// the census doesn't cover sinceLedger's day (caller falls back to the exact scan).
func (r *ExplorerReader) recentContractsFromCensus(ctx context.Context, limit int, sinceLedger uint32) ([]ContractDirectoryRow, bool, error) {
	// Resolve the window floor's UTC day from the ledger sequence (pruned read on the ledgers PK).
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

	// Coverage check: the census must reach back to the floor day (a climbing backfill must not serve a truncated tail) AND its HEAD must
	// be fresh, or a stalled rollup would serve a stale ranking stamped as current. today() is read from the SAME server as the census to
	// avoid app/DB clock skew.
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
	// Head-freshness: a healthy head is today, or yesterday right after UTC rollover. Older
	// means the timer stalled; fall through to the exact scan (always fresh) so the degraded state surfaces.
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
