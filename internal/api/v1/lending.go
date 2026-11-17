package v1

import (
	"cmp"
	"context"
	"errors"
	"math"
	"math/big"
	"net/http"
	"slices"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// LendingReader is the storage-side seam for /v1/lending/pools; timescale.Store implements it.
type LendingReader interface {
	ListBlendPools(ctx context.Context) ([]timescale.BlendPoolSummary, error)
	BlendPoolAssets(ctx context.Context, pool string) ([]string, error)
	BlendPoolVersion(ctx context.Context, pool string) (blend.PoolVersion, error)
	BlendReserveConfigs(ctx context.Context, pool string) (map[string]blend.ReserveConfig, error)
}

// blntLendingReader is the BLNT V3 half of the lending seam, optional so a Blend-only reader still serves.
type blntLendingReader interface {
	ListBLNTPools(ctx context.Context) ([]timescale.BlendPoolSummary, error)
	BLNTPoolAssets(ctx context.Context, pool string) ([]string, error)
	BLNTReserveConfigMetadata(ctx context.Context, pool string) (map[string][]byte, error)
}

// blntLendingAtReader is ListBLNTPools with the cache fill time, as lendingAtReader is for Blend.
type blntLendingAtReader interface {
	ListBLNTPoolsAt(ctx context.Context) ([]timescale.BlendPoolSummary, time.Time, bool, error)
}

// blntReserveReader is the lake read of a BLNT pool's reserve state.
type blntReserveReader interface {
	BLNTPoolReserves(ctx context.Context, pool string, assets []string, configs map[string]blnt.ReserveConfig) (clickhouse.BlendPoolState, error)
}

var (
	_ blntLendingReader   = (*timescale.Store)(nil)
	_ blntLendingAtReader = (*CachedLendingReader)(nil)
	_ blntLendingReader   = (*CachedLendingReader)(nil)
	_ blntReserveReader   = (*clickhouse.ExplorerReader)(nil)
)

// lendingAtReader is the optional fill-time read a cached LendingReader offers, so the envelope dates a cached list.
type lendingAtReader interface {
	ListBlendPoolsAt(ctx context.Context) ([]timescale.BlendPoolSummary, time.Time, bool, error)
}

// LendingPool is a /v1/lending/pools entry (Blend or BLNT, named by protocol); net_supplied_30d/net_borrowed_30d are a 30-day NET-FLOW proxy, NOT TVL, and null across more than one reserve asset.
type LendingPool struct {
	Protocol          string   `json:"protocol"`
	Pool              string   `json:"pool"`
	Auctions24h       int64    `json:"auctions_24h"`
	AuctionsTotal     int64    `json:"auctions_total"`
	UniqueUsers30d    int64    `json:"unique_users_30d"`
	LastSeen          WireTime `json:"last_seen"`
	NetSupplied30d    *string  `json:"net_supplied_30d"`              // token base-units, window net-flow proxy; null when multi-asset
	NetBorrowed30d    *string  `json:"net_borrowed_30d"`              // token base-units, window net-flow proxy; null when multi-asset
	Utilization30dPct *float64 `json:"utilization_30d_pct,omitempty"` // single-asset borrow/supply window ratio
	// Summary is the pool's current state from its cached reserves view; null while that view's first build runs
	// (SummaryPending) or after it failed (SummaryError), and when no reserves reader is wired.
	Summary        *LendingPoolSummary `json:"summary"`
	SummaryPending bool                `json:"summary_pending"`
	SummaryError   bool                `json:"summary_error"`
}

// LendingPoolSummary totals one pool's GET /v1/lending/pools/{pool}/reserves view. Utilization and APRs cover priced reserves only.
type LendingPoolSummary struct {
	TVLUSD     *string `json:"tvl_usd"`
	LowerBound bool    `json:"lower_bound"` // TVLUSD leaves out an unpriced reserve
	// BorrowedUSD sums the reserves with a borrowed USD value; null when none has one.
	BorrowedUSD        *string  `json:"borrowed_usd"`
	BorrowedLowerBound bool     `json:"borrowed_lower_bound"`
	UtilizationPct     *float64 `json:"utilization_pct"`
	//floatmoney:ok supplied-weighted yield rate, not an amount
	SupplyAPR          *float64 `json:"supply_apr"`
	BorrowAPR          *float64 `json:"borrow_apr"`
	PricedReserveCount int      `json:"priced_reserve_count"` // reserves with both supplied and borrowed USD values
	ReserveCount       int      `json:"reserve_count"`
}

// lendingPoolListBudget bounds the pool-list read: a cold fill runs per-pool auction and user counts over the trades hypertable.
const lendingPoolListBudget = 8 * time.Second

// handleLendingPools serves GET /v1/lending/pools: one row per Blend pool seen in the trailing-7d auction stream, by total
// auctions desc. 200 with an empty array when no LendingReader is wired, like other feature-gated handlers.
func (s *Server) handleLendingPools(w http.ResponseWriter, r *http.Request) {
	reader := s.Lending
	if reader == nil {
		writeJSON(w, []LendingPool{}, Flags{})
		return
	}
	lpCtx, lpCancel := context.WithTimeout(r.Context(), lendingPoolListBudget)
	defer lpCancel()
	var vintage dataVintage
	rows, stale, err := listBlendPools(lpCtx, reader, &vintage)
	if err != nil {
		s.writeListBlendPoolsError(lpCtx, w, r, err)
		return
	}
	out := make([]LendingPool, 0, len(rows))
	for _, p := range rows {
		out = append(out, lendingPoolRow("blend", p))
	}
	blntRows, blntStale, blntFailed := s.listBLNTPools(lpCtx, reader, &vintage)
	for _, p := range blntRows {
		out = append(out, lendingPoolRow(blnt.SourceName, p))
	}
	slices.SortStableFunc(out, func(a, b LendingPool) int { return cmp.Compare(b.AuctionsTotal, a.AuctionsTotal) })
	// Omitted BLNT rows make the list a lower bound; Degraded alone is header-only.
	flags := Flags{Stale: stale || blntStale, Degraded: blntFailed, Truncated: blntFailed}
	if s.attachLendingSummaries(out, &vintage, &flags) {
		w.Header().Set("Cache-Control", "no-store")
	}
	writeEnvelope(w, Envelope{Data: out, AsOf: vintage.asOf(), Flags: flags})
}

// writeListBlendPoolsError maps a failed pool-list read: nothing for an aborted client, the retryable 503 on its deadline or an
// unavailable cache, else 500.
func (s *Server) writeListBlendPoolsError(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
	if clientAborted(r, err) {
		return
	}
	if handlerTimedOut(ctx, err) {
		s.logger.Warn("ListBlendPools deadline exceeded")
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/lending-timeout",
			"Lending pools query timed out", http.StatusServiceUnavailable,
			"the per-pool auction + user aggregates didn't return in 8s; retry shortly.")
		return
	}
	if IsCacheUnavailable(err) {
		s.logger.Warn("ListBlendPools cache unavailable", "err", err)
		writeCacheUnavailableProblem(w, r)
		return
	}
	s.logger.Error("ListBlendPools failed", "err", err)
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/internal",
		"Internal error", http.StatusInternalServerError, "")
}

