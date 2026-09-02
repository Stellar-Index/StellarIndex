// Package sourcenet is the single answer to "does this source exist on
// this Stellar network?" (#483).
//
// Every protocol source in the registry is anchored to CONTRACT IDENTITY
// (ADR-0035): the soroswap factory, Blend's child gate, the curated
// phoenix / aquarius / defindex / comet sets, the reflector / redstone /
// band oracle contracts. Those identities are PUBNET addresses. On testnet
// or futurenet the same decoders correctly match nothing — but the
// completeness catalogue, the coverage endpoint and the gap-detector still
// listed all of them with pubnet genesis floors (soroswap 50,746,266 on a
// network whose tip is 4.4M), so /v1/coverage on both test nets read
// "0 of 14 complete" BY CONSTRUCTION and the completeness alerts there were
// noise. The lake itself was complete.
//
// This package is deliberately a static table, not configuration: a
// source becomes applicable on a network when a contract set for that
// network is added to its decoder, which is a code change — the table
// changes in the same commit. Consumers: the reconciliation catalogue
// (compute-completeness), the coverage endpoint, and the per-source
// gap-detector targets.
package sourcenet

import (
	"fmt"
	"sort"
)

// Network names as spelled in config ([stellar] network = …).
const (
	Pubnet    = "pubnet"
	Testnet   = "testnet"
	Futurenet = "futurenet"
)

// allNetworks lists the sources whose substrate is the ledger itself, not
// a contract set — they exist on every network by construction.
var allNetworks = map[string]struct{}{
	"sdex":            {}, // classic order-book: protocol-level, every network
	"sep41_transfers": {}, // SAC / SEP-41 token events: any contract, any network
	"sep41_supply":    {},
	"recognition":     {}, // "no unrecognised event shapes on unowned contracts"
	"ledgers":         {}, // substrate axes
	"transactions":    {},
	"operations":      {},
	"contract_events": {},
	"cap67_movements": {},
	"entry_changes":   {},
	"ledger_entries":  {},
}

// Applicable reports whether source exists on network, and if not, the
// operator-facing reason. Unknown networks are treated like pubnet so a
// misspelling can never silently hide a source (config validation rejects
// unknown network names before this is ever consulted).
func Applicable(source, network string) (bool, string) {
	if network == "" || network == Pubnet {
		return true, ""
	}
	if _, ok := allNetworks[source]; ok {
		return true, ""
	}
	return false, fmt.Sprintf("no contract set registered for network %s — the %s decoder is anchored to pubnet contract identities (ADR-0035)", network, source)
}

// NotApplicable is one entry of the coverage endpoint's exclusion list.
type NotApplicable struct {
	Source string
	Reason string
}

// Filter partitions sources into the applicable set (in the input order)
// and the excluded set (source-sorted, with reasons). It is the one
// routine every consumer goes through, so the split can never disagree
// between the catalogue and the endpoint.
func Filter(sources []string, network string) (applicable []string, excluded []NotApplicable) {
	for _, s := range sources {
		ok, reason := Applicable(s, network)
		if ok {
			applicable = append(applicable, s)
			continue
		}
		excluded = append(excluded, NotApplicable{Source: s, Reason: reason})
	}
	sort.Slice(excluded, func(i, j int) bool { return excluded[i].Source < excluded[j].Source })
	return applicable, excluded
}

// PubnetOnlySources is the canonical list of sources anchored to pubnet
// contract identities — the ones a non-pubnet network must report as not
// applicable. It exists so the coverage endpoint can name them WITHOUT
// depending on the ops catalogue (which lives in internal/ops/chops and
// must not be imported by the API). A test cross-checks it against
// config.KnownSources so a new source cannot be added to one and not the
// other.
var PubnetOnlySources = []string{
	"aquarius", "band", "blend", "blend_backstop", "blend_emitter", "cctp", "comet",
	"defindex", "phoenix", "redstone", "reflector-cex", "reflector-dex", "reflector-fx",
	"rozo", "sorocredit", "soroswap", "soroswap-router",
}

// NotApplicableOn lists, source-sorted with reasons, every canonical
// source that does not exist on network. Empty on pubnet.
func NotApplicableOn(network string) []NotApplicable {
	_, excluded := Filter(PubnetOnlySources, network)
	return excluded
}
