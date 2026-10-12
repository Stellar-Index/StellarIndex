package explorer

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// This file serves GET /v1/accounts/{g_strkey}/positions, the "DeFi positions" view across every
// ADR-0035-gated protocol this repo folds a per-user position out of (x-stability: experimental).
//
// Six independent folds live in internal/storage/timescale (SQL plus per-source amount-semantics
// evidence); this file only maps fold rows onto the shared wire shape, resolves display labels
// best-effort, and applies include_closed. It never computes a position amount itself.

// PositionsReader is the narrow Postgres read seam (the six per-protocol fold queries), satisfied by *timescale.Store; nil disables the endpoint (503).
type PositionsReader interface {
	BlendPositionsByUser(ctx context.Context, address string) ([]timescale.BlendPositionFold, error)
	BlendBackstopSharesByUser(ctx context.Context, address string) ([]timescale.BlendBackstopFold, error)
	PhoenixStakeByUser(ctx context.Context, address string) ([]timescale.PhoenixStakeFold, error)
	DefindexVaultSharesByUser(ctx context.Context, address string) ([]timescale.DefindexVaultFold, error)
	CreditPositionsByOwner(ctx context.Context, address string) ([]timescale.CreditPositionFold, error)
	AquariusGaugeByUser(ctx context.Context, address string) ([]timescale.AquariusGaugeFold, error)
}

// PoolTokensReader mirrors v1's ProtocolPoolTokensReader (a copy, since v1 imports explorer); nil degrades venue_label to absent and never fails the endpoint.
type PoolTokensReader interface {
	PoolTokens(ctx context.Context, source string) (map[string][]string, error)
}

// Position-kind, amount-semantics, and basis wire vocabularies for AccountPositionsView.
// amount_semantics is OUTPUT-only (never a filter the server 400s on) and gains values as folds
// are added.
const (
	PositionKindLendingSupply  = "lending_supply"
	PositionKindLendingBorrow  = "lending_borrow"
	PositionKindBackstopShares = "backstop_shares"
	PositionKindStake          = "stake"
	PositionKindVaultShares    = "vault_shares"
	PositionKindCredit         = "credit"
	PositionKindGauge          = "gauge"

	// AmountSemanticsNetUnderlying is a sum of historical per-event UNDERLYING-asset amounts
	// (never b/d-token or share amounts); it does NOT reflect accrual since each event. Blend
	// supply/borrow.
	AmountSemanticsNetUnderlying = "net_underlying_at_event_time"
	// AmountSemanticsShares is an exact current count of a protocol's own share/LP token (Blend
	// backstop, Phoenix stake, DeFindex vault), with no accrual ambiguity.
	AmountSemanticsShares = "shares"
	// AmountSemanticsStatefulCurrent is the protocol's own last PUBLISHED figure (sorocredit).
	// Its unit is not contract-source-confirmed, so it is served raw, never rescaled or valued.
	AmountSemanticsStatefulCurrent = "stateful_current_unconfirmed_unit"
	// AmountSemanticsSignedDeltaSum is a sum of signed per-event deltas whose unit is not
	// contract-source-confirmed (best-effort field mapping). Aquarius gauge.
	AmountSemanticsSignedDeltaSum = "signed_delta_sum_unconfirmed_unit"
	// AmountSemanticsSupersededByAuction marks a Blend leg moved by a liquidation, bad-debt
	// auction or write-off. Those moves carry no underlying amount, so amount is "" (unknown)
	// unless the b/d-token balance is exactly zero, when it is "0" and the leg is closed.
	AmountSemanticsSupersededByAuction = "superseded_by_auction"
	// AmountSemanticsNotYetPublished is a stateful position with no published figure yet: amount
	// is "" (unknown, not a verified zero). sorocredit.
	AmountSemanticsNotYetPublished = "not_yet_published"

	// BasisEventDerived is computed here by summing the fold's own event log.
	BasisEventDerived = "event_derived"
	// BasisStateful is read from a field the protocol itself last published.
	BasisStateful = "stateful"
)

