package explorer

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/entryhistory"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

const (
	// entryLedgerBatch is how many indexed ledgers one pass streams through the projector; entryLedgerPasses caps
	// the passes so a sparse role filter cannot walk the whole index inside one request.
	entryLedgerBatch  = 32
	entryLedgerPasses = 8
	// Lake partitions 39-53 were derived before intra_ledger_seq took its current meaning; every page names the range.
	entryChangesOldNumberingFrom = 39_000_000
	entryChangesOldNumberingThru = 53_999_999
	// entryCursorBelowLedger is the tx hash of a cursor that resumes strictly below its ledger.
	entryCursorBelowLedger = "-"
)

var entryChangeRoles = []string{clickhouse.EntryIndexRoleOwner, clickhouse.EntryIndexRoleClaimant, clickhouse.EntryIndexRoleSponsor}

// AccountEntryChangeEntry is one ledger-entry change the account took part in, decoded from the lake on read.
type AccountEntryChangeEntry struct {
	Ledger          uint32   `json:"ledger"`
	LedgerCloseTime string   `json:"ledger_close_time"`
	IntraLedgerSeq  uint32   `json:"intra_ledger_seq"`
	TxHash          string   `json:"tx_hash"`
	OpIndex         int32    `json:"op_index"`
	ChangeIndex     uint32   `json:"change_index"`
	Role            string   `json:"role"`
	EntryType       string   `json:"entry_type"`
	ChangeType      string   `json:"change_type"`
	Changed         []string `json:"changed"`
	Asset           string   `json:"asset,omitempty"`
	// Amount is a decimal string of stroops (ADR-0003): the entry's balance after the change; absent on sponsor rows.
	Amount string          `json:"amount,omitempty"`
	Entry  json.RawMessage `json:"entry"`
}

// AccountEntryChangesView is one page of /v1/accounts/{g_strkey}/entry-changes.
type AccountEntryChangesView struct {
	Account       string                    `json:"account"`
	Roles         []string                  `json:"roles"`
	Changes       []AccountEntryChangeEntry `json:"changes"`
	NextCursor    string                    `json:"next_cursor,omitempty"`
	ThroughLedger uint32                    `json:"through_ledger"`
	LowerBound    bool                      `json:"lower_bound"`
	CoverageNote  string                    `json:"coverage_note"`
}

// AccountEntryChanges serves GET /v1/accounts/{g_strkey}/entry-changes: the account's ledger-entry changes newest
// first, located through the entry index and decoded from the lake on read.
func (h *Handler) AccountEntryChanges(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	account, ok := h.parseAccountStrkey(w, r)
	if !ok {
		return
	}
	roles, ok := h.parseEntryRoles(w, r)
	if !ok {
		return
	}
	limit, ok := h.ParseLimit(w, r, accountMovementsDefaultLimit, accountMovementsMaxLimit)
	if !ok {
		return
	}
	cur, ok := h.parseEntryChangeCursor(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()
	wm, err := h.Reader.EntryIndexWatermark(ctx)
	if err != nil {
		h.accountEntryChangesError(w, r, ctx, err, account)
		return
	}
	if wm == 0 {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/entry-index-not-built", "Entry changes not yet derived",
			http.StatusServiceUnavailable, "the entry index has not been built on this deployment yet; retry later")
		return
	}
	raw, err := strkey.Decode(strkey.VersionByteAccountID, account)
	if err != nil {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-account", "Invalid account", http.StatusBadRequest, err.Error())
		return
	}
	page, err := h.accountEntryPage(ctx, string(raw), account, roles, limit, cur, wm)
	if err != nil {
		h.accountEntryChangesError(w, r, ctx, err, account)
		return
	}
	lowerBound := page.lowerBound || (cur.IsSet() && cur.Ledger > wm)
	out := AccountEntryChangesView{
		Account: account, Roles: roles, Changes: page.rows, NextCursor: page.next,
		ThroughLedger: wm, LowerBound: lowerBound, CoverageNote: accountEntryChangesNote(wm, roles, lowerBound),
	}
	h.writeJSONAt(w, out, h.movementsStale(ctx, wm), false, time.Time{})
}

