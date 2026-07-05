package mev

import (
	"strconv"

	"github.com/StellarIndex/stellar-index/internal/canonical"
)

// KindOracleSandwich is the mev_events.kind for trades bracketing an
// on-chain oracle update.
const KindOracleSandwich = "oracle_sandwich"

const oracleSandwichNote = "One account traded an asset in transactions on BOTH sides " +
	"(tx_index application order from the raw lake) of an on-chain oracle update " +
	"for that asset, all within a single ledger. Positional signature only: the " +
	"served rows don't carry trade direction, so the trade/update relationship is " +
	"not proven profitable — treat as a candidate, not proof."

// oracleSandwichDetail is the mev_events.detail payload for an
// oracle_sandwich candidate.
type oracleSandwichDetail struct {
	OracleSource   string       `json:"oracle_source"`
	OracleContract string       `json:"oracle_contract,omitempty"`
	OracleTxHash   string       `json:"oracle_tx_hash"`
	OracleTxIndex  uint32       `json:"oracle_tx_index"`
	Asset          string       `json:"asset"`
	Quote          string       `json:"quote,omitempty"`
	Account        string       `json:"account"`
	Legs           []OrderedLeg `json:"legs"` // nearest before + after trades
	NotionalUSD    string       `json:"notional_usd,omitempty"`
	Note           string       `json:"note"`
}

// DetectOracleSandwiches scans for trades bracketing an on-chain
// oracle update: within one ledger, account A trades a pair touching
// the oracle's asset in transactions strictly before AND strictly
// after the update's transaction (by lake tx_index). One candidate
// per (oracle update, account).
//
// txIdx must cover both the trades' and the oracle updates' tx
// hashes; unresolved hashes degrade detection silently.
func DetectOracleSandwiches(trades []canonical.Trade, usdVolume []string, oracles []OracleRef, txIdx map[string]uint32) []Candidate {
	if len(txIdx) == 0 || len(oracles) == 0 {
		return nil
	}
	tradesByLedger := map[uint32][]int{}
	for i, t := range trades {
		if !orderableTrade(t) {
			continue
		}
		if _, ok := txIdx[t.TxHash]; !ok {
			continue
		}
		tradesByLedger[t.Ledger] = append(tradesByLedger[t.Ledger], i)
	}

	var out []Candidate
	for _, o := range oracles {
		if o.Ledger == 0 || o.TxHash == "" {
			continue
		}
		oIdx, ok := txIdx[o.TxHash]
		if !ok {
			continue
		}
		out = append(out, oracleSandwichesForUpdate(trades, usdVolume, txIdx, o, oIdx, tradesByLedger[o.Ledger])...)
	}
	return out
}

// oracleSandwichesForUpdate evaluates one oracle update against its
// ledger's trades, emitting one candidate per bracketing account.
func oracleSandwichesForUpdate(trades []canonical.Trade, usdVolume []string, txIdx map[string]uint32,
	o OracleRef, oIdx uint32, ledgerTrades []int,
) []Candidate {
	byTaker := map[string][]int{}
	takerOrder := []string{}
	for _, i := range ledgerTrades {
		t := trades[i]
		if t.TxHash == o.TxHash || !tradeTouches(t, o.Asset) {
			continue
		}
		if _, seen := byTaker[t.Taker]; !seen {
			takerOrder = append(takerOrder, t.Taker)
		}
		byTaker[t.Taker] = append(byTaker[t.Taker], i)
	}

	var out []Candidate
	for _, taker := range takerOrder {
		c, ok := buildOracleSandwichCandidate(trades, usdVolume, txIdx, o, oIdx, taker, byTaker[taker])
		if ok {
			out = append(out, c)
		}
	}
	return out
}

// buildOracleSandwichCandidate finds the taker's nearest trades on
// each side of the oracle update and assembles the Candidate.
func buildOracleSandwichCandidate(trades []canonical.Trade, usdVolume []string, txIdx map[string]uint32,
	o OracleRef, oIdx uint32, taker string, idxs []int,
) (Candidate, bool) {
	before, after := -1, -1
	for _, i := range idxs {
		ti := txIdx[trades[i].TxHash]
		switch {
		case ti < oIdx:
			if before == -1 || ti > txIdx[trades[before].TxHash] {
				before = i // nearest before
			}
		case ti > oIdx:
			if after == -1 || ti < txIdx[trades[after].TxHash] {
				after = i // nearest after
			}
		}
	}
	if before == -1 || after == -1 {
		return Candidate{}, false
	}

	involved := []int{before, after}
	legs := []OrderedLeg{
		orderedLegFrom(trades[before], txIdx, "before"),
		orderedLegFrom(trades[after], txIdx, "after"),
	}
	notional := sumUSD(usdVolume, involved)
	tb := trades[before]
	c := Candidate{
		Kind:             KindOracleSandwich,
		Ledger:           o.Ledger,
		DetectedAtLedger: o.Ledger,
		Timestamp:        tb.Timestamp.UTC(),
		TxHash:           o.TxHash,
		Taker:            taker,
		TxHashes:         []string{tb.TxHash, o.TxHash, trades[after].TxHash},
		Accounts:         []string{taker},
		Assets:           []string{normAsset(o.Asset)},
		Sources:          distinctSources(trades, involved),
		NotionalUSD:      notional,
		Dedup: KindOracleSandwich + ":" + o.TxHash + ":" + strconv.FormatUint(uint64(o.OpIndex), 10) +
			":" + taker + ":" + normAsset(o.Asset),
		Detail: oracleSandwichDetail{
			OracleSource:   o.Source,
			OracleContract: o.ContractID,
			OracleTxHash:   o.TxHash,
			OracleTxIndex:  oIdx,
			Asset:          o.Asset,
			Quote:          o.Quote,
			Account:        taker,
			Legs:           legs,
			NotionalUSD:    notional,
			Note:           oracleSandwichNote,
		},
	}
	return c, true
}
