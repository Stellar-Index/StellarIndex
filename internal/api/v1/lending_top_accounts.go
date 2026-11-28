package v1

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"math/big"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// lendingPoolPositionsReader is the lake read of every account's current Positions entry in one pool.
type lendingPoolPositionsReader interface {
	LendingPoolPositions(ctx context.Context, pool string) (clickhouse.LendingPoolPositions, error)
}

// Top-accounts ranking keys, and the exclusion reasons only this route uses.
const (
	topAccountsRankUSD    = "value_usd"
	topAccountsRankAmount = "amount"

	topAccountsReasonTruncated   = "positions_truncated"
	topAccountsReasonUnreadable  = "positions_unreadable"
	topAccountsReasonList        = "reserve_list_unreadable"
	topAccountsReasonOvercounted = "positions_overcounted"

	topAccountsDefaultLimit  = 20
	topAccountsMaxLimit      = 100
	topAccountsShareDecimals = 6

	// topAccountsUnreconciledTTL is how long a fill that still mismatched after its re-read is served before the next read.
	topAccountsUnreconciledTTL = 5 * time.Second
)

const lendingTopAccountsNote = "Rows are positions, not accounts: each row is one account's current position in one reserve, so an account " +
	"holding several reserves appears once per reserve and value_usd is that one position's value, not the account's total. " +
	"Positions are read from the pool's Positions storage; amount is " +
	"its b- or d-tokens in the underlying at the reserve's stored rate, the figure /v1/lending/accounts/{account}/earnings serves as " +
	"current_value. Without asset, rows rank by value_usd and a reserve without a USD price is left out and named in excluded; " +
	"with asset, rows rank by amount. Positions are never ranked across reserves by token amount."

// LendingTopAccountsView is the GET /v1/lending/pools/{pool}/top-accounts body.
type LendingTopAccountsView struct {
	Pool     string  `json:"pool"`
	Protocol string  `json:"protocol"` // blend | blnt
	Side     string  `json:"side"`     // supply | borrow
	Asset    *string `json:"asset"`    // the reserve filter; null for every reserve
	Ranking  string  `json:"ranking"`  // value_usd | amount
	// Accounts counts the accounts holding a position on this side (in asset when filtered); a floor when lower_bound.
	Accounts int `json:"accounts"`
	// LowerBound marks rows and accounts as incomplete; Excluded names what was left out and Unattributed what no read
	// position accounts for.
	LowerBound   bool                             `json:"lower_bound"`
	Excluded     []LendingStatsExclusion          `json:"excluded"`
	Unattributed []LendingTopAccountsUnattributed `json:"unattributed"`
	Rows         []LendingTopAccount              `json:"rows"`
	AsOfLedger   uint32                           `json:"as_of_ledger"`
	Note         string                           `json:"note"`
}

// LendingTopAccount is one account's position in one reserve; amounts are base-unit decimal strings.
type LendingTopAccount struct {
	Rank     int    `json:"rank"`
	Account  string `json:"account"`
	Asset    string `json:"asset"`
	Decimals *int   `json:"decimals"`
	Tokens   string `json:"tokens"` // b-tokens (supply) or d-tokens (borrow) held on chain
	Amount   string `json:"amount"` // tokens in the underlying at the reserve's stored rate
	// ValueUSD is amount at the reserve's /reserves price; UsdReason names why it is null.
	ValueUSD  *string `json:"value_usd"`
	UsdReason *string `json:"usd_reason"`
	// ShareOfReserve is amount over the reserve's supplied (or borrowed) total, 6 dp; null when that total is zero.
	ShareOfReserve     *string `json:"share_of_reserve"`
	LastModifiedLedger uint32  `json:"last_modified_ledger"`
	// RankLowerBound marks a row ranked below what one unread position could hold, so an unread account may rank above it.
	RankLowerBound bool `json:"rank_lower_bound,omitempty"`
}

// LendingTopAccountsUnattributed is the part of a reserve's side total that no read position holds: tokens and their amount
// in the underlying as base-unit decimal strings, and tokens over the total at 6 dp.
type LendingTopAccountsUnattributed struct {
	Asset          string `json:"asset"`
	Side           string `json:"side"`
	Tokens         string `json:"tokens"`
	Amount         string `json:"amount"`
	ShareOfReserve string `json:"share_of_reserve"`
}

