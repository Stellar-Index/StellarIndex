package archive

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stellar/go-stellar-sdk/support/datastore"
)

// fakeBucket answers ListObjectsV2 with S3's prefix, delimiter and
// continuation semantics; pageCap forces pagination below MaxKeys.
type fakeBucket struct {
	objects map[string]mirrorObject
	pageCap int
	failOn  string
}

func (f *fakeBucket) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	prefix := aws.ToString(in.Prefix)
	if f.failOn != "" && strings.Contains(prefix, f.failOn) {
		return nil, errors.New("listing refused")
	}
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	delim := aws.ToString(in.Delimiter)
	type entry struct {
		key    string
		common bool
	}
	var entries []entry
	seen := map[string]bool{}
	for _, k := range keys {
		rest := strings.TrimPrefix(k, prefix)
		if i := strings.Index(rest, delim); delim != "" && i >= 0 {
			cp := prefix + rest[:i+1]
			if !seen[cp] {
				seen[cp] = true
				entries = append(entries, entry{cp, true})
			}
			continue
		}
		entries = append(entries, entry{k, false})
	}
	start := 0
	if in.ContinuationToken != nil {
		for i, e := range entries {
			if e.key > aws.ToString(in.ContinuationToken) {
				start = i
				break
			}
			start = len(entries)
		}
	}
	limit := int(aws.ToInt32(in.MaxKeys))
	if f.pageCap > 0 && f.pageCap < limit {
		limit = f.pageCap
	}
	end := min(start+limit, len(entries))
	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(end < len(entries))}
	for _, e := range entries[start:end] {
		if e.common {
			out.CommonPrefixes = append(out.CommonPrefixes, types.CommonPrefix{Prefix: aws.String(e.key)})
			continue
		}
		o := f.objects[e.key]
		out.Contents = append(out.Contents, types.Object{
			Key:          aws.String(e.key),
			ETag:         aws.String(`"` + o.etag + `"`),
			Size:         aws.Int64(o.size),
			LastModified: aws.Time(o.modified),
		})
	}
	if end < len(entries) {
		out.NextContinuationToken = aws.String(entries[end-1].key)
	}
	return out, nil
}

var mirrorTestSchema = datastore.DataStoreSchema{LedgersPerFile: 1, FilesPerPartition: 64000, FileExtension: "zst"}

const upstreamTestPrefix = "v1.1/stellar/ledgers/pubnet/"

var (
	mirroredAt = time.Date(2026, 4, 26, 0, 0, 0, 0, time.UTC)
	exportedAt = time.Date(2025, 12, 7, 0, 0, 0, 0, time.UTC)
)

// mirrorPair builds a local mirror and its upstream holding identical
// objects for ledgers, the local copies stamped later (copy time).
func mirrorPair(ledgers ...uint32) (local, upstream *fakeBucket) {
	local = &fakeBucket{objects: map[string]mirrorObject{}}
	upstream = &fakeBucket{objects: map[string]mirrorObject{}}
	for _, l := range ledgers {
		key := mirrorTestSchema.GetObjectKeyFromSequenceNumber(l)
		etag := strings.Repeat("ab", 16)
		local.objects[key] = mirrorObject{etag: etag, size: 227531, modified: mirroredAt}
		upstream.objects[upstreamTestPrefix+key] = mirrorObject{etag: etag, size: 227531, modified: exportedAt}
	}
	return local, upstream
}

func stores(local, upstream *fakeBucket) (mirrorStore, mirrorStore) {
	return mirrorStore{api: local, bucket: "galexie-archive"},
		mirrorStore{api: upstream, bucket: "aws-public-blockchain", prefix: strings.TrimSuffix(upstreamTestPrefix, "/")}
}

func runMirrorVerify(t *testing.T, local, upstream *fakeBucket, opts mirrorVerifyOpts) (mirrorVerifyResult, string) {
	t.Helper()
	if opts.parallel == 0 {
		opts.parallel = 2
	}
	if opts.maxReport == 0 {
		opts.maxReport = 1000
	}
	l, u := stores(local, upstream)
	res, err := verifyGalexieMirror(context.Background(), l, u, opts)
	if err != nil {
		t.Fatalf("verifyGalexieMirror: %v", err)
	}
	var buf bytes.Buffer
	reportMirrorVerify(&buf, res, opts.maxReport)
	return res, buf.String()
}