func lendingPoolRow(protocol string, p timescale.BlendPoolSummary) LendingPool {
	return LendingPool{
		Protocol:          protocol,
		Pool:              p.Pool,
		Auctions24h:       p.Auctions24h,
		AuctionsTotal:     p.AuctionsTotal,
		UniqueUsers30d:    p.UniqueUsers30d,
		LastSeen:          WireTime(p.LastSeen),
		NetSupplied30d:    p.NetSupplied30d,
		NetBorrowed30d:    p.NetBorrowed30d,
		Utilization30dPct: utilizationPct(p.NetSupplied30d, p.NetBorrowed30d),
	}
}

// listBlendPools reads the Blend pool rows, dating vintage by the cache fill when the reader is cached.
func listBlendPools(ctx context.Context, reader LendingReader, vintage *dataVintage) (rows []timescale.BlendPoolSummary, stale bool, err error) {
	at, ok := reader.(lendingAtReader)
	if !ok {
		vintage.note(time.Time{})
		rows, err = reader.ListBlendPools(ctx)
		return rows, false, err
	}
	var filled time.Time
	rows, filled, stale, err = at.ListBlendPoolsAt(ctx)
	vintage.note(filled)
	return rows, stale, err
}

// listBLNTPools reads the BLNT pool rows when the reader serves them. A failed read leaves them out and
// reports failed, which the list serves as flags.truncated rather than failing the Blend rows with it.
func (s *Server) listBLNTPools(ctx context.Context, reader LendingReader, vintage *dataVintage) (rows []timescale.BlendPoolSummary, stale, failed bool) {
	br, ok := reader.(blntLendingReader)
	if !ok {
		return nil, false, false
	}
	var err error
	var filled time.Time
	if at, ok := reader.(blntLendingAtReader); ok {
		rows, filled, stale, err = at.ListBLNTPoolsAt(ctx)
	} else {
		rows, err = br.ListBLNTPools(ctx)
	}
	if err != nil {
		s.logger.Warn("ListBLNTPools failed; BLNT pools omitted", "err", err)
		return nil, false, true
	}
	if len(rows) > 0 { // an empty list serves no BLNT data, so it must not date the envelope
		vintage.note(filled)
	}
	return rows, stale, false
}