// positionsHonestNote is always present: every position is a raw on-chain quantity, never a
// valuation, and event_derived positions never model accrual.
const positionsHonestNote = "Amounts are on-chain quantities only — no USD or other valuation is applied. " +
	"event_derived positions are a sum of historical per-event amounts and do NOT model interest, fees, " +
	"or exchange-rate accrual since each event, and a Blend leg a liquidation or bad-debt auction has moved " +
	"carries no net (superseded_by_auction); see each position's amount_semantics for exactly what the " +
	"number represents, and basis for whether it was derived here or read from the protocol's own published state."

// PositionLastActivity is the (ledger, time) of the most recent event this fold's evidence shows a position unchanged since.
type PositionLastActivity struct {
	Ledger uint32 `json:"ledger"`
	Time   string `json:"time"`
}

// PositionEntry is one row in the wire response for GET /v1/accounts/{g_strkey}/positions.
type PositionEntry struct {
	Protocol        string   `json:"protocol"`
	PositionKind    string   `json:"position_kind"`
	Venue           string   `json:"venue"`
	VenueLabel      string   `json:"venue_label,omitempty"`
	Assets          []string `json:"assets,omitempty"`
	Amount          string   `json:"amount"`
	AmountSemantics string   `json:"amount_semantics"`
	// Decimals scales Amount to whole units; set only where the unit is a
	// known token (net_underlying_at_event_time) and its scale was read.
	Decimals     *int                 `json:"decimals,omitempty"`
	LastActivity PositionLastActivity `json:"last_activity"`
	Basis        string               `json:"basis"`

	// closed is this fold's own net-zero/closed verdict — never
	// marshaled. Default include_closed=false drops these rows.
	closed bool
}

// AccountPositionsView is the wire response for GET /v1/accounts/{g_strkey}/positions.
type AccountPositionsView struct {
	Account       string          `json:"account"`
	Positions     []PositionEntry `json:"positions"`
	IncludeClosed bool            `json:"include_closed"`
	Note          string          `json:"note"`
	// CoverageNote is non-empty when one or more of the six per-protocol folds could NOT be read.
	// Each builder returns an empty slice on a read error, so without this a response with failed
	// reads would be byte-identical to "this account holds no positions". The static
	// [positionsHonestNote] says nothing about coverage and cannot carry this.
	CoverageNote string `json:"coverage_note,omitempty"`
}

// positionsCoverage collects the per-protocol read failures behind one
// request so the response can say which folds are missing rather than
// pass their absence off as an empty position set.
type positionsCoverage struct {
	degraded []string
	// labelsUnresolved: a pool-token read failed, so some rows lack their
	// venue/asset labels — the positions are complete, the display is not.
	labelsUnresolved bool
}

// fail records that `protocol`'s fold could not be read. Safe on a nil
// receiver so a builder called outside AccountPositions (tests, future
// callers) doesn't have to thread a collector it doesn't want.
func (c *positionsCoverage) fail(protocol string) {
	if c == nil {
		return
	}
	c.degraded = append(c.degraded, protocol)
}

// note renders the wire coverage_note, or "" when every fold ran.
func (c *positionsCoverage) note() string {
	if c == nil || len(c.degraded) == 0 {
		return ""
	}
	return "INCOMPLETE: " + strconv.Itoa(len(c.degraded)) + " of " +
		strconv.Itoa(positionsProtocolFolds) + " protocol reads failed (" +
		strings.Join(c.degraded, ", ") + "); positions from those protocols are MISSING " +
		"from this response, not proven absent on-chain — retry before treating this list as complete."
}

// positionsProtocolFolds is the number of per-protocol folds AccountPositions runs, and the
// denominator in coverage_note. Adding a fold means bumping it:
// TestExplorer_AccountPositions_TotalReadFailureIsNotAnEmptyAccount fails on the mismatch.
const positionsProtocolFolds = 6

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

