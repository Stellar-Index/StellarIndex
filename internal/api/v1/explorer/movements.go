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

// SEP41MovementsReader is the Postgres read seam for the sep41_transfers recent-tail half of the ADR-0048 D5 merge; nil serves the
// ClickHouse archive alone with a coverage_note.
type SEP41MovementsReader interface {
	ListSEP41TransfersByAddress(ctx context.Context, address string, limit int, cur timescale.SEP41TransferCursor, direction, contractID string, floorLedger uint32) ([]timescale.SEP41TransferRow, error)
}

// AccountMovementEntry is one row of GET /v1/accounts/{g_strkey}/movements (ADR-0048 D5); Amount is a string (ADR-0003), Decimals
// omitted when unresolved so clients never guess an exponent.
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

// AccountMovementsView is the wire response for GET /v1/accounts/{g_strkey}/movements.
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

// MovementKindCoverage states what the feed serves for one kind family: Status "served", "partial" or "not_served", with inclusive
// ledger bounds omitted when not served.
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

// parseMovementFilter reads optional ?kind= / ?direction= / ?asset=. An invalid direction is a problem+json (ok=false); an unknown kind
// matches nothing. asset is canonicalised if it parses as an asset id, else passed verbatim.
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

// normalizeMovementAssetFilter folds ?asset= to the stores' spelling ("native" / "CODE-ISSUER"); the filter is exact-match, so "XLM"
// would serve an authoritative-looking empty feed. Non-asset values (pool:<hex> legs, raw C... ids) pass through verbatim.
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

// movementCursorParts is the (ledger, tx_hash, op_index, leg_index) tuple the opaque `?cursor=` encodes; leg_index on the CH side maps
// 1:1 to event_index on PG.
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

// parseMovementCursor decodes the dotted `?cursor=` (tx_hash is fixed 64-char hex, so never contains '.'). An optional 5th segment is
// the pinned cap67 watermark. Malformed values write a problem+json and return ok=false.
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

// AccountMovements serves GET /v1/accounts/{g_strkey}/movements (ADR-0048 D5), merging ClickHouse below movementsSplit's pgFloor with
// Postgres sep41_transfers at or above it; CoverageNote flags an empty CH table.
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

// movementsWatermark resolves the cap67 watermark splitting a page between the CH archive and the PG tail, so arms stay gap- and double-
// count-free. A client pin is re-validated; a read error fails closed to wm=0 (static P23 boundary) and failed is reported.
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

// postP23Boundary folds a watermark below the P23 floor to 0: it covers no post-P23 ledger, so the feed gets the no-archive scope and
// coverage note.
func postP23Boundary(wm uint32) uint32 {
	if wm < timescale.MovementsFloor() {
		return 0
	}
	return wm
}

// movementsSplit maps the watermark to the CH arm's inclusive ledger ceiling and the PG tail's floor (pgFloor == chCeiling+1, no gap).
// The ceiling is never below the static P23 boundary, else pre-P23 history hides from both arms; ceiling 0 is reachable (HasMaxLedger).
func movementsSplit(wm uint32) (chCeiling, pgFloor uint32) {
	chCeiling = timescale.MovementsFloor() - 1
	if wm > chCeiling {
		chCeiling = min(wm, math.MaxUint32-1) // keep chCeiling+1 representable
	}
	return chCeiling, chCeiling + 1
}

// cap67MovementsStaleLedgers is how far the lake tip may run past the archive boundary before the feed is stale: ~300 s of ~5 s ledgers,
// matching lakeStaleThreshold. The derive tracks the tip every 1-30 s, so more lag means it stopped or hit a lake hole.
const cap67MovementsStaleLedgers uint32 = 60

// movementsStale is the feed's flags.stale: the lake is stale, or `wm` trails the lake tip by more than [cap67MovementsStaleLedgers].
// wm == 0 (no archive) is not stale (the coverage note scopes it), and an unreadable lake watermark degrades to not-stale.
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

