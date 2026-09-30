// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"regexp"
	"strings"
	"testing"
)

const (
	slaProbeWrapper = "configs/healthchecks/sla-probe.sh"
	slaProbeDoc     = "docs/operations/sla-probe.md"
)

// TestSLAProbeDocKnobsAreReadByWrapper pins the ops doc's override block
// to the wrapper: an operator setting a documented knob the wrapper never
// reads changes nothing, and a stale "# default" misstates what runs.
func TestSLAProbeDocKnobsAreReadByWrapper(t *testing.T) {
	wrapper := readRepoFile(t, slaProbeWrapper)
	doc := readRepoFile(t, slaProbeDoc)

	_, after, ok := strings.Cut(doc, "Override defaults via `/etc/default/stellarindex-healthchecks`")
	if !ok {
		t.Fatalf("%s no longer has the 'Override defaults via' block", slaProbeDoc)
	}
	block := regexp.MustCompile("(?s)```sh\n(.*?)```").FindStringSubmatch(after)
	if block == nil {
		t.Fatalf("%s: no ```sh block follows 'Override defaults via'", slaProbeDoc)
	}

	assign := regexp.MustCompile(`^([A-Z_][A-Z0-9_]*)=(\S*)\s*(#.*)?$`)
	seen := 0
	for _, line := range strings.Split(block[1], "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := assign.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("%s override block: unparseable line %q", slaProbeDoc, line)
			continue
		}
		seen++
		name, value, comment := m[1], m[2], m[3]
		// Every environment knob is read as ${NAME:-…}; a bare ${NAME} could be a wrapper-local.
		if !strings.Contains(wrapper, "${"+name+":-") {
			t.Errorf("%s documents %s, but %s never reads it", slaProbeDoc, name, slaProbeWrapper)
			continue
		}
		if !strings.HasPrefix(comment, "# default") {
			continue
		}
		dm := regexp.MustCompile(`\$\{` + name + `:-([^}]*)\}`).FindStringSubmatch(wrapper)
		if dm == nil {
			t.Errorf("%s marks %s=%s as the default, but %s sets no default for it", slaProbeDoc, name, value, slaProbeWrapper)
			continue
		}
		if dm[1] != value {
			t.Errorf("%s says %s defaults to %q; %s defaults it to %q", slaProbeDoc, name, value, slaProbeWrapper, dm[1])
		}
	}
	if seen == 0 {
		t.Fatalf("%s override block lists no variables", slaProbeDoc)
	}
}
