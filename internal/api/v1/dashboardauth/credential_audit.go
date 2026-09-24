package dashboardauth

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// audit_log actions for first-factor credential changes made from a
// dashboard session, and for passkey sign-ins refused on a theft signal.
// Each action's [obs.AdminAuditWriteFailuresTotal] surface is the action
// with "." replaced by "_"; internal/obs seeds every one of them.
const (
	AuditActionPasskeyRegister     = "passkey.register"
	AuditActionPasskeyDelete       = "passkey.delete"
	AuditActionPasskeyCloneWarning = "passkey.clone_warning"
	AuditActionPasskeyLoginReplay  = "passkey.login_replay"
	AuditActionKeyMint             = "key.mint"
	AuditActionKeyRevoke           = "key.revoke"
)

// CredentialEvent is one self-service change to a credential that can
// authenticate as the account.
type CredentialEvent struct {
	Action     string
	TargetKind string
	TargetID   string
	Metadata   map[string]any
}

// RecordSessionCredentialEvent appends ev to sink as an action taken by
// the session's user on their own account. Best-effort: the change has
// already committed, so a sink failure is logged and counted, never
// returned. A nil sink records nothing.
func RecordSessionCredentialEvent(
	r *http.Request, sink platform.AuditStore, logger *slog.Logger, now time.Time,
	sc SessionContext, ev CredentialEvent,
) {
	meta := map[string]any{"session_id": sc.Session.ID.String()}
	for k, v := range ev.Metadata {
		meta[k] = v
	}
	appendCredentialAudit(r, sink, logger, platform.AuditEntry{
		AccountID:   sc.User.AccountID,
		ActorUserID: sc.User.ID,
		ActorKind:   platform.ActorUser,
		Action:      ev.Action,
		TargetKind:  ev.TargetKind,
		TargetID:    ev.TargetID,
		Timestamp:   now,
	}, meta)
}

func (h *Handlers) recordPasskeyRegistered(r *http.Request, sc SessionContext, row platform.WebAuthnCredential) {
	RecordSessionCredentialEvent(r, h.cfg.Audit, h.cfg.Logger, h.cfg.Now(), sc, CredentialEvent{
		Action:     AuditActionPasskeyRegister,
		TargetKind: auditTargetPasskey,
		TargetID:   row.ID.String(),
		Metadata: map[string]any{
			"name":            row.Name,
			"transports":      row.Transports,
			"backup_eligible": row.BackupEligible,
		},
	})
}

// recordPasskeyLoginRefusal counts and audits a sign-in refused after
// the assertion signature verified. The presenter is unauthenticated,
// so the row is a system action on the credential owner's account with
// no actor user: attributing it to the owner would name the victim.
func (h *Handlers) recordPasskeyLoginRefusal(
	r *http.Request, action, reason string, owner platform.User, cred platform.WebAuthnCredential, presentedSignCount uint32,
) {
	obs.PasskeyLoginRefusalsTotal.WithLabelValues(reason).Inc()
	appendCredentialAudit(r, h.cfg.Audit, h.cfg.Logger, platform.AuditEntry{
		AccountID:  owner.AccountID,
		ActorKind:  platform.ActorSystem,
		Action:     action,
		TargetKind: auditTargetPasskey,
		TargetID:   cred.ID.String(),
		Timestamp:  h.cfg.Now(),
	}, map[string]any{
		"credential_owner_user_id": owner.ID.String(),
		"credential_name":          cred.Name,
		"stored_sign_count":        cred.SignCount,
		"presented_sign_count":     presentedSignCount,
	})
}

// auditTargetPasskey is the audit_log target_kind for a passkey row.
const auditTargetPasskey = "webauthn_credential"

func appendCredentialAudit(
	r *http.Request, sink platform.AuditStore, logger *slog.Logger, entry platform.AuditEntry, meta map[string]any,
) {
	if sink == nil {
		return
	}
	surface := strings.ReplaceAll(entry.Action, ".", "_")
	raw, err := json.Marshal(meta)
	if err != nil {
		obs.AdminAuditWriteFailuresTotal.WithLabelValues(surface).Inc()
		logger.Warn("credential audit: metadata marshal failed (skipping audit row)",
			"err", err, "action", entry.Action, "target_id", entry.TargetID)
		return
	}
	entry.Metadata = raw
	entry.IP = clientIP(r)
	entry.UserAgent = truncateUA(r.UserAgent())
	if err := sink.Append(r.Context(), entry); err != nil {
		obs.AdminAuditWriteFailuresTotal.WithLabelValues(surface).Inc()
		logger.Warn("credential audit: append failed (best-effort)",
			"err", err, "action", entry.Action, "target_id", entry.TargetID, "account_id", entry.AccountID)
	}
}
