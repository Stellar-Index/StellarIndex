// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package archive

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// TestArchiveCompletenessFix_NothingMissingIgnoresWriteGate: a complete range returns cleanly regardless of -write.
func TestArchiveCompletenessFix_NothingMissingIgnoresWriteGate(t *testing.T) {
	root := t.TempDir()
	// seq=63 -> ledger/00/00/00/ledger-0000003f.xdr.gz
	cpPath := filepath.Join(root, "ledger", "00", "00", "00", "ledger-0000003f.xdr.gz")
	if err := os.MkdirAll(filepath.Dir(cpPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cpPath, gzipBytes(t, "checkpoint fixture"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Run([]string{
		"archive-completeness", "fix",
		"-archive-root", root,
		"-from", "2", "-to", "63",
	})
	if err != nil {
		t.Fatalf("err = %v, want nil — the range is already complete", err)
	}
}

func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func treeEntries(t *testing.T, root string) []string {
	t.Helper()
	var got []string
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root || d.IsDir() {
			return nil
		}
		got = append(got, path)
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return got
}

// TestArchiveCompleteness_RejectsLedgerBeyondUint32: an oversized -to must be refused, not wrapped into a different (possibly vacuous) range, since ledgers are uint32.
func TestArchiveCompleteness_RejectsLedgerBeyondUint32(t *testing.T) {
	over := strconv.FormatUint(math.MaxUint32+1, 10)
	cases := []struct {
		name     string
		from, to string
	}{
		{"to-over", "2", over},
		{"from-and-to-over", over, over},
	}
	for _, mode := range []string{"check", "fix", "verify"} {
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				root := t.TempDir()
				err := Run([]string{
					"archive-completeness", mode,
					"-archive-root", root,
					"-from", tc.from, "-to", tc.to,
				})
				if err == nil || !strings.Contains(err.Error(), "exceeds the maximum ledger sequence") {
					t.Fatalf("err = %v, want a -to range error", err)
				}
				if entries := treeEntries(t, root); len(entries) != 0 {
					t.Errorf("archive root gained entries %v on a refused range", entries)
				}
			})
		}
	}
}

func TestParseArchiveCompletenessVerifyFlags_AcceptsMaxUint32(t *testing.T) {
	maxLedger := strconv.FormatUint(math.MaxUint32, 10)
	opts, err := parseArchiveCompletenessVerifyFlags([]string{"-from", maxLedger, "-to", maxLedger})
	if err != nil {
		t.Fatalf("err = %v, want nil at the uint32 boundary", err)
	}
	if opts.from != math.MaxUint32 || opts.to != math.MaxUint32 {
		t.Errorf("from/to = %d/%d, want %d/%d", opts.from, opts.to, uint32(math.MaxUint32), uint32(math.MaxUint32))
	}
}

// archiveCompletenessUnitFiles are the units that run `archive-completeness
// verify`: the ansible template is what r1 runs, the deploy/ copy is the
// operator-facing reference for self-hosters, so both must parse.
var archiveCompletenessUnitFiles = []string{
	"configs/ansible/roles/archival-node/templates/systemd/archive-completeness.service.j2",
	"deploy/systemd/archive-completeness.service",
}

// TestArchiveCompletenessUnits_NoDeadArchiveToDefault: neither unit may ship a non-zero ARCHIVE_TO default; the binary refuses 0 and a literal Environment= is always overridden by EnvironmentFile=, so it can only mislead.
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

// TestArchiveCompletenessUnits_ExecStartParses runs each unit's `archive-completeness verify` invocation through the real FlagSet; an unknown flag or ARCHIVE_TO=0 would fail the timer with only `failed` to show.
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

// TestArchiveCompleteness_DryRunByDefault pins that without -write both fix and verify (the mode the
// timer runs) report the residual-missing exit and touch nothing.
func TestArchiveCompleteness_DryRunByDefault(t *testing.T) {
	for _, mode := range []string{"fix", "verify"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			err := Run([]string{
				"archive-completeness", mode,
				"-archive-root", root,
				"-from", "2", "-to", "63", // exactly one checkpoint position (seq=63)
			})
			if !errors.Is(err, opsutil.ErrExitSilently) {
				t.Fatalf("err = %v, want opsutil.ErrExitSilently (1 residual missing checkpoint)", err)
			}
			if entries := treeEntries(t, root); len(entries) != 0 {
				t.Errorf("archive root gained %d entries without -write: %v", len(entries), entries)
			}
		})
	}
}
