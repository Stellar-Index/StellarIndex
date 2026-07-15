package v1

import (
	"context"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// GET /v1/liquidity-pools — CURRENT two-sided reserves + a
// constant-product depth approximation for Stellar's protocol-native
// (CAP-38) liquidity pools, read from the `liquidity_pool` LedgerEntry
// in the certified lake's current-state table (ADR-0039 pattern, the
// same read-time-decode design as /v1/pools/reserves and
// /v1/lending/pools/{pool}/reserves).
//
// Native pools carry TWO-sided reserves on-chain (ReserveA + ReserveB
// in the entry), so — unlike the Soroban AMM path, where reserves are
// consumed transiently at trade decode and persisted nowhere — the full
// per-pool reserve state is honestly available for every native pool.
// (The supply-verification `lp_reserve_observations` path only records
// the operator-watched side of watched-asset pools; this endpoint reads
// both sides of every pool straight from the ledger entry.)
//
// Query params:
//   - pool (optional): a native pool id — L-strkey (SEP-23) or 32-byte
//     hex. Restricts the response to that one pool; 404 when the id
//     isn't a captured native pool.
//   - limit (optional, listing only): 1-100, default 25. The listing
//     returns the top-N native pools ranked by pool-share trustline
//     count (number of liquidity providers) — the only cross-pool-
//     comparable size signal without USD pricing of arbitrary pool
//     assets.
//
// Consistency surface: current ledger-entry state (tip-adjacent, per-
// pool `as_of_ledger` stamps the exact state ledger) — not closed-
// bucket. ADR-0041 Decision 4: `flags.stale` reflects lake freshness.

// classicAssetDecimals is the fixed decimal scale of every classic
// Stellar asset (7 = stroops). Native-pool reserves are always at this
// scale, so there is no per-token decimals lookup as on the Soroban AMM
// path (Soroban tokens self-declare varying decimals).
const classicAssetDecimals = 7

const (
	nativeLPListingDefault = 25
	nativeLPListingMax     = 100
	// nativeLPListingCap is how many ranked rows the cache holds — the
	// endpoint's max page size, so any in-range ?limit slices the cache.
	nativeLPListingCap = 100
	// nativeLPListingTTL bounds how often the whole-prefix ranked scan
	// runs. 60s ≫ the ~5s ledger cadence — pool membership + the top-N
	// ranking drift slowly — and turns a burst of listing requests into
	// at most one lake scan a minute.
	nativeLPListingTTL = 60 * time.Second
)

// LiquidityPoolReservesRow is the wire shape for one native (CAP-38)
// constant-product pool's current reserve state. All token quantities
// are base-unit (stroop) decimal strings (ADR-0003).
type LiquidityPoolReservesRow struct {
	Pool    string `json:"pool"`     // L-strkey (SEP-23)
	PoolHex string `json:"pool_hex"` // 32-byte LiquidityPoolId, hex (Horizon/SDEX form)
	// Model labels the AMM depth approximation: "constant_product"
	// (x·y=k, fee on input) — a model-derived estimate from current
	// reserves, not an order book.
	Model      string `json:"model"`
	FeeBps     int    `json:"fee_bps"`      // pool's on-chain fee (30 for every constant-product pool today)
	AsOfLedger uint32 `json:"as_of_ledger"` // ledger of the pool's last state change
	// Trustlines is the number of pool-share trustlines — how many
	// distinct liquidity providers hold a stake. The listing rank key.
	Trustlines int64 `json:"trustlines"`
	// TotalShares is the pool's total issued share supply (base units).
	TotalShares string                   `json:"total_shares"`
	ReserveA    LiquidityPoolReserveSide `json:"reserve_a"`
	ReserveB    LiquidityPoolReserveSide `json:"reserve_b"`
	// Mid prices are decimals-adjusted display ratios (B per A and the
	// inverse); null when either side is empty.
	MidPriceAInB *string                   `json:"mid_price_a_in_b"`
	MidPriceBInA *string                   `json:"mid_price_b_in_a"`
	Depth        []LiquidityPoolDepthLevel `json:"depth"` // empty when either reserve is zero
}

// LiquidityPoolReserveSide is one side of a native pool: the classic
// asset's canonical id + its reserve.
type LiquidityPoolReserveSide struct {
	Asset    string `json:"asset"`    // canonical asset_id ("native" | "CODE-ISSUER")
	Decimals uint32 `json:"decimals"` // always 7 for classic assets
	Reserve  string `json:"reserve"`  // base units (stroops), decimal string
}

// LiquidityPoolDepthLevel is the depth estimate at one slippage tier
// (the same constant-product-with-fee-on-input model the Soroban AMM
// depth table uses, reusing its exact-integer helpers).
type LiquidityPoolDepthLevel struct {
	SlippagePct string        `json:"slippage_pct"`
	AssetAIn    PoolDepthSide `json:"asset_a_in"` // selling asset A for asset B
	AssetBIn    PoolDepthSide `json:"asset_b_in"` // selling asset B for asset A
}

// handleLiquidityPools serves GET /v1/liquidity-pools.
func (s *Server) handleLiquidityPools(w http.ResponseWriter, r *http.Request) {
	if s.explorer == nil {
		s.explorerUnavailable(w, r)
		return
	}

	// 12s ceiling: a single-pool PK lookup is ~ms; the listing's
	// whole-`liquidity_pool`-prefix scan is ~0.07s on r1 + a Go decode
	// of ~40k entries — generous margin, and the result is cached.
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	if poolFilter := r.URL.Query().Get("pool"); poolFilter != "" {
		s.serveOneLiquidityPool(ctx, w, r, poolFilter)
		return
	}

	limit, ok := parseExplorerLimit(w, r, nativeLPListingDefault, nativeLPListingMax)
	if !ok {
		return
	}
	rows, err := s.nativeLPListing(ctx)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("NativeLiquidityPoolsRanked failed", "err", err)
		writeProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	_, stale, _ := s.lakeWatermark(ctx)
	writeJSON(w, rows, Flags{Stale: stale})
}

// serveOneLiquidityPool handles the ?pool= single-pool path: validate
// the id, look it up in the lake, 404 when absent, else return the one
// row with its full depth table.
func (s *Server) serveOneLiquidityPool(ctx context.Context, w http.ResponseWriter, r *http.Request, poolFilter string) {
	if !canonical.IsLiquidityPool(poolFilter) && !isHex64(poolFilter) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-pool",
			"Invalid pool", http.StatusBadRequest,
			"pool must be a native liquidity-pool id — an L-strkey (SEP-23) or 32-byte hex.")
		return
	}
	states, err := s.explorer.NativeLiquidityPoolReserves(ctx, []string{poolFilter})
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("NativeLiquidityPoolReserves failed", "err", err)
		writeProblem(w, r, "https://api.stellarindex.io/errors/internal",
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

// nativeLPListing returns the cached top-N ranked native-pool listing,
// refreshing it at most every nativeLPListingTTL. The mutex is held
// across the refresh so a burst of concurrent cold requests collapses
// onto one whole-prefix lake scan (same posture as lakeWatermark). On a
// refresh error with a prior value cached, the stale value is served
// rather than failing the request.
func (s *Server) nativeLPListing(ctx context.Context) ([]LiquidityPoolReservesRow, error) {
	s.nativeLPMu.Lock()
	defer s.nativeLPMu.Unlock()
	if !s.nativeLPFetched.IsZero() && time.Since(s.nativeLPFetched) < nativeLPListingTTL {
		return s.nativeLPCached, nil
	}
	states, err := s.explorer.NativeLiquidityPoolsRanked(ctx, nativeLPListingCap)
	if err != nil {
		if !s.nativeLPFetched.IsZero() {
			return s.nativeLPCached, nil
		}
		return nil, err
	}
	rows := make([]LiquidityPoolReservesRow, 0, len(states))
	for _, st := range states {
		rows = append(rows, buildLiquidityPoolRow(st))
	}
	s.nativeLPCached, s.nativeLPFetched = rows, time.Now()
	return rows, nil
}

// buildLiquidityPoolRow assembles one wire row from a decoded native
// pool state, computing mid prices + the depth table with the shared
// exact-integer constant-product helpers (pools_reserves.go). The pool
// fee is per-pool (Params.Fee), not the Soroswap venue constant.
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
