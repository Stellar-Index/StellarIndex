package explorer

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// This file serves GET /v1/accounts/{g_strkey}/positions — the "DeFi
// positions" view: "enter an address, see all your DeFi positions"
// across every ADR-0035-gated on-chain protocol this repo folds a
// per-user position out of. Sibling of movements.go's
// GET /v1/accounts/{g_strkey}/movements — same package, same Handler,
// same response-writing seams, same x-stability: experimental
// posture.
//
// Six sources, six independent folds (positions.go in
// internal/storage/timescale has the SQL + the per-source
// amount-semantics evidence citations); this file's job is ONLY to map
// each fold row onto the shared wire shape, resolve venue/asset
// display labels best-effort, and apply the include_closed filter. It
// does not compute any position amount itself.

// PositionsReader is the narrow Postgres read seam this endpoint needs
// — the six per-protocol fold queries. *timescale.Store satisfies it.
// Nil disables the endpoint (503), same degrade pattern as Reader.
type PositionsReader interface {
	BlendPositionsByUser(ctx context.Context, address string) ([]timescale.BlendPositionFold, error)
	BlendBackstopSharesByUser(ctx context.Context, address string) ([]timescale.BlendBackstopFold, error)
	PhoenixStakeByUser(ctx context.Context, address string) ([]timescale.PhoenixStakeFold, error)
	DefindexVaultSharesByUser(ctx context.Context, address string) ([]timescale.DefindexVaultFold, error)
	CreditPositionsByOwner(ctx context.Context, address string) ([]timescale.CreditPositionFold, error)
	AquariusGaugeByUser(ctx context.Context, address string) ([]timescale.AquariusGaugeFold, error)
}

// PoolTokensReader mirrors package v1's ProtocolPoolTokensReader
// (protocol_pairs.go) structurally — *timescale.Store satisfies both.
// A separate, narrower copy lives here because this package must not
// import package v1 (v1 already imports explorer to register routes;
// the reverse would cycle — see this package's doc comment). Nil /
// a nil-returning source degrades every venue_label to absent — never
// fails the endpoint (same contract as v1's copy).
type PoolTokensReader interface {
	PoolTokens(ctx context.Context, source string) (map[string][]string, error)
}

// Position-kind, amount-semantics, and basis wire vocabularies for
// AccountPositionsView. amount_semantics is intentionally a plain
// string, not a closed set the server 400s on an unrecognized filter
// against — it is OUTPUT-only, documenting what each position's amount
// IS, and gains new values as new protocols/folds are added.
const (
	PositionKindLendingSupply  = "lending_supply"
	PositionKindLendingBorrow  = "lending_borrow"
	PositionKindBackstopShares = "backstop_shares"
	PositionKindStake          = "stake"
	PositionKindVaultShares    = "vault_shares"
	PositionKindCredit         = "credit"
	PositionKindGauge          = "gauge"

	// AmountSemanticsNetUnderlying — a sum of historical per-event
	// UNDERLYING-asset amounts (never a b/d-token or share amount);
	// does NOT reflect interest/fees accrued since each event. Blend
	// money-market supply/borrow.
	AmountSemanticsNetUnderlying = "net_underlying_at_event_time"
	// AmountSemanticsShares — an exact current count of a protocol's
	// own share/LP token (minted/burned 1:1 with the summed events;
	// no accrual ambiguity). Blend backstop, Phoenix stake, DeFindex
	// vault shares.
	AmountSemanticsShares = "shares"
	// AmountSemanticsStatefulCurrent — the protocol's own most-recently
	// PUBLISHED figure, not a delta sum this fold computed. sorocredit.
	AmountSemanticsStatefulCurrent = "stateful_current"
	// AmountSemanticsSignedDeltaSum — a sum of signed per-event deltas
	// whose UNIT is not contract-source-confirmed (best-effort field
	// mapping — see internal/sources/aquarius/decode_rewards.go's
	// decodePositionUpdate doc comment). Aquarius gauge.
	AmountSemanticsSignedDeltaSum = "signed_delta_sum_unconfirmed_unit"

	// BasisEventDerived — computed here by summing this fold's own
	// event log; not read from any single "current state" field.
	BasisEventDerived = "event_derived"
	// BasisStateful — read from a field the protocol itself last
	// published, not derived by summing events.
	BasisStateful = "stateful"
)

