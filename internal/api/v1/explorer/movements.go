package explorer

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// SEP41MovementsReader is the Postgres read seam for the sep41_transfers recent-tail half of the
// ADR-0048 D5 merge. Nil disables that half; the ClickHouse archive is then served alone with a
// coverage_note.
type SEP41MovementsReader interface {
	ListSEP41TransfersByAddress(ctx context.Context, address string, limit int, cur timescale.SEP41TransferCursor, direction, contractID string, floorLedger uint32) ([]timescale.SEP41TransferRow, error)
}

// AccountMovementEntry is one row in the wire response for GET
// /v1/accounts/{g_strkey}/movements (ADR-0048 D5). Amount is a string
// (ADR-0003 — i128 exceeds IEEE 754 double precision above 2^53).
// Decimals is Amount's per-row scale, omitted when unresolved so a client
// never divides by a guessed exponent.
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
	Decimals        *int           `json:"decimals,omitempty"`
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
	// CoverageNote is non-empty when this response is NOT the full ADR-0048 D5 merge: the
	// ClickHouse pre-P23 archive is not backfilled yet (an operator-run job, so CH starts empty),
	// or the Postgres recent-tail reader is unwired or errored. Absent means the full merge ran.
	CoverageNote string `json:"coverage_note,omitempty"`
	// Coverage is coverage_note as data: one entry per movement kind family.
	Coverage []MovementKindCoverage `json:"coverage"`
}

// MovementKindCoverage states what the feed serves for one kind family.
// Status is "served", "partial" (served over part of the range, or the
// archive absent) or "not_served"; the ledger bounds are inclusive and
// omitted when the kind is not served.
type MovementKindCoverage struct {
	Kind          string `json:"kind"`
	Status        string `json:"status"`
	FromLedger    uint32 `json:"from_ledger,omitempty"`
	ThroughLedger uint32 `json:"through_ledger,omitempty"`
}

// accountMovementsDefaultLimit / accountMovementsMaxLimit — ADR-0048
// D5's stated pagination contract.
const (
	accountMovementsDefaultLimit = 25
	accountMovementsMaxLimit     = 200
)

// parseMovementFilter reads the optional ?kind= / ?direction= / ?asset= params. An invalid
// direction writes a problem+json and returns ok=false. An unrecognized kind is not an error, it
// just matches nothing. asset is canonicalised when it parses as an asset id and passed through
// verbatim otherwise, because the movements asset domain is wider than ParseAsset's.
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

// normalizeMovementAssetFilter folds a ?asset= value to the canonical spelling the stores hold
// ("native" / "CODE-ISSUER"). The SQL filter is exact-match, so "XLM", "crypto:XLM" or
// "USDC:G..." would otherwise match nothing and serve an authoritative-looking empty feed.
// Values that do not parse as an asset id (pool:<hex> legs, raw C... contract ids) pass through
// verbatim and must not 400.
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

// movementCursorParts is the (ledger, tx_hash, op_index, leg_index) tuple the opaque `?cursor=`
// encodes. It means the same on both sides of the merge: leg_index on the CH side maps 1:1 to
// event_index on the PG side.
type movementCursorParts struct {
	Ledger   uint32
	TxHash   string
	OpIndex  uint32
	LegIndex uint32
	// PinnedWatermark is the cap67 archive watermark of the FIRST page, carried in the cursor so
	// every continuation reuses the same CH/PG split. Re-reading the advancing live watermark
	// mid-scroll would silently drop the native/unwatched sliver between old and new. Valid only
	// when HasPinnedWatermark is true (first-page and legacy 4-segment cursors have none).
	PinnedWatermark    uint32
	HasPinnedWatermark bool
}

func encodeMovementCursor(r clickhouse.AccountMovementRow, pinnedWatermark uint32) string {
	return fmt.Sprintf("%d.%s.%d.%d.%d", r.Ledger, r.TxHash, r.OpIndex, r.LegIndex, pinnedWatermark)
}

