package explorer

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// SEP41MovementsReader is the narrow Postgres read seam AccountMovements
// needs for the sep41_transfers "recent tail" half of the ADR-0048 D5
// merge. *timescale.Store satisfies it via ListSEP41TransfersByAddress.
// Nil disables the PG-side contribution (the endpoint still serves the
// ClickHouse pre-P23 archive alone, with an honest coverage_note — see
// AccountMovements below).
type SEP41MovementsReader interface {
	ListSEP41TransfersByAddress(ctx context.Context, address string, limit int, cur timescale.SEP41TransferCursor, direction string, floorLedger uint32) ([]timescale.SEP41TransferRow, error)
}

// AccountMovementEntry is one row in the wire response for GET
// /v1/accounts/{g_strkey}/movements (ADR-0048 D5). Amount is a string
// (ADR-0003 — i128 exceeds IEEE 754 double precision above 2^53).
type AccountMovementEntry struct {
	Ledger          uint32         `json:"ledger"`
	LedgerCloseTime string         `json:"ledger_close_time"`
	TxHash          string         `json:"tx_hash"`
	OpIndex         uint32         `json:"op_index"`
	LegIndex        uint32         `json:"leg_index"`
	MovementKind    string         `json:"movement_kind"`
	Direction       string         `json:"direction"`
	Asset           string         `json:"asset"`
	Amount          string         `json:"amount"`
	Counterparty    string         `json:"counterparty,omitempty"`
	Provenance      string         `json:"provenance"`
	Attributes      map[string]any `json:"attributes,omitempty"`
}

// AccountMovementsView is the wire response for GET
// /v1/accounts/{g_strkey}/movements.
type AccountMovementsView struct {
	Account    string                 `json:"account"`
	Movements  []AccountMovementEntry `json:"movements"`
	NextCursor string                 `json:"next_cursor,omitempty"`
	// CoverageNote is an honest-degrade signal (mirrors routed_via /
	// aggregators' coverage notes): non-empty when this response is
	// NOT the full ADR-0048 D5 merge — either the ClickHouse pre-P23
	// archive hasn't been backfilled yet (classic-movements-backfill
	// is a historical-only, operator-run job — CH starts EMPTY on a
	// fresh deployment) or the Postgres recent-tail reader isn't
	// wired / errored on this request. Absent = the full merge ran.
	CoverageNote string `json:"coverage_note,omitempty"`
}

// accountMovementsDefaultLimit / accountMovementsMaxLimit — ADR-0048
// D5's stated pagination contract.
const (
	accountMovementsDefaultLimit = 25
	accountMovementsMaxLimit     = 200
)

// parseMovementFilter reads the optional ?kind= / ?direction= /
// ?asset= query params. direction, when present, must be one of
// clickhouse's three AccountMovementDirection values — ok=false
// (after a problem+json) otherwise. kind is free-form (matched as an
// exact-equality filter; an unrecognized kind is not an error, just a
// filter that matches nothing). asset is normalized to the canonical
// spelling the stores hold when it parses as an asset id, and passed
// through verbatim otherwise (the movements asset domain is wider
// than ParseAsset's: pool:<hex> legs, raw C… contract ids).
func (h *Handler) parseMovementFilter(w http.ResponseWriter, r *http.Request) (clickhouse.AccountMovementFilter, bool) {
	f := clickhouse.AccountMovementFilter{
		Kind:  r.URL.Query().Get("kind"),
		Asset: normalizeMovementAssetFilter(r.URL.Query().Get("asset")),
	}
	if dir := r.URL.Query().Get("direction"); dir != "" {
		switch clickhouse.AccountMovementDirection(dir) {
		case clickhouse.AccountMovementSent, clickhouse.AccountMovementReceived, clickhouse.AccountMovementSelf:
			f.Direction = clickhouse.AccountMovementDirection(dir)
		default:
			h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-parameter",
				"Invalid parameter", http.StatusBadRequest,
				"direction must be one of sent, received, self")
			return clickhouse.AccountMovementFilter{}, false
		}
	}
	return f, true
}