// positionsHonestNote is always present on the response (not
// conditional like movements.go's coverage_note) — every position on
// this endpoint is a raw on-chain quantity, never a valuation, and
// event_derived positions never model accrual. Task requirement: "An
// honest top-level note that valuations are not included and
// event-derived positions don't model interest accrual."
const positionsHonestNote = "Amounts are on-chain quantities only — no USD or other valuation is applied. " +
	"event_derived positions are a sum of historical per-event amounts and do NOT model interest, fees, " +
	"or exchange-rate accrual since each event; see each position's amount_semantics for exactly what the " +
	"number represents, and basis for whether it was derived here or read from the protocol's own published state."

// PositionLastActivity is the (ledger, time) pair for the most recent
// event this fold's evidence a position is unchanged since.
type PositionLastActivity struct {
	Ledger uint32 `json:"ledger"`
	Time   string `json:"time"`
}

// PositionEntry is one row in the wire response for GET
// /v1/accounts/{g_strkey}/positions.
type PositionEntry struct {
	Protocol        string               `json:"protocol"`
	PositionKind    string               `json:"position_kind"`
	Venue           string               `json:"venue"`
	VenueLabel      string               `json:"venue_label,omitempty"`
	Assets          []string             `json:"assets,omitempty"`
	Amount          string               `json:"amount"`
	AmountSemantics string               `json:"amount_semantics"`
	LastActivity    PositionLastActivity `json:"last_activity"`
	Basis           string               `json:"basis"`

	// closed is this fold's own net-zero/closed verdict — never
	// marshaled. Default include_closed=false drops these rows.
	closed bool
}

// AccountPositionsView is the wire response for GET
// /v1/accounts/{g_strkey}/positions.
type AccountPositionsView struct {
	Account       string          `json:"account"`
	Positions     []PositionEntry `json:"positions"`
	IncludeClosed bool            `json:"include_closed"`
	Note          string          `json:"note"`
}

// positionsUnavailable writes the 503 for this endpoint specifically —
// distinct wording from unavailable() (that one names the ClickHouse
// explorer reader; this endpoint's hard dependency is the Postgres
// PositionsReader instead).
func (h *Handler) positionsUnavailable(w http.ResponseWriter, r *http.Request) {
	h.WriteProblem(w, r,
		"https://api.stellarindex.io/errors/explorer-unavailable",
		"Explorer unavailable", http.StatusServiceUnavailable,
		"This deployment hasn't wired the Postgres positions reader.")
}

