package controlwiring

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// ─── SL03: Sentinel must authenticate to the primary as a named ACL
// user under lockdown, not the disabled `default` user ────────────
//
// sentinel.conf.j2 has always rendered `sentinel auth-pass`, which
// authenticates as the `default` user. redis.conf.j2's ACL lockdown
// (`redis_acl_lockdown: true`, users.acl.j2) sets `user default off
// nopass nocommands` — so once lockdown is applied, Sentinel's
// username-less AUTH is rejected outright and it can no longer poll
// or fail over the primary at all. `sentinel auth-user` names the ACL
// user to authenticate as instead; without it, `auth-pass` alone is
// insufficient under lockdown.
var (
	sentinelAuthUserPattern = regexp.MustCompile(
		`(?m)^sentinel auth-user \{\{ redis_sentinel_master_name \}\} \{\{ redis_sentinel_auth_user \}\}\s*$`)
	sentinelACLUserPattern = regexp.MustCompile(`(?m)^user sentinel on >\{\{ redis_password \}\}`)
)

func TestRedisSentinelTemplate_AuthUserSetUnderLockdown(t *testing.T) {
	path := filepath.Join(repoRoot(t), "configs", "ansible", "roles", "redis-sentinel",
		"templates", "sentinel.conf.j2")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !sentinelAuthUserPattern.Match(raw) {
		t.Fatalf("%s: no `sentinel auth-user %%s %%s` directive — Sentinel would "+
			"authenticate to the primary as the disabled `default` user once "+
			"redis_acl_lockdown is applied", path)
	}
}

func TestRedisSentinelACL_DefinesSentinelUser(t *testing.T) {
	path := filepath.Join(repoRoot(t), "configs", "ansible", "roles", "redis-sentinel",
		"templates", "users.acl.j2")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !sentinelACLUserPattern.Match(raw) {
		t.Fatalf("%s: no `user sentinel on >{{ redis_password }}` ACL entry — "+
			"sentinel.conf.j2's `sentinel auth-user` would name a user that does "+
			"not exist under lockdown", path)
	}
}
