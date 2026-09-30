//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// An aclfile accepts neither comments nor line continuations, and
// redis-server refuses to start on the first bad line — so the shipped
// template is loaded into a real server, not just grepped.

var jinjaComment = regexp.MustCompile(`(?s)\{#.*?#\}`)

const aclLoadFixturePwd = "acl-load-fixture" // gitleaks:allow — throwaway container ACL password, not a credential

// renderShippedACL renders users.acl.j2 the way ansible's template module
// would for the only variable it uses, refusing any jinja it cannot render.
func renderShippedACL(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", aclTemplatePath))
	if err != nil {
		t.Fatalf("read ACL template: %v", err)
	}
	out := jinjaComment.ReplaceAllString(string(raw), "")
	out = strings.ReplaceAll(out, "{{ redis_password }}", aclLoadFixturePwd)
	for _, tok := range []string{"{{", "{%", "{#"} {
		if strings.Contains(out, tok) {
			t.Fatalf("%s uses jinja (%q) this fixture does not render; extend renderShippedACL", aclTemplatePath, tok)
		}
	}
	return out
}

func TestRedisACLTemplate_LoadsIntoRedisServer(t *testing.T) {
	ctx := context.Background()
	ctr, err := testcontainers.Run(ctx,
		"redis:7.4-alpine",
		testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader:            strings.NewReader(renderShippedACL(t)),
			ContainerFilePath: "/users.acl",
			FileMode:          0o644,
		}),
		testcontainers.WithCmd("redis-server", "--aclfile", "/users.acl"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("Ready to accept connections").WithStartupTimeout(30*time.Second),
		),
	)
	if ctr != nil {
		t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })
	}
	if err != nil {
		if isDockerUnavailable(err) {
			t.Skipf("docker unavailable, skipping integration test: %v", err)
		}
		logs := ""
		if ctr != nil {
			if rc, lerr := ctr.Logs(ctx); lerr == nil {
				b, _ := io.ReadAll(rc)
				logs = string(b)
			}
		}
		t.Fatalf("redis-server did not start with the rendered %s: %v\n%s", aclTemplatePath, err, logs)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "6379")
	if err != nil {
		t.Fatalf("container mapped port: %v", err)
	}
	addr := fmt.Sprintf("%s:%s", host, port.Port())
	client := func(user, pwd string) *redis.Client {
		c := redis.NewClient(&redis.Options{Addr: addr, Username: user, Password: pwd})
		t.Cleanup(func() { _ = c.Close() })
		return c
	}

	if err := client("", "").Ping(ctx).Err(); err == nil || !strings.Contains(err.Error(), "NOAUTH") {
		t.Fatalf("unauthenticated PING = %v, want NOAUTH: the default user is not off", err)
	}

	app := client("stellarindex", aclLoadFixturePwd)
	// The first and last key-pattern and channel lines of the rule, so a
	// rule cut short at a line break fails here.
	if err := app.Set(ctx, "vwap:acl-load", "1", time.Minute).Err(); err != nil {
		t.Fatalf("stellarindex SET vwap:*: %v", err)
	}
	if err := app.Set(ctx, "markets:list:acl-load", "1", time.Minute).Err(); err != nil {
		t.Fatalf("stellarindex SET markets:list:*: %v", err)
	}
	if err := app.Publish(ctx, "stream-acl-load", "x").Err(); err != nil {
		t.Fatalf("stellarindex PUBLISH stream-*: %v", err)
	}
	if err := app.FlushAll(ctx).Err(); err == nil || !strings.Contains(err.Error(), "NOPERM") {
		t.Fatalf("stellarindex FLUSHALL = %v, want NOPERM", err)
	}

	if err := client("redis_exporter", aclLoadFixturePwd).Info(ctx).Err(); err != nil {
		t.Fatalf("redis_exporter INFO: %v", err)
	}
	if err := client("sentinel", aclLoadFixturePwd).Do(ctx, "ROLE").Err(); err != nil {
		t.Fatalf("sentinel ROLE: %v", err)
	}
	if err := client("replication", aclLoadFixturePwd).Ping(ctx).Err(); err != nil {
		t.Fatalf("replication PING: %v", err)
	}
}
