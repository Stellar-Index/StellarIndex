package explorer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
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

// accountEntryPage walks the account's indexed ledgers and keeps the projector's rows for the account in the
// wanted roles.
func (h *Handler) accountEntryPage(ctx context.Context, key, account string, roles []string, limit int,
	cur clickhouse.AssetEntryChangeCursor, wm uint32,
) (entryChangesPage, error) {
	src := entryPageSource[clickhouse.AccountEntryChange]{
		ledgers: func(ctx context.Context, ceiling uint32) ([]uint32, error) {
			return h.Reader.AccountEntryLedgers(ctx, key, roles, ceiling, entryLedgerBatch)
		},
		stream: func(ctx context.Context, ledgers []uint32, fn func(clickhouse.EntryHistorySourceRow) error) error {
			return h.Reader.StreamAccountEntryChangesForLedgers(ctx, key, ledgers, fn)
		},
		keep: func(out, acct []clickhouse.AccountEntryChange, _ []clickhouse.AssetEntryChange) []clickhouse.AccountEntryChange {
			for _, a := range acct {
				if a.Account == account && slices.Contains(roles, a.Role) {
					out = append(out, a)
				}
			}
			return out
		},
		key: accountEntryKey,
	}
	page, err := walkEntryPage(ctx, src, limit, cur, wm)
	if err != nil {
		return entryChangesPage{}, err
	}
	out := entryChangesPage{rows: make([]AccountEntryChangeEntry, 0, len(page.rows)), next: page.next, lowerBound: page.lowerBound}
	for _, a := range page.rows {
		e, err := accountEntryChangeEntry(a)
		if err != nil {
			return entryChangesPage{}, err
		}
		out.rows = append(out.rows, e)
	}
	return out, nil
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
