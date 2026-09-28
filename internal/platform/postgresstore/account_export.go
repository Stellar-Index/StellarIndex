package postgresstore

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/pgarray"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// staffExportRedactedKeys are removed from the metadata of every audit row
// the requester did not write: another person's session and address.
var staffExportRedactedKeys = []string{"actor_email", "session_id", "actor_user_id"}

// ExportAccount reads everything [platform.AccountExport] describes for
// accountID, as requester sees it. Redis-held self-service keys are not
// in Postgres; the caller appends them.
func (r *AccountStore) ExportAccount(
	ctx context.Context, accountID, requester uuid.UUID, now time.Time,
) (platform.AccountExport, error) {
	out := platform.AccountExport{SchemaVersion: platform.AccountExportSchemaVersion, GeneratedAt: now.UTC()}
	if err := r.exportAccountRow(ctx, accountID, &out.Account); err != nil {
		return platform.AccountExport{}, err
	}
	readers := []struct {
		what string
		fn   func() error
	}{
		{"users", func() (err error) { out.Users, err = r.exportUsers(ctx, accountID, requester); return }},
		{"sessions", func() (err error) { out.Sessions, err = r.exportSessions(ctx, requester); return }},
		{"passkeys", func() (err error) { out.Passkeys, err = r.exportPasskeys(ctx, requester); return }},
		{"api keys", func() (err error) { out.APIKeys, err = r.exportAPIKeys(ctx, accountID); return }},
		{"webhooks", func() (err error) { out.Webhooks, err = r.exportWebhooks(ctx, accountID); return }},
		{"price alerts", func() (err error) { out.PriceAlerts, err = r.exportPriceAlerts(ctx, accountID); return }},
		{"invites", func() (err error) { out.Invites, err = r.exportInvites(ctx, accountID); return }},
		{"usage", func() (err error) {
			out.Usage, err = r.exportUsage(ctx, out.Account, out.APIKeys)
			return
		}},
		{"audit log", func() (err error) { out.AuditLog, err = r.exportAudit(ctx, accountID, requester); return }},
	}
	for _, rd := range readers {
		if err := rd.fn(); err != nil {
			return platform.AccountExport{}, fmt.Errorf("export account: %s: %w", rd.what, err)
		}
	}
	return out, nil
}

func (r *AccountStore) exportAccountRow(ctx context.Context, id uuid.UUID, a *platform.ExportAccount) error {
	var suspendedAt sql.NullTime
	var reason sql.NullString
	var rate, quota sql.NullInt64
	err := r.s.db.QueryRowContext(ctx, `
		SELECT id::text, name, slug, billing_email::text, tier, status, created_at,
		       suspended_at, suspended_reason, rate_limit_per_min_override, monthly_request_quota_override
		  FROM accounts WHERE id = $1`, id).
		Scan(&a.ID, &a.Name, &a.Slug, &a.BillingEmail, &a.Tier, &a.Status, &a.CreatedAt,
			&suspendedAt, &reason, &rate, &quota)
	if errors.Is(err, sql.ErrNoRows) {
		return platform.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("export account: account: %w", err)
	}
	a.CreatedAt = a.CreatedAt.UTC()
	a.SuspendedAt = nullTimePtr(suspendedAt)
	a.SuspendedReason = reason.String
	a.RateLimitPerMinOverride = nullInt64Ptr(rate)
	a.MonthlyRequestQuotaOverride = nullInt64Ptr(quota)
	return nil
}

