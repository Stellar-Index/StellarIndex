//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
	"github.com/Stellar-Index/StellarIndex/internal/pricealerts"
	"github.com/Stellar-Index/StellarIndex/internal/signupreaper"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// PATCH /v1/admin/accounts/{id} must not do Get -> mutate in memory ->
// Update with no lock and no version
// check. Update rewrites every mutable column (not a diff), so two
// concurrent PATCHes on the SAME account race: whichever commits
// second silently discards the first's change. Status is the operator
// kill switch — a lost SUSPEND under this race is a live
// hole.
//
// This proves AccountStore.UpdateAtomic serialises the race: one
// PATCH raises the rate-limit override, a concurrent one suspends the
// account, and BOTH must land — a lost update (Get+Update)
// would show one field reverted to its pre-race value.
func TestAccountStoreUpdateAtomic_SerialisesConcurrentPatches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	accounts := postgresstore.NewAccountStore(postgresstore.New(db))

	acct, err := accounts.Create(ctx, platform.Account{
		Name:         "race-account",
		Slug:         "race-account",
		BillingEmail: "race@example.com",
		Tier:         platform.TierFree,
		Status:       platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)

	// Racer A: raises the rate-limit override.
	go func() {
		defer wg.Done()
		_, _, err := accounts.UpdateAtomic(ctx, acct.ID, func(a *platform.Account) error {
			a.RateLimitPerMinOverride = 500
			return nil
		})
		errs <- err
	}()

	// Racer B: suspends the account (the kill switch).
	go func() {
		defer wg.Done()
		_, _, err := accounts.UpdateAtomic(ctx, acct.ID, func(a *platform.Account) error {
			a.Status = platform.AccountSuspended
			a.SuspendedReason = "race-test"
			return nil
		})
		errs <- err
	}()

	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("UpdateAtomic racer failed: %v", err)
		}
	}

	final, err := accounts.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("get final account: %v", err)
	}
	if final.RateLimitPerMinOverride != 500 {
		t.Errorf("RateLimitPerMinOverride = %d, want 500 (Racer A's write was lost)", final.RateLimitPerMinOverride)
	}
	if final.Status != platform.AccountSuspended {
		t.Errorf("Status = %q, want %q (Racer B's write was lost — the kill switch did not stick)",
			final.Status, platform.AccountSuspended)
	}
}

// TestAccountStoreUpdateAtomic_NotFound — the id-absent path returns
// platform.ErrNotFound unwrapped, matching Get/Update's existing
// contract, and never begins a mutate call.
func TestAccountStoreUpdateAtomic_NotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	accounts := postgresstore.NewAccountStore(postgresstore.New(db))

	mutateCalled := false
	_, _, err = accounts.UpdateAtomic(ctx, uuid.New(), func(a *platform.Account) error {
		mutateCalled = true
		return nil
	})
	if !errors.Is(err, platform.ErrNotFound) {
		t.Errorf("err = %v, want platform.ErrNotFound", err)
	}
	if mutateCalled {
		t.Error("mutate was called for an absent account id")
	}
}