// parseMovementCursor decodes the dotted `?cursor=` (tx_hash is fixed 64-char hex, so it never
// contains '.'). An optional 5th segment is the pinned cap67 watermark. A malformed value writes
// a problem+json and returns ok=false.
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

// AccountMovements serves GET /v1/accounts/{g_strkey}/movements (ADR-0048 D5):
// the unified account-activity feed, newest first, keyset-paged by the opaque
// (ledger, tx_hash, op_index, leg_index) cursor, with optional
// ?kind=/?direction=/?asset= filters.
//
// Merge seam: ClickHouse stellar.account_movements (classic archive plus cap67-derived rows up
// to its watermark) covers every ledger BELOW movementsSplit's pgFloor; Postgres sep41_transfers
// 'transfer' rows cover every ledger AT OR ABOVE it. The ranges cannot overlap by construction;
// assertMovementsNonOverlap checks it per request and the two-pointer merge stays correct if a
// regression breaks that.
//
// The CH table is empty until an operator runs classic-movements-backfill; CoverageNote says so
// rather than presenting the Postgres tail as a complete feed.
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

	wm, wmFailed, ok := h.movementsWatermark(ctx, w, r, cur)
	if !ok {
		return
	}
	chCeiling, pgFloor := movementsSplit(wm)
	chFilter := filter
	chFilter.MaxLedger = chCeiling
	chFilter.HasMaxLedger = true

	chRows, err := h.Reader.AccountMovements(ctx, g, limit, chCur, chFilter)
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

	pgRows, tailNote := h.fetchSEP41MovementsTail(ctx, g, limit, cur, filter, pgFloor)
	supply := h.movementsSupplyRange(ctx, wm)
	coverageNote := movementsCoverageNote(wm, tailNote, supply)
	if filter.Kind != "" && filter.Kind != "transfer" {
		coverageNote = movementsKindNote(filter.Kind, supply)
	}
	h.assertMovementsNonOverlap(chRows, pgRows, pgFloor)

	merged := mergeAccountMovementRows(chRows, pgRows, limit)
	out := AccountMovementsView{
		Account:      g,
		Movements:    h.accountMovementEntries(ctx, merged),
		CoverageNote: coverageNote,
		Coverage:     movementsCoverage(wm, tailNote, supply),
	}
	if len(merged) == limit {
		// Pin the boundary this sequence committed to (the live watermark
		// on page 1, the already-pinned value on continuation pages) so
		// every subsequent page reuses it.
		out.NextCursor = encodeMovementCursor(merged[len(merged)-1], wm)
	}
	h.writeJSONAt(w, out, h.movementsStale(ctx, wm), wmFailed, time.Time{})
}

// movementsWatermark resolves the cap67 watermark that splits a page between the ClickHouse
// archive and the Postgres tail. One value drives both the CH ceiling and the PG floor, so the
// arms stay gap-free and double-count-free while the derive is mid-catch-up.
//
// A paginated scroll reuses the value page 1 pinned into next_cursor. The pin is client-supplied,
// so it is re-validated against the live value: a pin above it would ceiling CH at ledgers not yet
// derived and floor PG above them, dropping that range from both arms. A pin at or below the live
// value is only ever more conservative.
//
// A read error fails closed to wm=0: the static P23 boundary keeps the arms disjoint even against
// a fully populated archive, and the coverage note discloses the reduced scope. No pin can be
// validated then, so none is trusted. failed reports the error; the caller serves a degraded 200.
func (h *Handler) movementsWatermark(ctx context.Context, w http.ResponseWriter, r *http.Request, cur movementCursorParts) (wm uint32, failed, ok bool) {
	liveWM, err := h.Reader.Cap67MovementsWatermark(ctx)
	if err != nil {
		h.Logger.Warn("cap67 movements watermark read failed — failing closed to the static P23 boundary", "err", err)
		return 0, true, true
	}
	if !cur.HasPinnedWatermark {
		return postP23Boundary(liveWM), false, true
	}
	if cur.PinnedWatermark > liveWM {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-cursor",
			"Invalid cursor", http.StatusBadRequest,
			"cursor is ahead of the movement archive boundary; restart pagination without a cursor")
		return 0, false, false
	}
	return postP23Boundary(cur.PinnedWatermark), false, true
}

