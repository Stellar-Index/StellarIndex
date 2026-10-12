package postgresstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Stellar-Index/StellarIndex/internal/pgarray"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// WebhookStore implements [platform.WebhookStore] against `customer_webhooks` and `webhook_deliveries`: fan-out enqueues deliveries, the delivery worker drains them.
type WebhookStore struct {
	s      *Store
	sealer *platform.WebhookKeySealer
}

// NewWebhookStore returns the Postgres-backed implementation without a sealer: signing keys are stored raw and a sealed key cannot be read back.
func NewWebhookStore(s *Store) *WebhookStore {
	return &WebhookStore{s: s}
}

// NewSealingWebhookStore is [NewWebhookStore] that seals signing keys on write and unseals them in GetWebhook; a nil sealer behaves as NewWebhookStore.
func NewSealingWebhookStore(s *Store, sealer *platform.WebhookKeySealer) *WebhookStore {
	return &WebhookStore{s: s, sealer: sealer}
}

// Compile-time interface conformance.
var _ platform.WebhookStore = (*WebhookStore)(nil)

// customerWebhooksURLKey is migration 0180's UNIQUE (account_id, url) index.
const customerWebhooksURLKey = "customer_webhooks_account_url_key"

// webhookURLConflict maps a violation of [customerWebhooksURLKey] to
// [platform.ErrConflict] and returns any other error unchanged.
func webhookURLConflict(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgErrUniqueViolation &&
		pgErr.ConstraintName == customerWebhooksURLKey {
		return platform.ErrConflict
	}
	return err
}

// CreateWebhook inserts the registry row, enforcing maxPerAccount atomically under a per-account advisory lock; maxPerAccount <= 0 admits nothing, a duplicate URL returns [platform.ErrConflict].
func (c *WebhookStore) CreateWebhook(ctx context.Context, w platform.CustomerWebhook, maxPerAccount int) (platform.CustomerWebhook, error) {
	if w.AccountID == uuid.Nil {
		return platform.CustomerWebhook{}, errors.New("postgresstore: CreateWebhook: AccountID is empty")
	}
	if w.URL == "" {
		return platform.CustomerWebhook{}, errors.New("postgresstore: CreateWebhook: URL is empty")
	}
	if len(w.Events) == 0 {
		return platform.CustomerWebhook{}, errors.New("postgresstore: CreateWebhook: Events is empty")
	}
	if maxPerAccount <= 0 {
		return platform.CustomerWebhook{}, platform.ErrWebhookQuotaExceeded
	}
	tx, err := c.s.db.BeginTx(ctx, nil)
	if err != nil {
		return platform.CustomerWebhook{}, fmt.Errorf("postgresstore: CreateWebhook: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := lockAccount(ctx, tx, lockNamespaceWebhook, w.AccountID); err != nil {
		return platform.CustomerWebhook{}, fmt.Errorf("postgresstore: CreateWebhook: %w", err)
	}

	const q = `
		WITH current_count AS (
		    SELECT COUNT(*) AS n
		      FROM customer_webhooks
		     WHERE account_id = $1
		)
		INSERT INTO customer_webhooks
		    (id, account_id, name, url, secret_hash, signing_key_sealed, events, enabled)
		SELECT $8, $1, $2, $3, $4, $9, $5, $6
		  FROM current_count
		 WHERE current_count.n < $7
		RETURNING id, created_at, updated_at
	`
	// The id is minted here because the sealed key is bound to it.
	id := uuid.New()
	var raw, sealed any = w.SigningKey, nil
	if c.sealer != nil {
		raw, sealed = nil, c.sealer.Seal(id, w.SigningKey)
	}
	events := w.Events
	row := tx.QueryRowContext(ctx, q,
		w.AccountID, w.Name, w.URL, raw,
		events, w.Enabled, maxPerAccount, id, sealed,
	)
	if err := row.Scan(&w.ID, &w.CreatedAt, &w.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return platform.CustomerWebhook{}, platform.ErrWebhookQuotaExceeded
		}
		return platform.CustomerWebhook{}, fmt.Errorf("postgresstore: CreateWebhook: %w", webhookURLConflict(err))
	}
	if err := tx.Commit(); err != nil {
		return platform.CustomerWebhook{}, fmt.Errorf("postgresstore: CreateWebhook: commit: %w", err)
	}
	return w, nil
}

