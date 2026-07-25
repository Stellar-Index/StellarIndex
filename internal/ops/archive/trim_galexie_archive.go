package archive

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stellar/go-stellar-sdk/support/datastore"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
)

// ─── stellarindex-ops trim-galexie-archive ──────────────────────
//
// Per ADR-0027 §Step 2: the DESTRUCTIVE operator that deletes
// cold-eligible LCM files from the local hot tier (galexie-archive
// MinIO bucket on r1) once their presence in the cold tier has
// been verified. Reclaims pool capacity by tiering off the bulky
// historical mirror; the cold tier (aws-public-blockchain) serves
// reads for those ranges through the TieredDataStore fallback.
//
// Safety stack (each is independent):
//
//   1. --dry-run is the DEFAULT. Actual deletion requires the
//      explicit --commit flag — there is no "are you sure?" prompt
//      because the dry-run output IS the review step. Mismatched
//      flags (e.g. --dry-run --commit) report which dominates and
//      stop before any S3 call.
//   2. --verify-upstream is the DEFAULT. Every candidate is
//      HEAD'd against the cold tier before being marked for
//      deletion. If cold.Exists returns false, the candidate is
//      SKIPPED. Pass --no-verify-upstream only for an isolated
//      restore-from-backup workflow where you've already proven
//      the upstream copy by other means.
//   3. --max-files caps deletions per run. Default 100000 — a
//      typo can never delete the full archive in one shot.
//   4. --older-than-ledger is REQUIRED. No implicit "trim
//      everything below tip - N". Operator names a specific
//      sequence; the planner shows the exact span.
//   5. Cold-tier MUST be configured (cfg.Storage.S3ColdBucketArchive
//      non-empty). Refuses to run otherwise — without a cold tier
//      every "trim" is unrecoverable data loss.

type trimOpts struct {
	cfgPath                string
	olderThan              uint32 // ledger sequence boundary; files entirely below this are candidates
	verifyUpstream         bool
	iHaveVerifiedOutOfBand bool // required alongside --no-verify-upstream (REL-05)
	dryRun                 bool
	commit                 bool
	maxFiles               int
}