// postP23Boundary folds a watermark below the P23 floor to 0: it covers no
// post-P23 ledger, so the feed's scope (and coverage note) is the no-archive
// one rather than "all assets through" a pre-P23 ledger.
func postP23Boundary(wm uint32) uint32 {
	if wm < timescale.MovementsFloor() {
		return 0
	}
	return wm
}

// movementsSplit maps the watermark to the CH arm's inclusive ledger ceiling and the PG tail's
// inclusive floor, with pgFloor == chCeiling+1 so no ledger falls between the arms.
//
// The ceiling is never below the static P23 boundary: the classic archive owns everything under
// it and the tail never reads beneath it, so a lower watermark would hide pre-P23 history from
// both arms. With wm == 0 the ceiling is that boundary, which trims a populated archive so
// cap67_derived rows do not double-list with the tail. It is applied as a SQL predicate, not a
// post-read trim. Ceiling 0 is reachable (genesis floor, no watermark) and serves nothing, hence
// HasMaxLedger rather than a MaxLedger>0 sentinel.
func movementsSplit(wm uint32) (chCeiling, pgFloor uint32) {
	chCeiling = timescale.MovementsFloor() - 1
	if wm > chCeiling {
		chCeiling = min(wm, math.MaxUint32-1) // keep chCeiling+1 representable
	}
	return chCeiling, chCeiling + 1
}

// cap67MovementsStaleLedgers is how far the lake tip may run past the all-assets archive boundary
// before the feed is flagged stale: ~300 s of ~5 s ledgers, matching lakeStaleThreshold. The
// derive tracks the tip every 1-30 s, so a larger lag means it stopped or hit a lake hole.
const cap67MovementsStaleLedgers uint32 = 60

// movementsStale is the feed's flags.stale: the lake is stale, or the archive boundary `wm`
// trails the lake tip by more than [cap67MovementsStaleLedgers]. wm == 0 (no archive) is not
// stale because the coverage note already scopes it. An unwired or unreadable lake watermark
// degrades to not-stale, as on every lake surface.
func (h *Handler) movementsStale(ctx context.Context, wm uint32) bool {
	if h.LakeWatermark == nil {
		return false
	}
	tip, lakeStale, ok := h.LakeWatermark(ctx)
	if !ok {
		return false
	}
	return lakeStale || (wm > 0 && tip > wm && tip-wm > cap67MovementsStaleLedgers)
}

// fetchSEP41MovementsTail reads and maps the Postgres recent-tail half of the merge. It returns a
// non-empty coverageNote when no reader is wired or the read failed; that degrades rather than
// fails the request, since the ClickHouse archive is still a valid if recency-incomplete answer.
// Kind filters that cannot match a PG-tail row (all are movement_kind="transfer") skip the
// round-trip.
func (h *Handler) fetchSEP41MovementsTail(ctx context.Context, address string, limit int, cur movementCursorParts, filter clickhouse.AccountMovementFilter, floorLedger uint32) ([]clickhouse.AccountMovementRow, string) {
	if h.SEP41Movements == nil {
		return nil, fmt.Sprintf("this deployment has not wired the recent Postgres tail reader; showing only the movement archive, which ends before ledger %d", floorLedger)
	}
	if filter.Kind != "" && filter.Kind != "transfer" {
		return nil, ""
	}
	pgCur := timescale.SEP41TransferCursor{Ledger: cur.Ledger, TxHash: cur.TxHash, OpIndex: cur.OpIndex, EventIndex: cur.LegIndex}
	scopeContract, _ := sep41AssetScope(filter.Asset)
	rows, err := h.SEP41Movements.ListSEP41TransfersByAddress(ctx, address, limit, pgCur, string(filter.Direction), scopeContract, floorLedger)
	if err != nil {
		h.Logger.Error("explorer AccountMovements (Postgres recent tail) failed", "err", err, "account", address)
		return nil, fmt.Sprintf("the recent tail is temporarily unavailable; showing only the movement archive, which ends before ledger %d", floorLedger)
	}
	return h.mapSEP41RowsToMovements(ctx, address, rows, filter.Asset), ""
}

