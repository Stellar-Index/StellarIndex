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
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ChangeSummaryReader is the seam the change-summary handler reads
// through. timescale.Store satisfies it via GetChangeSummary; tests
// substitute a fake.
type ChangeSummaryReader interface {
	GetChangeSummary(ctx context.Context, entityType, entityID string) (timescale.ChangeSummaryRow, error)
}

// ChangeSummaryResponse is the wire shape returned by
// GET /v1/changes/{entity_type}/{id}.
//
// Mirrors the change_summary_5m hypertable but with JSON-friendly
// types: pointers stay pointers (omitempty + null on the wire),
// timestamps are RFC 3339, and the entity-keying tuple is echoed
// in the response so a single payload is self-describing.
//
// Powers every multi-window delta strip on the explorer per
// data-inventory §6.1.
// M7 (INV-2): the *_value fields are MONEY (a price / market-cap snapshot)
// and cross the wire as JSON STRINGS, matching every other money field the
// API serves (e.g. /v1/price's `price`), and carry the exact decimal string
// the changesummary rollup stored — no float64 round-trip (GH #602). The
// *_delta_pct fields are percentages, not money, and stay JSON numbers,
// display-grade by design; streak_days stays an int.
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

// allowedChangeSummaryEntityTypes pins the set of entity_type values
// the API accepts: the families a change-summary worker actually
// writes. Both are computed off one aggregator pair's 1-minute VWAP
// (see buildChangeSummaryEntities in cmd/stellarindex-aggregator).
//
// The change_summary_5m CHECK is wider — it reserves 'protocol' and
// 'source' as well — but no worker computes those, so accepting them
// here bought only a 404 saying "the worker hasn't computed a row
// yet" for entities no worker will ever compute. The API advertises
// what it can serve; re-opening a family means landing its worker,
// this set, and the OpenAPI enum together.
var allowedChangeSummaryEntityTypes = map[string]struct{}{
	"coin": {},
	"pair": {},
}

// changeSummaryCoinCandidates returns the entity_id forms to try
// for a coin lookup. The worker writes rows under the canonical
// asset_id (`native`, `crypto:XLM`, `USDC-GA5Z…`), but consumers
// reasonably reach for the friendly slug (`XLM`, `USDC`); without
// expansion `/v1/changes/coin/XLM` 404s even when XLM data is
// populated under `native` and `crypto:XLM`.
//
// First entry is always the literal user input so an exact match
// short-circuits; subsequent entries are best-effort translations.
//
// For non-coin entity_types, returns just the literal input — the
// pair form is documented as an exact `base/quote` string.
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
	// Bare classic-asset code (e.g. "USDC", "EURC") → also try the
	// global crypto ticker form, which the aggregator publishes for
	// CEX/FX-quoted trades. The full `<CODE>-G…` strkey form is
	// already the literal entityID when callers provide it.
	//
	// A Stellar classic asset code is at most 12 characters
	// (SEP-11 alphanum12); the length bound keeps a 56-char strkey —
	// e.g. an XLM SAC C-address — from being manufactured into a
	// bogus `crypto:CAS3J7…` ticker that matches nothing.
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
	// C4-015 (W2-tail): fold in the FULL canonical alias family for
	// every form gathered so far, so the XLM SAC C-address (and any
	// configured classic↔SAC pair) is a candidate too — the change-
	// summary worker writes a Soroban-sourced XLM rollup under the SAC
	// form, which the native/crypto:XLM branches above omit. AssetAliasStrings
	// orders the SAC form LAST, so it is only ever tried after the deep
	// classic/CEX forms miss (money-safety: thin Soroban never shadows
	// the established forms). Ranging over a snapshot keeps the append
	// from feeding itself.
	for _, id := range append([]string(nil), out...) {
		if a, err := canonical.ParseAsset(id); err == nil {
			for _, form := range canonical.AssetAliasStrings(a) {
				add(form)
			}
		}
	}
	return out
}

