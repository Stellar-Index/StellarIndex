package archive

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stellar/go-stellar-sdk/support/datastore"
)

// TestParseTrimFlags covers the trim safety gates: HEAD-before-delete, the max-files ceiling, the second acknowledgement for -no-verify-upstream, and no dry-run beside a commit.
func TestParseTrimFlags(t *testing.T) {
	t.Parallel()
	base := []string{"-older-than-ledger", "1000"}
	with := func(extra ...string) []string { return append(slices.Clone(base), extra...) }
	cases := []struct {
		name    string
		args    []string
		wantErr string // substring; empty means the parse must succeed
		check   func(t *testing.T, o trimOpts)
	}{
		{"defaults", base, "", func(t *testing.T, o trimOpts) {
			if o.olderThan != 1000 {
				t.Errorf("olderThan = %d, want 1000", o.olderThan)
			}
			if !o.verifyUpstream {
				t.Errorf("verifyUpstream must default to true (HEAD-before-delete is the primary safety primitive)")
			}
			if o.maxFiles != 100000 {
				t.Errorf("maxFiles default = %d, want 100000", o.maxFiles)
			}
			if o.dryRun || o.commit {
				t.Errorf("neither dryRun nor commit should be set by default (the body promotes dryRun=true post-parse); got dryRun=%v commit=%v", o.dryRun, o.commit)
			}
		}},
		{"no-verify-upstream alone is refused", with("-no-verify-upstream"), "i-have-verified-cold-out-of-band", nil},
		{"no-verify-upstream with ack", with("-no-verify-upstream", "-i-have-verified-cold-out-of-band"), "", func(t *testing.T, o trimOpts) {
			if o.verifyUpstream {
				t.Errorf("-no-verify-upstream should flip verifyUpstream to false")
			}
			if !o.iHaveVerifiedOutOfBand {
				t.Errorf("iHaveVerifiedOutOfBand should be true")
			}
		}},
		{"ack alone is harmless", with("-i-have-verified-cold-out-of-band"), "", func(t *testing.T, o trimOpts) {
			if !o.verifyUpstream {
				t.Errorf("verifyUpstream must still default to true when -no-verify-upstream isn't passed")
			}
		}},
		// -commit is the units' spelling of the shared -write gate; both arm the same delete.
		{"commit arms the delete", with("-commit"), "", func(t *testing.T, o trimOpts) {
			if !o.commit || o.dryRun {
				t.Errorf("commit=%v dryRun=%v, want commit=true dryRun=false", o.commit, o.dryRun)
			}
		}},
		{"write arms the delete", with("-write"), "", func(t *testing.T, o trimOpts) {
			if !o.commit || o.dryRun {
				t.Errorf("commit=%v dryRun=%v, want commit=true dryRun=false", o.commit, o.dryRun)
			}
		}},
		// The shared gate lets -write win over -dry-run; an irreversible
		// DELETE must not, so contradictory flags are refused before any S3 call.
		{"dry-run with commit is refused", with("-dry-run", "-commit"), "mutually exclusive", nil},
		{"dry-run with write is refused", with("-dry-run", "-write"), "mutually exclusive", nil},
		{"cutoff overflow", []string{"-older-than-ledger", "9999999999"}, "uint32 range", nil},
		// -max-files once had only a > 0 check, so a typo could delete the full archive.
		{"max-files 99999999 refused", with("-max-files", "99999999"), "max-files", nil},
		{"max-files 1000001 refused", with("-max-files", "1000001"), "max-files", nil},
		{"max-files 0 refused", with("-max-files", "0"), "max-files", nil},
		{"max-files 1000000 accepted", with("-max-files", "1000000"), "", nil},
		{"max-files 100000 accepted", with("-max-files", "100000"), "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts, err := parseTrimFlags(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if tc.check != nil {
				tc.check(t, opts)
			}
		})
	}
}

func TestSplitBucketPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		bucket string
		prefix string
	}{
		{"galexie-archive", "galexie-archive", ""},
		{"aws-public-blockchain/v1.1/stellar/ledgers/pubnet", "aws-public-blockchain", "v1.1/stellar/ledgers/pubnet"},
		{"my-bucket/a/b/c", "my-bucket", "a/b/c"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			b, p, err := splitBucketPath(c.in)
			if err != nil {
				t.Fatalf("split %q: %v", c.in, err)
			}
			if b != c.bucket {
				t.Errorf("bucket = %q, want %q", b, c.bucket)
			}
			if p != c.prefix {
				t.Errorf("prefix = %q, want %q", p, c.prefix)
			}
		})
	}
}

func TestMinMaxInt(t *testing.T) {
	t.Parallel()
	if minInt(3, 5) != 3 {
		t.Error("minInt(3,5) != 3")
	}
	if minInt(7, 5) != 5 {
		t.Error("minInt(7,5) != 5")
	}
	if maxInt(3, 5) != 5 {
		t.Error("maxInt(3,5) != 5")
	}
	if maxInt(7, 5) != 7 {
		t.Error("maxInt(7,5) != 7")
	}
}

// Regression "trim can never trim": one unpaged ListFilePaths call is clamped by the SDK to 1000 keys, and Galexie's names embed MaxUint32-startLedger,
// so those were always the NEWEST 1000 (all above the cutoff; 999 skipped, not 1000, since .config.json is rejected by ParseRangeFromObjectKey).
// The fakes reproduce the 1000-key clamp and newest-first ordering.

// sdkListFilePathsMaxLimit mirrors the SDK's unexported
// listFilePathsMaxLimit. If the SDK ever raises it, these tests still
// describe the contract this code was written against.
const sdkListFilePathsMaxLimit = 1000

// fakeHotArchive is a keyspace-only stand-in for the galexie-archive MinIO bucket, implementing trimFileLister
// (SDK DataStore slice) and s3PrefixLister (raw delimited listing for partition discovery).
type fakeHotArchive struct {
	keys      []string // sorted ascending, exactly as S3 returns them
	keyPrefix string   // bucket key prefix ("" for galexie-archive)

	listFileCalls   int
	listObjectCalls int
	listFileErr     error
}

// newFakeArchive builds a fake bucket holding one object per ledger in
// [0, ledgers), laid out by the SDK's own schema so the key format
// cannot drift from what Galexie actually writes.
func newFakeArchive(t *testing.T, filesPerPartition, ledgers uint32) *fakeHotArchive {
	t.Helper()
	schema := datastore.DataStoreSchema{
		LedgersPerFile:    1,
		FilesPerPartition: filesPerPartition,
		FileExtension:     "zst",
	}
	f := &fakeHotArchive{keys: []string{".config.json"}}
	for seq := uint32(0); seq < ledgers; seq++ {
		f.keys = append(f.keys, schema.GetObjectKeyFromSequenceNumber(seq))
	}
	slices.Sort(f.keys)
	return f
}

// partitionsOf returns what listHotPartitions would return for this
// fake, so plan-level tests exercise the real discovery code rather
// than a hand-written partition list.
func (f *fakeHotArchive) partitionsOf(t *testing.T) []hotPartition {
	t.Helper()
	parts, _, err := listHotPartitions(context.Background(), f, "galexie-archive", f.keyPrefix)
	if err != nil {
		t.Fatalf("listHotPartitions: %v", err)
	}
	return parts
}

