package archive

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stellar/go-stellar-sdk/support/compressxdr"
	"github.com/stellar/go-stellar-sdk/support/datastore"
	"github.com/stellar/go-stellar-sdk/xdr"
	"golang.org/x/sync/errgroup"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
)

// galexie-mirror-verify compares every object of the local Galexie mirror with the upstream public dataset (storage.s3_cold_* tier) by ETag and size, partition by partition.
// galexie-archive-fill copies only absent objects and verify-archive checks header hashes, so an upstream re-export is invisible to both; single-part ETags are body MD5s, so equal ETags mean equal bytes.
// A mismatch is fetched from both sides and decoded: batches equal after normaliseLedgerBatch are `equivalent` (another core run's
// export of the same ledgers); the rest are classified by Last-Modified (upstream newer than our mirror time = re-exported).
// Read-only; exits non-zero on a listing or fetch error, a real mismatch, or an object upstream no longer lists.

const (
	mirrorCauseUpstreamRewritten = "upstream-rewritten"
	mirrorCauseLocalDiffers      = "local-differs"
	mirrorCauseLocalOnly         = "local-only"
	mirrorResultEquivalent       = "equivalent"

	// mirrorDecodeParallel bounds the per-partition fetch+decode of mismatched objects (two ~1 MiB GETs each).
	mirrorDecodeParallel = 8
	// mirrorMaxObjectBytes caps one compressed object read; a ledger batch is a few MiB at most.
	mirrorMaxObjectBytes = 256 << 20
)

