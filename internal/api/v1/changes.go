package v1

import (
	"context"
	"database/sql"
	"errors"
	"math/big"
	"net/http"
	"strconv"
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
// API serves (e.g. /v1/price's `price`). The *_delta_pct fields are
// percentages, not money, and stay JSON numbers; streak_days stays an int.
// The values are display-grade (the changesummary rollup accepts float64
// rounding by design — see internal/aggregate/changesummary/rollup.go), so
// the string carries the shortest decimal that round-trips the stored value.
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

// moneyStr formats a display-grade money value as the shortest decimal string
// that round-trips the float64 (so 1.1 → "1.1", not "1.1000000000000001").
func moneyStr(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

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

	writeJSON(w, s.changeSummaryResponse(row), Flags{})
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
// The legs come from the STORED entity id, which is what the worker keyed
// the row on, never from what the caller typed:
//
//   - "pair": the id IS the source pair (`base/quote`), so both legs are
//     known and the factor is exact.
//   - "coin": the id is the base asset only. The worker computes a coin
//     row off the first configured aggregator pair for that base and does
//     not record which, so the quote leg is taken as the standard scale.
//     That is exact for every quote a coin row is realistically computed
//     against (XLM, a classic or SAC-wrapped stablecoin, a fiat or global
//     ticker — none of which can be non-7dp); recording the source pair on
//     the row would remove the assumption and needs a schema change.
//
// An id that does not parse names nothing the confirmed table could hold,
// so it scales by nothing.
func (s *Server) changeSummaryValueScale(entityType, entityID string) *big.Rat {
	var base, quote canonical.Asset
	switch entityType {
	case "pair":
		pair, err := canonical.ParsePair(entityID)
		if err != nil {
			return nil
		}
		base, quote = pair.Base, pair.Quote
	case "coin":
		asset, err := canonical.ParseAsset(entityID)
		if err != nil {
			return nil
		}
		base, quote = asset, defaultPriceQuote
	default:
		return nil
	}
	baseDec := aggregate.ResolveDecimals(s.nonstandardDecimals, base)
	quoteDec := aggregate.ResolveDecimals(s.nonstandardDecimals, quote)
	if baseDec == quoteDec {
		return nil
	}
	return aggregate.DecimalsAdjustment(baseDec, quoteDec)
}

// scaledMoneyStr is [moneyStr] with the decimals factor applied. A nil
// scale is byte-identical to moneyStr.
//
// The multiply runs on the value's DECIMAL rendering, not on the float:
// 1.15 × 100 in binary floating point is 114.99999999999999, and the
// shortest round-trip string of that is exactly the kind of number this
// endpoint must not print. Through the decimal it is 115.
func scaledMoneyStr(v float64, scale *big.Rat) string {
	if scale == nil {
		return moneyStr(v)
	}
	r, ok := new(big.Rat).SetString(moneyStr(v))
	if !ok {
		// NaN / ±Inf have no decimal to scale; print them as before.
		return moneyStr(v)
	}
	f, _ := r.Mul(r, scale).Float64() // i128:ok v is already float64 upstream (#602); the Rat only applies the decimals scale exactly
	return moneyStr(f)
}

// scaledMoneyStrPtr is the nullable-money form of [scaledMoneyStr]: nil
// stays nil (omitempty → absent).
func scaledMoneyStrPtr(v *float64, scale *big.Rat) *string {
	if v == nil {
		return nil
	}
	out := scaledMoneyStr(*v, scale)
	return &out
}
