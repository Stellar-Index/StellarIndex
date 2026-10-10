// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"fmt"
	"os"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// Ops-batch ClickHouse identity: the LOW-priority counterpart of ADR-0048 D4's
// `api_serving` profile.
//
// Ops jobs and the aggregator contend INSIDE clickhouse-server; cgroup caps do
// not help, but a separate CH user lets CH's scheduler deprioritise ops. Every
// ops-side connection here resolves its Auth through [chAuth], from the ENVIRONMENT:
//
//	STELLARINDEX_CLICKHOUSE_OPS_USER      (e.g. "ops_batch")
//	STELLARINDEX_CLICKHOUSE_OPS_PASSWORD
//
// Environment, not argv (world-readable via /proc, lands in the journal), and
// not config (not all ops subcommands load it). The `ops_batch` user is
// provisioned by 20-clickhouse-serving-profile.yml.
//
// Live daemons may use a named `live_daemon` user (same `default` settings
// profile) so `default` can be locked down later; also environment.
//
// Precedence ([chAuthFrom]): ops pair, then live pair, then CH `default`. Ops
// first because batch units source both env files. Every pair unset is
// byte-for-byte the unconfigured behaviour (an empty Auth.Username is CH's
// `default` user), so a binary can ship before the CH user exists. The ops pair
// must reach ONLY batch jobs: each live-daemon unit strips it with
// `UnsetEnvironment=`, pinned by TestOpsBatchIdentityNeverReachesLiveDaemons.
// See docs/operations/clickhouse-ops-batch-profile.md.
const (
	// OpsUserEnv names the env var holding the ops-batch CH username.
	OpsUserEnv = "STELLARINDEX_CLICKHOUSE_OPS_USER"
	// OpsPasswordEnv names the env var holding that user's password.
	OpsPasswordEnv = "STELLARINDEX_CLICKHOUSE_OPS_PASSWORD"
	// LiveUserEnv names the env var holding the live-daemon CH username.
	LiveUserEnv = "STELLARINDEX_CLICKHOUSE_LIVE_USER"
	// LivePasswordEnv names the env var holding that user's password.
	LivePasswordEnv = "STELLARINDEX_CLICKHOUSE_LIVE_PASSWORD"
)

// chAuth resolves the Auth every ClickHouse connection builder in this
// package opens with when the caller passes no explicit credentials.
func chAuth() (clickhouse.Auth, error) {
	return chAuthFrom(os.Getenv)
}

// chAuthFrom is [chAuth] with the environment lookup injected.
func chAuthFrom(getenv func(string) string) (clickhouse.Auth, error) {
	auth, err := opsAuthFrom(getenv)
	if err != nil || auth.Username != "" {
		return auth, err
	}
	return pairAuth(getenv, LiveUserEnv, LivePasswordEnv, "live_daemon identity")
}

// authOrEnv is the Auth for an explicit username/password, or [chAuth]'s
// when both are empty.
func authOrEnv(username, password string) (clickhouse.Auth, error) {
	if username == "" && password == "" {
		return chAuth()
	}
	return clickhouse.Auth{Database: "stellar", Username: username, Password: password}, nil
}

// opsAuthFrom resolves the ops-batch pair alone.
func opsAuthFrom(getenv func(string) string) (clickhouse.Auth, error) {
	return pairAuth(getenv, OpsUserEnv, OpsPasswordEnv, "ops_batch identity")
}

// pairAuth reads one username/password env pair against `stellar`.
//
// A password WITHOUT a username is refused rather than silently
// ignored: clickhouse-go would send it as the `default` user's
// password, which CH rejects, and a half-set pair is always a
// templating mistake worth surfacing at open time rather than as a
// confusing authentication failure.
func pairAuth(getenv func(string) string, userEnv, passEnv, what string) (clickhouse.Auth, error) {
	user, pass := getenv(userEnv), getenv(passEnv)
	if user == "" && pass != "" {
		return clickhouse.Auth{}, fmt.Errorf("clickhouse: %s is set but %s is empty — set both (%s) or neither (CH default user)", passEnv, userEnv, what)
	}
	return clickhouse.Auth{Database: "stellar", Username: user, Password: pass}, nil
}
