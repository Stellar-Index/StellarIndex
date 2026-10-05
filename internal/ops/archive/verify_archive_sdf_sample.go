package archive

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"

	"github.com/stellar/go-stellar-sdk/support/datastore"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Tier C: sample ledgers and compare each of our galexie objects with
// the same key in SDF's public dataset (the storage.s3_cold_* tier) by
// ETag and size. Galexie objects are single-part uploads, so equal
// ETags mean equal bytes. Reuses galexie-mirror-verify's comparison;
// the difference is the bounded random sample, cheap enough to run
// without listing whole partitions.

const sdfSampleReason = "sdf-sample"

// sdfSampleSchema is pubnet's galexie layout (LedgersPerFile=1).
var sdfSampleSchema = datastore.DataStoreSchema{LedgersPerFile: 1, FilesPerPartition: 64000, FileExtension: "zst"}

type sdfSampleResult struct {
	sampled         int
	matched         int
	unverifiable    int
	missingUpstream int // SDF does not hold it: not evidence either way
	findings        []mirrorFinding
}

// pickSampleLedgers returns up to n distinct ledgers in [from, to], sorted.
func pickSampleLedgers(from, to uint32, n int, rng *rand.Rand) []uint32 {
	span := uint64(to) - uint64(from) + 1
	if uint64(n) >= span {
		out := make([]uint32, 0, span)
		for l := from; ; l++ {
			out = append(out, l)
			if l == to {
				break
			}
		}
		return out
	}
	seen := make(map[uint32]struct{}, n)
	for len(seen) < n {
		seen[from+uint32(rng.Uint64N(span))] = struct{}{}
	}
	out := make([]uint32, 0, n)
	for l := range seen {
		out = append(out, l)
	}
	slices.Sort(out)
	return out
}

// sampleObject looks up one exact key; ok is false when the store has none.
func sampleObject(ctx context.Context, s mirrorStore, key string) (mirrorObject, bool, error) {
	objs, err := listMirrorObjects(ctx, s, key)
	if err != nil {
		return mirrorObject{}, false, err
	}
	o, ok := objs[key]
	return o, ok, nil
}

func verifySDFSample(ctx context.Context, local, upstream mirrorStore, ledgers []uint32) (sdfSampleResult, error) {
	var res sdfSampleResult
	for _, seq := range ledgers {
		key := sdfSampleSchema.GetObjectKeyFromSequenceNumber(seq)
		up, upOK, err := sampleObject(ctx, upstream, key)
		if err != nil {
			return res, fmt.Errorf("upstream: %w", err)
		}
		if !upOK {
			res.missingUpstream++
			continue
		}
		lo, loOK, err := sampleObject(ctx, local, key)
		if err != nil {
			return res, fmt.Errorf("local: %w", err)
		}
		res.sampled++
		if !loOK {
			res.findings = append(res.findings, mirrorFinding{key: key, cause: "missing-local", upstream: up})
			continue
		}
		switch cause := compareMirrorObject(lo, up); cause {
		case "":
			res.matched++
		case "unverifiable":
			res.unverifiable++
		default:
			res.findings = append(res.findings, mirrorFinding{key: key, cause: cause, local: lo, upstream: up})
		}
	}
	return res, nil
}

// verifyArchiveSDFSample is the Tier C entry point. Any mismatch bumps
// the shared divergence counter (reason "sdf-sample") and fails the run.
func verifyArchiveSDFSample(cfg config.Config, bucketOverride string, from, to uint32, samples int) error {
	if to == 0 {
		return fmt.Errorf("tier sdf-sample needs an explicit -to (the sample range's upper bound)")
	}
	if from > to {
		return fmt.Errorf("-from %d is above -to %d", from, to)
	}
	if samples < 1 {
		return fmt.Errorf("-sdf-samples must be >= 1; got %d", samples)
	}
	local, upstream, err := buildMirrorStores(cfg, bucketOverride)
	if err != nil {
		return err
	}
	ledgers := pickSampleLedgers(from, to, samples, rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))) //nolint:gosec // sampling, not security
	res, err := verifySDFSample(context.Background(), local, upstream, ledgers)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "verify-archive: sdf-sample sampled=%d matched=%d unverifiable=%d missing-upstream=%d mismatched=%d\n",
		res.sampled, res.matched, res.unverifiable, res.missingUpstream, len(res.findings))
	for _, f := range res.findings {
		fmt.Fprintf(os.Stderr, "verify-archive: sdf-sample MISMATCH %s cause=%s local=%s/%d upstream=%s/%d\n",
			f.key, f.cause, f.local.etag, f.local.size, f.upstream.etag, f.upstream.size)
		obs.VerifyArchiveMismatchesTotal.WithLabelValues(sdfSampleReason, sdfSampleReason).Inc()
	}
	if len(res.findings) > 0 {
		return fmt.Errorf("sdf-sample: %d of %d sampled ledgers differ from SDF's dataset", len(res.findings), res.sampled)
	}
	return nil
}