// movementsCoverageNote is the feed's honest-scope statement, chosen by the cap67 watermark.
// tailNote (a tail wiring/availability failure) takes priority: the response is missing data
// beyond the structural scope.
//
// wm == 0: the cap67-derived archive is not provisioned, so post-P23 coverage is the
// watched-token Postgres tail only and classic XLM payment history after the boundary is absent.
// Saying so on every response keeps a busy XLM account's feed from masquerading as complete.
//
// wm > 0: all assets are served through the watermark; only the sliver above it is
// watched-tokens-only. The watermark is a progress marker, not a verdict (account_movements has
// no /v1/coverage row), so the note must not call it complete. Every variant ends with
// movementsKindGapNote, which scopes kinds as well as assets.
func movementsCoverageNote(wm uint32, tailNote string, supply supplyRange) string {
	return movementsArchiveNote(wm, tailNote) + "; " + movementsKindGapNote(supply)
}

// movementsCoverage is the structured form of movementsCoverageNote. Fees
// and order-book fills are never served on this feed, so they are always
// reported not_served rather than omitted.
func movementsCoverage(wm uint32, tailNote string, supply supplyRange) []MovementKindCoverage {
	floor := timescale.MovementsFloor()
	transfer := MovementKindCoverage{Kind: "transfer", Status: "served", FromLedger: floor, ThroughLedger: wm}
	if wm == 0 || tailNote != "" {
		transfer = MovementKindCoverage{Kind: "transfer", Status: "partial"}
	}
	supplyKinds := MovementKindCoverage{Kind: "mint_burn_clawback", Status: "not_served"}
	if supply.from != 0 {
		supplyKinds.Status = "served"
		supplyKinds.FromLedger, supplyKinds.ThroughLedger = supply.from, supply.thru
		if supply.from > floor {
			supplyKinds.Status = "partial"
		}
	}
	return []MovementKindCoverage{
		transfer,
		supplyKinds,
		{Kind: "fee", Status: "not_served"},
		{Kind: "fill", Status: "not_served"},
	}
}

// supplyRange is the ledger range the archive serves mint, burn and clawback
// over; from 0 means none.
type supplyRange struct{ from, thru uint32 }

// movementsSupplyRange reads the range the derive recorded as covered with the supply kinds,
// clamped to this page's (possibly pinned) watermark. Earlier-derived ledgers are outside it until
// backfilled, so the note states the recorded range, never P23..watermark. A read error reads as
// none: the note must not over-claim.
func (h *Handler) movementsSupplyRange(ctx context.Context, wm uint32) supplyRange {
	if wm == 0 {
		return supplyRange{}
	}
	from, thru, ok, err := h.Reader.Cap67SupplyCoverage(ctx)
	if err != nil {
		h.Logger.Warn("cap67 supply coverage read failed — disclosing no supply-kind coverage", "err", err)
		return supplyRange{}
	}
	thru = min(thru, wm)
	if !ok || thru < from {
		return supplyRange{}
	}
	return supplyRange{from: from, thru: thru}
}

