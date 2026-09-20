// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"strconv"
	"strings"
	"testing"
)

// ─── HO-361: ha-plan.md's `file:line` citations must resolve ────────
//
// docs/architecture/ha-plan.md backs several claims with a `path:line`
// or `path:start-end` citation into ansible/shell source, so a reader
// can check the claim themselves instead of trusting the prose. Two of
// these had drifted: `configs/healthchecks/sla-probe.sh:21` pointed past
// the BASE_URL assignment (now line 27, after comment lines were added
// above it), and the restore-drill-enable citation into
// 18-pgbackrest-backup.yml still said :333-350 after the ClickHouse
// schema-drift task block was inserted earlier in the file and pushed
// it down to :512-537. Neither prose claim was wrong — only the pointer
// — but a stale pointer sends an operator mid-incident to the wrong
// lines, which is the exact failure this doc's own restore-drill
// section (§8) warns about for a broken runbook link.
//
// This pins the citations against the files they name, so the next
// reflow of 18-pgbackrest-backup.yml (or sla-probe.sh) fails this test
// instead of leaving a silently wrong line number in the doc.
func TestHAPlanFileLineCitationsResolve(t *testing.T) {
	doc := readRepoFile(t, "docs/architecture/ha-plan.md")

	cases := []struct {
		name     string
		file     string
		line     int // 1-indexed; start of the cited range
		lineEnd  int // inclusive end; 0 means a single-line citation
		contains string
	}{
		{
			name:     "sla-probe base URL",
			file:     "configs/healthchecks/sla-probe.sh",
			line:     27,
			contains: "http://localhost:3000/v1",
		},
		{
			name:     "restore-drill timer enable task",
			file:     "configs/ansible/roles/archival-node/tasks/18-pgbackrest-backup.yml",
			line:     512,
			lineEnd:  537,
			contains: "name: Enable + start the restore-drill timer",
		},
		{
			name:     "restore-drill timer OnCalendar",
			file:     "configs/ansible/roles/archival-node/templates/systemd/restore-drill.timer.j2",
			line:     27,
			contains: "OnCalendar=Sat *-*-01..07 04:00:00 UTC",
		},
		{
			name:     "repo2 render gate condition",
			file:     "configs/ansible/roles/archival-node/tasks/18-pgbackrest-backup.yml",
			line:     132,
			contains: "pgbackrest_repo2_s3_bucket | default('') != ''",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			citation := tc.file + ":" + strconv.Itoa(tc.line)
			if tc.lineEnd != 0 {
				citation += "-" + strconv.Itoa(tc.lineEnd)
			}
			if !strings.Contains(doc, "`"+citation+"`") {
				t.Fatalf("ha-plan.md no longer cites %q at all — either it was corrected to a different line (update this test alongside it) or the citation was dropped", citation)
			}

			lines := strings.Split(readRepoFile(t, tc.file), "\n")
			end := tc.lineEnd
			if end == 0 {
				end = tc.line
			}
			if tc.line < 1 || end > len(lines) {
				t.Fatalf("%s: cited range %d-%d is out of bounds (file has %d lines)", tc.file, tc.line, end, len(lines))
			}
			block := strings.Join(lines[tc.line-1:end], "\n")
			if !strings.Contains(block, tc.contains) {
				t.Errorf("ha-plan.md cites %s:%d-%d as covering %q, but that range currently reads:\n%s", tc.file, tc.line, end, tc.contains, block)
			}
		})
	}
}
