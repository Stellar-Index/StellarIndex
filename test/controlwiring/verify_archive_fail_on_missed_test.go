package controlwiring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── F144: -fail-on-missed on the units where it can fire ──────────
//
// verify-archive consumes -fail-on-missed ONLY inside the checkpoint
// tier (internal/ops/archive/verify_archive.go: the flag is read by
// checkpointAnchorDecision, reached only `if doCheckpoint`, and
// doCheckpoint is `-tier checkpoint|all`). The tier-a units run
// `-tier chain`, where the flag is inert — adding it there would
// state an invariant the binary never evaluates. The control has to
// be wired on the units that engage the checkpoint tier: tier-b, in
// BOTH trees (the ansible template is r1's authority; deploy/systemd
// is the operator-facing reference copy).
//
// GRADUATED out of the k023evidence build tag (it lived in
// deployed_controls_test.go) once the flag could actually be turned
// on. It could not before: the walk runs to the galexie bucket's tip
// while the cross-anchor mirror is filled by its own daily job, so the
// trailing checkpoints were counted as missing archive data and the
// flag would have failed the unit every night. Measured on r1
// 2026-09-19, `matched=325 missed=23` with all 23 above the mirror's
// high-water and not one hole below it. The binary now separates the
// two (archiveMirrorCoverage), so `missed` means a hole inside the
// mirror's own coverage and the flag reads what ADR-0017 contract 3
// says it reads.
//
// The assertion reads the argv that reaches the BINARY rather than
// scanning the file's text, because r1's unit runs through the
// run-heavy-job.sh singleton wrapper and the wrapper's own argv prefix
// sits in front of `stellarindex-ops verify-archive`. A flag placed
// among the wrapper's leading arguments is consumed by the wrapper and
// never parsed by the binary.
var verifyArchiveUnits = []string{
	"configs/ansible/roles/archival-node/templates/systemd/verify-archive-tier-a.service.j2",
	"configs/ansible/roles/archival-node/templates/systemd/verify-archive-tier-b.service.j2",
	"deploy/systemd/verify-archive-tier-a.service",
	"deploy/systemd/verify-archive-tier-b.service",
}

func TestK023_VerifyArchiveCheckpointUnitsFailOnMissed(t *testing.T) {
	t.Parallel()
	checkpointUnits := 0
	for _, rel := range verifyArchiveUnits {
		args := verifyArchiveBinaryArgs(t, rel, readRepoFile(t, rel))
		tier, ok := flagValue(args, "-tier")
		if !ok {
			t.Fatalf("%s: ExecStart passes no -tier to stellarindex-ops verify-archive", rel)
		}
		if tier != "checkpoint" && tier != "all" {
			// chain / peers / archivist never reach the
			// checkpoint-anchor decision; the flag is inert there.
			continue
		}
		checkpointUnits++
		if !hasFlag(args, "-fail-on-missed") {
			t.Errorf("%s runs `-tier %s` without -fail-on-missed: a cross-anchor checkpoint "+
				"missing from inside the mirror's own coverage exits 0, so ADR-0017 X1.7's "+
				"\"hard invariant\" is a soft tolerance on the deployed path (F144)", rel, tier)
		}
	}
	if checkpointUnits == 0 {
		t.Fatal("no verify-archive unit engages the checkpoint tier — this test is asserting nothing")
	}
}

// TestVerifyArchiveTierBRunbook_ManualRerunFailsOnMissed holds the
// tier-b runbook's manual re-run commands to the nightly units'
// strictness. The mitigation hands an on-call operator a command for
// exactly the case where checkpoints were missed; without
// -fail-on-missed, checkpointAnchorDecision (verify_archive.go)
// accepts a partial miss (some matched, some missed) inside the
// mirror's own coverage span, so the run exits 0 and prints
// "checkpoint anchor OK" while an in-coverage hole goes unreported.
// Every checkpoint-tier invocation in the runbook is checked, so a
// command added later cannot drop the flag either.
func TestVerifyArchiveTierBRunbook_ManualRerunFailsOnMissed(t *testing.T) {
	t.Parallel()
	const rel = "docs/operations/runbooks/verify-archive-tier-b.md"
	checkpointCmds := 0
	for _, argv := range verifyArchiveRunbookCommands(readRepoFile(t, rel)) {
		tier, ok := flagValue(argv, "-tier")
		if !ok || (tier != "checkpoint" && tier != "all") {
			continue
		}
		checkpointCmds++
		if !hasFlag(argv, "-fail-on-missed") {
			t.Errorf("%s: manual `verify-archive -tier %s` omits -fail-on-missed: a partial miss "+
				"inside the mirror's own coverage exits 0 and reports the checkpoint anchor OK, "+
				"so the in-coverage hole goes unreported (argv=%v)", rel, tier, argv)
		}
	}
	if checkpointCmds == 0 {
		t.Fatalf("%s: no manual `stellarindex-ops verify-archive -tier checkpoint` command found "+
			"— this test is asserting nothing", rel)
	}
}

