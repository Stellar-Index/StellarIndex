package dashboardauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/accounterasure"
	"github.com/Stellar-Index/StellarIndex/internal/httpx"
	"github.com/Stellar-Index/StellarIndex/internal/notify"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// AccountEraser erases an account; *accounterasure.Eraser satisfies it.
type AccountEraser interface {
	Erase(ctx context.Context, id uuid.UUID, actor platform.ActorKind) (accounterasure.Report, error)
}

// AccountExporter builds an account's data export; *accounterasure.Exporter
// satisfies it.
type AccountExporter interface {
	Export(ctx context.Context, accountID, requester uuid.UUID, now time.Time) (platform.AccountExport, error)
}

const (
	// accountReauthWindow is how recently the session must have been
	// minted for an erasure or export: a session stolen days ago must not
	// be enough to destroy or download an account.
	accountReauthWindow = 10 * time.Minute
	// accountActionWindow, maxAccountErasures and maxAccountExports bound
	// attempts per user per instance.
	accountActionWindow = time.Hour
	maxAccountErasures  = 3
	maxAccountExports   = 5

	accountRoute       = "/v1/dashboard/account"
	accountExportRoute = "/v1/dashboard/account/export"

	// AuditActionAccountExport is the audit_log action for a data export.
	AuditActionAccountExport = "account.export"
)

// accountErasureRequest is the DELETE body: the account slug, typed.
type accountErasureRequest struct {
	Confirm string `json:"confirm"`
}

// HandleAccountDelete serves DELETE /v1/dashboard/account: erase the
// session's account and every member of it. Owner only, on a
// session minted within accountReauthWindow, with the slug typed back.
func (h *Handlers) HandleAccountDelete(w http.ResponseWriter, r *http.Request) {
	sc, ok := h.accountActionGate(w, r, accountRoute, "erase", maxAccountErasures)
	if !ok {
		return
	}
	var req accountErasureRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil || req.Confirm != sc.Account.Slug {
		writeProblem(w, http.StatusBadRequest,
			`confirm must be the account slug exactly: {"confirm":"<slug>"}`, accountRoute)
		return
	}
	rep, err := h.cfg.AccountEraser.Erase(r.Context(), sc.Account.ID, platform.ActorUser)
	switch {
	case errors.Is(err, accounterasure.ErrBlocked):
		httpx.WriteProblem(w, "https://api.stellarindex.io/errors/account-erasure-blocked",
			http.StatusConflict, "this account cannot be deleted from the dashboard; contact support", accountRoute)
		return
	case errors.Is(err, accounterasure.ErrCleanupIncomplete):
		// Committed: the account is gone, so the user is told so; an
		// operator finishes the Redis cleanup.
		h.cfg.Logger.Error("account erased but cleanup incomplete; run stellarindex-ops account-erase -finish-slug",
			"err", err, "account_id", sc.Account.ID, "finish_slug", rep.Plan.Slug)
	case err != nil:
		h.cfg.Logger.Error("account erasure failed", "err", err, "account_id", sc.Account.ID)
		writeProblem(w, http.StatusInternalServerError, "account deletion failed; retry", accountRoute)
		return
	}
	h.notifyAccountErased(r, rep.Plan.OwnerEmails)
	h.clearSessionCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

// HandleAccountExport serves GET /v1/dashboard/account/export: the
// session account's data as one JSON attachment. Owner only, on a session
// minted within accountReauthWindow.
func (h *Handlers) HandleAccountExport(w http.ResponseWriter, r *http.Request) {
	sc, ok := h.accountActionGate(w, r, accountExportRoute, "export", maxAccountExports)
	if !ok {
		return
	}
	doc, err := h.cfg.AccountExporter.Export(r.Context(), sc.Account.ID, sc.User.ID, h.cfg.Now())
	if err != nil {
		h.cfg.Logger.Error("account export failed", "err", err, "account_id", sc.Account.ID)
		writeProblem(w, http.StatusInternalServerError, "export failed; retry", accountExportRoute)
		return
	}
	RecordSessionCredentialEvent(r, h.cfg.Audit, h.cfg.Logger, h.cfg.Now(), sc, CredentialEvent{
		Action:     AuditActionAccountExport,
		TargetKind: "account",
		TargetID:   sc.Account.ID.String(),
		Metadata: map[string]any{
			"users": len(doc.Users), "api_keys": len(doc.APIKeys), "webhooks": len(doc.Webhooks),
			"audit_rows": len(doc.AuditLog), "usage_rows": len(doc.Usage),
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition",
		`attachment; filename="stellarindex-account-export-`+strconv.FormatInt(h.cfg.Now().Unix(), 10)+`.json"`)
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(doc); err != nil {
		h.cfg.Logger.Warn("account export: write failed", "err", err, "account_id", sc.Account.ID)
	}
}

// accountActionGate admits an owner whose session is recent and who is
// under the per-user attempt cap, writing the refusal otherwise.
func (h *Handlers) accountActionGate(
	w http.ResponseWriter, r *http.Request, route, action string, limit int,
) (SessionContext, bool) {
	sc, ok := SessionFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication required", route)
		return SessionContext{}, false
	}
	if sc.User.Role != platform.RoleOwner {
		writeProblem(w, http.StatusForbidden, "only an account owner can do this", route)
		return SessionContext{}, false
	}
	if h.cfg.Now().Sub(sc.Session.CreatedAt) > accountReauthWindow {
		httpx.WriteProblem(w, "https://api.stellarindex.io/errors/reauth-required",
			http.StatusUnauthorized, "sign in again, then retry within 10 minutes", route)
		return SessionContext{}, false
	}
	if !h.cfg.accountActions.Allow(action+":"+sc.User.ID.String(), limit) {
		w.Header().Set("Retry-After", strconv.Itoa(int(accountActionWindow.Seconds())))
		writeProblem(w, http.StatusTooManyRequests, "too many attempts; try again later", route)
		return SessionContext{}, false
	}
	return sc, true
}

// notifyAccountErased mails each owner address captured before the
// erasure. Best-effort: the erasure has committed.
func (h *Handlers) notifyAccountErased(r *http.Request, owners []string) {
	when := h.cfg.Now().UTC().Format("2 Jan 2006 15:04 UTC")
	for _, to := range owners {
		msg, err := notify.AccountErasedMessage(h.cfg.EmailFrom, to, notify.AccountErasedInput{When: when})
		if err == nil {
			sendCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), sendTimeout)
			err = h.cfg.Sender.Send(sendCtx, msg)
			cancel()
		}
		result := obs.NotifySendResultSent
		if errors.Is(err, notify.ErrSuppressed) {
			result = obs.NotifySendResultSuppressed
		} else if err != nil {
			result = obs.NotifySendResultFailed
			h.cfg.Logger.Error("account erased notice not sent", "err", err, "to", maskEmail(to))
		}
		obs.NotifySendsTotal.WithLabelValues(obs.NotifyTemplateAccountErased, result).Inc()
	}
}