// movementsKindGapNote names what each arm serves by kind. The cap67 archive
// carries mint, burn and clawback over the supply range only; elsewhere from
// P23 on, and in the Postgres tail, it is `transfer` alone; classicmovements
// has no fee or order-book-fill kind at any epoch.
func movementsKindGapNote(supply supplyRange) string {
	const noFees = ", and order-book fills are not served at any ledger; fees are not movements (ADR-0047 D3) " +
		"and are shown per transaction as fee_charged"
	if supply.from == 0 {
		return "from 2025-09-03 (P23) on, this feed carries transfer movements only — mint, burn and clawback " +
			"(including every payment to or from an asset's issuer) are not served yet" + noFees
	}
	var gap string
	from := "2025-09-03 (P23)"
	if supply.from > timescale.MovementsFloor() {
		gap = fmt.Sprintf("from 2025-09-03 (P23) through ledger %d this feed carries transfer movements only "+
			"(mint, burn and clawback there are still being backfilled); ", supply.from-1)
		from = fmt.Sprintf("ledger %d", supply.from)
	}
	return gap + fmt.Sprintf("from %s through ledger %d the archive also carries mint, burn and clawback "+
		"(CAP-67's form of every payment to or from an asset's issuer); above that ledger the feed carries "+
		"transfer movements only", from, supply.thru) + noFees
}

// movementsKindNote replaces the archive note under a ?kind= filter other
// than transfer, naming the ledgers that kind can match: the CAP-67 supply
// kinds only the supply range (clawback the pre-P23 classic archive too),
// every other kind only before P23.
func movementsKindNote(kind string, supply supplyRange) string {
	floor := timescale.MovementsFloor()
	switch kind {
	case "mint", "burn", "clawback":
		var pre string
		if kind == "clawback" {
			pre = fmt.Sprintf("before ledger %d (2025-09-03, P23) from classic clawback operations, and ", floor)
		}
		if supply.from == 0 {
			return fmt.Sprintf("movement_kind %q is served %sfrom P23 on only from the movement archive, which "+
				"has not derived this kind yet, so no later rows are returned; see "+
				"/accounts/{g}/operations for later activity", kind, pre)
		}
		note := fmt.Sprintf("movement_kind %q is served %sfrom ledger %d through ledger %d from the CAP-67 "+
			"movement archive (derive progress, not a verified completeness verdict)", kind, pre, max(supply.from, floor), supply.thru)
		if supply.from > floor {
			note += fmt.Sprintf("; ledgers %d through %d (from P23) are still being backfilled for this kind "+
				"and return no rows", floor, supply.from-1)
		}
		return note + "; newer movements of this kind appear as the archive follows the tip"
	}
	return fmt.Sprintf("movement_kind %q is served only before ledger %d (2025-09-03, P23): from P23 on, "+
		"this feed carries transfer, mint, burn and clawback movements only, so these results stop at that "+
		"boundary; see /accounts/{g}/operations for later activity", kind, floor)
}

func movementsArchiveNote(wm uint32, tailNote string) string {
	if tailNote != "" {
		return tailNote
	}
	if wm == 0 {
		return "movements after 2025-09-03 (P23) currently include watched Soroban/SAC tokens only — " +
			"classic XLM payment history after that date is not yet served on this feed " +
			"(the full-history movement archive is being extended); see /accounts/{g}/operations " +
			"for complete raw operation history"
	}
	return fmt.Sprintf("all assets through ledger %d, from the movement archive derived from the "+
		"CAP-67 event lake (derive progress, not a verified completeness verdict — this archive has no "+
		"/v1/coverage entry); more recent movements may include watched Soroban/SAC tokens only while "+
		"the archive follows the tip (near real-time)", wm)
}

