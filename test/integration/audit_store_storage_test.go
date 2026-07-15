//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// TestAuditStore exercises the postgresstore.AuditStore against the
// audit_log table from migration 0027. The audit trail is the
// append-only record of every privileged action (key.mint,
// plan.upgrade, session.revoke, Stripe tier changes) and was
// completely untested at every layer (audit-2026-06-14 A20 /
// maintainability-audit-2026-07-01 D10). One container per test,
// matching the storage-test convention.
func TestAuditStore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := postgresstore.New(db)
	accounts := postgresstore.NewAccountStore(store)
	audit := postgresstore.NewAuditStore(store)

	// A real account so the account_id FK link (and the AccountID
	// filter) can be exercised, not just the NULL/system path.
	acct, err := accounts.Create(ctx, platform.Account{
		Name: "Audit Co", Slug: "audit-co",
		BillingEmail: "billing@audit.example",
		Tier:         platform.TierPro, Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	t.Run("Append_ColumnFidelity", func(t *testing.T) {
		ts := time.Date(2026, 7, 5, 9, 0, 0, 0, time.UTC)
		in := platform.AuditEntry{
			AccountID:  acct.ID,
			ActorKind:  platform.ActorWebhook,
			Action:     "plan.upgrade",
			TargetKind: "subscription",
			TargetID:   "sub_123",
			Metadata:   json.RawMessage(`{"from":"starter","to":"pro"}`),
			IP:         net.ParseIP("203.0.113.7"),
			UserAgent:  "Stripe/1.0",
			Timestamp:  ts,
		}
		if err := audit.Append(ctx, in); err != nil {
			t.Fatalf("append: %v", err)
		}

		got := listOne(t, ctx, audit, platform.AuditQuery{Action: "plan.upgrade"})
		if got.AccountID != acct.ID {
			t.Errorf("AccountID = %v, want %v", got.AccountID, acct.ID)
		}
		if got.ActorKind != platform.ActorWebhook {
			t.Errorf("ActorKind = %q, want webhook", got.ActorKind)
		}
		if got.TargetKind != "subscription" || got.TargetID != "sub_123" {
			t.Errorf("target = (%q,%q)", got.TargetKind, got.TargetID)
		}
		if got.UserAgent != "Stripe/1.0" {
			t.Errorf("UserAgent = %q", got.UserAgent)
		}
		if !got.IP.Equal(net.ParseIP("203.0.113.7")) {
			t.Errorf("IP = %v, want 203.0.113.7 (inet round-trip)", got.IP)
		}
		if !got.Timestamp.Equal(ts) {
			t.Errorf("Timestamp = %v, want %v", got.Timestamp, ts)
		}
		if !jsonEqual(t, got.Metadata, in.Metadata) {
			t.Errorf("Metadata = %s, want %s (jsonb round-trip)", got.Metadata, in.Metadata)
		}
		if got.ID == uuid.Nil {
			t.Error("ID not populated on read-back")
		}
	})

	t.Run("Append_ZeroTimestampDefaultsToNow", func(t *testing.T) {
		before := time.Now().Add(-2 * time.Second)
		// Zero Timestamp → the COALESCE(NULLIF(...)) default fires and
		// the server stamps now(); a system-level (NULL account) row.
		if err := audit.Append(ctx, platform.AuditEntry{
			ActorKind: platform.ActorSystem,
			Action:    "cursor.reset",
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
		got := listOne(t, ctx, audit, platform.AuditQuery{Action: "cursor.reset"})
		if got.Timestamp.Before(before) || got.Timestamp.After(time.Now().Add(2*time.Second)) {
			t.Errorf("Timestamp = %v, want ~now (server default)", got.Timestamp)
		}
		if got.AccountID != uuid.Nil {
			t.Errorf("AccountID = %v, want Nil for a system-level row", got.AccountID)
		}
	})

	t.Run("List_FiltersAndOrdering", func(t *testing.T) {
		base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		// Three staff key.revoke rows at increasing times; List must
		// return them newest-first (ts DESC) and the time window +
		// action filters must scope correctly.
		for i := 0; i < 3; i++ {
			if err := audit.Append(ctx, platform.AuditEntry{
				AccountID: acct.ID,
				ActorKind: platform.ActorStaff,
				Action:    "key.revoke",
				TargetID:  string(rune('a' + i)),
				Timestamp: base.Add(time.Duration(i) * time.Hour),
			}); err != nil {
				t.Fatalf("append %d: %v", i, err)
			}
		}

		rows, err := audit.List(ctx, platform.AuditQuery{Action: "key.revoke"})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(rows) != 3 {
			t.Fatalf("len = %d, want 3", len(rows))
		}
		for i := 1; i < len(rows); i++ {
			if rows[i-1].Timestamp.Before(rows[i].Timestamp) {
				t.Errorf("rows not ts DESC: [%d]=%v before [%d]=%v",
					i-1, rows[i-1].Timestamp, i, rows[i].Timestamp)
			}
		}

		// Half-open [From,To) window: To is exclusive, so the row at
		// base+2h is excluded; From is inclusive so base is kept.
		windowed, err := audit.List(ctx, platform.AuditQuery{
			Action: "key.revoke",
			From:   base,
			To:     base.Add(2 * time.Hour),
		})
		if err != nil {
			t.Fatalf("windowed list: %v", err)
		}
		if len(windowed) != 2 {
			t.Errorf("windowed len = %d, want 2 (To is exclusive)", len(windowed))
		}

		// ActorKind filter excludes the webhook + system rows above.
		staffOnly, err := audit.List(ctx, platform.AuditQuery{ActorKind: platform.ActorStaff})
		if err != nil {
			t.Fatalf("staff list: %v", err)
		}
		for _, r := range staffOnly {
			if r.ActorKind != platform.ActorStaff {
				t.Errorf("actor filter leaked %q", r.ActorKind)
			}
		}
		if len(staffOnly) != 3 {
			t.Errorf("staff rows = %d, want 3", len(staffOnly))
		}

		// AccountID filter: the system-level (NULL account) row must
		// NOT appear when scoping to a specific account.
		byAccount, err := audit.List(ctx, platform.AuditQuery{AccountID: acct.ID})
		if err != nil {
			t.Fatalf("account list: %v", err)
		}
		for _, r := range byAccount {
			if r.AccountID != acct.ID {
				t.Errorf("account filter leaked %v", r.AccountID)
			}
		}

		// Limit is honoured (and stays ts DESC).
		limited, err := audit.List(ctx, platform.AuditQuery{Action: "key.revoke", Limit: 1})
		if err != nil {
			t.Fatalf("limited list: %v", err)
		}
		if len(limited) != 1 {
			t.Fatalf("limited len = %d, want 1", len(limited))
		}
		if !limited[0].Timestamp.Equal(base.Add(2 * time.Hour)) {
			t.Errorf("limit did not return the newest row: %v", limited[0].Timestamp)
		}
	})

	t.Run("AppendBatch_InsertsAll", func(t *testing.T) {
		batch := []platform.AuditEntry{
			{AccountID: acct.ID, ActorKind: platform.ActorSystem, Action: "batch.one", Timestamp: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)},
			{AccountID: acct.ID, ActorKind: platform.ActorSystem, Action: "batch.two", Timestamp: time.Date(2026, 5, 1, 1, 0, 0, 0, time.UTC)},
		}
		if err := audit.AppendBatch(ctx, batch); err != nil {
			t.Fatalf("append batch: %v", err)
		}
		for _, action := range []string{"batch.one", "batch.two"} {
			if got := listOne(t, ctx, audit, platform.AuditQuery{Action: action}); got.Action != action {
				t.Errorf("batch row %q not found", action)
			}
		}
	})

	t.Run("Append_RejectsOverlongAction", func(t *testing.T) {
		// audit_log CHECK (length(action) BETWEEN 1 AND 100). A 101-char
		// action must surface the DB error, not be silently truncated.
		err := audit.Append(ctx, platform.AuditEntry{
			ActorKind: platform.ActorSystem,
			Action:    strings.Repeat("x", 101),
		})
		if err == nil {
			t.Error("Append with a 101-char action: want CHECK-constraint error, got nil")
		}
	})
}

// listOne fetches the newest row matching q and fails if there isn't
// exactly one visible under a tight limit.
func listOne(t *testing.T, ctx context.Context, s *postgresstore.AuditStore, q platform.AuditQuery) platform.AuditEntry {
	t.Helper()
	q.Limit = 1
	rows, err := s.List(ctx, q)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("no rows for query %+v", q)
	}
	return rows[0]
}

// jsonEqual compares two JSON blobs semantically (jsonb may reorder
// keys / restyle whitespace on the round-trip).
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshal a (%s): %v", a, err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshal b (%s): %v", b, err)
	}
	return reflect.DeepEqual(av, bv)
}
