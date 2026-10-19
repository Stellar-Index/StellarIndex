package v1

import (
	"context"
	"database/sql"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ChangeSummaryReader is the seam the change-summary handler reads through; timescale.Store satisfies it.
type ChangeSummaryReader interface {
	GetChangeSummary(ctx context.Context, entityType, entityID string) (timescale.ChangeSummaryRow, error)
	AssetChangeFromSnapshot(ctx context.Context, assetID string) (timescale.ChangeSummaryRow, error)
}

// ChangeSummaryResponse is the GET /v1/changes/{entity_type}/{id} shape; *_value fields are money as exact decimal strings, *_delta_pct are display-grade numbers.
type ChangeSummaryResponse struct {
	EntityType   string `json:"entity_type"`
	EntityID     string `json:"entity_id"`
	RefreshedAt  string `json:"refreshed_at"`
	CurrentValue string `json:"current_value"`

	H1Value     *string  `json:"h1_value,omitempty"`
	H1DeltaPct  *float64 `json:"h1_delta_pct,omitempty"`
	H24Value    *string  `json:"h24_value,omitempty"`
	H24DeltaPct *float64 `json:"h24_delta_pct,omitempty"`
	D7Value     *string  `json:"d7_value,omitempty"`
	D7DeltaPct  *float64 `json:"d7_delta_pct,omitempty"`
	D30Value    *string  `json:"d30_value,omitempty"`
	D30DeltaPct *float64 `json:"d30_delta_pct,omitempty"`

	ATHValue *string `json:"ath_value,omitempty"`
	ATHAt    string  `json:"ath_at,omitempty"`
	ATLValue *string `json:"atl_value,omitempty"`
	ATLAt    string  `json:"atl_at,omitempty"`

	StreakDirection string `json:"streak_direction,omitempty"`
	StreakDays      *int   `json:"streak_days,omitempty"`
	Acceleration    string `json:"acceleration,omitempty"`
}

// allowedChangeSummaryEntityTypes pins the entity_type values a change-summary worker actually writes. The table CHECK also
// reserves 'protocol' and 'source', but accepting them would only yield a misleading 404; reopening one means landing its
// worker, this set and the OpenAPI enum together.
var allowedChangeSummaryEntityTypes = map[string]struct{}{
	"coin": {},
	"pair": {},
}

// changeSummaryCoinCandidates returns the entity_id forms to try for a coin: the worker keys rows by canonical asset_id
// (`native`, `crypto:XLM`, `USDC-GA5Z...`) but consumers use the slug (`XLM`). The literal input goes first; non-coin types
// return only it.
func changeSummaryCoinCandidates(entityType, entityID string) []string {
	if entityType != "coin" {
		return []string{entityID}
	}

	out := []string{entityID}
	seen := map[string]struct{}{entityID: {}}
	add := func(id string) {
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}

	upper := strings.ToUpper(strings.TrimSpace(entityID))
	if upper == "XLM" {
		add("native")
		add("crypto:XLM")
	}
	// A bare classic-asset code ("USDC") also tries the global crypto ticker form the aggregator
	// publishes for CEX/FX-quoted trades. A classic code is at most 12 characters (alphanum12);
	// the bound keeps a 56-char strkey such as an XLM SAC C-address from becoming a bogus ticker.
	if upper != "" && upper != "NATIVE" && len(upper) <= 12 &&
		!strings.Contains(entityID, "-") && !strings.Contains(entityID, ":") {
		add("crypto:" + upper)
	}
	// `native` → also try `crypto:XLM`.
	if entityID == "native" {
		add("crypto:XLM")
	}
	// Try canonical.ParseAsset to see if the literal form parses to
	// a known asset; if so, also include its String() form (which
	// may differ from the input, e.g. casing).
	if a, err := canonical.ParseAsset(entityID); err == nil {
		add(a.String())
	}
	// Fold in the FULL alias family of every form gathered so far, so the XLM SAC C-address (and any
	// classic<->SAC pair) is a candidate: the worker writes a Soroban-sourced XLM rollup under the
	// SAC form. AssetAliasStrings orders the SAC form LAST so thin Soroban data never shadows the
	// established forms. Ranging over a snapshot stops the append feeding itself.
	for _, id := range append([]string(nil), out...) {
		if a, err := canonical.ParseAsset(id); err == nil {
			for _, form := range canonical.AssetAliasStrings(a) {
				add(form)
			}
		}
	}
	return out
}

// handleChangeSummary serves GET /v1/changes/{entity_type}/{id}: 503 no reader, 400 bad entity_type, 404 no row yet. The cache
// header is short because the worker refreshes every 5 minutes.
func (s *Server) handleChangeSummary(w http.ResponseWriter, r *http.Request) {
	if s.ChangeSummary == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/change-summary-unavailable",
			"Change summary unavailable", http.StatusServiceUnavailable,
			"This deployment hasn't wired the change-summary reader yet.")
		return
	}

	entityType := r.PathValue("entity_type")
	if _, ok := allowedChangeSummaryEntityTypes[entityType]; !ok {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-entity-type",
			"Invalid entity_type", http.StatusBadRequest,
			"entity_type must be one of: coin, pair")
		return
	}
	entityID := r.PathValue("id")
	if entityID == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-entity-id",
			"Invalid entity_id", http.StatusBadRequest,
			"id path segment is required")
		return
	}

	// The worker writes one coin row per canonical asset_id form, so a bare slug would 404 on strict
	// equality: expand into candidates and take the first hit.
	//
	// Deliberately NOT the set `oracleAssetCandidates` builds: that ticker translation is gated on
	// the verified-currency catalogue because it answers a per-ISSUER identity with global-ticker
	// rows. This only promotes a BARE code; an id carrying `-` or `:` is left alone, so no
	// (code, issuer) pair is ever widened to a ticker.
	candidates := changeSummaryCoinCandidates(entityType, entityID)

	row, err := firstChangeSummary(candidates, func(id string) (timescale.ChangeSummaryRow, error) {
		return s.ChangeSummary.GetChangeSummary(r.Context(), entityType, id)
	})
	if errors.Is(err, sql.ErrNoRows) && entityType == "coin" {
		// The worker covers configured pairs only; a classic asset still has its listing price and short-window changes.
		row, err = firstChangeSummary(candidates, func(id string) (timescale.ChangeSummaryRow, error) {
			return s.ChangeSummary.AssetChangeFromSnapshot(r.Context(), id)
		})
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/change-summary-not-found",
			"Change summary not found", http.StatusNotFound,
			"The change-summary worker hasn't computed a row for this entity yet.")
		return
	}
	if err != nil {
		s.logger.Warn("change-summary read",
			"entity_type", entityType, "entity_id", entityID, "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/change-summary-error",
			"Change summary read failed", http.StatusInternalServerError,
			"Storage layer returned an error.")
		return
	}

	if s.changeSummaryWithheld(w, r, row) {
		return
	}
	stale := time.Since(row.RefreshedAt) > changeSummaryStaleAfter
	writeJSON(w, s.changeSummaryResponse(row), Flags{Stale: stale})
}