func trimGalexieArchive(args []string) error { //nolint:gocognit,gocyclo,funlen // CLI plumbing + filter + delete loop; six helpers would scatter the safety guard chain
	opts, err := parseTrimFlags(args)
	if err != nil {
		return err
	}
	if opts.olderThan == 0 {
		return fmt.Errorf("--older-than-ledger is required (no implicit cutoff — name a specific ledger sequence)")
	}
	if opts.dryRun && opts.commit {
		return fmt.Errorf("--dry-run and --commit are mutually exclusive; pick one")
	}
	// Default to dry-run when neither is set. The explicit lack
	// of --commit is the safety primitive.
	if !opts.commit {
		opts.dryRun = true
	}
	if opts.maxFiles <= 0 {
		return fmt.Errorf("--max-files must be > 0; got %d", opts.maxFiles)
	}

	// LoadWithEnv (not bare Load) so the STELLARINDEX_* env overrides —
	// the injected Postgres DSN / Redis + ClickHouse secrets — take
	// effect, matching every other binary (cmd/stellarindex-{api,indexer,
	// aggregator} and the rest of internal/ops). Bare Load left this
	// operator reading the placeholder TOML DSN/secrets (C3-14).
	cfg, err := config.LoadWithEnv(opts.cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !cfg.Storage.ColdTieringEnabled() {
		return fmt.Errorf("cold tier not configured — trimming without a cold fallback is unrecoverable data loss. Set storage.s3_cold_bucket_archive first")
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if !opts.verifyUpstream {
		// REL-05: --no-verify-upstream disables the ONLY check that the
		// cold tier actually holds what we're about to delete from hot.
		// Loud + unmissable, naming the exact span, since this is a
		// destructive run with the primary safety primitive off.
		logger.Warn("!!! trim-galexie-archive: --no-verify-upstream is set — deleting hot files below the cutoff with NO check that the cold tier holds them !!!",
			"older_than_ledger", opts.olderThan,
			"commit", opts.commit,
			"acknowledged_out_of_band", opts.iHaveVerifiedOutOfBand,
		)
	}

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	hot, err := datastore.NewDataStore(rootCtx, datastore.DataStoreConfig{
		Type: "S3",
		Params: map[string]string{
			"destination_bucket_path": cfg.Storage.S3BucketArchive,
			"region":                  cfg.Storage.S3Region,
			"endpoint_url":            cfg.Storage.S3Endpoint,
		},
		NetworkPassphrase: cfg.Stellar.Passphrase(),
		Compression:       "zstd",
	})
	if err != nil {
		return fmt.Errorf("hot datastore: %w", err)
	}
	defer func() { _ = hot.Close() }()

	var cold datastore.DataStore
	if opts.verifyUpstream {
		// pipeline.NewColdDataStore, NOT datastore.NewDataStore: the
		// SDK builds every S3 client from the ambient AWS credential
		// chain, which on r1 carries local MinIO's keys (the hot tier
		// authenticates through it). Presenting those to real AWS
		// failed every cold read with `InvalidAccessKeyId ... does not
		// exist in our records` (diagnosed 2026-07-25). That matters
		// doubly HERE: this operator DELETES hot files, and a cold
		// tier that cannot authenticate is a cold tier that cannot
		// prove the upstream copy exists.
		cold, err = pipeline.NewColdDataStore(rootCtx, cfg.Storage)
		if err != nil {
			return fmt.Errorf("cold datastore: %w", err)
		}
		defer func() { _ = cold.Close() }()
	}

	// Raw S3 client for DeleteObject — the SDK's datastore.DataStore
	// interface has no Delete method. We construct the same shape
	// the SDK's NewS3DataStore builds (path-style, optional anonymous
	// fallback for public buckets that don't need it here), then
	// call DeleteObject directly. Auth comes from the standard AWS
	// env vars (or the operator's ~/.aws/credentials if running
	// interactively). Hot is local MinIO; the env vars
	// STELLARINDEX_S3_ACCESS_KEY + STELLARINDEX_S3_SECRET_KEY map
	// to MinIO's root creds via the systemd EnvironmentFile.
	//
	// This client is HOT-ONLY and therefore does NOT share the
	// cold-tier credential bug (2026-07-25): every argument below
	// comes from cfg.Storage.S3* (the MinIO block), it only ever
	// DeleteObjects out of hotBucket, and the ambient AWS_* chain it
	// may fall back to holds MinIO's credentials — the right ones for
	// this endpoint. It is also the reason the cold path could not
	// simply reuse buildS3Client: a single ambient chain is correct
	// for exactly one of the two backends.
	hotBucket, hotKeyPrefix, err := splitBucketPath(cfg.Storage.S3BucketArchive)
	if err != nil {
		return fmt.Errorf("parse archive bucket path: %w", err)
	}
	s3Client, err := buildS3Client(rootCtx, cfg.Storage.S3Endpoint, cfg.Storage.S3Region, cfg.Storage.S3AccessKeyEnv, cfg.Storage.S3SecretKeyEnv)
	if err != nil {
		return fmt.Errorf("build s3 client: %w", err)
	}

	logger.Info("trim plan",
		"hot_bucket", hotBucket,
		"hot_key_prefix", hotKeyPrefix,
		"older_than_ledger", opts.olderThan,
		"verify_upstream", opts.verifyUpstream,
		"max_files", opts.maxFiles,
		"dry_run", opts.dryRun,
	)

	// Enumerate hot files: discover the partition directories, then
	// page through the ones that can hold sub-cutoff ledgers. See the
	// "partition-scoped enumeration" block below for why a single
	// ListFilePaths call can never work here.
	parts, rootObjects, err := listHotPartitions(rootCtx, s3Client, hotBucket, hotKeyPrefix)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		// No partition directories at all: either the bucket is empty,
		// or it was written with filesPerPartition<=1 (SDK
		// GetObjectKeyFromSequenceNumber emits a flat layout then).
		// Scan the whole bucket as one unnamed partition so the flat
		// case is enumerated COMPLETELY rather than silently trimming
		// nothing — silently trimming nothing is the exact failure
		// this change exists to remove.
		parts = []hotPartition{{}}
	}

	plan, err := planTrim(rootCtx, logger, hot, cold, parts, opts)
	if err != nil {
		return err
	}
	// Observability is part of the fix. The old line —
	// `hot file enumeration total_files=1000` — was indistinguishable
	// from a healthy "nothing to trim", which is why a bucket of 63.6M
	// objects looked empty for as long as it did. An operator must be
	// able to read, from one line, HOW MUCH of the archive was actually
	// looked at: enumeration_complete=false means this plan is a prefix
	// of the trimmable set (the --max-files cap stopped the scan), not
	// the whole of it.
	logger.Info("hot partition scan",
		"partitions_total", plan.partitionsTotal,
		"partitions_scanned", plan.partitionsScanned,
		"partitions_skipped_above_cutoff", plan.partitionsSkippedAboveCutoff,
		"partitions_straddling_cutoff", plan.partitionsStraddling,
		"partitions_unparsed_name_scanned", plan.partitionsUnparsed,
		"partitions_unvisited", plan.partitionsUnvisited,
		"files_enumerated", plan.filesEnumerated,
		"list_calls", plan.listCalls,
		"root_objects", rootObjects,
		"enumeration_complete", !plan.capReached,
	)

	candidates := plan.candidates
	errs := plan.verifyErrors

	logger.Info("trim plan ready",
		"candidates", len(candidates),
		"skipped_too_fresh", plan.skippedTooFresh,
		"skipped_not_in_cold", plan.skippedNotInCold,
		"verify_errors", errs,
		"dry_run", opts.dryRun,
	)

	if opts.dryRun {
		// Surface the first/last few candidates so an operator
		// reviewing dry-run output can sanity-check the boundary.
		if n := len(candidates); n > 0 {
			head := candidates[:minInt(3, n)]
			tail := candidates[maxInt(0, n-3):]
			logger.Info("dry-run sample",
				"first_3", head,
				"last_3", tail,
			)
		}
		return nil
	}

	// --commit: do the deletions. Per-object DeleteObject (vs
	// DeleteObjects bulk) so a partial failure leaves a clear
	// position cursor — operator can re-run --dry-run to see
	// what's left.
	start := time.Now()
	var deleted int
	for _, p := range candidates {
		if err := rootCtx.Err(); err != nil {
			return fmt.Errorf("trim aborted at %d/%d: %w", deleted, len(candidates), err)
		}
		fullKey := p
		if hotKeyPrefix != "" {
			fullKey = strings.TrimPrefix(hotKeyPrefix+"/", "/") + p
		}
		_, derr := s3Client.DeleteObject(rootCtx, &s3.DeleteObjectInput{
			Bucket: aws.String(hotBucket),
			Key:    aws.String(fullKey),
		})
		if derr != nil {
			logger.Warn("DeleteObject failed", "path", p, "err", derr)
			errs++
			continue
		}
		deleted++
	}
	logger.Info("trim complete",
		"deleted", deleted,
		"errors", errs,
		"elapsed", time.Since(start).String(),
	)
	if errs > 0 {
		return fmt.Errorf("trim finished with %d errors (deleted %d/%d)", errs, deleted, len(candidates))
	}
	return nil
}

// ─── partition-scoped enumeration ────────────────────────────────
//
// INCIDENT 2026-07-25: this operator could never trim anything, and
// said so in a way that read like success. It called
//
//	hot.ListFilePaths(rootCtx, datastore.ListFileOptions{})
//
// exactly once. The SDK clamps an unbounded request to 1000 keys —
// support/datastore/datastore.go:24 `listFilePathsMaxLimit = 1000`,
// applied in s3.go's ListFilePaths as `if remaining <= 0 || remaining >
// listFilePathsMaxLimit { remaining = listFilePathsMaxLimit }` — so
// Limit:0 means "1000", not "all". galexie-archive holds ~63.6 MILLION
// objects (.config.json: ledgersPerBatch=1, batchesPerPartition=64000;
// 995 partitions; one object per ledger).
//
// Worse, the 1000 it did see were always the WRONG 1000. Partition
// directories are named "%08X--<start>-<end>/" where the hex is
// MaxUint32-start (SDK DataStoreSchema.GetObjectKeyFromSequenceNumber),
// so the hex DESCENDS as the ledger ASCENDS: "FFFFFFFF--0-63999/"
// sorts before "FC354BFF--63616000-63679999/". A lexicographic listing
// therefore returns the NEWEST objects first, and the newest 1000 are
// above any cutoff worth naming. Measured on r1:
//
//	trim-galexie-archive -older-than-ledger 10000000 -dry-run
//	  hot file enumeration  total_files=1000
//	  trim plan ready       candidates=0 skipped_too_fresh=999
//	                        skipped_not_in_cold=0 verify_errors=0
//
// "candidates=0" is what a fully-trimmed archive looks like too. That
// indistinguishability is why the ADR-0027 §Step 2 capacity relief
// never happened.
//
// The replacement enumerates completely without brute-forcing 63,600
// sequential pages over the whole bucket: discover the partitions
// (one delimited listing), skip whole partitions that sit entirely
// at/above the cutoff without reading their contents, and page
// StartAfter through the rest. A partition that STRADDLES the cutoff
// is neither skipped nor trimmed wholesale — it is enumerated and
// filtered per file, exactly as before.
//
// The per-file safety chain (ParseRangeFromObjectKey bucketing, the
// cold-tier HEAD, --max-files, --dry-run) is untouched; only the set
// of files it is handed changed. --max-files still caps DELETIONS FOR
// THE WHOLE RUN, not per partition: the counter lives on trimPlan,
// which spans every partition scanned.

// trimListPageSize is the SDK's hard per-call ceiling (see above). We
// request it explicitly rather than relying on the Limit:0 default,
// because "0 means 1000" is the trap this whole block exists to avoid.
const trimListPageSize = 1000

// trimFileLister / trimColdChecker are the slices of
// datastore.DataStore the planner actually calls, split out so the
// enumeration is unit-testable against a fake that reproduces the
// SDK's 1000-key clamp — without live S3/MinIO. datastore.DataStore
// satisfies both structurally (same seam idiom as rehydrateStore).
type trimFileLister interface {
	ListFilePaths(ctx context.Context, options datastore.ListFileOptions) ([]string, error)
}

type trimColdChecker interface {
	Exists(ctx context.Context, path string) (bool, error)
}

// s3PrefixLister is the slice of *s3.Client partition discovery needs.
// The SDK's DataStore interface has no delimited/common-prefix
// listing, so discovery goes through the same raw hot-only client the
// deletion loop uses.
type s3PrefixLister interface {
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// hotPartition is one top-level partition directory of the Galexie
// archive: "%08X--<startLedger>-<endLedger>/", key-relative (the
// bucket's own key prefix already trimmed off). parsed is false when
// the directory name doesn't match that layout — such a partition is
// SCANNED, never skipped, since we can't prove its contents are above
// the cutoff.
type hotPartition struct {
	prefix string
	start  uint32
	end    uint32
	parsed bool
}

// trimPlan is the result of enumeration + filtering: the candidate
// list plus the counters that make "found nothing" distinguishable
// from "barely looked".
type trimPlan struct {
	candidates       []string
	skippedTooFresh  int
	skippedNotInCold int
	verifyErrors     int

	partitionsTotal              int
	partitionsScanned            int
	partitionsSkippedAboveCutoff int
	partitionsStraddling         int
	partitionsUnparsed           int
	partitionsUnvisited          int
	filesEnumerated              int
	listCalls                    int
	capReached                   bool
}

// partitionNameRE matches the SDK's partition directory naming.
// Hex is validated against MaxUint32-start by parsePartitionPrefix.
var partitionNameRE = regexp.MustCompile(`^([0-9A-Fa-f]{8})--(\d+)-(\d+)$`)

// parsePartitionPrefix extracts a partition's declared ledger range
// from its directory name. A name that doesn't match the schema — or
// whose hex doesn't equal MaxUint32-start — is returned unparsed, so
// the caller scans it rather than trusting a range it can't verify.
// Skipping on a mis-read name would silently leave trimmable data
// behind, which is the failure mode this file is fixing.
func parsePartitionPrefix(prefix string) hotPartition {
	part := hotPartition{prefix: prefix}
	m := partitionNameRE.FindStringSubmatch(strings.TrimSuffix(prefix, "/"))
	if m == nil {
		return part
	}
	hex, err := strconv.ParseUint(m[1], 16, 32)
	if err != nil {
		return part
	}
	start, err := strconv.ParseUint(m[2], 10, 32)
	if err != nil {
		return part
	}
	end, err := strconv.ParseUint(m[3], 10, 32)
	if err != nil || end < start || hex != uint64(math.MaxUint32)-start {
		return part
	}
	part.start, part.end, part.parsed = uint32(start), uint32(end), true
	return part
}

// listHotPartitions discovers the archive's top-level partition
// directories with a delimited listing — CommonPrefixes, so S3 rolls
// up the 63.6M objects into the 995 directories that contain them in
// a single round trip.
//
// It pages, deliberately: MaxKeys caps CommonPrefixes at 1000 exactly
// as it caps objects, and partition #1001 begins at ledger 64,000,000
// — roughly three weeks past 2026-07-25 at Stellar's ~15k ledgers/day.
// An unpaged delimited listing would silently truncate in a few weeks,
// reintroducing this very bug one level up.
//
// Discovery is ground truth rather than derived from the schema
// manifest: a partition already trimmed empty stops being returned, so
// the chunked re-runs in docs/operations/lcm-cache-tiering.md §Step 4
// don't pay a LIST call per already-emptied partition.
//
// The second return is the count of root-level objects (e.g.
// .config.json) — reported, not scanned.
func listHotPartitions(ctx context.Context, api s3PrefixLister, bucket, keyPrefix string) ([]hotPartition, int, error) {
	prefix := ""
	if keyPrefix != "" {
		prefix = strings.TrimSuffix(keyPrefix, "/") + "/"
	}
	var (
		parts       []hotPartition
		rootObjects int
		token       *string
	)
	for {
		out, err := api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(prefix),
			Delimiter:         aws.String("/"),
			ContinuationToken: token,
			MaxKeys:           aws.Int32(trimListPageSize),
			FetchOwner:        aws.Bool(false),
		})
		if err != nil {
			return nil, 0, fmt.Errorf("discover hot partitions (after %q): %w", aws.ToString(token), err)
		}
		for _, cp := range out.CommonPrefixes {
			name := strings.TrimPrefix(aws.ToString(cp.Prefix), prefix)
			if name == "" || name == "/" {
				continue
			}
			parts = append(parts, parsePartitionPrefix(name))
		}
		rootObjects += len(out.Contents)
		if out.IsTruncated == nil || !*out.IsTruncated || out.NextContinuationToken == nil {
			return parts, rootObjects, nil
		}
		token = out.NextContinuationToken
	}
}

