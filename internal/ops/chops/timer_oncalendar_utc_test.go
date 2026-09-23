// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ─── SL24: pgbackrest-backup.timer.j2 had no UTC suffix ─────────────
//
// systemd resolves an OnCalendar= expression against the unit's
// configured timezone, which defaults to the HOST's local zone, not
// UTC. Every sibling timer in this directory pins an explicit ` UTC`
// token on its fixed-clock OnCalendar= line so the schedule cannot
// drift with the box's timezone or a DST transition; pgbackrest-backup
// was the one exception, so a host provisioned (or later reconfigured)
// with a non-UTC zone would run the daily backup at a different wall
// clock than every other archival-node job planned around a shared
// 02:00-06:20 UTC backup/rollup window.
//
// This walks every *.timer.j2 in the role and asserts the same rule
// the sweep found broken in one file, so the next new timer that
// forgets the suffix fails here instead of drifting silently.
func TestArchivalNodeTimers_OnCalendarPinsUTC(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "configs", "ansible", "roles", "archival-node", "templates", "systemd")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	onCalendar := regexp.MustCompile(`(?m)^OnCalendar=(.+)$`)

	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".timer.j2") {
			continue
		}
		rel := filepath.Join("configs", "ansible", "roles", "archival-node", "templates", "systemd", e.Name())
		body := readRepoFile(t, rel)

		for _, m := range onCalendar.FindAllStringSubmatch(body, -1) {
			value := strings.TrimSpace(m[1])
			// Ansible-variable schedules (e.g. zfs_snapshot_on_calendar)
			// are configured elsewhere and out of scope here.
			if strings.Contains(value, "{{") {
				continue
			}
			// A shorthand hour:minute repeater (e.g. "*:0/15", no seconds
			// field) fires at the same relative cadence regardless of the
			// host's zone; it's not the fixed-wall-clock hazard SL24 is
			// about, so it's excluded rather than folded into this rule.
			if strings.Count(value, ":") < 2 {
				continue
			}
			found++
			if !strings.HasSuffix(value, " UTC") {
				t.Errorf("%s: OnCalendar=%q has no UTC suffix — systemd resolves it against "+
					"the host's local timezone, so this job's wall-clock time drifts across "+
					"hosts and DST while every sibling timer in this directory pins UTC (SL24)",
					rel, value)
			}
		}
	}

	if found == 0 {
		t.Fatal("no fixed-clock OnCalendar= lines found — test fixture or glob is broken")
	}
}
