//go:build integration

package integration_test

// Real-Redis, real-ACL coverage for the API-key lookup index
// (findings F057 / K051, reverification 2026-09-18).
//
// Why this exists alongside internal/auth/key_index_test.go
// ─────────────────────────────────────────────────────────
// The unit suite models an ACL denial with a client-side hook, because
// miniredis has no ACLs. Everything that makes the rollout safe rests
// on how redis-server ITSELF treats a Lua script that names a key the
// caller may not touch:
//
//   - **Nothing is written when the index family is denied.** The
//     issuance script touches the index before the record; if the
//     server ran any of it first, a denied deployment would half-write.
//   - **The denial reads as NOPERM.** The store falls back to the
//     record-only write on that text and on nothing else. If a server
//     phrased a script-level denial differently, a lockdown deployment
//     without `~apikey-index:*` would stop issuing keys.
//   - **The SHIPPED ACL admits the family.** The rule under test is
//     parsed out of configs/ansible/.../users.acl.j2, not retyped here,
//     so deleting the pattern from the template fails this test.
//
// The production trap this pins (codified is not applied): the binary
// reaches a host before the ansible-rendered ACL does. The middle
// subtest IS that window.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

const (
	aclTemplatePath  = "configs/ansible/roles/redis-sentinel/templates/users.acl.j2"
	aclIndexPattern  = "~apikey-index:*"
	aclFixtureAppPwd = "index-acl-fixture" // gitleaks:allow — throwaway container ACL user, not a credential
)

// shippedStellarindexACLRule returns the `user stellarindex …` rule of
// the shipped template as ACL SETUSER arguments, with the vaulted
// password placeholder replaced by the fixture word.
func shippedStellarindexACLRule(t *testing.T) []string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", aclTemplatePath))
	if err != nil {
		t.Fatalf("read ACL template: %v", err)
	}
	var rule []string
	inRule := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !inRule && !strings.HasPrefix(trimmed, "user stellarindex ") {
			continue
		}
		inRule = true
		rule = append(rule, strings.TrimSuffix(trimmed, "\\"))
		if !strings.HasSuffix(trimmed, "\\") {
			break
		}
	}
	joined := strings.Join(rule, " ")
	if !strings.Contains(joined, ">{{ redis_password }}") {
		t.Fatalf("template rule has no password placeholder to substitute: %q", joined)
	}
	joined = strings.Replace(joined, ">{{ redis_password }}", ">"+aclFixtureAppPwd, 1)
	fields := strings.Fields(joined)
	if len(fields) < 4 || fields[0] != "user" || fields[1] != "stellarindex" {
		t.Fatalf("could not parse the stellarindex rule out of %s: %q", aclTemplatePath, joined)
	}
	return fields[2:]
}

func withoutPattern(rule []string, pattern string) []string {
	out := make([]string, 0, len(rule))
	for _, f := range rule {
		if f != pattern {
			out = append(out, f)
		}
	}
	return out
}

func setACLUser(ctx context.Context, t *testing.T, admin *redis.Client, user string, rule []string) {
	t.Helper()
	args := []any{"ACL", "SETUSER", user, "reset"}
	for _, f := range rule {
		args = append(args, f)
	}
	if err := admin.Do(ctx, args...).Err(); err != nil {
		t.Fatalf("ACL SETUSER %s: %v", user, err)
	}
}