// lendingSummaryRetry is how long the list holds off rebuilding a pool whose first reserves build failed: one failing pool must
// neither refire a build on every request nor keep the list uncacheable.
const lendingSummaryRetry = time.Minute

// attachLendingSummaries fills each row's summary from the per-pool reserves cache without waiting on a build: a pool with no value
// yet starts its build and is marked pending. It reports whether any row is pending. as_of and flags take every used entry's.
func (s *Server) attachLendingSummaries(rows []LendingPool, vintage *dataVintage, flags *Flags) (pending bool) {
	if s.Explorer == nil {
		return false
	}
	for i := range rows {
		pool := rows[i].Pool
		if s.lendingSummaryFailedRecently(pool) && !s.lendingReserves.filled(pool) {
			rows[i].SummaryError = true
			continue
		}
		res, at, cacheStale, ok := s.lendingReserves.peek("pool_reserves", pool, s.fillPoolReserves(pool))
		if !ok {
			rows[i].SummaryPending, pending = true, true
			continue
		}
		rows[i].Summary = summarizeReserves(res.view)
		vintage.note(at)
		flags.Stale = flags.Stale || res.flags.Stale || cacheStale
		flags.Degraded = flags.Degraded || res.flags.Degraded
	}
	return pending
}

// fillPoolReserves is the one upstream both the list and the reserves handler fill through, so a failed or panicking build is
// remembered whichever handler started it.
func (s *Server) fillPoolReserves(pool string) func(context.Context) (poolReserves, error) {
	return func(ctx context.Context) (res poolReserves, err error) {
		failed := true
		defer func() {
			if failed {
				s.noteLendingSummaryFailure(pool)
			}
		}()
		res, err = s.buildPoolReserves(ctx, pool)
		failed = err != nil
		return res, err
	}
}

func (s *Server) lendingSummaryFailedRecently(pool string) bool {
	s.lendingSummaryMu.Lock()
	defer s.lendingSummaryMu.Unlock()
	at, ok := s.lendingSummaryFailed[pool]
	return ok && time.Since(at) < lendingSummaryRetry
}

func (s *Server) noteLendingSummaryFailure(pool string) {
	s.lendingSummaryMu.Lock()
	defer s.lendingSummaryMu.Unlock()
	if s.lendingSummaryFailed == nil {
		s.lendingSummaryFailed = map[string]time.Time{}
	}
	// The key is any client-supplied C-strkey; an entry past the retry window holds nothing off, so drop those.
	if len(s.lendingSummaryFailed) >= lendingReservesMaxEntries {
		for p, at := range s.lendingSummaryFailed {
			if time.Since(at) >= lendingSummaryRetry {
				delete(s.lendingSummaryFailed, p)
			}
		}
	}
	s.lendingSummaryFailed[pool] = time.Now()
}

// summarizeReserves totals a reserves view with exact decimal sums; a reserve without a USD value is left out of every figure, and a
// total that leaves one out is flagged a lower bound (AGENTS.md money invariant).
func summarizeReserves(v LendingPoolReservesView) *LendingPoolSummary {
	reserves := len(v.Reserves) + len(v.OmittedReserves)
	out := &LendingPoolSummary{TVLUSD: v.TVLUSD, LowerBound: v.LowerBound, ReserveCount: reserves}
	borrowed, bothSupplied, bothBorrowed := new(big.Rat), new(big.Rat), new(big.Rat)
	var supplyAPR, borrowAPR weightedRate
	nBorrowed := 0
	for _, r := range v.Reserves {
		sup, supOK := parseUSD(r.SuppliedUSD)
		bor, borOK := parseUSD(r.BorrowedUSD)
		if borOK {
			borrowed.Add(borrowed, bor)
			nBorrowed++
			borrowAPR.add(bor, r.BorrowAPR)
		}
		if supOK {
			supplyAPR.add(sup, r.SupplyAPR)
		}
		if supOK && borOK {
			bothSupplied.Add(bothSupplied, sup)
			bothBorrowed.Add(bothBorrowed, bor)
			out.PricedReserveCount++
		}
	}
	if nBorrowed > 0 {
		b := borrowed.FloatString(2)
		out.BorrowedUSD, out.BorrowedLowerBound = &b, nBorrowed < reserves
	}
	if bothSupplied.Sign() > 0 {
		pct, _ := new(big.Rat).Mul(new(big.Rat).Quo(bothBorrowed, bothSupplied), big.NewRat(100, 1)).Float64() // i128:ok a percentage, not an amount
		out.UtilizationPct = &pct
	}
	out.SupplyAPR, out.BorrowAPR = supplyAPR.value(), borrowAPR.value()
	return out
}

