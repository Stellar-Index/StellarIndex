//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/customerwebhook"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// SEC-06 / RLT-420 — the account kill switch was INBOUND-ONLY.
//
// Suspending or closing an account stopped its API keys authenticating
// (internal/auth), but every webhook query was blind to account status:
// the resolver (ListWebhooksSubscribedTo), both enqueue writers
// (EnqueueDelivery, AppendDelivery) and the claim query
// (ListPendingDeliveries). A suspended or closed customer therefore kept
// accruing queued rows AND kept receiving our data at the endpoints they
// had registered.
//
// This test executes the whole path against real Postgres for an ACTIVE,
// a SUSPENDED and a CLOSED account, and asserts what each one ENQUEUES
// as well as what each one DELIVERS. The Go-level worker gate is pinned
// separately in internal/customerwebhook/account_killswitch_test.go.

const killSwitchEvent = platform.WebhookEventIncidentSEV1

// killSwitchAccount is one account under test: its lifecycle status, its
// registered webhook, and — where the test needs it — the endpoint that
// counts what that webhook actually received.
type killSwitchAccount struct {
	name      string
	status    platform.AccountStatus
	accountID uuid.UUID
	webhookID uuid.UUID
	posts     *int64
}

func TestCustomerWebhookAccountKillSwitch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := postgresstore.New(db)
	accounts := postgresstore.NewAccountStore(store)
	webhooks := postgresstore.NewWebhookStore(store)

	cases := seedKillSwitchTrio(t, ctx, accounts, webhooks, "queue", false)

	t.Run("resolver hands the fan-out only active accounts", func(t *testing.T) {
		assertResolverSkipsInactive(t, ctx, webhooks, cases)
	})

	t.Run("enqueue writers refuse a non-active account", func(t *testing.T) {
		assertEnqueueRefusedForInactive(t, ctx, webhooks, cases)
		assertQueuedRows(t, ctx, db, cases)
	})

	t.Run("WebhookAccountStatus reports the owning account", func(t *testing.T) {
		for _, c := range cases {
			got, statusErr := webhooks.WebhookAccountStatus(ctx, c.webhookID)
			if statusErr != nil {
				t.Fatalf("%s account: WebhookAccountStatus: %v", c.name, statusErr)
			}
			if got != c.status {
				t.Errorf("%s account: status = %q, want %q", c.name, got, c.status)
			}
		}
		if _, missing := webhooks.WebhookAccountStatus(ctx, uuid.New()); !errors.Is(missing, platform.ErrNotFound) {
			t.Errorf("unknown webhook: err = %v, want ErrNotFound", missing)
		}
	})

	t.Run("claim query parks rows queued before a suspension", func(t *testing.T) {
		assertSuspensionParksBacklog(t, ctx, db, accounts, webhooks)
	})

	t.Run("worker POSTs to the active account only", func(t *testing.T) {
		assertWorkerDeliversToActiveOnly(t, ctx, accounts, webhooks)
	})

	t.Run("fan-out reports a mid-publish suspension as suppressed", func(t *testing.T) {
		assertMidPublishSuspensionIsSuppressed(t, ctx, accounts, webhooks)
	})
}

// ─── assertions ─────────────────────────────────────────────────

func assertResolverSkipsInactive(
	t *testing.T, ctx context.Context,
	webhooks *postgresstore.WebhookStore, cases []*killSwitchAccount,
) {
	t.Helper()
	subs, err := webhooks.ListWebhooksSubscribedTo(ctx, killSwitchEvent)
	if err != nil {
		t.Fatalf("ListWebhooksSubscribedTo: %v", err)
	}
	got := map[uuid.UUID]bool{}
	for _, s := range subs {
		got[s.ID] = true
	}
	for _, c := range cases {
		want := c.status == platform.AccountActive
		if got[c.webhookID] != want {
			t.Errorf("%s account: subscriber present = %v, want %v — "+
				"a non-active account must not be fanned out to",
				c.name, got[c.webhookID], want)
		}
	}
}

func assertEnqueueRefusedForInactive(
	t *testing.T, ctx context.Context,
	webhooks *postgresstore.WebhookStore, cases []*killSwitchAccount,
) {
	t.Helper()
	for _, c := range cases {
		enqErr := webhooks.EnqueueDelivery(ctx, dueDelivery(c.webhookID, "enqueue-probe"))
		_, appErr := webhooks.AppendDelivery(ctx, platform.WebhookDelivery{
			WebhookID: c.webhookID,
			EventType: string(killSwitchEvent),
			Payload:   []byte(`{"incident_id":"append-probe"}`),
		})
		if c.status == platform.AccountActive {
			if enqErr != nil {
				t.Errorf("active account: EnqueueDelivery: %v", enqErr)
			}
			if appErr != nil {
				t.Errorf("active account: AppendDelivery: %v", appErr)
			}
			continue
		}
		if !errors.Is(enqErr, postgresstore.ErrWebhookAccountInactive) {
			t.Errorf("%s account: EnqueueDelivery err = %v, want ErrWebhookAccountInactive — "+
				"rows must not be queued against it", c.name, enqErr)
		}
		if !errors.Is(appErr, postgresstore.ErrWebhookAccountInactive) {
			t.Errorf("%s account: AppendDelivery err = %v, want ErrWebhookAccountInactive",
				c.name, appErr)
		}
	}
}

