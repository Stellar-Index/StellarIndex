package main

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// Q146: a wired dashboard must start a retention reaper for sessions and
// for webhook_deliveries — the two platform tables that otherwise grow
// with every sign-in and every delivered event.
func TestRetentionReaperTargets_WiredDashboardBoundsSessionsAndDeliveries(t *testing.T) {
	pg := postgresstore.New(nil)
	b := dashboardBundle{
		users:        postgresstore.NewUserStore(pg),
		webhookStore: postgresstore.NewWebhookStore(pg),
	}
	got := map[string]time.Duration{}
	for _, o := range retentionReaperTargets(b, nil, "redis", slog.New(slog.NewTextHandler(io.Discard, nil))) {
		if o.Sweep == nil {
			t.Errorf("%s: nil Sweep", o.Name)
		}
		got[o.Name] = o.Retention
		if o.Name == obs.AuthReaperWebhookDelivery && o.Count == nil {
			t.Errorf("%s: nil Count — the row gauge would never publish", o.Name)
		}
	}
	want := map[string]time.Duration{
		obs.AuthReaperSession:         90 * 24 * time.Hour,
		obs.AuthReaperWebhookDelivery: 30 * 24 * time.Hour,
	}
	if len(got) != len(want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
	for name, ret := range want {
		if got[name] != ret {
			t.Errorf("%s retention = %v, want %v", name, got[name], ret)
		}
	}
}

// Unused /v1/register accounts get a reaper at the validator record's
// idle TTL only with Postgres accounts, Redis and the redis auth backend.
func TestRetentionReaperTargets_RegistrationReaperNeedsAccountsAndRedis(t *testing.T) {
	pg := postgresstore.New(nil)
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = rdb.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := dashboardBundle{accounts: postgresstore.NewAccountStore(pg)}

	var found bool
	for _, o := range retentionReaperTargets(b, rdb, "redis", logger) {
		if o.Name == obs.AuthReaperRegistration {
			found = true
			if o.Retention != auth.MirroredKeyIdleTTL || o.Sweep == nil {
				t.Errorf("registration reaper: retention %v, sweep nil %v; want %v, false",
					o.Retention, o.Sweep == nil, auth.MirroredKeyIdleTTL)
			}
		}
	}
	if !found {
		t.Fatal("no registration reaper with accounts + Redis wired")
	}
	for _, o := range retentionReaperTargets(b, nil, "redis", logger) {
		if o.Name == obs.AuthReaperRegistration {
			t.Fatal("registration reaper started without Redis; it could not prove a key dead")
		}
	}
	// Under the postgres backend the validator falls back to the
	// never-expiring api_keys row, so an expired Redis record does not
	// mean a dead key: a key in daily use would be erased.
	for _, o := range retentionReaperTargets(b, rdb, "postgres", logger) {
		if o.Name == obs.AuthReaperRegistration {
			t.Fatal("registration reaper started under auth_backend=postgres")
		}
	}
}

func TestRetentionReaperTargets_UnwiredDashboardStartsNone(t *testing.T) {
	if got := retentionReaperTargets(dashboardBundle{}, nil, "redis", slog.New(slog.NewTextHandler(io.Discard, nil))); len(got) != 0 {
		t.Fatalf("targets = %d, want 0 without a Postgres-backed dashboard", len(got))
	}
}