// mirrorAPI is the slice of *s3.Client the comparison needs: listing, plus fetching a mismatched object to decode it.
type mirrorAPI interface {
	s3PrefixLister
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

type mirrorVerifyOpts struct {
	cfgPath   string
	bucket    string
	from      uint32
	to        uint32
	parallel  int
	maxReport int
	textfile  string
}

// mirrorObject is the listing metadata compared per key.
type mirrorObject struct {
	etag     string
	size     int64
	modified time.Time
}

// mirrorStore is one side of the comparison: a bucket plus the key
// prefix its partitions sit under.
type mirrorStore struct {
	api    mirrorAPI
	bucket string
	prefix string
}

// mirrorFinding is one object that fails the comparison.
type mirrorFinding struct {
	partition string
	key       string
	cause     string
	local     mirrorObject
	upstream  mirrorObject
}

// mirrorVerifyResult carries exact counts; findings are complete, and
// only their printing is capped.
type mirrorVerifyResult struct {
	partitionsCompared     int
	partitionsLocalOnly    int
	partitionsUpstreamOnly int
	objectsCompared        int
	objectsMatched         int
	// equivalent objects differ in bytes but decode to the same ledgers after normaliseLedgerBatch.
	equivalent   int
	unverifiable int
	findings     []mirrorFinding
	// missing are upstream objects a compared partition lacks locally:
	// the fill's job, so they never fail the run.
	missing []mirrorFinding
}

func (r mirrorVerifyResult) count(cause string) int {
	n := 0
	for _, f := range r.findings {
		if f.cause == cause {
			n++
		}
	}
	return n
}

func galexieMirrorVerify(args []string) error {
	opts, err := parseMirrorVerifyFlags(args)
	if err != nil {
		return err
	}
	cfg, err := config.LoadWithEnv(opts.cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !cfg.Storage.ColdTieringEnabled() {
		return fmt.Errorf("upstream dataset not configured — set storage.s3_cold_bucket_archive in %s", opts.cfgPath)
	}
	local, upstream, err := buildMirrorStores(cfg, opts.bucket)
	if err != nil {
		return err
	}

	rootCtx, cancel := opsutil.SignalContext()
	defer cancel()

	res, err := verifyGalexieMirror(rootCtx, local, upstream, opts)
	if err != nil {
		return err
	}
	reportMirrorVerify(os.Stdout, res, opts.maxReport)
	if opts.textfile != "" {
		if err := writeMirrorVerifyTextfile(opts.textfile, res, time.Now()); err != nil {
			return err
		}
	}
	if n := len(res.findings); n > 0 {
		return fmt.Errorf("galexie mirror differs from upstream on %d object(s) — see MISMATCH lines above", n)
	}
	return nil
}

// buildMirrorStores resolves the local mirror (MinIO, the hot S3 block) and the upstream dataset (cold block, with its own isolated
// credentials, see pipeline.NewColdS3Client).
func buildMirrorStores(cfg config.Config, bucketOverride string) (local, upstream mirrorStore, err error) {
	bucketPath, err := opsutil.HistoricReadBucket(cfg, bucketOverride)
	if err != nil {
		return local, upstream, err
	}
	if local.bucket, local.prefix, err = splitBucketPath(bucketPath); err != nil {
		return local, upstream, fmt.Errorf("parse local bucket path %q: %w", bucketPath, err)
	}
	hotClient, err := buildS3Client(context.Background(), cfg.Storage.S3Endpoint, cfg.Storage.S3Region, cfg.Storage.S3AccessKeyEnv, cfg.Storage.S3SecretKeyEnv)
	if err != nil {
		return local, upstream, fmt.Errorf("local s3 client: %w", err)
	}
	local.api = hotClient
	if upstream.bucket, upstream.prefix, err = splitBucketPath(cfg.Storage.S3ColdBucketArchive); err != nil {
		return local, upstream, fmt.Errorf("parse upstream bucket path %q: %w", cfg.Storage.S3ColdBucketArchive, err)
	}
	coldClient, err := pipeline.NewColdS3Client(cfg.Storage)
	if err != nil {
		return local, upstream, err
	}
	upstream.api = coldClient
	return local, upstream, nil
}

// verifyGalexieMirror compares every partition present on both sides
// within [opts.from, opts.to] (0 = unbounded).
func verifyGalexieMirror(ctx context.Context, local, upstream mirrorStore, opts mirrorVerifyOpts) (mirrorVerifyResult, error) {
	var res mirrorVerifyResult
	localParts, _, err := listHotPartitions(ctx, local.api, local.bucket, local.prefix)
	if err != nil {
		return res, fmt.Errorf("local: %w", err)
	}
	upstreamParts, _, err := listHotPartitions(ctx, upstream.api, upstream.bucket, upstream.prefix)
	if err != nil {
		return res, fmt.Errorf("upstream: %w", err)
	}
	upstreamSet := make(map[string]bool, len(upstreamParts))
	for _, p := range upstreamParts {
		if mirrorPartitionInScope(p, opts) {
			upstreamSet[p.prefix] = true
		}
	}
	var shared []string
	for _, p := range localParts {
		if !mirrorPartitionInScope(p, opts) {
			continue
		}
		if upstreamSet[p.prefix] {
			shared = append(shared, p.prefix)
			delete(upstreamSet, p.prefix)
			continue
		}
		res.partitionsLocalOnly++
	}
	res.partitionsUpstreamOnly = len(upstreamSet)
	slices.Sort(shared)

	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(opts.parallel)
	for _, part := range shared {
		g.Go(func() error {
			pr, err := compareMirrorPartition(gctx, local, upstream, part)
			if err != nil {
				return err
			}
			mu.Lock()
			defer mu.Unlock()
			res.merge(pr)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return res, err
	}
	res.partitionsCompared = len(shared)
	slices.SortFunc(res.findings, func(a, b mirrorFinding) int {
		return strings.Compare(a.partition+a.key, b.partition+b.key)
	})
	return res, nil
}

func (r *mirrorVerifyResult) merge(o mirrorVerifyResult) {
	r.objectsCompared += o.objectsCompared
	r.objectsMatched += o.objectsMatched
	r.equivalent += o.equivalent
	r.unverifiable += o.unverifiable
	r.missing = append(r.missing, o.missing...)
	r.findings = append(r.findings, o.findings...)
}

// mirrorPartitionInScope keeps a partition overlapping [from, to]. An
// unparsed name is kept: its range cannot prove it is out of scope.
func mirrorPartitionInScope(p hotPartition, opts mirrorVerifyOpts) bool {
	if !p.parsed {
		return true
	}
	if opts.from != 0 && p.end < opts.from {
		return false
	}
	return opts.to == 0 || p.start <= opts.to
}

// compareMirrorPartition lists one partition on both sides and classifies every key. Local is listed first so an object upstream publishes
// mid-run reads as missing locally, never local-only.
func compareMirrorPartition(ctx context.Context, local, upstream mirrorStore, part string) (mirrorVerifyResult, error) {
	var res mirrorVerifyResult
	localObjs, err := listMirrorObjects(ctx, local, part)
	if err != nil {
		return res, fmt.Errorf("local: %w", err)
	}
	upstreamObjs, err := listMirrorObjects(ctx, upstream, part)
	if err != nil {
		return res, fmt.Errorf("upstream: %w", err)
	}
	var differing []mirrorFinding
	for key, lo := range localObjs {
		uo, ok := upstreamObjs[key]
		if !ok {
			res.findings = append(res.findings, mirrorFinding{partition: part, key: key, cause: mirrorCauseLocalOnly, local: lo})
			continue
		}
		res.objectsCompared++
		switch cause := compareMirrorObject(lo, uo); cause {
		case "":
			res.objectsMatched++
		case "unverifiable":
			res.unverifiable++
		default:
			differing = append(differing, mirrorFinding{partition: part, key: key, cause: cause, local: lo, upstream: uo})
		}
	}
	if err := classifyDiffering(ctx, local, upstream, differing, &res); err != nil {
		return res, err
	}
	for key, uo := range upstreamObjs {
		if _, ok := localObjs[key]; !ok {
			res.missing = append(res.missing, mirrorFinding{partition: part, key: key, upstream: uo})
		}
	}
	return res, nil
}

// compareMirrorObject returns "" for a match, "unverifiable" when a multipart ETag (not a body MD5) leaves only the size to compare and it
// agrees, or the mismatch cause.
func compareMirrorObject(local, upstream mirrorObject) string {
	if local.etag == upstream.etag && local.size == upstream.size {
		return ""
	}
	multipart := strings.Contains(local.etag, "-") || strings.Contains(upstream.etag, "-")
	if multipart && local.size == upstream.size {
		return "unverifiable"
	}
	if upstream.modified.After(local.modified) {
		return mirrorCauseUpstreamRewritten
	}
	return mirrorCauseLocalDiffers
}

// classifyDiffering decodes each byte-differing pair: equivalent ones are counted, the rest become findings.
func classifyDiffering(ctx context.Context, local, upstream mirrorStore, differing []mirrorFinding, res *mirrorVerifyResult) error {
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(mirrorDecodeParallel)
	for _, f := range differing {
		g.Go(func() error {
			eq, err := mirrorObjectsEquivalent(gctx, local, upstream, f.key)
			if err != nil {
				return err
			}
			mu.Lock()
			defer mu.Unlock()
			if eq {
				res.equivalent++
			} else {
				res.findings = append(res.findings, f)
			}
			return nil
		})
	}
	return g.Wait()
}

// mirrorObjectsEquivalent fetches key from both stores. A fetch error fails the run; an object that does not decode is not
// equivalent, so it stays a finding.
func mirrorObjectsEquivalent(ctx context.Context, local, upstream mirrorStore, key string) (bool, error) {
	lb, err := fetchMirrorObject(ctx, local, key)
	if err != nil {
		return false, fmt.Errorf("local: %w", err)
	}
	ub, err := fetchMirrorObject(ctx, upstream, key)
	if err != nil {
		return false, fmt.Errorf("upstream: %w", err)
	}
	ln, lerr := normalisedBatchBytes(lb)
	un, uerr := normalisedBatchBytes(ub)
	return lerr == nil && uerr == nil && bytes.Equal(ln, un), nil
}

func fetchMirrorObject(ctx context.Context, s mirrorStore, key string) ([]byte, error) {
	full := key
	if s.prefix != "" {
		full = strings.TrimSuffix(s.prefix, "/") + "/" + key
	}
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(full)})
	if err != nil {
		return nil, fmt.Errorf("get %s/%s: %w", s.bucket, full, err)
	}
	defer func() { _ = out.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(out.Body, mirrorMaxObjectBytes))
	if err != nil {
		return nil, fmt.Errorf("read %s/%s: %w", s.bucket, full, err)
	}
	return b, nil
}

// normalisedBatchBytes zstd-decodes one Galexie object and returns its LedgerCloseMetaBatch re-marshalled after normalisation.
func normalisedBatchBytes(compressed []byte) ([]byte, error) {
	var batch xdr.LedgerCloseMetaBatch
	if _, err := compressxdr.NewXDRDecoder(compressxdr.DefaultCompressor, &batch).ReadFrom(bytes.NewReader(compressed)); err != nil {
		return nil, err
	}
	if err := normaliseLedgerBatch(&batch); err != nil {
		return nil, err
	}
	return batch.MarshalBinary()
}

// normaliseLedgerBatch removes what differs between two stellar-core runs over the same ledgers: core_metrics invoke_time_nsecs
// diagnostic events (wall-clock timings) and the order of each operation's ledger-entry-change groups, which core does not fix.
// Everything else, other diagnostic events included, is left to the byte comparison.
func normaliseLedgerBatch(b *xdr.LedgerCloseMetaBatch) error {
	for i := range b.LedgerCloseMetas {
		lcm := &b.LedgerCloseMetas[i]
		var metas []*xdr.TransactionMeta
		switch {
		case lcm.V0 != nil:
			for j := range lcm.V0.TxProcessing {
				metas = append(metas, &lcm.V0.TxProcessing[j].TxApplyProcessing)
			}
		case lcm.V1 != nil:
			for j := range lcm.V1.TxProcessing {
				metas = append(metas, &lcm.V1.TxProcessing[j].TxApplyProcessing)
			}
		case lcm.V2 != nil:
			for j := range lcm.V2.TxProcessing {
				metas = append(metas, &lcm.V2.TxProcessing[j].TxApplyProcessing)
			}
		}
		for _, m := range metas {
			if err := normaliseTxMeta(m); err != nil {
				return err
			}
		}
	}
	return nil
}

func normaliseTxMeta(m *xdr.TransactionMeta) error {
	var ops []xdr.OperationMeta
	switch {
	case m.Operations != nil:
		ops = *m.Operations
	case m.V1 != nil:
		ops = m.V1.Operations
	case m.V2 != nil:
		ops = m.V2.Operations
	case m.V3 != nil:
		ops = m.V3.Operations
		if m.V3.SorobanMeta != nil {
			m.V3.SorobanMeta.DiagnosticEvents = dropTimingEvents(m.V3.SorobanMeta.DiagnosticEvents)
		}
	case m.V4 != nil:
		for k := range m.V4.Operations {
			if err := sortChangeGroups(m.V4.Operations[k].Changes); err != nil {
				return err
			}
		}
		m.V4.DiagnosticEvents = dropTimingEvents(m.V4.DiagnosticEvents)
	}
	for k := range ops {
		if err := sortChangeGroups(ops[k].Changes); err != nil {
			return err
		}
	}
	return nil
}

// dropTimingEvents removes the host's core_metrics invoke_time_nsecs events, whose value is a per-run wall-clock measurement.
func dropTimingEvents(evs []xdr.DiagnosticEvent) []xdr.DiagnosticEvent {
	out := evs[:0]
	for _, ev := range evs {
		if !isInvokeTimeEvent(ev) {
			out = append(out, ev)
		}
	}
	return out
}

func isInvokeTimeEvent(ev xdr.DiagnosticEvent) bool {
	e := ev.Event
	if e.Type != xdr.ContractEventTypeDiagnostic || e.ContractId != nil || e.Body.V0 == nil {
		return false
	}
	t := e.Body.V0.Topics
	return len(t) == 2 && t[0].Sym != nil && *t[0].Sym == "core_metrics" && t[1].Sym != nil && *t[1].Sym == "invoke_time_nsecs"
}

// sortChangeGroups orders one operation's changes canonically in place. A STATE entry and the change after it (its UPDATED or
// REMOVED) are one group and stay adjacent; groups sort by their XDR bytes.
func sortChangeGroups(c xdr.LedgerEntryChanges) error {
	type group struct {
		raw     []byte
		changes []xdr.LedgerEntryChange
	}
	groups := make([]group, 0, len(c))
	for i := 0; i < len(c); i++ {
		n := 1
		if c[i].Type == xdr.LedgerEntryChangeTypeLedgerEntryState && i+1 < len(c) {
			n = 2
		}
		var raw []byte
		for _, ch := range c[i : i+n] {
			b, err := ch.MarshalBinary()
			if err != nil {
				return err
			}
			raw = append(raw, b...)
		}
		groups = append(groups, group{raw: raw, changes: slices.Clone(c[i : i+n])})
		i += n - 1
	}
	slices.SortFunc(groups, func(a, b group) int { return bytes.Compare(a.raw, b.raw) })
	i := 0
	for _, g := range groups {
		i += copy(c[i:], g.changes)
	}
	return nil
}

// listMirrorObjects pages through one partition, keyed by the path
// relative to the store's prefix so both sides key identically.
func listMirrorObjects(ctx context.Context, s mirrorStore, part string) (map[string]mirrorObject, error) {
	base := ""
	if s.prefix != "" {
		base = strings.TrimSuffix(s.prefix, "/") + "/"
	}
	out := make(map[string]mirrorObject)
	var token *string
	for {
		page, err := s.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(base + part),
			ContinuationToken: token,
			MaxKeys:           aws.Int32(trimListPageSize),
		})
		if err != nil {
			return nil, fmt.Errorf("list %s/%s%s (after %q): %w", s.bucket, base, part, aws.ToString(token), err)
		}
		for _, o := range page.Contents {
			out[strings.TrimPrefix(aws.ToString(o.Key), base)] = mirrorObject{
				etag:     strings.ToLower(strings.Trim(aws.ToString(o.ETag), `"`)),
				size:     aws.ToInt64(o.Size),
				modified: aws.ToTime(o.LastModified),
			}
		}
		if !aws.ToBool(page.IsTruncated) || page.NextContinuationToken == nil {
			return out, nil
		}
		token = page.NextContinuationToken
	}
}

