package explorer

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// One read examines the key's most recent signer ledgers and at most this many candidate accounts; past either cap
// the list is a lower bound.
const (
	signerOnLedgerCap  = 200
	signerOnAccountCap = 500
)

// AccountSignerOn is one account the key currently signs for.
type AccountSignerOn struct {
	Account            string `json:"account"`
	Weight             uint32 `json:"weight"`
	LastModifiedLedger uint32 `json:"last_modified_ledger"`
}

// AccountSignerOnView is /v1/accounts/{g_strkey}/signer-on.
type AccountSignerOnView struct {
	Account       string            `json:"account"`
	SignerOn      []AccountSignerOn `json:"signer_on"`
	ThroughLedger uint32            `json:"through_ledger"`
	LowerBound    bool              `json:"lower_bound"`
	CoverageNote  string            `json:"coverage_note"`
}

// AccountSignerOn serves GET /v1/accounts/{g_strkey}/signer-on: the other accounts whose current signer list holds
// this account's key, located through the entry index's signer role.
func (h *Handler) AccountSignerOn(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	account, ok := h.parseAccountStrkey(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()
	wm, err := h.Reader.SignerIndexWatermark(ctx)
	if err != nil {
		h.accountSignerOnError(w, r, ctx, err, account)
		return
	}
	if wm == 0 {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/signer-index-not-built", "Signer index not yet built",
			http.StatusServiceUnavailable, "the entry index's signer role has not been built from the lake floor on this deployment yet; retry later")
		return
	}
	raw, err := strkey.Decode(strkey.VersionByteAccountID, account)
	if err != nil {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-account", "Invalid account", http.StatusBadRequest, err.Error())
		return
	}
	ledgers, err := h.Reader.AccountEntryLedgers(ctx, string(raw), []string{clickhouse.EntryIndexRoleSigner}, wm, signerOnLedgerCap)
	if err != nil {
		h.accountSignerOnError(w, r, ctx, err, account)
		return
	}
	scan, err := h.Reader.AccountsSignedBy(ctx, string(raw), ledgers, signerOnAccountCap)
	if err != nil {
		h.accountSignerOnError(w, r, ctx, err, account)
		return
	}
	out := AccountSignerOnView{Account: account, SignerOn: make([]AccountSignerOn, 0, len(scan.Signed)), ThroughLedger: wm}
	for _, s := range scan.Signed {
		out.SignerOn = append(out.SignerOn, AccountSignerOn{Account: s.Account, Weight: s.Weight, LastModifiedLedger: s.Ledger})
	}
	capped := len(ledgers) >= signerOnLedgerCap || scan.Candidates >= signerOnAccountCap
	out.LowerBound = capped
	out.CoverageNote = fmt.Sprintf("accounts whose current signers include this account's key, from set_options operations through ledger %d", wm)
	if capped {
		out.CoverageNote += fmt.Sprintf("; only the key's %d most recent signer ledgers (at most %d accounts) were examined, so older "+
			"arrangements may be missing", signerOnLedgerCap, signerOnAccountCap)
	}
	h.writeJSONAt(w, out, h.movementsStale(ctx, wm), false, time.Time{})
}

func (h *Handler) accountSignerOnError(w http.ResponseWriter, r *http.Request, ctx context.Context, err error, account string) {
	if h.ClientAborted(r, err) {
		return
	}
	if retryableColdMiss(ctx, err) {
		h.Logger.Warn("explorer AccountSignerOn deadline exceeded", "account", account)
		h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/account-signer-on-timeout", "Account signer-on timed out")
		return
	}
	h.Logger.Error("explorer AccountSignerOn failed", "err", err, "account", account)
	h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal", "Internal error", http.StatusInternalServerError, "")
}
