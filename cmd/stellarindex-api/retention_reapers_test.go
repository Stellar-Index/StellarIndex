package main

import (
	"io"
	"log/slog"
	"testing"
	"time"

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
	for _, o := range retentionReaperTargets(b, slog.New(slog.NewTextHandler(io.Discard, nil))) {
		if o.Sweep == nil {
			t.Errorf("%s: nil Sweep", o.Name)
		}
		got[o.Name] = o.Retention
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

func TestRetentionReaperTargets_UnwiredDashboardStartsNone(t *testing.T) {
	if got := retentionReaperTargets(dashboardBundle{}, slog.New(slog.NewTextHandler(io.Discard, nil))); len(got) != 0 {
		t.Fatalf("targets = %d, want 0 without a Postgres-backed dashboard", len(got))
	}
}
