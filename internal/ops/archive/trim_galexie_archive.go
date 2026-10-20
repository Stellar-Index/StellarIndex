package archive

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stellar/go-stellar-sdk/support/datastore"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
)

// trim-galexie-archive (ADR-0027) is DESTRUCTIVE: dry-run by default (deletion needs --commit/-write), every candidate HEAD'd against the cold tier,
// --max-files capped, --older-than-ledger required and below the hot tip by trimMinHotWindowLedgers, cold tier required;
// a deletion counts only once the hot datastore stops resolving the path (deleteTrimCandidates).

const (
	// trimMaxFilesCeiling bounds --max-files: ~1.6% of the 63.6M-object
	// archive, ten of the runbook's 100k chunked passes.
	trimMaxFilesCeiling = 1_000_000
	// trimMinHotWindowLedgers (30 days at 17,280 ledgers/day) is how far below the hot archive's newest ledger the cutoff must sit: the hot
	// window is 90 days, so 30 leaves the
	// scheduled cutoff 60 days of slack while refusing a cutoff typed at or near the live seam.
	trimMinHotWindowLedgers = 30 * 17_280
)

type trimOpts struct {
	cfgPath                string
	olderThan              uint32 // ledger sequence boundary; files entirely below this are candidates
	verifyUpstream         bool
	iHaveVerifiedOutOfBand bool // required alongside --no-verify-upstream
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
	// Default to dry-run when neither is set. The explicit lack
	// of --commit is the safety primitive.
	if !opts.commit {
		opts.dryRun = true
	}

	// LoadWithEnv, not Load, so STELLARINDEX_* env overrides (injected DSN and secrets) apply as in every other binary.
	cfg, err := config.LoadWithEnv(opts.cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !cfg.Storage.ColdTieringEnabled() {
		return fmt.Errorf("cold tier not configured — trimming without a cold fallback is unrecoverable data loss. Set storage.s3_cold_bucket_archive first")
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if !opts.verifyUpstream {
		// --no-verify-upstream disables the ONLY check that the cold tier actually holds what we're about to delete from hot.
		// Loud + unmissable, naming the exact span, since this is a destructive run with the primary safety primitive off.
		logger.Warn("!!! trim-galexie-archive: --no-verify-upstream is set — deleting hot files below the cutoff with NO check that the cold tier holds them !!!",
			"older_than_ledger", opts.olderThan,
			"commit", opts.commit,
			"acknowledged_out_of_band", opts.iHaveVerifiedOutOfBand,
		)
	}

	rootCtx, cancel := opsutil.SignalContext()
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

	hotTip, err := datastore.FindLatestLedgerSequence(rootCtx, hot)
	if err != nil {
		return fmt.Errorf("find the hot archive's newest ledger (the cutoff is bounded against it): %w", err)
	}
	if err := checkTrimCutoff(opts.olderThan, hotTip); err != nil {
		return err
	}

	var cold datastore.DataStore
	if opts.verifyUpstream {
		// pipeline.NewColdDataStore, NOT datastore.NewDataStore: the SDK uses the ambient AWS chain, which on r1 holds MinIO's keys,
		// so cold reads fail to authenticate and the upstream copy could never be proven to exist before deleting hot files.
		cold, err = pipeline.NewColdDataStore(rootCtx, cfg.Storage)
		if err != nil {
			return fmt.Errorf("cold datastore: %w", err)
		}
		defer func() { _ = cold.Close() }()
	}

	// Raw S3 client for DeleteObject (the SDK datastore has none), path-style. Credentials come from the dedicated trimmer MinIO identity
	// (List+Delete, galexie-archive only) via the systemd EnvironmentFile, never MinIO root. Hot-only, so it avoids the cold-tier ambient-credential hazard.
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
		"hot_tip_ledger", hotTip,
		"verify_upstream", opts.verifyUpstream,
		"max_files", opts.maxFiles,
		"dry_run", opts.dryRun,
	)

	// Enumerate hot files: discover the partition directories, then page through the ones that can hold sub-cutoff ledgers. See
	// the "partition-scoped enumeration" block below for why a single ListFilePaths call can never work here.
	parts, rootObjects, err := listHotPartitions(rootCtx, s3Client, hotBucket, hotKeyPrefix)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		// No partition directories: the bucket is empty or flat (filesPerPartition<=1). Scan it as one unnamed partition so trimming nothing is never silent.
		parts = []hotPartition{{}}
	}

	plan, err := planTrim(rootCtx, logger, hot, cold, parts, opts)
	if err != nil {
		return err
	}
	// Log how much of the archive was looked at: enumeration_complete=false means the plan is a prefix (the --max-files cap stopped the scan),
	// not a healthy nothing-to-trim.
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

	start := time.Now()
	deleted, delErrs, err := deleteTrimCandidates(rootCtx, logger, s3Client, hot, hotBucket, hotKeyPrefix, candidates)
	errs += delErrs
	logger.Info("trim complete",
		"deleted", deleted,
		"errors", errs,
		"elapsed", time.Since(start).String(),
	)
	if err != nil {
		return err
	}
	if errs > 0 {
		return fmt.Errorf("trim finished with %d errors (deleted %d/%d)", errs, deleted, len(candidates))
	}
	return nil
}

