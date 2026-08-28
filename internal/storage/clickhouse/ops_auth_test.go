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
