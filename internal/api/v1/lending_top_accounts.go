package v1

import (
	"cmp"
	"context"
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

	topAccountsReasonTruncated    = "positions_truncated"
	topAccountsReasonUnreadable   = "positions_unreadable"
	topAccountsReasonList         = "reserve_list_unreadable"
	topAccountsReasonUnreconciled = "positions_unreconciled"

	topAccountsDefaultLimit  = 20
	topAccountsMaxLimit      = 100
	topAccountsShareDecimals = 6
)

const lendingTopAccountsNote = "Each row is one account's current position in one reserve, read from the pool's Positions storage; amount is " +
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
	// LowerBound marks rows and accounts as incomplete; Excluded names what was left out.
	LowerBound bool                    `json:"lower_bound"`
	Excluded   []LendingStatsExclusion `json:"excluded"`
	Rows       []LendingTopAccount     `json:"rows"`
	AsOfLedger uint32                  `json:"as_of_ledger"`
	Note       string                  `json:"note"`
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
}

// poolTopAccounts is one pool's cached positions and the reserve states and views that convert and price them.
type poolTopAccounts struct {
	protocol        string
	positions       clickhouse.LendingPoolPositions
	states          map[string]clickhouse.BlendReserveState
	views           map[string]ReserveView
	stale, degraded bool
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
	built, _, cacheStale, err := s.lendingTopAccounts.get(ctx, "pool_top_accounts", pool, s.fillPoolTopAccounts(pr, pool, protocol))
	if err != nil {
		s.writeLendingLookupError(ctx, w, r, "lending top accounts", pool, err)
		return
	}
	view := rankTopAccounts(pool, built, side, asset, limit)
	wm, lakeStale, _ := s.lakeWatermark(ctx)
	view.AsOfLedger = wm
	flags := Flags{Stale: cacheStale || lakeStale || built.stale, Degraded: built.degraded || view.LowerBound}
	writeEnvelope(w, Envelope{Data: view, AsOf: WireTime(time.Now().UTC()), Flags: flags})
}

func (s *Server) fillPoolTopAccounts(pr lendingPoolPositionsReader, pool, protocol string) func(context.Context) (poolTopAccounts, error) {
	return func(ctx context.Context) (poolTopAccounts, error) {
		pos, err := pr.LendingPoolPositions(ctx, pool)
		if err != nil {
			return poolTopAccounts{}, err
		}
		states, views, stale, degraded, err := s.earningsReserveStates(ctx, pool)
		if err != nil {
			return poolTopAccounts{}, err
		}
		return poolTopAccounts{protocol: protocol, positions: pos, states: states, views: views, stale: stale, degraded: degraded}, nil
	}
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
	if pos.Readable && !pos.Truncated {
		for a, reason := range reserveReconciliation(built, side) {
			if asset == "" || a == asset {
				excluded[[2]string{a, reason}] = true
			}
		}
	}
	cands, accounts := topAccountCandidates(built, side, asset, excluded)
	view.Accounts = accounts
	if pos.Truncated {
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
		view.Rows = append(view.Rows, c.row)
	}
	view.LowerBound = len(view.Excluded) > 0
	return view
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

// reserveReconciliation names each reserve whose positions on the side cannot be shown complete: its total is unread
// (rate_unread), or the read tokens do not sum EXACTLY to its b_supply (d_supply). The tolerance is zero: b- and d-tokens
// are integers, minted and burned in the same transaction that writes the position, so a read that missed no entry and
// saw no later write sums exactly; any difference is a missing entry, a key layout this read does not match, or a write
// between the two reads, and none of those can be ranked truthfully.
func reserveReconciliation(built poolTopAccounts, side string) map[string]string {
	sums := map[string]*big.Int{}
	for _, acct := range built.positions.Accounts {
		held := acct.Supply
		if side == earningsSideBorrow {
			held = acct.Borrow
		}
		for a, tokens := range held {
			if tokens == nil {
				continue
			}
			if sums[a] == nil {
				sums[a] = new(big.Int)
			}
			sums[a].Add(sums[a], tokens)
		}
	}
	out := map[string]string{}
	for _, a := range built.positions.Reserves {
		st, ok := built.states[a]
		total := st.Data.BSupply
		if side == earningsSideBorrow {
			total = st.Data.DSupply
		}
		sum := sums[a]
		if sum == nil {
			sum = new(big.Int)
		}
		switch {
		case !ok || total == nil:
			out[a] = earningsReasonRateUnread
		case sum.Cmp(total) != 0:
			out[a] = topAccountsReasonUnreconciled
		}
	}
	return out
}

// topAccountCandidates converts every non-zero position on the side (in asset when set) and counts the accounts holding one;
// a position it cannot rank is recorded in excluded by (asset, reason). Without asset an unpriced position cannot be ranked.
func topAccountCandidates(built poolTopAccounts, side, asset string, excluded map[[2]string]bool) (cands []topCandidate, accounts int) {
	convert := blend.ReserveData.BTokensToUnderlying
	if side == earningsSideBorrow {
		convert = blend.ReserveData.DTokensToUnderlying
	}
	for _, acct := range built.positions.Accounts {
		held := acct.Supply
		if side == earningsSideBorrow {
			held = acct.Borrow
		}
		counted := false
		for a, tokens := range held {
			if tokens == nil || tokens.Sign() <= 0 || (asset != "" && a != asset) {
				continue
			}
			if !counted {
				accounts, counted = accounts+1, true
			}
			if excluded[[2]string{a, topAccountsReasonUnreconciled}] || excluded[[2]string{a, earningsReasonRateUnread}] {
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
