package v1

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"slices"
	"strconv"
	"strings"
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
	LendingAccountBackstop(ctx context.Context, backstop, pool, account string, tiers []string) (clickhouse.LendingAccountBackstop, error)
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
	earningsReasonProjectionBehind = "projection_behind"
	earningsReasonFoldInconsistent = "fold_inconsistent"
	earningsReasonOnchainUnread    = "onchain_unread"
	earningsReasonBalanceMismatch  = "balance_mismatch"
	earningsReasonRateUnread       = "rate_unread"
	// earningsReasonRateBehind withholds a leg whose read reserve rate was stored before its newest folded event: net_flow then
	// holds a flow made at a later rate than the one valuing it.
	earningsReasonRateBehind = "rate_behind"
	// earningsReasonHistoryUnverified withholds a leg whose event history is not verified back to its protocol's first ledger.
	earningsReasonHistoryUnverified    = "history_unverified"
	earningsReasonHistoryVerdictStale  = "history_verdict_stale"
	earningsReasonHistoryVerdictUnread = "history_verdict_unread"
)

const lendingEarningsNote = "accrued is current_value - net_flow, served only when the token balance folded from this account's " +
	"events equals its current on-chain balance (reconciled); otherwise it is null and reason names why. A pool leg is valued at " +
	"the reserve's last on-chain rate update (rate_as_of), and accrued is withheld (rate_behind) when that update predates the " +
	"leg's newest folded event. net_flow is " +
	"underlying deposited minus withdrawn (supply), borrowed minus repaid (borrow), or backstop tokens deposited minus " +
	"withdrawn (backstop); a backstop's accrued includes emissions claimed into it. A BLNT clawback counts as a withdrawal, so " +
	"accrued does not include the clawed-back amount as a loss. Emission claims are sums of the " +
	"claim events projected so far and are a lower bound."

// LendingEarningsView is the GET /v1/lending/accounts/{account}/earnings body.
type LendingEarningsView struct {
	Account    string                  `json:"account"`
	Legs       []LendingEarningsLeg    `json:"legs"`
	Emissions  []LendingEmissionsClaim `json:"emissions"`
	Note       string                  `json:"note"`
	AsOfLedger uint32                  `json:"as_of_ledger,omitempty"`
	// LowerBound is set when a read hit the per-account row cap; CoverageNote then names the capped reads.
	LowerBound   bool   `json:"lower_bound"`
	CoverageNote string `json:"coverage_note,omitempty"`
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
	CurrentValue  *string `json:"current_value"`  // onchain_tokens in the asset at the stored rate or share price
	// RateAsOf is when the reserve's stored b_rate/d_rate was last updated on chain; current_value and accrued are true as of
	// then (the rate moves only when the reserve is touched). Null for a backstop leg or an unread rate.
	RateAsOf   *WireTime `json:"rate_as_of"`
	Accrued    *string   `json:"accrued"` // interest earned (supply), interest paid (borrow), earned (backstop)
	Reconciled bool      `json:"reconciled"`
	Reason     *string   `json:"reason"`
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
	behind  bool                            // the lake entry changed after the newest folded event
	value   func(*big.Int) (*big.Int, bool) // tokens to asset amount; false when the rate or price is unread
	// rateBehind: the rate value uses was stored before the newest folded event, so it cannot value tokens that event minted.
	rateBehind bool
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
	case in.behind:
		reason = earningsReasonProjectionBehind
	case in.folded == nil:
		reason = earningsReasonFoldInconsistent
	case in.onchain == nil:
		reason = earningsReasonOnchainUnread
	case in.folded.Cmp(in.onchain) != 0:
		reason = earningsReasonBalanceMismatch
	case value == nil:
		reason = earningsReasonRateUnread
	case in.rateBehind && in.onchain.Sign() != 0:
		reason = earningsReasonRateBehind
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
	// Smart wallets and vaults supply into these pools as contract addresses.
	if !canonical.IsAccountID(account) && !canonical.IsContractID(account) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-account",
			"Invalid account", http.StatusBadRequest, "the account must be a valid G-, M- or C-strkey")
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
	view, flags, err := s.buildLendingEarnings(ctx, er, sr, account)
	if err != nil {
		s.writeLendingLookupError(ctx, w, r, "lending earnings", account, err)
		return
	}
	wm, stale, _ := s.lakeWatermark(ctx)
	view.AsOfLedger = wm
	flags.Stale = flags.Stale || stale
	writeEnvelope(w, Envelope{Data: view, AsOf: WireTime(time.Now().UTC()), Flags: flags})
}