// collectPositions folds the six ADR-0035-gated protocols' event tables into one per-(venue,
// kind) net-position list. Not paginated: each fold is capped by timescale's positionsVenueLimit.
//
// "Net-zero" (closed) differs per protocol (see each buildXPositions helper): a delta-summed net
// of exactly 0 for event_derived protocols, a Withdrawal-observed flag for sorocredit.
func (h *Handler) collectPositions(ctx context.Context, g string, resolve positionAssetResolver, cov *positionsCoverage) []PositionEntry {
	// The folds are INDEPENDENT Postgres reads, run concurrently so latency is their MAX, not
	// their SUM. Each writes its own slot (positions + coverage), merged afterwards in fixed fold
	// order, so output including coverage_note ordering is byte-identical to a serial run; per-fold
	// collectors also avoid a data race on the plain append to positionsCoverage.degraded.
	folds := []func(*positionsCoverage) []PositionEntry{
		func(c *positionsCoverage) []PositionEntry { return h.buildBlendPositions(ctx, g, resolve, c) },
		func(c *positionsCoverage) []PositionEntry { return h.buildBlendBackstopPositions(ctx, g, resolve, c) },
		func(c *positionsCoverage) []PositionEntry { return h.buildPhoenixStakePositions(ctx, g, resolve, c) },
		func(c *positionsCoverage) []PositionEntry { return h.buildDefindexPositions(ctx, g, c) },
		func(c *positionsCoverage) []PositionEntry { return h.buildCreditPositions(ctx, g, c) },
		func(c *positionsCoverage) []PositionEntry { return h.buildAquariusGaugePositions(ctx, g, resolve, c) },
	}
	slots := make([][]PositionEntry, len(folds))
	covs := make([]positionsCoverage, len(folds))
	var wg sync.WaitGroup
	for i, fold := range folds {
		wg.Add(1)
		go func(i int, fold func(*positionsCoverage) []PositionEntry) {
			defer wg.Done()
			// A panic in one fold must not take down the process: this runs off the handler
			// goroutine, so middleware.Recoverer does not cover it. Degrade that fold instead.
			defer func() {
				if rec := recover(); rec != nil {
					// worker.Report moves the panic counter; fold index and account stay in the
					// local log line because the metric label must remain a bounded constant.
					// covs[i].fail keeps the response HONEST: the coverage note discloses the
					// missing fold rather than silently under-reporting positions.
					worker.Report(h.Logger, "explorer-account-positions-fold", rec)
					h.Logger.Error("explorer AccountPositions fold panicked",
						"fold", i, "account", g)
					covs[i].fail("panic")
				}
			}()
			slots[i] = fold(&covs[i])
		}(i, fold)
	}
	wg.Wait()

	var positions []PositionEntry
	for i := range slots {
		positions = append(positions, slots[i]...)
		cov.degraded = append(cov.degraded, covs[i].degraded...)
		cov.labelsUnresolved = cov.labelsUnresolved || covs[i].labelsUnresolved
	}
	return positions
}

// accountPositionsSnapshot is the cached payload behind AccountPositions: the UNFILTERED fold
// output plus its coverage note. include_closed is applied at serve time so both variants share
// one cache entry.
//
// A DEGRADED snapshot IS cached: it travels with its coverage_note so it is never passed off as
// complete, and refusing to cache would make every request during an outage pay the full
// six-fold latency and still be degraded.
type accountPositionsSnapshot struct {
	positions        []PositionEntry
	coverageNote     string
	labelsUnresolved bool
}