// verifyArchiveRunbookCommands returns the argv after
// `stellarindex-ops verify-archive` of every runbook line that starts
// that invocation, following backslash continuations. Prose that
// mentions the command inline does not start a line with it.
func verifyArchiveRunbookCommands(body string) [][]string {
	var cmds [][]string
	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		fields := strings.Fields(strings.TrimSuffix(line, "\\"))
		if len(fields) < 2 || fields[1] != "verify-archive" ||
			(fields[0] != "stellarindex-ops" && !strings.HasSuffix(fields[0], "/stellarindex-ops")) {
			continue
		}
		argv := fields[2:]
		for strings.HasSuffix(line, "\\") && i+1 < len(lines) {
			i++
			line = strings.TrimSpace(lines[i])
			argv = append(argv, strings.Fields(strings.TrimSuffix(line, "\\"))...)
		}
		cmds = append(cmds, argv)
	}
	return cmds
}

// TestVerifyArchiveBinaryArgs_WrapperPrefixIsNotTheBinary pins the
// property a text-scan matcher lacks, on synthetic ExecStart lines so
// it needs no unit file: a flag sitting among run-heavy-job.sh's
// leading arguments, or quoted inside a comment or an Environment=
// line, is NOT wiring — only argv after `stellarindex-ops
// verify-archive` is.
func TestVerifyArchiveBinaryArgs_WrapperPrefixIsNotTheBinary(t *testing.T) {
	t.Parallel()
	const wrapper = "/usr/local/bin/run-heavy-job.sh verify-archive /usr/local/bin/stellarindex-ops verify-archive"
	for _, tc := range []struct {
		name  string
		body  string
		wired bool
	}{
		{
			name:  "after the binary is wiring",
			body:  "ExecStart=" + wrapper + " -tier checkpoint -fail-on-missed\n",
			wired: true,
		},
		{
			name:  "eaten by the wrapper prefix is not wiring",
			body:  "ExecStart=/usr/local/bin/run-heavy-job.sh -fail-on-missed verify-archive /usr/local/bin/stellarindex-ops verify-archive -tier checkpoint\n",
			wired: false,
		},
		{
			name:  "a mention in the header is not wiring",
			body:  "# consider -fail-on-missed here\nExecStart=" + wrapper + " -tier checkpoint\n",
			wired: false,
		},
		{
			name:  "an Environment= line is not wiring",
			body:  "Environment=EXTRA_ARGS=-fail-on-missed\nExecStart=" + wrapper + " -tier checkpoint\n",
			wired: false,
		},
		{
			name:  "a backslash continuation still reaches the binary",
			body:  "ExecStart=" + wrapper + " \\\n  -tier checkpoint \\\n  -fail-on-missed\n",
			wired: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := verifyArchiveBinaryArgs(t, tc.name, tc.body)
			if got := hasFlag(args, "-fail-on-missed"); got != tc.wired {
				t.Errorf("hasFlag(-fail-on-missed) = %v, want %v (argv reaching the binary = %v)", got, tc.wired, args)
			}
			if tier, ok := flagValue(args, "-tier"); !ok || tier != "checkpoint" {
				t.Errorf("flagValue(-tier) = %q,%v, want \"checkpoint\",true (argv = %v)", tier, ok, args)
			}
		})
	}
}

// verifyArchiveBinaryArgs returns the argv the systemd unit hands to
// `stellarindex-ops verify-archive`, with any wrapper prefix
// (run-heavy-job.sh and its job name) stripped.
func verifyArchiveBinaryArgs(t *testing.T, rel, body string) []string {
	t.Helper()
	argv := execStartArgv(body)
	for i, tok := range argv {
		if tok != "stellarindex-ops" && !strings.HasSuffix(tok, "/stellarindex-ops") {
			continue
		}
		if i+1 < len(argv) && argv[i+1] == "verify-archive" {
			return argv[i+2:]
		}
	}
	t.Fatalf("%s: ExecStart never invokes `stellarindex-ops verify-archive` (argv=%v)", rel, argv)
	return nil
}

// execStartArgv splices a unit's ExecStart= directive — including its
// backslash continuation lines — into one argv. Comment lines are
// ignored, so a flag merely DISCUSSED in the unit's header does not
// count as wired.
func execStartArgv(body string) []string {
	var argv []string
	continued := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		switch {
		case continued:
		case strings.HasPrefix(trimmed, "ExecStart="):
			trimmed = strings.TrimPrefix(trimmed, "ExecStart=")
		default:
			continue
		}
		continued = strings.HasSuffix(trimmed, "\\")
		argv = append(argv, strings.Fields(strings.TrimSuffix(trimmed, "\\"))...)
	}
	return argv
}

// hasFlag reports whether flag appears as its own argv element.
func hasFlag(argv []string, flag string) bool {
	for _, a := range argv {
		if a == flag {
			return true
		}
	}
	return false
}

// flagValue returns the argument following flag in argv.
func flagValue(argv []string, flag string) (string, bool) {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}

// readRepoFile lives here rather than in the build-tagged
// deployed_controls_test.go so the graduated legs can use it in the
// default suite; the tagged file compiles alongside this one.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel)) //nolint:gosec // repo-relative, test-only
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}
