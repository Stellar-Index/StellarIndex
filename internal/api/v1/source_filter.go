package v1

import (
	"net/http"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// isCEXVenue reports whether name is a centralised exchange venue: a
// market in its own right, not a vendor reselling other venues' data.
func isCEXVenue(name string) bool {
	md, ok := external.Registry[name]
	return ok && md.Class == external.ClassExchange && md.Subclass == external.SubclassCEX
}

// sourceSelectable reports whether a single-source selector may name this
// source: on-chain sources and CEX venues. Off-chain data vendors
// (aggregators, FX providers, Tiingo, Chainlink, sovereign anchors) are
// served only beside other sources. /v1/sources exposes it as `selectable`
// so clients gate on the same predicate the 400 uses.
func sourceSelectable(name string) bool {
	return external.IsOnChain(name) || isCEXVenue(name)
}

// sourceFilterOK validates an optional single-source selector: it must
// name a registered, selectable source (see sourceSelectable). A data
// vendor selected alone would make a route a proxy for that vendor's API.
// Writes the 400 on refusal.
func sourceFilterOK(w http.ResponseWriter, r *http.Request, source string) bool {
	if source == "" {
		return true
	}
	// An unknown name is a 400, not an empty page: a typo would otherwise
	// look identical on the wire to "this source has no data".
	if _, ok := external.Registry[source]; !ok {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/unknown-source",
			"Unknown source", http.StatusBadRequest,
			"source must be a registered source name (see /v1/sources for the canonical list); got "+source)
		return false
	}
	if !sourceSelectable(source) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/off-chain-source-filter",
			"Data-vendor source cannot be selected alone", http.StatusBadRequest,
			"source="+source+" is an off-chain data vendor (aggregator, FX, oracle or sovereign anchor); its data is served only alongside other sources. Omit source for the multi-source response, or select an on-chain source or exchange venue (see /v1/sources, selectable=true).")
		return false
	}
	return true
}
