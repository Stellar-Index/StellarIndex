// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	maxExecutionTimeRe = regexp.MustCompile(`max_execution_time\s*=\s*(\d+)`)
	timeoutStartSecRe  = regexp.MustCompile(`(?m)^TimeoutStartSec=(.+)$`)
	systemdSpanTokenRe = regexp.MustCompile(`^(\d+)(h|min|s)$`)
)

// TestHoldersRollupUnitTimeoutCoversStatementCeilings pins that systemd
// cannot kill a holders-rollup cycle ClickHouse would still let finish: the
// unit's TimeoutStartSec must be at least the serial sum of every statement's
// max_execution_time. Otherwise a slow-but-legal cycle is SIGTERMed before
// its EXCHANGE on every run and the board never refreshes.
func TestHoldersRollupUnitTimeoutCoversStatementCeilings(t *testing.T) {
	var ceiling time.Duration
	for i, stmt := range holdersRollupStatements(time.Now()) {
		m := maxExecutionTimeRe.FindAllStringSubmatch(stmt, -1)
		if len(m) == 0 {
			// Metadata-only statements: no scan to bound.
			head := strings.TrimSpace(stmt)
			if strings.HasPrefix(head, "TRUNCATE TABLE ") || strings.HasPrefix(head, "EXCHANGE TABLES ") {
				continue
			}
			t.Fatalf("holders rollup statement %d carries no max_execution_time, so the cycle has no computable ceiling:\n%s", i+1, stmt)
		}
		if len(m) > 1 {
			t.Fatalf("holders rollup statement %d carries %d max_execution_time settings, want one:\n%s", i+1, len(m), stmt)
		}
		secs, err := strconv.Atoi(m[0][1])
		if err != nil {
			t.Fatalf("statement %d: parse max_execution_time %q: %v", i+1, m[0][1], err)
		}
		ceiling += time.Duration(secs) * time.Second
	}
	if ceiling == 0 {
		t.Fatal("no capped statement found — this test would pass vacuously")
	}

	unit := filepath.Join(systemdRepoRoot(t), "configs", "ansible", "roles", "archival-node",
		"templates", "systemd", "holders-rollup.service.j2")
	body, err := os.ReadFile(unit)
	if err != nil {
		t.Fatalf("read %s: %v", unit, err)
	}
	m := timeoutStartSecRe.FindAllSubmatch(body, -1)
	if len(m) != 1 {
		t.Fatalf("%s: want exactly one TimeoutStartSec= line, found %d", unit, len(m))
	}
	timeout := parseSystemdSpan(t, strings.TrimSpace(string(m[0][1])))

	if timeout < ceiling {
		t.Fatalf("holders-rollup.service TimeoutStartSec=%s is below the cycle's summed max_execution_time %s: systemd kills a cycle ClickHouse would still let finish",
			timeout, ceiling)
	}
}

// parseSystemdSpan parses the subset of systemd.time(7) spans the unit
// templates use: space-separated <n>h / <n>min / <n>s tokens.
func parseSystemdSpan(t *testing.T, span string) time.Duration {
	t.Helper()
	units := map[string]time.Duration{"h": time.Hour, "min": time.Minute, "s": time.Second}
	var d time.Duration
	fields := strings.Fields(span)
	if len(fields) == 0 {
		t.Fatalf("empty systemd time span")
	}
	for _, tok := range fields {
		m := systemdSpanTokenRe.FindStringSubmatch(tok)
		if m == nil {
			t.Fatalf("unsupported systemd time span token %q in %q", tok, span)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("parse %q: %v", tok, err)
		}
		d += time.Duration(n) * units[m[2]]
	}
	return d
}
