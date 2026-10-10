//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/accounterasure"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
	"github.com/Stellar-Index/StellarIndex/internal/sources/classicmovements"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// Invariant: no unused anonymous /v1/register account outlives
// accounterasure.AbandonedRegistrationRetention. Seeds the register shape
// plus one near-miss per predicate and runs the real sweep.
func TestAbandonedRegistrationSweep(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rdb, _ := startRedis(t, ctx)
	accounts := postgresstore.NewAccountStore(postgresstore.New(db))
	eraser := &accounterasure.Eraser{Store: accounts, Redis: rdb}

	old := time.Now().Add(-accounterasure.AbandonedRegistrationRetention - 24*time.Hour)
	seed := func(slug string, created time.Time, used, member, liveRecord, withKey bool, tier string) {
		t.Helper()
		var id string
		if err := db.QueryRowContext(ctx, `INSERT INTO accounts (name, slug, billing_email, tier, created_at)
			VALUES ($1, $1, 'victim@example.com', $2, $3) RETURNING id`, slug, tier, created).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
		if member {
			mustExec(t, ctx, db, `INSERT INTO users (account_id, email, role) VALUES ($1, $2, 'owner')`, id, slug+"@example.com")
		}
		if !withKey {
			return
		}
		hash := sha256.Sum256([]byte("sip_" + slug))
		mustExec(t, ctx, db, `INSERT INTO api_keys (id, account_id, name, key_hash, key_prefix, tier, rate_limit_per_min, last_used_at)
			VALUES ($1, $2, 'registration key', $3, 'sip_00000000', 'apikey', 60, $4)`,
			"kid_"+hex.EncodeToString(hash[:6]), id, hash[:], map[bool]any{true: time.Now(), false: nil}[used])
		if liveRecord {
			if err := rdb.Set(ctx, cachekeys.APIKey(hex.EncodeToString(hash[:])).String(), "{}", time.Hour).Err(); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed("abandoned", old, false, false, false, true, "free")
	seed("young", time.Now().Add(-24*time.Hour), false, false, false, true, "free")
	seed("used", old, true, false, false, true, "free")
	seed("member", old, false, true, false, true, "free")
	seed("live-record", old, false, false, true, true, "free")
	seed("no-key", old, false, false, false, false, "free")
	seed("promoted", old, false, false, false, true, "pro")

	cutoff := time.Now().Add(-accounterasure.AbandonedRegistrationRetention)
	n, err := eraser.SweepAbandonedRegistrations(ctx, accounts, cutoff)
	if err != nil || n != 1 {
		t.Fatalf("sweep: erased %d, err %v; want 1, nil", n, err)
	}
	remaining := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT slug FROM accounts`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		remaining[s] = true
	}
	_ = rows.Close()
	if remaining["abandoned"] {
		t.Error("abandoned registration survived the sweep")
	}
	for _, s := range []string{"young", "used", "member", "live-record", "no-key", "promoted"} {
		if !remaining[s] {
			t.Errorf("%s was erased; only an unused, expired, member-less free registration may be", s)
		}
	}
	var keys int
	abandonedHash := sha256.Sum256([]byte("sip_abandoned"))
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM api_keys WHERE key_hash = $1`, abandonedHash[:]).Scan(&keys); err != nil || keys != 0 {
		t.Errorf("abandoned account's api_keys rows = %d (err %v), want 0", keys, err)
	}
	if n, err := eraser.SweepAbandonedRegistrations(ctx, accounts, cutoff); err != nil || n != 0 {
		t.Errorf("second sweep: erased %d, err %v; want 0, nil", n, err)
	}

	// A member who joins between the plan and the erase must stop the erase.
	seed("raced", old, false, false, false, true, "free")
	var racedID uuid.UUID
	if err := db.QueryRowContext(ctx, `SELECT id FROM accounts WHERE slug = 'raced'`).Scan(&racedID); err != nil {
		t.Fatal(err)
	}
	plan, err := accounts.PlanErasure(ctx, racedID)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, db, `INSERT INTO users (account_id, email, role) VALUES ($1, 'raced@example.com', 'owner')`, racedID)
	_, err = accounts.EraseAccount(ctx, postgresstore.ErasureRequest{Plan: plan, ErasedSubject: "erased:x", Actor: platform.ActorSystem, RequireNoUsers: true})
	if !errors.Is(err, platform.ErrConflict) {
		t.Errorf("erase after a member joined: err %v, want ErrConflict", err)
	}
	var left int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM accounts WHERE id = $1 AND status = 'active'`, racedID).Scan(&left); err != nil || left != 1 {
		t.Errorf("raced account active rows = %d (err %v), want 1", left, err)
	}
}

// erasureSeed is one fully populated account. Every string a leak scan
// looks for is recorded in needles.
type erasureSeed struct {
	accountID, ownerID, memberID uuid.UUID
	slug                         string
	pgKeyID                      string
	redisKeyID, redisPlaintext   string
	needles                      []string
}

// seedErasureAccount writes an account with two members and one row in
// every platform table, plus a Redis self-service key and usage counters.
// tag keeps the two seeded accounts' strings disjoint.
func seedErasureAccount(
	t *testing.T, ctx context.Context, db *sql.DB, rdb *redis.Client, slug, domain, tag string,
) erasureSeed {
	t.Helper()
	s := erasureSeed{slug: slug}
	owner, member := slug+"@"+domain, "mary-"+tag+"@"+domain
	billing := "billing-" + tag + "@" + domain
	ownerIP, memberIP := "198.51.100."+tag, "192.0.2."+tag
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed %s: %v\n%s", slug, err, q)
		}
	}
	scan := func(dst any, q string, args ...any) {
		t.Helper()
		if err := db.QueryRowContext(ctx, q, args...).Scan(dst); err != nil {
			t.Fatalf("seed %s: %v\n%s", slug, err, q)
		}
	}
	scan(&s.accountID, `INSERT INTO accounts (name, slug, billing_email, suspended_reason)
		VALUES ($1, $2, $3, $4) RETURNING id`, "Name "+tag, slug, billing, "note about "+owner)
	scan(&s.ownerID, `INSERT INTO users (account_id, email, display_name, role)
		VALUES ($1, $2, $3, 'owner') RETURNING id`, s.accountID, owner, "Display "+tag)
	scan(&s.memberID, `INSERT INTO users (account_id, email, role) VALUES ($1, $2, 'member') RETURNING id`,
		s.accountID, member)
	for _, u := range []struct {
		id uuid.UUID
		ip string
	}{{s.ownerID, ownerIP}, {s.memberID, memberIP}} {
		exec(`INSERT INTO sessions (user_id, expires_at, ip_first_seen, ip_last_seen, user_agent, token_hash, geo_first_seen)
			VALUES ($1, now() + interval '1 day', $2, $2, $3, $4, 'DE')`,
			u.id, u.ip, "UA-"+tag+"-"+u.ip, []byte(uuid.NewString()))
	}
	exec(`INSERT INTO webauthn_credentials (user_id, name, credential_id, public_key, transports)
		VALUES ($1, $2, $3, $4, '{usb}')`, s.ownerID, "Laptop "+tag, []byte("cred-"+tag), []byte("pk-"+tag))
	s.pgKeyID = "kid_" + strings.Repeat(tag[:1], 12)
	exec(`INSERT INTO api_keys (id, account_id, created_by_user_id, name, key_hash, key_prefix, tier,
		rate_limit_per_min, last_used_ip, last_used_user_agent)
		VALUES ($1, $2, $3, $4, $5, $6, 'apikey', 60, $7, $8)`,
		s.pgKeyID, s.accountID, s.ownerID, "pgkey-"+tag, []byte("hash-"+tag+strings.Repeat("x", 20)),
		"rek_"+strings.Repeat(tag[:1], 8), ownerIP, "KeyUA-"+tag)
	var hookID uuid.UUID
	scan(&hookID, `INSERT INTO customer_webhooks (account_id, name, url, secret_hash, events)
		VALUES ($1, $2, $3, $4, '{price.alert}') RETURNING id`,
		s.accountID, "hook-"+tag, "https://hooks."+domain+"/in", []byte("secret-"+tag))
	exec(`INSERT INTO webhook_deliveries (webhook_id, event_type, payload)
		VALUES ($1, 'price.alert', '{"amount": 123456789012345678901234567890, "who": "`+tag+`"}')`, hookID)
	exec(`INSERT INTO price_alerts (account_id, base_asset, quote_asset, condition, threshold, cooldown_seconds)
		VALUES ($1, 'native', 'fiat:USD', 'above', 0.000000000000000001, 300)`, s.accountID)
	exec(`INSERT INTO invites (token_hash, account_id, email, role, invited_by_user_id, expires_at)
		VALUES ($1, $2, $3, 'member', $4, now() + interval '1 day')`,
		[]byte("inv-"+tag), s.accountID, "invitee-"+tag+"@"+domain, s.ownerID)
	exec(`INSERT INTO magic_link_tokens (token_hash, email, purpose, expires_at, requested_ip)
		VALUES ($1, $2, 'login', now() + interval '1 hour', $3)`, []byte("ml-"+tag), owner, ownerIP)
	exec(`INSERT INTO login_code_lockouts (email, failed_count) VALUES ($1, 3)`, member)
	exec(`INSERT INTO login_code_lockouts (email, failed_count) VALUES ($1, 2)`, platform.LoginCodeDeviceKey(member))
	exec(`INSERT INTO api_usage_events (ts, account_id, key_id, route, method, status, duration_ms, client_ip)
		VALUES (now(), $1, $2, '/v1/price', 'GET', 200, 5, $3)`, s.accountID, s.pgKeyID, ownerIP)
	exec(`INSERT INTO usage_daily (day, subject, endpoint, ok_count) VALUES
		(current_date, $1, '/v1/price', 7), (current_date, $2, '/v1/price', 2)`,
		"id:acct:"+slug, "key:"+s.pgKeyID)

	rec, plaintext, err := auth.NewRedisAPIKeyStore(rdb).Create(ctx, auth.CreateAPIKeyRequest{
		Identifier: auth.AccountIdentifier(slug), Label: "redis-label-" + tag,
	})
	if err != nil {
		t.Fatalf("seed redis key: %v", err)
	}
	s.redisKeyID, s.redisPlaintext = rec.KeyID, plaintext
	c := usage.New(rdb)
	_ = c.IncrementBy(ctx, "id:acct:"+slug, 5)
	_ = c.IncrementDetail(ctx, "id:acct:"+slug, "/v1/price", usage.ClassOK)

	audit := func(accountID, actorUser any, kind, action, targetKind, targetID string, meta map[string]any, ip, ua string) {
		t.Helper()
		raw, _ := json.Marshal(meta)
		exec(`INSERT INTO audit_log (account_id, actor_user_id, actor_kind, action, target_kind, target_id, metadata, ip, user_agent)
			VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), $7::jsonb, $8, $9)`,
			accountID, actorUser, kind, action, targetKind, targetID, string(raw), ip, ua)
	}
	audit(s.accountID, s.ownerID, "user", "key.mint", "api_key", s.pgKeyID,
		map[string]any{"session_id": uuid.NewString(), "name": "pgkey-" + tag}, ownerIP, "UA-"+tag+"-"+ownerIP)
	audit(s.accountID, s.memberID, "user", "passkey.register", "webauthn_credential", uuid.NewString(),
		map[string]any{"name": "Laptop " + tag}, memberIP, "UA-"+tag+"-"+memberIP)
	audit(s.accountID, nil, "staff", "admin.account.read", "account", s.accountID.String(),
		map[string]any{"actor_key_id": "kid_staff0000000", "actor_identifier": "acct:staffco", "account_slug": slug},
		"203.0.113.99", "StaffUA")
	audit(s.accountID, nil, "staff", "account.override.set", "account", s.accountID.String(),
		map[string]any{
			"reason": "ticket 7", "before": map[string]any{"suspended_reason": "note about " + owner, "tier": "free"},
			"after": map[string]any{"suspended_reason": "", "tier": "free"},
		}, "203.0.113.99", "StaffUA")
	audit(nil, nil, "staff", "key.mint", "api_key", s.redisKeyID,
		map[string]any{
			"actor_identifier": "acct:staffco", "target_identifier": auth.AccountIdentifier(slug),
			"label": "redis-label-" + tag,
		}, "203.0.113.99", "StaffUA")

	s.needles = []string{
		s.accountID.String(), s.ownerID.String(), s.memberID.String(),
		owner, member, billing, "invitee-" + tag + "@" + domain, "hooks." + domain,
		"acct:" + slug, "Name " + tag, "Display " + tag, "Laptop " + tag, "pgkey-" + tag,
		"redis-label-" + tag, "UA-" + tag + "-", "KeyUA-" + tag, ownerIP, memberIP,
		s.pgKeyID, s.redisKeyID, "rek_" + strings.Repeat(tag[:1], 8),
	}
	return s
}

// leakScan counts, per table in public, the rows whose text form contains
// needle. audit_log.target_id is excluded: it holds the erased account's
// uuid on the account.erase row and key / passkey ids, which name nothing
// once the rows they pointed at are gone.
func leakScan(t *testing.T, ctx context.Context, db *sql.DB, needles []string) []string {
	t.Helper()
	tables := publicTables(t, ctx, db)
	if len(tables) < 50 {
		t.Fatalf("leak scan found only %d tables — the instrument is not looking at the schema", len(tables))
	}
	var hits []string
	for _, tbl := range tables {
		expr := "to_jsonb(x)"
		if tbl == "audit_log" {
			expr = "to_jsonb(x) - 'target_id'"
		}
		for _, n := range needles {
			var c int
			q := fmt.Sprintf(`SELECT count(*) FROM %q x WHERE (%s)::text ILIKE '%%' || $1 || '%%'`, tbl, expr)
			if err := db.QueryRowContext(ctx, q, n).Scan(&c); err != nil {
				t.Fatalf("scan %s: %v", tbl, err)
			}
			if c > 0 {
				hits = append(hits, fmt.Sprintf("%s: %d row(s) contain %q", tbl, c, n))
			}
		}
	}
	return hits
}

func publicTables(t *testing.T, ctx context.Context, db *sql.DB) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE' ORDER BY 1`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		tables = append(tables, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	return tables
}