func parseUSD(s *string) (*big.Rat, bool) {
	if s == nil {
		return nil, false
	}
	return new(big.Rat).SetString(*s)
}

// weightedRate is a USD-weighted mean of a rate over the reserves that carry both.
type weightedRate struct{ num, den big.Rat }

func (w *weightedRate) add(weight *big.Rat, rate *float64) {
	if rate == nil || math.IsNaN(*rate) || math.IsInf(*rate, 0) {
		return
	}
	r := new(big.Rat)
	if r.SetFloat64(*rate) == nil {
		return
	}
	w.num.Add(&w.num, r.Mul(r, weight))
	w.den.Add(&w.den, weight)
}

func (w *weightedRate) value() *float64 {
	if w.den.Sign() <= 0 {
		return nil
	}
	f, _ := new(big.Rat).Quo(&w.num, &w.den).Float64() // i128:ok a yield rate, not an amount
	return &f
}

// LendingPoolReservesView is the GET /v1/lending/pools/{pool}/reserves response: per-reserve current state (ADR-0039) from the lake's contract storage.
type LendingPoolReservesView struct {
	Pool   string  `json:"pool"`
	TVLUSD *string `json:"tvl_usd"` // sum of priced reserves; null when none priced
	// LowerBound is true when TVLUSD is rendered but leaves out at least
	// one reserve with no USD value — the reserves[] entries whose
	// supplied_usd is null — so the pool holds at least TVLUSD.
	LowerBound bool          `json:"lower_bound"`
	Reserves   []ReserveView `json:"reserves"`
	// OmittedReserves are the pool's reserve assets with no decoded state in the lake; TVLUSD leaves them out.
	OmittedReserves []string `json:"omitted_reserves,omitempty"`
	// PoolConfig is the pool's instance-storage config; null when that entry, or every reserve's state, is missing.
	PoolConfig *LendingPoolConfigView `json:"pool_config"`
	// ConfigDecimals is the fixed-point scale of the config fractions and backstop_take_rate.
	ConfigDecimals int `json:"config_decimals"`
	// AsOfLedger is the highest ledger the lake had captured at serve time (ADR-0041 Decision 4).
	// Reserve state is decoded from lake storage, so a wedged sink makes it stale; pairs with
	// `flags.stale`. Omitted when no watermark reader is wired.
	AsOfLedger uint32 `json:"as_of_ledger,omitempty"`
}

// ReserveView is one reserve's decoded state and derived metrics; amounts are base-unit decimal strings (ADR-0003), USD values decimal strings or null when unpriced.
type ReserveView struct {
	Asset string `json:"asset"`
	// Decimals is nil when the token's exponent is not established; Supplied and Borrowed are then unscalable base units.
	Decimals       *uint32 `json:"decimals"`
	Supplied       string  `json:"supplied"` // underlying token base units
	Borrowed       string  `json:"borrowed"`
	SuppliedUSD    *string `json:"supplied_usd"`
	BorrowedUSD    *string `json:"borrowed_usd"`
	UtilizationPct float64 `json:"utilization_pct"`
	// APRs are null when the reserve's ReserveConfig (rate-model params)
	// isn't in the captured contract-storage window — supplied /
	// borrowed / utilization are still exact.
	BorrowAPR *float64 `json:"borrow_apr"`
	//floatmoney:ok a yield rate/fraction (like BorrowAPR above), not a principal amount
	SupplyAPR *float64 `json:"supply_apr"`
	// PriceUSD is the USD price per whole token the USD values use; null whenever they are.
	PriceUSD   *string                   `json:"price_usd"`
	PriceStale bool                      `json:"price_stale"`
	Config     *LendingReserveConfigView `json:"config"`
	IRMod      *string                   `json:"ir_mod"` // raw interest-rate modifier at ir_mod_decimals
	// IRModDecimals is 9 on a Blend V1 pool, else 7.
	IRModDecimals  int       `json:"ir_mod_decimals"`
	BackstopCredit *string   `json:"backstop_credit"` // underlying base units owed to the backstop
	LastUpdate     *WireTime `json:"last_update"`
}

