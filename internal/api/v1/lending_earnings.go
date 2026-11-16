package v1

import (
	"context"
	"math/big"
	"net/http"
	"slices"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// lendingEarningsReader is the Postgres history behind GET /v1/lending/accounts/{account}/earnings.
type lendingEarningsReader interface {
	BlendPositionsByUser(ctx context.Context, address string) ([]timescale.BlendPositionFold, error)
	BLNTPositionsByUser(ctx context.Context, address string) ([]timescale.BlendPositionFold, error)
	BlendBackstopSharesByUser(ctx context.Context, address string) ([]timescale.BlendBackstopFold, error)
	BLNTBackstopByUser(ctx context.Context, address string) ([]timescale.BLNTBackstopFold, error)
	LendingAccountFlows(ctx context.Context, address string) (timescale.LendingAccountFlows, error)
}

// lendingAccountStateReader is the lake read of an account's current pool and backstop positions.
type lendingAccountStateReader interface {
	lendingBackstopReader
	LendingAccountPositions(ctx context.Context, pool, account string) (clickhouse.LendingAccountPositions, error)
	LendingAccountBackstop(ctx context.Context, backstop, pool, account string, tiers []string) (map[string]*big.Int, error)
}

var (
	_ lendingEarningsReader     = (*timescale.Store)(nil)
	_ lendingAccountStateReader = (*clickhouse.ExplorerReader)(nil)
)

// Earnings sides and the reasons a leg's accrued figure is withheld.
const (
	earningsSideSupply   = "supply"
	earningsSideBorrow   = "borrow"
	earningsSideBackstop = "backstop"

	earningsReasonAuctionMoved     = "auction_moved"
	earningsReasonFoldInconsistent = "fold_inconsistent"
	earningsReasonOnchainUnread    = "onchain_unread"
	earningsReasonBalanceMismatch  = "balance_mismatch"
	earningsReasonRateUnread       = "rate_unread"
)

const lendingEarningsNote = "accrued is current_value - net_flow, served only when the token balance folded from this account's " +
	"events equals its current on-chain balance (reconciled); otherwise it is null and reason names why. net_flow is " +
	"underlying deposited minus withdrawn (supply), borrowed minus repaid (borrow), or backstop tokens deposited minus " +
	"withdrawn (backstop); a backstop's accrued includes emissions claimed into it. Emission claims are sums of the " +
	"claim events projected so far and are a lower bound."

// LendingEarningsView is the GET /v1/lending/accounts/{account}/earnings body.
type LendingEarningsView struct {
	Account    string                  `json:"account"`
	Legs       []LendingEarningsLeg    `json:"legs"`
	Emissions  []LendingEmissionsClaim `json:"emissions"`
	Note       string                  `json:"note"`
	AsOfLedger uint32                  `json:"as_of_ledger,omitempty"`
}

// LendingEarningsLeg is one (pool, asset, side) or (pool, tier) backstop position; amounts are base-unit decimal strings.
type LendingEarningsLeg struct {
	Protocol      string  `json:"protocol"` // blend | blnt
	Pool          string  `json:"pool"`
	Side          string  `json:"side"` // supply | borrow | backstop
	Tier          *string `json:"tier"` // BLNT backstop tier; null elsewhere
	Asset         *string `json:"asset"`
	Decimals      *int    `json:"decimals"`
	NetFlow       string  `json:"net_flow"`
	FoldedTokens  *string `json:"folded_tokens"`  // b-, d- or backstop share tokens folded from events
	OnchainTokens *string `json:"onchain_tokens"` // the same balance read from contract storage
	CurrentValue  *string `json:"current_value"`  // onchain_tokens in the asset at the current rate or share price
	Accrued       *string `json:"accrued"`        // interest earned (supply), interest paid (borrow), earned (backstop)
	Reconciled    bool    `json:"reconciled"`
	Reason        *string `json:"reason"`
}

// LendingEmissionsClaim is the pool emissions one account claimed from one pool, in the pool's emission token.
type LendingEmissionsClaim struct {
	Protocol   string `json:"protocol"`
	Pool       string `json:"pool"`
	Claimed    string `json:"claimed"`
	LowerBound bool   `json:"lower_bound"`
}

// legInputs are one leg's folded and on-chain figures; nil folded or onchain means unknown, never zero.
type legInputs struct {
	netFlow *big.Int
	folded  *big.Int
	onchain *big.Int
	moved   bool
	value   func(*big.Int) (*big.Int, bool) // tokens to asset amount; false when the rate or price is unread
}

// reconcileLeg applies the earnings invariant: accrued is served only when the folded balance equals the on-chain one
// and no auction moved the leg; otherwise it is withheld with a reason while the raw components stay visible.
func reconcileLeg(leg *LendingEarningsLeg, in legInputs) {
	leg.NetFlow = in.netFlow.String()
	leg.FoldedTokens = bigStrRef(in.folded)
	leg.OnchainTokens = bigStrRef(in.onchain)
	var value *big.Int
	if in.onchain != nil {
		if in.onchain.Sign() == 0 {
			value = new(big.Int)
		} else if v, ok := in.value(in.onchain); ok {
			value = v
		}
	}
	leg.CurrentValue = bigStrRef(value)
	reason := ""
	switch {
	case in.moved:
		reason = earningsReasonAuctionMoved
	case in.folded == nil:
		reason = earningsReasonFoldInconsistent
	case in.onchain == nil:
		reason = earningsReasonOnchainUnread
	case in.folded.Cmp(in.onchain) != 0:
		reason = earningsReasonBalanceMismatch
	case value == nil:
		reason = earningsReasonRateUnread
	}
	if reason != "" {
		leg.Reason = strRef(reason)
		return
	}
	leg.Reconciled = true
	leg.Accrued = strRef(new(big.Int).Sub(value, in.netFlow).String())
}

func bigStrRef(n *big.Int) *string {
	if n == nil {
		return nil
	}
	return strRef(n.String())
}

func parseBig(s string) (*big.Int, bool) {
	return new(big.Int).SetString(s, 10)
}

// handleLendingAccountEarnings serves GET /v1/lending/accounts/{account}/earnings: per pool, reserve and backstop tier, what
// the account has earned and paid. Invariant: an accrued figure is exact only when the token balance folded from the
// account's events (b-tokens, d-tokens or backstop shares minted minus burned) equals its current on-chain balance for that
// (pool, reserve, side); any other case, including an auction-moved leg, withholds it (null, reconciled=false, reason).
func (s *Server) handleLendingAccountEarnings(w http.ResponseWriter, r *http.Request) {
	account := r.PathValue("account")
	if canonical.IsMuxedAccount(account) {
		if base, ok := canonical.MuxedAccountID(account); ok {
			account = base
		}
	}
	if !canonical.IsAccountID(account) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-account",
			"Invalid account", http.StatusBadRequest, "the account must be a valid G-strkey or M-strkey")
		return
	}
	er, okE := s.Positions.(lendingEarningsReader)
	sr, okS := s.Explorer.(lendingAccountStateReader)
	if !okE || !okS || s.Lending == nil {
		s.explorerUnavailable(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxHandlerBudget)
	defer cancel()
	view, err := s.buildLendingEarnings(ctx, er, sr, account)
	if err != nil {
		s.writeLendingLookupError(ctx, w, r, "lending earnings", account, err)
		return
	}
	wm, stale, _ := s.lakeWatermark(ctx)
	view.AsOfLedger = wm
	writeEnvelope(w, Envelope{Data: view, AsOf: WireTime(time.Now().UTC()), Flags: Flags{Stale: stale}})
}

