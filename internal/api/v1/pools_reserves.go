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

// GET /v1/pools/reserves serves CURRENT per-pool reserves plus a constant-product depth
// approximation, read from the lake's contract storage (ADR-0039 pattern).
//
// Honest-coverage contract: reserves are served ONLY for venues whose storage layout is verified
// against the lake, which today is Soroswap alone. Phoenix, Aquarius and Comet are not served
// because their layouts are unverified and we refuse to guess. No history is served: ingest does
// not persist reserves (SyncEvents are consumed transiently at trade decode).

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
	// token0 and inverse); null when either side is empty or either
	// token's decimals declaration could not be read.
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
	// default) when no readable declaration is captured, in which case
	// the row's mid prices are null.
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
//   - pool   (optional): a pool contract C-strkey; restricts to that pool. 404 when it is not a
//     registered Soroswap pair, so coverage is explicit rather than silent.
//   - source (optional): venue filter. Only "soroswap" is accepted; anything else is a 400 naming
//     the venues that serve reserves, not a silently empty response.
//
// Consistency surface: current contract state (tip-adjacent; per-pool as_of_ledger stamps the
// exact state ledger), not closed-bucket.
func (s *Server) handlePoolReserves(w http.ResponseWriter, r *http.Request) {
	if s.Explorer == nil {
		s.explorerUnavailable(w, r)
		return
	}
	if s.SoroswapPairs == nil {
		writeJSON(w, []PoolReservesRow{}, Flags{})
		return
	}

	poolFilter, ok := parsePoolReservesQuery(w, r)
	if !ok {
		return
	}
	assetFilter, ok := parsePoolAssetFilter(w, r)
	if !ok {
		return
	}

	// 10s ceiling: two PK-prefix batched lake lookups (~fast) + one
	// registry scan; generous margin over the observed cost.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	pairs, ok := s.poolReservesPairs(ctx, w, r, poolFilter, assetFilter)
	if !ok {
		return
	}

	states, err := s.Explorer.SoroswapPairReserves(ctx, pairs)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		if handlerTimedOut(ctx, err) {
			// Only a COLD archived-pair verdict snapshot can exceed the budget
			// (clickhouse.ttlLivenessCache); the DEXTVL refresher fills it at startup, so this is
			// the first minutes of a process at most and a retry lands warm: a retryable 503.
			s.logger.Warn("SoroswapPairReserves deadline exceeded (cold TTL-verdict snapshot; background refresh will fill it)")
			writeProblem(w, r, "https://api.stellarindex.io/errors/pool-reserves-timeout",
				"Pool reserves timed out", http.StatusServiceUnavailable,
				"the reserve read didn't return within its budget; retry shortly")
			return
		}
		s.logger.Error("SoroswapPairReserves failed", "err", err)
		writeProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	displays, displaysOK := s.poolReservesDisplays(ctx, states)

	out := make([]PoolReservesRow, 0, len(states))
	for _, pair := range pairs {
		st, ok := states[pair]
		if !ok {
			continue // instance not captured / layout mismatch — absent, never zero
		}
		out = append(out, buildPoolReservesRow(st, displays, displaysOK))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pool < out[j].Pool })
	// ADR-0041 Decision 4: rows carry their own per-pool `as_of_ledger`, but `flags.stale` still
	// reflects lake freshness, since a wedged sink makes EVERY reserve read stale whatever a
	// pool's last change. Stamp it from the cached watermark the other current-state reads use.
	_, stale, _ := s.lakeWatermark(ctx)
	writeJSON(w, out, Flags{Stale: stale, Degraded: !displaysOK}, sourceSoroswap)
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

// poolAssetFilter is a parsed ?asset= on the AMM reserve listings: every
// form the asset trades under, so a pool keyed on any alias matches.
type poolAssetFilter struct {
	contracts map[string]struct{} // Soroban token forms (SEP-41 / SAC C-strkeys)
	classic   map[string]struct{} // CAP-38 reserve forms ("native" / "CODE-ISSUER")
}

func newPoolAssetFilter(asset canonical.Asset) *poolAssetFilter {
	f := &poolAssetFilter{contracts: map[string]struct{}{}, classic: map[string]struct{}{}}
	for _, a := range canonical.AssetAliases(asset) {
		switch a.Type {
		case canonical.AssetSoroban:
			f.contracts[a.ContractID] = struct{}{}
		case canonical.AssetNative, canonical.AssetClassic:
			f.classic[a.String()] = struct{}{}
			// The SAC derives from (code, issuer), so a pair over it matches
			// even when no sac_wrappers entry registers the alias.
			if sac, err := a.SacContractID(); err == nil {
				f.contracts[sac] = struct{}{}
			}
		case canonical.AssetFiat, canonical.AssetCrypto, canonical.AssetRWA, canonical.AssetOracleRaw:
			// Off-chain identities never hold an AMM reserve.
		}
	}
	return f
}

func (f *poolAssetFilter) hasContract(c string) bool {
	_, ok := f.contracts[c]
	return ok
}

func (f *poolAssetFilter) hasClassic(a string) bool {
	_, ok := f.classic[a]
	return ok
}

