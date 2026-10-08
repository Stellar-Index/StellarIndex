//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/accounterasure"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

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
		mustExec(t, ctx, db, `INSERT INTO magic_link_tokens (token_hash, email, purpose, expires_at, requested_ip)
			VALUES ('ml-stranger', $1, 'login', now() + interval '1 hour', '203.0.113.5')`, stranger)
		mustExec(t, ctx, db, `INSERT INTO invites (token_hash, account_id, email, role, invited_by_user_id, expires_at)
			VALUES ('inv-stranger', $1, $2, 'member', $3, now() + interval '1 day')`, otherID, stranger, otherOwner)

		if _, err := eraser.Erase(ctx, x.accountID, platform.ActorUser); err != nil {
			t.Fatalf("Erase: %v", err)
		}
		var lockouts, links, invites, memberLockouts int
		if err := db.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM login_code_lockouts WHERE email = $1 AND failed_count = 4),
			(SELECT count(*) FROM magic_link_tokens WHERE email = $1),
			(SELECT count(*) FROM invites WHERE email = $1),
			(SELECT count(*) FROM login_code_lockouts WHERE email = 'mary-7@b.example')`, stranger).
			Scan(&lockouts, &links, &invites, &memberLockouts); err != nil {
			t.Fatal(err)
		}
		if lockouts != 1 || links != 1 || invites != 1 {
			t.Errorf("stranger's lockout/magic link/invite = %d/%d/%d, want 1/1/1 kept", lockouts, links, invites)
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