// handleLendingPoolReserves serves GET /v1/lending/pools/{pool}/reserves: current-state TVL, utilization and APY per reserve
// (ADR-0039) decoded from the pool's Soroban storage. USD values are best-effort and null without a USD price and established
// decimals; token-unit amounts, utilization and APY are always exact.
func (s *Server) handleLendingPoolReserves(w http.ResponseWriter, r *http.Request) {
	if s.Explorer == nil {
		s.explorerUnavailable(w, r)
		return
	}
	if s.Lending == nil {
		writeJSON(w, LendingPoolReservesView{Reserves: []ReserveView{}}, Flags{})
		return
	}
	pool := r.PathValue("pool")
	if !canonical.IsContractID(pool) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-pool",
			"Invalid pool", http.StatusBadRequest, "pool must be a C-strkey contract id")
		return
	}

	// The longest budget that still fires ahead of the blanket request deadline. The reserve read
	// is a PK-prefix probe, not a ledger scan, and pricing fans out bounded, so this ceiling is a
	// backstop for a degraded lake.
	ctx, cancel := context.WithTimeout(r.Context(), maxHandlerBudget)
	defer cancel()

	res, at, cacheStale, err := s.lendingReserves.get(ctx, "pool_reserves", pool, s.fillPoolReserves(pool))
	if errors.Is(err, errPoolLineageUnknown) {
		writeJSON(w, LendingPoolReservesView{Pool: pool, Reserves: []ReserveView{}}, Flags{})
		return
	}
	if err != nil {
		stage := "lending reserves cache wait"
		var se *lendingStageErr
		if errors.As(err, &se) {
			stage, err = se.stage, se.err
		}
		s.writeLendingLookupError(ctx, w, r, stage, pool, err)
		return
	}
	flags := res.flags
	flags.Stale = flags.Stale || cacheStale
	writeEnvelope(w, Envelope{Data: res.view, AsOf: WireTime(at.UTC()), Flags: flags})
}

// lendingReservesTTL bounds how long a pool's built reserves view is served before one detached rebuild; the page fans out one
// request per pool at once, and each build is a lake read plus per-reserve pricing.
const lendingReservesTTL = time.Minute

// lendingReservesMaxEntries bounds the per-pool cache: the key is a client-supplied C-strkey.
const lendingReservesMaxEntries = 256

// poolReserves is one built GET /v1/lending/pools/{pool}/reserves body and its flags, cached per pool.
type poolReserves struct {
	view    LendingPoolReservesView
	flags   Flags
	states  []clickhouse.BlendReserveState // the raw reserve state behind view, for account earnings
	version blend.PoolVersion              // PoolVersionUnknown for a BLNT pool
}

// lendingStageErr names the read that failed, so the handler maps it as it did before the cache.
type lendingStageErr struct {
	stage string
	err   error
}

func (e *lendingStageErr) Error() string { return e.stage + ": " + e.err.Error() }
func (e *lendingStageErr) Unwrap() error { return e.err }

// errPoolLineageUnknown withholds a pool's reserves without caching the empty view: any C-strkey reaches this path, and a cached
// success per junk key would evict the listed pools.
var errPoolLineageUnknown = errors.New("lending pool lineage unknown")

// buildPoolReserves reads and prices one pool's reserves; as_of_ledger and flags.stale are the lake watermark at build time.
func (s *Server) buildPoolReserves(ctx context.Context, pool string) (poolReserves, error) {
	version, err := s.Lending.BlendPoolVersion(ctx, pool)
	if err != nil {
		return poolReserves{}, &lendingStageErr{"BlendPoolVersion", err}
	}
	read := s.readBlendPoolReserves
	if version == blend.PoolVersionUnknown {
		read = s.readBLNTPoolReserves
	}
	pr, err := read(ctx, pool, version)
	if err != nil {
		return poolReserves{}, err
	}
	states := pr.state.Reserves

	// Price reserves with a BOUNDED fan-out, not serially: each USD price resolution can cost
	// several round-trips (alias walk, then peg probes), and serial cost scales with the reserve
	// count enough to threaten the ceiling on large pools.
	//
	// Each goroutine writes only its own index-keyed slot (see forEachBounded); the TVL is summed
	// afterwards in reserve order so the total does not depend on completion order.
	views := make([]ReserveView, len(states))
	suppliedUSD := make([]*big.Rat, len(states))
	stalePrice := make([]bool, len(states))
	forEachBounded(s.logger, len(states), readFanoutConcurrency, func(i int) {
		views[i], suppliedUSD[i], stalePrice[i] = s.buildReserveView(ctx, states[i]) // distinct indices — no mutex needed
	})

	out := LendingPoolReservesView{
		Pool: pool, Reserves: views, OmittedReserves: reservesWithoutState(pr.assets, states),
		PoolConfig: poolConfigView(pr.state.Instance, version), ConfigDecimals: lendingConfigDecimals,
	}
	out.TVLUSD, out.LowerBound = reservesTVLUSD(suppliedUSD)
	out.LowerBound = out.LowerBound || (out.TVLUSD != nil && len(out.OmittedReserves) > 0)
	// ADR-0041 Decision 4: stamp the lake watermark this current-state
	// read is fresh to, and flip `flags.stale` when the sink is wedged —
	// same disclosure the sibling /v1/pools/reserves + account-state
	// reads carry.
	wmLedger, stale, _ := s.lakeWatermark(ctx)
	out.AsOfLedger = wmLedger
	// A USD figure valued at a stale price is stale whatever the lake's own freshness.
	stale = stale || slices.Contains(stalePrice, true)
	return poolReserves{view: out, flags: Flags{Stale: stale, Degraded: pr.configsFailed}, states: states, version: version}, nil
}