// AccountPositions serves GET /v1/accounts/{g_strkey}/positions (the
// "DeFi positions" view, x-stability: experimental): folds the six
// ADR-0035-gated on-chain protocols' event tables into one per-(venue,
// kind) net-position list for the address. Not paginated — bounded by
// each fold's own venue-cardinality cap (timescale's positionsVenueLimit,
// 500 per protocol; a real user's realistic venue fan-out is nowhere
// near it).
//
// Net-zero / closed positions are excluded unless ?include_closed=true.
// "Net-zero" means different things per protocol (see each buildXPositions
// helper) — a delta-summed net of exactly 0 for event_derived protocols,
// or a Withdrawal-observed flag for sorocredit's stateful positions.
func (h *Handler) AccountPositions(w http.ResponseWriter, r *http.Request) {
	if h.Positions == nil {
		h.positionsUnavailable(w, r)
		return
	}
	g, ok := h.parseAccountStrkey(w, r)
	if !ok {
		return
	}
	includeClosed := r.URL.Query().Get("include_closed") == "true"

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	resolve := h.newPositionAssetResolver(ctx)

	var positions []PositionEntry
	positions = append(positions, h.buildBlendPositions(ctx, g, resolve)...)
	positions = append(positions, h.buildBlendBackstopPositions(ctx, g, resolve)...)
	positions = append(positions, h.buildPhoenixStakePositions(ctx, g, resolve)...)
	positions = append(positions, h.buildDefindexPositions(ctx, g)...)
	positions = append(positions, h.buildCreditPositions(ctx, g)...)
	positions = append(positions, h.buildAquariusGaugePositions(ctx, g, resolve)...)

	out := make([]PositionEntry, 0, len(positions))
	for _, p := range positions {
		if !includeClosed && p.closed {
			continue
		}
		out = append(out, p)
	}

	h.WriteJSON(w, AccountPositionsView{
		Account:       g,
		Positions:     out,
		IncludeClosed: includeClosed,
		Note:          positionsHonestNote,
	}, false)
}

// ─── asset / venue display-label resolution ──────────────────────────

// newPositionAssetResolver returns a per-request memoized contract-id ->
// display-label resolver, reusing resolveSEP41MovementAsset (movements.go)
// — the same SAC-then-raw-contract-id resolution the movements feed
// already relies on — so this endpoint doesn't duplicate SAC-resolution
// logic. Never fails: an unresolvable contract degrades to a truncated
// id.
//
// Unlike AccountMovements, this endpoint's hard dependency is
// h.Positions (Postgres), NOT h.Reader (the ClickHouse lake) —
// resolveSEP41MovementAsset itself has no nil-Reader guard (its only
// caller, movements.go, is only reachable once AccountMovements has
// already 503'd on h.Reader == nil), so a deployment that wires
// Positions without also wiring the ClickHouse Explorer reader must
// degrade the label to the raw contract id here instead of calling
// through to a nil interface.
func (h *Handler) newPositionAssetResolver(ctx context.Context) func(contractID string) string {
	cache := map[string]string{}
	return func(contractID string) string {
		if contractID == "" {
			return ""
		}
		if v, ok := cache[contractID]; ok {
			return v
		}
		resolved := contractID
		if h.Reader != nil {
			resolved = h.resolveSEP41MovementAsset(ctx, contractID)
		}
		v := assetDisplayLabel(resolved)
		cache[contractID] = v
		return v
	}
}

// assetDisplayLabel shortens a resolved asset identifier to its display
// code: "native" -> "XLM"; "CODE-ISSUER" / "CODE:GISSUER" (both
// separators occur — SACClassicAssetName's raw METADATA name is
// colon-separated on-wire SEP-11 form, distinct from this API's usual
// dash-separated canonical asset_id; resolveSEP41MovementAsset passes
// SACClassicAssetName's value straight through, so this endpoint must
// handle both — see movements.go's "?asset= FILTER ASYMMETRY" doc
// comment for the same fact documented on the sibling endpoint) ->
// "CODE"; anything else (a raw, unresolved C-strkey) -> a truncated
// contract id, matching v1/protocol_pairs.go's truncContract fallback.
func assetDisplayLabel(resolved string) string {
	if resolved == "" {
		return ""
	}
	if resolved == "native" {
		return "XLM"
	}
	if idx := strings.IndexAny(resolved, "-:"); idx > 0 {
		return resolved[:idx]
	}
	return truncPositionsContract(resolved)
}

// truncPositionsContract renders "CAS3…OWMA" for an unresolvable
// contract id, mirroring v1/protocol_pairs.go's truncContract (a
// private duplicate rather than a cross-package import — see this
// file's PoolTokensReader doc comment for why this package doesn't
// import v1).
func truncPositionsContract(c string) string {
	if len(c) <= 8 {
		return c
	}
	return c[:4] + "…" + c[len(c)-4:]
}

