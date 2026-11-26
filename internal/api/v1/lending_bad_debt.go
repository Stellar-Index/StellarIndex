package v1

import (
	"context"
	"errors"
	"net/http"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// LendingBadDebtView is the GET /v1/lending/pools/{pool}/bad-debt body: per reserve, the debt moved to the backstop and where it
// went, in d-tokens, underlying and USD at each event (outstanding at the current rate and price).
type LendingBadDebtView struct {
	Pool     string `json:"pool"`
	Protocol string `json:"protocol"`
	ValuedAt string `json:"valued_at"`
	// Backstop is the address the pool's instance storage names; null with BackstopReason when its position is not read.
	Backstop       *string `json:"backstop"`
	BackstopReason *string `json:"backstop_reason"`
	// PositionLedger is the ledger the backstop's position was last written at; the identity counts events up to it.
	PositionLedger      *uint32                 `json:"position_ledger"`
	EventsAfterPosition int                     `json:"events_after_position"`
	ZeroBidFills        int                     `json:"zero_bid_fills"`
	UnrecordedBidFills  int                     `json:"unrecorded_bid_fills"`
	Reserves            []LendingBadDebtReserve `json:"reserves"`
	TotalsUSD           LendingBadDebtTotals    `json:"totals_usd"`
}

// LendingBadDebtReserve is one reserve's parts; Reconciled checks moved = covered + socialised + repaid + outstanding in d-tokens.
type LendingBadDebtReserve struct {
	Asset                 string             `json:"asset"`
	MovedToBackstop       LendingBadDebtPart `json:"moved_to_backstop"`
	CoveredByBackstop     LendingBadDebtPart `json:"covered_by_backstop"`
	SocialisedToSuppliers LendingBadDebtPart `json:"socialised_to_suppliers"`
	RepaidByBackstop      LendingBadDebtPart `json:"repaid_by_backstop"`
	Outstanding           LendingBadDebtPart `json:"outstanding"`
	Reconciled            *bool              `json:"reconciled"`
	ReconcileReason       *string            `json:"reconcile_reason"`
	ResidualDTokens       *string            `json:"residual_d_tokens"`
}

// LendingBadDebtPart is one part's d-tokens (null with DTokensReason when not derivable), underlying and USD (null with USDReason).
type LendingBadDebtPart struct {
	DTokens       *string `json:"d_tokens"`
	DTokensReason *string `json:"d_tokens_reason"`
	Underlying    *string `json:"underlying"`
	USD           *string `json:"usd"`
	USDReason     *string `json:"usd_reason"`
	Events        int     `json:"events"`
}

// LendingBadDebtTotals is each part's USD summed over the pool's reserves.
type LendingBadDebtTotals struct {
	MovedToBackstop       LendingBadDebtTotal `json:"moved_to_backstop"`
	CoveredByBackstop     LendingBadDebtTotal `json:"covered_by_backstop"`
	SocialisedToSuppliers LendingBadDebtTotal `json:"socialised_to_suppliers"`
	RepaidByBackstop      LendingBadDebtTotal `json:"repaid_by_backstop"`
	Outstanding           LendingBadDebtTotal `json:"outstanding"`
}

// LendingBadDebtTotal is a part's pool USD: null when no reserve is valued, a lower bound when Excluded names one left out.
type LendingBadDebtTotal struct {
	USD        *string                   `json:"usd"`
	LowerBound bool                      `json:"lower_bound"`
	Excluded   []LendingBadDebtExclusion `json:"excluded"`
}

// LendingBadDebtExclusion is a reserve a total leaves out, and why.
type LendingBadDebtExclusion struct {
	Asset  string `json:"asset"`
	Reason string `json:"reason"`
}

// lendingBadDebtReader is the event history behind GET /v1/lending/pools/{pool}/bad-debt.
type lendingBadDebtReader interface {
	LendingBadDebtEvents(ctx context.Context, pool, backstop string, blnt bool) ([]timescale.LendingBadDebtEvent, error)
}

var _ lendingBadDebtReader = (*timescale.Store)(nil)

// lendingPositionsReader reads an account's current Positions entry in a pool from the lake.
type lendingPositionsReader interface {
	LendingAccountPositions(ctx context.Context, pool, account string) (clickhouse.LendingAccountPositions, error)
}

var errNoBadDebtReader = errors.New("lending: bad-debt reader not wired")

// LendingBadDebtEvents is a pass-through; errNoBadDebtReader when the upstream has no bad-debt history.
func (c *CachedLendingReader) LendingBadDebtEvents(ctx context.Context, pool, backstop string, isBLNT bool) ([]timescale.LendingBadDebtEvent, error) {
	if up, ok := c.upstream.(lendingBadDebtReader); ok {
		return up.LendingBadDebtEvents(ctx, pool, backstop, isBLNT)
	}
	return nil, errNoBadDebtReader
}

// handleLendingPoolBadDebt serves GET /v1/lending/pools/{pool}/bad-debt. Invariant: a part that cannot be derived is null with a
// reason, never 0; a 0 is a sum over events (0 of them for a pool with no bad debt).
func (s *Server) handleLendingPoolBadDebt(w http.ResponseWriter, r *http.Request) {
	pool := r.PathValue("pool")
	if !canonical.IsContractID(pool) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-pool",
			"Invalid pool", http.StatusBadRequest, "pool must be a C-strkey contract id")
		return
	}
	br, ok := s.Lending.(lendingBadDebtReader)
	if !ok {
		writeLendingBadDebtUnavailable(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxHandlerBudget)
	defer cancel()
	protocol, lineage, err := s.backstopLineage(ctx, pool)
	if err != nil {
		s.writeLendingLookupError(ctx, w, r, "lending bad-debt lineage", pool, err)
		return
	}
	if protocol == "" {
		writeLendingPoolNotFound(w, r)
		return
	}
	var flags Flags
	in, backstop, err := s.badDebtPoolState(ctx, pool, protocol, lineage)
	if err != nil {
		s.writeLendingLookupError(ctx, w, r, "lending bad-debt pool state", pool, err)
		return
	}
	in.events, err = br.LendingBadDebtEvents(ctx, pool, backstop, protocol == blnt.SourceName)
	switch {
	case errors.Is(err, errNoBadDebtReader):
		writeLendingBadDebtUnavailable(w, r)
		return
	case errors.Is(err, timescale.ErrLendingBadDebtCap):
		writeProblem(w, r, "https://api.stellarindex.io/errors/lending-unavailable", "Lending bad debt unavailable",
			http.StatusServiceUnavailable, "This pool has more bad-debt events than one read sums; no partial total is served.")
		return
	case err != nil:
		s.writeLendingLookupError(ctx, w, r, "LendingBadDebtEvents", pool, err)
		return
	}
	if backstop != "" {
		in.position, in.backstopReason = s.badDebtPosition(ctx, pool, backstop, &flags)
	}
	in.now = s.now()
	in.marks = s.badDebtMarks(ctx, pool, liquidationStateModel(protocol, lineage), in, &flags)
	view := LendingBadDebtView{Pool: pool, Protocol: protocol, ValuedAt: liquidationValuedAt}
	if backstop != "" {
		view.Backstop = strRef(backstop)
	}
	if in.backstopReason != "" {
		view.BackstopReason = strRef(in.backstopReason)
	} else {
		l := in.position.Ledger
		view.PositionLedger = &l
	}
	var facts badDebtPoolFacts
	view.Reserves, view.TotalsUSD, facts = foldBadDebt(in)
	view.EventsAfterPosition, view.ZeroBidFills, view.UnrecordedBidFills = facts.eventsAfterPosition, facts.zeroBidFills, facts.unrecordedBidFills
	writeEnvelope(w, Envelope{Data: view, AsOf: WireTime(in.now.UTC()), Flags: flags})
}