// poolTopAccounts is one pool's cached positions and the reserve states and views that convert and price them.
type poolTopAccounts struct {
	protocol        string
	positions       clickhouse.LendingPoolPositions
	states          map[string]clickhouse.BlendReserveState
	views           map[string]ReserveView
	asOf            uint32 // the lake watermark read just before the positions
	stale, degraded bool
	unreconciled    bool // a reserve still mismatched after the re-read
	version         blend.PoolVersion
	instance        *clickhouse.PoolInstance // nil when the instance is unread
	health          *poolHealthRead          // set only by the /health fill
}

// poolTopAccountsTTL serves an unreconciled fill only briefly, so a mismatch from a write between the reads is not every
// caller's answer for the full TTL.
func poolTopAccountsTTL(b poolTopAccounts) time.Duration {
	if b.unreconciled || b.health.transient() || (b.health != nil && b.health.backstopUnknown) {
		return topAccountsUnreconciledTTL
	}
	return lendingReservesTTL
}

// handleLendingPoolTopAccounts serves GET /v1/lending/pools/{pool}/top-accounts. Invariant: every amount is the account's
// on-chain token balance converted at the rate the earnings route uses; a ranking that leaves out any position is a lower
// bound naming what it left out, and positions in different reserves are never compared by token amount.
func (s *Server) handleLendingPoolTopAccounts(w http.ResponseWriter, r *http.Request) {
	pool := r.PathValue("pool")
	if !canonical.IsContractID(pool) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-pool",
			"Invalid pool", http.StatusBadRequest, "pool must be a C-strkey contract id")
		return
	}
	q := r.URL.Query()
	side := q.Get("side")
	if side != earningsSideSupply && side != earningsSideBorrow {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-side",
			"Invalid side", http.StatusBadRequest, "side must be supply or borrow")
		return
	}
	asset := q.Get("asset")
	if asset != "" && !canonical.IsContractID(asset) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-asset",
			"Invalid asset", http.StatusBadRequest, "asset must be the reserve token's C-strkey contract id")
		return
	}
	limit, ok := parseExplorerLimit(w, r, topAccountsDefaultLimit, topAccountsMaxLimit)
	if !ok {
		return
	}
	pr, okP := s.Explorer.(lendingPoolPositionsReader)
	if s.Explorer == nil || !okP {
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
		s.writeLendingLookupError(ctx, w, r, "lending top accounts lineage", pool, err)
		return
	}
	if protocol == "" {
		writeLendingPoolNotFound(w, r)
		return
	}
	built, at, cacheStale, err := s.lendingTopAccounts.get(ctx, "pool_top_accounts", pool, s.fillPoolTopAccounts(pr, pool, protocol))
	if err != nil {
		s.writeLendingLookupError(ctx, w, r, "lending top accounts", pool, err)
		return
	}
	if asset != "" && built.positions.Readable && !slices.Contains(built.positions.Reserves, asset) {
		writeLendingHistoryNotFound(w, r, "asset is not a reserve of this pool")
		return
	}
	view := rankTopAccounts(pool, built, side, asset, limit)
	view.AsOfLedger = built.asOf
	flags := Flags{Stale: cacheStale || built.stale, Degraded: built.degraded || view.LowerBound}
	writeEnvelope(w, Envelope{Data: view, AsOf: WireTime(at.UTC()), Flags: flags})
}

// fillPoolTopAccounts reads the pool's positions and then its reserve state. The two reads are not atomic, so a mismatch
// reads both once more; one that survives is served and marked unreconciled.
func (s *Server) fillPoolTopAccounts(pr lendingPoolPositionsReader, pool, protocol string) func(context.Context) (poolTopAccounts, error) {
	return func(ctx context.Context) (poolTopAccounts, error) {
		built, err := s.readPoolTopAccounts(ctx, pr, pool, protocol)
		if err != nil || built.reconciles() {
			return built, err
		}
		if built, err = s.readPoolTopAccounts(ctx, pr, pool, protocol); err != nil {
			return poolTopAccounts{}, err
		}
		built.unreconciled = !built.reconciles()
		return built, nil
	}
}

