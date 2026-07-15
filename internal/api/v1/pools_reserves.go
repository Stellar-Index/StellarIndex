package v1

import (
	"context"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// sourceSoroswap is the only venue whose pool-storage layout is
// verified for reserve serving today (matches soroswap.SourceName).
const sourceSoroswap = "soroswap"

// GET /v1/pools/reserves — CURRENT per-pool-contract reserves + a
// constant-product depth approximation, read from the certified lake's
// contract storage (ADR-0039 pattern, same as /v1/lending/pools/{pool}/
// reserves).
//
// Honest-coverage contract: reserves are served ONLY for venues whose
// pool-contract storage layout we have verified against the lake.
// Today that is Soroswap alone (u32-keyed pair instance storage,
// verified 2026-07-05). Phoenix / Aquarius / Comet pools are NOT
// served — their layouts are unverified and we refuse to guess.
// Historical reserve series are not served anywhere: reserves are not
// persisted by ingest (Soroswap SyncEvents are consumed transiently at
// trade decode), so only the current state is honestly available.

// soroswapFeeBps is Soroswap's Uniswap-v2-style total swap fee (0.3%,
// taken on the input side). A venue constant, not read from chain.
const soroswapFeeBps = 30

// poolDepthSlippagesBps are the slippage tiers the depth table is
// computed for. Tiers at or below the swap fee are meaningless (every
// trade pays the fee), so the smallest tier sits above soroswapFeeBps.
var poolDepthSlippagesBps = []int64{50, 100, 200}

// PoolReservesRow is the wire shape for one AMM pool contract's
// current reserve state. All token quantities are base-unit decimal
// strings (ADR-0003 — reserves are i128; never floats).
type PoolReservesRow struct {
	Pool   string `json:"pool"`   // pool (pair) contract C-strkey
	Source string `json:"source"` // venue; "soroswap" is the only venue served today
	// Model labels the AMM depth approximation: "constant_product"
	// (x·y=k, fee on input). The depth table is a model-derived
	// estimate from current reserves — not an order book.
	Model      string           `json:"model"`
	FeeBps     int              `json:"fee_bps"`
	AsOfLedger uint32           `json:"as_of_ledger"` // ledger of the pool's last state change
	Token0     PoolReserveToken `json:"token0"`
	Token1     PoolReserveToken `json:"token1"`
	// Mid prices are decimals-adjusted display ratios (token1 per
	// token0 and inverse); null when either side is empty.
	MidPrice0In1 *string          `json:"mid_price_0_in_1"`
	MidPrice1In0 *string          `json:"mid_price_1_in_0"`
	Depth        []PoolDepthLevel `json:"depth"` // empty when either reserve is zero
}

// PoolReserveToken is one side of a pool: token identity + reserve.
type PoolReserveToken struct {
	Contract string `json:"contract"`
	// Symbol is best-effort display metadata from the token's on-chain
	// METADATA entry; omitted when the token declares none. It is
	// self-declared by the token contract — NOT a verified identity.
	Symbol string `json:"symbol,omitempty"`
	// Decimals is the token's on-chain declaration; 7 (the SAC
	// default) when no readable declaration is captured.
	Decimals uint32 `json:"decimals"`
	Reserve  string `json:"reserve"` // base units, i128 decimal string
}

// PoolDepthLevel is the depth estimate at one slippage tier: the
// largest input trade (per direction) whose AVERAGE execution price
// stays within slippage_pct of the mid price, under the
// constant-product model with the pool fee applied on input.
type PoolDepthLevel struct {
	SlippagePct string        `json:"slippage_pct"`
	Token0In    PoolDepthSide `json:"token0_in"` // selling token0 for token1
	Token1In    PoolDepthSide `json:"token1_in"` // selling token1 for token0
}

// PoolDepthSide is one direction's depth at a tier. Base-unit decimal
// strings of the respective tokens.
type PoolDepthSide struct {
	MaxInput string `json:"max_input"`
	Output   string `json:"output"`
}

// handlePoolReserves serves GET /v1/pools/reserves.
//
// Query params:
//   - pool   (optional): a pool contract C-strkey; restricts to that
//     one pool. 404 when the contract isn't a registered Soroswap
//     pair — per-venue coverage is explicit, not silent.
//   - source (optional): venue filter. "soroswap" is the only value
//     accepted today; anything else is a 400 naming the venues that
//     do serve reserves, so coverage limits are visible at the API
//     edge rather than via silently-empty responses.
//
// Consistency surface: current contract state (tip-adjacent, per-pool
// as_of_ledger stamps the exact state ledger) — not closed-bucket.
func (s *Server) handlePoolReserves(w http.ResponseWriter, r *http.Request) {
	if s.explorer == nil {
		s.explorerUnavailable(w, r)
		return
	}
	if s.soroswapPairs == nil {
		writeJSON(w, []PoolReservesRow{}, Flags{})
		return
	}

	poolFilter, ok := parsePoolReservesQuery(w, r)
	if !ok {
		return
	}

	// 10s ceiling: two PK-prefix batched lake lookups (~fast) + one
	// registry scan; generous margin over the observed cost.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	pairs, ok := s.poolReservesPairs(ctx, w, r, poolFilter)
	if !ok {
		return
	}

	states, err := s.explorer.SoroswapPairReserves(ctx, pairs)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("SoroswapPairReserves failed", "err", err)
		writeProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	displays := s.poolReservesDisplays(ctx, states)

	out := make([]PoolReservesRow, 0, len(states))
	for _, pair := range pairs {
		st, ok := states[pair]
		if !ok {
			continue // instance not captured / layout mismatch — absent, never zero
		}
		out = append(out, buildPoolReservesRow(st, displays))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pool < out[j].Pool })
	// ADR-0041 Decision 4: each row already carries its exact per-pool
	// `as_of_ledger` (the pool's last state-change ledger — more precise
	// than the cached watermark), but the envelope's `flags.stale` still
	// reflects lake freshness: a wedged galexie→ClickHouse sink makes
	// EVERY reserve read stale regardless of when a given pool last
	// changed. Stamp it from the same cached watermark the other
	// current-state reads use.
	_, stale, _ := s.lakeWatermark(ctx)
	writeJSON(w, out, Flags{Stale: stale}, sourceSoroswap)
}