// mirrorLedgerLabel renders the ledger (or ledger range) a key holds.
func mirrorLedgerLabel(key string) string {
	from, to, err := datastore.ParseRangeFromObjectKey(path.Base(key))
	switch {
	case err != nil:
		return "?"
	case from == to:
		return fmt.Sprintf("%d", from)
	default:
		return fmt.Sprintf("%d-%d", from, to)
	}
}

func reportMirrorVerify(w io.Writer, res mirrorVerifyResult, maxReport int) {
	for i, f := range res.findings {
		if i == maxReport {
			_, _ = fmt.Fprintf(w, "... %d more finding(s) not printed (-max-report %d)\n", len(res.findings)-i, maxReport)
			break
		}
		_, _ = fmt.Fprintf(w, "MISMATCH partition=%s ledger=%s cause=%s local_etag=%s upstream_etag=%s local_size=%d upstream_size=%d local_modified=%s upstream_modified=%s key=%s\n",
			strings.TrimSuffix(f.partition, "/"), mirrorLedgerLabel(f.key), f.cause,
			f.local.etag, mirrorOrNone(f.upstream.etag), f.local.size, f.upstream.size,
			mirrorTime(f.local.modified), mirrorTime(f.upstream.modified), f.key)
	}
	for i, f := range res.missing {
		if i == maxReport {
			_, _ = fmt.Fprintf(w, "... %d more missing object(s) not printed (-max-report %d)\n", len(res.missing)-i, maxReport)
			break
		}
		_, _ = fmt.Fprintf(w, "MISSING partition=%s ledger=%s upstream_etag=%s upstream_size=%d upstream_modified=%s key=%s\n",
			strings.TrimSuffix(f.partition, "/"), mirrorLedgerLabel(f.key), f.upstream.etag, f.upstream.size,
			mirrorTime(f.upstream.modified), f.key)
	}
	_, _ = fmt.Fprintf(w, "galexie-mirror-verify: partitions_compared=%d partitions_local_only=%d partitions_upstream_only=%d objects_compared=%d matched=%d equivalent=%d upstream_rewritten=%d local_differs=%d local_only=%d unverifiable=%d missing_local=%d\n",
		res.partitionsCompared, res.partitionsLocalOnly, res.partitionsUpstreamOnly,
		res.objectsCompared, res.objectsMatched, res.equivalent,
		res.count(mirrorCauseUpstreamRewritten), res.count(mirrorCauseLocalDiffers), res.count(mirrorCauseLocalOnly),
		res.unverifiable, len(res.missing))
}

func mirrorOrNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func mirrorTime(t time.Time) string {
	if t.IsZero() {
		return "none"
	}
	return t.UTC().Format(time.RFC3339)
}

// writeMirrorVerifyTextfile publishes a completed run only; a failed
// run leaves the previous file, so its last_success age is the alarm.
func writeMirrorVerifyTextfile(path string, res mirrorVerifyResult, now time.Time) error {
	var b strings.Builder
	b.WriteString("# HELP galexie_archive_upstream_objects Objects of the last completed galexie-mirror-verify run, by comparison result.\n# TYPE galexie_archive_upstream_objects gauge\n")
	for _, s := range []struct {
		result string
		n      int
	}{
		{"matched", res.objectsMatched},
		{mirrorResultEquivalent, res.equivalent},
		{mirrorCauseUpstreamRewritten, res.count(mirrorCauseUpstreamRewritten)},
		{mirrorCauseLocalDiffers, res.count(mirrorCauseLocalDiffers)},
		{mirrorCauseLocalOnly, res.count(mirrorCauseLocalOnly)},
		{"unverifiable", res.unverifiable},
		{"missing-local", len(res.missing)},
	} {
		fmt.Fprintf(&b, "galexie_archive_upstream_objects{result=%q} %d\n", s.result, s.n)
	}
	fmt.Fprintf(&b, "# HELP galexie_archive_upstream_partitions_compared Partitions present on both the local mirror and upstream that the last completed run compared.\n# TYPE galexie_archive_upstream_partitions_compared gauge\ngalexie_archive_upstream_partitions_compared %d\n", res.partitionsCompared)
	fmt.Fprintf(&b, "# HELP galexie_archive_upstream_last_success_unix Unix time the last galexie-mirror-verify run completed its comparison.\n# TYPE galexie_archive_upstream_last_success_unix gauge\ngalexie_archive_upstream_last_success_unix %d\n", now.Unix())

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil { //nolint:gosec // operator-supplied path; the collector reads world-readable files
		return fmt.Errorf("write textfile %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename textfile %q → %q: %w", tmp, path, err)
	}
	return nil
}

