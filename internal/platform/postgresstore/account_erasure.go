package postgresstore

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// ErrErasureBlocked is returned by [AccountStore.PlanErasure] for an
// account an automated erasure must not touch: billing state exists (no
// writer on main, so it is handled by hand) or a member is staff.
var ErrErasureBlocked = errors.New("account erasure blocked")

// accountKeysReadCap bounds the whole-history api_keys reads erasure and
// export need (revoked rows included; see TestAPIKeysQueriesAreBounded).
// Reaching it is an error, never a silently short list.
const accountKeysReadCap = 100_000

// ErasurePlan is what an erasure collects before it deletes anything.
type ErasurePlan struct {
	AccountID    uuid.UUID
	Slug         string
	BillingEmail string
	// OwnerEmails are the addresses of the account's owner users, for the
	// confirmation mail.
	OwnerEmails []string
	// Emails is every member address plus the billing address, lowercased.
	Emails  []string
	UserIDs []uuid.UUID
	// KeyIDs and KeyHashes are the account's Postgres api_keys rows; the
	// hashes (hex) address the Redis validator records and cache rows.
	KeyIDs    []string
	KeyHashes []string
}

// ErasureRequest is one [AccountStore.EraseAccount] call.
type ErasureRequest struct {
	Plan ErasurePlan
	// ExtraKeyIDs are key ids held only in Redis (self-service keys), so
	// their audit rows and key:<id> usage rows are scrubbed too.
	ExtraKeyIDs []string
	// ErasedSubject replaces every usage_daily subject of the account.
	ErasedSubject string
	// Actor is recorded on the account.erase audit row.
	Actor platform.ActorKind
}

// ErasureCounts is what one EraseAccount removed, recorded on the
// account.erase audit row.
type ErasureCounts struct {
	AuditRowsScrubbed int64 `json:"audit_rows_scrubbed"`
	UsageRowsRenamed  int64 `json:"usage_rows_renamed"`
	Invites           int64 `json:"invites"`
	PriceAlerts       int64 `json:"price_alerts"`
	Webhooks          int64 `json:"webhooks"`
	APIKeys           int64 `json:"api_keys"`
	UsageEvents       int64 `json:"api_usage_events"`
	MagicLinkTokens   int64 `json:"magic_link_tokens"`
	LoginLockouts     int64 `json:"login_code_lockouts"`
	Users             int64 `json:"users"`
}

// UsageSubjects are the usage_daily / Redis usage subjects an account's
// traffic is counted under: the account-level id:acct:<slug> and the
// per-key key:<id> fallback.
func UsageSubjects(slug string, keyIDs ...string) []string {
	out := []string{"id:acct:" + slug}
	for _, k := range keyIDs {
		out = append(out, "key:"+k)
	}
	return out
}