func (s *Server) readPoolTopAccounts(ctx context.Context, pr lendingPoolPositionsReader, pool, protocol string) (poolTopAccounts, error) {
	wm, lakeStale, _ := s.lakeWatermark(ctx)
	pos, err := pr.LendingPoolPositions(ctx, pool)
	if err != nil {
		return poolTopAccounts{}, err
	}
	// Reserve state is read fresh AFTER the positions, never from the reserves cache, so no position is newer than the
	// rate that converts it.
	res, err := s.buildPoolReserves(ctx, pool)
	if err != nil && !errors.Is(err, errPoolLineageUnknown) {
		return poolTopAccounts{}, err
	}
	states := make(map[string]clickhouse.BlendReserveState, len(res.states))
	for _, st := range res.states {
		states[st.Asset] = st
	}
	views := make(map[string]ReserveView, len(res.view.Reserves))
	for _, rv := range res.view.Reserves {
		views[rv.Asset] = rv
	}
	return poolTopAccounts{
		protocol: protocol, positions: pos, states: states, views: views, asOf: wm,
		stale: lakeStale || res.flags.Stale, degraded: res.flags.Degraded, version: res.version, instance: res.instance,
	}, nil
}

// reconciles reports that every read reserve's positions sum exactly to its b_supply and d_supply; a read that cannot be
// reconciled at all (unreadable list, truncated) has nothing a re-read would fix.
func (b poolTopAccounts) reconciles() bool {
	for _, side := range []string{earningsSideSupply, earningsSideBorrow} {
		for _, r := range b.reconciliation(side) {
			if r.gap != nil || r.reason == topAccountsReasonOvercounted {
				return false
			}
		}
	}
	return true
}

// topCandidate is one ranked position before the limit is applied.
type topCandidate struct {
	row    LendingTopAccount
	amount *big.Int
	usd    *big.Rat
}

// rankTopAccounts builds one side's ranking from a pool's cached positions; it never mutates built.
func rankTopAccounts(pool string, built poolTopAccounts, side, asset string, limit int) LendingTopAccountsView {
	view := LendingTopAccountsView{
		Pool: pool, Protocol: built.protocol, Side: side, Ranking: topAccountsRankUSD,
		Excluded: []LendingStatsExclusion{}, Rows: []LendingTopAccount{}, Note: lendingTopAccountsNote,
	}
	if asset != "" {
		view.Asset, view.Ranking = strRef(asset), topAccountsRankAmount
	}
	recon := built.reconciliation(side)
	excluded := topAccountsReadExclusions(built, asset, recon)
	cands, accounts := topAccountCandidates(built, side, asset, excluded)
	var bound rankBound
	view.Unattributed, bound = unattributedGaps(built, side, asset, recon)
	view.Accounts = accounts
	if built.positions.Truncated {
		// The read stopped at its cap in key order, so the largest account may be past it: no row can be ranked.
		cands = nil
	}
	for _, k := range slices.SortedFunc(maps.Keys(excluded), func(a, b [2]string) int {
		return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1]))
	}) {
		var a *string
		if k[0] != "" {
			a = strRef(k[0])
		}
		view.Excluded = append(view.Excluded, LendingStatsExclusion{Kind: side, Pool: strRef(pool), Asset: a, Reason: k[1]})
	}
	sort.Slice(cands, func(i, j int) bool {
		x, y := cands[i], cands[j]
		// Amount ranks only within one reserve (the asset filter); across reserves only USD is comparable.
		c := x.amount.Cmp(y.amount)
		if view.Ranking == topAccountsRankUSD {
			c = x.usd.Cmp(y.usd)
		}
		if c != 0 {
			return c > 0
		}
		if x.row.Account != y.row.Account {
			return x.row.Account < y.row.Account
		}
		return x.row.Asset < y.row.Asset
	})
	for i, c := range cands[:min(limit, len(cands))] {
		c.row.Rank = i + 1
		c.row.RankLowerBound = bound.inexact(c, view.Ranking)
		view.Rows = append(view.Rows, c.row)
	}
	view.LowerBound = len(view.Excluded) > 0 || len(view.Unattributed) > 0
	return view
}

