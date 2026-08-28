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
// WHY (2026-08-28, r1): a runbook-prescribed `ch-rebuild -sep41`
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
// Auth through [opsAuth], which takes an optional username/password
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
// Both unset = byte-for-byte the pre-fix behaviour: clickhouse-go
// treats an empty Auth.Username as CH's `default` user. This is
// deliberately the ONLY switch — the indexer / aggregator / API
// services source /etc/default/stellarindex, which MUST NOT carry
// these variables (a live-ingest sink or the supply refresher running
// as ops_batch would be demoted to lowest priority, the exact inverse
// of what this exists for). See docs/operations/
// clickhouse-ops-batch-profile.md.
const (
	// OpsUserEnv names the env var holding the ops-batch CH username.
	OpsUserEnv = "STELLARINDEX_CLICKHOUSE_OPS_USER"
	// OpsPasswordEnv names the env var holding that user's password.
	OpsPasswordEnv = "STELLARINDEX_CLICKHOUSE_OPS_PASSWORD"
)

// opsAuth resolves the Auth every ops-side connection builder in this
// package opens with: the `stellar` database, plus the ops-batch
// username/password from the environment when set.
func opsAuth() (clickhouse.Auth, error) {
	return opsAuthFrom(os.Getenv)
}

// opsAuthFrom is [opsAuth] with the environment lookup injected, so
// the resolution rules are unit-testable without mutating the
// process environment.
//
// A password WITHOUT a username is refused rather than silently
// ignored: clickhouse-go would send it as the `default` user's
// password, which CH rejects, and a half-set pair is always a
// templating mistake worth surfacing at open time rather than as a
// confusing authentication failure.
func opsAuthFrom(getenv func(string) string) (clickhouse.Auth, error) {
	user, pass := getenv(OpsUserEnv), getenv(OpsPasswordEnv)
	if user == "" && pass != "" {
		return clickhouse.Auth{}, fmt.Errorf("clickhouse: %s is set but %s is empty — set both (ops_batch identity) or neither (CH default user)", OpsPasswordEnv, OpsUserEnv)
	}
	return clickhouse.Auth{Database: "stellar", Username: user, Password: pass}, nil
}
