package archive

import (
	"slices"
	"strings"
	"testing"
)

// trimUnitFiles are the unit definitions that run trim-galexie-archive on a
// timer: the ansible template is what r1 runs, the deploy/ copy is the
// operator-facing reference, so both must parse.
var trimUnitFiles = []string{
	"configs/ansible/roles/archival-node/templates/systemd/galexie-archive-trim.service.j2",
	"deploy/systemd/galexie-archive-trim.service",
}

// TestTrimUnits_ExecStartParses runs each scheduled unit's argv through the subcommand's own FlagSet, since an undefined flag exits 1 and a monthly timer reports only "failed". The unit must also be a real run: without -commit it deletes nothing, and it must keep upstream verification on.
func TestTrimUnits_ExecStartParses(t *testing.T) {
	t.Parallel()
	for _, rel := range trimUnitFiles {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			argv := unitExecStartArgv(readRepoFile(t, rel))
			i := slices.Index(argv, "trim-galexie-archive")
			if i < 0 {
				t.Fatalf("%s: ExecStart never invokes trim-galexie-archive (argv=%v)", rel, argv)
			}
			args := slices.Clone(argv[i+1:])
			for j, a := range args {
				// Stands in for the cutoff compute-trim-cutoff.sh writes.
				args[j] = strings.ReplaceAll(a, "${TRIM_CUTOFF}", "50000000")
			}
			opts, err := parseTrimFlags(args)
			if err != nil {
				t.Fatalf("%s: trim-galexie-archive rejects the unit's argv %v: %v", rel, args, err)
			}
			if opts.olderThan != 50000000 {
				t.Errorf("%s: -older-than-ledger did not carry ${TRIM_CUTOFF}: got %d", rel, opts.olderThan)
			}
			if !opts.commit {
				t.Errorf("%s: unit omits -commit, so every fire is a dry-run that deletes nothing", rel)
			}
			if !opts.verifyUpstream {
				t.Errorf("%s: unit disables upstream verification on a scheduled destructive run", rel)
			}
		})
	}
}

// TestTrimUnits_UsesDeleteCapableIdentity pins that the trim unit loads a delete-capable credential file (stellarindex-archive-trimmer) AFTER stellarindex-ops, because the read-only reader identity cannot s3:DeleteObject and every delete would AccessDenied silently. Later EnvironmentFile wins in systemd, so this checks position as well as presence.
func TestTrimUnits_UsesDeleteCapableIdentity(t *testing.T) {
	t.Parallel()
	for _, rel := range trimUnitFiles {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			lines := environmentFileLines(readRepoFile(t, rel))
			opsIdx := slices.Index(lines, "-/etc/default/stellarindex-ops")
			if opsIdx < 0 {
				t.Fatalf("%s: no EnvironmentFile for /etc/default/stellarindex-ops (lines=%v)", rel, lines)
			}
			trimIdx := slices.Index(lines, "-/etc/default/galexie-archive-trim")
			if trimIdx < 0 {
				t.Fatalf("%s: no EnvironmentFile for the delete-capable stellarindex-archive-trimmer identity "+
					"(/etc/default/galexie-archive-trim) — every DeleteObject in trim_galexie_archive.go runs as "+
					"stellarindex-reader (read-only) and returns AccessDenied", rel)
			}
			if trimIdx < opsIdx {
				t.Errorf("%s: /etc/default/galexie-archive-trim (index %d) loads BEFORE "+
					"/etc/default/stellarindex-ops (index %d) — systemd's LATER EnvironmentFile wins, so the "+
					"read-only stellarindex-reader creds would still shadow the trimmer identity", rel, trimIdx, opsIdx)
			}
		})
	}
}

// environmentFileLines extracts the value of every EnvironmentFile=
// directive, in file order, skipping comment lines.
func environmentFileLines(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if v, ok := strings.CutPrefix(trimmed, "EnvironmentFile="); ok {
			out = append(out, v)
		}
	}
	return out
}

// unitExecStartArgv splices a unit's ExecStart= directive, including its
// backslash continuation lines, into one argv. Comment lines are skipped so
// a flag merely discussed in the header does not count.
func unitExecStartArgv(body string) []string {
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