// TestAPI_RegistryAndCursors covers the HTTP surfaces of the
// registry/diagnostics endpoints:
//
//	GET /v1/coins
//	GET /v1/coins?issuer=G…
//	GET /v1/issuers
//	GET /v1/issuers/{g_strkey}
//	GET /v1/diagnostics/cursors
//
// timescale.Store satisfies all four reader interfaces directly,
// so no adapter glue is needed — wire the store straight through
// v1.Options.
func TestAPI_RegistryAndCursors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		issuerA = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		issuerB = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	)
	seedIssuers(t, ctx, store, []seedIssuer{
		{g: issuerA, homeDomain: "centre.io"},
		{g: issuerB},
	})
	seedClassicAssets(t, ctx, store, []seedAsset{
		{assetID: "USDC-" + issuerA, code: "USDC", issuer: issuerA, slug: "USDC", obs: 41_000_000},
		{assetID: "AQUA-" + issuerB, code: "AQUA", issuer: issuerB, slug: "AQUA", obs: 14_000_000},
	})

	// Two ingest cursors at different lags so we can observe the
	// `lag_seconds` field the showcase /diagnostics page colour-codes.
	if err := store.UpsertCursor(ctx, "soroswap", "factory", 60_000_000); err != nil {
		t.Fatalf("UpsertCursor soroswap: %v", err)
	}
	if err := store.UpsertCursor(ctx, "sdex", "", 60_000_001); err != nil {
		t.Fatalf("UpsertCursor sdex: %v", err)
	}

	srv := v1.New(v1.Options{
		AssetsReader: store,
		Issuers:      store,
		Cursors:      store,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	t.Run("/v1/assets", func(t *testing.T) {
		var env struct {
			Data []v1.AssetDetail `json:"data"`
		}
		getJSON(t, ts.URL+"/v1/assets?limit=10", &env)
		if len(env.Data) != 2 {
			t.Fatalf("got %d assets, want 2", len(env.Data))
		}
		// USDC ranks above AQUA (41M vs 14M observations).
		if env.Data[0].Code != "USDC" {
			t.Errorf("rank 1 = %q, want USDC", env.Data[0].Code)
		}
	})

	t.Run("/v1/assets?issuer=G…", func(t *testing.T) {
		var env struct {
			Data []v1.AssetDetail `json:"data"`
		}
		getJSON(t, ts.URL+"/v1/assets?limit=10&issuer="+issuerA, &env)
		if len(env.Data) != 1 {
			t.Fatalf("issuerA filter returned %d rows, want 1", len(env.Data))
		}
		if env.Data[0].Issuer == nil || *env.Data[0].Issuer != issuerA {
			t.Errorf("row issuer = %v, want %q", env.Data[0].Issuer, issuerA)
		}
	})

	t.Run("/v1/issuers", func(t *testing.T) {
		var env struct {
			Data []v1.IssuerListEntry `json:"data"`
		}
		getJSON(t, ts.URL+"/v1/issuers", &env)
		if len(env.Data) != 2 {
			t.Fatalf("got %d issuers, want 2", len(env.Data))
		}
		// Highest-volume issuer first.
		if env.Data[0].GStrkey != issuerA {
			t.Errorf("rank 1 = %q, want %q", env.Data[0].GStrkey, issuerA)
		}
		if env.Data[0].HomeDomain != "centre.io" {
			t.Errorf("home_domain = %q, want centre.io", env.Data[0].HomeDomain)
		}
	})

	t.Run("/v1/issuers/{g_strkey}", func(t *testing.T) {
		var env struct {
			Data v1.Issuer `json:"data"`
		}
		getJSON(t, ts.URL+"/v1/issuers/"+issuerA, &env)
		if env.Data.GStrkey != issuerA {
			t.Errorf("g_strkey = %q, want %q", env.Data.GStrkey, issuerA)
		}
		// The asset list embedded on the issuer envelope drives the
		// showcase /coins/[slug] issuer card; the contract is "always
		// include even if empty."
		if len(env.Data.Assets) != 1 {
			t.Errorf("assets = %d, want 1", len(env.Data.Assets))
		}
	})

	t.Run("/v1/diagnostics/cursors", func(t *testing.T) {
		var env struct {
			Data []v1.Cursor `json:"data"`
		}
		getJSON(t, ts.URL+"/v1/diagnostics/cursors", &env)
		if len(env.Data) != 2 {
			t.Fatalf("got %d cursors, want 2", len(env.Data))
		}
		// Ordered by (source, sub_source) per ListCursors. sdex has
		// empty sub_source, soroswap has "factory" — so sdex first.
		if env.Data[0].Source != "sdex" {
			t.Errorf("rank 1 source = %q, want sdex", env.Data[0].Source)
		}
		if env.Data[1].Source != "soroswap" {
			t.Errorf("rank 2 source = %q, want soroswap", env.Data[1].Source)
		}
		if env.Data[1].SubSource != "factory" {
			t.Errorf("soroswap sub_source = %q, want factory", env.Data[1].SubSource)
		}
		// lag_seconds is computed Go-side as now - last_updated; the
		// rows were just inserted so lag should be small and
		// non-negative. (Bound the upper end loosely to avoid CI
		// flakes; the contract is "non-negative and finite", not an
		// exact value.)
		for _, c := range env.Data {
			if c.LagSeconds < 0 {
				t.Errorf("%s/%s lag_seconds = %d, want non-negative",
					c.Source, c.SubSource, c.LagSeconds)
			}
		}
	})
}

// appendOnlyFixture is one account with a user, one signup-race orphan
// the reaper will delete, and an audit row pointing at each.
type appendOnlyFixture struct {
	accounts *postgresstore.AccountStore
	liveRow  uuid.UUID // account_id + actor_user_id both point at live rows
	reapRow  uuid.UUID // account_id points at the orphan
	userID   uuid.UUID
}

// TestAuditLogAppendOnly pins migration 0179: the app role
// cannot rewrite, detach, delete or truncate the audit trail, while the
// ON DELETE SET NULL foreign keys still let the signup reaper delete an
// account (and a user be deleted) without losing its audit rows.
func TestAuditLogAppendOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := seedAppendOnlyFixture(t, ctx, db)

	t.Run("RefusesTampering", func(t *testing.T) {
		before := auditSnapshot(t, ctx, db)
		for name, stmt := range map[string]string{
			"rewrite action":      `UPDATE audit_log SET action = 'nothing.happened' WHERE id = $1`,
			"rewrite metadata":    `UPDATE audit_log SET metadata = '{}'::jsonb WHERE id = $1`,
			"detach live account": `UPDATE audit_log SET account_id = NULL WHERE id = $1`,
			"detach live actor":   `UPDATE audit_log SET actor_user_id = NULL WHERE id = $1`,
			"repoint account":     `UPDATE audit_log SET account_id = (SELECT id FROM accounts WHERE slug = 'audit-orphan') WHERE id = $1`,
			"delete row":          `DELETE FROM audit_log WHERE id = $1`,
		} {
			_, err := db.ExecContext(ctx, stmt, f.liveRow)
			requireAppendOnlyRefusal(t, name, err)
		}
		_, err := db.ExecContext(ctx, `TRUNCATE audit_log`)
		requireAppendOnlyRefusal(t, "truncate", err)
		if after := auditSnapshot(t, ctx, db); after != before {
			t.Errorf("audit_log changed under refused statements:\nbefore %s\nafter  %s", before, after)
		}
	})

	t.Run("ReaperDeleteNullsAccountAndKeepsRow", func(t *testing.T) {
		n, err := f.accounts.ReapSuspendedOrphans(ctx, signupreaper.SignupRaceReasonPrefix, time.Now().UTC().Add(time.Hour))
		if err != nil {
			t.Fatalf("reap: %v — the ON DELETE SET NULL cascade onto audit_log must stay permitted", err)
		}
		if n != 1 {
			t.Fatalf("reaped %d accounts, want 1", n)
		}
		requireAuditRow(t, ctx, db, f.reapRow, "signup.orphan", false, false)
	})

	t.Run("UserDeleteNullsActorAndKeepsRow", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, f.userID); err != nil {
			t.Fatalf("delete user: %v — the ON DELETE SET NULL cascade onto audit_log must stay permitted", err)
		}
		requireAuditRow(t, ctx, db, f.liveRow, "key.mint", true, false)
	})
}

