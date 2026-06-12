package main

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/support/datastore"
)

// TestRehydratePaths_AlignsDownToFileBoundary verifies that a -from
// value sitting mid-file expands down to the file's start boundary
// so the file containing -from is in the rehydrate set.
func TestRehydratePaths_AlignsDownToFileBoundary(t *testing.T) {
	t.Parallel()
	schema := datastore.DataStoreSchema{
		LedgersPerFile:    64,
		FilesPerPartition: 1000,
	}
	// 100 sits in [64, 127] — same file as 64. Expect that file's
	// path included.
	paths := rehydratePaths(schema, 100, 100)
	if len(paths) == 0 {
		t.Fatal("expected at least one path")
	}
	expected := schema.GetObjectKeyFromSequenceNumber(64) // file 64-127
	found := false
	for _, p := range paths {
		if p == expected {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected path for file containing ledger 64 (%q) in result; got %v", expected, paths)
	}
}

// TestRehydratePaths_NoDuplicates verifies that the iteration
// emits each unique file path exactly once across a multi-file
// range. Adjacent ledgers share a file at LedgersPerFile > 1, so
// the dedupe map matters.
func TestRehydratePaths_NoDuplicates(t *testing.T) {
	t.Parallel()
	schema := datastore.DataStoreSchema{
		LedgersPerFile:    64,
		FilesPerPartition: 1000,
	}
	paths := rehydratePaths(schema, 1, 200)
	seen := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		if _, dup := seen[p]; dup {
			t.Errorf("duplicate path emitted: %q", p)
		}
		seen[p] = struct{}{}
	}
	// [1, 200] spans buckets aligned at ledger-per-file=64:
	// [0-63], [64-127], [128-191], [192-255]. Should be 4 unique
	// files (5 if from=0 boundary aliases differently).
	if len(paths) < 3 || len(paths) > 5 {
		t.Errorf("expected ~4 file paths for ledger range [1,200] at LPF=64; got %d (%v)", len(paths), paths)
	}
}

// TestRehydratePaths_HandlesZeroLedgersPerFile guards the
// defensive fallback — a malformed schema would otherwise cause
// an infinite loop. The function must degrade to single-ledger
// stepping (the Galexie default).
func TestRehydratePaths_HandlesZeroLedgersPerFile(t *testing.T) {
	t.Parallel()
	schema := datastore.DataStoreSchema{
		LedgersPerFile:    0, // would otherwise infinite-loop
		FilesPerPartition: 1000,
	}
	paths := rehydratePaths(schema, 10, 12)
	if len(paths) != 3 {
		t.Errorf("expected 3 paths for [10, 12] at LPF=0 fallback (1 per ledger); got %d (%v)", len(paths), paths)
	}
}

// TestRehydratePaths_SingleLedgerFile_DefaultCase mirrors the
// production Galexie shape on r1 (LedgersPerFile=1) — one file
// per ledger.
func TestRehydratePaths_SingleLedgerFile_DefaultCase(t *testing.T) {
	t.Parallel()
	schema := datastore.DataStoreSchema{
		LedgersPerFile:    1,
		FilesPerPartition: 64,
	}
	paths := rehydratePaths(schema, 50000000, 50000010)
	if len(paths) != 11 {
		t.Errorf("expected 11 paths for an 11-ledger range at LPF=1; got %d", len(paths))
	}
}

func TestParseRehydrateFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		args    []string
		wantErr bool
		from    uint32
		to      uint32
		dry     bool
	}{
		{
			name: "all flags set",
			args: []string{"-config", "/tmp/x.toml", "-from", "100", "-to", "200", "-dry-run"},
			from: 100, to: 200, dry: true,
		},
		{
			name: "defaults",
			args: []string{"-from", "1", "-to", "2"},
			from: 1, to: 2, dry: false,
		},
		{
			name:    "from out of range",
			args:    []string{"-from", "-1", "-to", "10"},
			wantErr: true,
		},
		{
			name:    "to out of range",
			args:    []string{"-from", "10", "-to", "9999999999"},
			wantErr: true,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			opts, err := parseRehydrateFlags(c.args)
			if c.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if opts.from != c.from {
				t.Errorf("from=%d want %d", opts.from, c.from)
			}
			if opts.to != c.to {
				t.Errorf("to=%d want %d", opts.to, c.to)
			}
			if opts.dryRun != c.dry {
				t.Errorf("dryRun=%v want %v", opts.dryRun, c.dry)
			}
		})
	}
}
