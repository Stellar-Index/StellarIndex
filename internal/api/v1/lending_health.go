package v1

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend/oracleagg"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// lendingPoolAuctionsReader is the lake read of a pool's live Auction keys.
type lendingPoolAuctionsReader interface {
	LiveAuctionUsers(ctx context.Context, pool string) (clickhouse.LendingPoolAuctions, error)
}

var _ lendingPoolAuctionsReader = (*clickhouse.ExplorerReader)(nil)

const (
	healthDefaultLimit = 50
	healthMaxLimit     = 200
)

// lendingHealthEnabled holds each server's [lending_health] flag; the server struct takes no new fields.
var lendingHealthEnabled sync.Map

// SetLendingHealthEnabled switches GET /v1/lending/pools/{pool}/health on; call once at startup.
func (s *Server) SetLendingHealthEnabled(on bool) { lendingHealthEnabled.Store(s, on) }

func (s *Server) lendingHealthOn() bool {
	v, _ := lendingHealthEnabled.Load(s)
	on, _ := v.(bool)
	return on
}

func writeLendingHealthDisabled(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, "https://api.stellarindex.io/errors/lending-health-not-enabled",
		"Lending health not enabled", http.StatusNotFound,
		"This deployment does not serve pool health: [lending_health] enabled is false. Enabling it is an operator action.")
}

// poolHealthRead is what the /health fill adds to the positions snapshot: the pool-oracle prices at one ledger and the
// pool's live auctions, both read before (prices) and after (auctions) the positions, all judged against that ledger.
type poolHealthRead struct {
	prices      map[string]LendingOraclePrice
	basis       LendingOracleBasis
	auctions    clickhouse.LendingPoolAuctions
	auctionsErr bool
	// backstopUnknown: the pool instance, or its Backstop, did not read, so the account that may not be called liquidatable
	// is unknown. Such a fill is cached briefly (a lapsed instance stays lapsed) and served no-store.
	backstopUnknown bool
	// view and rows are derived once per fill; a page slices rows.
	view LendingPoolHealthView
	rows []healthRow
}

// transient reports a failed read on our side: the answer is served uncached, never held for the TTL.
func (h *poolHealthRead) transient() bool {
	if h == nil {
		return false
	}
	if h.auctionsErr {
		return true
	}
	for _, p := range h.prices {
		if p.Verdict == oracleagg.VerdictReadFailed {
			return true
		}
	}
	return false
}

// lendingHealthPartial carries a fill that hit a failed read: served to the callers waiting on it, never cached as good.
type lendingHealthPartial struct{ built poolTopAccounts }

func (e *lendingHealthPartial) Error() string { return "lending health: a read failed" }

// handleLendingPoolHealth serves GET /v1/lending/pools/{pool}/health. Invariant: an account is called liquidatable only
// when the pool's own audited oracle prices every reserve it holds at one ledger and no auction is live for it; every
// other account is a named class or exclusion, and the totals are then lower bounds.
func (s *Server) handleLendingPoolHealth(w http.ResponseWriter, r *http.Request) {
	if !s.lendingHealthOn() {
		writeLendingHealthDisabled(w, r)
		return
	}
	pool := r.PathValue("pool")
	if !canonical.IsContractID(pool) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-pool",
			"Invalid pool", http.StatusBadRequest, "pool must be a C-strkey contract id")
		return
	}
	limit, ok := parseExplorerLimit(w, r, healthDefaultLimit, healthMaxLimit)
	if !ok {
		return
	}
	cur, ok := parseHealthCursor(r.URL.Query().Get("cursor"))
	if !ok {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-cursor",
			"Invalid cursor", http.StatusBadRequest, "cursor must be an opaque value returned in a prior pagination.next")
		return
	}
	pr, okP := s.Explorer.(lendingPoolPositionsReader)
	ar, okA := s.Explorer.(lendingPoolAuctionsReader)
	if s.Explorer == nil || !okP || !okA {
		s.explorerUnavailable(w, r)
		return
	}
	if s.Lending == nil {
		writeLendingPoolNotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxHandlerBudget)
	defer cancel()
	protocol, _, err := s.backstopLineage(ctx, pool)
	if err != nil {
		s.writeLendingLookupError(ctx, w, r, "lending health lineage", pool, err)
		return
	}
	if protocol == "" {
		writeLendingPoolNotFound(w, r)
		return
	}
	built, at, cacheStale, err := s.lendingTopAccounts.get(ctx, "pool_health", "health|"+pool, s.fillPoolHealth(pr, ar, pool, protocol))
	var partial *lendingHealthPartial
	noStore := errors.As(err, &partial)
	if noStore {
		built, err = partial.built, nil
	}
	if err != nil {
		obs.LendingHealthFillFailuresTotal.WithLabelValues(pool).Inc()
		s.writeLendingLookupError(ctx, w, r, "lending health", pool, err)
		return
	}
	noStore = noStore || built.health.backstopUnknown
	writeLendingHealth(w, built, healthServe{at: at, cacheStale: cacheStale, noStore: noStore, cur: cur, limit: limit})
}