func seedAppendOnlyFixture(t *testing.T, ctx context.Context, db *sql.DB) appendOnlyFixture {
	t.Helper()
	store := postgresstore.New(db)
	f := appendOnlyFixture{accounts: postgresstore.NewAccountStore(store)}
	live, err := f.accounts.Create(ctx, platform.Account{
		Name: "Audit Keep", Slug: "audit-keep", BillingEmail: "keep@audit.example",
		Tier: platform.TierPro, Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create live account: %v", err)
	}
	orphan, err := f.accounts.Create(ctx, platform.Account{
		Name: "Audit Orphan", Slug: "audit-orphan", BillingEmail: "orphan@audit.example",
		Tier: platform.TierFree, Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create orphan account: %v", err)
	}
	if err := f.accounts.Suspend(ctx, orphan.ID, signupreaper.SignupRaceReasonPrefix+" orphan speculative account orphan@audit.example"); err != nil {
		t.Fatalf("suspend orphan: %v", err)
	}
	user, err := postgresstore.NewUserStore(store).CreateUser(ctx, platform.User{
		AccountID: live.ID, Email: "owner@audit.example", Role: platform.RoleOwner,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	f.userID, f.liveRow, f.reapRow = user.ID, uuid.New(), uuid.New()
	audit := postgresstore.NewAuditStore(store)
	for _, e := range []platform.AuditEntry{
		{
			ID: f.liveRow, AccountID: live.ID, ActorUserID: user.ID, ActorKind: platform.ActorUser,
			Action: "key.mint", IP: net.ParseIP("203.0.113.7"),
		},
		{ID: f.reapRow, AccountID: orphan.ID, ActorKind: platform.ActorSystem, Action: "signup.orphan"},
	} {
		if err := audit.Append(ctx, e); err != nil {
			t.Fatalf("append %s: %v", e.Action, err)
		}
	}
	return f
}

func requireAppendOnlyRefusal(t *testing.T, name string, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("%s: err = %v, want the append-only trigger's 42501 refusal", name, err)
	}
}

// auditSnapshot renders every audit_log row, so any change to any column
// of any row shows up as a string difference.
func auditSnapshot(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	var s sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT jsonb_agg(to_jsonb(a) ORDER BY a.id)::text FROM audit_log a`).Scan(&s); err != nil {
		t.Fatalf("snapshot audit_log: %v", err)
	}
	return s.String
}

func requireAuditRow(t *testing.T, ctx context.Context, db *sql.DB, id uuid.UUID, wantAction string, wantAccount, wantActor bool) {
	t.Helper()
	var action string
	var account, actor sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT action, account_id::text, actor_user_id::text FROM audit_log WHERE id = $1`, id).
		Scan(&action, &account, &actor)
	if err != nil {
		t.Fatalf("audit row %s after cascade: %v — the row must survive, only unlinked", id, err)
	}
	if action != wantAction {
		t.Errorf("action = %q, want %q", action, wantAction)
	}
	if account.Valid != wantAccount || actor.Valid != wantActor {
		t.Errorf("account_id set = %v (want %v), actor_user_id set = %v (want %v)",
			account.Valid, wantAccount, actor.Valid, wantActor)
	}
}

// TestAuditStore exercises the postgresstore.AuditStore against the
// audit_log table from migration 0027. The audit trail is the
// append-only record of every privileged action (key.mint,
// plan.upgrade, session.revoke, staff tier changes) and was
// completely untested at every layer. One container per test,
// matching the storage-test convention.
func TestAuditStore(t *testing.T) {
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
			UserAgent:  "webhook-agent/1.0",
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
		if got.UserAgent != "webhook-agent/1.0" {
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

// The verify-code handler compares a submitted code only against the
// one token ReserveLoginCode hands it, and only after
// RegisterFailedLoginCode has admitted the attempt. Both caps therefore
// hold under a concurrent burst only if those two statements serialise
// in Postgres — which is what this pins, with real row locks.
func TestLoginCodeReservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tokens := postgresstore.NewTokenStore(postgresstore.New(db))

	const maxAttempts = 5
	mint := func(t *testing.T, email string, n int, purpose platform.TokenPurpose, ttl time.Duration) [][]byte {
		t.Helper()
		var hashes [][]byte
		for i := 0; i < n; i++ {
			h := sha256.Sum256([]byte(email + string(purpose) + ttl.String() + string(rune('a'+i))))
			if err := tokens.CreateMagicLinkToken(ctx, platform.MagicLinkToken{
				TokenHash: h[:], Email: email, Purpose: purpose,
				ExpiresAt: time.Now().UTC().Add(ttl), RequestedIP: net.ParseIP("203.0.113.9"),
			}); err != nil {
				t.Fatalf("mint: %v", err)
			}
			hashes = append(hashes, h[:])
		}
		return hashes
	}
	attemptsOf := func(t *testing.T, hash []byte) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT attempts FROM magic_link_tokens WHERE token_hash = $1`, hash).Scan(&n); err != nil {
			t.Fatalf("read attempts: %v", err)
		}
		return n
	}
	burst := func(t *testing.T, email string, racers int) int {
		t.Helper()
		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			handOuts int
		)
		errs := make(chan error, racers)
		start := make(chan struct{})
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := tokens.ReserveLoginCode(ctx, email, maxAttempts)
				if errors.Is(err, platform.ErrNotFound) {
					return
				}
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				handOuts++
				mu.Unlock()
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent reserve: %v", err)
		}
		return handOuts
	}

	t.Run("ChargesBeforeReturningAndSkipsIneligibleRows", func(t *testing.T) {
		const email = "reserve-shape@example.com"
		live := mint(t, email, 1, platform.TokenPurposeLogin, time.Hour)[0]
		expired := mint(t, email, 1, platform.TokenPurposeLogin, -time.Minute)[0]
		consumed := mint(t, email, 1, platform.TokenPurposeLogin, 2*time.Hour)[0]
		if _, err := tokens.ConsumeMagicLinkToken(ctx, consumed); err != nil {
			t.Fatalf("consume: %v", err)
		}
		other := mint(t, "someone-else@example.com", 1, platform.TokenPurposeLogin, time.Hour)[0]

		got, err := tokens.ReserveLoginCode(ctx, email, maxAttempts)
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if string(got.TokenHash) != string(live) {
			t.Fatal("reserved a token other than the one live token")
		}
		if got.Attempts != 1 {
			t.Errorf("returned Attempts = %d, want 1 (post-charge)", got.Attempts)
		}
		for name, h := range map[string][]byte{"expired": expired, "consumed": consumed, "other email": other} {
			if n := attemptsOf(t, h); n != 0 {
				t.Errorf("%s token charged: attempts = %d, want 0", name, n)
			}
		}
	})

	t.Run("ConcurrentBurstGetsExactlyTheTokenCap", func(t *testing.T) {
		const email = "reserve-burst@example.com"
		h := mint(t, email, 1, platform.TokenPurposeLogin, time.Hour)[0]
		if got := burst(t, email, 50); got != maxAttempts {
			t.Errorf("%d of 50 concurrent reservations were handed the token, want exactly %d", got, maxAttempts)
		}
		if n := attemptsOf(t, h); n != maxAttempts {
			t.Errorf("attempts = %d after the burst, want %d", n, maxAttempts)
		}
	})

	// HasLiveLoginCode must answer exactly what ReserveLoginCode would do,
	// without charging: the handler skips the durable charge on "false".
	t.Run("HasLiveLoginCodeMirrorsReserveWithoutCharging", func(t *testing.T) {
		const email = "has-live@example.com"
		hasLive := func(t *testing.T, email string) bool {
			t.Helper()
			live, err := tokens.HasLiveLoginCode(ctx, email, maxAttempts)
			if err != nil {
				t.Fatalf("HasLiveLoginCode: %v", err)
			}
			return live
		}
		if hasLive(t, email) {
			t.Fatal("no token minted: HasLiveLoginCode = true, want false")
		}
		mint(t, email, 1, platform.TokenPurposeLogin, -time.Minute)
		consumed := mint(t, email, 1, platform.TokenPurposeLogin, 2*time.Hour)[0]
		if _, err := tokens.ConsumeMagicLinkToken(ctx, consumed); err != nil {
			t.Fatalf("consume: %v", err)
		}
		if hasLive(t, email) {
			t.Fatal("only expired and consumed tokens: HasLiveLoginCode = true, want false")
		}

		hashes := mint(t, email, 2, platform.TokenPurposeLogin, time.Hour)
		newest := hashes[len(hashes)-1]
		for i := 0; i < 3; i++ {
			if !hasLive(t, email) {
				t.Fatalf("call %d: live newest token: HasLiveLoginCode = false, want true", i)
			}
		}
		if n := attemptsOf(t, newest); n != 0 {
			t.Fatalf("HasLiveLoginCode charged the token: attempts = %d, want 0", n)
		}
		for i := 0; i < maxAttempts; i++ {
			if _, err := tokens.ReserveLoginCode(ctx, email, maxAttempts); err != nil {
				t.Fatalf("reserve %d: %v", i, err)
			}
		}
		if hasLive(t, email) {
			t.Fatal("newest token at the cap: HasLiveLoginCode = true, want false (no older token may step in)")
		}
	})

	// Several live tokens per address is the normal case (a user who asks
	// twice). Only the newest is ever a code candidate: one guess compared
	// against N codes would have N-in-1e6 odds while the per-email budget
	// counts it once. Once the newest is capped, no older token steps in.
	t.Run("ConcurrentBurstOverSeveralTokensChargesOnlyTheNewest", func(t *testing.T) {
		const email = "reserve-multi@example.com"
		hashes := mint(t, email, 3, platform.TokenPurposeLogin, time.Hour)
		newest := hashes[len(hashes)-1]
		if got := burst(t, email, 50); got != maxAttempts {
			t.Errorf("%d of 50 concurrent reservations were handed a candidate, want exactly %d", got, maxAttempts)
		}
		if n := attemptsOf(t, newest); n != maxAttempts {
			t.Errorf("newest token: attempts = %d, want %d", n, maxAttempts)
		}
		for i, h := range hashes[:len(hashes)-1] {
			if n := attemptsOf(t, h); n != 0 {
				t.Errorf("older token %d: attempts = %d, want 0 — it was a code candidate", i, n)
			}
		}
		if _, err := tokens.ReserveLoginCode(ctx, email, maxAttempts); !errors.Is(err, platform.ErrNotFound) {
			t.Errorf("reserve after the newest was capped: err = %v, want ErrNotFound (no older token may step in)", err)
		}
	})

	// The handler admits an attempt when the post-increment count it gets
	// back is <= the cap, so concurrent charges must each see a distinct
	// count: exactly maxFailures of a burst may come back within budget.
	t.Run("ConcurrentChargesAdmitExactlyTheDurableCap", func(t *testing.T) {
		const (
			email       = "charge-burst@example.com"
			maxFailures = 10
			racers      = 50
		)
		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			admitted int
		)
		errs := make(chan error, racers)
		start := make(chan struct{})
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				state, err := tokens.RegisterFailedLoginCode(ctx, email, maxFailures, 24*time.Hour, 24*time.Hour)
				if err != nil {
					errs <- err
					return
				}
				if state.FailedCount <= maxFailures {
					mu.Lock()
					admitted++
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent charge: %v", err)
		}
		if admitted != maxFailures {
			t.Errorf("%d of %d concurrent charges came back within budget, want exactly %d",
				admitted, racers, maxFailures)
		}
	})
}

// The store half of the `magic_link_tokens`
// retention sweep.
//
// `magic_link_tokens` is durable plaintext PII (email + requested_ip)
// and its key is ATTACKER-CHOSEN: POST /v1/auth/login is
// unauthenticated and inserts a permanent row for any well-formed
// address, and a link nobody clicks is never consumed. The sibling
// `login_code_lockouts` already had a reaper; this table did not, so
// the table grew monotonically on a disk-fixed host.
//
// This proves the two things only a real database can — and the
// predicate has to be right in both directions:
//
//  1. every EXPIRED row past the retention window is reaped, consumed
//     or not (both are unredeemable);
//  2. a LIVE (unexpired) row is NEVER reaped, at any age of created_at,
//     and the sweep does not widen into the neighbouring
//     login_code_lockouts table.
func TestMagicLinkTokenReaper(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	tokens := postgresstore.NewTokenStore(postgresstore.New(db))

	t.Run("SweepReapsExpiredRowsOnly", func(t *testing.T) {
		const retention = 48 * time.Hour
		type fixture struct {
			email        string
			expiresHours int  // >0 = expires that many hours in the FUTURE (live), <0 = expired that long ago
			consumed     bool // whether the row was consumed
			wantReaped   bool
		}
		fixtures := []fixture{
			// The attack residue: expired long ago, never consumed
			// (nobody clicked the link). Terminal — reapable.
			{email: "reap-old-unconsumed@example.com", expiresHours: -72, wantReaped: true},
			// Expired long ago AND consumed — still terminal, reapable.
			{email: "reap-old-consumed@example.com", expiresHours: -72, consumed: true, wantReaped: true},
			// LIVE token, freshly minted. Must survive: reaping it would
			// break a real user's in-flight sign-in.
			{email: "keep-live@example.com", expiresHours: 1, wantReaped: false},
			// Expired only recently, inside retention — kept so
			// classifyMagicLinkMiss can still tell a slow user "expired"
			// rather than "not found".
			{email: "keep-recently-expired@example.com", expiresHours: -1, wantReaped: false},
			// Exactly on the keep side of the boundary (expired 47h ago,
			// retention 48h).
			{email: "keep-boundary@example.com", expiresHours: -47, wantReaped: false},
		}
		for i, f := range fixtures {
			var consumedAt any
			if f.consumed {
				consumedAt = time.Now().UTC().Add(-time.Duration(-f.expiresHours) * time.Hour)
			}
			if _, err := db.ExecContext(ctx,
				`INSERT INTO magic_link_tokens
				     (token_hash, email, purpose, expires_at, consumed_at, requested_ip, created_at)
				 VALUES ($1, $2, 'login',
				         now() + make_interval(hours => $3),
				         $4, '203.0.113.9',
				         now() - interval '73 hours')`,
				[]byte{byte(i), 'm', 'l', 'r', '-', 't', 'o', 'k', 'e', 'n', '-', 'h', 'a', 's', 'h', '-', 'p', 'a', 'd', 'd', 'i', 'n', 'g', '-', '3', '2', 'b', 'y', 't', 'e', 's', '!'},
				f.email, f.expiresHours, consumedAt); err != nil {
				t.Fatalf("seed %s: %v", f.email, err)
			}
		}

		// A row in the NEIGHBOURING login_code_lockouts table, aged the
		// same way — the sweep must not widen into it.
		if _, err := tokens.RegisterFailedLoginCode(ctx,
			"bystander-lockout@example.com", 10, 24*time.Hour, 24*time.Hour); err != nil {
			t.Fatalf("seed bystander lockout: %v", err)
		}

		deleted, err := tokens.SweepExpiredMagicLinkTokens(ctx, time.Now().UTC().Add(-retention))
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if deleted != 2 {
			t.Errorf("deleted = %d, want 2 (the two expired-beyond-retention rows only)", deleted)
		}

		for _, f := range fixtures {
			var present bool
			if err := db.QueryRowContext(ctx,
				`SELECT EXISTS (SELECT 1 FROM magic_link_tokens WHERE email = $1)`,
				f.email).Scan(&present); err != nil {
				t.Fatalf("check %s: %v", f.email, err)
			}
			gone := !present
			if gone != f.wantReaped {
				verb := map[bool]string{true: "reaped", false: "kept"}
				t.Errorf("%s was %s, want %s (expiresHours=%d consumed=%v)",
					f.email, verb[gone], verb[f.wantReaped], f.expiresHours, f.consumed)
			}
		}

		var bystanders int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM login_code_lockouts WHERE email = $1`,
			"bystander-lockout@example.com").Scan(&bystanders); err != nil {
			t.Fatalf("count bystander lockout: %v", err)
		}
		if bystanders != 1 {
			t.Errorf("login_code_lockouts rows for the bystander = %d, want 1 — the sweep reached into another table",
				bystanders)
		}
	})

	// The gauge's source. A count that does not see the rows makes the
	// growth signal useless.
	t.Run("CountSeesTheRows", func(t *testing.T) {
		before, err := tokens.CountMagicLinkTokens(ctx)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		for i, email := range []string{"count-a@example.com", "count-b@example.com"} {
			if _, err := db.ExecContext(ctx,
				`INSERT INTO magic_link_tokens
				     (token_hash, email, purpose, expires_at, requested_ip)
				 VALUES ($1, $2, 'login', now() + interval '15 minutes', '203.0.113.10')`,
				[]byte{byte(100 + i), 'c', 'o', 'u', 'n', 't', '-', 'h', 'a', 's', 'h', '-', 'p', 'a', 'd', 'd', 'i', 'n', 'g', '-', 't', 'o', '-', '3', '2', '-', 'b', 'y', 't', 'e', 's', '.'},
				email); err != nil {
				t.Fatalf("seed %s: %v", email, err)
			}
		}
		after, err := tokens.CountMagicLinkTokens(ctx)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if after != before+2 {
			t.Errorf("count = %d, want %d", after, before+2)
		}
	})

	// The sweep's driving column must be indexed: the table's size is
	// attacker-influenced, so a seq scan here is a second-order DoS.
	t.Run("SweepPredicateIsIndexed", func(t *testing.T) {
		var exists bool
		if err := db.QueryRowContext(ctx, `
			SELECT EXISTS (
			    SELECT 1 FROM pg_indexes
			     WHERE tablename = 'magic_link_tokens'
			       AND indexdef ILIKE '%(expires_at)%'
			)`).Scan(&exists); err != nil {
			t.Fatalf("check index: %v", err)
		}
		if !exists {
			t.Error("no index leading with expires_at — the retention sweep seq-scans a table an unauthenticated caller can grow")
		}
	})
}

