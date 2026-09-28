package archive

import (
	"slices"
	"strings"
	"testing"
)

// archiveCompletenessUnitFiles are the units that run `archive-completeness
// verify`: the ansible template is what r1 runs, the deploy/ copy is the
// operator-facing reference for self-hosters, so both must parse (#1179).
var archiveCompletenessUnitFiles = []string{
	"configs/ansible/roles/archival-node/templates/systemd/archive-completeness.service.j2",
	"deploy/systemd/archive-completeness.service",
}

// TestArchiveCompletenessUnits_NoDeadArchiveToDefault pins #1179: the
// ansible template shipped `Environment=ARCHIVE_TO=0`, which the binary
// refuses outright ("-to is required") — the exact landmine the deploy/
// reference unit's own comment already documents fixing ("a self-hoster
// who followed the wiring above got a unit that failed on every single
// timer tick"). It is inert today only because EnvironmentFile= always
// overrides a literal Environment= regardless of directive order, so
// compute-archive-to.sh's runtime write wins whenever it runs — but a
// non-zero default can only ever be MISLEADING (an operator inspecting
// or copying the unit sees a value that never actually takes effect)
// and was never load-bearing, so neither unit may ship one.
func TestArchiveCompletenessUnits_NoDeadArchiveToDefault(t *testing.T) {
	t.Parallel()
	for _, rel := range archiveCompletenessUnitFiles {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			for _, line := range strings.Split(readRepoFile(t, rel), "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "Environment=ARCHIVE_TO=") {
					t.Fatalf("%s: ships a literal %q — the binary refuses -to=0, and any "+
						"non-zero literal is dead weight since ARCHIVE_TO must come from "+
						"the live indexer cursor (compute-archive-to.sh)", rel, trimmed)
				}
			}
		})
	}
}

// substituteArchiveCompletenessUnitVars replaces the unit's runtime
// env-var / Jinja placeholders with concrete test values, so the
// resulting ExecStart argv parses through the real FlagSet.
func substituteArchiveCompletenessUnitVars(body string) string {
	r := strings.NewReplacer(
		"${ARCHIVE_FROM}", "2",
		"${ARCHIVE_TO}", "64000000",
		"${WORKERS}", "8",
		"${STELLAR_NETWORK}", "pubnet",
		"${TEXTFILE_OUTPUT}", "/tmp/archive-completeness.prom",
		"${REPORT_OUTPUT}", "/tmp/archive-completeness.json",
		"{{ stellar_network | default('pubnet') }}", "pubnet",
	)
	return r.Replace(body)
}

// TestArchiveCompletenessUnits_ExecStartParses runs each unit's
// `archive-completeness verify` invocation through the real FlagSet
// (#1179, mirrors TestTrimUnits_ExecStartParses). A flag the parser does
// not define, or the ARCHIVE_TO=0 landmine, exits 1 before anything is
// checked — a daily timer reports only `failed`, indistinguishable from
// "nothing to check" to anyone not reading the journal.
func TestArchiveCompletenessUnits_ExecStartParses(t *testing.T) {
	t.Parallel()
	for _, rel := range archiveCompletenessUnitFiles {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			body := substituteArchiveCompletenessUnitVars(readRepoFile(t, rel))
			argv := unitExecStartArgv(body)
			i := slices.Index(argv, "verify")
			if i < 0 || i == 0 || argv[i-1] != "archive-completeness" {
				t.Fatalf("%s: ExecStart never invokes `archive-completeness verify` (argv=%v)", rel, argv)
			}
			opts, err := parseArchiveCompletenessVerifyFlags(argv[i+1:])
			if err != nil {
				t.Fatalf("%s: archive-completeness verify rejects the unit's argv %v: %v", rel, argv[i+1:], err)
			}
			if opts.to == 0 {
				t.Errorf("%s: -to resolved to 0 — ARCHIVE_TO was not substituted or the unit hard-codes it", rel)
			}
			if opts.network != "pubnet" {
				t.Errorf("%s: -network = %q, want pubnet (the cross-anchor fill refuses without a pubnet value)", rel, opts.network)
			}
		})
	}
}