// parseEntryRoles reads ?role= as a comma-separated subset of owner, claimant and sponsor; the default leaves
// sponsorship out because its index rows arrive with the sponsorship proof.
func (h *Handler) parseEntryRoles(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	q := strings.TrimSpace(r.URL.Query().Get("role"))
	if q == "" {
		return []string{clickhouse.EntryIndexRoleOwner, clickhouse.EntryIndexRoleClaimant}, true
	}
	var roles []string
	for _, p := range strings.Split(q, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if !slices.Contains(entryChangeRoles, p) {
			h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-role", "Invalid role", http.StatusBadRequest,
				"role must be a comma-separated list of owner, claimant, sponsor")
			return nil, false
		}
		if !slices.Contains(roles, p) {
			roles = append(roles, p)
		}
	}
	return roles, true
}

func (h *Handler) accountEntryChangesError(w http.ResponseWriter, r *http.Request, ctx context.Context, err error, account string) {
	if h.ClientAborted(r, err) {
		return
	}
	if retryableColdMiss(ctx, err) {
		h.Logger.Warn("explorer AccountEntryChanges deadline exceeded", "account", account)
		h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/account-entry-changes-timeout",
			"Account entry changes timed out")
		return
	}
	h.Logger.Error("explorer AccountEntryChanges failed", "err", err, "account", account)
	h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal", "Internal error", http.StatusInternalServerError, "")
}

type entryChangesPage struct {
	rows       []AccountEntryChangeEntry
	next       string
	lowerBound bool
}

// accountEntryPage walks the account's indexed ledgers newest first, a batch at a time, until the page is full, the
// index is exhausted or the pass cap is hit.
func (h *Handler) accountEntryPage(ctx context.Context, key, account string, roles []string, limit int,
	cur clickhouse.AssetEntryChangeCursor, wm uint32,
) (entryChangesPage, error) {
	ceiling := wm
	if cur.IsSet() {
		c := cur.Ledger
		if cur.TxHash == entryCursorBelowLedger {
			c--
		}
		ceiling = min(ceiling, c)
	}
	var (
		rows []clickhouse.AccountEntryChange
		more bool
		last uint32
	)
	for pass := 0; pass < entryLedgerPasses; pass++ {
		ledgers, err := h.Reader.AccountEntryLedgers(ctx, key, roles, ceiling, entryLedgerBatch)
		if err != nil {
			return entryChangesPage{}, err
		}
		if len(ledgers) == 0 {
			more = false
			break
		}
		got, err := h.projectAccountRows(ctx, ledgers, account, roles, cur)
		if err != nil {
			return entryChangesPage{}, err
		}
		rows = append(rows, got...)
		last = ledgers[len(ledgers)-1]
		more = len(ledgers) == entryLedgerBatch
		if len(rows) >= limit || !more {
			break
		}
		ceiling = last - 1
	}
	return finishEntryChangesPage(rows, limit, more, last)
}

// finishEntryChangesPage orders the rows newest first and cuts the page; a full page continues from its last row, a
// short page that stopped at the pass cap continues strictly below the last ledger walked.
func finishEntryChangesPage(rows []clickhouse.AccountEntryChange, limit int, more bool, last uint32) (entryChangesPage, error) {
	slices.SortFunc(rows, func(a, b clickhouse.AccountEntryChange) int { return compareEntryChange(b, a) })
	var page entryChangesPage
	if len(rows) >= limit {
		rows = rows[:limit]
		r := rows[limit-1]
		page.next = fmt.Sprintf("%d.%s.%d.%d.%s", r.Ledger, r.TxHash, r.OpIndex, r.ChangeIndex, r.Role)
	} else if more {
		page.next = fmt.Sprintf("%d.%s.-1.0.%s", last, entryCursorBelowLedger, entryCursorBelowLedger)
		page.lowerBound = true
	}
	page.rows = make([]AccountEntryChangeEntry, 0, len(rows))
	for _, r := range rows {
		e, err := accountEntryChangeEntry(r)
		if err != nil {
			return entryChangesPage{}, err
		}
		page.rows = append(page.rows, e)
	}
	return page, nil
}

