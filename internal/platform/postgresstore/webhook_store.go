package postgresstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/pgarray"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// WebhookStore implements [platform.WebhookStore] against the
// `customer_webhooks` + `webhook_deliveries` tables from migration
// 0027.
//
// F-1270 (audit-2026-05-12): the data plane for customer-facing
// incident callbacks. The delivery worker that drains the queue
// is a follow-up; this commit lands the store half so the wire is
// end-to-end-ready.
type WebhookStore struct{ s *Store }

// NewWebhookStore returns the Postgres-backed implementation.
func NewWebhookStore(s *Store) *WebhookStore {
	return &WebhookStore{s: s}
}

// Compile-time interface conformance.
var _ platform.WebhookStore = (*WebhookStore)(nil)

// CreateWebhook inserts the registry row, enforcing the per-account
// `maxPerAccount` cap atomically. F-1248 (codex audit-2026-05-12):
// the handler's pre-check (`SELECT … then INSERT`) was raceable —
// N parallel HandleCreate requests for an account at 9 webhooks
// could all pass the precheck and each insert one row, taking the
// account to 9+N.
//
// Closure shape: the create runs inside a transaction guarded by
// `pg_advisory_xact_lock(hashtext('webhook:'||account_id))`. The
// advisory lock serialises every concurrent caller for the same
// account through one critical section, so the count + insert
// observes a stable view: at most ONE statement appends a row at
// a time, and the loser's count-CTE sees the winner's INSERT and
// short-circuits via WHERE n < $7. The lock auto-releases at
// COMMIT/ROLLBACK so a crashing client can't strand it. Keyed
// by account_id, so two different accounts creating concurrently
// don't serialise against each other.
//
// `maxPerAccount` is the value passed by the handler
// (MaxWebhooksPerAccount = 10 at time of writing). Tests can pass
// a smaller value to drive the race deterministically.
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
		maxPerAccount = 10
	}
	tx, err := c.s.db.BeginTx(ctx, nil)
	if err != nil {
		return platform.CustomerWebhook{}, fmt.Errorf("postgresstore: CreateWebhook: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// F-1248 (codex audit-2026-05-12): per-account advisory lock
	// inside the transaction. `hashtext` deterministically maps
	// 'webhook:'||account_id::text → int4 which pg_advisory_xact_lock
	// accepts as int8 via Postgres's implicit widening.
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('webhook:' || $1::text))`,
		w.AccountID); err != nil {
		return platform.CustomerWebhook{}, fmt.Errorf("postgresstore: CreateWebhook: advisory lock: %w", err)
	}

	const q = `
		WITH current_count AS (
		    SELECT COUNT(*) AS n
		      FROM customer_webhooks
		     WHERE account_id = $1
		)
		INSERT INTO customer_webhooks
		    (account_id, name, url, secret_hash, events, enabled)
		SELECT $1, $2, $3, $4, $5, $6
		  FROM current_count
		 WHERE current_count.n < $7
		RETURNING id, created_at, updated_at
	`
	events := w.Events
	row := tx.QueryRowContext(ctx, q,
		w.AccountID, w.Name, w.URL, w.SecretHash,
		events, w.Enabled, maxPerAccount,
	)
	if err := row.Scan(&w.ID, &w.CreatedAt, &w.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return platform.CustomerWebhook{}, platform.ErrWebhookQuotaExceeded
		}
		return platform.CustomerWebhook{}, fmt.Errorf("postgresstore: CreateWebhook: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return platform.CustomerWebhook{}, fmt.Errorf("postgresstore: CreateWebhook: commit: %w", err)
	}
	return w, nil
}

// ListWebhooksForAccount returns every webhook the account has
// registered, ordered by CreatedAt desc.
func (c *WebhookStore) ListWebhooksForAccount(ctx context.Context, accountID uuid.UUID) ([]platform.CustomerWebhook, error) {
	const q = `
		SELECT id, account_id, name, url, secret_hash, events, enabled,
		       created_at, updated_at
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

// ListWebhooksSubscribedTo returns every enabled webhook subscribed to
// `eventType` whose owning account is ACTIVE. The fan-out service
// iterates the result and calls EnqueueDelivery for each. The events
// column is a text[] in Postgres; ANY($1) is the membership predicate.
// F-1249 (codex audit-2026-05-12).
//
// SEC-06 / RLT-420: the account kill switch used to be inbound-only.
// Suspending or closing an account stopped its API keys authenticating
// (internal/auth/apikey_redis.go) but nothing on the OUTBOUND side read
// account status, so this resolver kept handing the fan-out a suspended
// customer's endpoints and we kept POSTing their data to them. The
// EXISTS is the resolver-side gate: a webhook whose account is anything
// other than `active` — and a webhook whose account row is missing
// altogether — is not a subscriber. Fail-closed, matching the inbound
// side's "active authenticates, everything else does not".
func (c *WebhookStore) ListWebhooksSubscribedTo(ctx context.Context, eventType platform.WebhookEventType) ([]platform.CustomerWebhook, error) {
	const q = `
		SELECT id, account_id, name, url, secret_hash, events, enabled,
		       created_at, updated_at
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

// GetWebhook returns one row by ID. ErrNotFound when absent.
func (c *WebhookStore) GetWebhook(ctx context.Context, id uuid.UUID) (platform.CustomerWebhook, error) {
	const q = `
		SELECT id, account_id, name, url, secret_hash, events, enabled,
		       created_at, updated_at
		  FROM customer_webhooks
		 WHERE id = $1
	`
	row := c.s.db.QueryRowContext(ctx, q, id)
	w, err := scanWebhookRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return platform.CustomerWebhook{}, platform.ErrNotFound
	}
	return w, err
}

// UpdateWebhook persists name / url / events / enabled changes.
// SecretHash + AccountID are immutable post-create.
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
		return fmt.Errorf("postgresstore: UpdateWebhook %s: %w", w.ID, err)
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

// EnqueueDelivery inserts one pending delivery row, conditional on the
// owning account being ACTIVE.
//
// SEC-06 / RLT-420: this is the choke point EVERY producer shares.
// ListWebhooksSubscribedTo already withholds a suspended account's
// endpoints from the fan-out, but internal/pricealerts/worker.go
// resolves its targets with ListWebhooksForAccount — the customer's own
// dashboard listing, deliberately unfiltered — so without the gate here
// a suspended account still accrued queued deliveries. Returns an error
// wrapping [ErrWebhookAccountInactive] when the account is not active,
// and [platform.ErrNotFound] when the webhook itself is gone (the
// pre-gate behaviour a foreign-key violation produced).
//
// d.ID is inserted as the primary key, so a producer that derives it from
// the event (customerwebhook.Fanout.PublishOnce) can re-run a partial
// fan-out without re-notifying subscribers already queued: the conflict
// inserts nothing and returns [platform.ErrDeliveryAlreadyEnqueued].
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

// ErrWebhookAccountInactive is the sentinel the enqueue paths wrap when
// they refuse to queue a delivery because the webhook's owning account
// is suspended or closed. It is a POLICY refusal, not a lost event: the
// returned error also answers WebhookSuppressed() true, so a fan-out can
// count it apart from a genuine enqueue failure — and stay off the
// lost-event alert — without importing this package.
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

// WebhookSuppressed marks this as a delivery withheld by policy rather
// than one lost to a failure (the net.Error.Timeout contract shape).
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

// WebhookAccountStatus returns the lifecycle status of the account that
// OWNS webhookID, or [platform.ErrNotFound] when the webhook (or the
// account it hangs off) is absent. It is the delivery worker's leg of
// the account kill switch: the worker re-reads it immediately before
// signing and POSTing, so an account suspended AFTER its deliveries were
// claimed is still caught (SEC-06 / RLT-420).
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

// ListPendingDeliveries atomically claims up to `limit` due
// deliveries, fair-shared across endpoints (see below). F-1247 (codex audit-2026-05-12): claim happens
// in the same statement as the read via UPDATE…RETURNING +
// `FOR UPDATE SKIP LOCKED`, so two workers running concurrently
// (horizontal scale or blue/green overlap during deploy) never
// hand the same row to two HTTP-POST paths.
//
// The lease is implemented by pushing `next_attempt_at` 5 minutes
// into the future as part of the claim. Any worker that subsequently
// runs the same query won't see the row (its next_attempt_at is now
// `now() + 5m`). On successful delivery [MarkDelivered] sets
// `delivered_at`; on failure [MarkAttemptFailed] writes the
// genuine backoff back into next_attempt_at. If a worker crashes
// after claiming but before either update, the lease expires after
// 5 minutes and another worker can pick the row up — that's
// idempotent because the receiver-side dedupe (event_id header)
// catches it; and customer-side metrics treat
// duplicate-post-after-worker-crash as the same class as 5xx-retry.
//
// Fair share (GH-663): the claim ranks each endpoint's due rows FIFO and
// takes every endpoint's first row before any endpoint's second, and at
// most maxClaimPerWebhook rows per endpoint per claim. One endpoint's
// backlog — say a black-holing host with hundreds of queued events —
// therefore cannot fill a batch and push every other customer's events
// behind it; the worker delivers each endpoint's rows serially, so this
// cap also bounds how long that endpoint's lane holds a poll. Within an
// endpoint, order stays FIFO by next_attempt_at.
//
// SEC-06 / RLT-420: the claim also skips any delivery whose owning
// account is not ACTIVE. Rows queued before a suspension are therefore
// PARKED, not destroyed — suspension is reversible (AccountStore has
// Unsuspend), so the conservation-correct behaviour is to withhold the
// POST and let the backlog resume if the account is reinstated. The
// EXISTS deliberately does not join `accounts` into the FROM list: a
// join would put the `FOR UPDATE … SKIP LOCKED` row lock on the accounts
// and customer_webhooks rows too, so an admin PATCH holding an account
// row would make the worker silently skip that customer's queue.
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

// SweepFinishedDeliveries deletes delivery rows created before
// olderThan that are finished — delivered, or failed permanently
// (next_attempt_at cleared) — returning how many were removed. Drives
// the webhook-delivery reaper (internal/retentionreaper).
//
// A row still carrying next_attempt_at is never touched at any age:
// that includes deliveries parked behind a suspended account, which
// resume if the account is reinstated.
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

// ─── helpers ────────────────────────────────────────────────────

// rowScanner is the subset of *sql.Row + *sql.Rows that
// scanWebhookRow + scanDeliveryRow need. Lets one helper handle
// both single-row and rows-iterator paths.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanWebhookRow(s rowScanner) (platform.CustomerWebhook, error) {
	var (
		w      platform.CustomerWebhook
		events []string
	)
	if err := s.Scan(
		&w.ID, &w.AccountID, &w.Name, &w.URL, &w.SecretHash,
		pgarray.Strings(&events), &w.Enabled, &w.CreatedAt, &w.UpdatedAt,
	); err != nil {
		return platform.CustomerWebhook{}, fmt.Errorf("postgresstore: scan webhook: %w", err)
	}
	w.Events = events
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

// RotateWebhookSecret replaces the signing secret. Returns the new
// plaintext.
//
// At-rest model: the bytes ARE persisted as the canonical
// `customer_webhooks.secret_hash` bytea — the delivery worker
// reads them back to sign future requests, identical to the
// create path. The field name is a historical misnomer
// (originally promised hash-only persistence, the implementation
// always stored the live HMAC key); see
// [platform.CustomerWebhook] for the full at-rest discussion
// including the F-1244 (codex audit-2026-05-13) reconciliation.
//
// Customer-facing visibility: the plaintext is returned by this
// call exactly once and never served back through any
// subsequent read. That one-time-visibility property is what
// "shown once" referred to in the prior docstring; it is NOT
// the same as "never persisted to disk", and the prior
// conflation of the two was the gap F-1244 closed.
//
// Stub: today returns "" + a not-implemented error. Customers
// rotate by deleting + recreating the webhook (which already
// works via the create path), so the in-place rotation surface
// isn't on the critical path. The interface seam stays here so
// the v2 dashboard can plug in-place rotation without
// re-shaping the store boundary.
func (c *WebhookStore) RotateWebhookSecret(ctx context.Context, id uuid.UUID) (string, error) {
	_ = ctx
	_ = id
	return "", errors.New("postgresstore: RotateWebhookSecret not yet implemented (dashboard rotates by delete + recreate today; the v2 in-place path lands when the dashboard CRUD UI ships it)")
}

// AppendDelivery records one delivery attempt. Returns the
// inserted row with `ID` + `CreatedAt` populated. Used by the
// dashboard-flow path where the caller has the full attempt state
// already (vs EnqueueDelivery which seeds a fresh queue row).
//
// SEC-06 / RLT-420: gated on the owning account being ACTIVE, exactly as
// [WebhookStore.EnqueueDelivery] is — it is the second way a row reaches
// webhook_deliveries, and a kill switch honoured by only one of two
// writers is not a kill switch.
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

// UpdateDelivery rewrites the attempt-state fields. Idempotent; the
// row is keyed by ID and only the mutable fields are touched.
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

// ListDeliveries returns the most-recent `limit` attempts for one
// webhook, newest first. Used by the dashboard delivery log.
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
