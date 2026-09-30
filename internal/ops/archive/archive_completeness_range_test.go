// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package archive

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// TestArchiveCompleteness_RejectsLedgerBeyondUint32: -from/-to parse as
// uint but ledger sequences are uint32, so an oversized -to must be refused
// rather than wrapped into a different (possibly vacuous) range.
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
