package explorer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// The caps bound one read to ~1.2 s on r1 for the heaviest claimant (a spam target with ~7k claimant ledgers);
// an account past either cap is served as a lower bound.
const (
	claimableLedgerCap = 200
	claimableKeyCap    = 500
)

// AccountClaimableBalance is one unclaimed claimable balance the account can claim.
type AccountClaimableBalance struct {
	BalanceID string `json:"balance_id"`
	Asset     string `json:"asset"`
	// Amount is a decimal string of stroops (ADR-0003).
	Amount             string          `json:"amount"`
	Sponsor            string          `json:"sponsor,omitempty"`
	LastModifiedLedger uint32          `json:"last_modified_ledger"`
	LastModifiedTime   string          `json:"last_modified_time"`
	Entry              json.RawMessage `json:"entry"`
}

// AccountClaimableBalancesView is /v1/accounts/{g_strkey}/claimable-balances.
type AccountClaimableBalancesView struct {
	Account           string                    `json:"account"`
	ClaimableBalances []AccountClaimableBalance `json:"claimable_balances"`
	ThroughLedger     uint32                    `json:"through_ledger"`
	LowerBound        bool                      `json:"lower_bound"`
	CoverageNote      string                    `json:"coverage_note"`
}

// AccountClaimableBalances serves GET /v1/accounts/{g_strkey}/claimable-balances: the unclaimed claimable balances
// that name the account as a claimant, newest first: from the claimable index once it is built, otherwise located through
// the entry index's claimant role.
func (h *Handler) AccountClaimableBalances(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	account, ok := h.parseAccountStrkey(w, r)
	if !ok {
		return
	}
	raw, err := strkey.Decode(strkey.VersionByteAccountID, account)
	if err != nil {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-account", "Invalid account", http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()
	cwm, err := h.Reader.ClaimableIndexWatermark(ctx)
	if err != nil {
		h.accountClaimableError(w, r, ctx, err, account)
		return
	}
	if cwm > 0 {
		scan, err := h.Reader.IndexedClaimableBalances(ctx, string(raw), claimableKeyCap)
		if err != nil {
			h.accountClaimableError(w, r, ctx, err, account)
			return
		}
		h.writeAccountClaimables(w, r, ctx, account, cwm, scan, scan.Keys >= claimableKeyCap, true)
		return
	}
	wm, err := h.Reader.EntryIndexWatermark(ctx)
	if err != nil {
		h.accountClaimableError(w, r, ctx, err, account)
		return
	}
	if wm == 0 {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/entry-index-not-built", "Claimable balances not yet derived",
			http.StatusServiceUnavailable, "the entry index has not been built on this deployment yet; retry later")
		return
	}
	ledgers, err := h.Reader.AccountEntryLedgers(ctx, string(raw), []string{clickhouse.EntryIndexRoleClaimant}, wm, claimableLedgerCap)
	if err != nil {
		h.accountClaimableError(w, r, ctx, err, account)
		return
	}
	scan, err := h.Reader.LiveClaimableBalancesInLedgers(ctx, string(raw), ledgers, claimableKeyCap)
	if err != nil {
		h.accountClaimableError(w, r, ctx, err, account)
		return
	}
	h.writeAccountClaimables(w, r, ctx, account, wm, scan, len(ledgers) >= claimableLedgerCap || scan.Keys >= claimableKeyCap, false)
}

func (h *Handler) writeAccountClaimables(w http.ResponseWriter, r *http.Request, ctx context.Context, account string, wm uint32,
	scan clickhouse.ClaimableScan, capped, indexed bool,
) {
	balances, err := claimableBalancesFor(account, scan.Live)
	if err != nil {
		h.accountClaimableError(w, r, ctx, err, account)
		return
	}
	out := AccountClaimableBalancesView{
		Account: account, ClaimableBalances: balances, ThroughLedger: wm, LowerBound: capped || scan.Unresolved > 0,
		CoverageNote: accountClaimableNote(wm, capped, indexed, scan.Unresolved),
	}
	h.writeJSONAt(w, out, h.movementsStale(ctx, wm), false, time.Time{})
}

// claimableBalancesFor keeps the balances whose claimants include the account; the reader's byte prefilter also
// matches an account that appears only as sponsor or inside an asset issuer.
func claimableBalancesFor(account string, live []clickhouse.LiveClaimableBalance) ([]AccountClaimableBalance, error) {
	out := make([]AccountClaimableBalance, 0, len(live))
	for _, c := range live {
		if !slices.Contains(c.Claimants, account) {
			continue
		}
		fields := c.Fields
		entry, err := json.Marshal(fields)
		if err != nil {
			return nil, fmt.Errorf("ledger %d: encode claimable balance: %w", c.Ledger, err)
		}
		b := AccountClaimableBalance{
			BalanceID: fmt.Sprint(fields["balance_id"]), Asset: fmt.Sprint(fields["asset"]), Amount: fmt.Sprint(fields["amount"]),
			LastModifiedLedger: c.Ledger, LastModifiedTime: c.CloseTime.UTC().Format(time.RFC3339), Entry: entry,
		}
		if s, ok := fields["sponsor"].(string); ok {
			b.Sponsor = s
		}
		out = append(out, b)
	}
	return out, nil
}

func (h *Handler) accountClaimableError(w http.ResponseWriter, r *http.Request, ctx context.Context, err error, account string) {
	if h.ClientAborted(r, err) {
		return
	}
	if retryableColdMiss(ctx, err) {
		h.Logger.Warn("explorer AccountClaimableBalances deadline exceeded", "account", account)
		h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/account-claimable-balances-timeout",
			"Account claimable balances timed out")
		return
	}
	h.Logger.Error("explorer AccountClaimableBalances failed", "err", err, "account", account)
	h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal", "Internal error", http.StatusInternalServerError, "")
}

func accountClaimableNote(wm uint32, capped, indexed bool, unresolved int) string {
	note := fmt.Sprintf("unclaimed claimable balances naming the account as a claimant, through ledger %d", wm)
	if unresolved > 0 {
		note += fmt.Sprintf("; %d balance(s) naming the account have no current-state row in the lake, so whether they "+
			"are still unclaimed is unknown and they are left out", unresolved)
	}
	switch {
	case capped && indexed:
		note += fmt.Sprintf("; only the account's %d most recently modified unclaimed balances are listed, so older ones are left out",
			claimableKeyCap)
	case capped:
		note += fmt.Sprintf("; only the account's %d most recent claimant ledgers (at most %d balances) were examined, "+
			"so older unclaimed balances may be missing", claimableLedgerCap, claimableKeyCap)
	}
	return note
}