// normalizeMovementAssetFilter folds a ?asset= value to the canonical
// spelling the movement stores hold ("native" / "CODE-ISSUER"): the CH
// side filters `asset = ?` in SQL against canonical spellings, so
// "XLM", "crypto:XLM" and Horizon-style "USDC:G…" would otherwise
// exact-match nothing and serve an authoritative-looking empty feed
// (XLM dual-form rule; the AssetHolders handler normalizes for exactly
// this reason). Values that don't parse as an asset id pass through
// verbatim — the movements domain also filters on pool:<hex> legs and
// raw C… contract ids, which must not 400.
func normalizeMovementAssetFilter(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := canonical.ParseAsset(raw)
	if err != nil {
		return raw
	}
	if isNativeHoldersAsset(parsed) {
		return canonical.NativeAsset().String()
	}
	return parsed.String()
}

// movementCursorParts is the (ledger, tx_hash, op_index, leg_index)
// tuple ADR-0048 D5's opaque `?cursor=` encodes — the same tuple
// meaning on both sides of the merge (leg_index on the CH side maps
// 1:1 to event_index on the PG side; both are "sub-index within the
// op", the 4th ORDER BY column each store's own query keys off).
type movementCursorParts struct {
	Ledger   uint32
	TxHash   string
	OpIndex  uint32
	LegIndex uint32
	// PinnedWatermark is the cap67 archive watermark that produced the
	// FIRST page of this pagination sequence, carried in the cursor so
	// every continuation page reuses the SAME CH/PG arm boundary instead
	// of re-reading the live (advancing) watermark. Without it, a derive
	// advance mid-scroll moves the split under the cursor and silently
	// drops the native/unwatched sliver between the old and new watermark
	// (W1-chrollup-2). Valid only when HasPinnedWatermark is true (a
	// first-page request, or a legacy 4-segment cursor, has none).
	PinnedWatermark    uint32
	HasPinnedWatermark bool
}

func encodeMovementCursor(r clickhouse.AccountMovementRow, pinnedWatermark uint32) string {
	return fmt.Sprintf("%d.%s.%d.%d.%d", r.Ledger, r.TxHash, r.OpIndex, r.LegIndex, pinnedWatermark)
}

// parseMovementCursor decodes the opaque `?cursor=` — dotted-decimal
// with the tx_hash segment in the middle (safe: tx_hash is a fixed
// 64-char hex string, never contains '.'). A trailing 5th segment, when
// present, is the pinned cap67 watermark (W1-chrollup-2); a legacy
// 4-segment cursor carries none. ok=false (after a problem+json) on a
// malformed value.
func (h *Handler) parseMovementCursor(w http.ResponseWriter, r *http.Request) (movementCursorParts, bool) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return movementCursorParts{}, true
	}
	bad := func() (movementCursorParts, bool) {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-cursor",
			"Invalid cursor", http.StatusBadRequest,
			"cursor must be an opaque value returned in a prior next_cursor")
		return movementCursorParts{}, false
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 4 && len(parts) != 5 {
		return bad()
	}
	ledger, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil || ledger == 0 {
		return bad()
	}
	txHash := parts[1]
	if len(txHash) == 0 {
		return bad()
	}
	opIdx, err := strconv.ParseUint(parts[2], 10, 32)
	if err != nil {
		return bad()
	}
	legIdx, err := strconv.ParseUint(parts[3], 10, 32)
	if err != nil {
		return bad()
	}
	out := movementCursorParts{Ledger: uint32(ledger), TxHash: txHash, OpIndex: uint32(opIdx), LegIndex: uint32(legIdx)}
	if len(parts) == 5 {
		pinned, err := strconv.ParseUint(parts[4], 10, 32)
		if err != nil {
			return bad()
		}
		out.PinnedWatermark = uint32(pinned)
		out.HasPinnedWatermark = true
	}
	return out, true
}

