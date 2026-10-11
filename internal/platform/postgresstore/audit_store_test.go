package postgresstore

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// TestBuildAuditWhere_Empty: an empty query yields no WHERE and a nil arg slice, since List appends ORDER BY/LIMIT directly.
func TestBuildAuditWhere_Empty(t *testing.T) {
	where, args := buildAuditWhere(platform.AuditQuery{})
	if where != "" {
		t.Errorf("where = %q, want empty", where)
	}
	if args != nil {
		t.Errorf("args = %v, want nil", args)
	}
}

// TestBuildAuditWhere_SingleFilters: each scoping field maps to exactly one clause bound to $1; a swapped >= / < would return the wrong half of a time range.
func TestBuildAuditWhere_SingleFilters(t *testing.T) {
	acct := uuid.New()
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		q         platform.AuditQuery
		wantWhere string
		wantArg   any
	}{
		{"account", platform.AuditQuery{AccountID: acct}, "WHERE account_id = $1", acct},
		{"actor_kind", platform.AuditQuery{ActorKind: platform.ActorStaff}, "WHERE actor_kind = $1", string(platform.ActorStaff)},
		{"action", platform.AuditQuery{Action: "key.mint"}, "WHERE action = $1", "key.mint"},
		{"from", platform.AuditQuery{From: from}, "WHERE ts >= $1", from},
		{"to", platform.AuditQuery{To: to}, "WHERE ts < $1", to},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			where, args := buildAuditWhere(tc.q)
			if where != tc.wantWhere {
				t.Errorf("where = %q, want %q", where, tc.wantWhere)
			}
			if len(args) != 1 {
				t.Fatalf("len(args) = %d, want 1", len(args))
			}
			if !reflect.DeepEqual(args[0], tc.wantArg) {
				t.Errorf("args[0] = %v (%T), want %v (%T)", args[0], args[0], tc.wantArg, tc.wantArg)
			}
		})
	}
}

// TestBuildAuditWhere_AllFilters: placeholders must count 1..5 in the exact order the args are appended, or Postgres binds the wrong value to a column.
func TestBuildAuditWhere_AllFilters(t *testing.T) {
	acct := uuid.New()
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	q := platform.AuditQuery{
		AccountID: acct,
		ActorKind: platform.ActorWebhook,
		Action:    "plan.upgrade",
		From:      from,
		To:        to,
	}

	where, args := buildAuditWhere(q)

	const want = "WHERE account_id = $1 AND actor_kind = $2 AND " +
		"action = $3 AND ts >= $4 AND ts < $5"
	if where != want {
		t.Errorf("where = %q, want %q", where, want)
	}
	wantArgs := []any{acct, string(platform.ActorWebhook), "plan.upgrade", from, to}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %v, want %v", args, wantArgs)
	}
}

// TestNullable_Helpers: Go zero values must map to SQL NULL and non-zero values pass through, or empty strings and zero UUIDs break FK SET NULL semantics.
func TestNullable_Helpers(t *testing.T) {
	if got := nullableUUID(uuid.Nil); got != nil {
		t.Errorf("nullableUUID(Nil) = %v, want nil", got)
	}
	id := uuid.New()
	if got := nullableUUID(id); got != id {
		t.Errorf("nullableUUID(id) = %v, want %v", got, id)
	}

	if got := nullableString(""); got != nil {
		t.Errorf("nullableString(\"\") = %v, want nil", got)
	}
	if got := nullableString("api_key"); got != "api_key" {
		t.Errorf("nullableString = %v, want api_key", got)
	}

	if got := nullableJSONB(nil); got != nil {
		t.Errorf("nullableJSONB(nil) = %v, want nil", got)
	}
	if got := nullableJSONB([]byte{}); got != nil {
		t.Errorf("nullableJSONB(empty) = %v, want nil", got)
	}
	body := []byte(`{"to":"pro"}`)
	if got := nullableJSONB(body); !reflect.DeepEqual(got, body) {
		t.Errorf("nullableJSONB = %v, want %v", got, body)
	}

	if got := nullableInet(nil); got != nil {
		t.Errorf("nullableInet(nil) = %v, want nil", got)
	}
	// nullableInet stringifies so the pq driver binds it into the
	// inet column; a raw net.IP would otherwise error at bind time.
	if got := nullableInet(net.ParseIP("203.0.113.7")); got != "203.0.113.7" {
		t.Errorf("nullableInet = %v, want 203.0.113.7", got)
	}
}

// TestAuditAppend_ValidationRejectsEmpty: Append and AppendBatch reject empty action/actor_kind before any IO (a nil-DB store would panic on ExecContext).
func TestAuditAppend_ValidationRejectsEmpty(t *testing.T) {
	a := NewAuditStore(New(nil))
	ctx := context.Background()

	if err := a.Append(ctx, platform.AuditEntry{ActorKind: platform.ActorUser}); err == nil {
		t.Error("Append with empty Action: want error, got nil")
	}
	if err := a.Append(ctx, platform.AuditEntry{Action: "key.mint"}); err == nil {
		t.Error("Append with empty ActorKind: want error, got nil")
	}
	// AppendBatch must surface an invalid entry's error (wrapped with
	// its index) rather than silently dropping it. The invalid entry
	// is first so the guard fires before any (nil-DB) IO is attempted.
	batch := []platform.AuditEntry{
		{Action: "", ActorKind: platform.ActorSystem, Metadata: json.RawMessage(`{}`)}, // invalid
		{Action: "ok.two", ActorKind: platform.ActorSystem},
	}
	if err := a.AppendBatch(ctx, batch); err == nil {
		t.Error("AppendBatch with an invalid entry: want error, got nil")
	}
}

// TestAuditStoreDocDoesNotClaimUnbuiltDashboardReader: the AuditStore doc must not claim List powers a dashboard surface (no non-test caller).
func TestAuditStoreDocDoesNotClaimUnbuiltDashboardReader(t *testing.T) {
	src, err := os.ReadFile("audit_store.go")
	if err != nil {
		t.Fatalf("read audit_store.go: %v", err)
	}
	text := string(src)
	if strings.Contains(text, "powers the dashboard's audit-trail surface") {
		t.Error(`audit_store.go doc still claims List "powers the dashboard's audit-trail surface"; no dashboard or API route calls List`)
	}
	if !strings.Contains(text, "no dashboard or API route calls it yet") {
		t.Error("audit_store.go's AuditStore doc should say plainly that List isn't wired into any dashboard/API route")
	}
	for _, invented := range []string{"staff/ops tooling", "staff console", "the dashboard paginates"} {
		if strings.Contains(text, invented) {
			t.Errorf("audit_store.go names %q as a List reader; List has no non-test caller", invented)
		}
	}
}

// TestMigration0027AuditLogCommentDoesNotClaimImplementedRetention: the migration header must not present 12-month/S3 retention as current (no retention job exists).
func TestMigration0027AuditLogCommentDoesNotClaimImplementedRetention(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	migPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "..",
		"migrations", "0027_platform_v1_schema.up.sql")
	src, err := os.ReadFile(migPath)
	if err != nil {
		t.Fatalf("read migration 0027: %v", err)
	}
	text := string(src)
	if !strings.Contains(text, "No retention/archival job exists") {
		t.Error("migration 0027's audit_log header comment should say plainly that no retention/archival job exists yet, not present it as current behavior")
	}
}