// checkTrimCutoff refuses a cutoff within trimMinHotWindowLedgers of the hot archive's newest ledger, so a wrong variable or stray digit
// cannot trim the hot tier up to the live seam.
func checkTrimCutoff(olderThan, hotTip uint32) error {
	var highest uint32
	if hotTip > trimMinHotWindowLedgers {
		highest = hotTip - trimMinHotWindowLedgers
	}
	if olderThan > highest {
		return fmt.Errorf("--older-than-ledger %d is within %d ledgers (30 days) of the hot archive's newest ledger %d; the highest cutoff accepted is %d",
			olderThan, trimMinHotWindowLedgers, hotTip, highest)
	}
	return nil
}

// s3ObjectDeleter is the slice of *s3.Client the deletion loop calls.
type s3ObjectDeleter interface {
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// deleteTrimCandidates deletes one object per call so a partial failure leaves a clear position. S3/MinIO succeed for absent keys, so a deletion
// counts only once hot.Exists (same key space as the listing) stops resolving the path; a path that still resolves aborts the run.
func deleteTrimCandidates(ctx context.Context, logger *slog.Logger, del s3ObjectDeleter, hot trimColdChecker, bucket, keyPrefix string, candidates []string) (deleted, errs int, err error) {
	for _, p := range candidates {
		if err := ctx.Err(); err != nil {
			return deleted, errs, fmt.Errorf("trim aborted at %d/%d: %w", deleted, len(candidates), err)
		}
		key := path.Join(keyPrefix, p)
		if _, derr := del.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		}); derr != nil {
			logger.Warn("DeleteObject failed", "path", p, "key", key, "err", derr)
			errs++
			continue
		}
		still, xerr := hot.Exists(ctx, p)
		if xerr != nil {
			logger.Warn("post-delete existence check failed; not counted as deleted", "path", p, "err", xerr)
			errs++
			continue
		}
		if still {
			return deleted, errs + 1, fmt.Errorf("DeleteObject(%s/%s) returned success but the hot datastore still resolves %q — the delete key does not name the listed object; aborting after %d verified deletion(s)",
				bucket, key, p, deleted)
		}
		deleted++
	}
	return deleted, errs, nil
}

// Partition-scoped enumeration: one hot.ListFilePaths call is clamped to 1000 keys, and partition names embed MaxUint32-start so those are the NEWEST,
// all above the cutoff, giving candidates=0 that looks like success. Discover partitions, skip those wholly at/above the cutoff,
// and page StartAfter through the rest. --max-files caps the whole run (counter on trimPlan).

// trimListPageSize is the SDK's hard per-call ceiling, requested explicitly because the Limit:0 default ("0 means 1000") is the trap this
// block exists to avoid.
const trimListPageSize = 1000

// trimFileLister and trimColdChecker are the slices of datastore.DataStore the planner calls, so tests can fake the SDK's 1000-key clamp.
type trimFileLister interface {
	ListFilePaths(ctx context.Context, options datastore.ListFileOptions) ([]string, error)
}

type trimColdChecker interface {
	Exists(ctx context.Context, path string) (bool, error)
}