// AccountPositions serves GET /v1/accounts/{g_strkey}/positions, excluding closed positions unless ?include_closed=true; SWR-cached under the same contract as AccountActivity.
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

	v, asOf, degraded, err := h.contractDetailCached(ctx, positionsCacheKey+g, func(rctx context.Context) (any, error) {
		// The resolver and folds must run on the REFRESH context: binding either to a request that
		// may be gone would silently produce a half-resolved payload.
		var cov positionsCoverage
		positions := h.collectPositions(rctx, g, h.newPositionAssetResolver(rctx), &cov)
		return accountPositionsSnapshot{positions: positions, coverageNote: cov.note(), labelsUnresolved: cov.labelsUnresolved}, nil
	})
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if retryableColdMiss(ctx, err) {
			h.Logger.Warn("explorer AccountPositions deadline exceeded", "account", g)
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/account-positions-timeout",
				"Account positions timed out")
			return
		}
		h.Logger.Error("explorer AccountPositions failed", "err", err, "account", g)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	snap, ok := v.(accountPositionsSnapshot)
	if !ok {
		h.Logger.Error("explorer AccountPositions: unexpected cached payload type", "account", g)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	out := make([]PositionEntry, 0, len(snap.positions))
	for _, p := range snap.positions {
		if !includeClosed && p.closed {
			continue
		}
		out = append(out, p)
	}

	h.writeJSONAt(w, AccountPositionsView{
		Account:       g,
		Positions:     out,
		IncludeClosed: includeClosed,
		Note:          positionsHonestNote,
		CoverageNote:  snap.coverageNote,
	}, degraded, degraded || snap.coverageNote != "" || snap.labelsUnresolved, asOf)
}

// ─── asset / venue display-label resolution ──────────────────────────

