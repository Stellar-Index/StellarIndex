package redstone

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// navFundamentalInFiat is the explicit, evidenced attestation list for
// the ONE case where a bare RedStone `_FUNDAMENTAL` feed may carry a
// fiat quote: the token's reserve asset genuinely IS that fiat, so the
// NAV really is a dollar (or euro) figure rather than a ratio.
//
// It exists because the `_FUNDAMENTAL` suffix alone does not say what
// the NAV is denominated in — that is a per-token fact about the
// reserve, and getting it wrong is not a rounding error but an
// order-of-magnitude lie on the wire (D8: `SolvBTC_FUNDAMENTAL`
// published 1.00295305 and was served as `quote=fiat:USD`, i.e. "a
// BTC-backed token is worth $1.00" while its own
// `SolvBTC_FUNDAMENTAL/USD` sibling said $78,313.03 — live r1
// `/v1/oracle/streams?include_unmapped=true`, 2026-08-29).
//
// Adding a feed here is therefore an assertion about the world and
// needs the evidence recorded alongside it: the feed's published value
// next to the reserve asset's own price. Anything not attested must
// name its reserve asset in [feedEntry.Quote] instead. Keep the
// wording aligned with the same feed's line in feeds.go.
var navFundamentalInFiat = map[string]string{
	"BENJI_ETHEREUM_FUNDAMENTAL": "Franklin Templeton FOBXX money-market fund token; " +
		"reserve is USD cash + T-bills and the share NAV is a dollar figure " +
		"pinned at $1.00 (lake fixture ledger 60104689: 1.00000000). No `/USD` sibling feed.",
	"iBENJI_ETHEREUM_FUNDAMENTAL": "Accruing share class of BENJI; same USD reserve, " +
		"same dollar NAV (lake fixture ledger 60104689: 1.00000000). No `/USD` sibling feed.",
	"USST_FUNDAMENTAL": "STBL treasury-backed stablecoin; reserve is US Treasuries, " +
		"NAV is the dollar value per token (live 1.0096, 2026-07-27). No `/USD` sibling feed.",
	"savUSD_FUNDAMENTAL": "Avant staked avUSD vault share; reserve is avUSD, a " +
		"USD-denominated stablecoin, so the vault exchange rate is a dollar figure " +
		"(live 1.1877, 2026-07-27 — plausible only as USD). No `/USD` sibling feed.",
}

// TestFeedRegistry_NAVFeedsQuoteTheirReserveAsset is the class guard
// for D8. A bare `_FUNDAMENTAL` feed publishes net asset value in the
// token's RESERVE asset; registering one against `fiat:USD` when the
// reserve is a crypto asset mislabels a ratio (~1.00) as a dollar
// price and serves it on `/v1/oracle/streams` with `mapped=true`.
//
// The rule, table-driven over the WHOLE live registry so a feed added
// tomorrow is covered without touching this test:
//
//	feed_id contains `_FUNDAMENTAL`
//	  AND carries no explicit `/<QUOTE>` suffix (which would declare
//	      its own denomination on the wire)
//	  AND is registered with a fiat quote
//	⇒ it MUST appear in navFundamentalInFiat with its evidence.
//
// A new NAV feed whose reserve is not fiat therefore fails CI until
// its quote names the reserve asset; a new NAV feed whose reserve
// really is fiat fails CI until someone records why.
func TestFeedRegistry_NAVFeedsQuoteTheirReserveAsset(t *testing.T) {
	for feedID, entry := range feedRegistry {
		if !strings.Contains(feedID, "_FUNDAMENTAL") {
			continue
		}
		// `X_FUNDAMENTAL/USD` names its own quote on the wire — the
		// ADR-0028 `<BASE>/<QUOTE>` convention — so it is not a bare
		// NAV ratio and is out of scope for this rule.
		if strings.ContainsRune(feedID, '/') {
			continue
		}
		if entry.Quote.Type != canonical.AssetFiat {
			continue // quoted in a reserve asset — the corrected shape
		}
		if _, attested := navFundamentalInFiat[feedID]; !attested {
			t.Errorf("feed %q publishes a NAV and is registered against fiat quote %s "+
				"without an entry in navFundamentalInFiat: either its NAV is a RATIO in a "+
				"non-fiat reserve (quote it with that asset, as SolvBTC_FUNDAMENTAL is quoted "+
				"in crypto:BTC) or its reserve really is that fiat (record the evidence). "+
				"See D8 — mislabelling a ~1.0 ratio as a USD price serves a $1.00 claim for a "+
				"token worth ~$78,313.",
				feedID, entry.Quote.String())
		}
	}

	// Two-way lockstep: an attestation that no longer describes a
	// fiat-quoted bare NAV feed is stale and must be deleted, so the
	// list can never silently pre-authorise a future mislabel.
	for feedID := range navFundamentalInFiat {
		entry, ok := feedRegistry[feedID]
		if !ok {
			t.Errorf("navFundamentalInFiat attests feed %q, which is not in feedRegistry — stale entry", feedID)
			continue
		}
		if entry.Quote.Type != canonical.AssetFiat {
			t.Errorf("navFundamentalInFiat attests feed %q, but it is quoted in %s (not fiat) — stale entry",
				feedID, entry.Quote.String())
		}
	}
}