func (s *Server) buildLendingEarnings(ctx context.Context, er lendingEarningsReader, sr lendingAccountStateReader, account string) (LendingEarningsView, error) {
	view := LendingEarningsView{Account: account, Legs: []LendingEarningsLeg{}, Emissions: []LendingEmissionsClaim{}, Note: lendingEarningsNote}
	blendFolds, err := er.BlendPositionsByUser(ctx, account)
	if err != nil {
		return view, err
	}
	blntFolds, err := er.BLNTPositionsByUser(ctx, account)
	if err != nil {
		return view, err
	}
	flows, err := er.LendingAccountFlows(ctx, account)
	if err != nil {
		return view, err
	}
	for _, p := range []struct {
		protocol string
		folds    []timescale.BlendPositionFold
	}{{"blend", blendFolds}, {blnt.SourceName, blntFolds}} {
		legs, err := s.poolEarningsLegs(ctx, sr, p.protocol, account, p.folds)
		if err != nil {
			return view, err
		}
		view.Legs = append(view.Legs, legs...)
	}
	backstopLegs, err := s.backstopEarningsLegs(ctx, er, sr, account, flows)
	if err != nil {
		return view, err
	}
	view.Legs = append(view.Legs, backstopLegs...)
	for _, c := range flows.BlendClaims {
		view.Emissions = append(view.Emissions, LendingEmissionsClaim{Protocol: "blend", Pool: c.Pool, Claimed: c.Claimed, LowerBound: true})
	}
	for _, c := range flows.BLNTClaims {
		view.Emissions = append(view.Emissions, LendingEmissionsClaim{Protocol: blnt.SourceName, Pool: c.Pool, Claimed: c.Claimed, LowerBound: true})
	}
	return view, nil
}

