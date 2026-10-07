// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"fmt"
	"os"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// Ops-batch ClickHouse identity — the LOW-priority counterpart of
// ADR-0048 D4's `api_serving` profile.
//
// WHY: a runbook-prescribed `ch-rebuild -sep41`
// dry-run over 2M ledgers drove host load to 12.9 and starved the
// aggregator's supply refresher — stellarindex_aggregator_
// supply_refresh_error_dominant fired for all 39 watched contracts
// within 3 minutes; killing the job cleared it. run-heavy-job.sh's
// cgroup caps (CPUWeight=50 / IOWeight=50 / MemoryMax=20G) did not
// help because the contention was INSIDE clickhouse-server: the ops
// job's queries and the aggregator's queries both ran as CH's
// unauthenticated `default` user, so CH's query scheduler had no
// signal that one of them was a batch job it should yield with.
//
// The fix is an identity, not a cgroup: every ops-side ClickHouse
// connection built in this package (openRead's heavy-FINAL gate/
// reconcile class, the Sink / participant / account-movements /
// entry-change writers, and the no-credential NewExplorerReader /
// NewSupplyReader constructors the ops subcommands use) resolves its
// Auth through [chAuth], which takes an optional username/password
// from the ENVIRONMENT:
//
//	STELLARINDEX_CLICKHOUSE_OPS_USER      (e.g. "ops_batch")
//	STELLARINDEX_CLICKHOUSE_OPS_PASSWORD
//
// Both come from /etc/default/stellarindex-ops (configs/ansible/roles/
// archival-node/tasks/09-minio.yml, mode 0640, vault-sourced) — the
// env file only the stellarindex-ops binary, its systemd timers and
// the scripts/ops/*.sh wrappers source. The `ops_batch` CH settings
// profile + user is provisioned by 20-clickhouse-serving-profile.yml
// (priority = large number = lowest, small max_threads, capped
// max_memory_usage, readonly=0 because ops jobs write). Environment
// rather than argv because a password in argv is world-readable via
// /proc and lands in the journal through run-heavy-job.sh's
// systemd-run line (feedback: never pass secrets in argv); and
// environment rather than a stellarindex.toml field because the ops
// subcommands take `-ch` as a bare flag and do not all load the
// config file.
//
// Live-daemon ClickHouse identity. The indexer, aggregator and API
// otherwise reach CH as its unauthenticated `default` user; a named
// `live_daemon` user (provisioned by 20-clickhouse-serving-profile.yml,
// same `default` settings profile) lets `default` be locked down later
// without cutting those daemons off. The pair lives in
// /etc/default/stellarindex, the env file only the live-daemon units
// source; like the ops pair it is environment rather than config
// because the connection builders here are shared with the ops CLI,
// which does not load stellarindex.toml.
//
// Precedence ([chAuthFrom]): the ops pair, then the live pair, then CH
// `default`. Ops first because batch units source both env files and
// must still run at the batch tier. Every pair unset is byte-for-byte
// the pre-fix behaviour (clickhouse-go treats an empty Auth.Username as
// CH's `default` user), which keeps the rollout order-safe: a binary can
// ship before the CH user and env file exist. The ops pair must reach
// ONLY the batch jobs' environment; the deploy/systemd reference units
// share /etc/default/stellarindex-ops with the batch one-shots, so each
// live-daemon unit strips it with `UnsetEnvironment=`.
// TestOpsBatchIdentityNeverReachesLiveDaemons pins that live daemons
// resolve to live_daemon (or `default` when unconfigured) and never to
// ops_batch, and batch units to ops_batch.
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