// fetchSEP41MovementsTail reads the Postgres recent-tail half of the merge. A non-empty coverageNote (no reader, read failed) degrades
// rather than fails the request; kind filters that cannot match a PG-tail row (all are "transfer") skip the round-trip.
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

// movementsCoverageNote is the feed's honest-scope statement by cap67 watermark; tailNote (a tail failure) wins. wm == 0: only the
// watched-token PG tail covers post-P23. wm > 0: all assets through the watermark, a progress marker not a verdict, so never "complete".
func movementsCoverageNote(wm uint32, tailNote string, supply supplyRange) string {
	return movementsArchiveNote(wm, tailNote) + "; " + movementsKindGapNote(supply)
}

// movementsCoverage is the structured form of movementsCoverageNote; fees and order-book fills are never served, so always reported
// not_served.
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
		// Partial while either end is still being derived: below the range (backfill) or above it up to the watermark (lag).
		if supply.from > floor || supply.thru < wm {
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

// movementsSupplyRange reads the range the derive recorded as covered with the supply kinds, clamped to this page's watermark; the note
// states the recorded range, never P23..watermark. A read error reads as none: the note must not over-claim.
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

// movementsKindGapNote names what each arm serves by kind: the cap67 archive carries mint, burn and clawback over the supply range only,
// elsewhere from P23 on (and in the PG tail) `transfer` alone; classicmovements has no fee or order-book-fill kind at any epoch.
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

// movementsKindNote replaces the archive note under a ?kind= other than transfer, naming the ledgers it can match: the CAP-67 supply
// kinds only the supply range (clawback also pre-P23 classic), every other kind only before P23.
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

// mapSEP41RowsToMovements converts sep41_transfers 'transfer' rows to clickhouse.AccountMovementRow. Each DISTINCT contract_id resolves
// to the CH canonical asset form; a non-empty assetFilter is re-checked, so a raw C... filter naming a SAC matches nothing.
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

// resolveSEP41MovementAsset resolves a SEP-41 contract_id to its display form: a SAC's wrapped asset name, else the raw id. The event-topic
// fallback is attacker-influenceable (a hostile token can claim USDC): trusted only when the re-derived SAC address equals contractID.
func (h *Handler) resolveSEP41MovementAsset(ctx context.Context, contractID string) string {
	name, _ := h.resolveSEP41MovementAssetChecked(ctx, contractID)
	return name
}

// resolveSEP41MovementAssetChecked also reports lookupFailed: the SAC metadata read errored and no event-topic proof recovered, so the
// returned contract id may be a SAC we could not fold, not a proven non-SAC.
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

// sacNameMemo remembers contracts proven to be a SAC. A SAC's address derives from the asset it wraps and its executable cannot be
// upgraded, so a hit never goes stale; a miss is not kept, since the lake may yet capture the instance. Zero value ready to use.
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

// sep41AssetScope maps a normalized ?asset= to the one sep41_transfers contract_id whose rows can render as it, so the PG arm filters
// before its LIMIT. A native/classic asset's only SEP-41 contract is its derived SAC; any other value can only equal a raw id.
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

// canonicalizeSACName folds a SAC instance-metadata asset name ("native" / "CODE:ISSUER") to the asset_id spelling ("native" / "CODE-
// ISSUER"). Unparseable input returns unchanged.
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

// assertMovementsNonOverlap checks ADR-0048 D5's non-overlap rule at pgFloor: CH serves strictly below it, PG at or above. Without a
// watermark pgFloor is timescale.MovementsFloor() (TestP23BoundaryConstantsAgree). A violation is logged, not a panic.
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

// mergeAccountMovementRows is a two-pointer merge (not a concatenation) of two DESCENDING-sorted (ledger, tx_hash, op_index, leg_index)
// row sets, truncated to limit, so it stays correct even if the inputs' ledger ranges ever overlap.
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

// accountMovementEntries renders the merged page, resolving each distinct asset's decimals once: the feed spans every token contract
// (ADR-0048), so the scale is per asset, never a feed-wide 7.
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

// movementAssetDecimals returns a movement asset's smallest-unit scale, or nil when unknown (unrecognised spelling, no decimals seam,
// failed read).
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