// PlanErasure reads what an erasure of id will remove. platform.ErrNotFound
// means the account is already gone.
func (r *AccountStore) PlanErasure(ctx context.Context, id uuid.UUID) (ErasurePlan, error) {
	p := ErasurePlan{AccountID: id}
	var stripe sql.NullString
	var subs, staff int
	err := r.s.db.QueryRowContext(ctx, `
		SELECT a.slug, a.billing_email::text, a.stripe_customer_id,
		       (SELECT count(*) FROM subscriptions s WHERE s.account_id = a.id),
		       (SELECT count(*) FROM users u WHERE u.account_id = a.id AND u.is_staff)
		  FROM accounts a WHERE a.id = $1`, id).
		Scan(&p.Slug, &p.BillingEmail, &stripe, &subs, &staff)
	if errors.Is(err, sql.ErrNoRows) {
		return ErasurePlan{}, platform.ErrNotFound
	}
	if err != nil {
		return ErasurePlan{}, fmt.Errorf("plan erasure: account: %w", err)
	}
	switch {
	case stripe.Valid || subs > 0:
		return ErasurePlan{}, fmt.Errorf("%w: the account has billing state; erase it by hand (runbook)", ErrErasureBlocked)
	case staff > 0:
		return ErasurePlan{}, fmt.Errorf("%w: a member is staff", ErrErasureBlocked)
	}
	p.Emails = append(p.Emails, strings.ToLower(p.BillingEmail))

	rows, err := r.s.db.QueryContext(ctx,
		`SELECT id, email::text, role FROM users WHERE account_id = $1 ORDER BY created_at`, id)
	if err != nil {
		return ErasurePlan{}, fmt.Errorf("plan erasure: users: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var uid uuid.UUID
		var email, role string
		if err := rows.Scan(&uid, &email, &role); err != nil {
			return ErasurePlan{}, fmt.Errorf("plan erasure: users scan: %w", err)
		}
		p.UserIDs = append(p.UserIDs, uid)
		p.Emails = append(p.Emails, strings.ToLower(email))
		if platform.Role(role) == platform.RoleOwner {
			p.OwnerEmails = append(p.OwnerEmails, email)
		}
	}
	if err := rows.Err(); err != nil {
		return ErasurePlan{}, fmt.Errorf("plan erasure: users: %w", err)
	}

	keys, err := r.s.db.QueryContext(ctx,
		`SELECT id, key_hash FROM api_keys WHERE account_id = $1 LIMIT $2`, id, accountKeysReadCap)
	if err != nil {
		return ErasurePlan{}, fmt.Errorf("plan erasure: keys: %w", err)
	}
	defer func() { _ = keys.Close() }()
	for keys.Next() {
		var kid string
		var hash []byte
		if err := keys.Scan(&kid, &hash); err != nil {
			return ErasurePlan{}, fmt.Errorf("plan erasure: keys scan: %w", err)
		}
		p.KeyIDs = append(p.KeyIDs, kid)
		p.KeyHashes = append(p.KeyHashes, hex.EncodeToString(hash))
	}
	if err := keys.Err(); err != nil {
		return ErasurePlan{}, fmt.Errorf("plan erasure: keys: %w", err)
	}
	if len(p.KeyIDs) >= accountKeysReadCap {
		return ErasurePlan{}, fmt.Errorf("%w: more than %d api_keys rows", ErrErasureBlocked, accountKeysReadCap-1)
	}
	return p, nil
}

// EraseAccount closes the account and deletes or scrubs every row that
// holds it, in ONE transaction: a failure leaves the account exactly as
// it was (still active, so its owner can retry), and success leaves no
// half-erased state for the session middleware to lock the owner out of.
// platform.ErrNotFound means the account is already gone.
//
// Order matters: the audit scrub runs while the account is closed but
// its users and keys still exist (the 0188 trigger checks linkage), the
// explicit deletes run child-first so the RESTRICT foreign keys stay a
// guard against a missed table, and the slug tombstone and the
// account.erase row land in the same commit.
func (r *AccountStore) EraseAccount(ctx context.Context, req ErasureRequest) (ErasureCounts, error) {
	p := req.Plan
	var c ErasureCounts
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return c, fmt.Errorf("erase account: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var slug string
	err = tx.QueryRowContext(ctx, `SELECT slug FROM accounts WHERE id = $1 FOR UPDATE`, p.AccountID).Scan(&slug)
	if errors.Is(err, sql.ErrNoRows) {
		return c, platform.ErrNotFound
	}
	if err != nil {
		return c, fmt.Errorf("erase account: lock: %w", err)
	}
	if slug != p.Slug {
		return c, fmt.Errorf("erase account: slug changed since the plan (%w)", platform.ErrConflict)
	}
	users := uuidStrings(p.UserIDs)
	keyIDs := append(append([]string{}, p.KeyIDs...), req.ExtraKeyIDs...)
	identifier := "acct:" + p.Slug

	steps := []struct {
		n    *int64
		what string
		q    string
		args []any
	}{
		{nil, "close", `UPDATE accounts SET status = 'closed' WHERE id = $1`, []any{p.AccountID}},
		{nil, "guc", `SELECT set_config('stellarindex.erasing_account', $1, true)`, []any{p.AccountID.String()}},
		{
			&c.AuditRowsScrubbed, "audit scrub", `
			UPDATE audit_log
			   SET metadata   = audit_log_erase_metadata(metadata),
			       ip         = CASE WHEN actor_kind = 'staff' THEN ip END,
			       user_agent = CASE WHEN actor_kind = 'staff' THEN user_agent END
			 WHERE (account_id = $1
			        OR (target_kind = 'account' AND target_id = $1::text)
			        OR actor_user_id = ANY($2::uuid[])
			        OR (target_kind = 'api_key' AND target_id = ANY($3::text[]))
			        OR metadata ->> 'target_identifier' = $4
			        OR metadata ->> 'actor_identifier' = $4)
			   AND (metadata IS DISTINCT FROM audit_log_erase_metadata(metadata)
			        OR (actor_kind <> 'staff' AND (ip IS NOT NULL OR user_agent IS NOT NULL)))`,
			[]any{p.AccountID, users, keyIDs, identifier},
		},
		{
			&c.Invites, "invites", `
			DELETE FROM invites
			 WHERE account_id = $1 OR invited_by_user_id = ANY($2::uuid[]) OR lower(email::text) = ANY($3::text[])`,
			[]any{p.AccountID, users, p.Emails},
		},
		{&c.PriceAlerts, "price alerts", `DELETE FROM price_alerts WHERE account_id = $1`, []any{p.AccountID}},
		{&c.Webhooks, "webhooks", `DELETE FROM customer_webhooks WHERE account_id = $1`, []any{p.AccountID}},
		{&c.APIKeys, "api keys", `DELETE FROM api_keys WHERE account_id = $1`, []any{p.AccountID}},
		{&c.UsageEvents, "usage events", `DELETE FROM api_usage_events WHERE account_id = $1`, []any{p.AccountID}},
		{&c.MagicLinkTokens, "magic links", `DELETE FROM magic_link_tokens WHERE lower(email::text) = ANY($1::text[])`, []any{p.Emails}},
		{&c.LoginLockouts, "lockouts", `DELETE FROM login_code_lockouts WHERE lower(email) = ANY($1::text[])`, []any{p.Emails}},
		{&c.Users, "users", `DELETE FROM users WHERE account_id = $1`, []any{p.AccountID}},
		{nil, "account", `DELETE FROM accounts WHERE id = $1`, []any{p.AccountID}},
		{nil, "tombstone", `
			INSERT INTO erased_account_slugs (slug_sha256)
			VALUES (sha256(convert_to($1::text, 'UTF8'))) ON CONFLICT DO NOTHING`, []any{p.Slug}},
	}
	for _, s := range steps {
		res, err := tx.ExecContext(ctx, s.q, s.args...)
		if err != nil {
			return ErasureCounts{}, fmt.Errorf("erase account: %s: %w", s.what, err)
		}
		if s.n != nil {
			*s.n, _ = res.RowsAffected()
		}
	}
	n, err := renameUsageSubjects(ctx, tx, UsageSubjects(p.Slug, keyIDs...), req.ErasedSubject)
	if err != nil {
		return ErasureCounts{}, err
	}
	c.UsageRowsRenamed = n

	meta, err := json.Marshal(c)
	if err != nil {
		return ErasureCounts{}, fmt.Errorf("erase account: audit metadata: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_log (actor_kind, action, target_kind, target_id, metadata)
		VALUES ($1, 'account.erase', 'account', $2, $3::jsonb)`,
		string(req.Actor), p.AccountID.String(), string(meta)); err != nil {
		return ErasureCounts{}, fmt.Errorf("erase account: audit row: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ErasureCounts{}, fmt.Errorf("erase account: commit: %w", err)
	}
	return c, nil
}

// RenameUsageSubjects moves usage_daily rows from subjects to erased,
// merging into rows already there. Erasure re-runs it after its commit:
// a rollup sweep that read the Redis counters before they were deleted
// can upsert an old subject afterwards.
func (r *AccountStore) RenameUsageSubjects(ctx context.Context, subjects []string, erased string) (int64, error) {
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("rename usage subjects: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	n, err := renameUsageSubjects(ctx, tx, subjects, erased)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("rename usage subjects: commit: %w", err)
	}
	return n, nil
}

// renameUsageSubjects sums the old subjects' counters per (day, endpoint)
// into erased — they are distinct counters — and merges with GREATEST
// into an erased row a previous pass wrote, since that row already holds
// the same counters.
func renameUsageSubjects(ctx context.Context, tx *sql.Tx, subjects []string, erased string) (int64, error) {
	if erased == "" || !strings.HasPrefix(erased, "erased:") {
		return 0, fmt.Errorf("rename usage subjects: target %q is not an erased:<id> subject", erased)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO usage_daily (day, subject, endpoint, ok_count, client_error_count,
		                         server_error_count, throttled_count, updated_at)
		SELECT day, $2, endpoint, sum(ok_count), sum(client_error_count),
		       sum(server_error_count), sum(throttled_count), now()
		  FROM usage_daily WHERE subject = ANY($1::text[])
		 GROUP BY day, endpoint
		ON CONFLICT (day, subject, endpoint) DO UPDATE SET
		       ok_count           = GREATEST(usage_daily.ok_count, EXCLUDED.ok_count),
		       client_error_count = GREATEST(usage_daily.client_error_count, EXCLUDED.client_error_count),
		       server_error_count = GREATEST(usage_daily.server_error_count, EXCLUDED.server_error_count),
		       throttled_count    = GREATEST(usage_daily.throttled_count, EXCLUDED.throttled_count),
		       updated_at         = now()`, subjects, erased); err != nil {
		return 0, fmt.Errorf("rename usage subjects: merge: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM usage_daily WHERE subject = ANY($1::text[])`, subjects)
	if err != nil {
		return 0, fmt.Errorf("rename usage subjects: delete: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// SlugErased reports whether an erasure retired slug.
func (r *AccountStore) SlugErased(ctx context.Context, slug string) (bool, error) {
	var ok bool
	err := r.s.db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM erased_account_slugs
		                WHERE slug_sha256 = sha256(convert_to($1::text, 'UTF8')))`, slug).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("slug erased: %w", err)
	}
	return ok, nil
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}