// AccountMovements serves GET /v1/accounts/{g_strkey}/movements
// (ADR-0048 D5) — the unified account-activity feed, newest first,
// keyset-paged by the opaque composite (ledger, tx_hash, op_index,
// leg_index) cursor, with optional ?kind=/?direction=/?asset= filters.
//
// Merge seam: ClickHouse's stellar.account_movements (the pre-P23
// classic-movement archive, ADR-0047/0048 D2) covers every ledger
// BELOW classicmovements.P23StartLedger; Postgres' sep41_transfers
// 'transfer' rows (ADR-0048 D5's "recent tail",
// ListSEP41TransfersByAddress) cover every ledger AT OR ABOVE it. The
// two ranges cannot overlap by construction — assertP23NonOverlap
// checks that invariant on every request rather than only trusting
// the doc comment. Because the ranges never overlap, merging two
// DESC-sorted per-store pages degenerates to "drain whichever side's
// next row is newer", which mergeAccountMovementRows implements as a
// real two-pointer merge (not a special-cased concatenation) so the
// endpoint stays correct even if that invariant is ever violated by a
// future regression elsewhood.
//
// Honest empty-state: classic-movements-backfill is a historical-only,
// operator-run job (CLAUDE.md "Heavy one-shot jobs on r1"), so
// stellar.account_movements is EMPTY on every deployment until an
// operator runs it — before that, this endpoint serves only the
// Postgres tail, and CoverageNote says so explicitly rather than
// silently presenting a partial feed as complete.
func (h *Handler) AccountMovements(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	g, ok := h.parseAccountStrkey(w, r)
	if !ok {
		return
	}
	limit, ok := h.ParseLimit(w, r, accountMovementsDefaultLimit, accountMovementsMaxLimit)
	if !ok {
		return
	}
	cur, ok := h.parseMovementCursor(w, r)
	if !ok {
		return
	}
	filter, ok := h.parseMovementFilter(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	chCur := clickhouse.AccountMovementCursor{Ledger: cur.Ledger, TxHash: cur.TxHash, OpIndex: cur.OpIndex, LegIndex: cur.LegIndex}

	// Dynamic archive boundary (inventory #1): the cap67-derived
	// archive covers ALL assets through its watermark; the Postgres
	// tail covers watched tokens above it. ONE watermark value drives
	// BOTH the CH ceiling and the PG floor so the arms stay gap-free and
	// double-count-free even while the derive is mid-catch-up (rows
	// landing between the two reads are excluded from CH by the ceiling
	// and served by PG above the floor).
	//
	// The watermark advances every derive window (~minutes), so a
	// paginated scroll must NOT re-read the live value on each page — that
	// would move the CH/PG split under the cursor and silently drop the
	// native/unwatched sliver between the old and new boundary
	// (W1-chrollup-2). The first page reads the live watermark and pins it
	// into next_cursor; continuation pages reuse that pinned value, so the
	// whole sequence shares one boundary and one honest coverage note.
	var wm uint32
	if cur.HasPinnedWatermark {
		wm = cur.PinnedWatermark
	} else {
		liveWM, wmErr := h.Reader.Cap67MovementsWatermark(ctx)
		if wmErr != nil {
			// Fail closed (W1-chrollup-1): a watermark read error must NOT
			// disable the CH ceiling. The cap67 archive may be fully
			// populated to the tip, and serving it untrimmed alongside the
			// Postgres watched-token tail double-lists every post-P23
			// watched transfer. Fall back to the static P23 boundary (wm=0
			// => CH ceilinged at P23-1 below, PG floored at P23) so the arms
			// stay disjoint and only the post-P23 native sliver degrades to
			// watched-tokens-only — exactly the scope the wm==0 coverage
			// note already discloses.
			h.Logger.Warn("cap67 movements watermark read failed — failing closed to the static P23 boundary", "err", wmErr)
			liveWM = 0
		}
		wm = liveWM
	}

	chRows, err := h.Reader.AccountMovements(ctx, g, limit, chCur, filter)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if retryableColdMiss(ctx, err) {
			h.Logger.Warn("explorer AccountMovements deadline exceeded", "account", g)
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/account-movements-timeout",
				"Account movements timed out")
			return
		}
		h.Logger.Error("explorer AccountMovements (ClickHouse archive) failed", "err", err, "account", g)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	// Clamp the CH arm to its ceiling ALWAYS — not only when wm>0
	// (W1-chrollup-1). With a populated cap67 archive but a zero/failed
	// watermark, an unclamped CH arm returns cap67_derived rows across the
	// whole post-P23 range that the Postgres tail ALSO serves, double-
	// listing every post-P23 watched transfer. When the watermark is
	// unknown/absent the ceiling is the static P23 boundary
	// (SEP41MovementsFloorLedger-1): a no-op for a genuinely-absent archive
	// (no post-P23 rows exist) and a real trim for a populated one.
	chCeiling := timescale.SEP41MovementsFloorLedger - 1
	if wm > 0 {
		chCeiling = wm
	}
	trimmed := chRows[:0]
	for _, row := range chRows {
		if row.Ledger <= chCeiling {
			trimmed = append(trimmed, row)
		}
	}
	chRows = trimmed

	pgFloor := timescale.SEP41MovementsFloorLedger
	if wm+1 > pgFloor {
		pgFloor = wm + 1
	}
	pgRows, tailNote := h.fetchSEP41MovementsTail(ctx, g, limit, cur, filter, pgFloor)
	coverageNote := movementsCoverageNote(wm, tailNote)
	h.assertMovementsNonOverlap(chRows, pgRows, pgFloor)

	merged := mergeAccountMovementRows(chRows, pgRows, limit)
	out := AccountMovementsView{
		Account:      g,
		Movements:    make([]AccountMovementEntry, len(merged)),
		CoverageNote: coverageNote,
	}
	for i, m := range merged {
		out.Movements[i] = accountMovementEntryView(m)
	}
	if len(merged) == limit {
		// Pin the boundary this sequence committed to (the live watermark
		// on page 1, the already-pinned value on continuation pages) so
		// every subsequent page reuses it (W1-chrollup-2).
		out.NextCursor = encodeMovementCursor(merged[len(merged)-1], wm)
	}
	h.WriteJSON(w, out, false)
}