// assertQueuedRows pins what each account has in webhook_deliveries: the
// active account accrues rows, the non-active ones accrue nothing beyond
// the one seeded before their status changed.
func assertQueuedRows(t *testing.T, ctx context.Context, db *sql.DB, cases []*killSwitchAccount) {
	t.Helper()
	for _, c := range cases {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM webhook_deliveries WHERE webhook_id = $1`,
			c.webhookID).Scan(&n); err != nil {
			t.Fatalf("%s: count deliveries: %v", c.name, err)
		}
		// 1 seeded while still active, plus the EnqueueDelivery and the
		// AppendDelivery probes — which only land for an active account.
		want := 1
		if c.status == platform.AccountActive {
			want = 3
		}
		if n != want {
			t.Errorf("%s account has %d queued delivery row(s), want %d", c.name, n, want)
		}
	}
}

// assertSuspensionParksBacklog: a row queued while the account was
// ACTIVE, with the account suspended afterwards, must be withheld from
// the claim — and must SURVIVE, because suspension is reversible.
func assertSuspensionParksBacklog(
	t *testing.T, ctx context.Context, db *sql.DB,
	accounts *postgresstore.AccountStore, webhooks *postgresstore.WebhookStore,
) {
	t.Helper()
	acct, hook := freshKillSwitchWebhook(t, ctx, accounts, webhooks, "https://parked.example/hook")
	if err := webhooks.EnqueueDelivery(ctx, dueDelivery(hook, "parked")); err != nil {
		t.Fatalf("park: EnqueueDelivery: %v", err)
	}
	suspendAccount(t, ctx, accounts, acct)

	claimed, err := webhooks.ListPendingDeliveries(ctx, 100)
	if err != nil {
		t.Fatalf("ListPendingDeliveries: %v", err)
	}
	for _, d := range claimed {
		if d.WebhookID == hook {
			t.Fatal("claim handed the worker a delivery for a suspended account")
		}
	}
	var still int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE webhook_id = $1 AND delivered_at IS NULL`,
		hook).Scan(&still); err != nil {
		t.Fatalf("count parked: %v", err)
	}
	if still != 1 {
		t.Errorf("parked deliveries = %d, want 1 — suspension is reversible, "+
			"the backlog must be withheld, not destroyed", still)
	}
}