// comparePartitions orders partitions oldest-ledger-first. That is the
// order a tiering trim wants: the oldest ledgers are the most
// cold-eligible, so when --max-files stops a run early it stops having
// done the most useful work, and the next run picks up what is left.
// Unparsed names sort last — they are scanned, but they must not
// displace known-old data under the cap.
//
// Ordering WITHIN a partition is the backend's, i.e. lexicographic,
// i.e. newest-ledger-first (filenames embed MaxUint32-ledger). A capped
// run therefore takes whole old partitions plus the newest slice of the
// partition where the cap bit. That is a harmless artefact — every file
// in a partition spans the same 64000-ledger band and is equally
// cold-eligible — and re-sorting 64000 keys per partition to "fix" it
// would buy nothing.
func comparePartitions(a, b hotPartition) int {
	switch {
	case a.parsed && b.parsed:
		return cmp.Compare(a.start, b.start)
	case a.parsed:
		return -1
	case b.parsed:
		return 1
	default:
		return strings.Compare(a.prefix, b.prefix)
	}
}

// planTrim walks the partitions oldest-first and builds the deletion
// plan. Partitions entirely at/above the cutoff are skipped WITHOUT
// enumerating their contents (the whole point of scoping by
// partition); everything else — including the one partition that
// straddles the cutoff, and any partition whose name we couldn't
// parse — is enumerated in full and filtered per file.
func planTrim(ctx context.Context, logger *slog.Logger, hot trimFileLister, cold trimColdChecker, parts []hotPartition, opts trimOpts) (trimPlan, error) {
	ordered := slices.Clone(parts)
	slices.SortFunc(ordered, comparePartitions)

	plan := trimPlan{partitionsTotal: len(ordered)}
	for i, part := range ordered {
		if err := ctx.Err(); err != nil {
			return plan, fmt.Errorf("trim planning aborted after %d/%d partitions: %w", i, len(ordered), err)
		}
		switch {
		case part.parsed && part.start >= opts.olderThan:
			// Every file in here is at/above the cutoff. Skipping it
			// unread is the only reason this doesn't cost 63,600 LIST
			// calls on every run.
			plan.partitionsSkippedAboveCutoff++
			continue
		case !part.parsed:
			plan.partitionsUnparsed++
		case part.end >= opts.olderThan:
			plan.partitionsStraddling++
		}
		if err := scanPartition(ctx, logger, hot, cold, part, opts, &plan); err != nil {
			return plan, err
		}
		if plan.capReached {
			plan.partitionsUnvisited = len(ordered) - i - 1
			break
		}
	}
	return plan, nil
}

