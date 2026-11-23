package v1

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// LendingLiquidationsView is the GET /v1/lending/pools/{pool}/liquidations body: user-liquidation and bad-debt fills, each valued
// at the fill (valued_at "event_time"), never at today's prices.
type LendingLiquidationsView struct {
	Pool         string                   `json:"pool"`
	ValuedAt     string                   `json:"valued_at"`
	PriceMaxAge  int                      `json:"price_max_age_seconds"`
	Liquidations []LendingLiquidationView `json:"liquidations"`
}

// LendingLiquidationView is one fill. Lot is what the filler received, Bid what it repaid. LotUSD and BidUSD sum the side's valued
// legs (null when none is, a lower bound when a leg is left out); FillerProfitUSD is LotUSD - BidUSD only when both are exact.
type LendingLiquidationView struct {
	AuctionType      uint32                  `json:"auction_type"`
	AuctionTypeLabel string                  `json:"auction_type_label"`
	User             string                  `json:"user"`
	Filler           string                  `json:"filler"`
	FillPercent      string                  `json:"fill_percent"`
	Ledger           uint32                  `json:"ledger"`
	Time             WireTime                `json:"time"`
	TxHash           string                  `json:"tx_hash"`
	Block            *uint32                 `json:"block"`
	AmountsRecorded  bool                    `json:"amounts_recorded"`
	Lot              []LendingLiquidationLeg `json:"lot"`
	Bid              []LendingLiquidationLeg `json:"bid"`
	LotUSD           *string                 `json:"lot_usd"`
	LotUSDLowerBound bool                    `json:"lot_usd_lower_bound"`
	BidUSD           *string                 `json:"bid_usd"`
	BidUSDLowerBound bool                    `json:"bid_usd_lower_bound"`
	FillerProfitUSD  *string                 `json:"filler_profit_usd"`
	USDReason        *string                 `json:"usd_reason"` // set when the fill as a whole cannot be valued
}

// LendingLiquidationLeg is one asset of a side: the event's token amount, its underlying at the fill's reserve rate, and its USD at
// the price bucket that closed at or before the fill (PriceAt); Reason says why a missing value is missing.
type LendingLiquidationLeg struct {
	Asset      string    `json:"asset"`
	Kind       string    `json:"kind"`
	Amount     string    `json:"amount"`
	Underlying *string   `json:"underlying"`
	PriceUSD   *string   `json:"price_usd"`
	PriceAt    *WireTime `json:"price_at"`
	USD        *string   `json:"usd"`
	Reason     *string   `json:"reason"`
}

// lendingLiquidationReader is the fill history behind GET /v1/lending/pools/{pool}/liquidations.
type lendingLiquidationReader interface {
	LendingLiquidations(ctx context.Context, pool string, limit int, cur timescale.LendingConfigCursor) ([]timescale.LendingLiquidationFill, error)
}

var _ lendingLiquidationReader = (*timescale.Store)(nil)

// errNoLiquidationReader is a lending reader wired without fill history; an empty list would read as "no liquidations".
var errNoLiquidationReader = errors.New("lending: liquidation history reader not wired")

// LendingLiquidations is a pass-through; errNoLiquidationReader when the upstream has no fill history.
func (c *CachedLendingReader) LendingLiquidations(ctx context.Context, pool string, limit int, cur timescale.LendingConfigCursor) ([]timescale.LendingLiquidationFill, error) {
	if up, ok := c.upstream.(lendingLiquidationReader); ok {
		return up.LendingLiquidations(ctx, pool, limit, cur)
	}
	return nil, errNoLiquidationReader
}

const (
	liquidationCursorKind = "fill_auction"
	liquidationValuedAt   = "event_time"
)

// handleLendingPoolLiquidations serves GET /v1/lending/pools/{pool}/liquidations: the pool's fills newest first, keyset-paginated.
func (s *Server) handleLendingPoolLiquidations(w http.ResponseWriter, r *http.Request) {
	pool := r.PathValue("pool")
	if !canonical.IsContractID(pool) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-pool",
			"Invalid pool", http.StatusBadRequest, "pool must be a C-strkey contract id")
		return
	}
	limit, ok := parseExplorerLimit(w, r, 50, 200)
	if !ok {
		return
	}
	cur, ok := parseLendingCursor(w, r, map[string]bool{liquidationCursorKind: true})
	if !ok {
		return
	}
	lr, ok := s.Lending.(lendingLiquidationReader)
	if !ok {
		writeLendingLiquidationsUnavailable(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxHandlerBudget)
	defer cancel()
	rows, err := lr.LendingLiquidations(ctx, pool, limit+1, cur)
	if errors.Is(err, errNoLiquidationReader) {
		writeLendingLiquidationsUnavailable(w, r)
		return
	}
	if err != nil {
		s.writeLendingLookupError(ctx, w, r, "LendingLiquidations", pool, err)
		return
	}
	view := LendingLiquidationsView{
		Pool: pool, ValuedAt: liquidationValuedAt, PriceMaxAge: int(liquidationPriceMaxAge / time.Second),
		Liquidations: make([]LendingLiquidationView, 0, len(rows)),
	}
	env := Envelope{Data: &view, AsOf: WireTime(time.Now().UTC())}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1].LendingEventPos
		env.Pagination = &Pagination{Next: encodeLendingConfigCursor(timescale.LendingConfigChange{
			Ts: last.Ts, Ledger: last.Ledger, TxHash: last.TxHash, OpIndex: last.OpIndex, EventIndex: last.EventIndex, Kind: liquidationCursorKind,
		})}
	}
	if len(rows) > 0 {
		protocol, _, lerr := s.backstopLineage(ctx, pool)
		if lerr != nil {
			s.logger.Warn("lending liquidations: lineage read failed", "pool", pool, "err", lerr)
			env.Flags.Degraded = true
		}
		lv := liqValuer{protocol: protocol, marks: s.liquidationMarks(ctx, pool, protocol, rows, &env.Flags)}
		for _, f := range rows {
			view.Liquidations = append(view.Liquidations, lv.value(f))
		}
	}
	writeEnvelope(w, env)
}

func writeLendingLiquidationsUnavailable(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, "https://api.stellarindex.io/errors/lending-unavailable",
		"Lending liquidations unavailable", http.StatusServiceUnavailable,
		"This deployment hasn't wired the lending liquidation history reader.")
}

// liquidationView is a fill's identity and token amounts, before valuation.
func liquidationView(f timescale.LendingLiquidationFill) LendingLiquidationView {
	label, ok := auctionTypeLabels[f.AuctionType]
	if !ok {
		label = "unknown_" + strconv.FormatUint(uint64(f.AuctionType), 10)
	}
	return LendingLiquidationView{
		AuctionType: f.AuctionType, AuctionTypeLabel: label, User: f.User, Filler: f.Filler, FillPercent: f.FillPercent,
		Ledger: f.Ledger, Time: WireTime(f.Ts), TxHash: f.TxHash, Block: f.Block, AmountsRecorded: f.HasAmounts,
		Lot: []LendingLiquidationLeg{}, Bid: []LendingLiquidationLeg{},
	}
}
