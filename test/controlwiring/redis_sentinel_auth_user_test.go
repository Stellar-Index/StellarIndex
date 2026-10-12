package controlwiring

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
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

// The postgres-backend API-key validator caches api_keys rows under
// `apikey-cache:` (cachekeys.APIKeyCache), outside `apikey:` on purpose,
// so the lockdown app user must admit that family by name.
func TestRedisACL_AppUserAdmitsAPIKeyCache(t *testing.T) {
	path := filepath.Join(repoRoot(t), "configs", "ansible", "roles", "redis-sentinel",
		"templates", "users.acl.j2")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var rule []string
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if len(rule) == 0 && !strings.HasPrefix(trimmed, "user stellarindex ") {
			continue
		}
		rule = append(rule, strings.Fields(strings.TrimSuffix(trimmed, "\\"))...)
		if !strings.HasSuffix(trimmed, "\\") {
			break
		}
	}
	if !slices.Contains(rule, "~apikey-cache:*") {
		t.Fatalf("%s: the stellarindex user rule does not admit ~apikey-cache:* — "+
			"under lockdown every postgres-backend key-cache read and write is NOPERM", path)
	}
}

// lint-replay-plan.sh derives the projected set by parsing source_spec.go
// with awk, so a spec written in a shape the parser does not know would
// silently drop out of the gate. Compare it with the registry itself.
func TestReplayPlanGateProjectedSetMatchesRegistry(t *testing.T) {
	var want []string
	for _, s := range pipeline.Specs() {
		if s.Projector != nil {
			want = append(want, s.Name)
		}
	}
	slices.Sort(want)

	root := repoRoot(t)
	cmd := exec.Command("bash", filepath.Join(root, "scripts/ci/lint-replay-plan.sh"), "--list-projected")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lint-replay-plan.sh --list-projected: %v\n%s", err, out)
	}
	got := strings.Fields(string(out))
	slices.Sort(got)

	if !slices.Equal(got, want) {
		t.Errorf("lint-replay-plan.sh projected set differs from pipeline.Specs():\n gate:     %v\n registry: %v", got, want)
	}
}