// poolEarningsLegs builds the supply and borrow legs of one protocol's folds, reading each pool's state once.
func (s *Server) poolEarningsLegs(ctx context.Context, sr lendingAccountStateReader, protocol, account string, folds []timescale.BlendPositionFold) ([]LendingEarningsLeg, error) {
	var out []LendingEarningsLeg
	byPool := map[string][]timescale.BlendPositionFold{}
	var pools []string
	for _, f := range folds {
		if _, seen := byPool[f.Pool]; !seen {
			pools = append(pools, f.Pool)
		}
		byPool[f.Pool] = append(byPool[f.Pool], f)
	}
	for _, pool := range pools {
		reserves, err := s.earningsReserveStates(ctx, protocol, pool)
		if err != nil {
			return nil, err
		}
		pos, err := sr.LendingAccountPositions(ctx, pool, account)
		if err != nil {
			return nil, err
		}
		for _, f := range byPool[pool] {
			out = append(out, poolFoldLegs(protocol, f, reserves[f.Asset], pos)...)
		}
	}
	return out, nil
}

// earningsReserveStates reads a pool's reserve state through the reserves endpoint's own read path, by asset.
func (s *Server) earningsReserveStates(ctx context.Context, protocol, pool string) (map[string]clickhouse.BlendReserveState, error) {
	read := s.readBLNTPoolReserves
	version := blend.PoolVersionUnknown
	if protocol == "blend" {
		v, err := s.Lending.BlendPoolVersion(ctx, pool)
		if err != nil {
			return nil, err
		}
		if v == blend.PoolVersionUnknown {
			return map[string]clickhouse.BlendReserveState{}, nil
		}
		version, read = v, s.readBlendPoolReserves
	}
	pr, err := read(ctx, pool, version)
	if err != nil {
		return nil, err
	}
	out := make(map[string]clickhouse.BlendReserveState, len(pr.state.Reserves))
	for _, st := range pr.state.Reserves {
		out[st.Asset] = st
	}
	return out, nil
}

// poolFoldLegs turns one (pool, asset) fold into its supply and borrow legs.
func poolFoldLegs(protocol string, f timescale.BlendPositionFold, st clickhouse.BlendReserveState, pos clickhouse.LendingAccountPositions) []LendingEarningsLeg {
	haveRate := st.Asset != ""
	var decimals *int
	if haveRate && st.DecimalsFound {
		d := int(st.Decimals)
		decimals = &d
	}
	sides := []struct {
		side      string
		has       bool
		net, toks string
		moved     bool
		onchain   map[string]*big.Int
		convert   func(blend.ReserveData, *big.Int) *big.Int
	}{
		{earningsSideSupply, f.HasSupplyLeg, f.SupplyNet, f.SupplyTokens, f.SupplySuperseded, pos.Supply, blend.ReserveData.BTokensToUnderlying},
		{earningsSideBorrow, f.HasBorrowLeg, f.BorrowNet, f.BorrowTokens, f.BorrowSuperseded, pos.Borrow, blend.ReserveData.DTokensToUnderlying},
	}
	var out []LendingEarningsLeg
	for _, sd := range sides {
		if !sd.has {
			continue
		}
		leg := LendingEarningsLeg{Protocol: protocol, Pool: f.Pool, Side: sd.side, Asset: strRef(f.Asset), Decimals: decimals}
		in := legInputs{moved: sd.moved, value: func(n *big.Int) (*big.Int, bool) {
			if !haveRate {
				return nil, false
			}
			return sd.convert(st.Data, n), true
		}}
		in.folded, _ = parseBig(sd.toks)
		if in.netFlow, _ = parseBig(sd.net); in.netFlow == nil {
			in.netFlow, in.folded = new(big.Int), nil
		}
		if pos.Readable {
			in.onchain = new(big.Int)
			if b := sd.onchain[f.Asset]; b != nil {
				in.onchain = b
			}
		}
		reconcileLeg(&leg, in)
		out = append(out, leg)
	}
	return out
}

