package v1

import (
	"context"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// GET /v1/liquidity-pools: CURRENT two-sided reserves plus a constant-product depth approximation
// for native (CAP-38) pools, read-time decoded from the lake's `liquidity_pool` LedgerEntry state.
//
// limit (1-100, default 25) ranks by pool-share trustline count, the only cross-pool size signal
// without USD pricing of arbitrary assets. Data is current ledger-entry state (per-pool
// as_of_ledger), not closed-bucket; flags.stale reflects lake freshness.

// classicAssetDecimals is the fixed scale of every classic asset (7 = stroops); native-pool
// reserves need no per-token decimals lookup, unlike Soroban tokens that self-declare.
const classicAssetDecimals = 7

const (
	nativeLPListingDefault = 25
	nativeLPListingMax     = 100
	// nativeLPListingCap is how many ranked rows the cache holds: the max page size, so any
	// in-range ?limit slices the cache.
	nativeLPListingCap = 100
	// nativeLPListingTTL bounds how often the whole-prefix ranked scan runs. Pool membership and
	// the top-N drift slowly against the ~5s ledger cadence, so a burst costs one scan a minute.
	nativeLPListingTTL = 60 * time.Second
	// nativeLPListingRefreshTimeout bounds ONE detached rescan (and the prewarm call), run by a background goroutine, not a
	// request; the scan is ~0.07s plus a Go decode of ~40k entries, 30s is contention headroom.
	nativeLPListingRefreshTimeout = 30 * time.Second
)

// LiquidityPoolReservesRow is one native (CAP-38) constant-product pool's current reserves; all quantities are stroop decimal strings.
type LiquidityPoolReservesRow struct {
	Pool    string `json:"pool"`     // L-strkey (SEP-23)
	PoolHex string `json:"pool_hex"` // 32-byte LiquidityPoolId, hex (Horizon/SDEX form)
	// Model labels the depth approximation: "constant_product" (x·y=k, fee on input), an estimate
	// from current reserves, not an order book.
	Model      string `json:"model"`
	FeeBps     int    `json:"fee_bps"`      // pool's on-chain fee (30 for every constant-product pool today)
	AsOfLedger uint32 `json:"as_of_ledger"` // ledger of the pool's last state change
	// Trustlines is the number of pool-share trustlines (distinct providers); the listing rank key.
	Trustlines int64 `json:"trustlines"`
	// TotalShares is the pool's total issued share supply (base units).
	TotalShares string                   `json:"total_shares"`
	ReserveA    LiquidityPoolReserveSide `json:"reserve_a"`
	ReserveB    LiquidityPoolReserveSide `json:"reserve_b"`
	// Mid prices are decimals-adjusted ratios (B per A and the inverse); null when a side is empty.
	MidPriceAInB *string                   `json:"mid_price_a_in_b"`
	MidPriceBInA *string                   `json:"mid_price_b_in_a"`
	Depth        []LiquidityPoolDepthLevel `json:"depth"` // empty when either reserve is zero
}

// LiquidityPoolReserveSide is one side of a native pool: the classic asset's id and its reserve.
type LiquidityPoolReserveSide struct {
	Asset    string `json:"asset"`    // canonical asset_id ("native" | "CODE-ISSUER")
	Decimals uint32 `json:"decimals"` // always 7 for classic assets
	Reserve  string `json:"reserve"`  // base units (stroops), decimal string
}

// LiquidityPoolDepthLevel is the depth estimate at one slippage tier, using the Soroban AMM depth table's exact-integer constant-product model.
type LiquidityPoolDepthLevel struct {
	SlippagePct string        `json:"slippage_pct"`
	AssetAIn    PoolDepthSide `json:"asset_a_in"` // selling asset A for asset B
	AssetBIn    PoolDepthSide `json:"asset_b_in"` // selling asset B for asset A
}

// handleLiquidityPools serves GET /v1/liquidity-pools.
func (s *Server) handleLiquidityPools(w http.ResponseWriter, r *http.Request) {
	if s.Explorer == nil {
		s.explorerUnavailable(w, r)
		return
	}

	// 12s ceiling: a single-pool lookup is ~ms and the cached listing scan ~0.07s plus a decode.
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	assetFilter, ok := parsePoolAssetFilter(w, r)
	if !ok {
		return
	}
	if poolFilter := r.URL.Query().Get("pool"); poolFilter != "" {
		s.serveOneLiquidityPool(ctx, w, r, poolFilter)
		return
	}

	limit, ok := parseExplorerLimit(w, r, nativeLPListingDefault, nativeLPListingMax)
	if !ok {
		return
	}
	rows, all, degraded, err := s.nativeLPSnapshot(ctx)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("NativeLiquidityPoolsRanked failed", "err", err)
		writeProblemErr(w, r, err, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	if assetFilter != nil {
		rows = nativeLPRowsForAsset(all, assetFilter, limit)
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	_, stale, _ := s.lakeWatermark(ctx)
	writeJSON(w, rows, Flags{Stale: stale, Degraded: degraded})
}

// serveOneLiquidityPool handles the ?pool= path: validate the id, look it up in the lake (404 when
// absent) and return the one row with its full depth table.
func (s *Server) serveOneLiquidityPool(ctx context.Context, w http.ResponseWriter, r *http.Request, poolFilter string) {
	if !canonical.IsLiquidityPool(poolFilter) && !isHex64(poolFilter) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-pool",
			"Invalid pool", http.StatusBadRequest,
			"pool must be a native liquidity-pool id — an L-strkey (SEP-23) or 32-byte hex.")
		return
	}
	states, err := s.Explorer.NativeLiquidityPoolReserves(ctx, []string{poolFilter})
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("NativeLiquidityPoolReserves failed", "err", err)
		writeProblemErr(w, r, err, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	if len(states) == 0 {
		writeProblem(w, r, "https://api.stellarindex.io/errors/unknown-pool",
			"Unknown pool", http.StatusNotFound,
			"not a captured native (CAP-38) liquidity pool; only pools present in the certified lake are served.")
		return
	}
	out := make([]LiquidityPoolReservesRow, 0, 1)
	for _, st := range states {
		out = append(out, buildLiquidityPoolRow(st))
	}
	_, stale, _ := s.lakeWatermark(ctx)
	writeJSON(w, out, Flags{Stale: stale})
}

// nativeLPListing returns the cached top-N ranked native-pool listing. STALE-SERVE: an existing entry is ALWAYS returned at
// once and a lapsed one kicks ONE detached refresh (holding nativeLPMu would queue every caller behind a whole-prefix lake
// scan); only a never-computed process fills inline. flags.stale is NOT set from cache age (it means lake freshness); cache
// health is stellarindex_explorer_swr_refresh_total{cache="native_lp_listing"}.
func (s *Server) nativeLPListing(ctx context.Context) ([]LiquidityPoolReservesRow, error) {
	rows, _, _, err := s.nativeLPSnapshot(ctx)
	return rows, err
}

// nativeLPSnapshot is [Server.nativeLPListing] plus every captured pool's state in ranked order (the ?asset= filter needs pools
// beyond the top N), and whether rows are carried forward: past the TTL, or last-good after a failed scan.
func (s *Server) nativeLPSnapshot(ctx context.Context) ([]LiquidityPoolReservesRow, []clickhouse.NativeLiquidityPoolState, bool, error) {
	s.nativeLPMu.Lock()
	rows, all, fetched := s.nativeLPCached, s.nativeLPAll, s.nativeLPFetched
	s.nativeLPMu.Unlock()

	if !fetched.IsZero() {
		lapsed := time.Since(fetched) >= nativeLPListingTTL
		if lapsed {
			s.refreshNativeLPListing() //nolint:contextcheck // intentional detach — the rescan must outlive the request that noticed the lapse (see refreshNativeLPListing)
		}
		return rows, all, lapsed, nil
	}
	return s.fillNativeLPListing(ctx)
}

// nativeLPRowsForAsset returns up to limit ranked pools holding any alias
// form of the filtered asset on either side.
func nativeLPRowsForAsset(all []clickhouse.NativeLiquidityPoolState, f *poolAssetFilter, limit int) []LiquidityPoolReservesRow {
	out := []LiquidityPoolReservesRow{}
	for _, st := range all {
		if len(out) >= limit {
			break
		}
		if f.hasClassic(st.AssetA) || f.hasClassic(st.AssetB) {
			out = append(out, buildLiquidityPoolRow(st))
		}
	}
	return out
}

// fillNativeLPListing runs the ranked scan and stores the result. The fill mutex collapses a
// burst of concurrent COLD callers onto one scan and is never held while a warm entry is served.
func (s *Server) fillNativeLPListing(ctx context.Context) ([]LiquidityPoolReservesRow, []clickhouse.NativeLiquidityPoolState, bool, error) {
	s.nativeLPFillMu.Lock()
	defer s.nativeLPFillMu.Unlock()

	// Double-check: a caller queued behind another's fill serves that result, not a rescan.
	s.nativeLPMu.Lock()
	rows, all, fetched := s.nativeLPCached, s.nativeLPAll, s.nativeLPFetched
	s.nativeLPMu.Unlock()
	if !fetched.IsZero() && time.Since(fetched) < nativeLPListingTTL {
		return rows, all, false, nil
	}

	// Uncapped: the scan ranks every pool anyway and ?asset= needs the tail beyond the top N.
	start := time.Now()
	states, err := s.Explorer.NativeLiquidityPoolsRanked(ctx, 0)
	obs.ObserveExplorerSWRRefresh("native_lp_listing", start, err)
	if err != nil {
		if !fetched.IsZero() {
			return rows, all, true, nil // last-good beats failing the request
		}
		return nil, nil, false, err
	}
	top := states
	if len(top) > nativeLPListingCap {
		top = top[:nativeLPListingCap]
	}
	out := make([]LiquidityPoolReservesRow, 0, len(top))
	for _, st := range top {
		out = append(out, buildLiquidityPoolRow(st))
	}
	s.nativeLPMu.Lock()
	s.nativeLPCached, s.nativeLPAll, s.nativeLPFetched = out, states, time.Now()
	s.nativeLPMu.Unlock()
	return out, states, false, nil
}

// refreshNativeLPListing kicks ONE detached rescan (no-op while one runs); detached so no visitor waits, as a request-bound
// slow scan would die with the request and the next visitor would pay again.
func (s *Server) refreshNativeLPListing() {
	if !s.nativeLPRefreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		// The deferred Store releases the CAS gate on every path including a recovered panic,
		// which leaves the last-good entry served and lets the NEXT visitor kick a fresh rescan.
		defer s.nativeLPRefreshing.Store(false)
		defer worker.Recover(s.logger, "api-native-lp-listing-refresh")
		ctx, cancel := context.WithTimeout(context.Background(), nativeLPListingRefreshTimeout)
		defer cancel()
		if _, _, _, err := s.fillNativeLPListing(ctx); err != nil {
			// The previous entry is still served; the refresh metric shows a dying refresher.
			s.logger.Warn("native liquidity-pool listing detached refresh failed (serving last good)", "err", err)
		}
	}()
}

// PrewarmNativeLiquidityPools primes the ranked listing by calling the EXACT function the handler calls, so prewarmed and requested keys cannot drift.
func (s *Server) PrewarmNativeLiquidityPools(ctx context.Context) {
	if s.Explorer == nil || ctx.Err() != nil {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, nativeLPListingRefreshTimeout)
	defer cancel()
	if _, err := s.nativeLPListing(pctx); err != nil {
		s.logger.Debug("prewarm native liquidity-pool listing failed", "err", err)
	}
}

// buildLiquidityPoolRow assembles one wire row, computing mid prices and depth with the shared
// exact-integer helpers (pools_reserves.go). The fee is per-pool (Params.Fee), not Soroswap's constant.
func buildLiquidityPoolRow(st clickhouse.NativeLiquidityPoolState) LiquidityPoolReservesRow {
	row := LiquidityPoolReservesRow{
		Pool:        st.PoolStrkey,
		PoolHex:     st.PoolHex,
		Model:       "constant_product",
		FeeBps:      int(st.FeeBps),
		AsOfLedger:  st.Ledger,
		Trustlines:  st.Trustlines,
		TotalShares: st.TotalShares.String(),
		ReserveA:    LiquidityPoolReserveSide{Asset: st.AssetA, Decimals: classicAssetDecimals, Reserve: st.ReserveA.String()},
		ReserveB:    LiquidityPoolReserveSide{Asset: st.AssetB, Decimals: classicAssetDecimals, Reserve: st.ReserveB.String()},
	}
	if st.ReserveA.Sign() <= 0 || st.ReserveB.Sign() <= 0 {
		row.Depth = []LiquidityPoolDepthLevel{}
		return row
	}
	feeBps := int64(st.FeeBps)
	mAB := midPriceString(st.ReserveB, st.ReserveA, classicAssetDecimals, classicAssetDecimals)
	mBA := midPriceString(st.ReserveA, st.ReserveB, classicAssetDecimals, classicAssetDecimals)
	row.MidPriceAInB = &mAB
	row.MidPriceBInA = &mBA

	row.Depth = make([]LiquidityPoolDepthLevel, 0, len(poolDepthSlippagesBps))
	for _, slipBps := range poolDepthSlippagesBps {
		inA := maxInputWithinSlippage(st.ReserveA, feeBps, slipBps)
		inB := maxInputWithinSlippage(st.ReserveB, feeBps, slipBps)
		row.Depth = append(row.Depth, LiquidityPoolDepthLevel{
			SlippagePct: bpsToPctString(slipBps),
			AssetAIn: PoolDepthSide{
				MaxInput: inA.String(),
				Output:   constantProductOutput(st.ReserveA, st.ReserveB, inA, feeBps).String(),
			},
			AssetBIn: PoolDepthSide{
				MaxInput: inB.String(),
				Output:   constantProductOutput(st.ReserveB, st.ReserveA, inB, feeBps).String(),
			},
		})
	}
	return row
}

// isHex64 reports whether s is exactly 64 hex digits — a raw 32-byte
// liquidity-pool id in Horizon/SDEX form.
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