func (r *AccountStore) exportUsers(ctx context.Context, accountID, requester uuid.UUID) ([]platform.ExportUser, error) {
	rows, err := r.s.db.QueryContext(ctx, `
		SELECT id, email::text, COALESCE(display_name, ''), role, email_verified_at, last_login_at,
		       mfa_enabled, created_at
		  FROM users WHERE account_id = $1 ORDER BY created_at`, accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []platform.ExportUser{}
	for rows.Next() {
		var u platform.ExportUser
		var id uuid.UUID
		var verified, lastLogin sql.NullTime
		if err := rows.Scan(&id, &u.Email, &u.DisplayName, &u.Role, &verified, &lastLogin,
			&u.MFAEnabled, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.ID, u.IsRequester = id.String(), id == requester
		u.EmailVerifiedAt, u.LastLoginAt = nullTimePtr(verified), nullTimePtr(lastLogin)
		u.CreatedAt = u.CreatedAt.UTC()
		out = append(out, u)
	}
	return out, rows.Err()
}

func (r *AccountStore) exportSessions(ctx context.Context, requester uuid.UUID) ([]platform.ExportSession, error) {
	rows, err := r.s.db.QueryContext(ctx, `
		SELECT id::text, created_at, last_seen_at, expires_at, revoked_at,
		       host(ip_first_seen), host(ip_last_seen), user_agent,
		       COALESCE(geo_first_seen, ''), COALESCE(geo_last_seen, '')
		  FROM sessions WHERE user_id = $1 ORDER BY created_at`, requester)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []platform.ExportSession{}
	for rows.Next() {
		var s platform.ExportSession
		var revoked sql.NullTime
		if err := rows.Scan(&s.ID, &s.CreatedAt, &s.LastSeenAt, &s.ExpiresAt, &revoked,
			&s.IPFirstSeen, &s.IPLastSeen, &s.UserAgent, &s.GeoFirstSeen, &s.GeoLastSeen); err != nil {
			return nil, err
		}
		s.CreatedAt, s.LastSeenAt, s.ExpiresAt = s.CreatedAt.UTC(), s.LastSeenAt.UTC(), s.ExpiresAt.UTC()
		s.RevokedAt = nullTimePtr(revoked)
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *AccountStore) exportPasskeys(ctx context.Context, requester uuid.UUID) ([]platform.ExportPasskey, error) {
	rows, err := r.s.db.QueryContext(ctx, `
		SELECT id::text, name, aaguid, transports, backup_eligible, backup_state, created_at, last_used_at
		  FROM webauthn_credentials WHERE user_id = $1 ORDER BY created_at`, requester)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []platform.ExportPasskey{}
	for rows.Next() {
		var p platform.ExportPasskey
		var aaguid []byte
		var lastUsed sql.NullTime
		if err := rows.Scan(&p.ID, &p.Name, &aaguid, pgarray.Strings(&p.Transports),
			&p.BackupEligible, &p.BackupState, &p.CreatedAt, &lastUsed); err != nil {
			return nil, err
		}
		if len(aaguid) > 0 {
			p.AAGUID = hex.EncodeToString(aaguid)
		}
		if p.Transports == nil {
			p.Transports = []string{}
		}
		p.CreatedAt, p.LastUsedAt = p.CreatedAt.UTC(), nullTimePtr(lastUsed)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *AccountStore) exportAPIKeys(ctx context.Context, accountID uuid.UUID) ([]platform.ExportAPIKey, error) {
	rows, err := r.s.db.QueryContext(ctx, `
		SELECT id, key_prefix, name, tier, scopes, rate_limit_per_min, COALESCE(monthly_quota, 0),
		       ip_allowlist::text[], referer_allowlist, created_at, expires_at, revoked_at,
		       COALESCE(revoked_reason, ''), last_used_at, COALESCE(host(last_used_ip), ''),
		       COALESCE(last_used_user_agent, '')
		  FROM api_keys WHERE account_id = $1 ORDER BY created_at LIMIT $2`, accountID, accountKeysReadCap)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []platform.ExportAPIKey{}
	for rows.Next() {
		k := platform.ExportAPIKey{Store: "postgres"}
		var expires, revoked, lastUsed sql.NullTime
		if err := rows.Scan(&k.ID, &k.Prefix, &k.Name, &k.Tier, pgarray.Strings(&k.Scopes),
			&k.RateLimitPerMin, &k.MonthlyQuota, pgarray.Strings(&k.IPAllowlist),
			pgarray.Strings(&k.RefererAllowlist), &k.CreatedAt, &expires, &revoked,
			&k.RevokedReason, &lastUsed, &k.LastUsedIP, &k.LastUsedUserAgent); err != nil {
			return nil, err
		}
		if k.Scopes == nil {
			k.Scopes = []string{}
		}
		k.CreatedAt = k.CreatedAt.UTC()
		k.ExpiresAt, k.RevokedAt, k.LastUsedAt = nullTimePtr(expires), nullTimePtr(revoked), nullTimePtr(lastUsed)
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) >= accountKeysReadCap {
		return nil, fmt.Errorf("more than %d api_keys rows", accountKeysReadCap-1)
	}
	return out, nil
}

func (r *AccountStore) exportWebhooks(ctx context.Context, accountID uuid.UUID) ([]platform.ExportWebhook, error) {
	out, err := r.exportWebhookRows(ctx, accountID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		id, err := uuid.Parse(out[i].ID)
		if err != nil {
			return nil, err
		}
		if out[i].Deliveries, err = r.exportDeliveries(ctx, id); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (r *AccountStore) exportWebhookRows(ctx context.Context, accountID uuid.UUID) ([]platform.ExportWebhook, error) {
	rows, err := r.s.db.QueryContext(ctx, `
		SELECT id::text, name, url, events, enabled, created_at, updated_at
		  FROM customer_webhooks WHERE account_id = $1 ORDER BY created_at`, accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []platform.ExportWebhook{}
	for rows.Next() {
		var w platform.ExportWebhook
		if err := rows.Scan(&w.ID, &w.Name, &w.URL, pgarray.Strings(&w.Events), &w.Enabled,
			&w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, err
		}
		w.CreatedAt, w.UpdatedAt = w.CreatedAt.UTC(), w.UpdatedAt.UTC()
		out = append(out, w)
	}
	return out, rows.Err()
}

func (r *AccountStore) exportDeliveries(ctx context.Context, webhookID uuid.UUID) ([]platform.ExportWebhookDelivery, error) {
	rows, err := r.s.db.QueryContext(ctx, `
		SELECT id::text, event_type, payload::text, attempt_count, delivered_at,
		       COALESCE(last_error, ''), COALESCE(last_response_status, 0), created_at
		  FROM webhook_deliveries WHERE webhook_id = $1 ORDER BY created_at`, webhookID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []platform.ExportWebhookDelivery{}
	for rows.Next() {
		var d platform.ExportWebhookDelivery
		var payload string
		var delivered sql.NullTime
		if err := rows.Scan(&d.ID, &d.EventType, &payload, &d.AttemptCount, &delivered,
			&d.LastError, &d.LastResponseStatus, &d.CreatedAt); err != nil {
			return nil, err
		}
		d.Payload = json.RawMessage(payload)
		d.DeliveredAt, d.CreatedAt = nullTimePtr(delivered), d.CreatedAt.UTC()
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *AccountStore) exportPriceAlerts(ctx context.Context, accountID uuid.UUID) ([]platform.ExportPriceAlert, error) {
	rows, err := r.s.db.QueryContext(ctx, `
		SELECT id::text, base_asset, quote_asset, condition, threshold::text, cooldown_seconds,
		       enabled, last_fired_at, created_at
		  FROM price_alerts WHERE account_id = $1 ORDER BY created_at`, accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []platform.ExportPriceAlert{}
	for rows.Next() {
		var a platform.ExportPriceAlert
		var fired sql.NullTime
		if err := rows.Scan(&a.ID, &a.BaseAsset, &a.QuoteAsset, &a.Condition, &a.Threshold,
			&a.CooldownSeconds, &a.Enabled, &fired, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.LastFiredAt, a.CreatedAt = nullTimePtr(fired), a.CreatedAt.UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *AccountStore) exportInvites(ctx context.Context, accountID uuid.UUID) ([]platform.ExportInvite, error) {
	rows, err := r.s.db.QueryContext(ctx, `
		SELECT email::text, role, created_at, expires_at, accepted_at, revoked_at
		  FROM invites WHERE account_id = $1 ORDER BY created_at`, accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []platform.ExportInvite{}
	for rows.Next() {
		var i platform.ExportInvite
		var accepted, revoked sql.NullTime
		if err := rows.Scan(&i.Email, &i.Role, &i.CreatedAt, &i.ExpiresAt, &accepted, &revoked); err != nil {
			return nil, err
		}
		i.CreatedAt, i.ExpiresAt = i.CreatedAt.UTC(), i.ExpiresAt.UTC()
		i.AcceptedAt, i.RevokedAt = nullTimePtr(accepted), nullTimePtr(revoked)
		out = append(out, i)
	}
	return out, rows.Err()
}

// exportUsage reads the account's usage_daily rows. Rows dated before the
// account existed belong to an earlier holder of the slug and are left out.
func (r *AccountStore) exportUsage(
	ctx context.Context, acct platform.ExportAccount, keys []platform.ExportAPIKey,
) ([]platform.ExportUsageDay, error) {
	ids := make([]string, 0, len(keys))
	for _, k := range keys {
		ids = append(ids, k.ID)
	}
	rows, err := r.s.db.QueryContext(ctx, `
		SELECT day::text, subject, endpoint, ok_count, client_error_count, server_error_count, throttled_count
		  FROM usage_daily
		 WHERE subject = ANY($1::text[]) AND day >= $2::date
		 ORDER BY day, subject, endpoint`, UsageSubjects(acct.Slug, ids...), acct.CreatedAt)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []platform.ExportUsageDay{}
	for rows.Next() {
		var u platform.ExportUsageDay
		if err := rows.Scan(&u.Day, &u.Subject, &u.Endpoint, &u.OKCount, &u.ClientErrorCount,
			&u.ServerErrorCount, &u.ThrottledCount); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (r *AccountStore) exportAudit(ctx context.Context, accountID, requester uuid.UUID) ([]platform.ExportAuditRecord, error) {
	rows, err := r.s.db.QueryContext(ctx, `
		SELECT ts, actor_kind, actor_user_id IS NOT DISTINCT FROM $2::uuid AS mine, action,
		       COALESCE(target_kind, ''), COALESCE(target_id, ''),
		       CASE WHEN actor_user_id IS NOT DISTINCT FROM $2::uuid THEN metadata
		            ELSE metadata - $3::text[] END::text,
		       CASE WHEN actor_user_id IS NOT DISTINCT FROM $2::uuid THEN COALESCE(host(ip), '') ELSE '' END,
		       CASE WHEN actor_user_id IS NOT DISTINCT FROM $2::uuid THEN COALESCE(user_agent, '') ELSE '' END
		  FROM audit_log
		 WHERE account_id = $1
		    OR (target_kind = 'account' AND target_id = $1::text)
		    OR actor_user_id IN (SELECT id FROM users WHERE account_id = $1)
		 ORDER BY ts`, accountID, requester, staffExportRedactedKeys)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []platform.ExportAuditRecord{}
	for rows.Next() {
		var a platform.ExportAuditRecord
		var meta sql.NullString
		if err := rows.Scan(&a.Timestamp, &a.ActorKind, &a.ByRequester, &a.Action, &a.TargetKind,
			&a.TargetID, &meta, &a.IP, &a.UserAgent); err != nil {
			return nil, err
		}
		a.Timestamp = a.Timestamp.UTC()
		if meta.Valid {
			a.Metadata = json.RawMessage(meta.String)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func nullTimePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}

func nullInt64Ptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}