// parsePoolAssetFilter validates ?asset= (a canonical asset id) and refuses
// it beside ?pool=. nil when absent; ok=false after a problem+json write.
func parsePoolAssetFilter(w http.ResponseWriter, r *http.Request) (*poolAssetFilter, bool) {
	raw := r.URL.Query().Get("asset")
	if raw == "" {
		return nil, true
	}
	if r.URL.Query().Get("pool") != "" {
		writeProblem(w, r, "https://api.stellarindex.io/errors/conflicting-filters",
			"Conflicting filters", http.StatusBadRequest,
			"asset and pool are different filter shapes; pick one.")
		return nil, false
	}
	asset, err := canonical.ParseAsset(raw)
	if err != nil {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-asset-id",
			"Invalid asset", http.StatusBadRequest,
			"asset must be a canonical asset_id (e.g. 'native', 'USDC-G…', a C-strkey); got "+raw+" ("+err.Error()+")")
		return nil, false
	}
	return newPoolAssetFilter(asset), true
}

// poolReservesPairs loads the Soroswap pair registry and applies the
// optional single-pool or asset filter. ok=false after a problem+json
// write (registry failure, or an unregistered pool → honest 404).
func (s *Server) poolReservesPairs(ctx context.Context, w http.ResponseWriter, r *http.Request, poolFilter string, assetFilter *poolAssetFilter) ([]string, bool) {
	registry, err := s.SoroswapPairs.LoadSoroswapPairRegistry(ctx)
	if err != nil {
		if !clientAborted(r, err) {
			s.logger.Error("LoadSoroswapPairRegistry failed", "err", err)
			writeProblemErr(w, r, err, "https://api.stellarindex.io/errors/internal",
				"Internal error", http.StatusInternalServerError, "")
		}
		return nil, false
	}
	pairs := make([]string, 0, len(registry))
	for _, p := range registry {
		if poolFilter != "" && p.PairStrkey != poolFilter {
			continue
		}
		if assetFilter != nil && !assetFilter.hasContract(p.Token0Strkey) && !assetFilter.hasContract(p.Token1Strkey) {
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
// token in the decoded states. Best-effort (a failure never blocks the reserves), but ok=false on
// a fetch failure so callers can tell "the lookup errored" from "the token declares no metadata".
// Conflating them would stamp decimals=7 everywhere, mis-scaling every non-7dp pair's mid price
// by 10^(d-7) on one transient error.
func (s *Server) poolReservesDisplays(ctx context.Context, states map[string]clickhouse.SoroswapPairState) (map[string]clickhouse.TokenDisplayMeta, bool) {
	tokenSet := make(map[string]struct{}, len(states)*2)
	for _, st := range states {
		tokenSet[st.Token0] = struct{}{}
		tokenSet[st.Token1] = struct{}{}
	}
	tokens := make([]string, 0, len(tokenSet))
	for t := range tokenSet {
		tokens = append(tokens, t)
	}
	displays, err := s.Explorer.TokenDisplays(ctx, tokens)
	if err != nil {
		s.logger.Warn("TokenDisplays failed — serving reserves without mid prices this request", "err", err)
		return nil, false
	}
	return displays, true
}

// buildPoolReservesRow assembles one wire row from a decoded pair
// state and the best-effort token display map. Decimals-dependent mid prices are emitted only
// when BOTH tokens' decimals were actually read; otherwise they are null rather than computed
// from the default-7 stamp, which would mis-scale them by 10^(d-7). Reserves and depth are
// base-unit exact and decimals-independent, so always served.
func buildPoolReservesRow(st clickhouse.SoroswapPairState, displays map[string]clickhouse.TokenDisplayMeta, displaysOK bool) PoolReservesRow {
	tok := func(contract string, reserve *big.Int) (PoolReserveToken, bool) {
		t := PoolReserveToken{Contract: contract, Decimals: 7, Reserve: reserve.String()}
		meta, ok := displays[contract]
		if !ok || !meta.HasMeta {
			return t, false
		}
		t.Decimals = meta.Decimals
		t.Symbol = meta.Symbol
		return t, true
	}
	token0, known0 := tok(st.Token0, st.Reserve0)
	token1, known1 := tok(st.Token1, st.Reserve1)
	row := PoolReservesRow{
		Pool:       st.Pair,
		Source:     sourceSoroswap,
		Model:      "constant_product",
		FeeBps:     soroswapFeeBps,
		AsOfLedger: st.Ledger,
		Token0:     token0,
		Token1:     token1,
	}
	if st.Reserve0.Sign() <= 0 || st.Reserve1.Sign() <= 0 {
		row.Depth = []PoolDepthLevel{}
		return row
	}
	if displaysOK && known0 && known1 {
		m01 := midPriceString(st.Reserve1, st.Reserve0, row.Token1.Decimals, row.Token0.Decimals)
		m10 := midPriceString(st.Reserve0, st.Reserve1, row.Token0.Decimals, row.Token1.Decimals)
		row.MidPrice0In1 = &m01
		row.MidPrice1In0 = &m10
	}

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
// Constant-product AMM with the fee on the input side (Uniswap-v2 / Soroswap): for reserves
// (x, y), input Δx and fee f = feeBps/10⁴, the output is
//
//	Δy = y·(1−f)·Δx / (x + (1−f)·Δx)
//
// so the average execution price relative to mid carries slippage
// s = 1 − x·(1−f)/(x + (1−f)·Δx). The largest Δx with slippage ≤ s is
//
//	Δx = x·(s−f)·10⁴ / ((10⁴−f)·(10⁴−s))    [bps-scaled]
//
// Both use exact big.Int arithmetic, floored to base units. The wire's `model` label states the
// assumptions: x·y=k with the whole fee on input, and no other trade between state and execution.

// maxInputWithinSlippage returns the largest input (base units of the
// input-side token, floored) whose average execution price stays within slipBps of mid. Zero when
// slipBps ≤ feeBps (the fee alone exceeds the tier).
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