// ListFilePaths reproduces support/datastore/s3.go: Limit<=0 or >1000 clamps to 1000, prefix-filtered, StartAfter-exclusive, ascending.
func (f *fakeHotArchive) ListFilePaths(_ context.Context, o datastore.ListFileOptions) ([]string, error) {
	f.listFileCalls++
	if f.listFileErr != nil {
		return nil, f.listFileErr
	}
	limit := int(o.Limit)
	if limit <= 0 || limit > sdkListFilePathsMaxLimit {
		limit = sdkListFilePathsMaxLimit
	}
	prefix := o.Prefix
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	// f.keys are key-relative, matching what the SDK hands back after
	// trimming the datastore's own bucket prefix; Prefix and StartAfter
	// are relative for the same reason.
	var out []string
	for _, k := range f.keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if o.StartAfter != "" && k <= o.StartAfter {
			continue
		}
		out = append(out, k)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// ListObjectsV2 reproduces the delimited-listing semantics discovery needs: CommonPrefixes roll-up, MaxKeys counting prefixes, continuation tokens.
func (f *fakeHotArchive) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.listObjectCalls++
	prefix := aws.ToString(in.Prefix)
	delim := aws.ToString(in.Delimiter)

	type entry struct {
		name     string
		isPrefix bool
	}
	var entries []entry
	seen := map[string]bool{}
	for _, k := range f.keys {
		full := f.keyPrefix + k
		if !strings.HasPrefix(full, prefix) {
			continue
		}
		rest := strings.TrimPrefix(full, prefix)
		if i := strings.Index(rest, delim); delim != "" && i >= 0 {
			cp := prefix + rest[:i+len(delim)]
			if !seen[cp] {
				seen[cp] = true
				entries = append(entries, entry{name: cp, isPrefix: true})
			}
			continue
		}
		entries = append(entries, entry{name: full})
	}

	start := 0
	if tok := aws.ToString(in.ContinuationToken); tok != "" {
		n, err := strconv.Atoi(tok)
		if err != nil {
			return nil, fmt.Errorf("fake: bad continuation token %q", tok)
		}
		start = n
	}
	maxKeys := int(aws.ToInt32(in.MaxKeys))
	if maxKeys <= 0 || maxKeys > sdkListFilePathsMaxLimit {
		maxKeys = sdkListFilePathsMaxLimit
	}
	end := min(start+maxKeys, len(entries))

	out := &s3.ListObjectsV2Output{}
	for _, e := range entries[start:end] {
		if e.isPrefix {
			out.CommonPrefixes = append(out.CommonPrefixes, types.CommonPrefix{Prefix: aws.String(e.name)})
			continue
		}
		out.Contents = append(out.Contents, types.Object{Key: aws.String(e.name)})
	}
	if end < len(entries) {
		out.IsTruncated = aws.Bool(true)
		out.NextContinuationToken = aws.String(strconv.Itoa(end))
	}
	return out, nil
}

// fakeCold answers the --verify-upstream HEAD. Default: cold holds
// everything (the healthy production case).
type fakeCold struct {
	missing map[string]bool
	errOn   map[string]error
	calls   int
}

func (c *fakeCold) Exists(_ context.Context, path string) (bool, error) {
	c.calls++
	if err, ok := c.errOn[path]; ok {
		return false, err
	}
	return !c.missing[path], nil
}

// ledgerOf extracts the (single) ledger a fake archive key holds.
func ledgerOf(t *testing.T, key string) uint32 {
	t.Helper()
	low, high, err := datastore.ParseRangeFromObjectKey(key)
	if err != nil {
		t.Fatalf("ParseRangeFromObjectKey(%q): %v", key, err)
	}
	if low != high {
		t.Fatalf("key %q spans %d-%d; fake archive is one ledger per file", key, low, high)
	}
	return low
}

// ledgerSetOf returns the ledgers a candidate list covers, ascending. The set is the contract, not the order:
// inside a partition keys sort newest-ledger-first (names embed MaxUint32-ledger); only the partition walk is oldest-first.
func ledgerSetOf(t *testing.T, keys []string) []uint32 {
	t.Helper()
	out := make([]uint32, 0, len(keys))
	for _, k := range keys {
		out = append(out, ledgerOf(t, k))
	}
	slices.Sort(out)
	return out
}

// ledgerSeq is the expected-value builder: every ledger in [from, to].
func ledgerSeq(from, to uint32) []uint32 {
	out := make([]uint32, 0, to-from+1)
	for s := from; s <= to; s++ {
		out = append(out, s)
	}
	return out
}

func trimTestOpts(olderThan uint32, maxFiles int) trimOpts {
	return trimOpts{olderThan: olderThan, verifyUpstream: true, maxFiles: maxFiles}
}