func parseMirrorVerifyFlags(args []string) (mirrorVerifyOpts, error) {
	fs := flag.NewFlagSet("galexie-mirror-verify", flag.ContinueOnError)
	var (
		opts     mirrorVerifyOpts
		from, to int64
	)
	fs.StringVar(&opts.cfgPath, "config", "/etc/stellarindex.toml", "Path to stellarindex.toml")
	fs.StringVar(&opts.bucket, "bucket", "", "Local mirror bucket path (default storage.s3_bucket_archive)")
	fs.Int64Var(&from, "from", 0, "Compare only partitions holding ledgers at or above this sequence (0 = from genesis)")
	fs.Int64Var(&to, "to", 0, "Compare only partitions holding ledgers at or below this sequence (0 = to the tip)")
	fs.IntVar(&opts.parallel, "parallel", 4, "Partitions compared concurrently")
	fs.IntVar(&opts.maxReport, "max-report", 1000, "Most MISMATCH lines printed; counts stay exact")
	fs.StringVar(&opts.textfile, "textfile-output", "", "Prometheus textfile to write on a completed run (empty = none)")
	if err := fs.Parse(args); err != nil {
		return mirrorVerifyOpts{}, err
	}
	for name, v := range map[string]int64{"-from": from, "-to": to} {
		if v < 0 || v > int64(^uint32(0)) {
			return mirrorVerifyOpts{}, fmt.Errorf("%s out of uint32 range: %d", name, v)
		}
	}
	if to != 0 && from > to {
		return mirrorVerifyOpts{}, fmt.Errorf("-from %d is above -to %d", from, to)
	}
	if opts.parallel < 1 || opts.parallel > 64 {
		return mirrorVerifyOpts{}, fmt.Errorf("-parallel must be in 1..64; got %d", opts.parallel)
	}
	if opts.maxReport < 0 {
		return mirrorVerifyOpts{}, fmt.Errorf("-max-report must be >= 0; got %d", opts.maxReport)
	}
	opts.from, opts.to = uint32(from), uint32(to)
	return opts, nil
}