// rankBound is the least a row must hold for its rank to be exact: one unread position could hold a short reserve's whole gap.
type rankBound struct {
	amount *big.Int // ranking by amount: the reserve's unattributed amount
	usd    *big.Rat // ranking by value_usd: the short reserves' unattributed amounts in USD, summed
	all    bool     // a short reserve's gap has no USD value, so no USD rank is exact
}

func (b rankBound) inexact(c topCandidate, ranking string) bool {
	if ranking == topAccountsRankAmount {
		return b.amount != nil && c.amount.Cmp(b.amount) < 0
	}
	return b.all || (b.usd != nil && c.usd.Cmp(b.usd) < 0)
}

// unattributedGaps serves, in reserve order, each ranked reserve's tokens on the side that no read position holds (in asset
// when set), and the bound below which a row's rank is not exact.
func unattributedGaps(built poolTopAccounts, side, asset string, recon map[string]reserveRecon) ([]LendingTopAccountsUnattributed, rankBound) {
	out := []LendingTopAccountsUnattributed{}
	var bound rankBound
	convert := sideConverter(side)
	for _, a := range built.positions.Reserves {
		gap := recon[a].gap
		if gap == nil || (asset != "" && a != asset) {
			continue
		}
		st := built.states[a]
		amount := convert(st.Data, gap)
		out = append(out, LendingTopAccountsUnattributed{
			Asset: a, Side: side, Tokens: gap.String(), Amount: amount.String(),
			ShareOfReserve: ratString(new(big.Rat).SetFrac(gap, sideTotal(st, side)), topAccountsShareDecimals),
		})
		if asset != "" {
			bound.amount = amount
			continue
		}
		amt := amount.String()
		value, reason := legUSD(&amt, built.views[a])
		if reason != "" {
			bound.all = true
			continue
		}
		if bound.usd == nil {
			bound.usd = new(big.Rat)
		}
		bound.usd.Add(bound.usd, value)
	}
	return out, bound
}

func sideConverter(side string) func(blend.ReserveData, *big.Int) *big.Int {
	if side == earningsSideBorrow {
		return blend.ReserveData.DTokensToUnderlying
	}
	return blend.ReserveData.BTokensToUnderlying
}

// sideTotal is the reserve's b_supply (supply) or d_supply (borrow); nil when unread.
func sideTotal(st clickhouse.BlendReserveState, side string) *big.Int {
	if side == earningsSideBorrow {
		return st.Data.DSupply
	}
	return st.Data.BSupply
}

// topAccountCandidate converts one position at its reserve's stored rate and values it through the earnings route's legUSD;
// reason is set when the position cannot be converted (rate_unread) or valued (withheld, unpriced).
func topAccountCandidate(acct clickhouse.LendingPoolPosition, asset string, tokens *big.Int, side string, built poolTopAccounts,
	convert func(blend.ReserveData, *big.Int) *big.Int,
) (topCandidate, string) {
	st, ok := built.states[asset]
	if !ok {
		return topCandidate{}, earningsReasonRateUnread
	}
	rv := built.views[asset]
	amount := convert(st.Data, tokens)
	row := LendingTopAccount{
		Account: acct.Account, Asset: asset, Tokens: tokens.String(), Amount: amount.String(),
		LastModifiedLedger: acct.Ledger,
	}
	if st.DecimalsFound {
		d := int(st.Decimals)
		row.Decimals = &d
	} else if rv.Decimals != nil {
		d := int(*rv.Decimals)
		row.Decimals = &d
	}
	total := rv.Supplied
	if side == earningsSideBorrow {
		total = rv.Borrowed
	}
	if t, ok := parseBig(total); ok && t.Sign() > 0 {
		row.ShareOfReserve = strRef(ratString(new(big.Rat).SetFrac(amount, t), topAccountsShareDecimals))
	}
	value, reason := legUSD(&row.Amount, rv)
	if reason != "" {
		row.UsdReason = strRef(reason)
		return topCandidate{row: row, amount: amount}, reason
	}
	row.ValueUSD = strRef(ratString(value, 2))
	return topCandidate{row: row, amount: amount, usd: value}, ""
}