// TestPlanTrim_EnumeratesPastTheSDKThousandKeyCap is THE regression for the SDK key cap (3000 objects, 6 partitions of 500, cutoff 1250). One unbounded call would see only .config.json plus ledgers 2999..2001, all above the cutoff; paging finds all 1250 sub-cutoff files and never reads the three partitions above it.
func TestPlanTrim_EnumeratesPastTheSDKThousandKeyCap(t *testing.T) {
	t.Parallel()
	hot := newFakeArchive(t, 500, 3000)
	if len(hot.keys) <= sdkListFilePathsMaxLimit {
		t.Fatalf("fixture must exceed the SDK cap; got %d keys", len(hot.keys))
	}
	plan, err := planTrim(context.Background(), discardLogger(), hot, &fakeCold{}, hot.partitionsOf(t), trimTestOpts(1250, 100000))
	if err != nil {
		t.Fatalf("planTrim: %v", err)
	}

	// The corrected value: EVERY ledger below the cutoff, no more, no
	// fewer. An unpaged listing leaves this list empty.
	if got, want := ledgerSetOf(t, plan.candidates), ledgerSeq(0, 1249); !slices.Equal(got, want) {
		t.Errorf("candidates cover %d ledgers (%v…) — want all 1250 of 0..1249. Pre-fix this was 0 candidates: the 1000-key cap only ever showed the newest ledgers",
			len(got), got[:min(5, len(got))])
	}
	// Only the straddling partition contributes too-fresh files; the
	// three partitions above the cutoff are skipped unread.
	if plan.skippedTooFresh != 250 {
		t.Errorf("skippedTooFresh = %d, want 250 (ledgers 1250..1499 inside the straddling partition)", plan.skippedTooFresh)
	}
	if plan.filesEnumerated != 1500 {
		t.Errorf("filesEnumerated = %d, want 1500 (3 scanned partitions x 500)", plan.filesEnumerated)
	}
	if plan.partitionsTotal != 6 || plan.partitionsScanned != 3 || plan.partitionsSkippedAboveCutoff != 3 || plan.partitionsStraddling != 1 {
		t.Errorf("partition accounting = total %d / scanned %d / skipped %d / straddling %d, want 6/3/3/1",
			plan.partitionsTotal, plan.partitionsScanned, plan.partitionsSkippedAboveCutoff, plan.partitionsStraddling)
	}
	if plan.capReached {
		t.Error("capReached = true under a 100000-file cap; enumeration_complete would lie to the operator")
	}
}

// TestPlanTrim_StraddlingPartitionIsFilteredPerFile: a partition straddling the cutoff must be filtered per file (skipping it leaves trimmable data; trimming it deletes hot files). 2000 objects in one partition also force intra-partition StartAfter paging.
func TestPlanTrim_StraddlingPartitionIsFilteredPerFile(t *testing.T) {
	t.Parallel()
	hot := newFakeArchive(t, 2000, 2000)
	plan, err := planTrim(context.Background(), discardLogger(), hot, &fakeCold{}, hot.partitionsOf(t), trimTestOpts(1500, 100000))
	if err != nil {
		t.Fatalf("planTrim: %v", err)
	}
	if plan.partitionsStraddling != 1 || plan.partitionsScanned != 1 {
		t.Fatalf("straddling %d / scanned %d, want 1/1", plan.partitionsStraddling, plan.partitionsScanned)
	}
	if got, want := ledgerSetOf(t, plan.candidates), ledgerSeq(0, 1499); !slices.Equal(got, want) {
		t.Errorf("candidates cover %d ledgers, want all 1500 of 0..1499; pre-fix the single capped list saw only ledgers 1000..1999, giving 500", len(got))
	}
	if plan.skippedTooFresh != 500 {
		t.Errorf("skippedTooFresh = %d, want 500 (ledgers 1500..1999 kept hot)", plan.skippedTooFresh)
	}
	if plan.filesEnumerated != 2000 {
		t.Errorf("filesEnumerated = %d, want 2000 — the straddling partition must be read in full", plan.filesEnumerated)
	}
	if plan.listCalls != 3 {
		t.Errorf("listCalls = %d, want 3 (1000 + 1000 + terminating empty page)", plan.listCalls)
	}
	if slices.ContainsFunc(plan.candidates, func(k string) bool { return ledgerOf(t, k) >= 1500 }) {
		t.Error("a file at/above the cutoff was planned for deletion — the straddling partition must not be trimmed wholesale")
	}
}

