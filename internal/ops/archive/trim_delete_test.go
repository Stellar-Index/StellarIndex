package archive

import (
	"context"
	"path"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

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

// TestDeleteTrimCandidates_DeletesTheListedObject: with a configured bucket
// path carrying a trailing slash, the hand-built key
// (TrimPrefix(prefix+"/", "/") + p) was "archive//FFFF…", which names no
// object; S3 answered success and the run reported deleted=N while the
// disk never moved (#1198). The key must be the one the listing used.
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

// TestDeleteTrimCandidates_UnverifiedDeleteFails: a DeleteObject that
// returns success while the object still resolves is not a deletion. It
// must fail the run, not be counted.
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

// TestParseTrimFlags_MaxFilesCeiling: --max-files had only a > 0 check, so
// -max-files 99999999 was accepted despite the "a typo can never delete the
// full archive" claim.
func TestParseTrimFlags_MaxFilesCeiling(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		v      string
		wantOK bool
	}{
		{"99999999", false},
		{"1000001", false},
		{"0", false},
		{"1000000", true},
		{"100000", true},
	} {
		_, err := parseTrimFlags([]string{"-older-than-ledger", "1000", "-max-files", tc.v})
		if (err == nil) != tc.wantOK {
			t.Errorf("-max-files %s: err = %v, want ok=%v", tc.v, err, tc.wantOK)
		}
	}
}

// TestCheckTrimCutoff: --older-than-ledger was unbounded against the tip, so
// -older-than-ledger 4294967295 would trim the hot tier to the live seam.
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