// heldOn is the account's b-tokens (supply) or d-tokens (borrow) by reserve.
func heldOn(acct clickhouse.LendingPoolPosition, side string) map[string]*big.Int {
	if side == earningsSideBorrow {
		return acct.Borrow
	}
	return acct.Supply
}

// reserveUnranked reports a reserve whose positions cannot be ranked because its total is unread or overcounted.
func reserveUnranked(excluded map[[2]string]bool, asset string) bool {
	return excluded[[2]string{asset, topAccountsReasonOvercounted}] || excluded[[2]string{asset, earningsReasonRateUnread}]
}

// topAccountsReadExclusions keys, by (asset, reason), what the positions read itself leaves out of the ranking.
func topAccountsReadExclusions(built poolTopAccounts, asset string, recon map[string]reserveRecon) map[[2]string]bool {
	excluded := map[[2]string]bool{}
	pos := built.positions
	switch {
	case !pos.Readable:
		excluded[[2]string{"", topAccountsReasonList}] = true
	case pos.Truncated:
		excluded[[2]string{"", topAccountsReasonTruncated}] = true
	}
	if pos.Unreadable > 0 {
		excluded[[2]string{"", topAccountsReasonUnreadable}] = true
	}
	for a, r := range recon {
		if r.reason != "" && (asset == "" || a == asset) {
			excluded[[2]string{a, r.reason}] = true
		}
	}
	return excluded
}

// reserveRecon is one reserve's reconciliation on one side.
type reserveRecon struct {
	reason string   // rate_unread | positions_overcounted: the reserve is not ranked; "" when it is
	gap    *big.Int // tokens of the side's total no read position holds; nil when the positions sum exactly
}

// reconciliation compares each reserve's read tokens on the side with its b_supply (d_supply), with zero tolerance: b- and
// d-tokens are integers, minted and burned in the transaction that writes the position, so a read that missed no entry and
// saw no later write sums exactly. A sum past the total cannot be true and the reserve is not ranked (positions_overcounted);
// a sum short of it ranks the reserve as a lower bound with the gap served, since an unread position may hold it. Nothing is
// reconciled when the read is unreadable or truncated.
func (b poolTopAccounts) reconciliation(side string) map[string]reserveRecon {
	out := map[string]reserveRecon{}
	if !b.positions.Readable || b.positions.Truncated {
		return out
	}
	sums := map[string]*big.Int{}
	for _, acct := range b.positions.Accounts {
		for a, tokens := range heldOn(acct, side) {
			if tokens == nil {
				continue
			}
			if sums[a] == nil {
				sums[a] = new(big.Int)
			}
			sums[a].Add(sums[a], tokens)
		}
	}
	for _, a := range b.positions.Reserves {
		st, ok := b.states[a]
		total := sideTotal(st, side)
		if !ok || total == nil {
			out[a] = reserveRecon{reason: earningsReasonRateUnread}
			continue
		}
		sum := sums[a]
		if sum == nil {
			sum = new(big.Int)
		}
		switch sum.Cmp(total) {
		case 1:
			out[a] = reserveRecon{reason: topAccountsReasonOvercounted}
		case -1:
			out[a] = reserveRecon{gap: new(big.Int).Sub(total, sum)}
		}
	}
	return out
}

// topAccountCandidates converts every non-zero position on the side (in asset when set) and counts the accounts holding one;
// a position it cannot rank is recorded in excluded by (asset, reason). Without asset an unpriced position cannot be ranked.
func topAccountCandidates(built poolTopAccounts, side, asset string, excluded map[[2]string]bool) (cands []topCandidate, accounts int) {
	convert := sideConverter(side)
	for _, acct := range built.positions.Accounts {
		counted := false
		for a, tokens := range heldOn(acct, side) {
			if tokens == nil || tokens.Sign() <= 0 || (asset != "" && a != asset) {
				continue
			}
			if !counted {
				accounts, counted = accounts+1, true
			}
			if reserveUnranked(excluded, a) {
				continue
			}
			c, reason := topAccountCandidate(acct, a, tokens, side, built, convert)
			if reason != "" && (asset == "" || reason == earningsReasonRateUnread) {
				excluded[[2]string{a, reason}] = true
				continue
			}
			cands = append(cands, c)
		}
	}
	return cands, accounts
}