// backstopEarningsLegs builds one leg per (pool, tier) the account has backstop history in.
func (s *Server) backstopEarningsLegs(ctx context.Context, er lendingEarningsReader, sr lendingAccountStateReader, account string, flows timescale.LendingAccountFlows) ([]LendingEarningsLeg, error) {
	blendShares, err := er.BlendBackstopSharesByUser(ctx, account)
	if err != nil {
		return nil, err
	}
	blntShares, err := er.BLNTBackstopByUser(ctx, account)
	if err != nil {
		return nil, err
	}
	folded := map[[2]string]*big.Int{}
	for _, f := range blendShares {
		folded[[2]string{f.Pool, ""}], _ = parseBig(f.SharesNet)
	}
	for _, f := range blntShares {
		folded[[2]string{f.Pool, f.Tier}], _ = parseBig(f.Shares)
	}
	var out []LendingEarningsLeg
	for _, p := range []struct {
		protocol string
		flows    []timescale.LendingBackstopTokenFold
	}{{"blend", flows.BlendBackstop}, {blnt.SourceName, flows.BLNTBackstop}} {
		pools := map[string][]timescale.LendingBackstopTokenFold{}
		var order []string
		for _, f := range p.flows {
			if _, seen := pools[f.Pool]; !seen {
				order = append(order, f.Pool)
			}
			pools[f.Pool] = append(pools[f.Pool], f)
		}
		for _, pool := range order {
			legs, err := s.poolBackstopLegs(ctx, sr, p.protocol, pool, account, pools[pool], folded)
			if err != nil {
				return nil, err
			}
			out = append(out, legs...)
		}
	}
	return out, nil
}

// poolBackstopLegs reads one pool's backstop and the account's balance in it, then reconciles each tier.
func (s *Server) poolBackstopLegs(ctx context.Context, sr lendingAccountStateReader, protocol, pool, account string,
	flows []timescale.LendingBackstopTokenFold, folded map[[2]string]*big.Int,
) ([]LendingEarningsLeg, error) {
	var st clickhouse.LendingBackstopState
	held := map[string]*big.Int{}
	lineage, reason, err := s.backstopLineage(ctx, pool)
	if err != nil {
		return nil, err
	}
	if reason == "" && lineage == protocol {
		if st, err = sr.LendingBackstop(ctx, pool, protocol == blnt.SourceName); err != nil {
			return nil, err
		}
		if st.Instance != nil && st.Instance.Backstop != "" {
			tiers := make([]string, 0, len(flows))
			for _, f := range flows {
				tiers = append(tiers, f.Tier)
			}
			if held, err = sr.LendingAccountBackstop(ctx, st.Instance.Backstop, pool, account, tiers); err != nil {
				return nil, err
			}
		}
	}
	out := make([]LendingEarningsLeg, 0, len(flows))
	for _, f := range flows {
		leg := LendingEarningsLeg{Protocol: protocol, Pool: pool, Side: earningsSideBackstop, Tier: nonEmptyPtr(f.Tier)}
		tier := backstopTier(st, f.Tier)
		token := st.Token
		if tier != nil && tier.Token != "" {
			token = tier.Token
		}
		leg.Asset = nonEmptyPtr(token)
		shares := folded[[2]string{pool, f.Tier}]
		net, ok := parseBig(f.NetDeposited)
		if !ok {
			net, shares = new(big.Int), nil
		}
		reconcileLeg(&leg, legInputs{
			netFlow: net, folded: shares, onchain: held[f.Tier],
			value: func(n *big.Int) (*big.Int, bool) { return backstopShareValue(tier, n) },
		})
		if reason != "" && leg.Reason != nil && *leg.Reason == earningsReasonOnchainUnread {
			leg.Reason = strRef(reason)
		}
		out = append(out, leg)
	}
	return out, nil
}

func backstopTier(st clickhouse.LendingBackstopState, tier string) *clickhouse.BackstopTierState {
	i := slices.IndexFunc(st.Tiers, func(t clickhouse.BackstopTierState) bool { return t.Tier == tier })
	if i < 0 {
		return nil
	}
	return &st.Tiers[i]
}

// backstopShareValue is shares x pool tokens / pool shares, rounded down (backstop pool.rs convert_to_tokens).
func backstopShareValue(tier *clickhouse.BackstopTierState, shares *big.Int) (*big.Int, bool) {
	if tier == nil || tier.Balance == nil || tier.Balance.Shares.Sign() <= 0 {
		return nil, false
	}
	n := new(big.Int).Mul(shares, tier.Balance.Tokens)
	return n.Quo(n, tier.Balance.Shares), true
}