// poolTokensFor best-effort loads source's pool -> token-contract map
// (empty map on a nil reader / read error — every caller already
// treats a missing key as "no label available"). Not memoized across
// the six buildX* calls in AccountPositions: each protocol calls this
// AT MOST ONCE per request (once for "blend" from two different
// builders — money-market + backstop legitimately share the same
// lending-pool reserve set — memoizing that one cross-builder case
// isn't worth a request-scoped cache for an endpoint bounded by a
// single address's small fold result).
func (h *Handler) poolTokensFor(ctx context.Context, source string) map[string][]string {
	if h.PoolTokens == nil {
		return nil
	}
	m, err := h.PoolTokens.PoolTokens(ctx, source)
	if err != nil {
		h.Logger.Warn("positions pool-tokens read failed", "source", source, "err", err)
		return nil
	}
	return m
}

// joinAssetLabels renders a pool's token set as a "/"-joined human pair
// ("XLM/USDC"), each token individually resolved+shortened via resolve.
// Empty when tokens is empty (no PoolTokens entry for this venue) —
// callers leave VenueLabel/Assets absent rather than emit an empty
// string.
func joinAssetLabels(tokens []string, resolve func(string) string) []string {
	if len(tokens) == 0 {
		return nil
	}
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if lbl := resolve(t); lbl != "" {
			out = append(out, lbl)
		}
	}
	return out
}

func isZeroDecimal(s string) bool {
	return s == "" || s == "0"
}

func fmtActivity(ledger uint32, t time.Time) PositionLastActivity {
	if t.IsZero() {
		return PositionLastActivity{Ledger: ledger}
	}
	return PositionLastActivity{Ledger: ledger, Time: t.UTC().Format(time.RFC3339)}
}

// ─── per-protocol builders ────────────────────────────────────────────

// buildBlendPositions maps BlendPositionFold rows to up to two
// PositionEntry rows each (an independent lending_supply and/or
// lending_borrow leg — see timescale.BlendPositionFold's doc comment).
func (h *Handler) buildBlendPositions(ctx context.Context, address string, resolve func(string) string) []PositionEntry {
	rows, err := h.Positions.BlendPositionsByUser(ctx, address)
	if err != nil {
		h.Logger.Error("positions: blend read failed", "err", err, "account", address)
		return nil
	}
	poolTokens := h.poolTokensFor(ctx, "blend")

	out := make([]PositionEntry, 0, len(rows)*2)
	for _, row := range rows {
		venueLabel := strings.Join(joinAssetLabels(poolTokens[row.Pool], resolve), "/")
		assetLabel := resolve(row.Asset)
		var assets []string
		if assetLabel != "" {
			assets = []string{assetLabel}
		}
		if row.HasSupplyLeg {
			out = append(out, PositionEntry{
				Protocol:        "blend",
				PositionKind:    PositionKindLendingSupply,
				Venue:           row.Pool,
				VenueLabel:      venueLabel,
				Assets:          assets,
				Amount:          row.SupplyNet,
				AmountSemantics: AmountSemanticsNetUnderlying,
				LastActivity:    fmtActivity(row.SupplyLastLedger, row.SupplyLastActivity),
				Basis:           BasisEventDerived,
				closed:          isZeroDecimal(row.SupplyNet),
			})
		}
		if row.HasBorrowLeg {
			out = append(out, PositionEntry{
				Protocol:        "blend",
				PositionKind:    PositionKindLendingBorrow,
				Venue:           row.Pool,
				VenueLabel:      venueLabel,
				Assets:          assets,
				Amount:          row.BorrowNet,
				AmountSemantics: AmountSemanticsNetUnderlying,
				LastActivity:    fmtActivity(row.BorrowLastLedger, row.BorrowLastActivity),
				Basis:           BasisEventDerived,
				closed:          isZeroDecimal(row.BorrowNet),
			})
		}
	}
	return out
}

