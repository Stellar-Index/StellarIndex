package archive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

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

// TestRehydratePaths_AlignsDownToFileBoundary: a mid-file -from expands down to its file's start boundary.
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

// TestRehydratePaths_NoDuplicates: each file path is emitted once across a multi-file range, since adjacent ledgers share a file.
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

// TestRehydratePaths_HandlesZeroLedgersPerFile: a malformed zero schema degrades to single-ledger stepping instead of looping forever.
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

// TestRehydratePaths_SingleLedgerFile_DefaultCase mirrors the r1 Galexie shape, one file per ledger.
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

// TestRehydratePaths_TerminatesAtUint32Max pins termination when the step past -to would overflow uint32.
func TestRehydratePaths_TerminatesAtUint32Max(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		lpf, from, to uint32
		want          int
	}{
		{lpf: 1, from: math.MaxUint32, to: math.MaxUint32, want: 1},
		{lpf: 64, from: math.MaxUint32 - 1, to: math.MaxUint32 - 1, want: 1},
		{lpf: 64, from: math.MaxUint32 - 200, to: math.MaxUint32, want: 4},
		{lpf: 64, from: 200, to: 100, want: 0},
	} {
		schema := datastore.DataStoreSchema{LedgersPerFile: tc.lpf, FilesPerPartition: 64}
		done := make(chan []string, 1)
		go func() { done <- rehydratePaths(schema, tc.from, tc.to) }()
		select {
		case paths := <-done:
			if len(paths) != tc.want {
				t.Errorf("LPF=%d to=%d: got %d paths, want %d", tc.lpf, tc.to, len(paths), tc.want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("LPF=%d to=%d: rehydratePaths did not terminate", tc.lpf, tc.to)
		}
	}
}

// TestRehydrateFiles_TransientColdErrorIsNotMissing: a transient cold-tier fault counts as an error, not "missing", so the non-zero exit fires and a real infra problem is not mislabelled a data gap.
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

// TestRehydrateFiles_GenuineGapIsMissing: only a definitive cold.Exists false increments missing.
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

// TestRehydrateFiles_GetFileErrorAfterExistsIsError: GetFile failing after Exists confirmed presence is a transient fault, not a gap.
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

// TestRehydrateFiles_HappyPathCopies: the successful copy path still works end to end.
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

// TestRehydrateFiles_AlreadyInHotIsSkipped: hot.Exists=true short-circuits before touching cold.
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

// TestRehydrateExitError_AllMissingFailsClosed: every path missing in cold (a hot/cold schema mismatch) must exit non-zero.
func TestRehydrateExitError_AllMissingFailsClosed(t *testing.T) {
	err := rehydrateExitError(5, 5, 0)
	if err == nil {
		t.Fatal("expected a non-nil error when every requested path is missing in cold, got nil (exit 0)")
	}
}

// TestRehydrateExitError_PartialMissingIsNotAFailure: partial `missing` is an expected archive gap and must not fail the run.
func TestRehydrateExitError_PartialMissingIsNotAFailure(t *testing.T) {
	err := rehydrateExitError(5, 2, 0)
	if err != nil {
		t.Fatalf("partial missing must not fail the run, got: %v", err)
	}
}

// TestRehydrateExitError_ErrorsAlwaysFailRegardlessOfMissing: errs>0 always fails.
func TestRehydrateExitError_ErrorsAlwaysFailRegardlessOfMissing(t *testing.T) {
	err := rehydrateExitError(5, 0, 1)
	if err == nil {
		t.Fatal("expected a non-nil error when errs > 0, got nil")
	}
}

// TestRehydrateExitError_EmptyRangeIsNotAFailure: an empty range must not trip the all-missing guard via 0==0.
func TestRehydrateExitError_EmptyRangeIsNotAFailure(t *testing.T) {
	err := rehydrateExitError(0, 0, 0)
	if err != nil {
		t.Fatalf("empty range must not fail, got: %v", err)
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
			// Fail-closed write-gate : -write opts into writing to
			// hot, so dryRun is false.
			name: "write opts in",
			args: []string{"-config", "/tmp/x.toml", "-from", "100", "-to", "200", "-write"},
			from: 100, to: 200, dry: false,
		},
		{
			// The DEFAULT is a fail-closed dry run — no -write means no
			// writes, the reverse of a default-WRITE convention.
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