// fetchSEP41MovementsTail reads + maps the Postgres recent-tail half
// of the merge. Returns a non-empty coverageNote whenever the
// response is honestly incomplete: no reader wired, or the read
// itself failed (degrade, don't fail the whole request — the
// ClickHouse archive is still a valid, if recency-incomplete, answer).
// kind filters that structurally CANNOT match a PG-tail row (every
// synthesized row is movement_kind="transfer") short-circuit without a
// round-trip.
func (h *Handler) fetchSEP41MovementsTail(ctx context.Context, address string, limit int, cur movementCursorParts, filter clickhouse.AccountMovementFilter, floorLedger uint32) ([]clickhouse.AccountMovementRow, string) {
	if h.SEP41Movements == nil {
		return nil, "this deployment has not wired the recent (post-P23) Postgres tail reader; showing only the ClickHouse pre-P23 archive"
	}
	if filter.Kind != "" && filter.Kind != "transfer" {
		return nil, ""
	}
	pgCur := timescale.SEP41TransferCursor{Ledger: cur.Ledger, TxHash: cur.TxHash, OpIndex: cur.OpIndex, EventIndex: cur.LegIndex}
	rows, err := h.SEP41Movements.ListSEP41TransfersByAddress(ctx, address, limit, pgCur, string(filter.Direction), floorLedger)
	if err != nil {
		h.Logger.Error("explorer AccountMovements (Postgres recent tail) failed", "err", err, "account", address)
		return nil, "the recent (post-P23) tail is temporarily unavailable; showing the pre-P23 ClickHouse archive only"
	}
	return h.mapSEP41RowsToMovements(ctx, address, rows, filter.Asset), ""
}

// movementsCoverageNote is the feed's honest-scope statement, chosen by
// the cap67 archive watermark. tailNote (a tail wiring/availability
// failure) takes priority — it means the response is MISSING data
// beyond the structural scope.
//
// wm == 0: the cap67-derived archive (inventory #1) isn't provisioned —
// post-P23 coverage is the watched-token Postgres tail only, and
// classic XLM payment history after the boundary is absent. Saying so
// on EVERY response is what keeps a busy XLM account's feed from
// masquerading as complete (the GATL report, site audit 2026-08-08).
//
// wm > 0: all assets are covered through the watermark; only the sliver
// above it (the derive follows the tip on a ~5-minute timer) is
// watched-tokens-only.
func movementsCoverageNote(wm uint32, tailNote string) string {
	if tailNote != "" {
		return tailNote
	}
	if wm == 0 {
		return "movements after 2025-09-03 (P23) currently include watched Soroban/SAC tokens only — " +
			"classic XLM payment history after that date is not yet served on this feed " +
			"(the full-history movement archive is being extended); see /accounts/{g}/operations " +
			"for complete raw operation history"
	}
	return fmt.Sprintf("complete for all assets through ledger %d; more recent movements may include "+
		"watched Soroban/SAC tokens only while the archive follows the tip (~minutes)", wm)
}

