package mev

import (
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
)

// OracleRef is one on-chain oracle_updates row, as the ordering-aware
// detectors consume it. Asset/Quote are the canonical asset strings
// the served tier stores (e.g. "CAS3J7…" SAC ids, "fiat:USD").
//
// Canonical definition lives in [domain.MEVOracleRef] (D8 M0-1:
// internal/storage/timescale reads/writes this shape and must not
// import upward into this package to do so); this is a transparent
// alias so every existing caller of mev.OracleRef is unaffected.
type OracleRef = domain.MEVOracleRef

// AuctionFill is one blend_auctions fill row (event_kind='fill',
// liquidation-relevant auction types) for the cascade detector.
//
// Canonical definition lives in [domain.MEVAuctionFill] — see the
// [OracleRef] doc for why this is an alias.
type AuctionFill = domain.MEVAuctionFill

// xlmSAC is the native-XLM Stellar Asset Contract id — the same asset
// as "native" under a different identity (Soroban venues emit the SAC
// id, SDEX emits "native").
const xlmSAC = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"

// normAsset collapses the native-XLM SAC onto "native" so pair
// grouping and oracle-asset matching see one XLM identity.
func normAsset(a string) string {
	if a == xlmSAC {
		return "native"
	}
	return a
}

// unorderedPairKey is an orientation-independent pair identity: the
// same pool pair can appear as A/B or B/A across trades, so grouping
// keys on the sorted asset strings.
func unorderedPairKey(t canonical.Trade) string {
	b := normAsset(t.Pair.Base.String())
	q := normAsset(t.Pair.Quote.String())
	if b > q {
		b, q = q, b
	}
	return b + "|" + q
}

// tradeTouches reports whether the (normalised) asset is either side
// of the trade's pair.
func tradeTouches(t canonical.Trade, asset string) bool {
	a := normAsset(asset)
	return normAsset(t.Pair.Base.String()) == a || normAsset(t.Pair.Quote.String()) == a
}

// orderableTrade reports whether a trade can participate in a
// tx_index-ordered pattern at all (on-chain, with an actor).
func orderableTrade(t canonical.Trade) bool {
	return t.Ledger > 0 && t.TxHash != "" && t.Taker != ""
}

// OrderingTxHashes returns the distinct tx hashes the ordering-aware
// detectors (sandwich + oracle_sandwich) would need tx_index for,
// prefiltered so the lake lookup stays bounded:
//
//   - trades in a (ledger, pair) group with ≥3 trades, ≥2 distinct
//     takers, and some taker holding ≥2 distinct txs (the minimum
//     sandwich shape), and
//   - trades in a ledger that also carries an on-chain oracle update,
//     where the trade touches the oracle's asset, plus the oracle
//     update txs themselves.
func OrderingTxHashes(trades []canonical.Trade, oracles []OracleRef) []string {
	need := map[string]struct{}{}
	collectSandwichHashes(trades, need)
	collectOracleHashes(trades, oracles, need)
	out := make([]string, 0, len(need))
	for h := range need {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func collectSandwichHashes(trades []canonical.Trade, need map[string]struct{}) {
	type groupStat struct {
		idxs   []int
		takers map[string]map[string]struct{} // taker → distinct tx set
	}
	groups := map[string]*groupStat{}
	for i, t := range trades {
		if !orderableTrade(t) {
			continue
		}
		key := ledgerKey(t.Ledger) + unorderedPairKey(t)
		g, ok := groups[key]
		if !ok {
			g = &groupStat{takers: map[string]map[string]struct{}{}}
			groups[key] = g
		}
		g.idxs = append(g.idxs, i)
		txs := g.takers[t.Taker]
		if txs == nil {
			txs = map[string]struct{}{}
			g.takers[t.Taker] = txs
		}
		txs[t.TxHash] = struct{}{}
	}
	for _, g := range groups {
		if len(g.idxs) < 3 || len(g.takers) < 2 {
			continue
		}
		multiTx := false
		for _, txs := range g.takers {
			if len(txs) >= 2 {
				multiTx = true
				break
			}
		}
		if !multiTx {
			continue
		}
		for _, i := range g.idxs {
			need[trades[i].TxHash] = struct{}{}
		}
	}
}

func collectOracleHashes(trades []canonical.Trade, oracles []OracleRef, need map[string]struct{}) {
	byLedger := map[uint32][]OracleRef{}
	for _, o := range oracles {
		if o.Ledger == 0 || o.TxHash == "" {
			continue
		}
		byLedger[o.Ledger] = append(byLedger[o.Ledger], o)
	}
	if len(byLedger) == 0 {
		return
	}
	for _, t := range trades {
		if !orderableTrade(t) {
			continue
		}
		for _, o := range byLedger[t.Ledger] {
			if tradeTouches(t, o.Asset) {
				need[t.TxHash] = struct{}{}
				need[o.TxHash] = struct{}{}
			}
		}
	}
}

// ledgerKey renders a ledger seq as a fixed-width group-key prefix.
func ledgerKey(l uint32) string {
	// 10 digits covers uint32; fixed width keeps keys collision-free
	// against the pair suffix.
	buf := [11]byte{}
	for i := 9; i >= 0; i-- {
		buf[i] = byte('0' + l%10)
		l /= 10
	}
	buf[10] = ':'
	return string(buf[:])
}