// parsePoolReservesQuery validates ?source= and ?pool=. ok=false after
// a problem+json write.
func parsePoolReservesQuery(w http.ResponseWriter, r *http.Request) (poolFilter string, ok bool) {
	if src := r.URL.Query().Get("source"); src != "" && src != sourceSoroswap {
		writeProblem(w, r, "https://api.stellarindex.io/errors/unsupported-reserves-source",
			"Reserves not served for this venue", http.StatusBadRequest,
			"per-pool reserves are served for source=soroswap only today — other venues' pool-storage layouts are not yet verified (ADR-0039).")
		return "", false
	}
	poolFilter = r.URL.Query().Get("pool")
	if poolFilter != "" && !canonical.IsContractID(poolFilter) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-pool",
			"Invalid pool", http.StatusBadRequest, "pool must be a C-strkey contract id")
		return "", false
	}
	return poolFilter, true
}

// poolReservesPairs loads the Soroswap pair registry and applies the
// optional single-pool filter. ok=false after a problem+json write
// (registry failure, or an unregistered pool → honest 404).
func (s *Server) poolReservesPairs(ctx context.Context, w http.ResponseWriter, r *http.Request, poolFilter string) ([]string, bool) {
	registry, err := s.soroswapPairs.LoadSoroswapPairRegistry(ctx)
	if err != nil {
		if !clientAborted(r, err) {
			s.logger.Error("LoadSoroswapPairRegistry failed", "err", err)
			writeProblem(w, r, "https://api.stellarindex.io/errors/internal",
				"Internal error", http.StatusInternalServerError, "")
		}
		return nil, false
	}
	pairs := make([]string, 0, len(registry))
	for _, p := range registry {
		if poolFilter != "" && p.PairStrkey != poolFilter {
			continue
		}
		pairs = append(pairs, p.PairStrkey)
	}
	if poolFilter != "" && len(pairs) == 0 {
		writeProblem(w, r, "https://api.stellarindex.io/errors/unknown-pool",
			"Unknown pool", http.StatusNotFound,
			"not a registered Soroswap pair contract; per-pool reserves are served for Soroswap pairs only today.")
		return nil, false
	}
	return pairs, true
}

