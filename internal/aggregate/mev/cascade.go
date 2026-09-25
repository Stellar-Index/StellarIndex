package mev

import (
	"sort"
	"strconv"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// KindLiquidationCascade is the mev_events.kind for clustered Blend
// liquidation fills correlated with an oracle move.
const KindLiquidationCascade = "liquidation_cascade"

const cascadeNote = "A Blend liquidation-auction fill followed at least one other " +
	"fill against a DIFFERENT position within " + cascadeWindowStr + " ledgers, with an " +
	"on-chain oracle update for one of the filled position's own debt or collateral " +
	"assets inside the bracket. Correlation signature: the cluster + oracle timing is " +
	"the evidence; causality (the first liquidation moving the price that triggered " +
	"the next) is not proven. accounts lists the fillers (liquidators) only; the " +
	"positions' owners appear in the detail as `liquidated` and are the subjects of " +
	"the liquidations, not actors in the pattern."

// cascadeWindowLedgers is the clustering window: fills within this
// many ledgers of each other (≈1 minute at ~5s closes) are one
// cascade cluster.
const (
	cascadeWindowLedgers = 12
	cascadeWindowStr     = "12"
)

// cascadeFillRef is one fill's evidence entry in the detail payload.
// Liquidated is the owner of the position being auctioned — the subject
// of the liquidation, named under that role so it is never read as an
// actor.
type cascadeFillRef struct {
	Pool        string   `json:"pool"`
	Liquidated  string   `json:"liquidated"`
	Filler      string   `json:"filler,omitempty"`
	AuctionType int16    `json:"auction_type"` // 0=UserLiquidation, 1=BadDebt
	Assets      []string `json:"assets"`       // the position's reserves in this fill's bid + lot
	Ledger      uint32   `json:"ledger"`
	TxHash      string   `json:"tx_hash"`
	OpIndex     uint32   `json:"op_index"`
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
	correlated := correlatedOracles(f, lowLedger, oracles)
	if len(correlated) == 0 {
		return Candidate{}, false
	}

	return assembleCascadeCandidate(f, priors, correlated), true
}

// correlatedOracles returns the oracle updates inside [lowLedger − window,
// f.Ledger] that price one of f's own position assets. Without the asset
// key, any mapped update from a many-feed oracle publishing on a short
// cadence lands in almost every bracket and the leg carries no evidence.
// A fill with no recorded assets correlates with nothing (fail closed).
func correlatedOracles(f AuctionFill, lowLedger uint32, oracles []OracleRef) []cascadeOracleRef {
	position := positionAssetSet(f.Assets)
	var out []cascadeOracleRef
	for _, o := range oracles {
		if o.Ledger == 0 || o.Ledger+cascadeWindowLedgers < lowLedger || o.Ledger > f.Ledger {
			continue
		}
		if !oracleRefIsMapped(o) {
			continue // raw: row — record-layer only, never cascade evidence
		}
		if _, ok := position[normAsset(o.Asset)]; !ok {
			continue
		}
		out = append(out, cascadeOracleRef{
			Source:     o.Source,
			ContractID: o.ContractID,
			Asset:      o.Asset,
			Ledger:     o.Ledger,
			TxHash:     o.TxHash,
		})
	}
	return out
}

// positionAssetSet is the position's assets closed over their alias
// forms (normalised), so an oracle keyed on one identity of an asset
// (a SAC id, crypto:XLM) matches a position recorded under another.
func positionAssetSet(assets []string) map[string]struct{} {
	set := make(map[string]struct{}, len(assets))
	for _, s := range assets {
		set[normAsset(s)] = struct{}{}
		a, err := canonical.ParseAsset(s)
		if err != nil {
			continue
		}
		for _, alias := range canonical.AssetAliasStrings(a) {
			set[normAsset(alias)] = struct{}{}
		}
	}
	return set
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
	// Only fillers are actors in the pattern. The liquidated owners stay
	// in the detail under their own role, never in the published
	// accounts list, and never as the Taker fallback.
	addAcct(f.Filler)
	for _, p := range priors {
		priorRefs = append(priorRefs, fillRef(p))
		if _, ok := txSet[p.TxHash]; !ok {
			txSet[p.TxHash] = struct{}{}
			txs = append(txs, p.TxHash)
		}
		addAcct(p.Filler)
	}

	return Candidate{
		Kind:             KindLiquidationCascade,
		Ledger:           f.Ledger,
		DetectedAtLedger: f.Ledger,
		Timestamp:        f.Timestamp.UTC(),
		TxHash:           f.TxHash,
		Taker:            f.Filler,
		TxHashes:         txs,
		Accounts:         accts,
		Assets:           sortedKeys(positionAssets(f.Assets)),
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

// oracleRefIsMapped reports whether the oracle row's asset is a MAPPED
// canonical asset. The `raw:` rows the oracle capture-totality design
// records verbatim for unmapped symbols are orientation-unknown
// reference data, never interpretation input, so they never become
// cascade evidence. OracleUpdatesForMEVScan already excludes them in SQL; this is
// the in-process guard for any other OracleScanner. A string that is
// not a canonical asset at all is treated as unmapped (fail closed).
func oracleRefIsMapped(o OracleRef) bool {
	a, err := canonical.ParseAsset(o.Asset)
	return err == nil && a.IsMapped()
}

// positionAssets is the fill's asset list as a set.
func positionAssets(assets []string) map[string]struct{} {
	set := make(map[string]struct{}, len(assets))
	for _, a := range assets {
		set[a] = struct{}{}
	}
	return set
}

func fillRef(f AuctionFill) cascadeFillRef {
	return cascadeFillRef{
		Pool:        f.Pool,
		Liquidated:  f.User,
		Filler:      f.Filler,
		AuctionType: f.AuctionType,
		Assets:      sortedKeys(positionAssets(f.Assets)),
		Ledger:      f.Ledger,
		TxHash:      f.TxHash,
		OpIndex:     f.OpIndex,
	}
}