// mapSEP41RowsToMovements converts sep41_transfers 'transfer' rows into
// the same clickhouse.AccountMovementRow shape the CH archive returns,
// address-relative (direction/counterparty computed against `address`,
// mirroring clickhouse.FanOutAccountMovement's sent/received/self
// rule), so mergeAccountMovementRows can operate on one uniform type.
//
// Asset resolution: sep41_transfers stores the raw token contract_id;
// the response's `asset` field should read the SAME canonical form CH
// rows use where possible. Resolves each row's contract_id through
// SACClassicAssetName then SACAssetFromEvents (deduped to one lookup
// per DISTINCT contract_id in this page — bounded by page size, not a
// per-row cost), falling back to the raw contract_id for a genuine
// Soroban-native token (no SAC wrapper). assetFilter, when non-empty,
// is applied HERE post-resolution (not in the SQL query — see
// timescale.ListSEP41TransfersByAddress's doc comment for why the two
// sides' asset-filter semantics are asymmetric): a page may therefore
// return fewer than `limit` PG-side rows even when more matching rows
// exist further back — an accepted, documented limitation of this
// experimental endpoint's Postgres tail.
func (h *Handler) mapSEP41RowsToMovements(ctx context.Context, address string, rows []timescale.SEP41TransferRow, assetFilter string) []clickhouse.AccountMovementRow {
	if len(rows) == 0 {
		return nil
	}
	assetNames := make(map[string]string, len(rows))
	out := make([]clickhouse.AccountMovementRow, 0, len(rows))
	for _, tr := range rows {
		asset, ok := assetNames[tr.ContractID]
		if !ok {
			asset = h.resolveSEP41MovementAsset(ctx, tr.ContractID)
			assetNames[tr.ContractID] = asset
		}
		if assetFilter != "" && asset != assetFilter {
			continue
		}
		row := clickhouse.AccountMovementRow{
			Address:         address,
			Ledger:          tr.Ledger,
			LedgerCloseTime: tr.ObservedAt,
			TxHash:          tr.TxHash,
			OpIndex:         tr.OpIndex,
			LegIndex:        tr.EventIndex,
			MovementKind:    "transfer",
			Provenance:      "cap67_event",
			Asset:           asset,
			Amount:          tr.Amount,
		}
		switch {
		case tr.FromAddr == address && tr.ToAddr == address:
			row.Direction = clickhouse.AccountMovementSelf
		case tr.FromAddr == address:
			row.Direction = clickhouse.AccountMovementSent
			row.Counterparty = tr.ToAddr
		case tr.ToAddr == address:
			row.Direction = clickhouse.AccountMovementReceived
			row.Counterparty = tr.FromAddr
		default:
			// Defensive: ListSEP41TransfersByAddress already filters to
			// from_addr=address OR to_addr=address, so this is
			// unreachable in practice; skip rather than emit a row with
			// no direction.
			continue
		}
		out = append(out, row)
	}
	return out
}

// resolveSEP41MovementAsset resolves a SEP-41 token contract_id to the
// canonical display form: the wrapped classic asset's name when it's
// a SAC (SACClassicAssetName, then the derivation-cross-checked
// event-topic fallback for a SAC whose wrap isn't captured in
// ledger_entries_current yet), else the raw contract_id itself (a
// genuine Soroban-native token, which has no classic-asset name to
// resolve to).
//
// The event-topic hint is attacker-influenceable and MUST be
// cross-checked (W2-explorer-1): sep41_transfers are ingested from ANY
// token contract (not identity-gated), so a hostile non-SAC token can
// emit a CAP-67 transfer whose trailing sep0011 topic claims a trusted
// asset (e.g. Circle USDC) and — rendered verbatim — impersonate that
// identity on this public, unauthenticated feed. So the fallback routes
// through sacAssetViaEvents (the SAME helper wasm_view.go uses): it
// re-derives the SAC address from the claimed asset and only trusts the
// label when it matches contractID (the topic is influenceable, the
// deterministic derivation is not). A non-matching claim falls through
// to the raw contract_id — the spoof renders as itself, never as the
// asset it impersonated.
func (h *Handler) resolveSEP41MovementAsset(ctx context.Context, contractID string) string {
	if name, ok, err := h.Reader.SACClassicAssetName(ctx, contractID); err == nil && ok {
		// The instance METADATA name is colon form ("USDC:GA5Z…", exactly
		// as the CAP-67 topic carries it) — normalize to the canonical
		// dash form the CH side of the merge stores, so one ?asset= value
		// matches both sides and the response's asset field doesn't flip
		// spelling across the P23 boundary (cold audit 2026-08-03). An
		// unparseable name passes through verbatim (status quo).
		return canonicalizeSACName(name)
	}
	if name, ok := h.sacAssetViaEvents(ctx, contractID); ok {
		return name // already canonical — asset.String()
	}
	return contractID
}

// canonicalizeSACName folds a SAC instance-METADATA asset name
// ("native" / "CODE:ISSUER") to the canonical asset_id spelling
// ("native" / "CODE-ISSUER"). Unparseable input returns unchanged.
func canonicalizeSACName(name string) string {
	if name == "native" {
		return canonical.NativeAsset().String()
	}
	code, issuer, ok := strings.Cut(name, ":")
	if !ok {
		return name
	}
	asset, err := canonical.NewClassicAsset(code, issuer)
	if err != nil {
		return name
	}
	return asset.String()
}