// poolReservesDisplays batch-fetches token display metadata for every
// token appearing in the decoded states. Best-effort: a failure
// degrades symbols/decimals to defaults, never the reserves.
func (s *Server) poolReservesDisplays(ctx context.Context, states map[string]clickhouse.SoroswapPairState) map[string]clickhouse.TokenDisplayMeta {
	tokenSet := make(map[string]struct{}, len(states)*2)
	for _, st := range states {
		tokenSet[st.Token0] = struct{}{}
		tokenSet[st.Token1] = struct{}{}
	}
	tokens := make([]string, 0, len(tokenSet))
	for t := range tokenSet {
		tokens = append(tokens, t)
	}
	displays, err := s.explorer.TokenDisplays(ctx, tokens)
	if err != nil {
		s.logger.Warn("TokenDisplays failed", "err", err)
		return nil
	}
	return displays
}

// buildPoolReservesRow assembles one wire row from a decoded pair
// state + the best-effort token display map.
func buildPoolReservesRow(st clickhouse.SoroswapPairState, displays map[string]clickhouse.TokenDisplayMeta) PoolReservesRow {
	tok := func(contract string, reserve *big.Int) PoolReserveToken {
		t := PoolReserveToken{Contract: contract, Decimals: 7, Reserve: reserve.String()}
		if meta, ok := displays[contract]; ok && meta.HasMeta {
			t.Decimals = meta.Decimals
			t.Symbol = meta.Symbol
		}
		return t
	}
	row := PoolReservesRow{
		Pool:       st.Pair,
		Source:     sourceSoroswap,
		Model:      "constant_product",
		FeeBps:     soroswapFeeBps,
		AsOfLedger: st.Ledger,
		Token0:     tok(st.Token0, st.Reserve0),
		Token1:     tok(st.Token1, st.Reserve1),
	}
	if st.Reserve0.Sign() <= 0 || st.Reserve1.Sign() <= 0 {
		row.Depth = []PoolDepthLevel{}
		return row
	}
	m01 := midPriceString(st.Reserve1, st.Reserve0, row.Token1.Decimals, row.Token0.Decimals)
	m10 := midPriceString(st.Reserve0, st.Reserve1, row.Token0.Decimals, row.Token1.Decimals)
	row.MidPrice0In1 = &m01
	row.MidPrice1In0 = &m10

	row.Depth = make([]PoolDepthLevel, 0, len(poolDepthSlippagesBps))
	for _, slipBps := range poolDepthSlippagesBps {
		in0 := maxInputWithinSlippage(st.Reserve0, soroswapFeeBps, slipBps)
		in1 := maxInputWithinSlippage(st.Reserve1, soroswapFeeBps, slipBps)
		row.Depth = append(row.Depth, PoolDepthLevel{
			SlippagePct: bpsToPctString(slipBps),
			Token0In: PoolDepthSide{
				MaxInput: in0.String(),
				Output:   constantProductOutput(st.Reserve0, st.Reserve1, in0, soroswapFeeBps).String(),
			},
			Token1In: PoolDepthSide{
				MaxInput: in1.String(),
				Output:   constantProductOutput(st.Reserve1, st.Reserve0, in1, soroswapFeeBps).String(),
			},
		})
	}
	return row
}

