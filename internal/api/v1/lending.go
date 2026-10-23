package v1

import (
	"context"
	"math/big"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
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

// lendingAtReader is the optional fill-time read a cached LendingReader offers, so the envelope dates a cached list.
type lendingAtReader interface {
	ListBlendPoolsAt(ctx context.Context) ([]timescale.BlendPoolSummary, time.Time, bool, error)
}

// LendingPool is a /v1/lending/pools entry (Blend-only); net_supplied_30d/net_borrowed_30d are a 30-day NET-FLOW proxy, NOT TVL, and null across more than one reserve asset.
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
}

// handleLendingPools serves GET /v1/lending/pools: one row per Blend pool seen in the trailing-7d auction stream, by total
// auctions desc. 200 with an empty array when no LendingReader is wired, like other feature-gated handlers.
func (s *Server) handleLendingPools(w http.ResponseWriter, r *http.Request) {
	reader := s.Lending
	if reader == nil {
		writeJSON(w, []LendingPool{}, Flags{})
		return
	}
	// 8s ceiling — same pattern as the cold-path series.
	// ListBlendPools fans out per-pool auction-count + user-count
	// queries against the trades hypertable; cold cache can take 5+s.
	lpCtx, lpCancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer lpCancel()
	var vintage dataVintage
	var rows []timescale.BlendPoolSummary
	var stale bool
	var err error
	if at, ok := reader.(lendingAtReader); ok {
		var filled time.Time
		rows, filled, stale, err = at.ListBlendPoolsAt(lpCtx)
		vintage.note(filled)
	} else {
		rows, err = reader.ListBlendPools(lpCtx)
		vintage.note(time.Time{})
	}
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(lpCtx, err) {
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
		return
	}
	out := make([]LendingPool, len(rows))
	for i, p := range rows {
		out[i] = LendingPool{
			Protocol:          "blend",
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
	writeEnvelope(w, Envelope{Data: out, AsOf: vintage.asOf(), Flags: Flags{Stale: stale}})
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
	// AsOfLedger is the highest ledger the lake had captured at serve time (ADR-0041 Decision 4).
	// Reserve state is decoded from lake storage, so a wedged sink makes it stale; pairs with
	// `flags.stale`. Omitted when no watermark reader is wired.
	AsOfLedger uint32 `json:"as_of_ledger,omitempty"`
}

// ReserveView is one reserve's decoded state and derived metrics; amounts are base-unit decimal strings (ADR-0003), USD values decimal strings or null when unpriced.
type ReserveView struct {
	Asset          string  `json:"asset"`
	Decimals       uint32  `json:"decimals"`
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

	version, assets, proceed := s.blendReserveInputs(ctx, w, r, pool)
	if !proceed {
		return
	}
	// Rate-model configs (for APY) come from blend_admin events — the
	// on-chain ResConfig storage entry is usually uncaptured. Best-effort:
	// a failure here just means APY is omitted, not a failed response.
	configs, err := s.Lending.BlendReserveConfigs(ctx, pool)
	var configsFailed bool
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Warn("BlendReserveConfigs failed", "err", err, "pool", pool)
		configsFailed = true
		configs = nil
	}
	states, err := s.Explorer.BlendPoolReserves(ctx, pool, version, assets, configs)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(ctx, err) {
			s.writeLendingReservesTimeout(w, r, "BlendPoolReserves", pool)
			return
		}
		s.logger.Error("BlendPoolReserves failed", "err", err, "pool", pool)
		writeProblem(w, r, "https://api.stellarindex.io/errors/internal", "Internal error", http.StatusInternalServerError, "")
		return
	}

	// Price reserves with a BOUNDED fan-out, not serially: each USD price resolution can cost
	// several round-trips (alias walk, then peg probes), and serial cost scales with the reserve
	// count enough to threaten the ceiling on large pools.
	//
	// Each goroutine writes only its own index-keyed slot (see forEachBounded); the TVL is summed
	// afterwards in reserve order so the total does not depend on completion order.
	views := make([]ReserveView, len(states))
	suppliedUSD := make([]*big.Rat, len(states))
	forEachBounded(s.logger, len(states), readFanoutConcurrency, func(i int) {
		views[i], suppliedUSD[i] = s.buildReserveView(ctx, states[i]) // distinct indices — no mutex needed
	})

	out := LendingPoolReservesView{Pool: pool, Reserves: views}
	out.TVLUSD, out.LowerBound = reservesTVLUSD(suppliedUSD)
	// ADR-0041 Decision 4: stamp the lake watermark this current-state
	// read is fresh to, and flip `flags.stale` when the sink is wedged —
	// same disclosure the sibling /v1/pools/reserves + account-state
	// reads carry.
	wmLedger, stale, _ := s.lakeWatermark(ctx)
	out.AsOfLedger = wmLedger
	writeJSON(w, out, Flags{Stale: stale, Degraded: configsFailed})
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

// blendReserveInputs resolves the pool's contract generation and reserve assets. A pool with no known V1/V2 lineage gets an
// empty reserve list, since a guessed rate scale is off by 10^3. proceed=false means a response was written or the client has
// gone.
func (s *Server) blendReserveInputs(ctx context.Context, w http.ResponseWriter, r *http.Request, pool string) (version blend.PoolVersion, assets []string, proceed bool) {
	version, err := s.Lending.BlendPoolVersion(ctx, pool)
	if err != nil {
		s.writeLendingLookupError(ctx, w, r, "BlendPoolVersion", pool, err)
		return version, nil, false
	}
	if version == blend.PoolVersionUnknown {
		s.logger.Warn("blend pool lineage unknown; reserves withheld", "pool", pool)
		writeJSON(w, LendingPoolReservesView{Pool: pool, Reserves: []ReserveView{}}, Flags{})
		return version, nil, false
	}
	assets, err = s.Lending.BlendPoolAssets(ctx, pool)
	if err != nil {
		s.writeLendingLookupError(ctx, w, r, "BlendPoolAssets", pool, err)
		return version, nil, false
	}
	return version, assets, true
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
// contribution (nil when unpriced) for the pool TVL sum.
func (s *Server) buildReserveView(ctx context.Context, st clickhouse.BlendReserveState) (ReserveView, *big.Rat) {
	rv := ReserveView{
		Asset:          st.Asset,
		Decimals:       st.Decimals,
		Supplied:       st.Metrics.SuppliedUnderlying.String(),
		Borrowed:       st.Metrics.BorrowedUnderlying.String(),
		UtilizationPct: round2(st.Metrics.UtilizationPct),
	}
	if st.Metrics.HasAPR {
		b := round4(st.Metrics.BorrowAPR)
		sup := round4(st.Metrics.SupplyAPR)
		rv.BorrowAPR = &b
		rv.SupplyAPR = &sup
	}
	dec, decKnown := s.reserveDecimals(ctx, st)
	if decKnown {
		rv.Decimals = uint32(dec)
	}
	price, priceScale, ok := s.reservePriceUSD(ctx, st.Asset)
	if !ok || !decKnown {
		// Without established decimals the USD figures, and the pool TVL,
		// would be wrong by a power of ten; withhold the valuation instead.
		return rv, nil
	}
	if priceScale != dec {
		// The price was normalised on a different scale than the amounts
		// are divided by; their product is off by 10^|difference|.
		obs.NonstandardDecimalsLockstepMismatchTotal.WithLabelValues("lending_reserve", "").Inc()
		return rv, nil
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
	return rv, suppliedContrib
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
// (nonstandard-decimals projection, else 7): only an amount divided by that same exponent can be multiplied by it.
func (s *Server) reservePriceUSD(ctx context.Context, assetC string) (price string, scale int, ok bool) {
	if canonicalID, ok := s.resolveSACAsset(assetC); ok {
		if a, err := canonical.ParseAsset(canonicalID); err == nil {
			if p, ok := s.lookupUSDPrice(ctx, a); ok {
				return p, aggregate.ResolveDecimals(s.NonstandardDecimals, a), true
			}
		}
	}
	asset, err := canonical.ParseAsset(assetC)
	if err != nil {
		return "", 0, false
	}
	price, ok = s.lookupUSDPrice(ctx, asset)
	return price, aggregate.ResolveDecimals(s.NonstandardDecimals, asset), ok
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