// buildBlendBackstopPositions maps BlendBackstopFold rows to
// backstop_shares PositionEntry rows. Assets is a fixed descriptive
// label, not a resolved contract — the blend_backstop_events table
// carries no LP-token contract id (see BlendBackstopFold's doc
// comment); the label text is the protocol's own documented backstop
// asset (internal/sources/blend_backstop/decode.go package doc:
// "the backstop token (BLND:USDC LP)"), not a guess.
func (h *Handler) buildBlendBackstopPositions(ctx context.Context, address string, resolve func(string) string) []PositionEntry {
	rows, err := h.Positions.BlendBackstopSharesByUser(ctx, address)
	if err != nil {
		h.Logger.Error("positions: blend_backstop read failed", "err", err, "account", address)
		return nil
	}
	poolTokens := h.poolTokensFor(ctx, "blend")

	out := make([]PositionEntry, 0, len(rows))
	for _, row := range rows {
		reserveLabel := strings.Join(joinAssetLabels(poolTokens[row.Pool], resolve), "/")
		venueLabel := reserveLabel
		if venueLabel != "" {
			venueLabel += " backstop"
		}
		out = append(out, PositionEntry{
			Protocol:        "blend",
			PositionKind:    PositionKindBackstopShares,
			Venue:           row.Pool,
			VenueLabel:      venueLabel,
			Assets:          []string{"BLND:USDC (backstop token)"},
			Amount:          row.SharesNet,
			AmountSemantics: AmountSemanticsShares,
			LastActivity:    fmtActivity(row.LastLedger, row.LastActivity),
			Basis:           BasisEventDerived,
			closed:          isZeroDecimal(row.SharesNet),
		})
	}
	return out
}

// buildPhoenixStakePositions maps PhoenixStakeFold rows to `stake`
// PositionEntry rows. No PoolTokens entry keys on stake_contract (that
// map is pool-contract-keyed for Phoenix, not stake-contract-keyed —
// see pool_tokens.go's phoenixPoolTokens), so VenueLabel is left
// absent — a documented best-effort gap, not a bug.
func (h *Handler) buildPhoenixStakePositions(ctx context.Context, address string, resolve func(string) string) []PositionEntry {
	rows, err := h.Positions.PhoenixStakeByUser(ctx, address)
	if err != nil {
		h.Logger.Error("positions: phoenix_stake read failed", "err", err, "account", address)
		return nil
	}
	out := make([]PositionEntry, 0, len(rows))
	for _, row := range rows {
		var assets []string
		if lbl := resolve(row.LPToken); lbl != "" {
			assets = []string{lbl}
		}
		out = append(out, PositionEntry{
			Protocol:        "phoenix",
			PositionKind:    PositionKindStake,
			Venue:           row.StakeContract,
			Assets:          assets,
			Amount:          row.NetAmount,
			AmountSemantics: AmountSemanticsShares,
			LastActivity:    fmtActivity(row.LastLedger, row.LastActivity),
			Basis:           BasisEventDerived,
			closed:          isZeroDecimal(row.NetAmount),
		})
	}
	return out
}

// buildDefindexPositions maps DefindexVaultFold rows to `vault_shares`
// PositionEntry rows. defindex_flows carries no per-asset contract
// addresses for the vault layer (only amounts_vec, a positional numeric
// array with no identity — migration 0050), so Assets is left absent
// rather than guessed; no PoolTokens entry exists for defindex either
// (pool_tokens.go's documented gap), so VenueLabel stays absent too.
func (h *Handler) buildDefindexPositions(ctx context.Context, address string) []PositionEntry {
	rows, err := h.Positions.DefindexVaultSharesByUser(ctx, address)
	if err != nil {
		h.Logger.Error("positions: defindex read failed", "err", err, "account", address)
		return nil
	}
	out := make([]PositionEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, PositionEntry{
			Protocol:        "defindex",
			PositionKind:    PositionKindVaultShares,
			Venue:           row.ContractID,
			Amount:          row.SharesNet,
			AmountSemantics: AmountSemanticsShares,
			LastActivity:    fmtActivity(row.LastLedger, row.LastActivity),
			Basis:           BasisEventDerived,
			closed:          isZeroDecimal(row.SharesNet),
		})
	}
	return out
}