func partitionOf(l uint32) string {
	return strings.TrimSuffix(mirrorTestSchema.GetObjectKeyFromSequenceNumber(l), "/"+filepath.Base(mirrorTestSchema.GetObjectKeyFromSequenceNumber(l)))
}

func TestGalexieMirrorVerify_AlteredLocalObjectReportedWithPartitionAndLedger(t *testing.T) {
	local, upstream := mirrorPair(58973374, 58973375, 58973376, 59501299)
	key := mirrorTestSchema.GetObjectKeyFromSequenceNumber(58973375)
	altered := local.objects[key]
	altered.etag = "9a21b83c30a8faf17c4ad022f54dd018"
	local.objects[key] = altered

	res, out := runMirrorVerify(t, local, upstream, mirrorVerifyOpts{})

	want := "MISMATCH partition=" + partitionOf(58973375) + " ledger=58973375 cause=local-differs"
	if !strings.Contains(out, want) {
		t.Fatalf("report missing %q:\n%s", want, out)
	}
	if len(res.findings) != 1 || res.objectsMatched != 3 || res.objectsCompared != 4 {
		t.Fatalf("findings=%d matched=%d compared=%d, want 1/3/4", len(res.findings), res.objectsMatched, res.objectsCompared)
	}
}

func TestGalexieMirrorVerify_UpstreamReExportAfterMirrorClassified(t *testing.T) {
	local, upstream := mirrorPair(59501298, 59501299)
	key := upstreamTestPrefix + mirrorTestSchema.GetObjectKeyFromSequenceNumber(59501299)
	upstream.objects[key] = mirrorObject{etag: strings.Repeat("cd", 16), size: 230000, modified: mirroredAt.Add(24 * time.Hour)}

	res, out := runMirrorVerify(t, local, upstream, mirrorVerifyOpts{})

	if !strings.Contains(out, "ledger=59501299 cause=upstream-rewritten") {
		t.Fatalf("re-export not classified:\n%s", out)
	}
	if res.count(mirrorCauseUpstreamRewritten) != 1 {
		t.Fatalf("upstream_rewritten=%d, want 1", res.count(mirrorCauseUpstreamRewritten))
	}
}

func TestGalexieMirrorVerify_PresenceDifferences(t *testing.T) {
	local, upstream := mirrorPair(1000, 1001)
	// Upstream-only object in a shared partition: the fill's job, counted.
	upstream.objects[upstreamTestPrefix+mirrorTestSchema.GetObjectKeyFromSequenceNumber(1002)] = mirrorObject{etag: "ee", size: 1}
	// Local-only object: upstream no longer lists what we hold.
	local.objects[mirrorTestSchema.GetObjectKeyFromSequenceNumber(1003)] = mirrorObject{etag: "ff", size: 1, modified: mirroredAt}
	// A partition only upstream holds (trimmed locally) is not compared.
	upstream.objects[upstreamTestPrefix+mirrorTestSchema.GetObjectKeyFromSequenceNumber(70000)] = mirrorObject{etag: "aa", size: 1}

	res, out := runMirrorVerify(t, local, upstream, mirrorVerifyOpts{})

	if !strings.Contains(out, "ledger=1003 cause=local-only") || !strings.Contains(out, "upstream_etag=none") {
		t.Fatalf("local-only object not reported:\n%s", out)
	}
	if res.missingLocal != 1 || res.partitionsCompared != 1 || res.partitionsUpstreamOnly != 1 || len(res.findings) != 1 {
		t.Fatalf("missing_local=%d compared=%d upstream_only=%d findings=%d, want 1/1/1/1",
			res.missingLocal, res.partitionsCompared, res.partitionsUpstreamOnly, len(res.findings))
	}
}