// poolRead is one pool's reserve assets and lake state; configsFailed means APRs are withheld.
type poolRead struct {
	assets        []string
	state         clickhouse.BlendPoolState
	configsFailed bool
}

// readBlendPoolReserves reads a Blend pool's reserve assets, configs and lake state.
func (s *Server) readBlendPoolReserves(ctx context.Context, pool string, version blend.PoolVersion) (poolRead, error) {
	assets, err := s.Lending.BlendPoolAssets(ctx, pool)
	if err != nil {
		return poolRead{}, &lendingStageErr{"BlendPoolAssets", err}
	}
	// Rate-model configs (for APY) come from blend_admin events — the
	// on-chain ResConfig storage entry is usually uncaptured. Best-effort:
	// a failure here just means APY is omitted, not a failed response.
	configs, err := s.Lending.BlendReserveConfigs(ctx, pool)
	var configsFailed bool
	if err != nil {
		s.logger.Warn("BlendReserveConfigs failed", "err", err, "pool", pool)
		configsFailed = true
		configs = nil
	}
	state, err := s.Explorer.BlendPoolReserves(ctx, pool, version, assets, configs)
	if err != nil {
		return poolRead{}, &lendingStageErr{"BlendPoolReserves", err}
	}
	return poolRead{assets: assets, state: state, configsFailed: configsFailed}, nil
}

// readBLNTPoolReserves serves a pool with no Blend lineage when blnt_events knows it as a BLNT pool; anything else is
// errPoolLineageUnknown. Configs are best-effort as on the Blend path.
func (s *Server) readBLNTPoolReserves(ctx context.Context, pool string, _ blend.PoolVersion) (poolRead, error) {
	lr, okL := s.Lending.(blntLendingReader)
	rr, okR := s.Explorer.(blntReserveReader)
	var assets []string
	if okL && okR {
		var err error
		if assets, err = lr.BLNTPoolAssets(ctx, pool); err != nil {
			return poolRead{}, &lendingStageErr{"BLNTPoolAssets", err}
		}
	}
	if len(assets) == 0 {
		s.logger.Warn("lending pool lineage unknown; reserves withheld", "pool", pool)
		return poolRead{}, errPoolLineageUnknown
	}
	meta, err := lr.BLNTReserveConfigMetadata(ctx, pool)
	configsFailed := err != nil
	if configsFailed {
		s.logger.Warn("BLNTReserveConfigMetadata failed", "err", err, "pool", pool)
	}
	configs := make(map[string]blnt.ReserveConfig, len(meta))
	for asset, b := range meta {
		cfg, err := blnt.ParseReserveConfigMetadata(b)
		if err != nil {
			s.logger.Warn("BLNT reserve config metadata unparseable", "err", err, "pool", pool, "asset", asset)
			configsFailed = true
			continue
		}
		configs[asset] = cfg
	}
	state, err := rr.BLNTPoolReserves(ctx, pool, assets, configs)
	if err != nil {
		return poolRead{}, &lendingStageErr{"BLNTPoolReserves", err}
	}
	return poolRead{assets: assets, state: state, configsFailed: configsFailed}, nil
}

// reservesWithoutState lists the pool's reserve assets the lake returned no state for, in pool order.
func reservesWithoutState(assets []string, states []clickhouse.BlendReserveState) []string {
	have := make(map[string]struct{}, len(states))
	for _, st := range states {
		have[st.Asset] = struct{}{}
	}
	var out []string
	for _, a := range assets {
		if _, ok := have[a]; !ok {
			out = append(out, a)
		}
	}
	return out
}

