// Package sourcenet answers "does this source exist on this Stellar network?"
//
// Every protocol source is anchored to CONTRACT IDENTITY (ADR-0035), and those identities are
// PUBNET addresses. On testnet or futurenet the decoders correctly match nothing, so a consumer
// listing them anyway would carry pubnet genesis floors and report incomplete by construction.
//
// The table is static: it changes in the same commit that adds a contract set to a decoder.
//
// A consumer whose names are not config.KnownSources (the gap detector keys per TABLE) maps each
// onto a canonical name and guards that mapping with [Known], because [Applicable] answers false
// for anything it does not classify, which silently REMOVES a signal.
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
	"soroban_events":  {}, // the raw Soroban landing zone: every contract event, any network
	"ledgers":         {}, // substrate axes
	"transactions":    {},
	"operations":      {},
	"contract_events": {},
	"cap67_movements": {},
	"entry_changes":   {},
	"ledger_entries":  {},
}

// Applicable reports whether source exists on network, and if not, the operator-facing reason.
// Unknown networks are treated like pubnet so a misspelling can never silently hide a source.
func Applicable(source, network string) (bool, string) {
	if network == "" || network == Pubnet {
		return true, ""
	}
	if _, ok := allNetworks[source]; ok {
		return true, ""
	}
	return false, fmt.Sprintf("no contract set registered for network %s — the %s decoder is anchored to pubnet contract identities (ADR-0035)", network, source)
}

// Known reports whether source is classified (ledger-anchored or pubnet-only). Consumers outside
// config.KnownSources use it to fail CI on an unclassified name rather than let [Applicable]
// silently exclude it from every non-pubnet network.
func Known(source string) bool {
	if _, ok := allNetworks[source]; ok {
		return true
	}
	for _, s := range PubnetOnlySources {
		if s == source {
			return true
		}
	}
	return false
}

// NotApplicable is one entry of the coverage endpoint's exclusion list.
type NotApplicable struct {
	Source string
	Reason string
}

// Filter partitions sources into the applicable set (input order) and the excluded set
// (source-sorted, with reasons). Every consumer uses it, so catalogue and endpoint cannot disagree.
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

// PubnetOnlySources lists the sources anchored to pubnet contract identities, which a non-pubnet
// network must report as not applicable. It lets the API name them without importing the ops
// catalogue; a test cross-checks it against config.KnownSources.
var PubnetOnlySources = []string{
	"aquarius", "band", "blend", "blend_backstop", "blend_emitter", "cctp", "comet",
	"defindex", "phoenix", "redstone", "reflector-cex", "reflector-dex", "reflector-fx",
	"rozo", "sorocredit", "soroswap", "soroswap-router", "sushiswap_v3",
	"spectra", "upshift",
}

// NotApplicableOn lists, source-sorted with reasons, every canonical
// source that does not exist on network. Empty on pubnet.
func NotApplicableOn(network string) []NotApplicable {
	_, excluded := Filter(PubnetOnlySources, network)
	return excluded
}