// healthServe is how one request is answered from a snapshot.
type healthServe struct {
	at                  time.Time
	cacheStale, noStore bool
	cur                 healthCursor
	limit               int
}

// writeLendingHealth serves one page of the fill's derived view; a snapshot behind a failed read is no-store and degraded.
func writeLendingHealth(w http.ResponseWriter, built poolTopAccounts, q healthServe) {
	view := built.health.view
	page, next := pageHealthRows(built.health.rows, q.cur, q.limit, view.Basis.Ledger)
	view.ViewChanged = q.cur.set && q.cur.ledger != view.Basis.Ledger
	view.Accounts = make([]LendingHealthAccount, 0, len(page))
	for _, row := range page {
		view.Accounts = append(view.Accounts, row.view)
	}
	env := Envelope{Data: view, AsOf: WireTime(q.at.UTC()), Flags: Flags{
		Stale: q.cacheStale || built.stale, Degraded: q.noStore || built.degraded || built.unreconciled || view.Summary.LowerBound,
	}}
	if next != "" {
		env.Pagination = &Pagination{Next: next}
	}
	if q.noStore {
		w.Header().Set("Cache-Control", "no-store")
	}
	writeEnvelope(w, env)
}

// fillPoolHealth fixes the evaluation ledger first (the oracle read), then reads positions, reserves and auctions, so
// every entry is the state at that ledger or is visibly newer than it.
func (s *Server) fillPoolHealth(pr lendingPoolPositionsReader, ar lendingPoolAuctionsReader, pool, protocol string) func(context.Context) (poolTopAccounts, error) {
	return func(ctx context.Context) (poolTopAccounts, error) {
		// The oracle is a constructor argument of the pool (no function changes it), so the cached reserves view names it.
		res, _, _, err := s.lendingReserves.get(ctx, "pool_reserves", pool, s.fillPoolReserves(pool))
		if err != nil && !errors.Is(err, errPoolLineageUnknown) {
			return poolTopAccounts{}, err
		}
		h := &poolHealthRead{prices: map[string]LendingOraclePrice{}}
		if pc := res.view.PoolConfig; pc != nil && pc.Oracle != nil {
			assets := append([]string{}, res.view.OmittedReserves...)
			for _, rv := range res.view.Reserves {
				assets = append(assets, rv.Asset)
			}
			h.prices, h.basis = s.PoolOraclePrices(ctx, LendingOraclePool{ID: pool, Oracle: *pc.Oracle, Reserves: assets, Ledger: s.freshLakeLedger(ctx)})
			s.withholdExcludedOracle(ctx, h.prices)
		}
		built, err := s.fillPoolTopAccounts(pr, pool, protocol)(ctx)
		if err != nil {
			return poolTopAccounts{}, err
		}
		auctions, err := ar.LiveAuctionUsers(ctx, pool)
		if err != nil {
			s.logger.Warn("lending health: auctions read failed", "err", err, "pool", pool)
		}
		h.auctions, h.auctionsErr = auctions, err != nil
		_, unknown := healthBackstop(built)
		h.backstopUnknown = unknown != ""
		built.health = h
		h.view, h.rows = deriveHealth(pool, built)
		if h.transient() {
			return built, &lendingHealthPartial{built: built}
		}
		return built, nil
	}
}