// handleChangeSummary serves GET /v1/changes/{entity_type}/{id}.
//
// Returns 503 when no ChangeSummary reader is wired (operator
// hasn't run the rollup worker yet). Returns 400 for bad
// entity_type. Returns 404 (problem+json) when the entity has no
// row yet — the worker's first refresh hasn't run, or the entity
// was added since the last refresh.
//
// Cache header: short-lived, since the worker refreshes on a
// 5-minute cadence.
func (s *Server) handleChangeSummary(w http.ResponseWriter, r *http.Request) {
	if s.changesum == nil {
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

	// For coin entities, the worker writes one row per canonical
	// asset_id form (`native`, `crypto:XLM`, `USDC-GA5Z…`, …). A
	// caller passing the friendly slug "XLM" or just "USDC" without
	// the issuer suffix would 404 against the strict-equality lookup
	// even when the underlying data exists. Expand into candidate
	// forms, then try each in order; first hit wins.
	//
	// Deliberately NOT the same set as `oracleAssetCandidates` (which
	// it once mirrored): that helper's ticker translation is gated on
	// the verified-currency catalogue since #336, because it answers a
	// per-ISSUER identity with global-ticker rows. This one only ever
	// promotes a BARE code the caller typed — an id carrying `-` or `:`
	// is left alone (see changeSummaryCoinCandidates), so no
	// (code, issuer) pair is ever widened to a ticker here.
	candidates := changeSummaryCoinCandidates(entityType, entityID)

	var (
		row timescale.ChangeSummaryRow
		err error
		hit bool
	)
	for _, id := range candidates {
		row, err = s.changesum.GetChangeSummary(r.Context(), entityType, id)
		if err == nil {
			hit = true
			break
		}
		if !errors.Is(err, sql.ErrNoRows) {
			break // real storage error — surface it below
		}
	}
	if !hit && errors.Is(err, sql.ErrNoRows) {
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

// changeSummaryStaleAfter is the row-age past which a served change
// summary is flagged stale. Matches the openapi contract for this
// surface: the worker refreshes every 5 minutes, and a row older than
// 10 minutes means it is lagging.
const changeSummaryStaleAfter = 10 * time.Minute

// changeSummaryGateSurface labels this surface on the withholding metrics.
const changeSummaryGateSurface = "change_summary"

// changeSummaryWithheld applies /v1/price's withholding verdict to a stored
// row and writes the withheld problem when it fires. Every value on the row
// is an aggregated price claim for the row's market — current_value, the
// window values and the ATH/ATL — so a market /v1/price refuses to price
// must not be priced here either. Decided at read time, like the decimals
// scale: a verdict (a newly flagged issuer, a market gone thin) must take
// effect on rows the worker wrote before it.
//
// A "pair" row names its market exactly. A "coin" row names only the base
// (the worker does not record which configured pair it read), so the
// substance side asks the listing's single-asset question — does ANY
// plausible backing market clear the floor — and the scam side asks about
// the base. An id that does not parse names no market that can be vetted,
// and is withheld rather than served unvetted.
func (s *Server) changeSummaryWithheld(w http.ResponseWriter, r *http.Request, row timescale.ChangeSummaryRow) bool {
	base, quote, ok := changeSummaryLegs(row.EntityType, row.EntityID)
	if !ok {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/change-summary-not-found",
			"Change summary not found", http.StatusNotFound,
			"The stored change summary does not name a market this deployment can vet.")
		return true
	}
	ctx := r.Context()
	if s.writeIfScamWithheld(w, r, base, quote, changeSummaryGateSurface) {
		return true
	}
	if s.substance == nil {
		return false
	}
	var allowed bool
	if row.EntityType == "coin" {
		allowed = s.listingPriceAllowed(ctx, base)
	} else {
		allowed = s.substance.Allowed(ctx, base, quote, changeSummaryGateSurface)
	}
	if !allowed {
		writePriceWithheldProblem(w, r, base, quote, PriceWithheldSubstance)
		return true
	}
	return false
}

// changeSummaryLegs returns the market a stored row's absolute values are
// priced in, read from the STORED entity id — what the worker keyed the row
// on, never what the caller typed:
//
//   - "pair": the id IS the source pair (`base/quote`).
//   - "coin": the id is the base asset only. The worker computes a coin row
//     off the first configured aggregator pair for that base and does not
//     record which, so the quote leg is taken as [defaultPriceQuote].
//     Recording the source pair on the row would remove the assumption and
//     needs a schema change.
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

// changeSummaryResponse projects a stored row onto the wire shape,
// decimals-normalising every absolute value on the way out.
//
// change_summary_5m holds RAW prices_1m ratios: the rollup worker copies
// the CAGG's vwap through untouched (internal/aggregate/changesummary).
// The *_delta_pct fields survive that — both legs of each percentage come
// from the same raw series, so the factor cancels — and so do the streak
// and acceleration labels. The absolute figures do not: current_value,
// the four window values and the ATH/ATL were published unscaled for a
// confirmed non-7-decimals token while /v1/price normalised the same
// bucket.
//
// WHY AT READ TIME, not in the worker. The upsert RATCHETS ath_value /
// atl_value (GREATEST / LEAST against the stored row), and a token is
// flagged only after it has already been trading. Normalising at write
// would switch a row's scale mid-life and leave the ratchet pinned to an
// extreme from the other scale for good. Keeping the table raw and
// scaling here keeps one scale per row for ever, follows the table the
// moment a row is confirmed or corrected, and matches how every other
// CAGG-derived surface is handled (the CAGGs stay raw; serving
// normalises).
func (s *Server) changeSummaryResponse(row timescale.ChangeSummaryRow) ChangeSummaryResponse {
	scale := s.changeSummaryValueScale(row.EntityType, row.EntityID)
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

// changeSummaryValueScale returns the exact dex-nonstandard-decimals
// factor for a stored row's absolute values, or nil when there is nothing
// to scale (the overwhelmingly common case — callers then format the
// stored float exactly as before).
//
// The legs are [changeSummaryLegs]'. For a "coin" row the assumed quote is
// the standard scale, which is exact for every quote a coin row is
// realistically computed against (XLM, a classic or SAC-wrapped
// stablecoin, a fiat or global ticker — none of which can be non-7dp).
// An id that does not parse names nothing the confirmed table could hold,
// so it scales by nothing.
func (s *Server) changeSummaryValueScale(entityType, entityID string) *big.Rat {
	base, quote, ok := changeSummaryLegs(entityType, entityID)
	if !ok {
		return nil
	}
	baseDec := aggregate.ResolveDecimals(s.nonstandardDecimals, base)
	quoteDec := aggregate.ResolveDecimals(s.nonstandardDecimals, quote)
	if baseDec == quoteDec {
		return nil
	}
	return aggregate.DecimalsAdjustment(baseDec, quoteDec)
}

// scaledMoneyStr applies the decimals factor to a stored decimal string. A
// nil scale returns v unchanged.
//
// v is already the exact decimal the rollup stored (GH #602) — no float64
// round-trip here or in the multiply. big.Rat carries the multiply exactly,
// and ratDecimalString renders it back to decimal exactly: the scale is
// always a power of ten (see [aggregate.DecimalsAdjustment]), so the
// product's denominator only ever picks up factors of 2 and 5.
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

// ratDecimalString renders r as an exact base-10 decimal string. ok=false
// if r's reduced denominator has a prime factor other than 2 or 5 (i.e.
// r is not a terminating decimal) — not expected at this call site, but
// the caller falls back to the unscaled string rather than panicking.
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
