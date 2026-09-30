// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// The ops-batch identity resolves from the environment and ONLY from
// the environment (2026-08-28 r1: ch-rebuild as CH `default` starved
// the aggregator's supply refresher; the fix is that ops jobs
// authenticate as the low-priority `ops_batch` user when
// /etc/default/stellarindex-ops carries the pair).
func TestOpsAuthFrom(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	t.Run("unset is the CH default user against stellar", func(t *testing.T) {
		got, err := opsAuthFrom(env(nil))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := clickhouse.Auth{Database: "stellar"}
		if got != want {
			t.Fatalf("opsAuthFrom(unset) = %+v, want %+v (pre-fix behaviour must be unchanged)", got, want)
		}
	})

	t.Run("both set authenticates as the ops-batch user", func(t *testing.T) {
		got, err := opsAuthFrom(env(map[string]string{
			OpsUserEnv:     "ops_batch",
			OpsPasswordEnv: "s3cret",
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := clickhouse.Auth{Database: "stellar", Username: "ops_batch", Password: "s3cret"}
		if got != want {
			t.Fatalf("opsAuthFrom(set) = %+v, want %+v", got, want)
		}
	})

	t.Run("user without password is allowed (passwordless CH user)", func(t *testing.T) {
		got, err := opsAuthFrom(env(map[string]string{OpsUserEnv: "ops_batch"}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Username != "ops_batch" || got.Password != "" || got.Database != "stellar" {
			t.Fatalf("opsAuthFrom(user only) = %+v", got)
		}
	})

	t.Run("password without user is refused, naming both vars", func(t *testing.T) {
		_, err := opsAuthFrom(env(map[string]string{OpsPasswordEnv: "s3cret"}))
		if err == nil {
			t.Fatal("expected an error for a password without a username")
		}
		for _, want := range []string{OpsUserEnv, OpsPasswordEnv} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %s", err, want)
			}
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("error %q leaks the password", err)
		}
	})

	t.Run("env var names are the documented ones", func(t *testing.T) {
		// Pinned because 09-minio.yml's /etc/default/stellarindex-ops
		// template and docs/operations/clickhouse-ops-batch-profile.md
		// spell these out by hand; a rename here must sweep them.
		if OpsUserEnv != "STELLARINDEX_CLICKHOUSE_OPS_USER" || OpsPasswordEnv != "STELLARINDEX_CLICKHOUSE_OPS_PASSWORD" {
			t.Fatalf("env var names drifted: %q / %q", OpsUserEnv, OpsPasswordEnv)
		}
	})
}

// The live-daemon pair is consulted only when the ops pair is absent, and
// every pair unset stays CH `default` so a binary can ship before the CH
// user and env file exist.
func TestChAuthFrom(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	live := map[string]string{LiveUserEnv: "live_daemon", LivePasswordEnv: "l1ve"}
	both := map[string]string{LiveUserEnv: "live_daemon", LivePasswordEnv: "l1ve", OpsUserEnv: "ops_batch", OpsPasswordEnv: "0ps"}
	cases := []struct {
		name string
		env  map[string]string
		want clickhouse.Auth
	}{
		{"nothing set is the CH default user", nil, clickhouse.Auth{Database: "stellar"}},
		{"live pair alone is live_daemon", live, clickhouse.Auth{Database: "stellar", Username: "live_daemon", Password: "l1ve"}},
		{"ops pair wins over the live pair", both, clickhouse.Auth{Database: "stellar", Username: "ops_batch", Password: "0ps"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := chAuthFrom(env(tc.env))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("chAuthFrom = %+v, want %+v", got, tc.want)
			}
		})
	}

	t.Run("live password without user is refused, naming both vars", func(t *testing.T) {
		_, err := chAuthFrom(env(map[string]string{LivePasswordEnv: "l1ve"}))
		if err == nil {
			t.Fatal("expected an error for a live password without a username")
		}
		for _, want := range []string{LiveUserEnv, LivePasswordEnv} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %s", err, want)
			}
		}
		if strings.Contains(err.Error(), "l1ve") {
			t.Errorf("error %q leaks the password", err)
		}
	})

	t.Run("live env var names are the documented ones", func(t *testing.T) {
		// stellarindex.env.j2 spells these out by hand.
		if LiveUserEnv != "STELLARINDEX_CLICKHOUSE_LIVE_USER" || LivePasswordEnv != "STELLARINDEX_CLICKHOUSE_LIVE_PASSWORD" {
			t.Fatalf("env var names drifted: %q / %q", LiveUserEnv, LivePasswordEnv)
		}
	})
}