// reservesTVLUSD sums the priced reserves' supplied USD in reserve order. total is nil when none is priced; lowerBound is true
// when it excludes a reserve with no USD value (AGENTS.md money invariant: a partial total is a lower bound, never a total).
func reservesTVLUSD(suppliedUSD []*big.Rat) (total *string, lowerBound bool) {
	tvl := new(big.Rat)
	priced := 0
	for _, v := range suppliedUSD {
		if v != nil {
			tvl.Add(tvl, v)
			priced++
		}
	}
	if priced == 0 {
		return nil, false
	}
	s := tvl.FloatString(2)
	return &s, priced < len(suppliedUSD)
}

// writeLendingLookupError maps a failed served-tier lookup: nothing for an aborted client, the retryable 503 on the handler
// deadline, else 500.
func (s *Server) writeLendingLookupError(ctx context.Context, w http.ResponseWriter, r *http.Request, stage, pool string, err error) {
	if clientAborted(r, err) {
		return
	}
	if handlerTimedOut(ctx, err) {
		s.writeLendingReservesTimeout(w, r, stage, pool)
		return
	}
	s.logger.Error(stage+" failed", "err", err, "pool", pool)
	writeProblem(w, r, "https://api.stellarindex.io/errors/internal", "Internal error", http.StatusInternalServerError, "")
}

// writeLendingReservesTimeout is the typed 503 when the [maxHandlerBudget] ceiling fires ahead of the global deadline. A
// lake-read deadline is retryable capacity, not a fault: a 500 would tell clients not to retry, count against 5xx availability,
// and is not declared in OpenAPI for this path (400 and 503 only).
func (s *Server) writeLendingReservesTimeout(w http.ResponseWriter, r *http.Request, stage, pool string) {
	s.logger.Warn("lending reserves deadline exceeded", "stage", stage, "pool", pool)
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/lending-timeout",
		"Lending reserves query timed out", http.StatusServiceUnavailable,
		"the pool's reserve state didn't decode within "+maxHandlerBudget.String()+"; retry shortly.")
}

// buildReserveView maps one decoded reserve state to its wire shape, pricing it in USD best-effort; returns the supplied-USD
// contribution (nil when unpriced) for the pool TVL sum, and whether a served USD figure rests on a stale price.
func (s *Server) buildReserveView(ctx context.Context, st clickhouse.BlendReserveState) (ReserveView, *big.Rat, bool) {
	rv := reserveStateView(st)
	dec, decKnown := s.reserveDecimals(ctx, st)
	if decKnown {
		d := uint32(dec)
		rv.Decimals = &d
	}
	price, priceScale, stale, ok := s.reservePriceUSD(ctx, st.Asset)
	if !ok || !decKnown {
		// Without established decimals the USD figures, and the pool TVL,
		// would be wrong by a power of ten; withhold the valuation instead.
		return rv, nil, false
	}
	if priceScale != dec {
		// The price was normalised on a different scale than the amounts
		// are divided by; their product is off by 10^|difference|.
		obs.NonstandardDecimalsLockstepMismatchTotal.WithLabelValues("lending_reserve", "").Inc()
		return rv, nil, false
	}
	var suppliedContrib *big.Rat
	if usd, err := usdMarketValue(st.Metrics.SuppliedUnderlying, price, dec); err == nil {
		rv.SuppliedUSD = &usd
		if v, ok := new(big.Rat).SetString(usd); ok {
			suppliedContrib = v
		}
	}
	if usd, err := usdMarketValue(st.Metrics.BorrowedUnderlying, price, dec); err == nil {
		rv.BorrowedUSD = &usd
	}
	valued := rv.SuppliedUSD != nil || rv.BorrowedUSD != nil
	if valued {
		rv.PriceUSD, rv.PriceStale = &price, stale
	}
	return rv, suppliedContrib, stale && valued
}

// reserveStateView is a reserve's wire shape without decimals and USD values: everything decoded from contract storage.
func reserveStateView(st clickhouse.BlendReserveState) ReserveView {
	rv := ReserveView{
		Asset:          st.Asset,
		Supplied:       st.Metrics.SuppliedUnderlying.String(),
		Borrowed:       st.Metrics.BorrowedUnderlying.String(),
		UtilizationPct: round2(st.Metrics.UtilizationPct),
		Config:         reserveConfigView(st.Config),
		IRMod:          bigString(st.Data.IRMod),
		IRModDecimals:  irModDecimals(st.Data.Version),
		BackstopCredit: bigString(st.Data.BackstopCredit),
		LastUpdate:     reserveLastUpdate(st.Data.LastTime),
	}
	if st.Metrics.HasAPR {
		b := round4(st.Metrics.BorrowAPR)
		sup := round4(st.Metrics.SupplyAPR)
		rv.BorrowAPR = &b
		rv.SupplyAPR = &sup
	}
	return rv
}