// badDebtPoolState reads the pool's reserves, their current state and the backstop its instance storage names. BLNT's
// backstop is tiered and which address carries moved debt is unverified, so its position is never read.
func (s *Server) badDebtPoolState(ctx context.Context, pool, protocol, lineage string) (badDebtInputs, string, error) {
	var in badDebtInputs
	if protocol == blnt.SourceName {
		in.backstopReason = badDebtReasonBackstopRoleUnverified
		if lr, ok := s.Lending.(blntLendingReader); ok {
			assets, err := lr.BLNTPoolAssets(ctx, pool)
			in.assets = assets
			return in, "", err
		}
		return in, "", nil
	}
	if s.Explorer == nil {
		in.backstopReason = badDebtReasonLakeUnavailable
		assets, err := s.Lending.BlendPoolAssets(ctx, pool)
		in.assets = assets
		return in, "", err
	}
	version := blend.PoolV2
	if lineage == backstopReasonV1 {
		version = blend.PoolV1
	}
	pr, err := s.readBlendPoolReserves(ctx, pool, version)
	if err != nil {
		return in, "", err
	}
	in.assets = pr.assets
	in.current = make(map[string]blend.ReserveData, len(pr.state.Reserves))
	for _, st := range pr.state.Reserves {
		in.current[st.Asset] = st.Data
	}
	if pr.state.Instance == nil || pr.state.Instance.Backstop == "" {
		in.backstopReason = badDebtReasonBackstopUnresolved
		return in, "", nil
	}
	return in, pr.state.Instance.Backstop, nil
}