// scanPartition enumerates one partition COMPLETELY — paging with
// StartAfter (the SDK's own pagination handle; see its
// LedgerFileIter, which uses the same last-key-as-cursor idiom) — and
// buckets every file it finds. It returns early only when the shared
// --max-files cap is reached.
func scanPartition(ctx context.Context, logger *slog.Logger, hot trimFileLister, cold trimColdChecker, part hotPartition, opts trimOpts, plan *trimPlan) error {
	plan.partitionsScanned++
	var startAfter string
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("trim planning aborted in partition %q: %w", part.prefix, err)
		}
		page, err := hot.ListFilePaths(ctx, datastore.ListFileOptions{
			Prefix:     part.prefix,
			StartAfter: startAfter,
			Limit:      trimListPageSize,
		})
		plan.listCalls++
		if err != nil {
			return fmt.Errorf("list hot files (partition %q, after %q): %w", part.prefix, startAfter, err)
		}
		if len(page) == 0 {
			return nil
		}
		for _, p := range page {
			plan.filesEnumerated++
			evaluateTrimFile(ctx, logger, cold, p, opts, plan)
			if len(plan.candidates) >= opts.maxFiles {
				plan.capReached = true
				logger.Warn("max-files cap reached; enumeration stopped early — this plan is a PREFIX of the trimmable set, not all of it. Re-run after committing to continue.",
					"max_files", opts.maxFiles,
					"partition", part.prefix,
					"files_enumerated", plan.filesEnumerated,
				)
				return nil
			}
		}
		last := page[len(page)-1]
		if last <= startAfter {
			// Defensive: a backend that ignored StartAfter would spin
			// forever re-listing the same page. Fail loudly instead.
			return fmt.Errorf("hot listing did not advance past %q in partition %q", startAfter, part.prefix)
		}
		startAfter = last
		if len(page) < trimListPageSize {
			// A short page means the SDK exhausted its own continuation
			// loop, i.e. the partition is done.
			return nil
		}
	}
}

