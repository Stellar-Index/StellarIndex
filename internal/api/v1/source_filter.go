package v1

import (
	"net/http"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// sourceFilterOK validates an optional single-source selector: it must
// name a registered source, and that source must be on-chain. An off-chain
// provider (CEX, FX, aggregator, sovereign anchor, Chainlink, Tiingo) is
// served only beside other sources, never selected on its own, so a route
// cannot act as a proxy for one vendor's API. Writes the 400 on refusal.
func sourceFilterOK(w http.ResponseWriter, r *http.Request, param, source string) bool {
	if source == "" {
		return true
	}
	// An unknown name is a 400, not an empty page: a typo would otherwise
	// look identical on the wire to "this source has no data".
	if _, ok := external.Registry[source]; !ok {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/unknown-source",
			"Unknown source", http.StatusBadRequest,
			param+" must be a registered source name (see /v1/sources for the canonical list); got "+source)
		return false
	}
	if !external.IsOnChain(source) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/off-chain-source-filter",
			"Off-chain source cannot be selected alone", http.StatusBadRequest,
			param+"="+source+" names an off-chain provider; its data is served only alongside other sources. Omit "+param+" for the multi-source response, or select an on-chain source (see /v1/sources, on_chain=true).")
		return false
	}
	return true
}