// badDebtPosition reads the backstop's current Positions entry; an unreadable one leaves repaid and outstanding null.
func (s *Server) badDebtPosition(ctx context.Context, pool, backstop string, flags *Flags) (clickhouse.LendingAccountPositions, string) {
	pr, ok := s.Explorer.(lendingPositionsReader)
	if !ok {
		return clickhouse.LendingAccountPositions{}, badDebtReasonLakeUnavailable
	}
	pos, err := pr.LendingAccountPositions(ctx, pool, backstop)
	if err != nil {
		s.logger.Warn("lending bad debt: backstop position read failed", "pool", pool, "err", err)
		flags.Degraded = true
		return clickhouse.LendingAccountPositions{}, badDebtReasonPositionUnread
	}
	if !pos.Readable {
		return clickhouse.LendingAccountPositions{}, badDebtReasonPositionUnread
	}
	return pos, ""
}

// badDebtMarks reads one price series per asset from the oldest event to now, and the reserve state every event's
// transaction stored, through the liquidations valuer.
func (s *Server) badDebtMarks(ctx context.Context, pool, model string, in badDebtInputs, flags *Flags) map[string]*liqMarks {
	need := map[string]bool{}
	oldest := in.now
	var txs []timescale.LendingLiquidationFill
	for _, e := range in.events {
		if d, ok := parseBig(e.DTokens); e.Asset == "" || !ok || d.Sign() == 0 {
			continue
		}
		need[e.Asset] = true
		if e.Ts.Before(oldest) {
			oldest = e.Ts
		}
		txs = append(txs, timescale.LendingLiquidationFill{LendingEventPos: timescale.LendingEventPos{Ledger: e.Ledger, TxHash: e.TxHash, Ts: e.Ts}})
	}
	for a, d := range in.position.Borrow {
		if d != nil && d.Sign() > 0 {
			need[a] = true
		}
	}
	out := make(map[string]*liqMarks, len(need))
	assets := make([]string, 0, len(need))
	for a := range need {
		out[a] = s.liquidationPrices(ctx, a, oldest, in.now, flags)
		assets = append(assets, a)
	}
	if len(txs) == 0 {
		return out
	}
	rates, reason := s.liquidationRates(ctx, pool, model, assets, txs, flags)
	for _, a := range assets {
		m := out[a]
		m.rateReason, m.rates = reason, map[clickhouse.LendingTxRef]blend.ReserveData{}
		for k, d := range rates {
			if k.Asset == a {
				m.rates[k.LendingTxRef] = d
			}
		}
	}
	return out
}

func writeLendingBadDebtUnavailable(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, "https://api.stellarindex.io/errors/lending-unavailable",
		"Lending bad debt unavailable", http.StatusServiceUnavailable,
		"This deployment hasn't wired the lending bad-debt reader.")
}