// evaluateTrimFile buckets one enumerated path. This is the pre-fix
// per-file safety chain verbatim — parse, cutoff, cold-tier HEAD —
// moved into a function so the enumeration around it could change
// without the filter changing with it.
func evaluateTrimFile(ctx context.Context, logger *slog.Logger, cold trimColdChecker, path string, opts trimOpts, plan *trimPlan) {
	_, to, perr := datastore.ParseRangeFromObjectKey(path)
	if perr != nil {
		// Unparseable path — leave alone. This is the safe
		// posture; only files we can confidently bucket as
		// "fully below cutoff" are candidates.
		return
	}
	if to >= opts.olderThan {
		plan.skippedTooFresh++
		return
	}
	if opts.verifyUpstream {
		ok, ferr := cold.Exists(ctx, path)
		if ferr != nil {
			logger.Warn("cold.Exists failed; treating as not-present (skip)", "path", path, "err", ferr)
			plan.skippedNotInCold++
			plan.verifyErrors++
			return
		}
		if !ok {
			plan.skippedNotInCold++
			return
		}
	}
	plan.candidates = append(plan.candidates, path)
}

func parseTrimFlags(args []string) (trimOpts, error) {
	fs := flag.NewFlagSet("trim-galexie-archive", flag.ContinueOnError)
	var (
		opts      trimOpts
		olderThan int64
		noVerify  bool
	)
	fs.StringVar(&opts.cfgPath, "config", "/etc/stellarindex.toml", "Path to stellarindex.toml")
	fs.Int64Var(&olderThan, "older-than-ledger", 0, "REQUIRED. Files whose entire ledger range is below this sequence become deletion candidates. No default to prevent unintentional trims.")
	fs.BoolVar(&opts.dryRun, "dry-run", false, "List would-delete candidates without deleting. Default when neither --dry-run nor --commit is set.")
	fs.BoolVar(&opts.commit, "commit", false, "Actually delete. Requires explicit opt-in (default behaviour is dry-run).")
	fs.BoolVar(&noVerify, "no-verify-upstream", false, "Skip the HEAD-against-cold check. NOT RECOMMENDED — disables the primary safety primitive. Requires --i-have-verified-cold-out-of-band too.")
	fs.BoolVar(&opts.iHaveVerifiedOutOfBand, "i-have-verified-cold-out-of-band", false, "Required alongside --no-verify-upstream: an explicit second acknowledgement that you have confirmed the cold tier holds these files by some other means. --no-verify-upstream alone is refused.")
	fs.IntVar(&opts.maxFiles, "max-files", 100000, "Hard cap on candidates per run. Default 100000 — a typo can never delete the full archive in one invocation.")
	if err := fs.Parse(args); err != nil {
		return trimOpts{}, err
	}
	if olderThan < 0 || olderThan > int64(^uint32(0)) {
		return trimOpts{}, fmt.Errorf("--older-than-ledger out of uint32 range: %d", olderThan)
	}
	opts.olderThan = uint32(olderThan)
	opts.verifyUpstream = !noVerify
	if noVerify && !opts.iHaveVerifiedOutOfBand {
		return trimOpts{}, fmt.Errorf("--no-verify-upstream disables the primary safety primitive and requires --i-have-verified-cold-out-of-band as a second explicit acknowledgement — refusing to proceed with only one flag set")
	}
	return opts, nil
}