// mapSEP41RowsToMovements converts sep41_transfers 'transfer' rows into the
// clickhouse.AccountMovementRow shape, with direction/counterparty computed against `address`
// as clickhouse.FanOutAccountMovement does, so the merge works on one type.
//
// sep41_transfers stores the raw contract_id, but `asset` should read the same canonical form the
// CH rows use. Each DISTINCT contract_id in the page (so cost is bounded by page size) resolves
// through SACClassicAssetName then SACAssetFromEvents, falling back to the raw contract_id for a
// Soroban-native token. A non-empty assetFilter already scoped the SQL (sep41AssetScope);
// re-checking the resolved name drops a raw C... filter naming a SAC, whose rows render as the
// classic asset and so match nothing.
func (h *Handler) mapSEP41RowsToMovements(ctx context.Context, address string, rows []timescale.SEP41TransferRow, assetFilter string) []clickhouse.AccountMovementRow {
	if len(rows) == 0 {
		return nil
	}
	assetNames := make(map[string]string, len(rows))
	if sac, label := sep41AssetScope(assetFilter); label != "" {
		assetNames[sac] = label
	}
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

// resolveSEP41MovementAsset resolves a SEP-41 token contract_id to its display form: the wrapped
// classic asset's name for a SAC (SACClassicAssetName, then an event-topic fallback for a SAC
// whose wrap is not yet in ledger_entries_current), else the raw contract_id.
//
// The event-topic hint is attacker-influenceable and MUST be cross-checked: sep41_transfers
// ingests ANY token contract, so a hostile non-SAC token can emit a transfer whose sep0011 topic
// claims a trusted asset (e.g. Circle USDC) and impersonate it. sacAssetViaEvents re-derives the
// SAC address from the claimed asset and trusts the label only when it equals contractID; a
// mismatch renders as the raw contract_id. A proven SAC is memoised across requests.
func (h *Handler) resolveSEP41MovementAsset(ctx context.Context, contractID string) string {
	name, _ := h.resolveSEP41MovementAssetChecked(ctx, contractID)
	return name
}

// resolveSEP41MovementAssetChecked also reports lookupFailed: the SAC
// metadata read errored and no event-topic proof recovered, so the returned
// contract id may be a SAC we could not fold, not a proven non-SAC.
func (h *Handler) resolveSEP41MovementAssetChecked(ctx context.Context, contractID string) (name string, lookupFailed bool) {
	if name, ok := h.sacNames.get(contractID); ok {
		return name, false
	}
	name, lookupFailed = h.resolveSEP41AssetUncached(ctx, contractID)
	if name != contractID {
		h.sacNames.put(contractID, name)
	}
	return name, lookupFailed
}

func (h *Handler) resolveSEP41AssetUncached(ctx context.Context, contractID string) (string, bool) {
	name, ok, err := h.Reader.SACClassicAssetName(ctx, contractID)
	if err == nil && ok {
		// The instance metadata name is colon form ("USDC:GA5Z..."); normalize to the dash form the
		// CH side stores so one ?asset= value matches both arms and the spelling does not flip
		// across the P23 boundary. An unparseable name passes through verbatim.
		return canonicalizeSACName(name), false
	}
	if name, ok := h.sacAssetViaEvents(ctx, contractID); ok {
		return name, false // already canonical — asset.String()
	}
	return contractID, err != nil
}

// sacNameMemoMax bounds the SAC memo; a full memo is emptied, so a flood
// of distinct SACs costs lake reads (as with no memo), never memory.
const sacNameMemoMax = 4096

// sacNameMemo remembers contracts proven to be a SAC. A SAC's address is
// derived from the asset it wraps and its executable cannot be upgraded,
// so a hit never goes stale; a miss is not kept, since the lake may yet
// capture the instance. Zero value ready to use.
type sacNameMemo struct {
	mu    sync.Mutex
	names map[string]string
}

func (m *sacNameMemo) get(contractID string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name, ok := m.names[contractID]
	return name, ok
}

func (m *sacNameMemo) put(contractID, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.names == nil || len(m.names) >= sacNameMemoMax {
		m.names = make(map[string]string)
	}
	m.names[contractID] = name
}

// sep41AssetScope maps a normalized ?asset= value to the one
// sep41_transfers contract_id whose rows can render as it, so the Postgres arm filters before
// its LIMIT. A native/classic asset's only SEP-41 contract is its SAC, derived from the asset
// itself (the trust anchor sacAssetViaEvents also uses); label is the asset those rows render as.
// Any other value can only equal a raw contract_id, with label "". An empty filter returns "", "".
func sep41AssetScope(assetFilter string) (contractID, label string) {
	if assetFilter == "" {
		return "", ""
	}
	parsed, err := canonical.ParseAsset(assetFilter)
	if err != nil || (parsed.Type != canonical.AssetNative && parsed.Type != canonical.AssetClassic) {
		return assetFilter, ""
	}
	sac, err := parsed.SacContractID()
	if err != nil {
		return assetFilter, ""
	}
	return sac, parsed.String()
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

// assertMovementsNonOverlap checks ADR-0048 D5's non-overlap requirement at the dynamic merge
// boundary pgFloor: the CH arm serves strictly below it, the PG arm at or above it. Without a
// cap67 watermark pgFloor is timescale.MovementsFloor(), pinned to the P23 ledger by
// TestP23BoundaryConstantsAgree. A violation means a floor or ceiling regressed elsewhere; it is
// logged as an error rather than panicking a user-facing read path.
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

// mergeAccountMovementRows merges two DESCENDING-sorted (ledger, tx_hash, op_index, leg_index)
// row sets (CH archive page and mapped Postgres tail page) into one descending result truncated
// to limit. It is a real two-pointer merge, not a concatenation, so it stays correct even if the
// inputs' ledger ranges ever overlap.
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

// classicAmountDecimals is the protocol-fixed stroop scale of native,
// classic credit and liquidity-pool-share amounts.
const classicAmountDecimals = 7

// movementDecimalsBudget bounds a whole page's token-decimals reads, so a
// page of many distinct Soroban tokens cannot stretch the request.
const movementDecimalsBudget = 2 * time.Second

// accountMovementEntries renders the merged page, resolving each distinct
// asset's decimals once (the feed spans every token contract since
// ADR-0048, so the scale is per asset, never a feed-wide 7).
func (h *Handler) accountMovementEntries(ctx context.Context, merged []clickhouse.AccountMovementRow) []AccountMovementEntry {
	dctx, cancel := context.WithTimeout(ctx, movementDecimalsBudget)
	defer cancel()
	scale := h.assetScales(dctx)
	out := make([]AccountMovementEntry, len(merged))
	for i, m := range merged {
		out[i] = accountMovementEntryView(m)
		out[i].Decimals = scale(m.Asset)
	}
	return out
}

// assetScales memoises movementAssetDecimals per asset for one page.
func (h *Handler) assetScales(ctx context.Context) func(asset string) *int {
	scales := make(map[string]*int)
	return func(asset string) *int {
		d, seen := scales[asset]
		if !seen {
			d = h.movementAssetDecimals(ctx, asset)
			scales[asset] = d
		}
		return d
	}
}

// movementAssetDecimals returns a movement asset's smallest-unit scale, or
// nil when it is unknown: an unrecognised asset spelling, no decimals seam,
// or a failed contract read.
func (h *Handler) movementAssetDecimals(ctx context.Context, asset string) *int {
	if strings.HasPrefix(asset, "pool:") {
		d := classicAmountDecimals
		return &d
	}
	parsed, err := canonical.ParseAsset(asset)
	if err != nil {
		return nil
	}
	switch parsed.Type {
	case canonical.AssetNative, canonical.AssetClassic:
		d := classicAmountDecimals
		return &d
	case canonical.AssetSoroban:
		if h.TokenDecimals == nil {
			return nil
		}
		if d, ok := h.TokenDecimals(ctx, parsed.ContractID); ok {
			return &d
		}
		return nil
	default:
		// fiat / crypto / rwa never reach an on-chain movement feed.
		return nil
	}
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