// buildLendingEarnings reads the account's folds, then each pool's lake state; a pool whose lake read fails withholds its legs
// as unread and sets degraded instead of failing the response. Stale is set by a stale cached reserve rate or history verdict.
func (s *Server) buildLendingEarnings(ctx context.Context, er lendingEarningsReader, sr lendingAccountStateReader, account string) (LendingEarningsView, Flags, error) {
	view := LendingEarningsView{Account: account, Legs: []LendingEarningsLeg{}, Emissions: []LendingEmissionsClaim{}, Note: lendingEarningsNote}
	var capped []string
	blendFolds, err := er.BlendPositionsByUser(ctx, account)
	if err = noteCapped(&capped, "blend positions", err); err != nil {
		return view, Flags{}, err
	}
	blntFolds, err := er.BLNTPositionsByUser(ctx, account)
	if err = noteCapped(&capped, "blnt positions", err); err != nil {
		return view, Flags{}, err
	}
	flows, err := er.LendingAccountFlows(ctx, account)
	if err = noteCapped(&capped, "emission claims and backstop token flows", err); err != nil {
		return view, Flags{}, err
	}
	backstop, err := s.backstopEarningsJobs(ctx, er, account, flows, &capped)
	if err != nil {
		return view, Flags{}, err
	}
	if len(capped) > 0 {
		view.LowerBound = true
		view.CoverageNote = "TRUNCATED: " + strings.Join(capped, ", ") + " exceed the per-account cap; only the first " +
			strconv.Itoa(timescale.PositionsVenueLimit) + " rows of each, in pool order, are read, so legs and emission claims " +
			"past them are missing and this response is a lower bound."
	}
	jobs := append(poolEarningsJobs("blend", blendFolds), poolEarningsJobs(blnt.SourceName, blntFolds)...)
	jobs = append(jobs, backstop...)
	legs := make([][]LendingEarningsLeg, len(jobs))
	failed, stale := make([]bool, len(jobs)), make([]bool, len(jobs))
	forEachBounded(s.logger, len(jobs), readFanoutConcurrency, func(i int) {
		failed[i] = true // stays set if the read panics
		legs[i], failed[i], stale[i] = s.earningsJobLegs(ctx, sr, account, jobs[i])
	})
	view.Legs = append(view.Legs, slices.Concat(legs...)...)
	historyFailed, historyStale := s.withholdUnverifiedHistory(ctx, view.Legs)
	for _, c := range flows.BlendClaims {
		view.Emissions = append(view.Emissions, LendingEmissionsClaim{Protocol: "blend", Pool: c.Pool, Claimed: c.Claimed, LowerBound: true})
	}
	for _, c := range flows.BLNTClaims {
		view.Emissions = append(view.Emissions, LendingEmissionsClaim{Protocol: blnt.SourceName, Pool: c.Pool, Claimed: c.Claimed, LowerBound: true})
	}
	return view, Flags{
		Stale:    historyStale || slices.Contains(stale, true),
		Degraded: historyFailed || slices.Contains(failed, true) || view.LowerBound,
	}, nil
}

// noteCapped records a fold read that hit the per-account row cap under `what` and passes any other error through.
func noteCapped(capped *[]string, what string, err error) error {
	if errors.Is(err, timescale.ErrPositionsTruncated) {
		*capped = append(*capped, what)
		return nil
	}
	return err
}

// earningsLegSource names the completeness source whose projection wrote a leg's events.
func earningsLegSource(leg LendingEarningsLeg) string {
	switch {
	case leg.Protocol == blnt.SourceName:
		return blnt.SourceName
	case leg.Side == earningsSideBackstop:
		return "blend_backstop"
	default:
		return "blend"
	}
}

// withholdUnverifiedHistory withholds every otherwise-exact leg whose source is not verified from its genesis: events
// before the projected range would be missing from net_flow while the token balances still match. Only the legs' own
// sources' verdicts are judged; failed reports an unread one and stale one past its freshness gate.
func (s *Server) withholdUnverifiedHistory(ctx context.Context, legs []LendingEarningsLeg) (failed, stale bool) {
	var sources []string
	for _, leg := range legs {
		if src := earningsLegSource(leg); leg.Reconciled && !slices.Contains(sources, src) {
			sources = append(sources, src)
		}
	}
	if len(sources) == 0 {
		return false, false
	}
	verdicts := s.projectionHistoryVerdicts(ctx, sources...)
	for i := range legs {
		if !legs[i].Reconciled {
			continue
		}
		reason := earningsReasonHistoryUnverified
		switch verdicts[earningsLegSource(legs[i])] {
		case historyVerified:
			continue
		case historyVerdictStale:
			reason, stale = earningsReasonHistoryVerdictStale, true
		case historyVerdictUnread:
			reason, failed = earningsReasonHistoryVerdictUnread, true
		case historyUnverified:
		}
		legs[i].Reconciled, legs[i].Accrued = false, nil
		legs[i].Reason = strRef(reason)
	}
	return failed, stale
}

