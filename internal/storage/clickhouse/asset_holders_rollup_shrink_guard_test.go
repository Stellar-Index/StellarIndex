// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// shrinkGuardConn is a scripted holdersRollupConn: every Exec succeeds and
// is recorded in issue order, and QueryRow answers a fixed count() per
// exact "SELECT count() FROM <table>" query.
type shrinkGuardConn struct {
	counts    map[string]uint64
	execCalls []string
}

func (c *shrinkGuardConn) Exec(_ context.Context, query string, _ ...any) error {
	c.execCalls = append(c.execCalls, query)
	return nil
}

func (c *shrinkGuardConn) QueryRow(_ context.Context, query string, _ ...any) driver.Row {
	for table, n := range c.counts {
		if query == "SELECT count() FROM "+table {
			return &shrinkGuardRow{n: n}
		}
	}
	return &shrinkGuardRow{err: fmt.Errorf("shrinkGuardConn: no scripted count for %q", query)}
}

type shrinkGuardRow struct {
	driver.Row
	n   uint64
	err error
}

func (r *shrinkGuardRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	p, ok := dest[0].(*uint64)
	if !ok {
		return fmt.Errorf("shrinkGuardRow: unexpected dest %T, want *uint64", dest[0])
	}
	*p = r.n
	return nil
}

func (r *shrinkGuardRow) Err() error { return r.err }

// TestRunHoldersRollupRefusesAShrunkenBoard pins T397: a staging arm that
// comes out of the fills far smaller than what is currently live must not
// be published. ClickHouse can finish the FINAL scan that fills a staging
// arm having read far fewer rows than a healthy cycle WITHOUT returning any
// error, so the only thing that can catch it is a row-count comparison
// between the fills finishing and the EXCHANGE firing.
//
// Proven red against the pre-fix RunHoldersRollup, which executed every
// statement in holdersRollupStatements — including the final EXCHANGE — in
// a plain loop with no check in between: the swap fired anyway and the
// degraded board went live.
func TestRunHoldersRollupRefusesAShrunkenBoard(t *testing.T) {
	conn := &shrinkGuardConn{counts: map[string]uint64{
		// Staging came out of this cycle's fills with almost nothing —
		// the shape a truncated/partial FINAL scan produces.
		"stellar.asset_holders_rollup_staging": 3,
		// What is CURRENTLY live: a healthy prior cycle's board.
		"stellar.asset_holders_rollup":         500_000,
		"stellar.asset_holders_counts_staging": 500,
		"stellar.asset_holders_counts":         500,
	}}

	err := runHoldersRollupSteps(context.Background(), conn, func(string, ...any) {})
	if err == nil {
		t.Fatal("runHoldersRollupSteps swapped a board that shrank from 500,000 to 3 rows instead of refusing")
	}
	if !strings.Contains(err.Error(), "shrink guard") {
		t.Errorf("error does not name the shrink guard: %v", err)
	}

	for _, q := range conn.execCalls {
		if strings.Contains(q, "EXCHANGE TABLES") {
			t.Errorf("EXCHANGE TABLES was issued despite the shrink — the previous (good) cycle must stay live:\n%s", q)
		}
	}
}

// TestRunHoldersRollupPublishesAHealthyBoard is the guard's negative case:
// a cycle whose staging counts are in line with (or ahead of) live must
// still swap, so the shrink guard cannot itself become a standing outage.
func TestRunHoldersRollupPublishesAHealthyBoard(t *testing.T) {
	conn := &shrinkGuardConn{counts: map[string]uint64{
		"stellar.asset_holders_rollup_staging": 500_100,
		"stellar.asset_holders_rollup":         500_000,
		"stellar.asset_holders_counts_staging": 505,
		"stellar.asset_holders_counts":         500,
	}}

	if err := runHoldersRollupSteps(context.Background(), conn, func(string, ...any) {}); err != nil {
		t.Fatalf("runHoldersRollupSteps: %v", err)
	}

	found := false
	for _, q := range conn.execCalls {
		if strings.Contains(q, "EXCHANGE TABLES") {
			found = true
		}
	}
	if !found {
		t.Error("a healthy cycle did not issue the EXCHANGE TABLES swap")
	}
}
