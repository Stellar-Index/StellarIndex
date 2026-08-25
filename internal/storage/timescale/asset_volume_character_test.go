package timescale

import (
	"strings"
	"testing"
)

// TestDeriveVolumeCharacter_Census pins the §2 classifier against the
// design's taxonomy table (the census is the oracle). The required cases
// from the build directive — scam AUD → concentrated, AUDD/AUDR →
// operational, a normal multi-account asset → market — plus the other two
// taxonomy species and the floor guard.
func TestDeriveVolumeCharacter_Census(t *testing.T) {
	cases := []struct {
		name string
		in   AssetVolumeCharacter
		want string
	}{
		{
			// The reported scam AUD: ~$205k/day, 108/109 of its 14d
			// XLM/AUD trades are one wallet pair, taker == issuer.
			// Market-styled (native XLM) + single account pair → wash.
			name: "scam_AUD_volume_painting_wash",
			in: AssetVolumeCharacter{
				VolumeUSD: 2_870_000, IsMarketStyled: true,
				TopAccountPairVolShare: 0.99, IssuerSideShare: 0.99,
				DistinctMakers: 2, DistinctTakers: 1,
			},
			want: VolumeCharacterConcentrated,
		},
		{
			// AUDD wrap/redeem corridor: issuer-side, non-market (wrap)
			// pair against its sibling AUDR.
			name: "AUDD_operational_corridor",
			in: AssetVolumeCharacter{
				VolumeUSD: 344_000, IsMarketStyled: false,
				TopAccountPairVolShare: 0.98, IssuerSideShare: 1.0,
			},
			want: VolumeCharacterOperational,
		},
		{
			name: "AUDR_operational_corridor",
			in: AssetVolumeCharacter{
				VolumeUSD: 344_000, IsMarketStyled: false,
				TopAccountPairVolShare: 0.97, IssuerSideShare: 0.99,
			},
			want: VolumeCharacterOperational,
		},
		{
			// A healthy asset: many distinct makers/takers, no single
			// account pair dominates, on a real price surface.
			name: "normal_multi_account_market",
			in: AssetVolumeCharacter{
				VolumeUSD: 5_000_000, IsMarketStyled: true,
				TopAccountPairVolShare: 0.15, IssuerSideShare: 0.05,
				DistinctMakers: 800, DistinctTakers: 750,
			},
			want: VolumeCharacterMarket,
		},
		{
			// Third-party ping-pong (XAUa↔USDV): two NON-issuer wallets
			// round-tripping a non-market pair. One account pair owns the
			// volume but the issuer isn't the counterparty → fabricated.
			name: "third_party_ping_pong_concentrated",
			in: AssetVolumeCharacter{
				VolumeUSD: 23_000, IsMarketStyled: false,
				TopAccountPairVolShare: 0.97, IssuerSideShare: 0.0,
			},
			want: VolumeCharacterConcentrated,
		},
		{
			// Dust-bot (HELIX/XLM): one account pair, thousands of micro
			// trades on a market surface.
			name: "dust_bot_concentrated",
			in: AssetVolumeCharacter{
				VolumeUSD: 5_500, IsMarketStyled: true,
				TopAccountPairVolShare: 1.0, IssuerSideShare: 0.0,
			},
			want: VolumeCharacterConcentrated,
		},
		{
			// An issuer redeeming on a MARKET surface with a single
			// counterparty is still wash per the design discriminator
			// (issuer-side alone isn't the excuse — market-styled + single
			// pair is the tell).
			name: "issuer_side_but_market_styled_is_wash",
			in: AssetVolumeCharacter{
				VolumeUSD: 100_000, IsMarketStyled: true,
				TopAccountPairVolShare: 0.95, IssuerSideShare: 0.95,
			},
			want: VolumeCharacterConcentrated,
		},
		{
			// Below the volume floor: never editorialize a quiet asset,
			// even if its handful of trades is one account pair.
			name: "below_floor_defaults_market",
			in: AssetVolumeCharacter{
				VolumeUSD: 500, IsMarketStyled: true,
				TopAccountPairVolShare: 1.0, IssuerSideShare: 1.0,
			},
			want: VolumeCharacterMarket,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deriveVolumeCharacter(c.in); got != c.want {
				t.Errorf("deriveVolumeCharacter(%+v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestAssetVolumeCharacterSQL_Shape pins the rollup query to the signal
// definitions in design §2 so a future edit can't silently drop one.
func TestAssetVolumeCharacterSQL_Shape(t *testing.T) {
	q := assetVolumeCharacterSQL
	musts := []string{
		// Priced volume only — an unpriced trade can't move a share.
		"usd_volume IS NOT NULL",
		// Trailing window bound + interval param.
		"ts >= now() - $2::interval",
		// Alias-complete, matched on BOTH sides.
		"base_asset = ANY($1) OR quote_asset = ANY($1)",
		// UNORDERED account pair so a round-trip folds to one pair.
		"GROUP BY LEAST(maker, taker), GREATEST(maker, taker)",
		// Self-cross share.
		"maker IS NOT NULL AND maker = taker",
		// Issuer-side predicate, no-op when the issuer param is empty.
		"$3 <> '' AND (maker = $3 OR taker = $3)",
		// Market-surface (real price surface) detection.
		"counterpart = 'native'",
		"counterpart LIKE 'fiat:%'",
		"counterpart LIKE 'USDC-%'",
	}
	for _, m := range musts {
		if !strings.Contains(q, m) {
			t.Errorf("assetVolumeCharacterSQL missing %q:\n%s", m, q)
		}
	}
}

// TestVolumeCharacterWindow is 14 days — the forensic window that proved
// the reported wash.
func TestVolumeCharacterWindow(t *testing.T) {
	if d := int(volumeCharacterWindow.Hours()) / 24; d != 14 {
		t.Errorf("volumeCharacterWindow = %d days, want 14", d)
	}
}