// projectAccountRows streams the listed ledgers through the projector and keeps the account's rows in the wanted
// roles that lie after the cursor. Unmerged ReplacingMergeTree duplicates are adjacent in sort-key order.
func (h *Handler) projectAccountRows(ctx context.Context, ledgers []uint32, account string, roles []string,
	cur clickhouse.AssetEntryChangeCursor,
) ([]clickhouse.AccountEntryChange, error) {
	p := entryhistory.NewProjector()
	var (
		out      []clickhouse.AccountEntryChange
		prev     clickhouse.EntryHistorySourceRow
		curSeq   uint32
		curFound bool
	)
	err := h.Reader.StreamEntryChangesForLedgers(ctx, ledgers, func(s clickhouse.EntryHistorySourceRow) error {
		if sameEntryChange(s, prev) {
			return nil
		}
		prev = s
		if isEntryCursorRow(s, cur) {
			curSeq, curFound = s.IntraLedgerSeq, true
		}
		acct, _, err := p.Project(s)
		if err != nil {
			return fmt.Errorf("ledger %d change %s/%d/%d: %w", s.Ledger, s.TxHash, s.OpIndex, s.ChangeIndex, err)
		}
		for _, a := range acct {
			if a.Account == account && slices.Contains(roles, a.Role) {
				out = append(out, a)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if cur.IsSet() {
		out = slices.DeleteFunc(out, func(a clickhouse.AccountEntryChange) bool { return !afterEntryCursor(a, cur, curSeq, curFound) })
	}
	return out, nil
}

func sameEntryChange(a, b clickhouse.EntryHistorySourceRow) bool {
	return a.Ledger == b.Ledger && a.TxHash == b.TxHash && a.OpIndex == b.OpIndex && a.ChangeIndex == b.ChangeIndex
}

func isEntryCursorRow(s clickhouse.EntryHistorySourceRow, cur clickhouse.AssetEntryChangeCursor) bool {
	return cur.IsSet() && s.Ledger == cur.Ledger && s.TxHash == cur.TxHash && s.OpIndex == cur.OpIndex && s.ChangeIndex == cur.ChangeIndex
}

// afterEntryCursor reports whether a lies after the cursor in newest-first order; the cursor row's intra-ledger
// position is known only when the row itself was seen in the stream.
func afterEntryCursor(a clickhouse.AccountEntryChange, cur clickhouse.AssetEntryChangeCursor, curSeq uint32, curFound bool) bool {
	if a.Ledger != cur.Ledger {
		return a.Ledger < cur.Ledger
	}
	if curFound && a.IntraLedgerSeq != curSeq {
		return a.IntraLedgerSeq < curSeq
	}
	return cmp.Or(strings.Compare(a.TxHash, cur.TxHash), cmp.Compare(a.OpIndex, cur.OpIndex),
		cmp.Compare(a.ChangeIndex, cur.ChangeIndex), strings.Compare(a.Role, cur.Role)) < 0
}

// compareEntryChange orders rows oldest first: ledger, apply order within the ledger, then the lake sort key.
func compareEntryChange(a, b clickhouse.AccountEntryChange) int {
	return cmp.Or(cmp.Compare(a.Ledger, b.Ledger), cmp.Compare(a.IntraLedgerSeq, b.IntraLedgerSeq),
		strings.Compare(a.TxHash, b.TxHash), cmp.Compare(a.OpIndex, b.OpIndex),
		cmp.Compare(a.ChangeIndex, b.ChangeIndex), strings.Compare(a.Role, b.Role))
}

func accountEntryChangeEntry(a clickhouse.AccountEntryChange) (AccountEntryChangeEntry, error) {
	entry := json.RawMessage(a.Fields)
	if !json.Valid(entry) {
		return AccountEntryChangeEntry{}, fmt.Errorf("ledger %d change %s/%d/%d: fields is not JSON", a.Ledger, a.TxHash, a.OpIndex, a.ChangeIndex)
	}
	e := AccountEntryChangeEntry{
		Ledger: a.Ledger, LedgerCloseTime: a.CloseTime.UTC().Format(time.RFC3339), IntraLedgerSeq: a.IntraLedgerSeq,
		TxHash: a.TxHash, OpIndex: a.OpIndex, ChangeIndex: a.ChangeIndex, Role: a.Role, EntryType: a.EntryType,
		ChangeType: a.ChangeType, Changed: a.Changed, Entry: entry,
	}
	if e.Changed == nil {
		e.Changed = []string{}
	}
	if a.Asset != "" && a.Balance != nil {
		e.Asset, e.Amount = a.Asset, a.Balance.String()
	}
	return e, nil
}

func accountEntryChangesNote(wm uint32, roles []string, lowerBound bool) string {
	note := fmt.Sprintf("entry changes where the account is %s, decoded from the ledger-entry-change lake through ledger %d; "+
		"ledgers %d to %d carry the earlier intra_ledger_seq numbering", strings.Join(roles, " or "), wm,
		entryChangesOldNumberingFrom, entryChangesOldNumberingThru)
	if lowerBound {
		note += "; this page is partial: follow next_cursor"
	}
	return note
}