// TestPlanTrim_MaxFilesCapsAcrossPartitions: --max-files caps the whole run, not each partition (cap 600 over 4 partitions stops mid-second-partition).
func TestPlanTrim_MaxFilesCapsAcrossPartitions(t *testing.T) {
	t.Parallel()
	hot := newFakeArchive(t, 500, 2000)
	plan, err := planTrim(context.Background(), discardLogger(), hot, &fakeCold{}, hot.partitionsOf(t), trimTestOpts(2000, 600))
	if err != nil {
		t.Fatalf("planTrim: %v", err)
	}
	if len(plan.candidates) != 600 {
		t.Fatalf("candidates = %d, want exactly 600 (the run-wide cap)", len(plan.candidates))
	}
	if !plan.capReached {
		t.Error("capReached = false; the plan is a prefix of the trimmable set and must say so")
	}
	if plan.partitionsScanned != 2 {
		t.Errorf("partitionsScanned = %d, want 2 — the cap must stop the walk, not restart per partition", plan.partitionsScanned)
	}
	if plan.partitionsUnvisited != 2 {
		t.Errorf("partitionsUnvisited = %d, want 2", plan.partitionsUnvisited)
	}
	// Oldest partition first, and the cap bites 100 files into the second partition (900..999, since keys sort newest-first within a partition).
	// A per-partition counter would have yielded 1000 candidates.
	want := append(ledgerSeq(0, 499), ledgerSeq(900, 999)...)
	if got := ledgerSetOf(t, plan.candidates); !slices.Equal(got, want) {
		t.Errorf("capped candidate set = %d ledgers spanning %d..%d, want 500 from the first partition + 100 from the second", len(got), got[0], got[len(got)-1])
	}
}

// TestPlanTrim_SkipsAboveCutoffPartitionsWithoutListing: partitions at or above the cutoff cost zero listings, so a chunked trim does not re-page the bucket every run.
func TestPlanTrim_SkipsAboveCutoffPartitionsWithoutListing(t *testing.T) {
	t.Parallel()
	hot := newFakeArchive(t, 500, 2000)
	parts := hot.partitionsOf(t)
	hot.listFileCalls = 0
	plan, err := planTrim(context.Background(), discardLogger(), hot, &fakeCold{}, parts, trimTestOpts(1, 100000))
	if err != nil {
		t.Fatalf("planTrim: %v", err)
	}
	if hot.listFileCalls != 1 || plan.listCalls != 1 {
		t.Errorf("listFileCalls = %d / plan.listCalls = %d, want 1 each (only the partition containing ledger 0)", hot.listFileCalls, plan.listCalls)
	}
	if plan.partitionsSkippedAboveCutoff != 3 {
		t.Errorf("partitionsSkippedAboveCutoff = %d, want 3", plan.partitionsSkippedAboveCutoff)
	}
	if len(plan.candidates) != 1 {
		t.Errorf("candidates = %d, want 1 (ledger 0 only)", len(plan.candidates))
	}
}

// TestPlanTrim_VerifyUpstreamStillGatesEveryCandidate: a file the cold tier lacks, or that errors on HEAD, is never a deletion candidate.
func TestPlanTrim_VerifyUpstreamStillGatesEveryCandidate(t *testing.T) {
	t.Parallel()
	hot := newFakeArchive(t, 500, 1500)
	parts := hot.partitionsOf(t)
	schema := datastore.DataStoreSchema{LedgersPerFile: 1, FilesPerPartition: 500, FileExtension: "zst"}
	absent := schema.GetObjectKeyFromSequenceNumber(7)
	boom := schema.GetObjectKeyFromSequenceNumber(9)
	cold := &fakeCold{
		missing: map[string]bool{absent: true},
		errOn:   map[string]error{boom: errors.New("cold HEAD timeout")},
	}
	plan, err := planTrim(context.Background(), discardLogger(), hot, cold, parts, trimTestOpts(1000, 100000))
	if err != nil {
		t.Fatalf("planTrim: %v", err)
	}
	if len(plan.candidates) != 998 {
		t.Errorf("candidates = %d, want 998 (1000 below cutoff, minus one absent and one HEAD failure)", len(plan.candidates))
	}
	if plan.skippedNotInCold != 2 || plan.verifyErrors != 1 {
		t.Errorf("skippedNotInCold = %d (want 2), verifyErrors = %d (want 1)", plan.skippedNotInCold, plan.verifyErrors)
	}
	if slices.Contains(plan.candidates, absent) || slices.Contains(plan.candidates, boom) {
		t.Error("a file unverified in cold reached the deletion candidate list")
	}
	if cold.calls != 1000 {
		t.Errorf("cold.Exists calls = %d, want 1000 (one HEAD per sub-cutoff file)", cold.calls)
	}
}