// freshLakeLedger reads the lake watermark directly, not from the 15 s cache, so fewer entries land past the evaluation
// ledger; 0 (take the cached one) when the read fails.
func (s *Server) freshLakeLedger(ctx context.Context) uint32 {
	if s.LakeWatermark == nil {
		return 0
	}
	l, _, err := s.LakeWatermark.LakeWatermark(ctx)
	if err != nil {
		return 0
	}
	return l
}

// healthReflectorSources are the feeds the oracle-aggregator adapters read, as our stored rounds name them.
var healthReflectorSources = []string{reflector.SourceCEX, reflector.SourceDEX, reflector.SourceFX}

// withholdExcludedOracle replaces a price reproduced from rounds of a source the operator excluded at the round's time
// with source_excluded; an unread exclusion set withholds every price as a failed read.
func (s *Server) withholdExcludedOracle(ctx context.Context, prices map[string]LendingOraclePrice) {
	set, judge, v := s.exclusionSnapshot(ctx)
	if v == pricingguard.NotWithheld && !judge {
		return
	}
	for a, p := range prices {
		if p.Verdict != "" {
			continue
		}
		if v != pricingguard.NotWithheld {
			prices[a] = LendingOraclePrice{Verdict: oracleagg.VerdictReadFailed, Detail: "source exclusions unread"}
			continue
		}
		base, err := canonical.NewSorobanAsset(a)
		at := time.Unix(int64(p.Price.Timestamp), 0).UTC() //nolint:gosec // a ledger-era unix second fits int64
		if err != nil || set.Withholding(ctx, base, p.Quote, healthReflectorSources, at, at.Add(time.Second)) != pricingguard.NotWithheld {
			prices[a] = LendingOraclePrice{Verdict: healthVerdictSourceExcluded, Detail: "an operator source exclusion covers the round"}
		}
	}
}

// healthVerdictSourceExcluded is ours, not the adapter's: the round's source is excluded, so it values nothing here.
const healthVerdictSourceExcluded oracleagg.Verdict = "source_excluded"

// healthCursor is the last row served: the view's ledger, and the row's bases (priced) or only its address (unpriced).
type healthCursor struct {
	set     bool
	ledger  uint32
	c, l    *big.Int
	account string
}

// parseHealthCursor decodes "h.<ledger>.<collateral>.<liabilities>.<account>" or "n.<ledger>.<account>"; "" is the first page.
func parseHealthCursor(raw string) (healthCursor, bool) {
	if raw == "" {
		return healthCursor{}, true
	}
	parts := strings.Split(raw, ".")
	validAccount := func(a string) bool { return canonical.IsContractID(a) || canonical.IsAccountID(a) }
	if len(parts) < 3 {
		return healthCursor{}, false
	}
	ledger, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return healthCursor{}, false
	}
	switch {
	case len(parts) == 3 && parts[0] == "n" && validAccount(parts[2]):
		return healthCursor{set: true, ledger: uint32(ledger), account: parts[2]}, true
	case len(parts) == 5 && parts[0] == "h" && validAccount(parts[4]):
		c, okC := parseBig(parts[2])
		l, okL := parseBig(parts[3])
		if okC && okL && c.Sign() >= 0 && l.Sign() > 0 {
			return healthCursor{set: true, ledger: uint32(ledger), c: c, l: l, account: parts[4]}, true
		}
	}
	return healthCursor{}, false
}

func healthCursorOf(row healthRow, ledger uint32) string {
	l := strconv.FormatUint(uint64(ledger), 10)
	if row.l == nil {
		return "n." + l + "." + row.view.Account
	}
	return "h." + l + "." + row.c.String() + "." + row.l.String() + "." + row.view.Account
}

// pageHealthRows returns the rows after cur, up to limit, and the cursor of the next page ("" on the last). The cursor
// is a sort key, so it pages a view computed at another ledger too.
func pageHealthRows(rows []healthRow, cur healthCursor, limit int, ledger uint32) ([]healthRow, string) {
	start := 0
	if cur.set {
		start = sort.Search(len(rows), func(i int) bool {
			return compareHealthKey(rows[i].c, rows[i].l, rows[i].view.Account, cur.c, cur.l, cur.account) > 0
		})
	}
	end := min(start+limit, len(rows))
	if end < len(rows) {
		return rows[start:end], healthCursorOf(rows[end-1], ledger)
	}
	return rows[start:end], ""
}