// redisLeaks lists every Redis key whose name or value contains a needle.
func redisLeaks(t *testing.T, ctx context.Context, rdb *redis.Client, needles []string) []string {
	t.Helper()
	var hits []string
	iter := rdb.Scan(ctx, 0, "*", 1000).Iterator()
	for iter.Next(ctx) {
		k := iter.Val()
		text := k
		switch rdb.Type(ctx, k).Val() {
		case "string":
			text += " " + rdb.Get(ctx, k).Val()
		case "hash":
			for f, v := range rdb.HGetAll(ctx, k).Val() {
				text += " " + f + "=" + v
			}
		}
		for _, n := range needles {
			if strings.Contains(text, n) || strings.Contains(text, url.QueryEscape(n)) {
				hits = append(hits, fmt.Sprintf("redis %s contains %q", k, n))
			}
		}
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("redis scan: %v", err)
	}
	return hits
}

func tableSnapshot(t *testing.T, ctx context.Context, db *sql.DB, accountID uuid.UUID) string {
	t.Helper()
	var s string
	err := db.QueryRowContext(ctx, `SELECT concat_ws('|',
		(SELECT jsonb_agg(to_jsonb(a) ORDER BY id)::text FROM accounts a WHERE id = $1),
		(SELECT jsonb_agg(to_jsonb(u) ORDER BY id)::text FROM users u WHERE account_id = $1),
		(SELECT jsonb_agg(to_jsonb(k) ORDER BY id)::text FROM api_keys k WHERE account_id = $1),
		(SELECT jsonb_agg(to_jsonb(l) ORDER BY id)::text FROM audit_log l WHERE account_id = $1),
		(SELECT jsonb_agg(to_jsonb(d) ORDER BY subject)::text FROM usage_daily d
		  WHERE subject LIKE 'id:acct:' || (SELECT slug FROM accounts WHERE id = $1)))`, accountID).Scan(&s)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return s
}

