package pipeline

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/band"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	blend_backstop "github.com/Stellar-Index/StellarIndex/internal/sources/blend_backstop"
	"github.com/Stellar-Index/StellarIndex/internal/sources/cctp"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/defindex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	"github.com/Stellar-Index/StellarIndex/internal/sources/redstone"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	"github.com/Stellar-Index/StellarIndex/internal/sources/rozo"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	sep41_supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41_transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	soroswap_router "github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
)

// TestIsProjectedEvent_TableDriven pins IsProjectedEvent to the ADR-0032 contract: every event from a source projector buildSource returns is projected=true,
// every event from out-of-scope sources (sdex, external CEX/FX, band, supply observers) is false. Drift means a silent double-write or dropped event.
func TestIsProjectedEvent_TableDriven(t *testing.T) {
	cases := []struct {
		name      string
		event     consumer.Event
		projected bool
	}{
		// ── Projected (projector writes; dispatcher skips Phase 4+) ──
		{"soroswap.TradeEvent", soroswap.TradeEvent{Trade: canonical.Trade{Source: "soroswap"}}, true},
		{"soroswap.SkimEvent", soroswap.SkimEvent{}, true},
		{"aquarius.TradeEvent", aquarius.TradeEvent{Trade: canonical.Trade{Source: "aquarius"}}, true},
		{"phoenix.TradeEvent", phoenix.TradeEvent{Trade: canonical.Trade{Source: "phoenix"}}, true},
		{"phoenix.LiquidityEvent", phoenix.LiquidityEvent{}, true},
		{"phoenix.StakeEvent", phoenix.StakeEvent{}, true},
		{"comet.TradeEvent", comet.TradeEvent{Trade: canonical.Trade{Source: "comet"}}, true},
		{"comet.LiquidityEvent", comet.LiquidityEvent{}, true},
		{"reflector.UpdateEvent", reflector.UpdateEvent{Update: canonical.OracleUpdate{Source: "reflector-dex"}}, true},
		{"redstone.UpdateEvent", redstone.UpdateEvent{Update: canonical.OracleUpdate{Source: "redstone"}}, true},
		{"blend.NewAuctionEvent", blend.NewAuctionEvent{}, true},
		{"blend.FillAuctionEvent", blend.FillAuctionEvent{}, true},
		{"blend.DeleteAuctionEvent", blend.DeleteAuctionEvent{}, true},
		{"blend.PositionEvent", blend.PositionEvent{}, true},
		{"blend.EmissionEvent", blend.EmissionEvent{}, true},
		{"blend.AdminEvent", blend.AdminEvent{}, true},
		{"blend_backstop.Event", blend_backstop.Event{}, true},
		{"cctp.Event", cctp.Event{}, true},
		{"rozo.Event", rozo.Event{}, true},
		{"defindex.Event", defindex.Event{}, true},
		{"defindex.VaultEvent", defindex.VaultEvent{}, true},
		{"sep41_supply.Event", sep41_supply.Event{}, true},
		{"sep41_transfers.Event", sep41_transfers.Event{}, true},

		// ── Non-projected (dispatcher continues to write Phase 4+) ──
		{"sdex.TradeEvent", sdex.TradeEvent{Trade: canonical.Trade{Source: "sdex"}}, false},
		{"external.TradeEvent", external.TradeEvent{Trade: canonical.Trade{Source: "binance"}}, false},
		{"external.UpdateEvent", external.UpdateEvent{Update: canonical.OracleUpdate{Source: "ecb"}}, false},
		{"band.UpdateEvent", band.UpdateEvent{Update: canonical.OracleUpdate{Source: "band"}}, false},
		// soroswap_router.Event is log-only (written only to soroswap_router_swaps) and MUST stay projected=false:
		// RouterSwap amounts mix realized amounts with a user slippage LIMIT, so a price from them would be the limit price and corrupt VWAP.
		{"soroswap_router.Event", soroswap_router.Event{Swap: soroswap_router.RouterSwap{Source: soroswap_router.SourceName}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsProjectedEvent(tc.event)
			if got != tc.projected {
				t.Errorf("IsProjectedEvent(%T) = %v; want %v", tc.event, got, tc.projected)
			}
		})
	}
}