func appClient(t *testing.T, admin *redis.Client, user string) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: admin.Options().Addr, Username: user, Password: aclFixtureAppPwd})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestAPIKeyIndex_RealRedisACL(t *testing.T) {
	ctx := context.Background()
	admin := startPlainRedis(ctx, t)
	shipped := shippedStellarindexACLRule(t)
	index := cachekeys.APIKeyIndex().String()

	t.Run("the shipped template admits the index family", func(t *testing.T) {
		found := false
		for _, f := range shipped {
			found = found || f == aclIndexPattern
		}
		if !found {
			t.Fatalf("%s does not grant %s: every index access is NOPERM under lockdown and the keyspace walk never retires",
				aclTemplatePath, aclIndexPattern)
		}
	})

	t.Run("ACL not yet applied: issuance and revocation work, nothing half-written", func(t *testing.T) {
		if err := admin.FlushAll(ctx).Err(); err != nil {
			t.Fatalf("flushall: %v", err)
		}
		setACLUser(ctx, t, admin, "stellarindex", withoutPattern(shipped, aclIndexPattern))
		app := appClient(t, admin, "stellarindex")
		store := auth.NewRedisAPIKeyStore(app)
		validator := auth.NewRedisAPIKeyValidator(app)

		// The denial really is in force, and really reads NOPERM.
		err := app.HGet(ctx, index, "ready").Err()
		if err == nil || errors.Is(err, redis.Nil) || !strings.Contains(err.Error(), "NOPERM") {
			t.Fatalf("index read under the old ACL = %v, want a NOPERM denial", err)
		}

		const owner = "account:acl-gap"
		first, firstPlain, err := store.Create(ctx, auth.CreateAPIKeyRequest{Identifier: owner})
		if err != nil {
			t.Fatalf("Create while the index family is denied: %v — key issuance is DOWN until ansible runs", err)
		}
		second, secondPlain, err := store.Create(ctx, auth.CreateAPIKeyRequest{Identifier: owner})
		if err != nil {
			t.Fatalf("second Create: %v", err)
		}
		if _, err := validator.Lookup(ctx, firstPlain); err != nil {
			t.Fatalf("key issued under the old ACL does not authenticate: %v", err)
		}
		if n, err := admin.Exists(ctx, index).Result(); err != nil || n != 0 {
			t.Fatalf("index key exists=%d err=%v after denied issuance: the script ran past the denial", n, err)
		}
		if n, err := admin.DBSize(ctx).Result(); err != nil || n != 2 {
			t.Fatalf("dbsize=%d err=%v, want exactly the 2 credential records", n, err)
		}

		recs, err := store.ListKeysForIdentifier(ctx, owner)
		if err != nil || len(recs) != 2 {
			t.Fatalf("list under the old ACL: %d records, err=%v; want 2", len(recs), err)
		}
		if err := store.RevokeKeyByID(ctx, owner, first.KeyID); err != nil {
			t.Fatalf("revoke under the old ACL: %v", err)
		}
		if _, err := validator.Lookup(ctx, firstPlain); err == nil {
			t.Fatal("revoked key still authenticates under the old ACL: revocation silently no-ops")
		}

		// Ansible applies the template: the pattern is granted to the live
		// user. The API process is NOT restarted — same store, same pool —
		// so this also proves no restart is needed to retire the walk.
		if err := admin.Do(ctx, "ACL", "SETUSER", "stellarindex", aclIndexPattern).Err(); err != nil {
			t.Fatalf("grant %s: %v", aclIndexPattern, err)
		}
		recs, err = store.ListKeysForIdentifier(ctx, owner)
		if err != nil || len(recs) != 1 || recs[0].KeyID != second.KeyID {
			t.Fatalf("list after the ACL is applied = %+v, err=%v; want the key minted during the gap", recs, err)
		}
		if ready, err := admin.HGet(ctx, index, "ready").Result(); err != nil || ready != "1" {
			t.Fatalf("index ready=%q err=%v after the first lookup under the shipped ACL", ready, err)
		}
		if err := store.RevokeKeyByID(ctx, owner, second.KeyID); err != nil {
			t.Fatalf("revoke after the ACL is applied: %v", err)
		}
		if _, err := validator.Lookup(ctx, secondPlain); err == nil {
			t.Fatal("a key minted during the ACL gap survives revocation once the index is live")
		}
	})

	t.Run("shipped ACL: indexed issuance, lookups and revoke, surviving a script-cache flush", func(t *testing.T) {
		if err := admin.FlushAll(ctx).Err(); err != nil {
			t.Fatalf("flushall: %v", err)
		}
		setACLUser(ctx, t, admin, "stellarindex", shipped)
		app := appClient(t, admin, "stellarindex")
		store := auth.NewRedisAPIKeyStore(app)

		const owner = "account:steady-state"
		if _, err := store.ListKeysForIdentifier(ctx, owner); err != nil { // builds on an empty deployment
			t.Fatalf("first lookup: %v", err)
		}
		rec, plaintext, err := store.Create(ctx, auth.CreateAPIKeyRequest{Identifier: owner})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Every Redis restart or failover empties the script cache.
		if err := admin.ScriptFlush(ctx).Err(); err != nil {
			t.Fatalf("script flush: %v", err)
		}
		mirrored := auth.MirroredKey{
			Plaintext:  "sip_" + strings.Repeat("cd", 32),
			KeyID:      "kid_acl_mirror",
			Identifier: owner,
		}
		if err := store.CreateWithSecret(ctx, mirrored); err != nil {
			t.Fatalf("CreateWithSecret after SCRIPT FLUSH: %v", err)
		}
		if ptr, err := admin.HGet(ctx, index, "k:"+rec.KeyID).Result(); err != nil || len(ptr) != 64 {
			t.Fatalf("KeyID pointer = %q err=%v, want the record's sha256 hex", ptr, err)
		}
		if ttl, err := admin.TTL(ctx, index).Result(); err != nil || ttl >= 0 {
			t.Fatalf("index ttl=%v err=%v, want none", ttl, err)
		}
		recs, err := store.ListKeysForIdentifier(ctx, owner)
		if err != nil || len(recs) != 2 {
			t.Fatalf("list: %d records, err=%v; want 2", len(recs), err)
		}
		if _, err := store.UpdateRateLimit(ctx, rec.KeyID, 42); err != nil {
			t.Fatalf("UpdateRateLimit: %v", err)
		}
		if err := store.RevokeKeyByID(ctx, owner, rec.KeyID); err != nil {
			t.Fatalf("RevokeKeyByID: %v", err)
		}
		if _, err := auth.NewRedisAPIKeyValidator(app).Lookup(ctx, plaintext); err == nil {
			t.Fatal("revoked key still authenticates")
		}
		if n, err := admin.HExists(ctx, index, "k:"+rec.KeyID).Result(); err != nil || n {
			t.Fatalf("KeyID pointer survived the revoke (exists=%v err=%v)", n, err)
		}
	})
}