// TestAccountErasure pins account erasure end to end against real Postgres and
// Redis: a whole-database and whole-Redis leak scan after the erasure, a
// control account left byte-identical, staff audit rows kept, a retry
// after a mid-transaction failure, and a different person whose address
// shares the erased local part not inheriting the slug.
func TestAccountErasure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rdb, _ := startRedis(t, ctx)

	x := seedErasureAccount(t, ctx, db, rdb, "john", "b.example", "7")
	y := seedErasureAccount(t, ctx, db, rdb, "yves", "d.example", "9")
	// An invite from the control account TO the erased owner is the
	// erased person's data, held by someone else.
	if _, err := db.ExecContext(ctx, `INSERT INTO invites (token_hash, account_id, email, role, invited_by_user_id, expires_at)
		VALUES ('inv-cross', $1, 'john@b.example', 'member', $2, now() + interval '1 day')`, y.accountID, y.ownerID); err != nil {
		t.Fatalf("cross invite: %v", err)
	}
	controlBefore := tableSnapshot(t, ctx, db, y.accountID)

	accounts := postgresstore.NewAccountStore(postgresstore.New(db))
	eraser := &accounterasure.Eraser{Store: accounts, Redis: rdb}

	// The instrument: before the erasure the scan must see the account.
	if hits := leakScan(t, ctx, db, x.needles); len(hits) < 10 {
		t.Fatalf("pre-erasure leak scan found only %d hits — it cannot see the seeded account:\n%s",
			len(hits), strings.Join(hits, "\n"))
	}

	t.Run("FailureMidTransactionChangesNothing", func(t *testing.T) {
		before := tableSnapshot(t, ctx, db, x.accountID)
		mustExec(t, ctx, db, `CREATE FUNCTION erasure_boom() RETURNS trigger LANGUAGE plpgsql AS
			$$ BEGIN RAISE EXCEPTION 'injected'; END $$`)
		mustExec(t, ctx, db, `CREATE TRIGGER erasure_boom BEFORE DELETE ON price_alerts
			FOR EACH ROW EXECUTE FUNCTION erasure_boom()`)
		if _, err := eraser.Erase(ctx, x.accountID, platform.ActorUser); err == nil {
			t.Fatal("Erase succeeded through an injected failure")
		}
		mustExec(t, ctx, db, `DROP TRIGGER erasure_boom ON price_alerts`)
		mustExec(t, ctx, db, `DROP FUNCTION erasure_boom()`)
		if after := tableSnapshot(t, ctx, db, x.accountID); after != before {
			t.Errorf("a failed erasure changed state (the owner would be locked out mid-erasure):\nbefore %s\nafter  %s", before, after)
		}
		if _, err := auth.NewRedisAPIKeyValidator(rdb).Lookup(ctx, x.redisPlaintext); err != nil {
			t.Errorf("a failed erasure already removed Redis keys: %v", err)
		}
	})

	var rep accounterasure.Report
	t.Run("Erase", func(t *testing.T) {
		rep, err = eraser.Erase(ctx, x.accountID, platform.ActorUser)
		if err != nil {
			t.Fatalf("Erase: %v", err)
		}
		if rep.Counts.Users != 2 || rep.Counts.APIKeys != 1 || rep.Counts.Invites != 2 || rep.Counts.AuditRowsScrubbed != 5 {
			t.Errorf("counts = %+v, want 2 users, 1 key, 2 invites, 5 audit rows", rep.Counts)
		}
	})

	t.Run("NoTraceInPostgres", func(t *testing.T) {
		if hits := leakScan(t, ctx, db, x.needles); len(hits) > 0 {
			t.Errorf("erased account still present:\n%s", strings.Join(hits, "\n"))
		}
	})

	t.Run("NoTraceInRedis", func(t *testing.T) {
		if hits := redisLeaks(t, ctx, rdb, []string{"acct:john", x.redisKeyID}); len(hits) > 0 {
			t.Errorf("erased account still in Redis:\n%s", strings.Join(hits, "\n"))
		}
		v := auth.NewRedisAPIKeyValidator(rdb, auth.WithAccountStatus(accounts))
		if _, err := v.Lookup(ctx, x.redisPlaintext); err == nil {
			t.Error("the erased account's Redis key still authenticates")
		}
		if _, err := v.Lookup(ctx, y.redisPlaintext); err != nil {
			t.Errorf("the control account's key stopped authenticating: %v", err)
		}
	})

	t.Run("UsageKeptUnderOneUnlinkedSubject", func(t *testing.T) {
		var subjects, ok int64
		if err := db.QueryRowContext(ctx, `SELECT count(DISTINCT subject), COALESCE(sum(ok_count), 0)
			FROM usage_daily WHERE subject LIKE 'erased:%'`).Scan(&subjects, &ok); err != nil {
			t.Fatal(err)
		}
		if subjects != 1 || ok != 9 {
			t.Errorf("erased usage = %d subject(s), %d ok; want 1 and 9 (7 account + 2 key)", subjects, ok)
		}
		// A rollup sweep that read the counters before they were deleted
		// upserts the old subject after the commit; the re-run folds it in.
		mustExec(t, ctx, db, `INSERT INTO usage_daily (day, subject, endpoint, ok_count)
			VALUES (current_date, 'id:acct:john', '/v1/price', 8)`)
		if _, err := accounts.RenameUsageSubjects(ctx, postgresstore.UsageSubjects("john"), rep.ErasedSubject); err != nil {
			t.Fatalf("rename: %v", err)
		}
		if err := db.QueryRowContext(ctx, `SELECT count(DISTINCT subject), COALESCE(sum(ok_count), 0)
			FROM usage_daily WHERE subject LIKE 'erased:%' OR subject LIKE '%john%'`).Scan(&subjects, &ok); err != nil {
			t.Fatal(err)
		}
		if subjects != 1 || ok != 9 {
			t.Errorf("after the late upsert: %d subject(s), %d ok; want 1 subject holding max(9, 8) = 9", subjects, ok)
		}
	})

	t.Run("StaffRowsKeepStaffIdentity", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, `SELECT action, host(ip), user_agent, metadata::text FROM audit_log
			WHERE actor_kind = 'staff' AND account_id IS NULL AND (action <> 'key.mint' OR target_id = $1)`, x.redisKeyID)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		n := 0
		for rows.Next() {
			var action, ip, ua, meta string
			if err := rows.Scan(&action, &ip, &ua, &meta); err != nil {
				t.Fatal(err)
			}
			n++
			if ip != "203.0.113.99" || ua != "StaffUA" {
				t.Errorf("%s: staff ip/ua = %s/%s, want kept", action, ip, ua)
			}
			if strings.Contains(meta, "account_slug") || strings.Contains(meta, "target_identifier") {
				t.Errorf("%s: subject keys survived in %s", action, meta)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if n != 3 {
			t.Errorf("found %d staff rows, want the erased account's 3 kept", n)
		}
		var erase int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM audit_log WHERE action = 'account.erase'
			AND target_id = $1 AND account_id IS NULL AND ip IS NULL`, x.accountID.String()).Scan(&erase); err != nil {
			t.Fatal(err)
		}
		if erase != 1 {
			t.Errorf("account.erase rows = %d, want 1", erase)
		}
	})

	t.Run("ControlAccountUntouched", func(t *testing.T) {
		if after := tableSnapshot(t, ctx, db, y.accountID); after != controlBefore {
			t.Errorf("control account changed:\nbefore %s\nafter  %s", controlBefore, after)
		}
	})

	t.Run("RetryIsANoOp", func(t *testing.T) {
		again, err := eraser.Erase(ctx, x.accountID, platform.ActorUser)
		if err != nil || !again.AlreadyErased {
			t.Fatalf("retry = %+v, %v; want AlreadyErased", again, err)
		}
		fin, err := eraser.FinishBySlug(ctx, "john")
		if err != nil {
			t.Fatalf("FinishBySlug: %v", err)
		}
		if fin.RedisKeys != 0 || fin.LateUsageRows != 0 {
			t.Errorf("FinishBySlug found leftovers after a clean erasure: %+v", fin)
		}
		if _, err := eraser.FinishBySlug(ctx, "yves"); err == nil {
			t.Error("FinishBySlug accepted a live account's slug")
		}
	})

	// A different person whose address shares the local part must not get
	// the slug: john@e.example would otherwise be handed acct:john.
	t.Run("SameLocalPartDifferentPersonGetsANewSlug", func(t *testing.T) {
		_, err := accounts.Create(ctx, platform.Account{
			Name: "john@e.example", Slug: "john", BillingEmail: "john@e.example",
			Tier: platform.TierFree, Status: platform.AccountActive,
		})
		if !errors.Is(err, platform.ErrConflict) {
			t.Fatalf("Create(slug=john) = %v, want ErrConflict from the tombstone", err)
		}
		fresh, err := accounts.Create(ctx, platform.Account{
			Name: "john@e.example", Slug: "john-1a2b", BillingEmail: "john@e.example",
			Tier: platform.TierFree, Status: platform.AccountActive,
		})
		if err != nil {
			t.Fatalf("Create(suffixed): %v", err)
		}
		recs, err := auth.NewRedisAPIKeyStore(rdb).ListKeysForIdentifier(ctx, auth.AccountIdentifier(fresh.Slug))
		if err != nil || len(recs) != 0 {
			t.Errorf("new account lists %d keys (%v), want 0", len(recs), err)
		}
		if n, _ := usage.New(rdb).MonthToDate(ctx, "id:acct:"+fresh.Slug); n != 0 {
			t.Errorf("new account inherits %d usage units", n)
		}
	})

	t.Run("ExportSeesOnlyTheRequestersPersonalData", func(t *testing.T) {
		mustExec(t, ctx, db, `INSERT INTO usage_daily (day, subject, endpoint, ok_count)
			VALUES (current_date, $1, '/v1/price', 4)`, "key:"+y.redisKeyID)
		exp, err := (&accounterasure.Exporter{Store: accounts, Redis: rdb}).Export(ctx, y.accountID, y.ownerID, time.Now())
		if err != nil {
			t.Fatalf("ExportAccount: %v", err)
		}
		raw, err := json.Marshal(exp)
		if err != nil {
			t.Fatal(err)
		}
		doc := string(raw)
		for _, secret := range []string{"key_hash", "secret_hash", "token_hash", "public_key", "mfa_secret", "recovery"} {
			if strings.Contains(doc, secret) {
				t.Errorf("export carries %q", secret)
			}
		}
		if strings.Contains(doc, "192.0.2.9") || strings.Contains(doc, "UA-9-192.0.2.9") {
			t.Error("export carries another member's address or agent")
		}
		if len(exp.Sessions) != 1 || exp.Sessions[0].IPFirstSeen != "198.51.100.9" {
			t.Errorf("sessions = %+v, want only the requester's one", exp.Sessions)
		}
		if len(exp.PriceAlerts) != 1 || exp.PriceAlerts[0].Threshold != "0.000000000000000001" {
			t.Errorf("price alert threshold = %+v, want the exact decimal string", exp.PriceAlerts)
		}
		if len(exp.Webhooks) != 1 || len(exp.Webhooks[0].Deliveries) != 1 ||
			!strings.Contains(string(exp.Webhooks[0].Deliveries[0].Payload), "123456789012345678901234567890") {
			t.Errorf("delivery payload lost precision: %+v", exp.Webhooks)
		}
		for _, a := range exp.AuditLog {
			if a.ActorKind == "staff" && (a.IP != "" || strings.Contains(string(a.Metadata), "actor_email")) {
				t.Errorf("staff row exported with staff identity: %+v", a)
			}
			if !a.ByRequester && a.IP != "" {
				t.Errorf("another actor's address exported: %+v", a)
			}
		}
		if len(exp.Usage) != 3 || len(exp.Users) != 2 || len(exp.APIKeys) != 2 {
			t.Errorf("usage %d, users %d, keys %d; want 3, 2, 2", len(exp.Usage), len(exp.Users), len(exp.APIKeys))
		}
		redisUsage := false
		for _, u := range exp.Usage {
			redisUsage = redisUsage || (u.Subject == "key:"+y.redisKeyID && u.OKCount == 4)
		}
		if !redisUsage {
			t.Errorf("usage = %+v, want the Redis key's key:%s row", exp.Usage, y.redisKeyID)
		}
	})
}

// TestAccountErasureBoundaries pins what an erasure must leave alone.
func TestAccountErasureBoundaries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rdb, _ := startRedis(t, ctx)
	accounts := postgresstore.NewAccountStore(postgresstore.New(db))
	eraser := &accounterasure.Eraser{Store: accounts, Redis: rdb}

	// /v1/register records the billing address unverified: it can be a
	// stranger's, whose login state and invites the erasure must keep.
	t.Run("BillingAddressOwnerKeepsLoginState", func(t *testing.T) {
		x := seedErasureAccount(t, ctx, db, rdb, "john", "b.example", "7")
		stranger := "billing-7@b.example"
		var otherID, otherOwner uuid.UUID
		if err := db.QueryRowContext(ctx, `INSERT INTO accounts (name, slug, billing_email)
			VALUES ('Other', 'other', 'other@c.example') RETURNING id`).Scan(&otherID); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, `INSERT INTO users (account_id, email, role)
			VALUES ($1, $2, 'owner') RETURNING id`, otherID, strings.ToUpper(stranger)).Scan(&otherOwner); err != nil {
			t.Fatal(err)
		}
		mustExec(t, ctx, db, `INSERT INTO login_code_lockouts (email, failed_count) VALUES ($1, 4)`, stranger)
		mustExec(t, ctx, db, `INSERT INTO login_code_lockouts (email, failed_count) VALUES ($1, 4)`, platform.LoginCodeDeviceKey(stranger))
		mustExec(t, ctx, db, `INSERT INTO magic_link_tokens (token_hash, email, purpose, expires_at, requested_ip)
			VALUES ('ml-stranger', $1, 'login', now() + interval '1 hour', '203.0.113.5')`, stranger)
		mustExec(t, ctx, db, `INSERT INTO invites (token_hash, account_id, email, role, invited_by_user_id, expires_at)
			VALUES ('inv-stranger', $1, $2, 'member', $3, now() + interval '1 day')`, otherID, stranger, otherOwner)

		if _, err := eraser.Erase(ctx, x.accountID, platform.ActorUser); err != nil {
			t.Fatalf("Erase: %v", err)
		}
		var lockouts, links, invites, memberLockouts int
		if err := db.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM login_code_lockouts WHERE email IN ($1, $2) AND failed_count = 4),
			(SELECT count(*) FROM magic_link_tokens WHERE email = $1),
			(SELECT count(*) FROM invites WHERE email = $1),
			(SELECT count(*) FROM login_code_lockouts WHERE email IN ('mary-7@b.example', 'mary-7@b.example device'))`,
			stranger, platform.LoginCodeDeviceKey(stranger)).
			Scan(&lockouts, &links, &invites, &memberLockouts); err != nil {
			t.Fatal(err)
		}
		if lockouts != 2 || links != 1 || invites != 1 {
			t.Errorf("stranger's lockout/magic link/invite = %d/%d/%d, want 2/1/1 kept", lockouts, links, invites)
		}
		if memberLockouts != 0 {
			t.Errorf("the erased member's lockout survived (%d)", memberLockouts)
		}
	})

	// Create and EraseAccount serialise on one per-slug advisory lock, so a
	// Create racing an erasure of its slug sees the tombstone. holdSlugLock
	// takes that lock on its own connection, standing in for the other side.
	holdSlugLock := func(t *testing.T, slug string) *sql.Tx {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('slug:' || $1::text, 0))`, slug); err != nil {
			t.Fatal(err)
		}
		return tx
	}
	blocked := func(done <-chan error) bool {
		select {
		case <-done:
			return false
		case <-time.After(500 * time.Millisecond):
			return true
		}
	}

	t.Run("CreateRacingAnErasureSeesTheTombstone", func(t *testing.T) {
		eraseTx := holdSlugLock(t, "racer")
		done := make(chan error, 1)
		go func() {
			_, err := accounts.Create(ctx, platform.Account{
				Name: "racer", Slug: "racer", BillingEmail: "racer@f.example",
				Tier: platform.TierFree, Status: platform.AccountActive,
			})
			done <- err
		}()
		if !blocked(done) {
			_ = eraseTx.Rollback()
			t.Fatal("Create did not wait for the in-flight erasure's slug lock")
		}
		if _, err := eraseTx.ExecContext(ctx, `INSERT INTO erased_account_slugs (slug_sha256)
			VALUES (sha256(convert_to('racer', 'UTF8')))`); err != nil {
			t.Fatal(err)
		}
		if err := eraseTx.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, platform.ErrConflict) {
			t.Errorf("Create after the erasure committed = %v, want ErrConflict from the tombstone", err)
		}
	})

	t.Run("EraseWaitsForAnInFlightCreate", func(t *testing.T) {
		a, err := accounts.Create(ctx, platform.Account{
			Name: "waiter", Slug: "waiter", BillingEmail: "waiter@f.example",
			Tier: platform.TierFree, Status: platform.AccountActive,
		})
		if err != nil {
			t.Fatal(err)
		}
		createTx := holdSlugLock(t, "waiter")
		done := make(chan error, 1)
		go func() {
			_, err := eraser.Erase(ctx, a.ID, platform.ActorUser)
			done <- err
		}()
		if !blocked(done) {
			_ = createTx.Rollback()
			t.Fatal("EraseAccount did not take the slug lock")
		}
		if err := createTx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Errorf("Erase after the lock released: %v", err)
		}
	})
}

// TestAccountMovements_AssetFilterScopesPostgresTail executes the
// contract-scoped sep41_transfers query against real Postgres and the
// full /movements stack over HTTP. The account's newest transfers are all
// another token; its USDC transfers sit below them. ?asset=USDC must fill
// the page from USDC rows and page through all of them — the filter
// applied after the LIMIT served an empty page with no cursor.
func TestAccountMovements_AssetFilterScopesPostgresTail(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	g := gAccountFromSeed(t, 0x51)
	issuer := gAccountFromSeed(t, 0x52)
	counterparty := gAccountFromSeed(t, 0x53)
	usdc, err := canonical.NewClassicAsset("USDC", issuer)
	if err != nil {
		t.Fatal(err)
	}
	eurc, err := canonical.NewClassicAsset("EURC", issuer)
	if err != nil {
		t.Fatal(err)
	}
	usdcSAC, err := usdc.SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	otherContract, err := eurc.SacContractID()
	if err != nil {
		t.Fatal(err)
	}

	base := classicmovements.P23StartLedger + 1000
	var rows []timescale.SEP41TransferRow
	add := func(ledger uint32, contract, from, to string) {
		rows = append(rows, timescale.SEP41TransferRow{
			ObservedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC).Add(time.Duration(ledger-base) * time.Second),
			Ledger:     ledger,
			TxHash:     fmt.Sprintf("%064x", ledger),
			ContractID: contract,
			Kind:       timescale.SEP41Transfer,
			FromAddr:   from,
			ToAddr:     to,
			Amount:     big.NewInt(int64(ledger - base + 1)),
		})
	}
	for l := base + 100; l < base+105; l++ { // newest: another token
		add(l, otherContract, g, counterparty)
	}
	for l := base + 10; l < base+14; l++ { // older: USDC, both directions
		if (l-base)%2 == 0 {
			add(l, usdcSAC, g, counterparty)
		} else {
			add(l, usdcSAC, counterparty, g)
		}
	}
	if err := store.InsertSEP41TransferBatch(ctx, rows); err != nil {
		t.Fatalf("InsertSEP41TransferBatch: %v", err)
	}

	t.Run("store", func(t *testing.T) {
		page, err := store.ListSEP41TransfersByAddress(ctx, g, 3, timescale.SEP41TransferCursor{}, "", usdcSAC, 0)
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		assertTransferLedgers(t, "page 1", page, base+13, base+12, base+11)
		last := page[len(page)-1]
		cur := timescale.SEP41TransferCursor{Ledger: last.Ledger, TxHash: last.TxHash, OpIndex: last.OpIndex, EventIndex: last.EventIndex}
		page, err = store.ListSEP41TransfersByAddress(ctx, g, 3, cur, "", usdcSAC, 0)
		if err != nil {
			t.Fatalf("page 2: %v", err)
		}
		assertTransferLedgers(t, "page 2", page, base+10)
		sent, err := store.ListSEP41TransfersByAddress(ctx, g, 3, timescale.SEP41TransferCursor{}, "sent", usdcSAC, 0)
		if err != nil {
			t.Fatalf("sent: %v", err)
		}
		assertTransferLedgers(t, "sent", sent, base+12, base+10)
	})

	t.Run("http", func(t *testing.T) {
		chAddr := clickhouseAddr(t)
		if err := chstore.EnsureAccountMovementsTable(ctx, chAddr); err != nil {
			t.Fatalf("EnsureAccountMovementsTable: %v", err)
		}
		er, err := chstore.NewExplorerReader(ctx, chAddr)
		if err != nil {
			t.Fatalf("NewExplorerReader: %v", err)
		}
		t.Cleanup(func() { _ = er.Close() })
		ts := httptest.NewServer(v1.New(v1.Options{Explorer: er, SEP41Movements: store}).Handler())
		t.Cleanup(ts.Close)

		q := url.Values{"asset": {usdc.String()}, "limit": {"3"}}
		page1 := getMovements(t, ts.URL, g, q)
		assertMovementLedgers(t, "page 1", page1, usdc.String(), base+13, base+12, base+11)
		if page1.NextCursor == "" {
			t.Fatal("page 1 is full but next_cursor is empty — the older USDC transfer is unreachable")
		}
		q.Set("cursor", page1.NextCursor)
		page2 := getMovements(t, ts.URL, g, q)
		assertMovementLedgers(t, "page 2", page2, usdc.String(), base+10)
	})
}

func getMovements(t *testing.T, base, g string, q url.Values) v1.AccountMovementsView {
	t.Helper()
	resp, err := http.Get(base + "/v1/accounts/" + g + "/movements?" + q.Encode())
	if err != nil {
		t.Fatalf("GET movements: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.AccountMovementsView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Data
}

func assertTransferLedgers(t *testing.T, label string, got []timescale.SEP41TransferRow, want ...uint32) {
	t.Helper()
	ledgers := make([]uint32, len(got))
	for i, r := range got {
		ledgers[i] = r.Ledger
	}
	if fmt.Sprint(ledgers) != fmt.Sprint(want) {
		t.Fatalf("%s ledgers = %v, want %v", label, ledgers, want)
	}
}

func assertMovementLedgers(t *testing.T, label string, v v1.AccountMovementsView, asset string, want ...uint32) {
	t.Helper()
	ledgers := make([]uint32, len(v.Movements))
	for i, m := range v.Movements {
		ledgers[i] = m.Ledger
		if m.Asset != asset {
			t.Errorf("%s ledger %d asset = %q, want %q", label, m.Ledger, m.Asset, asset)
		}
	}
	if fmt.Sprint(ledgers) != fmt.Sprint(want) {
		t.Fatalf("%s ledgers = %v, want %v", label, ledgers, want)
	}
}

// TestAccountMovements_LedgerCeilingBoundsTheQuery executes the fixed
// SQL against a real ClickHouse: the /movements merge ceiling travels in
// AccountMovementFilter.MaxLedger/HasMaxLedger and is applied as a WHERE
// predicate, so a bounded read returns a FULL page of servable rows.
//
// The un-fixed reader emitted no ledger bound at all and the handler
// dropped the over-ceiling rows afterwards: with every one of the `limit`
// newest rows above the ceiling the page collapsed to zero, next_cursor
// was suppressed, and the account's pre-watermark history was unreachable.
// Here the same shape must yield `limit` rows, the newest of them exactly
// at the ceiling.
func TestAccountMovements_LedgerCeilingBoundsTheQuery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	chAddr := clickhouseAddr(t)
	if err := chstore.EnsureAccountMovementsTable(ctx, chAddr); err != nil {
		t.Fatalf("EnsureAccountMovementsTable: %v", err)
	}

	g := gAccountFromSeed(t, 0x41)
	counterparty := gAccountFromSeed(t, 0x42)
	const (
		firstLedger = 40_000_000
		rowCount    = 40
		ceiling     = firstLedger + 19 // 20 servable ledgers, 20 above the ceiling
		limit       = 10
	)

	movements := make([]chstore.AccountMovement, 0, rowCount)
	for i := 0; i < rowCount; i++ {
		movements = append(movements, chstore.AccountMovement{
			MovementKind:    "payment",
			Provenance:      "classic_derived",
			Ledger:          uint32(firstLedger + i),
			LedgerCloseTime: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute),
			TxHash:          fmt.Sprintf("%064x", i),
			OpIndex:         0,
			LegIndex:        0,
			Asset:           "native",
			Amount:          big.NewInt(int64(1_000_000 + i)),
			FromAddress:     g,
			ToAddress:       counterparty,
		})
	}
	if _, err := chstore.InsertAccountMovements(ctx, chAddr, movements); err != nil {
		t.Fatalf("InsertAccountMovements: %v", err)
	}

	er, err := chstore.NewExplorerReader(ctx, chAddr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	rows, err := er.AccountMovements(ctx, g, limit, chstore.AccountMovementCursor{},
		chstore.AccountMovementFilter{HasMaxLedger: true, MaxLedger: ceiling})
	if err != nil {
		t.Fatalf("AccountMovements (bounded): %v", err)
	}
	if len(rows) != limit {
		t.Fatalf("bounded read returned %d rows, want a full page of %d — the ceiling did not bound the "+
			"query, so the newest (unservable) rows ate the LIMIT (F055)", len(rows), limit)
	}
	if rows[0].Ledger != ceiling {
		t.Errorf("newest bounded row is ledger %d, want %d (the ceiling itself is inclusive)", rows[0].Ledger, ceiling)
	}
	for _, r := range rows {
		if r.Ledger > ceiling {
			t.Fatalf("row at ledger %d exceeds the ceiling %d", r.Ledger, ceiling)
		}
	}

	// A ceiling of 0 is a real ceiling (an installed genesis movements
	// floor with no cap67 watermark), not "unset": it must serve nothing.
	zeroRows, err := er.AccountMovements(ctx, g, limit, chstore.AccountMovementCursor{},
		chstore.AccountMovementFilter{HasMaxLedger: true, MaxLedger: 0})
	if err != nil {
		t.Fatalf("AccountMovements (ceiling 0): %v", err)
	}
	if len(zeroRows) != 0 {
		t.Fatalf("ceiling 0 served %d rows, want 0 — the arm must fail closed at an installed genesis floor", len(zeroRows))
	}

	// No ceiling = the whole archive.
	allRows, err := er.AccountMovements(ctx, g, rowCount, chstore.AccountMovementCursor{},
		chstore.AccountMovementFilter{})
	if err != nil {
		t.Fatalf("AccountMovements (unbounded): %v", err)
	}
	if len(allRows) != rowCount {
		t.Fatalf("unbounded read returned %d rows, want %d", len(allRows), rowCount)
	}
}

// TestAccountMovements_MergesCHArchiveAndPGTail is the ADR-0048 D5
// end-to-end proof: real ClickHouse rows in stellar.account_movements
// (pre-P23 archive) + real Postgres rows in sep41_transfers (post-P23
// tail), read through the actual production stack
// (chstore.ExplorerReader + timescale.Store, wired into v1.New
// exactly like cmd/stellarindex-api/main.go does), come back as ONE
// merged, correctly-ordered, correctly-paginated feed over real HTTP —
// catching any regression a stubbed unit test (explorer_movements_test.go)
// can't: real SQL WHERE-clause correctness on both sides, real
// ClickHouse tuple-comparison pagination, real Postgres index usage.
func TestAccountMovements_MergesCHArchiveAndPGTail(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// ─── Postgres side (sep41_transfers "recent tail") ──────────────
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	g := gAccountFromSeed(t, 0x21)
	counterparty := gAccountFromSeed(t, 0x22)
	postP23Ledger := classicmovements.P23StartLedger + 1000

	if err := store.InsertSEP41TransferBatch(ctx, []timescale.SEP41TransferRow{
		{
			ObservedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
			Ledger:     postP23Ledger,
			TxHash:     "pgtxaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			OpIndex:    0,
			EventIndex: 0,
			ContractID: "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCTAIL",
			Kind:       timescale.SEP41Transfer,
			FromAddr:   g,
			ToAddr:     counterparty,
			Amount:     big.NewInt(9_000_000),
		},
	}); err != nil {
		t.Fatalf("InsertSEP41TransferBatch: %v", err)
	}

	// ─── ClickHouse side (pre-P23 account_movements archive) ────────
	chAddr := clickhouseAddr(t)
	if err := chstore.EnsureAccountMovementsTable(ctx, chAddr); err != nil {
		t.Fatalf("EnsureAccountMovementsTable: %v", err)
	}
	prePayment := chstore.AccountMovement{
		MovementKind:    "payment",
		Provenance:      "classic_derived",
		Ledger:          classicmovements.P23StartLedger - 1000,
		LedgerCloseTime: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		TxHash:          "chtxbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		OpIndex:         0,
		LegIndex:        0,
		Asset:           "native",
		Amount:          big.NewInt(500_000),
		FromAddress:     g,
		ToAddress:       counterparty,
	}
	if _, err := chstore.InsertAccountMovements(ctx, chAddr, []chstore.AccountMovement{prePayment}); err != nil {
		t.Fatalf("InsertAccountMovements: %v", err)
	}

	// ─── Wire the real production stack (mirrors cmd/stellarindex-api) ──
	er, err := chstore.NewExplorerReader(ctx, chAddr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	srv := v1.New(v1.Options{Explorer: er, SEP41Movements: store})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/v1/accounts/" + g + "/movements?limit=10")
	if err != nil {
		t.Fatalf("GET movements: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var body struct {
		Data v1.AccountMovementsView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The healthy-path note is ALWAYS
	// present, stating the feed's post-P23 scope: without the cap67
	// archive provisioned (this harness has no watermark table), the
	// watched-token disclosure; with it, the through-ledger statement.
	if !strings.Contains(body.Data.CoverageNote, "watched") {
		t.Errorf("coverage_note = %q, want the post-P23 scope statement", body.Data.CoverageNote)
	}
	if len(body.Data.Movements) != 2 {
		t.Fatalf("movements = %d, want 2 (1 CH + 1 PG): %+v", len(body.Data.Movements), body.Data.Movements)
	}
	// Newest first: the post-P23 Postgres row (higher ledger) precedes
	// the pre-P23 ClickHouse row.
	if got := body.Data.Movements[0].TxHash; got != "pgtxaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("movements[0].tx_hash = %q, want the Postgres-tail row (newest)", got)
	}
	if got := body.Data.Movements[0].Provenance; got != "cap67_event" {
		t.Errorf("movements[0].provenance = %q, want cap67_event", got)
	}
	if got := body.Data.Movements[1].TxHash; got != "chtxbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("movements[1].tx_hash = %q, want the ClickHouse archive row (oldest)", got)
	}
	if got := body.Data.Movements[1].Provenance; got != "classic_derived" {
		t.Errorf("movements[1].provenance = %q, want classic_derived", got)
	}
	if body.Data.NextCursor != "" {
		t.Errorf("next_cursor = %q, want empty — both rows fit in one page (limit=10, 2 rows)", body.Data.NextCursor)
	}
}

// TestAccountMovements_DedupServesNewestVersion executes the account
// movements read against two un-merged versions of one key that differ
// only in counterparty — the shape an in-place re-derive leaves behind.
// The newer ingested_at must be served whichever part was written first;
// a LIMIT 1 BY without the version in its ORDER BY keeps an arbitrary one.
func TestAccountMovements_DedupServesNewestVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	chAddr := clickhouseAddr(t)
	if err := chstore.EnsureAccountMovementsTable(ctx, chAddr); err != nil {
		t.Fatalf("EnsureAccountMovementsTable: %v", err)
	}
	raw := dialClickHouse(t, ctx, "stellar")
	if err := raw.Exec(ctx, "SYSTEM STOP MERGES stellar.account_movements"); err != nil {
		t.Fatalf("SYSTEM STOP MERGES: %v", err)
	}
	t.Cleanup(func() { _ = raw.Exec(context.Background(), "SYSTEM START MERGES stellar.account_movements") })

	er, err := chstore.NewExplorerReader(ctx, chAddr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	stale := gAccountFromSeed(t, 0x61) // the original derive's counterparty
	fresh := gAccountFromSeed(t, 0x62) // the re-derive's counterparty
	insert := func(address, counterparty, ingestedAt string) {
		t.Helper()
		q := fmt.Sprintf(`INSERT INTO stellar.account_movements
			(address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
			 movement_kind, provenance, asset, counterparty, amount, ingested_at)
			VALUES ('%s', 45000000, toDateTime64('2024-01-01 00:00:00', 0, 'UTC'), '%064x', 0, 0, 'sent',
			 'payment', 'classic_derived', 'native', '%s', 1000, toDateTime('%s', 'UTC'))`,
			address, 7, counterparty, ingestedAt)
		if err := raw.Exec(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	for i, newerFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("newer_written_first=%v", newerFirst), func(t *testing.T) {
			g := gAccountFromSeed(t, byte(0x63+i))
			if newerFirst {
				insert(g, fresh, "2026-09-02 00:00:00")
				insert(g, stale, "2026-09-01 00:00:00")
			} else {
				insert(g, stale, "2026-09-01 00:00:00")
				insert(g, fresh, "2026-09-02 00:00:00")
			}
			rows, err := er.AccountMovements(ctx, g, 10, chstore.AccountMovementCursor{}, chstore.AccountMovementFilter{})
			if err != nil {
				t.Fatalf("AccountMovements: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want 1 (LIMIT 1 BY dedup of the two versions)", len(rows))
			}
			if rows[0].Counterparty != fresh {
				t.Errorf("counterparty = %s, want the newer version's %s (stale %s was served)", rows[0].Counterparty, fresh, stale)
			}
		})
	}
}