// s3PrefixLister is the slice of *s3.Client partition discovery needs: the SDK's DataStore has no delimited/common-prefix listing, so
// discovery uses the same raw hot-only client as the deletion loop.
type s3PrefixLister interface {
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// hotPartition is one top-level partition directory ("%08X--<start>-<end>/", key-relative); parsed is false when the name does not match,
// and such a partition is SCANNED, never skipped, since its range is unproven.
type hotPartition struct {
	prefix string
	start  uint32
	end    uint32
	parsed bool
}

// trimPlan is the result of enumeration + filtering: the candidates plus the counters that make "found nothing" distinguishable from
// "barely looked".
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

// parsePartitionPrefix extracts a partition's ledger range from its name; a mismatch (or hex != MaxUint32-start) returns unparsed so the caller scans it,
// since skipping on a mis-read name would silently leave trimmable data behind.
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

// listHotPartitions discovers top-level partitions with a delimited listing (CommonPrefixes). It pages: MaxKeys caps prefixes at 1000 and the archive
// has passed that, so an unpaged listing would truncate. Discovery is ground truth so emptied partitions drop out. Second return: root-level object count (reported, not scanned).
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

// comparePartitions orders oldest-ledger-first so a --max-files stop has done the most cold-eligible work; unparsed names sort last.
// Order within a partition is the backend's (newest-first), a harmless artefact since every file spans the same cold-eligible band.
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

// planTrim walks partitions oldest-first, skipping those wholly at/above the cutoff WITHOUT enumerating them; the straddling partition and
// unparsed names are enumerated in full and filtered per file.
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

// scanPartition enumerates one partition COMPLETELY, paging with StartAfter, and returns early only when the shared --max-files cap is reached.
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

// evaluateTrimFile buckets one enumerated path through the per-file safety chain (parse, cutoff, cold-tier HEAD), separate so enumeration
// can change without the filter changing.
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
	fs, gate := opsutil.NewMutatingFlagSet("trim-galexie-archive")
	var (
		opts      trimOpts
		olderThan int64
		noVerify  bool
	)
	fs.StringVar(&opts.cfgPath, "config", "/etc/stellarindex.toml", "Path to stellarindex.toml")
	fs.Int64Var(&olderThan, "older-than-ledger", 0, "REQUIRED. Files whose entire ledger range is below this sequence become deletion candidates. No default to prevent unintentional trims.")
	fs.BoolVar(&opts.commit, "commit", false, "Actually delete: an alias for -write. Requires explicit opt-in (default behaviour is dry-run).")
	fs.BoolVar(&noVerify, "no-verify-upstream", false, "Skip the HEAD-against-cold check. NOT RECOMMENDED — disables the primary safety primitive. Requires --i-have-verified-cold-out-of-band too.")
	fs.BoolVar(&opts.iHaveVerifiedOutOfBand, "i-have-verified-cold-out-of-band", false, "Required alongside --no-verify-upstream: an explicit second acknowledgement that you have confirmed the cold tier holds these files by some other means. --no-verify-upstream alone is refused.")
	fs.IntVar(&opts.maxFiles, "max-files", 100000, fmt.Sprintf("Hard cap on candidates per run, at most %d.", trimMaxFilesCeiling))
	if err := fs.Parse(args); err != nil {
		return trimOpts{}, err
	}
	opts.commit = opts.commit || gate.Enabled()
	opts.dryRun = gate.DryRunStated()
	// Unlike the shared gate, -write does not win over -dry-run here: the
	// write is an irreversible DELETE, so contradictory flags are refused.
	if opts.dryRun && opts.commit {
		return trimOpts{}, errors.New("--dry-run and --commit/--write are mutually exclusive; pick one")
	}
	if olderThan < 0 || olderThan > int64(^uint32(0)) {
		return trimOpts{}, fmt.Errorf("--older-than-ledger out of uint32 range: %d", olderThan)
	}
	if opts.maxFiles <= 0 || opts.maxFiles > trimMaxFilesCeiling {
		return trimOpts{}, fmt.Errorf("--max-files must be in 1..%d; got %d", trimMaxFilesCeiling, opts.maxFiles)
	}
	opts.olderThan = uint32(olderThan)
	opts.verifyUpstream = !noVerify
	if noVerify && !opts.iHaveVerifiedOutOfBand {
		return trimOpts{}, fmt.Errorf("--no-verify-upstream disables the primary safety primitive and requires --i-have-verified-cold-out-of-band as a second explicit acknowledgement — refusing to proceed with only one flag set")
	}
	return opts, nil
}

// splitBucketPath parses the SDK-style "bucket/prefix/path" form into (bucket, prefix), mirroring datastore/s3.go's FromS3Client.
func splitBucketPath(bucketPath string) (bucket, prefix string, err error) {
	parsed, err := url.Parse("s3://" + bucketPath)
	if err != nil {
		return "", "", err
	}
	return parsed.Host, strings.TrimPrefix(parsed.Path, "/"), nil
}

// buildS3Client builds a path-style (MinIO) *s3.Client; accessKeyEnv and secretKeyEnv are env-var NAMES, resolved at call time.
func buildS3Client(ctx context.Context, endpoint, region, accessKeyEnv, secretKeyEnv string) (*s3.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	// Prefer explicit creds from the env-var names cfg.Storage points at, matching the codebase's env naming convention.
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