// firstChangeSummary returns the first candidate's row; sql.ErrNoRows only when every candidate missed, any other error at once.
func firstChangeSummary(candidates []string, get func(id string) (timescale.ChangeSummaryRow, error)) (timescale.ChangeSummaryRow, error) {
	err := sql.ErrNoRows
	for _, id := range candidates {
		var row timescale.ChangeSummaryRow
		if row, err = get(id); !errors.Is(err, sql.ErrNoRows) {
			return row, err
		}
	}
	return timescale.ChangeSummaryRow{}, err
}

// changeSummaryStaleAfter is the row age past which a summary is flagged stale: 10 minutes against the worker's 5-minute
// refresh, per the openapi contract.
const changeSummaryStaleAfter = 10 * time.Minute

// changeSummaryGateSurface labels this surface on the withholding metrics.
const changeSummaryGateSurface = "change_summary"

// changeSummaryWithheld applies /v1/price's withholding verdict to a stored row and writes the withheld problem when it fires: every value
// is an aggregated price claim. Decided at read time so new verdicts cover older rows. A "pair" row names its market; a "coin" row names
// only the base, so substance asks whether ANY plausible market clears the floor. An unparseable id is withheld.
func (s *Server) changeSummaryWithheld(w http.ResponseWriter, r *http.Request, row timescale.ChangeSummaryRow) bool {
	base, quote, ok := changeSummaryLegs(row.EntityType, row.EntityID)
	if !ok {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/change-summary-not-found",
			"Change summary not found", http.StatusNotFound,
			"The stored change summary does not name a market this deployment can vet.")
		return true
	}
	if row.EntityType != "coin" {
		// A pair row names one market: both gates, through the package's
		// one fold.
		withheld := withheldBy(r.Context(), s.Substance, s.Scam, base, quote, changeSummaryGateSurface)
		if withheld == pricingguard.NotWithheld {
			return false
		}
		writePriceWithheldProblem(w, r, base, quote, withheldReasonFor(withheld))
		return true
	}
	if s.writeIfScamWithheld(w, r, base, quote, changeSummaryGateSurface) {
		return true
	}
	if s.Substance == nil || s.assetPriceAllowed(r.Context(), base, changeSummaryGateSurface) {
		return false
	}
	writePriceWithheldProblem(w, r, base, quote, PriceWithheldSubstance)
	return true
}

