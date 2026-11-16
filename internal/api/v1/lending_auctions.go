package v1

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// LendingAuctionsView is the GET /v1/lending/pools/{pool}/auctions body.
type LendingAuctionsView struct {
	Pool     string               `json:"pool"`
	Auctions []LendingAuctionView `json:"auctions"`
}

// LendingAuctionView is one auction: its start, bid and lot (asset -> base-unit decimal string), fills and delete.
type LendingAuctionView struct {
	AuctionType      uint32                   `json:"auction_type"`
	AuctionTypeLabel string                   `json:"auction_type_label"`
	User             string                   `json:"user"`
	StartLedger      uint32                   `json:"start_ledger"`
	StartTime        WireTime                 `json:"start_time"`
	TxHash           string                   `json:"tx_hash"`
	Percent          *uint32                  `json:"percent"`
	Block            *uint32                  `json:"block"` // the auction's pricing start ledger
	Bid              map[string]string        `json:"bid"`
	Lot              map[string]string        `json:"lot"`
	Fills            []LendingAuctionFillView `json:"fills"`
	Deleted          bool                     `json:"deleted"`
	DeletedLedger    *uint32                  `json:"deleted_ledger,omitempty"`
	DeletedTime      *WireTime                `json:"deleted_time,omitempty"`
}

// LendingAuctionFillView is one fill of an auction.
type LendingAuctionFillView struct {
	Filler      string   `json:"filler"`
	FillPercent string   `json:"fill_percent"`
	Ledger      uint32   `json:"ledger"`
	Time        WireTime `json:"time"`
	TxHash      string   `json:"tx_hash"`
}

// auctionTypeLabels is pool auctions/auction.rs AuctionType; protocol_fee exists in BLNT only.
var auctionTypeLabels = map[uint32]string{0: "user_liquidation", 1: "bad_debt", 2: "interest", 3: "protocol_fee"}

// auctionCursorKind fills the shared cursor's kind slot; the auctions query ignores it.
const auctionCursorKind = "new_auction"

// handleLendingPoolAuctions serves GET /v1/lending/pools/{pool}/auctions: the pool's auctions newest first, keyset-paginated.
func (s *Server) handleLendingPoolAuctions(w http.ResponseWriter, r *http.Request) {
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
	cur, ok := parseLendingCursor(w, r, map[string]bool{auctionCursorKind: true})
	if !ok {
		return
	}
	view := LendingAuctionsView{Pool: pool, Auctions: []LendingAuctionView{}}
	ar, ok := s.Lending.(lendingAuctionReader)
	if !ok {
		writeJSON(w, view, Flags{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxHandlerBudget)
	defer cancel()
	rows, truncated, err := ar.LendingAuctions(ctx, pool, limit+1, cur)
	if err != nil {
		s.writeLendingLookupError(ctx, w, r, "LendingAuctions", pool, err)
		return
	}
	env := Envelope{Data: &view, AsOf: WireTime(time.Now().UTC()), Flags: Flags{Degraded: truncated}}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1].LendingEventPos
		env.Pagination = &Pagination{Next: encodeLendingConfigCursor(timescale.LendingConfigChange{
			Ts: last.Ts, Ledger: last.Ledger, TxHash: last.TxHash, OpIndex: last.OpIndex, EventIndex: last.EventIndex, Kind: auctionCursorKind,
		})}
	}
	for _, a := range rows {
		view.Auctions = append(view.Auctions, auctionView(a))
	}
	writeEnvelope(w, env)
}

func auctionView(a timescale.LendingAuction) LendingAuctionView {
	label, ok := auctionTypeLabels[a.AuctionType]
	if !ok {
		label = "unknown_" + strconv.FormatUint(uint64(a.AuctionType), 10)
	}
	v := LendingAuctionView{
		AuctionType: a.AuctionType, AuctionTypeLabel: label, User: a.User, StartLedger: a.Ledger, StartTime: WireTime(a.Ts),
		TxHash: a.TxHash, Percent: a.Percent, Block: a.Block, Bid: a.Bid, Lot: a.Lot, Fills: []LendingAuctionFillView{},
	}
	for _, f := range a.Fills {
		v.Fills = append(v.Fills, LendingAuctionFillView{Filler: f.Filler, FillPercent: f.FillPercent, Ledger: f.Ledger, Time: WireTime(f.Ts), TxHash: f.TxHash})
	}
	if d := a.Deleted; d != nil {
		at := WireTime(d.Ts)
		v.Deleted, v.DeletedLedger, v.DeletedTime = true, &d.Ledger, &at
	}
	return v
}