// newPositionAssetResolver returns a per-request memoized contract-id -> display-label resolver
// built on resolveSEP41MovementAsset (SAC-then-raw-contract-id). It never fails: an unresolvable
// contract degrades to a truncated id.
//
// This endpoint's hard dependency is h.Positions (Postgres), NOT h.Reader (the ClickHouse lake),
// and resolveSEP41MovementAsset has no nil-Reader guard, so without a Reader the label degrades
// to the raw contract id instead of calling through a nil interface.
func (h *Handler) newPositionAssetResolver(ctx context.Context) positionAssetResolver {
	// MUST be concurrency-safe: the parallel folds share this resolver, and concurrent map writes
	// are a FATAL runtime throw that no recover() catches. The lock is held across the resolve
	// (which may hit the lake) on purpose, so a contract is resolved once per request, not per fold.
	var mu sync.Mutex
	cache := map[string]string{}
	return func(contractID string) string {
		if contractID == "" {
			return ""
		}
		mu.Lock()
		defer mu.Unlock()
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

// positionAssetResolver maps a contract id to its display label,
// memoised per request. Safe for concurrent use (see
// newPositionAssetResolver).
type positionAssetResolver = func(contractID string) string

// assetDisplayLabel shortens a resolved asset identifier to a display code: "native" -> "XLM";
// "CODE-ISSUER" or "CODE:GISSUER" -> "CODE" (both separators occur: SAC metadata names are the
// colon-separated SEP-11 form, passed through unchanged by resolveSEP41MovementAsset); anything
// else (a raw C-strkey) -> a truncated contract id. The label is display-only, never identity.
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

// truncPositionsContract renders "CAS3…OWMA" for an unresolvable contract id. It duplicates v1's
// truncContract because this package cannot import v1 (see PoolTokensReader).
func truncPositionsContract(c string) string {
	if len(c) <= 8 {
		return c
	}
	return c[:4] + "…" + c[len(c)-4:]
}

// poolTokensFor best-effort loads source's pool -> token-contract map. A nil reader or read error
// yields no map (callers treat a missing key as "no label"); a read error is also recorded on
// cov. Production wiring is TTL-cached per source in package v1.
func (h *Handler) poolTokensFor(ctx context.Context, source string, cov *positionsCoverage) map[string][]string {
	if h.PoolTokens == nil {
		return nil
	}
	m, err := h.PoolTokens.PoolTokens(ctx, source)
	if err != nil {
		h.Logger.Warn("positions pool-tokens read failed", "source", source, "err", err)
		if cov != nil {
			cov.labelsUnresolved = true
		}
		return nil
	}
	return m
}

// joinAssetLabels resolves each pool token to its display label. It returns nil when tokens is
// empty, so callers leave VenueLabel/Assets absent rather than emit an empty string.
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
func (h *Handler) buildBlendPositions(ctx context.Context, address string, resolve func(string) string, cov *positionsCoverage) []PositionEntry {
	rows, err := h.Positions.BlendPositionsByUser(ctx, address)
	if err != nil {
		h.Logger.Error("positions: blend read failed", "err", err, "account", address)
		cov.fail("blend")
		return nil
	}
	poolTokens := h.poolTokensFor(ctx, "blend", cov)
	dctx, cancel := context.WithTimeout(ctx, movementDecimalsBudget)
	defer cancel()
	scale := h.assetScales(dctx)
	withScale := func(e PositionEntry, asset string) PositionEntry {
		if e.AmountSemantics == AmountSemanticsNetUnderlying {
			e.Decimals = scale(asset)
		}
		return e
	}

	out := make([]PositionEntry, 0, len(rows)*2)
	for _, row := range rows {
		venueLabel := strings.Join(joinAssetLabels(poolTokens[row.Pool], resolve), "/")
		assetLabel := resolve(row.Asset)
		var assets []string
		if assetLabel != "" {
			assets = []string{assetLabel}
		}
		if row.HasSupplyLeg {
			out = append(out, withScale(blendLegEntry(PositionEntry{
				Protocol:     "blend",
				PositionKind: PositionKindLendingSupply,
				Venue:        row.Pool,
				VenueLabel:   venueLabel,
				Assets:       assets,
				LastActivity: fmtActivity(row.SupplyLastLedger, row.SupplyLastActivity),
				Basis:        BasisEventDerived,
			}, row.SupplyNet, row.SupplySuperseded, row.SupplyTokens), row.Asset))
		}
		if row.HasBorrowLeg {
			out = append(out, withScale(blendLegEntry(PositionEntry{
				Protocol:     "blend",
				PositionKind: PositionKindLendingBorrow,
				Venue:        row.Pool,
				VenueLabel:   venueLabel,
				Assets:       assets,
				LastActivity: fmtActivity(row.BorrowLastLedger, row.BorrowLastActivity),
				Basis:        BasisEventDerived,
			}, row.BorrowNet, row.BorrowSuperseded, row.BorrowTokens), row.Asset))
		}
	}
	return out
}

// blendLegEntry sets a Blend leg's amount. A superseded leg's underlying
// net omits the auctioned b/d-tokens, so it serves no amount; only an
// exactly-zero token balance (fully seized or repaid) is a known zero.
func blendLegEntry(e PositionEntry, net string, superseded bool, tokens string) PositionEntry {
	if superseded {
		e.AmountSemantics = AmountSemanticsSupersededByAuction
		if tokens == "0" { // strict: an unread balance ("") is not a known zero
			e.Amount = "0"
			e.closed = true
		}
		return e
	}
	e.Amount = net
	e.AmountSemantics = AmountSemanticsNetUnderlying
	e.closed = isZeroDecimal(net)
	return e
}

// buildBlendBackstopPositions maps BlendBackstopFold rows to backstop_shares entries. Assets is a
// fixed descriptive label, not a resolved contract: blend_backstop_events carries no LP-token
// contract id, and the label is the protocol's documented backstop token (BLND:USDC LP).
func (h *Handler) buildBlendBackstopPositions(ctx context.Context, address string, resolve func(string) string, cov *positionsCoverage) []PositionEntry {
	rows, err := h.Positions.BlendBackstopSharesByUser(ctx, address)
	if err != nil {
		h.Logger.Error("positions: blend_backstop read failed", "err", err, "account", address)
		cov.fail("blend_backstop")
		return nil
	}
	poolTokens := h.poolTokensFor(ctx, "blend", cov)

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

// buildPhoenixStakePositions maps PhoenixStakeFold rows to `stake` entries. The PoolTokens map is
// pool-keyed for Phoenix, not stake-contract-keyed, so VenueLabel is left absent (a known
// best-effort gap).
func (h *Handler) buildPhoenixStakePositions(ctx context.Context, address string, resolve func(string) string, cov *positionsCoverage) []PositionEntry {
	rows, err := h.Positions.PhoenixStakeByUser(ctx, address)
	if err != nil {
		h.Logger.Error("positions: phoenix_stake read failed", "err", err, "account", address)
		cov.fail("phoenix_stake")
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

// buildDefindexPositions maps DefindexVaultFold rows to `vault_shares` entries. defindex_flows
// has only a positional amounts_vec with no asset identity, so Assets is left absent rather than
// guessed; there is no PoolTokens entry for defindex either, so VenueLabel stays absent too.
func (h *Handler) buildDefindexPositions(ctx context.Context, address string, cov *positionsCoverage) []PositionEntry {
	rows, err := h.Positions.DefindexVaultSharesByUser(ctx, address)
	if err != nil {
		h.Logger.Error("positions: defindex read failed", "err", err, "account", address)
		cov.fail("defindex")
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

// buildCreditPositions maps CreditPositionFold rows to `credit` entries, the one STATEFUL
// protocol here. Assets is hardcoded to ["USDC"] because sorocredit runs a single USDC book and
// credit_statements carries no debt-asset address.
//
// "Closed" is Withdrawn (a Withdrawal event observed), NOT a zero-amount check: a freshly opened
// position with no statement yet has amount "" and not_yet_published, since "0" would
// misrepresent "unknown yet" as "verified zero".
func (h *Handler) buildCreditPositions(ctx context.Context, address string, cov *positionsCoverage) []PositionEntry {
	rows, err := h.Positions.CreditPositionsByOwner(ctx, address)
	if err != nil {
		h.Logger.Error("positions: sorocredit read failed", "err", err, "account", address)
		cov.fail("sorocredit")
		return nil
	}
	out := make([]PositionEntry, 0, len(rows))
	for _, row := range rows {
		activityLedger, activityTime := row.LatestLedger, row.LatestActivity
		if activityTime.IsZero() {
			// No statement published yet — fall back to the position-open
			// event as the last known activity.
			activityLedger, activityTime = row.OpenedLedger, row.OpenedAt
		}
		semantics := AmountSemanticsStatefulCurrent
		if row.LatestAmount == "" {
			semantics = AmountSemanticsNotYetPublished
		}
		out = append(out, PositionEntry{
			Protocol:        "sorocredit",
			PositionKind:    PositionKindCredit,
			Venue:           row.CollateralContract,
			Assets:          []string{"USDC"},
			Amount:          row.LatestAmount,
			AmountSemantics: semantics,
			LastActivity:    fmtActivity(activityLedger, activityTime),
			Basis:           BasisStateful,
			closed:          row.Withdrawn,
		})
	}
	return out
}

// buildAquariusGaugePositions maps AquariusGaugeFold rows to `gauge` entries. Venue is the pool
// contract id, the same key aquariusPoolTokens uses, so PoolTokens("aquarius") resolves directly.
func (h *Handler) buildAquariusGaugePositions(ctx context.Context, address string, resolve func(string) string, cov *positionsCoverage) []PositionEntry {
	rows, err := h.Positions.AquariusGaugeByUser(ctx, address)
	if err != nil {
		h.Logger.Error("positions: aquarius_rewards read failed", "err", err, "account", address)
		cov.fail("aquarius_rewards")
		return nil
	}
	poolTokens := h.poolTokensFor(ctx, "aquarius", cov)

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
