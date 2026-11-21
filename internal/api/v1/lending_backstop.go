package v1

import (
	"context"
	"math/big"
	"net/http"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// lendingBackstopReader is the lake read of a pool's backstop balances.
type lendingBackstopReader interface {
	LendingBackstop(ctx context.Context, pool string, tiered bool) (clickhouse.LendingBackstopState, error)
}

// lendingAuctionReader is the auction history behind GET /v1/lending/pools/{pool}/auctions.
type lendingAuctionReader interface {
	LendingAuctions(ctx context.Context, pool string, limit int, cur timescale.LendingConfigCursor) ([]timescale.LendingAuction, bool, error)
}

var (
	_ lendingBackstopReader = (*clickhouse.ExplorerReader)(nil)
	_ lendingAuctionReader  = (*timescale.Store)(nil)
	_ lendingAuctionReader  = (*CachedLendingReader)(nil)
)

// LendingBackstopView is the GET /v1/lending/pools/{pool}/backstop body: the backstop's balances for this pool from contract storage.
type LendingBackstopView struct {
	Pool     string  `json:"pool"`
	Protocol *string `json:"protocol"` // blend | blnt; null when the pool's lineage is unknown
	// Available is false when the backstop cannot be read for this pool; UnavailableReason says why and the other fields are null.
	Available         bool                      `json:"available"`
	UnavailableReason *string                   `json:"unavailable_reason,omitempty"`
	Backstop          *string                   `json:"backstop"`       // backstop contract C-strkey, from the pool's instance
	BackstopToken     *string                   `json:"backstop_token"` // Blend only; a BLNT tier carries its own
	TakeRate          *string                   `json:"take_rate"`      // raw contract integer at config_decimals
	ConfigDecimals    int                       `json:"config_decimals"`
	Tiers             []LendingBackstopTierView `json:"tiers"`
	AsOfLedger        uint32                    `json:"as_of_ledger,omitempty"`
	// TotalUSD sums the valued tiers' tokens_usd; null when none is valued, and LowerBound when a tier is left out.
	TotalUSD   *string `json:"total_usd"`
	LowerBound bool    `json:"lower_bound"`
}

// LendingBackstopTierView is one backstop tier's PoolBalance; the amounts are null when the lake holds no live entry.
type LendingBackstopTierView struct {
	Tier         *string  `json:"tier"` // FirstLoss | SecondLoss | ThirdLoss; null for Blend's single tier
	Tokens       *string  `json:"tokens"`
	Shares       *string  `json:"shares"`
	QueuedShares *string  `json:"queued_shares"`
	QueuedPct    *float64 `json:"queued_pct"`
	// BackstopAsset, BackstopToken and TakeRateWeight are a BLNT tier's configuration; null for Blend and when unread.
	BackstopAsset  *string `json:"backstop_asset"`
	BackstopToken  *string `json:"backstop_token"`
	TakeRateWeight *uint32 `json:"take_rate_weight"`
	// TokensUSD is tokens at the backstop token's current price; null with USDReason when it cannot be valued exactly.
	TokensUSD  *string `json:"tokens_usd"`
	PriceUSD   *string `json:"price_usd"`
	PriceStale bool    `json:"price_stale"`
	USDReason  *string `json:"usd_reason"`
}

const (
	backstopReasonV1      = "blend_v1_unsupported"
	backstopReasonUnknown = "pool_lineage_unknown"
	backstopReasonNoState = "pool_instance_unavailable"
)

// handleLendingPoolBackstop serves GET /v1/lending/pools/{pool}/backstop from the backstop contract's storage in the lake.
func (s *Server) handleLendingPoolBackstop(w http.ResponseWriter, r *http.Request) {
	if s.Explorer == nil {
		s.explorerUnavailable(w, r)
		return
	}
	pool := r.PathValue("pool")
	if !canonical.IsContractID(pool) {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-pool",
			"Invalid pool", http.StatusBadRequest, "pool must be a C-strkey contract id")
		return
	}
	view := LendingBackstopView{Pool: pool, ConfigDecimals: lendingConfigDecimals, Tiers: []LendingBackstopTierView{}}
	br, ok := s.Explorer.(lendingBackstopReader)
	if !ok || s.Lending == nil {
		view.UnavailableReason = strRef(backstopReasonUnknown)
		writeJSON(w, view, Flags{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxHandlerBudget)
	defer cancel()
	protocol, reason, err := s.backstopLineage(ctx, pool)
	if err != nil {
		s.writeLendingLookupError(ctx, w, r, "lending backstop lineage", pool, err)
		return
	}
	if protocol != "" {
		view.Protocol = strRef(protocol)
	}
	if reason != "" {
		view.UnavailableReason = strRef(reason)
		writeJSON(w, view, Flags{})
		return
	}
	st, err := br.LendingBackstop(ctx, pool, protocol == blnt.SourceName)
	if err != nil {
		s.writeLendingLookupError(ctx, w, r, "LendingBackstop", pool, err)
		return
	}
	fillBackstopView(&view, st)
	var priceStale, priceUnread bool
	if view.Available {
		priceStale, priceUnread = s.valueBackstopView(ctx, &view, st)
	}
	wm, stale, _ := s.lakeWatermark(ctx)
	view.AsOfLedger = wm
	writeEnvelope(w, Envelope{Data: view, AsOf: WireTime(time.Now().UTC()), Flags: Flags{Stale: stale || priceStale, Degraded: priceUnread}})
}

// backstopLineage names the pool's protocol, or the unavailable reason: V1 backstop storage keys are not verified, so never guessed.
func (s *Server) backstopLineage(ctx context.Context, pool string) (protocol, reason string, err error) {
	version, err := s.Lending.BlendPoolVersion(ctx, pool)
	if err != nil {
		return "", "", err
	}
	switch version {
	case blend.PoolV2:
		return "blend", "", nil
	case blend.PoolV1:
		return "blend", backstopReasonV1, nil
	case blend.PoolVersionUnknown:
	}
	if lr, ok := s.Lending.(blntLendingReader); ok {
		assets, err := lr.BLNTPoolAssets(ctx, pool)
		if err != nil {
			return "", "", err
		}
		if len(assets) > 0 {
			return blnt.SourceName, "", nil
		}
	}
	return "", backstopReasonUnknown, nil
}

func fillBackstopView(view *LendingBackstopView, st clickhouse.LendingBackstopState) {
	if st.Instance == nil || st.Instance.Backstop == "" {
		view.UnavailableReason = strRef(backstopReasonNoState)
		return
	}
	view.Available = true
	view.Backstop = strRef(st.Instance.Backstop)
	view.BackstopToken = nonEmptyPtr(st.Token)
	view.TakeRate = strRef(u32String(st.Instance.Config.BstopRate))
	for _, t := range st.Tiers {
		tv := LendingBackstopTierView{
			Tier: nonEmptyPtr(t.Tier), BackstopAsset: nonEmptyPtr(t.Asset), BackstopToken: nonEmptyPtr(t.Token),
			TakeRateWeight: t.TakeRateWeight,
		}
		if b := t.Balance; b != nil {
			tv.Tokens, tv.Shares, tv.QueuedShares = strRef(b.Tokens.String()), strRef(b.Shares.String()), strRef(b.Q4W.String())
			tv.QueuedPct = sharePct(b.Q4W, b.Shares)
		}
		view.Tiers = append(view.Tiers, tv)
	}
}

// sharePct is part/whole as a percentage (2dp); nil when whole is not positive.
func sharePct(part, whole *big.Int) *float64 {
	if whole.Sign() <= 0 {
		return nil
	}
	pct, _ := new(big.Rat).SetFrac(new(big.Int).Mul(part, big.NewInt(100)), whole).Float64() // i128:ok share percentage, not an amount
	pct = round2(pct)
	return &pct
}

func strRef(s string) *string { return &s }