// ListWebhooksForAccount returns every webhook the account registered, ordered by CreatedAt desc.
func (c *WebhookStore) ListWebhooksForAccount(ctx context.Context, accountID uuid.UUID) ([]platform.CustomerWebhook, error) {
	const q = `
		SELECT id, account_id, name, url, events, enabled,
		       created_at, updated_at, previous_secret_expires_at
		  FROM customer_webhooks
		 WHERE account_id = $1
		 ORDER BY created_at DESC
	`
	rows, err := c.s.db.QueryContext(ctx, q, accountID)
	if err != nil {
		return nil, fmt.Errorf("postgresstore: ListWebhooksForAccount: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []platform.CustomerWebhook
	for rows.Next() {
		w, err := scanWebhookRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgresstore: ListWebhooksForAccount rows: %w", err)
	}
	return out, nil
}

// ListWebhooksSubscribedTo returns enabled webhooks subscribed to eventType; fail-closed, a webhook whose account is not `active` or is missing is not a subscriber.
func (c *WebhookStore) ListWebhooksSubscribedTo(ctx context.Context, eventType platform.WebhookEventType) ([]platform.CustomerWebhook, error) {
	const q = `
		SELECT id, account_id, name, url, events, enabled,
		       created_at, updated_at, previous_secret_expires_at
		  FROM customer_webhooks
		 WHERE enabled = TRUE
		   AND $1 = ANY(events)
		   AND EXISTS (SELECT 1
		                 FROM accounts a
		                WHERE a.id = customer_webhooks.account_id
		                  AND a.status = 'active')
	`
	rows, err := c.s.db.QueryContext(ctx, q, string(eventType))
	if err != nil {
		return nil, fmt.Errorf("postgresstore: ListWebhooksSubscribedTo: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []platform.CustomerWebhook
	for rows.Next() {
		w, err := scanWebhookRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgresstore: ListWebhooksSubscribedTo rows: %w", err)
	}
	return out, nil
}

// GetWebhook returns one row by ID with its signing key unsealed, or ErrNotFound; an unopenable sealed key returns the row without SigningKey and an error wrapping [platform.ErrWebhookKeyUnsealable].
func (c *WebhookStore) GetWebhook(ctx context.Context, id uuid.UUID) (platform.CustomerWebhook, error) {
	const q = `
		SELECT id, account_id, name, url, events, enabled,
		       created_at, updated_at, previous_secret_expires_at,
		       secret_hash, signing_key_sealed,
		       previous_secret, previous_signing_key_sealed
		  FROM customer_webhooks
		 WHERE id = $1
	`
	var raw, sealed, prevRaw, prevSealed []byte
	row := c.s.db.QueryRowContext(ctx, q, id)
	w, err := scanWebhookRow(row, &raw, &sealed, &prevRaw, &prevSealed)
	if errors.Is(err, sql.ErrNoRows) {
		return platform.CustomerWebhook{}, platform.ErrNotFound
	}
	if err != nil {
		return platform.CustomerWebhook{}, err
	}
	if w.SigningKey, err = c.openSigningKey(w.ID, raw, sealed); err != nil {
		return w, fmt.Errorf("postgresstore: GetWebhook: %w", err)
	}
	// An expired previous key is never opened, and one that fails to open
	// is dropped: it must not take the current key down with it.
	if time.Now().Before(w.PreviousSecretExpiresAt) {
		if w.PreviousSigningKey, err = c.openSigningKey(w.ID, prevRaw, prevSealed); err != nil {
			slog.Warn("webhook previous signing key unopenable, dropped",
				"webhook_id", w.ID, "error", err)
			w.PreviousSigningKey = nil
		}
	}
	return w, nil
}

func (c *WebhookStore) openSigningKey(id uuid.UUID, raw, sealed []byte) ([]byte, error) {
	if sealed == nil {
		return raw, nil
	}
	if c.sealer == nil {
		return nil, fmt.Errorf("webhook %s: key is sealed: %w", id, platform.ErrWebhookSealKeyMissing)
	}
	return c.sealer.Open(id, sealed)
}

// SealLegacySigningKeys seals every raw signing key (current and previous) and clears the raw copy, returning the count; each is a compare-and-swap on its raw value, safe against concurrent runs, rotation and delete.
func (c *WebhookStore) SealLegacySigningKeys(ctx context.Context) (int, error) {
	if c.sealer == nil {
		return 0, errors.New("postgresstore: SealLegacySigningKeys: no seal key configured")
	}
	todo, err := c.rawSigningKeys(ctx)
	if err != nil {
		return 0, fmt.Errorf("postgresstore: SealLegacySigningKeys: %w", err)
	}
	sealed := 0
	for _, l := range todo {
		if l.raw != nil {
			n, err := c.sealRawKey(ctx, `
				UPDATE customer_webhooks
				   SET signing_key_sealed = $2, secret_hash = NULL
				 WHERE id = $1 AND signing_key_sealed IS NULL AND secret_hash = $3`, l.id, l.raw)
			if err != nil {
				return sealed, err
			}
			sealed += n
		}
		if l.previous != nil {
			n, err := c.sealRawKey(ctx, `
				UPDATE customer_webhooks
				   SET previous_signing_key_sealed = $2, previous_secret = NULL
				 WHERE id = $1 AND previous_signing_key_sealed IS NULL AND previous_secret = $3`, l.id, l.previous)
			if err != nil {
				return sealed, err
			}
			sealed += n
		}
	}
	return sealed, nil
}

// sealRawKey runs one compare-and-swap UPDATE taking ($1 id, $2 sealed,
// $3 raw) and returns the rows it changed.
func (c *WebhookStore) sealRawKey(ctx context.Context, q string, id uuid.UUID, raw []byte) (int, error) {
	res, err := c.s.db.ExecContext(ctx, q, id, c.sealer.Seal(id, raw), raw)
	if err != nil {
		return 0, fmt.Errorf("postgresstore: SealLegacySigningKeys %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("postgresstore: SealLegacySigningKeys %s rows affected: %w", id, err)
	}
	return int(n), nil
}

type rawSigningKey struct {
	id       uuid.UUID
	raw      []byte
	previous []byte
}

// rawSigningKeys reads every webhook holding a current or previous key
// raw. The rows are drained before any UPDATE so the sweep holds one
// connection at a time.
func (c *WebhookStore) rawSigningKeys(ctx context.Context) ([]rawSigningKey, error) {
	rows, err := c.s.db.QueryContext(ctx, `
		SELECT id,
		       CASE WHEN signing_key_sealed IS NULL THEN secret_hash END,
		       CASE WHEN previous_signing_key_sealed IS NULL THEN previous_secret END
		  FROM customer_webhooks
		 WHERE (signing_key_sealed IS NULL AND secret_hash IS NOT NULL)
		    OR (previous_signing_key_sealed IS NULL AND previous_secret IS NOT NULL)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []rawSigningKey
	for rows.Next() {
		var k rawSigningKey
		if err := rows.Scan(&k.id, &k.raw, &k.previous); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// UpdateWebhook persists name, url, events and enabled changes; SigningKey and AccountID are immutable.
func (c *WebhookStore) UpdateWebhook(ctx context.Context, w platform.CustomerWebhook) error {
	if w.ID == uuid.Nil {
		return errors.New("postgresstore: UpdateWebhook: ID is empty")
	}
	if len(w.Events) == 0 {
		return errors.New("postgresstore: UpdateWebhook: Events is empty")
	}
	const q = `
		UPDATE customer_webhooks
		   SET name       = $2,
		       url        = $3,
		       events     = $4,
		       enabled    = $5,
		       updated_at = now()
		 WHERE id = $1
	`
	events := w.Events
	res, err := c.s.db.ExecContext(ctx, q, w.ID, w.Name, w.URL, events, w.Enabled)
	if err != nil {
		return fmt.Errorf("postgresstore: UpdateWebhook %s: %w", w.ID, webhookURLConflict(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgresstore: UpdateWebhook %s rows affected: %w", w.ID, err)
	}
	if n == 0 {
		return platform.ErrNotFound
	}
	return nil
}

// DeleteWebhook removes the row + cascades to webhook_deliveries.
func (c *WebhookStore) DeleteWebhook(ctx context.Context, id uuid.UUID) error {
	const q = `DELETE FROM customer_webhooks WHERE id = $1`
	if _, err := c.s.db.ExecContext(ctx, q, id); err != nil {
		return fmt.Errorf("postgresstore: DeleteWebhook %s: %w", id, err)
	}
	return nil
}

// EnqueueDelivery inserts one pending delivery only if the owning account is ACTIVE (the shared choke point); errors wrap [ErrWebhookAccountInactive], [platform.ErrNotFound] or [platform.ErrDeliveryAlreadyEnqueued].
func (c *WebhookStore) EnqueueDelivery(ctx context.Context, d platform.WebhookDelivery) error {
	if d.WebhookID == uuid.Nil {
		return errors.New("postgresstore: EnqueueDelivery: WebhookID is empty")
	}
	if d.EventType == "" {
		return errors.New("postgresstore: EnqueueDelivery: EventType is empty")
	}
	payload := d.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	const q = `
		INSERT INTO webhook_deliveries
		    (id, webhook_id, event_type, payload, attempt_count, next_attempt_at)
		SELECT $5, cw.id, $2, $3, 0,
		       COALESCE(NULLIF($4, '0001-01-01 00:00:00+00'::timestamptz), now())
		  FROM customer_webhooks cw
		 WHERE cw.id = $1
		   AND EXISTS (SELECT 1
		                 FROM accounts a
		                WHERE a.id = cw.account_id
		                  AND a.status = 'active')
		ON CONFLICT (id) DO NOTHING
	`
	res, err := c.s.db.ExecContext(ctx, q,
		d.WebhookID, string(d.EventType), payload, d.NextAttemptAt, d.ID,
	)
	if err != nil {
		return fmt.Errorf("postgresstore: EnqueueDelivery: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgresstore: EnqueueDelivery rows affected: %w", err)
	}
	if n == 0 {
		if dupErr := c.alreadyEnqueued(ctx, d.ID, d.WebhookID); dupErr != nil {
			return dupErr
		}
		return c.refuseEnqueue(ctx, "EnqueueDelivery", d.WebhookID)
	}
	return nil
}

// alreadyEnqueued explains an EnqueueDelivery that inserted nothing
// because d.ID already exists. It returns nil when no such row exists, so
// the caller falls through to the kill-switch explanation. An existing ID
// on a DIFFERENT webhook is a key collision, never an idempotent replay.
func (c *WebhookStore) alreadyEnqueued(ctx context.Context, id, webhookID uuid.UUID) error {
	var owner uuid.UUID
	err := c.s.db.QueryRowContext(ctx,
		`SELECT webhook_id FROM webhook_deliveries WHERE id = $1`, id).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("postgresstore: EnqueueDelivery: queued nothing and the existing-row check failed: %w", err)
	case owner != webhookID:
		return fmt.Errorf("postgresstore: EnqueueDelivery: delivery id %s already belongs to webhook %s, not %s", id, owner, webhookID)
	}
	return fmt.Errorf("postgresstore: EnqueueDelivery: delivery %s for webhook %s: %w",
		id, webhookID, platform.ErrDeliveryAlreadyEnqueued)
}

// ErrWebhookAccountInactive is wrapped when a delivery is refused because the owning account is suspended or closed; a POLICY refusal, not a lost event, and WebhookSuppressed() reports true so fan-out can count it apart.
var ErrWebhookAccountInactive = errors.New("postgresstore: webhook account is not active")

// accountInactiveError reports an enqueue refused by the account kill
// switch. It carries the observed status so the caller's log line names
// WHY, and implements the WebhookSuppressed() behavioural contract
// customerwebhook.Fanout matches on.
type accountInactiveError struct {
	op        string
	webhookID uuid.UUID
	status    platform.AccountStatus
}

func (e accountInactiveError) Error() string {
	return fmt.Sprintf("postgresstore: %s: webhook %s: account is %s, not active",
		e.op, e.webhookID, e.status)
}

// Unwrap exposes [ErrWebhookAccountInactive] to errors.Is.
func (e accountInactiveError) Unwrap() error { return ErrWebhookAccountInactive }

// WebhookSuppressed marks a delivery withheld by policy rather than lost to a failure.
func (e accountInactiveError) WebhookSuppressed() bool { return true }

// refuseEnqueue turns an enqueue that matched no row into the specific
// reason it matched none: the webhook is gone, the account is not
// active, or the reason lookup itself failed — which is surfaced as an
// error, never flattened into a suppression.
func (c *WebhookStore) refuseEnqueue(ctx context.Context, op string, webhookID uuid.UUID) error {
	status, err := c.WebhookAccountStatus(ctx, webhookID)
	switch {
	case errors.Is(err, platform.ErrNotFound):
		return fmt.Errorf("postgresstore: %s: webhook %s: %w", op, webhookID, platform.ErrNotFound)
	case err != nil:
		return fmt.Errorf("postgresstore: %s: webhook %s: queued nothing and the account status is unreadable: %w",
			op, webhookID, err)
	}
	return accountInactiveError{op: op, webhookID: webhookID, status: status}
}

// WebhookAccountStatus returns the status of the account that OWNS webhookID, or [platform.ErrNotFound]; the delivery worker re-reads it just before signing so a post-claim suspension is still caught.
func (c *WebhookStore) WebhookAccountStatus(ctx context.Context, webhookID uuid.UUID) (platform.AccountStatus, error) {
	const q = `
		SELECT a.status
		  FROM customer_webhooks cw
		  JOIN accounts a ON a.id = cw.account_id
		 WHERE cw.id = $1
	`
	var status string
	if err := c.s.db.QueryRowContext(ctx, q, webhookID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", platform.ErrNotFound
		}
		return "", fmt.Errorf("postgresstore: WebhookAccountStatus %s: %w", webhookID, err)
	}
	return platform.AccountStatus(status), nil
}

// maxClaimPerWebhook caps one endpoint's rows in a single claim. At the
// worker's 10s attempt timeout, 5 rows bound a stalled endpoint's lane to
// under a minute per poll; a healthy endpoint with a backlog drains at 5
// rows per poll instead of a whole batch.
const maxClaimPerWebhook = 5

// ListPendingDeliveries atomically claims up to limit due deliveries (FOR UPDATE SKIP LOCKED, fair-shared per endpoint, 5-minute lease); rows of non-ACTIVE accounts are parked, and the EXISTS must not join `accounts`.
func (c *WebhookStore) ListPendingDeliveries(ctx context.Context, limit int) ([]platform.WebhookDelivery, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	// Window functions cannot share a SELECT with FOR UPDATE, so `due`
	// ranks without locking and `claimed` locks; the due predicates are
	// repeated on wd so a row another worker claimed since `due` read it
	// is re-checked against its new version and dropped.
	const q = `
		WITH due AS (
		    SELECT id, next_attempt_at,
		           row_number() OVER (PARTITION BY webhook_id
		                              ORDER BY next_attempt_at, id) AS lane_rank
		      FROM webhook_deliveries
		     WHERE delivered_at IS NULL
		       AND next_attempt_at IS NOT NULL
		       AND next_attempt_at <= now()
		       AND EXISTS (SELECT 1
		                     FROM customer_webhooks cw
		                     JOIN accounts a ON a.id = cw.account_id
		                    WHERE cw.id = webhook_deliveries.webhook_id
		                      AND a.status = 'active')
		), claimed AS (
		    SELECT wd.id
		      FROM webhook_deliveries wd
		      JOIN due ON due.id = wd.id
		     WHERE due.lane_rank <= $2
		       AND wd.delivered_at IS NULL
		       AND wd.next_attempt_at <= now()
		     ORDER BY due.lane_rank, due.next_attempt_at, wd.id
		     LIMIT $1
		     FOR UPDATE OF wd SKIP LOCKED
		)
		UPDATE webhook_deliveries
		   SET next_attempt_at = now() + interval '5 minutes'
		  FROM claimed
		 WHERE webhook_deliveries.id = claimed.id
		RETURNING webhook_deliveries.id,
		          webhook_deliveries.webhook_id,
		          webhook_deliveries.event_type,
		          webhook_deliveries.payload,
		          webhook_deliveries.attempt_count,
		          COALESCE(webhook_deliveries.next_attempt_at, '0001-01-01 00:00:00+00'::timestamptz),
		          COALESCE(webhook_deliveries.delivered_at,    '0001-01-01 00:00:00+00'::timestamptz),
		          COALESCE(webhook_deliveries.last_error, ''),
		          COALESCE(webhook_deliveries.last_response_status, 0),
		          webhook_deliveries.created_at
	`
	rows, err := c.s.db.QueryContext(ctx, q, limit, maxClaimPerWebhook)
	if err != nil {
		return nil, fmt.Errorf("postgresstore: ListPendingDeliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []platform.WebhookDelivery
	for rows.Next() {
		d, err := scanDeliveryRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgresstore: ListPendingDeliveries rows: %w", err)
	}
	return out, nil
}

// MarkDelivered records a successful POST.
func (c *WebhookStore) MarkDelivered(ctx context.Context, id uuid.UUID, responseStatus int) error {
	const q = `
		UPDATE webhook_deliveries
		   SET delivered_at         = now(),
		       attempt_count        = attempt_count + 1,
		       next_attempt_at      = NULL,
		       last_response_status = $2,
		       last_error           = NULL
		 WHERE id = $1
	`
	if _, err := c.s.db.ExecContext(ctx, q, id, responseStatus); err != nil {
		return fmt.Errorf("postgresstore: MarkDelivered %s: %w", id, err)
	}
	return nil
}

// MarkAttemptFailed records a failed POST + schedules the next try.
func (c *WebhookStore) MarkAttemptFailed(ctx context.Context, id uuid.UUID, errMsg string, responseStatus int, nextAttemptAt time.Time) error {
	// nextAttemptAt zero → permanently failed: clear next_attempt_at
	// so the row drops out of the pending-listing predicate.
	var nextArg any
	if !nextAttemptAt.IsZero() {
		nextArg = nextAttemptAt
	}
	const q = `
		UPDATE webhook_deliveries
		   SET attempt_count        = attempt_count + 1,
		       last_error           = $2,
		       last_response_status = $3,
		       next_attempt_at      = $4
		 WHERE id = $1
	`
	if _, err := c.s.db.ExecContext(ctx, q, id, errMsg, responseStatus, nextArg); err != nil {
		return fmt.Errorf("postgresstore: MarkAttemptFailed %s: %w", id, err)
	}
	return nil
}

// SweepFinishedDeliveries deletes delivery rows created before olderThan that are delivered or permanently failed (next_attempt_at cleared); a row still carrying next_attempt_at (incl. parked behind a suspended account) is never touched.
func (c *WebhookStore) SweepFinishedDeliveries(ctx context.Context, olderThan time.Time) (int64, error) {
	const q = `
		DELETE FROM webhook_deliveries
		 WHERE id IN (
		     SELECT id FROM webhook_deliveries
		      WHERE created_at < $1
		        AND (delivered_at IS NOT NULL OR next_attempt_at IS NULL)
		      LIMIT $2
		 )
	`
	return c.s.deleteInBatches(ctx, "postgresstore: SweepFinishedDeliveries", q, olderThan, defaultSweepBatchRows)
}

// CountDeliveries returns the webhook_deliveries row count, feeding the [obs.RetentionReaperRows] gauge.
func (c *WebhookStore) CountDeliveries(ctx context.Context) (int64, error) {
	var n int64
	if err := c.s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM webhook_deliveries`).Scan(&n); err != nil {
		return 0, fmt.Errorf("postgresstore: CountDeliveries: %w", err)
	}
	return n, nil
}

// ─── helpers ────────────────────────────────────────────────────

// rowScanner is the subset of *sql.Row + *sql.Rows that
// scanWebhookRow + scanDeliveryRow need. Lets one helper handle
// both single-row and rows-iterator paths.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanWebhookRow scans the key-free webhook columns, then any extra
// trailing columns into extra.
func scanWebhookRow(s rowScanner, extra ...any) (platform.CustomerWebhook, error) {
	var (
		w              platform.CustomerWebhook
		events         []string
		previousExpiry sql.NullTime
	)
	dest := append([]any{
		&w.ID, &w.AccountID, &w.Name, &w.URL,
		pgarray.Strings(&events), &w.Enabled, &w.CreatedAt, &w.UpdatedAt,
		&previousExpiry,
	}, extra...)
	if err := s.Scan(dest...); err != nil {
		return platform.CustomerWebhook{}, fmt.Errorf("postgresstore: scan webhook: %w", err)
	}
	w.Events = events
	w.PreviousSecretExpiresAt = previousExpiry.Time
	return w, nil
}

func scanDeliveryRow(s rowScanner) (platform.WebhookDelivery, error) {
	var (
		d         platform.WebhookDelivery
		eventType string
	)
	if err := s.Scan(
		&d.ID, &d.WebhookID, &eventType, &d.Payload,
		&d.AttemptCount,
		&d.NextAttemptAt, &d.DeliveredAt,
		&d.LastError, &d.LastResponseStatus,
		&d.CreatedAt,
	); err != nil {
		return platform.WebhookDelivery{}, fmt.Errorf("postgresstore: scan delivery: %w", err)
	}
	d.EventType = string(eventType)
	// COALESCE pushed sentinel zero-times for nullable columns;
	// translate them back to Go zero-value time.Time so callers
	// can use IsZero() consistently.
	if d.NextAttemptAt.Year() == 1 {
		d.NextAttemptAt = time.Time{}
	}
	if d.DeliveredAt.Year() == 1 {
		d.DeliveredAt = time.Time{}
	}
	return d, nil
}

// ─── Dashboard-flow surfaces (extends EnqueueDelivery/MarkDelivered) ─────

// RotateWebhookSecret makes newSecret the signing key in place under a row lock, moving the old key to the previous slot; keys are stored as create stores them and a sealer-less store refuses a sealed row.
func (c *WebhookStore) RotateWebhookSecret(ctx context.Context, id uuid.UUID, newSecret []byte, previousExpiresAt time.Time) error {
	if len(newSecret) == 0 {
		return errors.New("postgresstore: RotateWebhookSecret: newSecret is empty")
	}
	if previousExpiresAt.IsZero() {
		return errors.New("postgresstore: RotateWebhookSecret: previousExpiresAt is zero")
	}
	tx, err := c.s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgresstore: RotateWebhookSecret %s: begin: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	var raw, sealed []byte
	err = tx.QueryRowContext(ctx, `
		SELECT secret_hash, signing_key_sealed
		  FROM customer_webhooks
		 WHERE id = $1
		   FOR UPDATE`, id).Scan(&raw, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return platform.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("postgresstore: RotateWebhookSecret %s: %w", id, err)
	}

	var curRaw, curSealed, prevRaw, prevSealed any
	switch {
	case c.sealer != nil && sealed != nil:
		curSealed, prevSealed = c.sealer.Seal(id, newSecret), sealed
	case c.sealer != nil:
		curSealed, prevSealed = c.sealer.Seal(id, newSecret), c.sealer.Seal(id, raw)
	case sealed != nil:
		return fmt.Errorf("postgresstore: RotateWebhookSecret %s: key is sealed: %w", id, platform.ErrWebhookSealKeyMissing)
	default:
		curRaw, prevRaw = newSecret, raw
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE customer_webhooks
		   SET secret_hash                 = $2,
		       signing_key_sealed          = $3,
		       previous_secret             = $4,
		       previous_signing_key_sealed = $5,
		       previous_secret_expires_at  = $6,
		       updated_at                  = now()
		 WHERE id = $1`,
		id, curRaw, curSealed, prevRaw, prevSealed, previousExpiresAt); err != nil {
		return fmt.Errorf("postgresstore: RotateWebhookSecret %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgresstore: RotateWebhookSecret %s: commit: %w", id, err)
	}
	return nil
}

// AppendDelivery records one delivery attempt and returns the row; gated on the owning account being ACTIVE exactly like [WebhookStore.EnqueueDelivery], since a kill switch honoured by one of two writers is not one.
func (c *WebhookStore) AppendDelivery(ctx context.Context, d platform.WebhookDelivery) (platform.WebhookDelivery, error) {
	if d.WebhookID == uuid.Nil {
		return platform.WebhookDelivery{}, errors.New("postgresstore: AppendDelivery: WebhookID is empty")
	}
	payload := d.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	var (
		deliveredAt    any
		nextAttemptAt  any
		responseStatus any
		lastError      any
	)
	if !d.DeliveredAt.IsZero() {
		deliveredAt = d.DeliveredAt
	}
	if !d.NextAttemptAt.IsZero() {
		nextAttemptAt = d.NextAttemptAt
	}
	if d.LastResponseStatus != 0 {
		responseStatus = d.LastResponseStatus
	}
	if d.LastError != "" {
		lastError = d.LastError
	}
	const q = `
		INSERT INTO webhook_deliveries
		    (webhook_id, event_type, payload,
		     attempt_count, next_attempt_at, delivered_at,
		     last_error, last_response_status)
		SELECT cw.id, $2, $3, $4, $5, $6, $7, $8
		  FROM customer_webhooks cw
		 WHERE cw.id = $1
		   AND EXISTS (SELECT 1
		                 FROM accounts a
		                WHERE a.id = cw.account_id
		                  AND a.status = 'active')
		RETURNING id, created_at
	`
	row := c.s.db.QueryRowContext(ctx, q,
		d.WebhookID, string(d.EventType), payload,
		d.AttemptCount, nextAttemptAt, deliveredAt,
		lastError, responseStatus,
	)
	if err := row.Scan(&d.ID, &d.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return platform.WebhookDelivery{}, c.refuseEnqueue(ctx, "AppendDelivery", d.WebhookID)
		}
		return platform.WebhookDelivery{}, fmt.Errorf("postgresstore: AppendDelivery: %w", err)
	}
	return d, nil
}

// UpdateDelivery rewrites the attempt-state fields of the delivery keyed by ID; idempotent.
func (c *WebhookStore) UpdateDelivery(ctx context.Context, d platform.WebhookDelivery) error {
	if d.ID == uuid.Nil {
		return errors.New("postgresstore: UpdateDelivery: ID is empty")
	}
	var (
		deliveredAt    any
		nextAttemptAt  any
		responseStatus any
		lastError      any
	)
	if !d.DeliveredAt.IsZero() {
		deliveredAt = d.DeliveredAt
	}
	if !d.NextAttemptAt.IsZero() {
		nextAttemptAt = d.NextAttemptAt
	}
	if d.LastResponseStatus != 0 {
		responseStatus = d.LastResponseStatus
	}
	if d.LastError != "" {
		lastError = d.LastError
	}
	const q = `
		UPDATE webhook_deliveries
		   SET attempt_count        = $2,
		       next_attempt_at      = $3,
		       delivered_at         = $4,
		       last_error           = $5,
		       last_response_status = $6
		 WHERE id = $1
	`
	if _, err := c.s.db.ExecContext(ctx, q,
		d.ID, d.AttemptCount, nextAttemptAt, deliveredAt,
		lastError, responseStatus,
	); err != nil {
		return fmt.Errorf("postgresstore: UpdateDelivery %s: %w", d.ID, err)
	}
	return nil
}

// ListDeliveries returns the most-recent limit attempts for one webhook, newest first.
func (c *WebhookStore) ListDeliveries(ctx context.Context, webhookID uuid.UUID, limit int) ([]platform.WebhookDelivery, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	const q = `
		SELECT id, webhook_id, event_type, payload, attempt_count,
		       COALESCE(next_attempt_at, '0001-01-01 00:00:00+00'::timestamptz),
		       COALESCE(delivered_at,    '0001-01-01 00:00:00+00'::timestamptz),
		       COALESCE(last_error, ''),
		       COALESCE(last_response_status, 0),
		       created_at
		  FROM webhook_deliveries
		 WHERE webhook_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2
	`
	rows, err := c.s.db.QueryContext(ctx, q, webhookID, limit)
	if err != nil {
		return nil, fmt.Errorf("postgresstore: ListDeliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []platform.WebhookDelivery
	for rows.Next() {
		d, err := scanDeliveryRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgresstore: ListDeliveries rows: %w", err)
	}
	return out, nil
}
