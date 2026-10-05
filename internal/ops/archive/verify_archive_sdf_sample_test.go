package archive

import (
	"context"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type fakeSampleLister map[string]string // key -> etag

func (f fakeSampleLister) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	out := &s3.ListObjectsV2Output{}
	for k, etag := range f {
		if strings.HasPrefix(k, aws.ToString(in.Prefix)) {
			out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k), ETag: aws.String(`"` + etag + `"`), Size: aws.Int64(10)})
		}
	}
	return out, nil
}

func TestVerifySDFSample(t *testing.T) {
	t.Parallel()
	k := func(seq uint32) string { return sdfSampleSchema.GetObjectKeyFromSequenceNumber(seq) }
	local := mirrorStore{api: fakeSampleLister{k(10): "aa", k(11): "bb", k(12): "cc"}}
	upstream := mirrorStore{api: fakeSampleLister{k(10): "aa", k(11): "XX", k(12): "cc", k(13): "dd"}}
	// 10 matches, 11 differs, 13 is missing locally, 14 is absent upstream.
	res, err := verifySDFSample(context.Background(), local, upstream, []uint32{10, 11, 13, 14})
	if err != nil {
		t.Fatal(err)
	}
	if res.sampled != 3 || res.matched != 1 || res.missingUpstream != 1 || len(res.findings) != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
	causes := res.findings[0].cause + "," + res.findings[1].cause
	if causes != mirrorCauseLocalDiffers+",missing-local" {
		t.Fatalf("causes = %s", causes)
	}
}

func TestPickSampleLedgers(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	got := pickSampleLedgers(100, 200, 20, rng)
	if len(got) != 20 {
		t.Fatalf("len = %d", len(got))
	}
	for i, l := range got {
		if l < 100 || l > 200 || (i > 0 && l <= got[i-1]) {
			t.Fatalf("bad sample %v", got)
		}
	}
	if all := pickSampleLedgers(5, 7, 10, rng); len(all) != 3 {
		t.Fatalf("small range = %v", all)
	}
}
