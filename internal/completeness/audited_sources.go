package completeness

import (
	"sort"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// staticAuditedSources are the reconciliation-catalogue entries
// compute-completeness audits under every configuration
// (internal/ops/chops/reconciliation_catalogue.go).
var staticAuditedSources = []string{
	"aquarius", "blend", "blend_backstop", "blend_emitter", "cctp", "comet", "defindex",
	"phoenix", "rozo", "sdex", "sorocredit", "soroswap", "soroswap-router", "sushiswap_v3",
	"upshift",
}

// AuditedSources returns, sorted, every source compute-completeness publishes a
// verdict for under cfg, before network scoping. It is the denominator of
// /v1/coverage: a source listed here with no completeness_snapshots row (its
// first audit failed, or the row was cleared) must still count as a source
// without a verdict. The catalogue lives in internal/ops/chops, which the API
// must not import; a chops test holds the two in lockstep for every
// config-gated entry.
func AuditedSources(cfg config.Config) []string {
	out := append([]string(nil), staticAuditedSources...)
	gated := []struct {
		contract string
		name     string
	}{
		{cfg.Oracle.Reflector.DEXContract, "reflector-dex"},
		{cfg.Oracle.Reflector.CEXContract, "reflector-cex"},
		{cfg.Oracle.Reflector.FXContract, "reflector-fx"},
		{cfg.Oracle.Redstone.AdapterContract, "redstone"},
		{cfg.Oracle.Band.StandardReferenceContract, "band"},
	}
	for _, g := range gated {
		if g.contract != "" {
			out = append(out, g.name)
		}
	}
	if len(cfg.Supply.WatchedSEP41Contracts) > 0 {
		out = append(out, "sep41_transfers", "sep41_supply")
	}
	sort.Strings(out)
	return out
}
