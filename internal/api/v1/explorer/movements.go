package explorer

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/classicmovements"
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
	ListSEP41TransfersByAddress(ctx context.Context, address string, limit int, cur timescale.SEP41TransferCursor, direction string) ([]timescale.SEP41TransferRow, error)
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
// (after a problem+json) otherwise. kind/asset are free-form (matched
// as exact-equality filters against whatever movement_kind/asset
// values the two backing stores hold; an unrecognized kind is not an
// error, just a filter that matches nothing).
func (h *Handler) parseMovementFilter(w http.ResponseWriter, r *http.Request) (clickhouse.AccountMovementFilter, bool) {
	f := clickhouse.AccountMovementFilter{
		Kind:  r.URL.Query().Get("kind"),
		Asset: r.URL.Query().Get("asset"),
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
}

func encodeMovementCursor(r clickhouse.AccountMovementRow) string {
	return fmt.Sprintf("%d.%s.%d.%d", r.Ledger, r.TxHash, r.OpIndex, r.LegIndex)
}

// parseMovementCursor decodes the opaque `?cursor=` — dotted-decimal
// with the tx_hash segment in the middle (safe: tx_hash is a fixed
// 64-char hex string, never contains '.'). ok=false (after a
// problem+json) on a malformed value.
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
	if len(parts) != 4 {
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
	return movementCursorParts{Ledger: uint32(ledger), TxHash: txHash, OpIndex: uint32(opIdx), LegIndex: uint32(legIdx)}, true
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

	chRows, err := h.Reader.AccountMovements(ctx, g, limit, chCur, filter)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		h.Logger.Error("explorer AccountMovements (ClickHouse archive) failed", "err", err, "account", g)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	pgRows, coverageNote := h.fetchSEP41MovementsTail(ctx, g, limit, cur, filter)
	h.assertP23NonOverlap(chRows, pgRows)

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
		out.NextCursor = encodeMovementCursor(merged[len(merged)-1])
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
func (h *Handler) fetchSEP41MovementsTail(ctx context.Context, address string, limit int, cur movementCursorParts, filter clickhouse.AccountMovementFilter) ([]clickhouse.AccountMovementRow, string) {
	if h.SEP41Movements == nil {
		return nil, "this deployment has not wired the recent (post-P23) Postgres tail reader; showing only the ClickHouse pre-P23 archive"
	}
	if filter.Kind != "" && filter.Kind != "transfer" {
		return nil, ""
	}
	pgCur := timescale.SEP41TransferCursor{Ledger: cur.Ledger, TxHash: cur.TxHash, OpIndex: cur.OpIndex, EventIndex: cur.LegIndex}
	rows, err := h.SEP41Movements.ListSEP41TransfersByAddress(ctx, address, limit, pgCur, string(filter.Direction))
	if err != nil {
		h.Logger.Error("explorer AccountMovements (Postgres recent tail) failed", "err", err, "account", address)
		return nil, "the recent (post-P23) tail is temporarily unavailable; showing the pre-P23 ClickHouse archive only"
	}
	return h.mapSEP41RowsToMovements(ctx, address, rows, filter.Asset), ""
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
// a SAC (SACClassicAssetName, then the SACAssetFromEvents fallback for
// a SAC whose wrap isn't captured in ledger_entries_current yet), else
// the raw contract_id itself (a genuine Soroban-native token, which
// has no classic-asset name to resolve to).
func (h *Handler) resolveSEP41MovementAsset(ctx context.Context, contractID string) string {
	if name, ok, err := h.Reader.SACClassicAssetName(ctx, contractID); err == nil && ok {
		return name
	}
	if name, ok, err := h.Reader.SACAssetFromEvents(ctx, contractID); err == nil && ok {
		return name
	}
	return contractID
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
func (h *Handler) assertP23NonOverlap(chRows, pgRows []clickhouse.AccountMovementRow) {
	for _, row := range chRows {
		if row.Ledger >= classicmovements.P23StartLedger {
			h.Logger.Error("ADR-0048 D5 invariant violated: ClickHouse account_movements row at/past the P23 boundary",
				"ledger", row.Ledger, "tx_hash", row.TxHash, "boundary", classicmovements.P23StartLedger)
		}
	}
	for _, row := range pgRows {
		if row.Ledger < classicmovements.P23StartLedger {
			h.Logger.Error("ADR-0048 D5 invariant violated: Postgres sep41_transfers-tail row before the P23 boundary",
				"ledger", row.Ledger, "tx_hash", row.TxHash, "boundary", classicmovements.P23StartLedger)
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