// earningsJob is one pool's legs to build: its supply/borrow folds, or its backstop tiers.
type earningsJob struct {
	protocol, pool string
	folds          []timescale.BlendPositionFold
	backstop       []timescale.LendingBackstopTokenFold
	shares         map[[2]string]*big.Int
}

// poolEarningsJobs groups one protocol's folds by pool, in first-seen order.
func poolEarningsJobs(protocol string, folds []timescale.BlendPositionFold) []earningsJob {
	var jobs []earningsJob
	at := map[string]int{}
	for _, f := range folds {
		i, seen := at[f.Pool]
		if !seen {
			i = len(jobs)
			at[f.Pool] = i
			jobs = append(jobs, earningsJob{protocol: protocol, pool: f.Pool})
		}
		jobs[i].folds = append(jobs[i].folds, f)
	}
	return jobs
}

func (s *Server) earningsJobLegs(ctx context.Context, sr lendingAccountStateReader, account string, job earningsJob) (legs []LendingEarningsLeg, failed, stale bool) {
	if job.backstop != nil {
		legs, failed = s.poolBackstopLegs(ctx, sr, job, account)
		return legs, failed, false
	}
	return s.poolEarningsLegs(ctx, sr, job, account)
}

// poolEarningsLegs builds one pool's supply and borrow legs from its cached reserve state and the account's Positions entry;
// stale reports a cached reserve state past its TTL or built from a stale lake.
func (s *Server) poolEarningsLegs(ctx context.Context, sr lendingAccountStateReader, job earningsJob, account string) (out []LendingEarningsLeg, failed, stale bool) {
	reserves, stale, err := s.earningsReserveStates(ctx, job.pool)
	if err != nil {
		s.logger.Warn("lending earnings: reserve read failed", "err", err, "pool", job.pool)
		failed = true
	}
	pos, err := sr.LendingAccountPositions(ctx, job.pool, account)
	if err != nil {
		s.logger.Warn("lending earnings: positions read failed", "err", err, "pool", job.pool)
		pos, failed = clickhouse.LendingAccountPositions{}, true
	}
	behind := pos.Ledger > newestFoldedLedger(job.folds)
	for _, f := range job.folds {
		out = append(out, poolFoldLegs(job.protocol, f, reserves[f.Asset], pos, behind)...)
	}
	return out, failed, stale
}

// newestFoldedLedger is the newest event ledger folded into a pool's legs. Every transaction that changes an account's
// positions rewrites its Positions entry, so an entry modified (or removed) after this ledger has events the fold lacks.
func newestFoldedLedger(folds []timescale.BlendPositionFold) uint32 {
	var newest uint32
	for _, f := range folds {
		newest = max(newest, f.SupplyLastLedger, f.BorrowLastLedger)
	}
	return newest
}