// TestFeedRegistry_SuffixedFeedNeverSharesQuoteWithItsBareSibling is
// the name-shape-independent half of the same guard. When RedStone
// publishes BOTH `X` and `X/<FIAT>`, the suffixed id declares the
// denomination explicitly; the bare id therefore publishes a
// DIFFERENT quantity and cannot legitimately carry the same quote.
// D8 is exactly this shape: `SolvBTC_FUNDAMENTAL` and
// `SolvBTC_FUNDAMENTAL/USD` were both registered `fiat:USD`.
func TestFeedRegistry_SuffixedFeedNeverSharesQuoteWithItsBareSibling(t *testing.T) {
	for feedID, entry := range feedRegistry {
		i := strings.LastIndexByte(feedID, '/')
		if i < 0 {
			continue
		}
		bareEntry, ok := feedRegistry[feedID[:i]]
		if !ok {
			continue // no bare sibling registered
		}
		if bareEntry.Quote.Equal(entry.Quote) {
			t.Errorf("feeds %q and %q are both quoted %s: the suffixed id already declares that "+
				"denomination, so the bare id publishes a different quantity and must be quoted "+
				"in the asset its value is denominated in (D8)",
				feedID[:i], feedID, entry.Quote.String())
		}
	}
}

// TestDecode_SolvBTCFamily_NAVRatiosQuotedInReserveAsset pins the
// corrected values end-to-end through the decoder, using the four
// SolvBTC-family feeds exactly as observed live on r1 via
// `/v1/oracle/streams?include_unmapped=true` on 2026-08-29:
//
//	crypto:SolvBTC.BBN_FUNDAMENTAL       1.00000000
//	crypto:SolvBTC.BBN_FUNDAMENTAL_USD  78313.02974310
//	crypto:SolvBTC_FUNDAMENTAL           1.00295305
//	crypto:SolvBTC_FUNDAMENTAL_USD      78313.02974310
//
// The two `_USD` legs are byte-identical (also true of the 2026-07-27
// capture, 6543063913439 both), which is what fixes each ratio's
// denominator:
//
//   - SolvBTC NAV in USD / BTC in USD = 1.00295 ⇒ the bare
//     `SolvBTC_FUNDAMENTAL` ratio is denominated in BTC.
//   - SolvBTC.BBN NAV in USD equals SolvBTC NAV in USD while the bare
//     ratio is exactly 1.00000000 (three independent captures:
//     lake ledger 60104689, 2026-07-27, 2026-08-29) ⇒ SolvBTC.BBN is
//     1:1 with SolvBTC and its ratio is denominated in SolvBTC, not
//     BTC. Quoting it crypto:BTC would contradict our own
//     `SolvBTC.BBN_FUNDAMENTAL_USD` row.
func TestDecode_SolvBTCFamily_NAVRatiosQuotedInReserveAsset(t *testing.T) {
	feedIDs := []string{
		"SolvBTC.BBN_FUNDAMENTAL",
		"SolvBTC.BBN_FUNDAMENTAL/USD",
		"SolvBTC_FUNDAMENTAL",
		"SolvBTC_FUNDAMENTAL/USD",
	}
	prices := []*big.Int{
		big.NewInt(1_00000000),     // 1.00000000 SolvBTC per SolvBTC.BBN
		big.NewInt(78313_02974310), // $78,313.02974310
		big.NewInt(1_00295305),     // 1.00295305 BTC per SolvBTC
		big.NewInt(78313_02974310), // $78,313.02974310
	}
	body := encodeWritePricesBody(t, relayerG, prices, 1_756_400_000_000, 1_756_400_060_000)
	ev := &events.Event{
		Topic: []string{TopicSymbolRedstone},
		Value: body,
		OpArgs: []string{
			encodeAddressArg(t, relayerG),
			encodeStringVecArg(t, feedIDs),
			encodePayloadArg(t),
		},
		TxHash: "abcd",
	}
	updates, err := decodeWritePrices(ev, time.Now())
	if err != nil {
		t.Fatalf("decodeWritePrices: %v", err)
	}
	if len(updates) != len(feedIDs) {
		t.Fatalf("got %d updates, want %d", len(updates), len(feedIDs))
	}

	want := []struct{ asset, quote string }{
		{"crypto:SolvBTC.BBN_FUNDAMENTAL", "crypto:SolvBTC"},
		{"crypto:SolvBTC.BBN_FUNDAMENTAL_USD", "fiat:USD"},
		{"crypto:SolvBTC_FUNDAMENTAL", "crypto:BTC"},
		{"crypto:SolvBTC_FUNDAMENTAL_USD", "fiat:USD"},
	}
	for i, w := range want {
		if got := updates[i].Asset.String(); got != w.asset {
			t.Errorf("feed %q → asset %s, want %s", feedIDs[i], got, w.asset)
		}
		if got := updates[i].Quote.String(); got != w.quote {
			t.Errorf("feed %q → quote %s, want %s (D8: a NAV ratio must name the asset it is "+
				"denominated in, never fiat:USD)", feedIDs[i], got, w.quote)
		}
		// The label is the only thing D8 got wrong — the observed
		// value must pass through byte-identical.
		if updates[i].Price.BigInt().Cmp(prices[i]) != 0 {
			t.Errorf("feed %q → price %s, want %s unchanged", feedIDs[i], updates[i].Price, prices[i])
		}
	}
}