// changeSummaryLegs returns the market a stored row is priced in, from the STORED entity id, never the caller's input. "pair":
// the id is `base/quote`. "coin": base only, so the quote is assumed [defaultPriceQuote] (removing that needs a schema change).
func changeSummaryLegs(entityType, entityID string) (base, quote canonical.Asset, ok bool) {
	switch entityType {
	case "pair":
		pair, err := canonical.ParsePair(entityID)
		if err != nil {
			return canonical.Asset{}, canonical.Asset{}, false
		}
		return pair.Base, pair.Quote, true
	case "coin":
		asset, err := canonical.ParseAsset(entityID)
		if err != nil {
			return canonical.Asset{}, canonical.Asset{}, false
		}
		return asset, defaultPriceQuote, true
	}
	return canonical.Asset{}, canonical.Asset{}, false
}

// changeSummaryResponse projects a stored row onto the wire. change_summary_5m holds RAW prices_1m ratios: deltas, streaks and
// accelerations survive (the factor cancels) but absolute figures are scaled here for a confirmed non-7-decimals token, as
// /v1/price. AT READ TIME because the upsert RATCHETS ath_value/atl_value (GREATEST/LEAST): normalising at write would switch
// scale mid-life and pin the ratchet to an extreme from the other scale.
func (s *Server) changeSummaryResponse(row timescale.ChangeSummaryRow) ChangeSummaryResponse {
	var scale *big.Rat
	if !row.Normalized {
		scale = s.changeSummaryValueScale(row.EntityType, row.EntityID)
	}
	resp := ChangeSummaryResponse{
		EntityType:      row.EntityType,
		EntityID:        row.EntityID,
		RefreshedAt:     row.RefreshedAt.UTC().Format(time.RFC3339),
		CurrentValue:    scaledMoneyStr(row.CurrentValue, scale),
		H1Value:         scaledMoneyStrPtr(row.H1Value, scale),
		H1DeltaPct:      row.H1DeltaPct,
		H24Value:        scaledMoneyStrPtr(row.H24Value, scale),
		H24DeltaPct:     row.H24DeltaPct,
		D7Value:         scaledMoneyStrPtr(row.D7Value, scale),
		D7DeltaPct:      row.D7DeltaPct,
		D30Value:        scaledMoneyStrPtr(row.D30Value, scale),
		D30DeltaPct:     row.D30DeltaPct,
		ATHValue:        scaledMoneyStrPtr(row.ATHValue, scale),
		ATLValue:        scaledMoneyStrPtr(row.ATLValue, scale),
		StreakDirection: row.StreakDirection,
		StreakDays:      row.StreakDays,
		Acceleration:    row.Acceleration,
	}
	if row.ATHAt != nil {
		resp.ATHAt = row.ATHAt.UTC().Format(time.RFC3339)
	}
	if row.ATLAt != nil {
		resp.ATLAt = row.ATLAt.UTC().Format(time.RFC3339)
	}
	return resp
}