// reserveDecimals returns the reserve token's real decimal exponent: the pool's reserve config when captured, else 7 for a
// known SAC, else the token contract's own decimals() from the lake. ok=false when none is available or the lake read failed.
func (s *Server) reserveDecimals(ctx context.Context, st clickhouse.BlendReserveState) (int, bool) {
	if st.DecimalsFound {
		return int(st.Decimals), true
	}
	if _, ok := s.resolveSACAsset(st.Asset); ok {
		return defaultTokenDecimals, true
	}
	d, known, err := s.lakeTokenDecimals(ctx, st.Asset)
	if err != nil || !known {
		return 0, false
	}
	return d, true
}

// reservePriceUSD resolves a USD price for a reserve's token. A SAC C-id is mapped back to the classic/native asset it wraps and priced
// there (feeds are keyed by (code, issuer)); ok=false means token units only. scale is the decimals the price was normalised with
// (nonstandard-decimals projection, else 7): only an amount divided by that same exponent can be multiplied by it. stale is the
// price's own verdict ([usdPriceLookup]).
func (s *Server) reservePriceUSD(ctx context.Context, assetC string) (price string, scale int, stale, ok bool) {
	if canonicalID, ok := s.resolveSACAsset(assetC); ok {
		if a, err := canonical.ParseAsset(canonicalID); err == nil {
			if l := s.lookupUSDPriceWithSources(ctx, a); l.price != "" {
				return l.price, aggregate.ResolveDecimals(s.NonstandardDecimals, a), l.stale, true
			}
		}
	}
	asset, err := canonical.ParseAsset(assetC)
	if err != nil {
		return "", 0, false, false
	}
	l := s.lookupUSDPriceWithSources(ctx, asset)
	return l.price, aggregate.ResolveDecimals(s.NonstandardDecimals, asset), l.stale, l.price != ""
}

// pubnetPassphrase is the mainnet passphrase; the verified-currency catalogue is pubnet-only, so SAC contract ids are computed
// against it.
const pubnetPassphrase = "Public Global Stellar Network ; September 2015"

// resolveSACAsset maps a SAC contract C-strkey to the canonical classic/native asset id it wraps, via a map built once from the
// verified-currency catalogue's Stellar assets (+ native XLM); ok=false when not a known asset's SAC.
func (s *Server) resolveSACAsset(contractC string) (string, bool) {
	s.sacReserveOnce.Do(func() { s.sacReserveAssets = s.buildSACReserveMap() })
	id, ok := s.sacReserveAssets[contractC]
	return id, ok
}

// buildSACReserveMap computes the SAC contract id for native XLM + every
// verified-currency Stellar asset and indexes them by C-strkey.
func (s *Server) buildSACReserveMap() map[string]string {
	m := make(map[string]string)
	if sac, ok := xdrjson.SACContractID("native", pubnetPassphrase); ok {
		m[sac] = "native"
	}
	if s.VerifiedCurrencies == nil {
		return m
	}
	for _, vc := range s.VerifiedCurrencies.All() {
		for _, n := range vc.Issuance {
			if n.AssetID == "" {
				continue
			}
			if sac, ok := xdrjson.SACContractID(n.AssetID, pubnetPassphrase); ok {
				m[sac] = n.AssetID
			}
		}
	}
	return m
}

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }
func round4(f float64) float64 { return float64(int64(f*10000+0.5)) / 10000 }

// utilizationPct returns the window borrow/supply ratio as a percentage (2dp), or nil when either flow is absent (a multi-asset
// window has no single unit) or net supply <= 0. Inputs are decimal big-int strings in the same asset's base units.
func utilizationPct(netSupplied, netBorrowed *string) *float64 {
	if netSupplied == nil || netBorrowed == nil {
		return nil
	}
	supplied, ok := new(big.Rat).SetString(*netSupplied)
	if !ok || supplied.Sign() <= 0 {
		return nil
	}
	borrowed, ok := new(big.Rat).SetString(*netBorrowed)
	if !ok || borrowed.Sign() < 0 {
		return nil
	}
	ratio := new(big.Rat).Quo(borrowed, supplied)
	pct, _ := new(big.Rat).Mul(ratio, big.NewRat(100, 1)).Float64() // i128:ok utilisation percentage, not an amount
	pct = float64(int64(pct*100+0.5)) / 100                         // round to 2dp
	return &pct
}
