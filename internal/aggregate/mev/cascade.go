package mev

import (
	"sort"
	"strconv"
)

// KindLiquidationCascade is the mev_events.kind for clustered Blend
// liquidation fills correlated with an oracle move.
const KindLiquidationCascade = "liquidation_cascade"

const cascadeNote = "A Blend liquidation-auction fill followed at least one other " +
	"fill against a DIFFERENT position within " + cascadeWindowStr + " ledgers, with an " +
	"on-chain oracle update inside the bracket. Correlation signature: the cluster + " +
	"oracle timing is the evidence; causality (the first liquidation moving the price " +
	"that triggered the next) is not proven."

// cascadeWindowLedgers is the clustering window: fills within this
// many ledgers of each other (≈1 minute at ~5s closes) are one
// cascade cluster.
const (
	cascadeWindowLedgers = 12
	cascadeWindowStr     = "12"
)

// cascadeFillRef is one fill's evidence entry in the detail payload.
type cascadeFillRef struct {
	Pool        string `json:"pool"`
	User        string `json:"user"`
	Filler      string `json:"filler,omitempty"`
	AuctionType int16  `json:"auction_type"` // 0=UserLiquidation, 1=BadDebt
	Ledger      uint32 `json:"ledger"`
	TxHash      string `json:"tx_hash"`
	OpIndex     uint32 `json:"op_index"`
}

// cascadeOracleRef is one correlated oracle update in the detail
// payload.
type cascadeOracleRef struct {
	Source     string `json:"source"`
	ContractID string `json:"contract_id,omitempty"`
	Asset      string `json:"asset"`
	Ledger     uint32 `json:"ledger"`
	TxHash     string `json:"tx_hash"`
}

// cascadeDetail is the mev_events.detail payload for a
// liquidation_cascade candidate.
type cascadeDetail struct {
	WindowLedgers int                `json:"window_ledgers"`
	Fill          cascadeFillRef     `json:"fill"`        // the fill that extended the cascade
	PriorFills    []cascadeFillRef   `json:"prior_fills"` // distinct positions filled just before it
	OracleUpdates []cascadeOracleRef `json:"oracle_updates"`
	Note          string             `json:"note"`
}

// DetectLiquidationCascades scans Blend auction fills for cascade
// clusters: a fill preceded by ≥1 fill against a DIFFERENT position
// (pool, user) within cascadeWindowLedgers, with ≥1 on-chain oracle
// update ledger-positioned inside [earliest prior fill − window,
// fill]. One candidate per cascade-extending fill, anchored on that
// fill's identity so re-scans of overlapping windows dedup cleanly.
func DetectLiquidationCascades(fills []AuctionFill, oracles []OracleRef) []Candidate {
	if len(fills) < 2 {
		return nil
	}
	sorted := make([]AuctionFill, len(fills))
	copy(sorted, fills)
	sort.Slice(sorted, func(a, b int) bool {
		fa, fb := sorted[a], sorted[b]
		if fa.Ledger != fb.Ledger {
			return fa.Ledger < fb.Ledger
		}
		if fa.TxHash != fb.TxHash {
			return fa.TxHash < fb.TxHash
		}
		return fa.OpIndex < fb.OpIndex
	})

	var out []Candidate
	for i := 1; i < len(sorted); i++ {
		c, ok := buildCascadeCandidate(sorted, i, oracles)
		if ok {
			out = append(out, c)
		}
	}
	return out
}

// buildCascadeCandidate evaluates whether sorted[i] extends a cascade.
func buildCascadeCandidate(sorted []AuctionFill, i int, oracles []OracleRef) (Candidate, bool) {
	f := sorted[i]
	var priors []AuctionFill
	for j := i - 1; j >= 0; j-- {
		p := sorted[j]
		if f.Ledger-p.Ledger > cascadeWindowLedgers {
			break
		}
		if p.Pool == f.Pool && p.User == f.User {
			continue // same position (partial fills of one auction) — not a cascade
		}
		if p.TxHash == f.TxHash {
			continue // one atomic tx filling several auctions is a single actor's batch
		}
		priors = append(priors, p)
	}
	if len(priors) == 0 {
		return Candidate{}, false
	}

	lowLedger := priors[len(priors)-1].Ledger // earliest prior in the window
	var correlated []cascadeOracleRef
	for _, o := range oracles {
		if o.Ledger == 0 {
			continue
		}
		if o.Ledger+cascadeWindowLedgers >= lowLedger && o.Ledger <= f.Ledger {
			correlated = append(correlated, cascadeOracleRef{
				Source:     o.Source,
				ContractID: o.ContractID,
				Asset:      o.Asset,
				Ledger:     o.Ledger,
				TxHash:     o.TxHash,
			})
		}
	}
	if len(correlated) == 0 {
		return Candidate{}, false
	}

	return assembleCascadeCandidate(f, priors, correlated), true
}

func assembleCascadeCandidate(f AuctionFill, priors []AuctionFill, correlated []cascadeOracleRef) Candidate {
	const maxEvidence = 10
	if len(priors) > maxEvidence {
		priors = priors[:maxEvidence]
	}
	if len(correlated) > maxEvidence {
		correlated = correlated[:maxEvidence]
	}
	priorRefs := make([]cascadeFillRef, 0, len(priors))
	txSet := map[string]struct{}{f.TxHash: {}}
	txs := []string{f.TxHash}
	acctSet := map[string]struct{}{}
	accts := []string{}
	addAcct := func(a string) {
		if a == "" {
			return
		}
		if _, ok := acctSet[a]; ok {
			return
		}
		acctSet[a] = struct{}{}
		accts = append(accts, a)
	}
	addAcct(f.Filler)
	addAcct(f.User)
	for _, p := range priors {
		priorRefs = append(priorRefs, fillRef(p))
		if _, ok := txSet[p.TxHash]; !ok {
			txSet[p.TxHash] = struct{}{}
			txs = append(txs, p.TxHash)
		}
		addAcct(p.Filler)
		addAcct(p.User)
	}

	primary := f.Filler
	if primary == "" {
		primary = f.User
	}
	return Candidate{
		Kind:             KindLiquidationCascade,
		Ledger:           f.Ledger,
		DetectedAtLedger: f.Ledger,
		Timestamp:        f.Timestamp.UTC(),
		TxHash:           f.TxHash,
		Taker:            primary,
		TxHashes:         txs,
		Accounts:         accts,
		Sources:          []string{"blend"},
		Dedup: KindLiquidationCascade + ":" + f.TxHash + ":" + f.Pool + ":" + f.User +
			":" + strconv.FormatUint(uint64(f.OpIndex), 10),
		Detail: cascadeDetail{
			WindowLedgers: cascadeWindowLedgers,
			Fill:          fillRef(f),
			PriorFills:    priorRefs,
			OracleUpdates: correlated,
			Note:          cascadeNote,
		},
	}
}

func fillRef(f AuctionFill) cascadeFillRef {
	return cascadeFillRef{
		Pool:        f.Pool,
		User:        f.User,
		Filler:      f.Filler,
		AuctionType: f.AuctionType,
		Ledger:      f.Ledger,
		TxHash:      f.TxHash,
		OpIndex:     f.OpIndex,
	}
}