// TestPlanTrim_UnparsedPartitionNameIsScannedNotSkipped: a directory name we cannot bucket must be read, not assumed fresh, or trimmable data is silently left behind.
func TestPlanTrim_UnparsedPartitionNameIsScannedNotSkipped(t *testing.T) {
	t.Parallel()
	hot := newFakeArchive(t, 500, 1000)
	hot.keys = append(hot.keys, "legacy-dump/FFFFFFFB--4.xdr.zst")
	slices.Sort(hot.keys)
	parts := hot.partitionsOf(t)
	plan, err := planTrim(context.Background(), discardLogger(), hot, &fakeCold{}, parts, trimTestOpts(5, 100000))
	if err != nil {
		t.Fatalf("planTrim: %v", err)
	}
	if plan.partitionsUnparsed != 1 {
		t.Fatalf("partitionsUnparsed = %d, want 1", plan.partitionsUnparsed)
	}
	if !slices.Contains(plan.candidates, "legacy-dump/FFFFFFFB--4.xdr.zst") {
		t.Errorf("file under the unparsed prefix was not enumerated; candidates = %v", plan.candidates)
	}
}

// TestPlanTrim_PropagatesListErrors: a failed enumeration must fail the run, not plan a partial trim.
func TestPlanTrim_PropagatesListErrors(t *testing.T) {
	t.Parallel()
	hot := newFakeArchive(t, 500, 1000)
	parts := hot.partitionsOf(t)
	hot.listFileErr = errors.New("minio: connection reset")
	_, err := planTrim(context.Background(), discardLogger(), hot, &fakeCold{}, parts, trimTestOpts(1000, 100000))
	if err == nil {
		t.Fatal("expected a list error to abort planning")
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("error should wrap the backend failure, got: %v", err)
	}
}

// TestListHotPartitions_PagesPastOneThousandCommonPrefixes: MaxKeys caps CommonPrefixes at 1000 like objects, so an unpaged delimited listing would reintroduce the key-cap bug one level up.
func TestListHotPartitions_PagesPastOneThousandCommonPrefixes(t *testing.T) {
	t.Parallel()
	const partitions = 1200
	hot := newFakeArchive(t, 2, partitions*2)
	parts, rootObjects, err := listHotPartitions(context.Background(), hot, "galexie-archive", "")
	if err != nil {
		t.Fatalf("listHotPartitions: %v", err)
	}
	if len(parts) != partitions {
		t.Errorf("partitions = %d, want %d — a single delimited call would have stopped at 1000", len(parts), partitions)
	}
	if hot.listObjectCalls != 2 {
		t.Errorf("listObjectCalls = %d, want 2 (1000 + 201 incl. the root object)", hot.listObjectCalls)
	}
	if rootObjects != 1 {
		t.Errorf("rootObjects = %d, want 1 (.config.json)", rootObjects)
	}
	for _, p := range parts {
		if !p.parsed {
			t.Fatalf("partition %q did not parse", p.prefix)
		}
	}
	// Discovery returns partitions lexicographically, i.e. newest first (descending hex), the ordering trap behind the original bug.
	// planTrim re-sorts; see TestComparePartitions_OldestFirstUnparsedLast.
	if parts[0].start != (partitions-1)*2 {
		t.Errorf("first discovered partition starts at %d, want %d (lexicographic = newest first)", parts[0].start, (partitions-1)*2)
	}
	if last := parts[len(parts)-1]; last.start != 0 || last.end != 1 {
		t.Errorf("last discovered partition = %d-%d, want 0-1", last.start, last.end)
	}
}

