package timescale

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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
				VolumeUSD: "2870000", IsMarketStyled: true,
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
				VolumeUSD: "344000", IsMarketStyled: false,
				TopAccountPairVolShare: 0.98, IssuerSideShare: 1.0,
			},
			want: VolumeCharacterOperational,
		},
		{
			name: "AUDR_operational_corridor",
			in: AssetVolumeCharacter{
				VolumeUSD: "344000", IsMarketStyled: false,
				TopAccountPairVolShare: 0.97, IssuerSideShare: 0.99,
			},
			want: VolumeCharacterOperational,
		},
		{
			// A healthy asset: many distinct makers/takers, no single
			// account pair dominates, on a real price surface.
			name: "normal_multi_account_market",
			in: AssetVolumeCharacter{
				VolumeUSD: "5000000", IsMarketStyled: true,
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
				VolumeUSD: "23000", IsMarketStyled: false,
				TopAccountPairVolShare: 0.97, IssuerSideShare: 0.0,
			},
			want: VolumeCharacterConcentrated,
		},
		{
			// Dust-bot (HELIX/XLM): one account pair, thousands of micro
			// trades on a market surface.
			name: "dust_bot_concentrated",
			in: AssetVolumeCharacter{
				VolumeUSD: "5500", IsMarketStyled: true,
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
				VolumeUSD: "100000", IsMarketStyled: true,
				TopAccountPairVolShare: 0.95, IssuerSideShare: 0.95,
			},
			want: VolumeCharacterConcentrated,
		},
		{
			// Below the volume floor: never editorialize a quiet asset,
			// even if its handful of trades is one account pair.
			name: "below_floor_defaults_market",
			in: AssetVolumeCharacter{
				VolumeUSD: "500", IsMarketStyled: true,
				TopAccountPairVolShare: 1.0, IssuerSideShare: 1.0,
			},
			want: VolumeCharacterMarket,
		},
		{
			// The floor compares the exact decimal: a fraction of a cent
			// under $1k is still below it.
			name: "just_below_floor_exact_defaults_market",
			in: AssetVolumeCharacter{
				VolumeUSD: "999.999999", IsMarketStyled: true,
				TopAccountPairVolShare: 1.0, IssuerSideShare: 1.0,
			},
			want: VolumeCharacterMarket,
		},
		{
			name: "at_floor_exact_is_classified",
			in: AssetVolumeCharacter{
				VolumeUSD: "1000.00", IsMarketStyled: true,
				TopAccountPairVolShare: 1.0, IssuerSideShare: 1.0,
			},
			want: VolumeCharacterConcentrated,
		},
		{
			// Unreadable volume is no signal: never badge on it.
			name: "unparseable_volume_defaults_market",
			in: AssetVolumeCharacter{
				VolumeUSD: "NaN", IsMarketStyled: true,
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
		"GROUP BY LEAST(COALESCE(maker, taker), taker), GREATEST(COALESCE(maker, taker), taker)",
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

// TestVolumeCharacterSQL_PoolMakerIsNotAnAccount: a classic liquidity-pool
// fill stores the hex pool id in trades.maker (sdex decode). Both the
// per-asset query and the rollup must project maker through an account
// discriminator that keeps a G-strkey and drops the pool id, and key a pool
// fill on its lone taker rather than on the raw (pool, taker) pair, or the
// pool counts as a distinct market maker and a round-trip through pools
// splits into per-pool halves.
func TestVolumeCharacterSQL_PoolMakerIsNotAnAccount(t *testing.T) {
	var raw [32]byte
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	account, err := strkey.Encode(strkey.VersionByteAccountID, raw[:])
	if err != nil || !canonical.IsAccountID(account) {
		t.Fatalf("fixture account %q invalid: %v", account, err)
	}
	contract, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	if err != nil {
		t.Fatalf("fixture contract: %v", err)
	}
	// The exact form the sdex decoder writes for a pool fill's Maker.
	poolMaker := fmt.Sprintf("%x", xdr.PoolId(raw))

	accountPattern := regexp.MustCompile(`maker ~ '([^']+)' THEN maker END AS maker`)
	for name, q := range map[string]string{
		"assetVolumeCharacterSQL":               assetVolumeCharacterSQL,
		"assetVolumeCharacterRollupSQLTemplate": assetVolumeCharacterRollupSQLTemplate,
	} {
		m := accountPattern.FindStringSubmatch(q)
		if m == nil {
			t.Errorf("%s: maker is not projected through an account discriminator", name)
			continue
		}
		isAccount := regexp.MustCompile(m[1])
		if !isAccount.MatchString(account) {
			t.Errorf("%s: account pattern %q rejects the account maker %s", name, m[1], account)
		}
		for _, notAccount := range []string{poolMaker, contract, ""} {
			if isAccount.MatchString(notAccount) {
				t.Errorf("%s: account pattern %q accepts non-account maker %q", name, m[1], notAccount)
			}
		}
		if !strings.Contains(q, "(maker IS NOT NULL AND maker !~ '"+m[1]+"') AS pool_fill") {
			t.Errorf("%s: pool fills are not flagged with the same pattern", name)
		}
		if !strings.Contains(q, "LEAST(COALESCE(maker, taker), taker), GREATEST(COALESCE(maker, taker), taker)") {
			t.Errorf("%s: a pool fill is not keyed on its lone taker", name)
		}
		if strings.Contains(q, "LEAST(maker, taker)") {
			t.Errorf("%s: still pairs the raw maker, so a pool reads as an account", name)
		}
	}
}