// assertWorkerDeliversToActiveOnly is the end of the line: drain a fresh
// queue with the real worker against the real store and count the HTTP
// POSTs each customer endpoint actually received.
func assertWorkerDeliversToActiveOnly(
	t *testing.T, ctx context.Context,
	accounts *postgresstore.AccountStore, webhooks *postgresstore.WebhookStore,
) {
	t.Helper()
	cases := seedKillSwitchTrio(t, ctx, accounts, webhooks, "deliver", true)

	// httptest listens on 127.0.0.1 with a self-signed cert, both of
	// which the production client rejects by design (SSRF guard,
	// certificate verification).
	w := customerwebhook.NewUnguardedForIntegrationTest(webhooks, customerwebhook.Options{
		PollInterval: 50 * time.Millisecond,
		HTTPClient: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	runCtx, stop := context.WithTimeout(ctx, 800*time.Millisecond)
	defer stop()
	_ = w.Run(runCtx)

	for _, c := range cases {
		got := atomic.LoadInt64(c.posts)
		want := int64(0)
		if c.status == platform.AccountActive {
			want = 1
		}
		if got != want {
			t.Errorf("%s account's endpoint received %d POST(s), want %d — "+
				"a non-active account must never be sent our data", c.name, got, want)
		}
	}
}

// assertMidPublishSuspensionIsSuppressed drives the narrow window the
// enqueue-side gate exists for: the resolver saw an active account and
// the insert happened after the suspension landed. That is a deliberate
// withholding, not a lost customer event.
func assertMidPublishSuspensionIsSuppressed(
	t *testing.T, ctx context.Context,
	accounts *postgresstore.AccountStore, webhooks *postgresstore.WebhookStore,
) {
	t.Helper()
	acct, hook := freshKillSwitchWebhook(t, ctx, accounts, webhooks, "https://suppressed.example/hook")
	f := customerwebhook.NewFanout(&suspendBetweenResolveAndInsert{
		WebhookStore: webhooks,
		suspend:      func() { suspendAccount(t, ctx, accounts, acct) },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	res, err := f.Publish(ctx, killSwitchEvent, []byte(`{"incident_id":"race"}`))
	if err != nil {
		t.Fatalf("Publish reported a lost event for a deliberate suppression: %v", err)
	}
	if res.Suppressed != 1 {
		t.Errorf("Suppressed = %d, want 1 (webhook %s)", res.Suppressed, hook)
	}
	if res.Failed != 0 {
		t.Errorf("Failed = %d, want 0 — a suppression must not reach the lost-event alert",
			res.Failed)
	}
	if res.Enqueued+res.Failed+res.Suppressed != res.Subscribers {
		t.Errorf("counts do not partition the subscriber set: %+v", res)
	}
}

// ─── fixtures ───────────────────────────────────────────────────

// suspendBetweenResolveAndInsert resolves subscribers through the real
// store, then suspends the account before the fan-out gets to insert —
// reproducing the race the enqueue-side gate exists to close.
type suspendBetweenResolveAndInsert struct {
	*postgresstore.WebhookStore
	suspend func()
	once    atomic.Bool
}

func (s *suspendBetweenResolveAndInsert) ListWebhooksSubscribedTo(
	ctx context.Context, eventType platform.WebhookEventType,
) ([]platform.CustomerWebhook, error) {
	subs, err := s.WebhookStore.ListWebhooksSubscribedTo(ctx, eventType)
	if err == nil && s.once.CompareAndSwap(false, true) {
		s.suspend()
	}
	return subs, err
}

// seedKillSwitchTrio creates one active, one suspended and one closed
// account, each with an enabled webhook and one DUE delivery queued
// while the account was still active — so the only difference between
// the three is the account's status. With `withEndpoints` each webhook
// points at a live endpoint that counts the POSTs it receives.
func seedKillSwitchTrio(
	t *testing.T, ctx context.Context,
	accounts *postgresstore.AccountStore, webhooks *postgresstore.WebhookStore,
	tag string, withEndpoints bool,
) []*killSwitchAccount {
	t.Helper()
	cases := []*killSwitchAccount{
		{name: "active", status: platform.AccountActive},
		{name: "suspended", status: platform.AccountSuspended},
		{name: "closed", status: platform.AccountClosed},
	}
	for _, c := range cases {
		url := fmt.Sprintf("https://%s-%s.example/hook", tag, c.name)
		if withEndpoints {
			var posts int64
			c.posts = &posts
			// TLS, not plain http: the customer_webhooks.url CHECK
			// constraint (migration 0027) only accepts `^https://`.
			ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt64(&posts, 1)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(ts.Close)
			url = ts.URL
		}
		c.accountID, c.webhookID = freshKillSwitchWebhook(t, ctx, accounts, webhooks, url)
		if err := webhooks.EnqueueDelivery(ctx, dueDelivery(c.webhookID, tag)); err != nil {
			t.Fatalf("%s/%s: seed EnqueueDelivery: %v", tag, c.name, err)
		}
		switch c.status {
		case platform.AccountActive:
			// Already created active; nothing to change.
		case platform.AccountSuspended:
			suspendAccount(t, ctx, accounts, c.accountID)
		case platform.AccountClosed:
			closeAccount(t, ctx, accounts, c.accountID)
		}
	}
	return cases
}

// freshKillSwitchWebhook creates an ACTIVE account with one enabled
// webhook subscribed to killSwitchEvent, and returns both ids.
func freshKillSwitchWebhook(
	t *testing.T, ctx context.Context,
	accounts *postgresstore.AccountStore, webhooks *postgresstore.WebhookStore,
	url string,
) (uuid.UUID, uuid.UUID) {
	t.Helper()
	suffix := strings.ToLower(uuid.New().String()[:8])
	acct, err := accounts.Create(ctx, platform.Account{
		Name:         "Kill Switch " + suffix,
		Slug:         "ks-" + suffix,
		BillingEmail: fmt.Sprintf("ks-%s@k.example", suffix),
		Tier:         platform.TierFree,
		Status:       platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	secret := sha256.Sum256([]byte("kill-switch-signing-material-" + suffix))
	hook, err := webhooks.CreateWebhook(ctx, platform.CustomerWebhook{
		AccountID:  acct.ID,
		Name:       "hook-" + suffix,
		URL:        url,
		SecretHash: secret[:],
		Events:     []string{string(killSwitchEvent)},
		Enabled:    true,
	}, 10)
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	return acct.ID, hook.ID
}

func dueDelivery(webhookID uuid.UUID, tag string) platform.WebhookDelivery {
	return platform.WebhookDelivery{
		WebhookID:     webhookID,
		EventType:     string(killSwitchEvent),
		Payload:       []byte(fmt.Sprintf(`{"incident_id":%q}`, tag)),
		NextAttemptAt: time.Now().UTC().Add(-time.Second),
	}
}

func suspendAccount(t *testing.T, ctx context.Context, accounts *postgresstore.AccountStore, id uuid.UUID) {
	t.Helper()
	if err := accounts.Suspend(ctx, id, "kill-switch test"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
}

func closeAccount(t *testing.T, ctx context.Context, accounts *postgresstore.AccountStore, id uuid.UUID) {
	t.Helper()
	acct, err := accounts.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get before close: %v", err)
	}
	acct.Status = platform.AccountClosed
	if err := accounts.Update(ctx, acct); err != nil {
		t.Fatalf("close account: %v", err)
	}
}