// earningsReserveStates reads a pool's reserve state through the reserves endpoint's cache, by asset; a pool of unknown lineage
// has none.
func (s *Server) earningsReserveStates(ctx context.Context, pool string) (map[string]clickhouse.BlendReserveState, bool, error) {
	res, _, cacheStale, err := s.lendingReserves.get(ctx, "pool_reserves", pool, s.fillPoolReserves(pool))
	if errors.Is(err, errPoolLineageUnknown) {
		return map[string]clickhouse.BlendReserveState{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	out := make(map[string]clickhouse.BlendReserveState, len(res.states))
	for _, st := range res.states {
		out[st.Asset] = st
	}
	return out, cacheStale || res.flags.Stale, nil
}

// poolFoldLegs turns one (pool, asset) fold into its supply and borrow legs.
func poolFoldLegs(protocol string, f timescale.BlendPositionFold, st clickhouse.BlendReserveState, pos clickhouse.LendingAccountPositions, behind bool) []LendingEarningsLeg {
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
		last      time.Time
		onchain   map[string]*big.Int
		convert   func(blend.ReserveData, *big.Int) *big.Int
	}{
		{earningsSideSupply, f.HasSupplyLeg, f.SupplyNet, f.SupplyTokens, f.SupplySuperseded, f.SupplyLastActivity, pos.Supply, blend.ReserveData.BTokensToUnderlying},
		{earningsSideBorrow, f.HasBorrowLeg, f.BorrowNet, f.BorrowTokens, f.BorrowSuperseded, f.BorrowLastActivity, pos.Borrow, blend.ReserveData.DTokensToUnderlying},
	}
	var out []LendingEarningsLeg
	for _, sd := range sides {
		if !sd.has {
			continue
		}
		leg := LendingEarningsLeg{Protocol: protocol, Pool: f.Pool, Side: sd.side, Asset: strRef(f.Asset), Decimals: decimals}
		if haveRate {
			leg.RateAsOf = reserveLastUpdate(st.Data.LastTime)
		}
		// Every pool action accrues and stores its reserve, so a rate stored before the leg's newest event is a stale read.
		// A BLNT clawback is assumed to do so too (no contract evidence in the repo); if not, its leg is withheld, not misvalued.
		rateBehind := haveRate && !sd.last.IsZero() && st.Data.LastTime < uint64(max(sd.last.Unix(), 0))
		in := legInputs{moved: sd.moved, behind: behind, rateBehind: rateBehind, value: func(n *big.Int) (*big.Int, bool) {
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

// backstopEarningsJobs groups the account's backstop flows into one job per (protocol, pool).
func (s *Server) backstopEarningsJobs(ctx context.Context, er lendingEarningsReader, account string, flows timescale.LendingAccountFlows,
	capped *[]string,
) ([]earningsJob, error) {
	blendShares, err := er.BlendBackstopSharesByUser(ctx, account)
	if err = noteCapped(capped, "blend backstop shares", err); err != nil {
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
	var jobs []earningsJob
	for _, p := range []struct {
		protocol string
		flows    []timescale.LendingBackstopTokenFold
	}{{"blend", flows.BlendBackstop}, {blnt.SourceName, flows.BLNTBackstop}} {
		at := map[string]int{}
		for _, f := range p.flows {
			i, seen := at[f.Pool]
			if !seen {
				i = len(jobs)
				at[f.Pool] = i
				jobs = append(jobs, earningsJob{protocol: p.protocol, pool: f.Pool, shares: folded})
			}
			jobs[i].backstop = append(jobs[i].backstop, f)
		}
	}
	return jobs, nil
}

// poolBackstopLegs reads one pool's backstop and the account's balance in it, then reconciles each tier; a failed read
// withholds every tier as unread.
func (s *Server) poolBackstopLegs(ctx context.Context, sr lendingAccountStateReader, job earningsJob, account string) ([]LendingEarningsLeg, bool) {
	tiers := make([]string, 0, len(job.backstop))
	for _, f := range job.backstop {
		tiers = append(tiers, f.Tier)
	}
	st, held, reason, err := s.readAccountBackstop(ctx, sr, job.protocol, job.pool, account, tiers)
	failed := err != nil
	if failed {
		s.logger.Warn("lending earnings: backstop read failed", "err", err, "pool", job.pool)
		st, held, reason = clickhouse.LendingBackstopState{}, clickhouse.LendingAccountBackstop{}, ""
	}
	out := make([]LendingEarningsLeg, 0, len(job.backstop))
	for _, f := range job.backstop {
		out = append(out, backstopLeg(job, f, st, held, reason))
	}
	return out, failed
}

func backstopLeg(job earningsJob, f timescale.LendingBackstopTokenFold, st clickhouse.LendingBackstopState, held clickhouse.LendingAccountBackstop, reason string) LendingEarningsLeg {
	leg := LendingEarningsLeg{Protocol: job.protocol, Pool: job.pool, Side: earningsSideBackstop, Tier: nonEmptyPtr(f.Tier)}
	tier := backstopTier(st, f.Tier)
	token := st.Token
	if tier != nil && tier.Token != "" {
		token = tier.Token
	}
	leg.Asset = nonEmptyPtr(token)
	shares := job.shares[[2]string{job.pool, f.Tier}]
	net, ok := parseBig(f.NetDeposited)
	if !ok || f.NullAmounts > 0 {
		net, shares = new(big.Int), nil
	}
	// Every backstop action of the account rewrites its UserBalance entry, as for pool Positions.
	reconcileLeg(&leg, legInputs{
		netFlow: net, folded: shares, onchain: held.Held[f.Tier], behind: held.Ledger[f.Tier] > f.LastLedger,
		value: func(n *big.Int) (*big.Int, bool) { return backstopShareValue(tier, n) },
	})
	if reason != "" && leg.Reason != nil && *leg.Reason == earningsReasonOnchainUnread {
		leg.Reason = strRef(reason)
	}
	return leg
}

// readAccountBackstop reads a pool's backstop and the account's held shares per tier; reason is the lineage's unavailable
// reason, when there is one.
func (s *Server) readAccountBackstop(ctx context.Context, sr lendingAccountStateReader, protocol, pool, account string, tiers []string,
) (st clickhouse.LendingBackstopState, held clickhouse.LendingAccountBackstop, reason string, err error) {
	lineage, reason, err := s.backstopLineage(ctx, pool)
	if err != nil || reason != "" || lineage != protocol {
		return st, held, reason, err
	}
	if st, err = sr.LendingBackstop(ctx, pool, protocol == blnt.SourceName); err != nil {
		return st, held, reason, err
	}
	if st.Instance == nil || st.Instance.Backstop == "" {
		return st, held, reason, nil
	}
	held, err = sr.LendingAccountBackstop(ctx, st.Instance.Backstop, pool, account, tiers)
	return st, held, reason, err
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