// splitBucketPath parses the SDK-style "bucket/prefix/path" form
// into (bucket, prefix). Mirrors the SDK's `url.Parse("s3://" + ...)`
// logic from datastore/s3.go's FromS3Client.
func splitBucketPath(bucketPath string) (bucket, prefix string, err error) {
	parsed, err := url.Parse("s3://" + bucketPath)
	if err != nil {
		return "", "", err
	}
	return parsed.Host, strings.TrimPrefix(parsed.Path, "/"), nil
}

// buildS3Client builds an *s3.Client with path-style addressing
// (required by MinIO) and the operator-configured endpoint /
// region / credentials. accessKeyEnv + secretKeyEnv are env-var
// NAMES (not values) — they're resolved at call time so the
// binary inherits whatever the systemd EnvironmentFile sets.
func buildS3Client(ctx context.Context, endpoint, region, accessKeyEnv, secretKeyEnv string) (*s3.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	// Prefer explicit creds from the env-var names cfg.Storage points
	// at. The SDK's default chain also picks them up if the env vars
	// match its canonical names (AWS_ACCESS_KEY_ID etc.), but we go
	// explicit to align with the rest of the codebase's env-var
	// naming convention.
	ak := os.Getenv(accessKeyEnv)
	sk := os.Getenv(secretKeyEnv)
	if ak != "" && sk != "" {
		awsCfg.Credentials = credentials.NewStaticCredentialsProvider(ak, sk, "")
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		o.UsePathStyle = true
		o.Region = region
	}), nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
