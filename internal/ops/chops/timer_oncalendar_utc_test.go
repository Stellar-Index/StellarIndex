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

// systemd resolves OnCalendar= against the HOST's timezone, so every fixed-clock timer pins an explicit ` UTC` token to
// avoid drift with timezone or DST and keep jobs inside the shared 02:00-06:20 UTC backup/rollup window. Walks every
// *.timer.j2 so a new timer without the suffix fails.
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
			// A shorthand hour:minute repeater (e.g. "*:0/15") fires at the same relative cadence in any zone, so it is not the
			// fixed-wall-clock hazard this test targets and is excluded.
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