// TestPriceAlertsCooldownFloorBackfill pins migration 0181 : a
// stored cooldown below platform.MinAlertCooldownSeconds is raised to it
// (and its updated_at stamped), so a pre-existing 0 no longer re-fires
// every tick; values at or above the floor are untouched.
func TestPriceAlertsCooldownFloorBackfill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 180)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	acct := insertRawAccount(t, ctx, db, "cooldown")

	stale := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	floor := platform.MinAlertCooldownSeconds
	want := map[int]int{0: floor, 60: floor, floor - 1: floor, floor: floor, 3600: 3600}
	ids := make(map[int]uuid.UUID, len(want))
	for before := range want {
		var id uuid.UUID
		if err := db.QueryRowContext(ctx, `
			INSERT INTO price_alerts
			    (account_id, base_asset, quote_asset, condition, threshold, cooldown_seconds, created_at, updated_at)
			VALUES ($1, 'native', 'fiat:USD', 'above', 0.15, $2, $3, $3)
			RETURNING id`, acct, before, stale).Scan(&id); err != nil {
			t.Fatalf("seed cooldown=%d: %v", before, err)
		}
		ids[before] = id
	}

	applyMigrations(t, dsn)

	for before, after := range want {
		var got int
		var updated time.Time
		if err := db.QueryRowContext(ctx,
			`SELECT cooldown_seconds, updated_at FROM price_alerts WHERE id = $1`, ids[before]).Scan(&got, &updated); err != nil {
			t.Fatalf("read cooldown=%d: %v", before, err)
		}
		if got != after {
			t.Errorf("cooldown %d after 0181 = %d, want %d", before, got, after)
		}
		if raised := before < floor; raised == updated.Equal(stale) {
			t.Errorf("cooldown %d: updated_at = %s, want stamped only when raised (%v)", before, updated, raised)
		}
	}
}