// buildCreditPositions maps CreditPositionFold rows to `credit`
// PositionEntry rows — the one STATEFUL protocol in this endpoint (see
// timescale.CreditPositionFold's doc comment). Assets is hardcoded to
// ["USDC"] — sorocredit's README: "The protocol runs its own USDC
// credit book (verified independent — not a wrapper)" — a documented
// protocol fact, not a per-position resolution (credit_statements
// carries no distinct debt-asset address; the whole book is USDC).
//
// "Closed" here is Withdrawn (a Withdrawal event observed against this
// position), NOT a zero-amount check — a freshly opened position with
// no statement published yet legitimately has no reportable amount
// (LatestAmount == "") without being closed; reporting "0" for it would
// misrepresent "unknown yet" as "verified zero".
func (h *Handler) buildCreditPositions(ctx context.Context, address string) []PositionEntry {
	rows, err := h.Positions.CreditPositionsByOwner(ctx, address)
	if err != nil {
		h.Logger.Error("positions: sorocredit read failed", "err", err, "account", address)
		return nil
	}
	out := make([]PositionEntry, 0, len(rows))
	for _, row := range rows {
		amount := row.LatestAmount
		activityLedger, activityTime := row.LatestLedger, row.LatestActivity
		if activityTime.IsZero() {
			// No statement published yet — fall back to the position-open
			// event as the last known activity; amount stays "" (unknown,
			// not a verified zero).
			activityLedger, activityTime = row.OpenedLedger, row.OpenedAt
		}
		if amount == "" {
			amount = "0"
		}
		out = append(out, PositionEntry{
			Protocol:        "sorocredit",
			PositionKind:    PositionKindCredit,
			Venue:           row.CollateralContract,
			Assets:          []string{"USDC"},
			Amount:          amount,
			AmountSemantics: AmountSemanticsStatefulCurrent,
			LastActivity:    fmtActivity(activityLedger, activityTime),
			Basis:           BasisStateful,
			closed:          row.Withdrawn,
		})
	}
	return out
}

// buildAquariusGaugePositions maps AquariusGaugeFold rows to `gauge`
// PositionEntry rows. Venue = the pool contract id — the SAME key
// pool_tokens.go's aquariusPoolTokens uses, so PoolTokens("aquarius")
// resolves directly (unlike Phoenix stake / DeFindex vault, no
// cross-mapping needed here).
func (h *Handler) buildAquariusGaugePositions(ctx context.Context, address string, resolve func(string) string) []PositionEntry {
	rows, err := h.Positions.AquariusGaugeByUser(ctx, address)
	if err != nil {
		h.Logger.Error("positions: aquarius_rewards read failed", "err", err, "account", address)
		return nil
	}
	poolTokens := h.poolTokensFor(ctx, "aquarius")

	out := make([]PositionEntry, 0, len(rows))
	for _, row := range rows {
		assets := joinAssetLabels(poolTokens[row.ContractID], resolve)
		venueLabel := strings.Join(assets, "/")
		out = append(out, PositionEntry{
			Protocol:        "aquarius",
			PositionKind:    PositionKindGauge,
			Venue:           row.ContractID,
			VenueLabel:      venueLabel,
			Assets:          assets,
			Amount:          row.NetDelta,
			AmountSemantics: AmountSemanticsSignedDeltaSum,
			LastActivity:    fmtActivity(row.LastLedger, row.LastActivity),
			Basis:           BasisEventDerived,
			closed:          isZeroDecimal(row.NetDelta),
		})
	}
	return out
}