func TestGalexieMirrorVerify_PaginatesEveryObject(t *testing.T) {
	ledgers := make([]uint32, 0, 25)
	for l := uint32(64000); l < 64025; l++ {
		ledgers = append(ledgers, l)
	}
	local, upstream := mirrorPair(ledgers...)
	local.pageCap, upstream.pageCap = 4, 3
	key := mirrorTestSchema.GetObjectKeyFromSequenceNumber(64000)
	o := local.objects[key]
	o.size++
	local.objects[key] = o

	res, out := runMirrorVerify(t, local, upstream, mirrorVerifyOpts{})

	if res.objectsCompared != 25 || !strings.Contains(out, "ledger=64000 cause=local-differs") {
		t.Fatalf("compared=%d, want 25 with the last-sorted object found:\n%s", res.objectsCompared, out)
	}
}

func TestGalexieMirrorVerify_ScopeAndListingError(t *testing.T) {
	local, upstream := mirrorPair(10, 64010, 128010)
	res, _ := runMirrorVerify(t, local, upstream, mirrorVerifyOpts{from: 64000, to: 127999})
	if res.partitionsCompared != 1 || res.objectsCompared != 1 {
		t.Fatalf("scoped run compared %d partitions / %d objects, want 1/1", res.partitionsCompared, res.objectsCompared)
	}

	upstream.failOn = partitionOf(64010)
	l, u := stores(local, upstream)
	if _, err := verifyGalexieMirror(context.Background(), l, u, mirrorVerifyOpts{parallel: 1}); err == nil {
		t.Fatal("a failed partition listing must fail the run, not read as a clean comparison")
	}
}

func TestCompareMirrorObject(t *testing.T) {
	base := mirrorObject{etag: "abc", size: 10, modified: mirroredAt}
	for _, tc := range []struct {
		name     string
		upstream mirrorObject
		want     string
	}{
		{"identical", mirrorObject{etag: "abc", size: 10, modified: exportedAt}, ""},
		{"same etag different size", mirrorObject{etag: "abc", size: 11, modified: exportedAt}, mirrorCauseLocalDiffers},
		{"multipart same size", mirrorObject{etag: "def-2", size: 10}, "unverifiable"},
		{"multipart different size", mirrorObject{etag: "def-2", size: 12}, mirrorCauseLocalDiffers},
		{"rewritten after mirror", mirrorObject{etag: "def", size: 10, modified: mirroredAt.Add(time.Second)}, mirrorCauseUpstreamRewritten},
	} {
		if got := compareMirrorObject(base, tc.upstream); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestReportMirrorVerify_CapsLinesNotCounts(t *testing.T) {
	res := mirrorVerifyResult{findings: []mirrorFinding{
		{partition: "P/", key: "P/a", cause: mirrorCauseLocalDiffers},
		{partition: "P/", key: "P/b", cause: mirrorCauseLocalDiffers},
	}}
	var buf bytes.Buffer
	reportMirrorVerify(&buf, res, 1)
	out := buf.String()
	if strings.Count(out, "MISMATCH") != 1 || !strings.Contains(out, "1 more finding(s)") || !strings.Contains(out, "local_differs=2") {
		t.Fatalf("cap output wrong:\n%s", out)
	}
}

func TestWriteMirrorVerifyTextfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "galexie_archive_upstream.prom")
	res := mirrorVerifyResult{partitionsCompared: 3, objectsMatched: 7, findings: []mirrorFinding{{cause: mirrorCauseUpstreamRewritten}}}
	if err := writeMirrorVerifyTextfile(path, res, time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`galexie_archive_upstream_objects{result="matched"} 7`,
		`galexie_archive_upstream_objects{result="upstream-rewritten"} 1`,
		`galexie_archive_upstream_objects{result="local-differs"} 0`,
		"galexie_archive_upstream_partitions_compared 3",
		"galexie_archive_upstream_last_success_unix 1700000000",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("textfile missing %q:\n%s", want, b)
		}
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind: %v", err)
	}
}

func TestParseMirrorVerifyFlags(t *testing.T) {
	if _, err := parseMirrorVerifyFlags([]string{"-from", "10", "-to", "5"}); err == nil {
		t.Error("-from above -to accepted")
	}
	if _, err := parseMirrorVerifyFlags([]string{"-parallel", "0"}); err == nil {
		t.Error("-parallel 0 accepted")
	}
	opts, err := parseMirrorVerifyFlags([]string{"-from", "64000"})
	if err != nil || opts.from != 64000 || opts.to != 0 || opts.parallel != 4 {
		t.Fatalf("defaults: %+v, %v", opts, err)
	}
}