// TestListHotPartitions_TrimsBucketKeyPrefix: ListFilePaths' Prefix is relative to the datastore key prefix while ListObjectsV2 returns absolute keys, so discovery must return the relative form.
func TestListHotPartitions_TrimsBucketKeyPrefix(t *testing.T) {
	t.Parallel()
	hot := newFakeArchive(t, 500, 1000)
	hot.keyPrefix = "v1.1/stellar/ledgers/pubnet/"
	parts, _, err := listHotPartitions(context.Background(), hot, "aws-public-blockchain", "v1.1/stellar/ledgers/pubnet")
	if err != nil {
		t.Fatalf("listHotPartitions: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("partitions = %d, want 2", len(parts))
	}
	if strings.Contains(parts[0].prefix, "pubnet") {
		t.Errorf("partition prefix %q still carries the bucket key prefix", parts[0].prefix)
	}
	plan, err := planTrim(context.Background(), discardLogger(), hot, &fakeCold{}, parts, trimTestOpts(1000, 100000))
	if err != nil {
		t.Fatalf("planTrim: %v", err)
	}
	if len(plan.candidates) != 1000 {
		t.Errorf("candidates = %d, want 1000 — key-prefixed buckets must enumerate too", len(plan.candidates))
	}
}

// TestParsePartitionPrefix covers real names seen on r1 plus rejects that must fall back to "scan it, don't trust it".
func TestParsePartitionPrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		parsed bool
		start  uint32
		end    uint32
	}{
		{"FFFFFFFF--0-63999/", true, 0, 63999},
		{"FC354BFF--63616000-63679999/", true, 63616000, 63679999},
		{"FC354BFF--63616000-63679999", true, 63616000, 63679999},
		// Hex must equal MaxUint32-start: a name we can't fully trust
		// is scanned rather than skipped.
		{"00000000--63616000-63679999/", false, 0, 0},
		{"FFFFFFFF--0/", false, 0, 0},
		{"legacy-dump/", false, 0, 0},
		{".config.json", false, 0, 0},
		{"", false, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			got := parsePartitionPrefix(c.in)
			if got.parsed != c.parsed {
				t.Fatalf("parsed = %v, want %v", got.parsed, c.parsed)
			}
			if got.prefix != c.in {
				t.Errorf("prefix = %q, want %q (must round-trip for the follow-up listing)", got.prefix, c.in)
			}
			if got.parsed && (got.start != c.start || got.end != c.end) {
				t.Errorf("range = %d-%d, want %d-%d", got.start, got.end, c.start, c.end)
			}
		})
	}
}

// TestComparePartitions_OldestFirstUnparsedLast pins the scan order --max-files depends on: a capped run spends its budget on the oldest ledgers.
func TestComparePartitions_OldestFirstUnparsedLast(t *testing.T) {
	t.Parallel()
	parts := []hotPartition{
		{prefix: "zz-unparsed/"},
		{prefix: "b/", start: 64000, end: 127999, parsed: true},
		{prefix: "aa-unparsed/"},
		{prefix: "a/", start: 0, end: 63999, parsed: true},
	}
	slices.SortFunc(parts, comparePartitions)
	want := []string{"a/", "b/", "aa-unparsed/", "zz-unparsed/"}
	for i, w := range want {
		if parts[i].prefix != w {
			t.Fatalf("order[%d] = %q, want %q (full order: %v)", i, parts[i].prefix, w, parts)
		}
	}
}

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

// fakeHotBucket models S3/MinIO delete semantics: DeleteObject succeeds
// whether or not the key exists. swallow simulates a backend that answers
// success and deletes nothing.
type fakeHotBucket struct {
	objects map[string]bool
	swallow bool
}

func (b *fakeHotBucket) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	if !b.swallow {
		delete(b.objects, aws.ToString(in.Key))
	}
	return &s3.DeleteObjectOutput{}, nil
}