// assertP23NonOverlap is ADR-0048 D5's "assert [the non-overlap] in
// code" requirement: the ClickHouse archive is hard-clamped below
// classicmovements.P23StartLedger and the Postgres tail is hard-floored
// at-or-above it (timescale.SEP41MovementsFloorLedger, pinned to the
// same value by TestP23BoundaryConstantsAgree), so the two inputs to
// mergeAccountMovementRows should never straddle the boundary. A
// violation can only mean one of those two floors/clamps regressed
// elsewhere; it's logged as an error rather than panicking a
// user-facing read path — loud in observability, not a 500.
// assertMovementsNonOverlap checks the merge invariant at the DYNAMIC
// boundary (the cap67 watermark's Postgres floor — pgFloor): the CH arm
// serves strictly below it, the PG arm at/above it. Generalizes the old
// static-P23 assertion; pgFloor == P23StartLedger when the cap67
// archive isn't provisioned, so the pre-inventory-#1 invariant is the
// degenerate case.
func (h *Handler) assertMovementsNonOverlap(chRows, pgRows []clickhouse.AccountMovementRow, pgFloor uint32) {
	for _, row := range chRows {
		if row.Ledger >= pgFloor {
			h.Logger.Error("ADR-0048 D5 invariant violated: ClickHouse account_movements row at/past the merge boundary",
				"ledger", row.Ledger, "tx_hash", row.TxHash, "boundary", pgFloor)
		}
	}
	for _, row := range pgRows {
		if row.Ledger < pgFloor {
			h.Logger.Error("ADR-0048 D5 invariant violated: Postgres sep41_transfers-tail row below the merge boundary",
				"ledger", row.Ledger, "tx_hash", row.TxHash, "boundary", pgFloor)
		}
	}
}

// mergeAccountMovementRows merges two DESCENDING-sorted
// (ledger, tx_hash, op_index, leg_index) row sets (the ClickHouse
// archive page + the mapped Postgres tail page) into one
// DESCENDING-sorted result, truncated to limit. A genuine two-pointer
// merge (not a concatenation) — see AccountMovements' doc comment for
// why that matters even though the two inputs' ledger ranges never
// overlap in practice.
func mergeAccountMovementRows(a, b []clickhouse.AccountMovementRow, limit int) []clickhouse.AccountMovementRow {
	if limit <= 0 {
		return nil
	}
	out := make([]clickhouse.AccountMovementRow, 0, limit)
	i, j := 0, 0
	for len(out) < limit && (i < len(a) || j < len(b)) {
		switch {
		case i >= len(a):
			out = append(out, b[j])
			j++
		case j >= len(b):
			out = append(out, a[i])
			i++
		case movementRowIsNewer(a[i], b[j]):
			out = append(out, a[i])
			i++
		default:
			out = append(out, b[j])
			j++
		}
	}
	return out
}

// movementRowIsNewer reports whether x sorts strictly before y in the
// feed's descending (ledger, tx_hash, op_index, leg_index) order.
func movementRowIsNewer(x, y clickhouse.AccountMovementRow) bool {
	if x.Ledger != y.Ledger {
		return x.Ledger > y.Ledger
	}
	if x.TxHash != y.TxHash {
		return x.TxHash > y.TxHash
	}
	if x.OpIndex != y.OpIndex {
		return x.OpIndex > y.OpIndex
	}
	return x.LegIndex > y.LegIndex
}

// accountMovementEntryView renders one merged row as its wire shape.
func accountMovementEntryView(m clickhouse.AccountMovementRow) AccountMovementEntry {
	amt := "0"
	if m.Amount != nil {
		amt = m.Amount.String()
	}
	return AccountMovementEntry{
		Ledger:          m.Ledger,
		LedgerCloseTime: m.LedgerCloseTime.UTC().Format(time.RFC3339),
		TxHash:          m.TxHash,
		OpIndex:         m.OpIndex,
		LegIndex:        m.LegIndex,
		MovementKind:    m.MovementKind,
		Direction:       string(m.Direction),
		Asset:           m.Asset,
		Amount:          amt,
		Counterparty:    m.Counterparty,
		Provenance:      m.Provenance,
		Attributes:      m.Attributes,
	}
}