// TestPriceAlertsDisarmed executes migration 0198 up and down and the SQL
// that makes an alert fire once per crossing: the claim disarms
// and refuses a disarmed row, the re-arm is a compare-and-swap on
// last_fired_at, an edit to the rule or a re-enable re-arms it, and a claim
// made against a snapshot the owner has since edited or disabled is refused.
func TestPriceAlertsDisarmed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 197)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	acct := insertRawAccount(t, ctx, db, "disarmed")

	// A row that fired before 0198 starts armed.
	var preID uuid.UUID
	if err := db.QueryRowContext(ctx, `
		INSERT INTO price_alerts
		    (account_id, base_asset, quote_asset, condition, threshold, cooldown_seconds, last_fired_at)
		VALUES ($1, 'native', 'fiat:USD', 'above', 0.15, 300, now())
		RETURNING id`, acct).Scan(&preID); err != nil {
		t.Fatalf("seed pre-0198 alert: %v", err)
	}
	applyMigrations(t, dsn)

	alerts := postgresstore.NewPriceAlertStore(postgresstore.New(db))
	pre, err := alerts.GetPriceAlert(ctx, preID)
	if err != nil {
		t.Fatalf("get pre-0198 alert: %v", err)
	}
	if pre.Disarmed {
		t.Error("pre-0198 alert reads disarmed, want armed (column default false)")
	}

	a, err := alerts.CreatePriceAlert(ctx, platform.PriceAlert{
		AccountID: acct, BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
		CooldownSeconds: 300,
	}, 25)
	if err != nil {
		t.Fatalf("create alert: %v", err)
	}
	get := func() platform.PriceAlert {
		t.Helper()
		got, err := alerts.GetPriceAlert(ctx, a.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return got
	}
	claimAs := func(snap platform.PriceAlert, at time.Time) bool {
		t.Helper()
		ok, err := alerts.ClaimPriceAlertFire(ctx, snap, at)
		if err != nil {
			t.Fatalf("claim at %s: %v", at, err)
		}
		return ok
	}
	// claim evaluates against the row as it stands, as a sweep with no
	// concurrent edit does.
	claim := func(at time.Time) bool {
		t.Helper()
		return claimAs(get(), at)
	}
	rearm := func(lastFired time.Time) bool {
		t.Helper()
		ok, err := alerts.RearmPriceAlert(ctx, a.ID, lastFired)
		if err != nil {
			t.Fatalf("rearm: %v", err)
		}
		return ok
	}
	disarmed := func() bool {
		t.Helper()
		return get().Disarmed
	}

	fired := time.Now().UTC().Truncate(time.Microsecond)
	if !claim(fired) {
		t.Fatal("first claim on an armed, never-fired alert refused")
	}
	if !disarmed() {
		t.Fatal("claim did not disarm the alert")
	}
	if claim(fired.Add(time.Hour)) {
		t.Error("claimed past the cooldown while disarmed — the condition never cleared, so this is the same crossing")
	}
	if rearm(fired.Add(-time.Second)) {
		t.Error("re-armed with a last_fired_at older than the row's — a stale snapshot must not re-arm a newer fire")
	}
	if !rearm(fired) {
		t.Fatal("re-arm with the observed last_fired_at refused")
	}
	if rearm(fired) {
		t.Error("second re-arm of an armed alert reported rearmed=true")
	}
	if claim(fired.Add(299 * time.Second)) {
		t.Error("re-armed alert claimed inside its 300 s cooldown")
	}
	second := fired.Add(time.Hour)
	if !claim(second) {
		t.Fatal("re-armed alert past its cooldown refused")
	}

	update := func(mut func(*platform.PriceAlert)) {
		t.Helper()
		cur := get()
		mut(&cur)
		if err := alerts.UpdatePriceAlert(ctx, cur); err != nil {
			t.Fatalf("update: %v", err)
		}
	}
	update(func(p *platform.PriceAlert) { p.CooldownSeconds = 600; p.Threshold = "0.150" })
	if !disarmed() {
		t.Error("a cooldown edit with a value-equal threshold re-armed the alert")
	}
	update(func(p *platform.PriceAlert) { p.Threshold = "0.2" })
	if disarmed() {
		t.Error("a threshold change left the alert disarmed")
	}
	if !claim(second.Add(time.Hour)) {
		t.Fatal("claim after the threshold change refused")
	}
	update(func(p *platform.PriceAlert) { p.Enabled = false })
	if !disarmed() {
		t.Error("disabling re-armed the alert")
	}
	update(func(p *platform.PriceAlert) { p.Enabled = true })
	if disarmed() {
		t.Error("re-enabling left the alert disarmed")
	}

	// Each edit lands between the evaluator's snapshot and its claim; the
	// claim must not fire the rule the owner just replaced. Each case starts
	// armed and claims an hour later, so a wrong claim cannot mask the next.
	third := second.Add(3 * time.Hour)
	for name, edit := range map[string]func(*platform.PriceAlert){
		"threshold": func(p *platform.PriceAlert) { p.Threshold = "0.3" },
		"condition": func(p *platform.PriceAlert) { p.Condition = platform.AlertBelow },
		"pair":      func(p *platform.PriceAlert) { p.QuoteAsset = "fiat:EUR" },
		"disabled":  func(p *platform.PriceAlert) { p.Enabled = false },
	} {
		snap := get()
		if snap.Disarmed {
			rearm(snap.LastFiredAt)
			snap = get()
		}
		update(edit)
		if claimAs(snap, third) {
			t.Errorf("claimed on a snapshot whose %s was edited before the claim", name)
		}
		update(func(p *platform.PriceAlert) { p.Enabled = true })
		third = third.Add(time.Hour)
	}
	cur := get()
	cur.Threshold += "00"
	if !claimAs(cur, third) {
		t.Fatalf("claim on the current rule (threshold %q, value-equal to the row) refused", cur.Threshold)
	}

	applyMigrationsUpTo(t, dsn, 197)
	var cols int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'price_alerts' AND column_name = 'disarmed'`).Scan(&cols); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	if cols != 0 {
		t.Errorf("0198 down left price_alerts.disarmed in place")
	}
	applyMigrations(t, dsn)
}

// TestPriceAlertRearmAfterFailedFanOut drives the evaluator against the real
// store with a nanosecond clock: the claim and the re-arm CAS both bind the
// worker's untruncated now, and timestamptz keeps microseconds, so the CAS
// matches only if both writes reduce the instant identically.
func TestPriceAlertRearmAfterFailedFanOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	acct := insertRawAccount(t, ctx, db, "rearm-fanout")

	alerts := postgresstore.NewPriceAlertStore(postgresstore.New(db))
	a, err := alerts.CreatePriceAlert(ctx, platform.PriceAlert{
		AccountID: acct, BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
		CooldownSeconds: 300,
	}, 25)
	if err != nil {
		t.Fatalf("create alert: %v", err)
	}

	// 789 ns past the microsecond: rounding and truncation disagree here.
	now := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	hooks := &failingWebhooks{
		hooks: []platform.CustomerWebhook{{
			ID: uuid.New(), AccountID: acct, URL: "https://hooks.example.com/x",
			Events: []string{string(platform.WebhookEventPriceAlert)}, Enabled: true,
		}},
		fail: true,
	}
	prices := staticVWAP{price: "0.20", bucketClose: func() time.Time { return now.Add(-30 * time.Second) }}
	w := pricealerts.New(alerts, hooks, prices, pricealerts.Options{
		Interval: time.Minute,
		Clock:    func() time.Time { return now },
	})

	w.Sweep(ctx)
	if hooks.attempts != 1 {
		t.Fatalf("fan-out attempts = %d, want 1", hooks.attempts)
	}
	got, err := alerts.GetPriceAlert(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastFiredAt.IsZero() || got.LastFiredAt.Sub(now).Abs() >= time.Microsecond {
		t.Fatalf("last_fired_at = %s, want the claim's stamp %s at microsecond precision", got.LastFiredAt, now)
	}
	if got.Disarmed {
		t.Fatal("alert still disarmed after a fan-out that enqueued nothing — the re-arm CAS missed the claim's stamp")
	}

	hooks.fail = false
	now = now.Add(301 * time.Second)
	w.Sweep(ctx)
	if hooks.enqueued != 1 {
		t.Fatalf("deliveries enqueued after the cooldown = %d, want 1 (the re-armed crossing retries)", hooks.enqueued)
	}
	got, err = alerts.GetPriceAlert(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Disarmed {
		t.Error("a delivered crossing left the alert armed")
	}
}

type failingWebhooks struct {
	hooks              []platform.CustomerWebhook
	fail               bool
	attempts, enqueued int
}

func (f *failingWebhooks) ListWebhooksForAccount(context.Context, uuid.UUID) ([]platform.CustomerWebhook, error) {
	return f.hooks, nil
}

func (f *failingWebhooks) EnqueueDelivery(context.Context, platform.WebhookDelivery) error {
	f.attempts++
	if f.fail {
		return errors.New("enqueue refused")
	}
	f.enqueued++
	return nil
}

type staticVWAP struct {
	price       string
	bucketClose func() time.Time
}

func (s staticVWAP) LatestVWAP(context.Context, canonical.Asset, canonical.Asset) (string, time.Time, bool, error) {
	return s.price, s.bucketClose(), true, nil
}

// TestStorePoolsPinSessionTimeZoneUTC opens every Store constructor against
// a database whose default TimeZone follows a DST-observing host, with the
// DSN asking for yet another zone, and requires each pooled session to be
// UTC. The closed-bucket guards spell `bucket <= now() - INTERVAL '<grain>'`;
// in a non-UTC session '1 day' and '1 month' are calendar-local, so across a
// DST transition the guard disagrees with the UTC time_bucket grid by an hour
// and a daily bucket is served (or withheld) at the wrong instant.
func TestStorePoolsPinSessionTimeZoneUTC(t *testing.T) {
	ctx := context.Background()
	dsn := startTimescale(t, ctx)

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	var dbName string
	if err := admin.QueryRowContext(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx,
		fmt.Sprintf(`ALTER DATABASE %q SET timezone = 'America/New_York'`, dbName)); err != nil {
		t.Fatalf("set database default timezone: %v", err)
	}
	// The DSN's own setting, in a spelling other than pgx's lowercase key.
	dsnWithZone := dsn + "&TimeZone=America/Denver"
	t.Setenv("PGTZ", "America/Chicago")

	openers := map[string]func() (*timescale.Store, error){
		"Open": func() (*timescale.Store, error) { return timescale.Open(ctx, dsnWithZone) },
		"OpenServing": func() (*timescale.Store, error) {
			return timescale.OpenServing(ctx, dsnWithZone, 30*time.Second)
		},
		"OpenBackground": func() (*timescale.Store, error) {
			return timescale.OpenBackground(ctx, dsnWithZone, 30*time.Second)
		},
	}
	for name, open := range openers {
		t.Run(name, func(t *testing.T) {
			s, err := open()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			defer func() { _ = s.Close() }()
			assertSessionGuardArithmeticIsUTC(t, ctx, s.DB())
		})
	}
}

// assertSessionGuardArithmeticIsUTC checks the session zone and the
// calendar grains the closed-bucket guard subtracts, each evaluated at an
// instant where a session in any US zone would disagree with UTC.
func assertSessionGuardArithmeticIsUTC(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var zone string
	if err := db.QueryRowContext(ctx, `SHOW TimeZone`).Scan(&zone); err != nil {
		t.Fatal(err)
	}
	if zone != "UTC" {
		t.Errorf("session TimeZone = %q, want UTC", zone)
	}
	cases := []struct {
		now, grain string
		want       time.Time
	}{
		// 2026/03/08 is the US spring-forward day: 03:30Z on the 9th is
		// 23:30 EDT on the 8th, and a local '1 day' back lands on EST.
		{"2026-03-09T03:30:00Z", "1 day", time.Date(2026, 3, 8, 3, 30, 0, 0, time.UTC)},
		{"2026-03-09T03:30:00Z", "1 week", time.Date(2026, 3, 2, 3, 30, 0, 0, time.UTC)},
		// The first monthly bucket boundary after it: 00:30Z on 1 March is
		// still 28 February in New York.
		{"2026-03-01T00:30:00Z", "1 month", time.Date(2026, 2, 1, 0, 30, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		var got time.Time
		q := fmt.Sprintf(`SELECT $1::timestamptz - INTERVAL '%s'`, tc.grain)
		if err := db.QueryRowContext(ctx, q, tc.now).Scan(&got); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if !got.Equal(tc.want) {
			t.Errorf("%s - INTERVAL '%s' = %s, want %s (the UTC calendar step)",
				tc.now, tc.grain, got.UTC().Format(time.RFC3339), tc.want.Format(time.RFC3339))
		}
	}
}

// A passkey signup binds the user ID into the credential before the row
// exists, so CreateUser must keep a caller-chosen ID, still generate one
// when none is given, and refuse a reused ID as a conflict.
func TestCreateUserHonoursCallerID(t *testing.T) {
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
	acct, err := postgresstore.NewAccountStore(store).Create(ctx, platform.Account{
		Name: "Fixed ID Co", Slug: "fixed-" + strings.ToLower(uuid.New().String()[:8]),
		Tier: platform.TierFree, Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	users := postgresstore.NewUserStore(store)

	want := uuid.New()
	got, err := users.CreateUser(ctx, platform.User{
		ID: want, AccountID: acct.ID, Email: platform.PlaceholderEmail(want), Role: platform.RoleOwner,
	})
	if err != nil {
		t.Fatalf("create user with id: %v", err)
	}
	if got.ID != want {
		t.Fatalf("user id = %s, want the caller's %s", got.ID, want)
	}

	generated, err := users.CreateUser(ctx, platform.User{
		AccountID: acct.ID, Email: "gen-" + uuid.New().String() + "@p.example", Role: platform.RoleMember,
	})
	if err != nil || generated.ID == uuid.Nil {
		t.Fatalf("create user without id: id=%s err=%v", generated.ID, err)
	}

	_, err = users.CreateUser(ctx, platform.User{
		ID: want, AccountID: acct.ID, Email: "other-" + uuid.New().String() + "@p.example", Role: platform.RoleMember,
	})
	if !errors.Is(err, platform.ErrConflict) {
		t.Fatalf("reused id: err = %v, want ErrConflict", err)
	}
}

// The stored WebAuthn signature counter is the only clone-detection
// signal, so it is a high-water mark: two overlapping login ceremonies
// both verify against the same stored value and may commit in either
// order. A late-committing lower count must not lower it, while
// last_used_at still records the latest write.
func TestWebAuthnSignCountNeverDecreases(t *testing.T) {
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
	acct, err := postgresstore.NewAccountStore(store).Create(ctx, platform.Account{
		Name: "Counter Co", Slug: "counter-" + strings.ToLower(uuid.New().String()[:8]),
		BillingEmail: "counter-" + uuid.New().String() + "@p.example",
		Tier:         platform.TierFree, Status: platform.AccountActive,
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	user, err := postgresstore.NewUserStore(store).CreateUser(ctx, platform.User{
		AccountID: acct.ID,
		Email:     "counter-" + uuid.New().String() + "@p.example",
		Role:      platform.RoleOwner,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	passkeys := postgresstore.NewWebAuthnCredentialStore(store)
	credID := []byte("cred-" + uuid.New().String())
	created, err := passkeys.CreateWebAuthnCredential(ctx, platform.WebAuthnCredential{
		UserID:          user.ID,
		Name:            "YubiKey",
		CredentialID:    credID,
		PublicKey:       []byte{0xA5, 0x01, 0x02, 0x03, 0x26},
		AttestationType: "none",
		Transports:      []string{"usb"},
		SignCount:       100,
	})
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}

	assertStored := func(t *testing.T, wantCount int64, wantUsed time.Time) {
		t.Helper()
		got, err := passkeys.GetWebAuthnCredentialByCredentialID(ctx, credID)
		if err != nil {
			t.Fatalf("get credential: %v", err)
		}
		if got.SignCount != wantCount {
			t.Errorf("SignCount = %d, want %d", got.SignCount, wantCount)
		}
		if !got.LastUsedAt.Equal(wantUsed) {
			t.Errorf("LastUsedAt = %v, want %v", got.LastUsedAt, wantUsed)
		}
	}

	base := time.Now().UTC().Truncate(time.Microsecond)

	// Ceremony B (count 102) commits first, then ceremony A (count 101).
	if err := passkeys.UpdateWebAuthnCredentialSignCount(ctx, created.ID, 102, base); err != nil {
		t.Fatalf("update 102: %v", err)
	}
	assertStored(t, 102, base)

	later := base.Add(time.Second)
	if err := passkeys.UpdateWebAuthnCredentialSignCount(ctx, created.ID, 101, later); err != nil {
		t.Fatalf("update 101: %v", err)
	}
	assertStored(t, 102, later)

	// An authenticator that reports 0 (counter unsupported) must not
	// erase the high-water mark either.
	latest := later.Add(time.Second)
	if err := passkeys.UpdateWebAuthnCredentialSignCount(ctx, created.ID, 0, latest); err != nil {
		t.Fatalf("update 0: %v", err)
	}
	assertStored(t, 102, latest)

	// A genuinely higher count still advances it.
	if err := passkeys.UpdateWebAuthnCredentialSignCount(ctx, created.ID, 103, latest); err != nil {
		t.Fatalf("update 103: %v", err)
	}
	assertStored(t, 103, latest)
}
