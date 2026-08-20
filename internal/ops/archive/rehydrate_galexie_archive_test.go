package archive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/stellar/go-stellar-sdk/support/datastore"
)

// fakeRehydrateStore is a DB/S3-free rehydrateStore: canned per-path
// responses so the not-found-vs-transient distinction in
// copyColdToHot is exercisable without live S3/MinIO.
type fakeRehydrateStore struct {
	exists      map[string]bool  // path -> exists (checked only if existsErr[path] unset)
	existsErr   map[string]error // path -> Exists error
	getFileErr  map[string]error // path -> GetFile error
	getFileBody map[string][]byte
	putErr      map[string]error
	putOK       map[string]bool // path -> PutFileIfNotExists ok result (default true)

	putCalls []string // paths PutFileIfNotExists was invoked for
}

func (f *fakeRehydrateStore) Exists(_ context.Context, path string) (bool, error) {
	if err, ok := f.existsErr[path]; ok {
		return false, err
	}
	return f.exists[path], nil
}

func (f *fakeRehydrateStore) GetFile(_ context.Context, path string) (io.ReadCloser, int64, error) {
	if err, ok := f.getFileErr[path]; ok {
		return nil, 0, err
	}
	body := f.getFileBody[path]
	return io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
}

func (f *fakeRehydrateStore) PutFileIfNotExists(_ context.Context, path string, _ io.WriterTo, _ map[string]string) (bool, error) {
	f.putCalls = append(f.putCalls, path)
	if err, ok := f.putErr[path]; ok {
		return false, err
	}
	if ok, has := f.putOK[path]; has {
		return ok, nil
	}
	return true, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

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

// TestRehydrateFiles_TransientColdErrorIsNotMissing is the DAT-09
// regression: a transient cold-tier fault (Exists errors, or GetFile
// fails despite Exists having just confirmed presence) must be
// counted as an error — NOT as "missing" — so the command's
// `errs > 0` non-zero exit actually fires. Silently folding a
// transient fault into "missing" both mislabels a real infra problem
// as a data gap and (previously) still exited 0.
func TestRehydrateFiles_TransientColdErrorIsNotMissing(t *testing.T) {
	hot := &fakeRehydrateStore{exists: map[string]bool{}} // nothing in hot
	cold := &fakeRehydrateStore{
		existsErr: map[string]error{"a": errors.New("dial tcp: connection refused")},
	}
	skipped, copied, missing, errs, err := rehydrateFiles(context.Background(), discardLogger(), hot, cold, []string{"a"}, false)
	if err != nil {
		t.Fatalf("rehydrateFiles: %v", err)
	}
	if missing != 0 {
		t.Fatalf("transient cold.Exists error must NOT be counted as missing, got missing=%d", missing)
	}
	if errs != 1 {
		t.Fatalf("transient cold.Exists error must be counted as an error, got errs=%d", errs)
	}
	if skipped != 0 || copied != 0 {
		t.Fatalf("unexpected skipped=%d copied=%d", skipped, copied)
	}
}

// TestRehydrateFiles_GenuineGapIsMissing: cold.Exists definitively
// returning false (no error) is the ONLY path that should increment
// missing.
func TestRehydrateFiles_GenuineGapIsMissing(t *testing.T) {
	hot := &fakeRehydrateStore{exists: map[string]bool{}}
	cold := &fakeRehydrateStore{exists: map[string]bool{"a": false}}
	_, _, missing, errs, err := rehydrateFiles(context.Background(), discardLogger(), hot, cold, []string{"a"}, false)
	if err != nil {
		t.Fatalf("rehydrateFiles: %v", err)
	}
	if missing != 1 {
		t.Fatalf("genuine cold absence must be counted as missing, got missing=%d", missing)
	}
	if errs != 0 {
		t.Fatalf("genuine gap must not be counted as an error, got errs=%d", errs)
	}
}

// TestRehydrateFiles_GetFileErrorAfterExistsIsError: cold.Exists
// confirms presence but the subsequent GetFile still fails — a
// transient read fault on a file that IS there, not a gap.
func TestRehydrateFiles_GetFileErrorAfterExistsIsError(t *testing.T) {
	hot := &fakeRehydrateStore{exists: map[string]bool{}}
	cold := &fakeRehydrateStore{
		exists:     map[string]bool{"a": true},
		getFileErr: map[string]error{"a": errors.New("i/o timeout")},
	}
	_, _, missing, errs, err := rehydrateFiles(context.Background(), discardLogger(), hot, cold, []string{"a"}, false)
	if err != nil {
		t.Fatalf("rehydrateFiles: %v", err)
	}
	if missing != 0 {
		t.Fatalf("a GetFile error after Exists confirmed presence must NOT be counted as missing, got missing=%d", missing)
	}
	if errs != 1 {
		t.Fatalf("expected errs=1, got errs=%d", errs)
	}
}

// TestRehydrateFiles_HappyPathCopies: the successful copy path still
// works end to end (regression guard against over-correcting into
// always-error).
func TestRehydrateFiles_HappyPathCopies(t *testing.T) {
	hot := &fakeRehydrateStore{exists: map[string]bool{}}
	cold := &fakeRehydrateStore{
		exists:      map[string]bool{"a": true},
		getFileBody: map[string][]byte{"a": []byte("lcm-bytes")},
	}
	skipped, copied, missing, errs, err := rehydrateFiles(context.Background(), discardLogger(), hot, cold, []string{"a"}, false)
	if err != nil {
		t.Fatalf("rehydrateFiles: %v", err)
	}
	if copied != 1 || skipped != 0 || missing != 0 || errs != 0 {
		t.Fatalf("copied=%d skipped=%d missing=%d errs=%d, want copied=1 rest=0", copied, skipped, missing, errs)
	}
	if len(cold.putCalls) != 0 { // put happens on hot, not cold
		t.Fatalf("unexpected calls on cold: %v", cold.putCalls)
	}
}

// TestRehydrateFiles_AlreadyInHotIsSkipped: hot.Exists=true short-
// circuits before ever touching cold.
func TestRehydrateFiles_AlreadyInHotIsSkipped(t *testing.T) {
	hot := &fakeRehydrateStore{exists: map[string]bool{"a": true}}
	cold := &fakeRehydrateStore{}
	skipped, copied, missing, errs, err := rehydrateFiles(context.Background(), discardLogger(), hot, cold, []string{"a"}, false)
	if err != nil {
		t.Fatalf("rehydrateFiles: %v", err)
	}
	if skipped != 1 || copied != 0 || missing != 0 || errs != 0 {
		t.Fatalf("skipped=%d copied=%d missing=%d errs=%d, want skipped=1 rest=0", skipped, copied, missing, errs)
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
			// Fail-closed write-gate (W8.15c): -write opts into writing to
			// hot, so dryRun is false.
			name: "write opts in",
			args: []string{"-config", "/tmp/x.toml", "-from", "100", "-to", "200", "-write"},
			from: 100, to: 200, dry: false,
		},
		{
			// The DEFAULT is now a fail-closed dry run — no -write means no
			// writes, the reversal of the old default-WRITE convention.
			name: "defaults are a fail-closed dry run",
			args: []string{"-from", "1", "-to", "2"},
			from: 1, to: 2, dry: true,
		},
		{
			// -dry-run is retained as an explicit no-op alias (dry run is
			// already the default) so existing callers keep working.
			name: "dry-run is a no-op alias",
			args: []string{"-from", "1", "-to", "2", "-dry-run"},
			from: 1, to: 2, dry: true,
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