// changeSummaryValueScale returns the exact non-standard-decimals factor for a row's absolute values, nil when nothing to
// scale. A "coin" row's assumed quote is the standard scale, exact for every realistic quote; an unparseable id scales by
// nothing.
func (s *Server) changeSummaryValueScale(entityType, entityID string) *big.Rat {
	base, quote, ok := changeSummaryLegs(entityType, entityID)
	if !ok {
		return nil
	}
	baseDec := aggregate.ResolveDecimals(s.NonstandardDecimals, base)
	quoteDec := aggregate.ResolveDecimals(s.NonstandardDecimals, quote)
	if baseDec == quoteDec {
		return nil
	}
	return aggregate.DecimalsAdjustment(baseDec, quoteDec)
}

// scaledMoneyStr applies the decimals factor to a stored decimal string (nil scale returns v). No float64: big.Rat multiplies
// exactly and ratDecimalString renders it exactly, as the scale is a power of ten ([aggregate.DecimalsAdjustment]).
func scaledMoneyStr(v string, scale *big.Rat) string {
	if scale == nil {
		return v
	}
	r, ok := new(big.Rat).SetString(v)
	if !ok {
		// Not a plain decimal (shouldn't happen for a NUMERIC-column
		// value); print it unscaled rather than fail closed.
		return v
	}
	r.Mul(r, scale)
	out, ok := ratDecimalString(r)
	if !ok {
		return v
	}
	return out
}

// scaledMoneyStrPtr is the nullable-money form of [scaledMoneyStr]: nil
// stays nil (omitempty → absent).
func scaledMoneyStrPtr(v *string, scale *big.Rat) *string {
	if v == nil {
		return nil
	}
	out := scaledMoneyStr(*v, scale)
	return &out
}

// ratDecimalString renders r as an exact decimal string; ok=false if r does not terminate (denominator has a prime factor other
// than 2 or 5), and the caller falls back to the unscaled string.
func ratDecimalString(r *big.Rat) (string, bool) {
	den := new(big.Int).Set(r.Denom())
	two, five, one := big.NewInt(2), big.NewInt(5), big.NewInt(1)
	twos, fives := 0, 0
	for new(big.Int).Mod(den, two).Sign() == 0 {
		den.Div(den, two)
		twos++
	}
	for new(big.Int).Mod(den, five).Sign() == 0 {
		den.Div(den, five)
		fives++
	}
	if den.Cmp(one) != 0 {
		return "", false
	}
	places := twos
	if fives > places {
		places = fives
	}
	scaleUp := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(places)), nil)
	numScaled := new(big.Int).Mul(r.Num(), scaleUp)
	numScaled.Div(numScaled, r.Denom()) // exact: r.Denom() divides num*scaleUp

	neg := numScaled.Sign() < 0
	if neg {
		numScaled.Neg(numScaled)
	}
	digits := numScaled.String()
	for len(digits) <= places {
		digits = "0" + digits
	}

	out := digits
	if places > 0 {
		intPart := digits[:len(digits)-places]
		fracPart := strings.TrimRight(digits[len(digits)-places:], "0")
		if fracPart == "" {
			out = intPart
		} else {
			out = intPart + "." + fracPart
		}
	}
	if neg {
		out = "-" + out
	}
	return out, true
}