// ─── Depth math — exact integer/rational arithmetic (ADR-0003) ────
//
// Constant-product AMM with the fee taken on the input side
// (Uniswap-v2 / Soroswap): for reserves (x, y), input Δx and fee
// f = feeBps/10⁴, the output is
//
//	Δy = y·(1−f)·Δx / (x + (1−f)·Δx)
//
// so the AVERAGE execution price (Δy/Δx) relative to the mid price
// (y/x) carries slippage s = 1 − x·(1−f)/(x + (1−f)·Δx). Solving for
// the largest Δx with slippage ≤ s:
//
//	Δx = x·(s−f)·10⁴ / ((10⁴−f)·(10⁴−s))    [bps-scaled]
//
// Both are evaluated in exact big.Int arithmetic, floored to base
// units. Assumptions the wire labels via `model`: the pool follows
// x·y=k with the whole fee on input, and no other trade lands between
// the state ledger and execution.

// maxInputWithinSlippage returns the largest input (base units of the
// input-side token, floored) whose average execution price stays
// within slipBps of the mid price. Zero when slipBps ≤ feeBps (the
// fee alone exceeds the tier).
//
//nolint:unparam // feeBps always receives soroswapFeeBps today (single-venue coverage); the parameter keeps the model math venue-generic and the fee assumption explicit at every call site
func maxInputWithinSlippage(reserveIn *big.Int, feeBps, slipBps int64) *big.Int {
	if slipBps <= feeBps || reserveIn.Sign() <= 0 {
		return new(big.Int)
	}
	num := new(big.Int).Mul(reserveIn, big.NewInt(slipBps-feeBps))
	num.Mul(num, big.NewInt(10000))
	den := new(big.Int).Mul(big.NewInt(10000-feeBps), big.NewInt(10000-slipBps))
	return num.Quo(num, den)
}

// constantProductOutput returns Δy = y·(1−f)·Δx / (x + (1−f)·Δx),
// floored to base units. Zero for a zero/negative input or empty pool.
//
//nolint:unparam // feeBps always receives soroswapFeeBps today — same rationale as maxInputWithinSlippage
func constantProductOutput(reserveIn, reserveOut, dx *big.Int, feeBps int64) *big.Int {
	if dx.Sign() <= 0 || reserveIn.Sign() <= 0 || reserveOut.Sign() <= 0 {
		return new(big.Int)
	}
	gammaDx := new(big.Int).Mul(dx, big.NewInt(10000-feeBps)) // (1−f)·Δx · 10⁴
	num := new(big.Int).Mul(reserveOut, gammaDx)
	den := new(big.Int).Mul(reserveIn, big.NewInt(10000))
	den.Add(den, gammaDx)
	return num.Quo(num, den)
}

// midPriceString renders the decimals-adjusted mid price
// (reserveNum/10^decNum) ÷ (reserveDen/10^decDen) as a decimal string:
// 18 fractional digits, trailing zeros trimmed.
func midPriceString(reserveNum, reserveDen *big.Int, decNum, decDen uint32) string {
	num := new(big.Int).Mul(reserveNum, pow10(decDen))
	den := new(big.Int).Mul(reserveDen, pow10(decNum))
	rat := new(big.Rat).SetFrac(num, den)
	s := rat.FloatString(18)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

func pow10(d uint32) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(d)), nil)
}

// bpsToPctString renders basis points as a percent string ("50" →
// "0.5", "200" → "2").
func bpsToPctString(bps int64) string {
	whole := bps / 100
	frac := bps % 100
	if frac == 0 {
		return strconv.FormatInt(whole, 10)
	}
	s := strconv.FormatInt(whole, 10) + "." + strings.TrimRight(strconv.FormatInt(frac+100, 10)[1:], "0")
	return s
}