// fakeHotView resolves a datastore-relative path the way the SDK's S3
// datastore does (path.Join(prefix, p)), i.e. the key space the trim's
// candidates were listed from.
type fakeHotView struct {
	bucket *fakeHotBucket
	prefix string
}

func (v fakeHotView) Exists(_ context.Context, p string) (bool, error) {
	return v.bucket.objects[path.Join(v.prefix, p)], nil
}

var trimDeleteCandidates = []string{
	"FFFFFFFD--2-63999/FFFFFFFD--2.xdr.zst",
	"FFFFFFFD--2-63999/FFFFFFFC--3.xdr.zst",
}

func newPrefixedHotBucket(t *testing.T, bucketPath string) (*fakeHotBucket, string, string) {
	t.Helper()
	bucket, prefix, err := splitBucketPath(bucketPath)
	if err != nil {
		t.Fatalf("splitBucketPath(%q): %v", bucketPath, err)
	}
	b := &fakeHotBucket{objects: map[string]bool{}}
	for _, p := range trimDeleteCandidates {
		b.objects[path.Join(prefix, p)] = true
	}
	return b, bucket, prefix
}

// TestDeleteTrimCandidates_DeletesTheListedObject: the delete key must be the one the listing used; a hand-built key with a doubled slash named no object, S3 answered success, and the disk never moved.
func TestDeleteTrimCandidates_DeletesTheListedObject(t *testing.T) {
	t.Parallel()
	for _, bucketPath := range []string{"galexie-archive", "galexie-archive/archive", "galexie-archive/archive/"} {
		t.Run(bucketPath, func(t *testing.T) {
			t.Parallel()
			b, bucket, prefix := newPrefixedHotBucket(t, bucketPath)
			deleted, errs, err := deleteTrimCandidates(context.Background(), discardLogger(), b, fakeHotView{b, prefix}, bucket, prefix, trimDeleteCandidates)
			if err != nil || errs != 0 {
				t.Fatalf("deleteTrimCandidates: deleted=%d errs=%d err=%v", deleted, errs, err)
			}
			if len(b.objects) != 0 {
				t.Fatalf("objects still present after a reported deletion: %v", b.objects)
			}
			if deleted != len(trimDeleteCandidates) {
				t.Fatalf("deleted = %d, want %d", deleted, len(trimDeleteCandidates))
			}
		})
	}
}

// TestDeleteTrimCandidates_UnverifiedDeleteFails: a DeleteObject that succeeds while the object still resolves is not a deletion and must fail the run.
func TestDeleteTrimCandidates_UnverifiedDeleteFails(t *testing.T) {
	t.Parallel()
	b, bucket, prefix := newPrefixedHotBucket(t, "galexie-archive/archive")
	b.swallow = true
	deleted, _, err := deleteTrimCandidates(context.Background(), discardLogger(), b, fakeHotView{b, prefix}, bucket, prefix, trimDeleteCandidates)
	if err == nil || !strings.Contains(err.Error(), "still resolves") {
		t.Fatalf("err = %v, want the unverified delete to abort the run", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0: no object was actually removed", deleted)
	}
}

// TestCheckTrimCutoff: --older-than-ledger must be bounded against the tip, or a huge value would trim the hot tier to the live seam.
func TestCheckTrimCutoff(t *testing.T) {
	t.Parallel()
	const tip = 62_000_000
	for _, tc := range []struct {
		name      string
		olderThan uint32
		tip       uint32
		wantOK    bool
	}{
		{"max uint32", 4294967295, tip, false},
		{"at the tip", tip, tip, false},
		{"one ledger inside the window", tip - trimMinHotWindowLedgers + 1, tip, false},
		{"at the window edge", tip - trimMinHotWindowLedgers, tip, true},
		{"90-day scheduled cutoff", tip - 90*17_280, tip, true},
		{"archive younger than the window", 2, trimMinHotWindowLedgers - 1, false},
	} {
		err := checkTrimCutoff(tc.olderThan, tc.tip)
		if (err == nil) != tc.wantOK {
			t.Errorf("%s: checkTrimCutoff(%d, %d) = %v, want ok=%v", tc.name, tc.olderThan, tc.tip, err, tc.wantOK)
		}
	}
}
