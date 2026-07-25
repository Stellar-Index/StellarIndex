package pipeline

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stellar/go-stellar-sdk/support/datastore"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// ─── ADR-0027 cold-tier datastore construction ───────────────────
//
// Why this exists instead of datastore.NewDataStore (2026-07-25
// incident, "the cold tier could never authenticate"):
//
// The vendored SDK builds EVERY S3 datastore through the AWS
// default credential chain —
// go-stellar-sdk@v0.6.0/support/datastore/s3.go:
//
//	cfg, err := config.LoadDefaultConfig(ctx)
//	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
//	    ...
//	    _, err := cfg.Credentials.Retrieve(ctx)
//	    if err != nil { o.Credentials = aws.AnonymousCredentials{} }
//	    ...
//	})
//
// — i.e. it degrades to anonymous ONLY when the ambient chain is
// completely empty. That is fine for a process with one S3 backend
// and fatal for ours, which has two with DIFFERENT credentials:
//
//   - HOT is local MinIO. r1's /etc/default/stellarindex exports
//     MinIO's root credentials in the standard AWS form
//     (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_REGION /
//     AWS_ENDPOINT_URL) because the hot datastore authenticates
//     through exactly that default chain.
//   - COLD is aws-public-blockchain, a public AWS Open Data bucket
//     that wants NO credentials.
//
// So on r1 cfg.Credentials.Retrieve() SUCCEEDS (it hands back
// MinIO's keys), the anonymous fallback never fires, and the cold
// client presents MinIO's access key to real AWS. Enabling the
// tier on r1 fails every read with:
//
//	InvalidAccessKeyId: The AWS Access Key Id you provided does not
//	exist in our records
//
// This went unnoticed for the tier's whole life because
// ledgerstream's cold-init failure is non-fatal by design (WARN +
// degrade to hot-only), so a permanently-broken tier looked like a
// quiet log line.
//
// datastore.FromS3Client is exported, so the correct fix is to
// build the cold *s3.Client ourselves and hand it over, which is
// what NewColdDataStore does. Note we deliberately do NOT call
// config.LoadDefaultConfig at all: it resolves AWS_ENDPOINT_URL
// into cfg.BaseEndpoint too, so a cold tier configured without an
// explicit endpoint would silently inherit r1's MinIO endpoint —
// the same disease one layer down. A cold client must inherit
// NOTHING from the ambient environment; every knob comes from the
// storage.s3_cold_* config block.

// NewColdDataStore builds the ADR-0027 cold-tier DataStore
// (read-only historical LCM upstream) from the storage.s3_cold_*
// config block, with credentials resolved explicitly rather than
// from the AWS SDK's ambient default chain. See the package-level
// note above for why the SDK's datastore.NewDataStore cannot be
// used here.
//
// Behaviourally identical to the SDK's NewS3DataStore apart from
// credential/endpoint resolution: same path-style addressing, same
// region plumbing, and the same datastore.FromS3Client tail — so
// the SDK's bucket-probe diagnostics (301 PermanentRedirect =>
// "wrong region", NoSuchBucket, AccessDenied) are preserved
// verbatim.
func NewColdDataStore(ctx context.Context, storage config.StorageConfig) (datastore.DataStore, error) {
	if storage.S3ColdBucketArchive == "" {
		return nil, fmt.Errorf("cold datastore: storage.s3_cold_bucket_archive is empty (cold tiering disabled)")
	}
	client, err := newColdS3Client(storage)
	if err != nil {
		return nil, err
	}
	return datastore.FromS3Client(ctx, client, storage.S3ColdBucketArchive)
}

// newColdS3Client builds the cold tier's *s3.Client from a
// zero-valued aws.Config so nothing leaks in from the process
// environment (see the note above). The two checksum knobs are set
// to the values config.LoadDefaultConfig would have resolved when
// no AWS_*_CHECKSUM_* env/profile setting is present, keeping
// on-the-wire behaviour identical to the SDK's own client; the
// option func mirrors NewS3DataStore's exactly.
func newColdS3Client(storage config.StorageConfig) (*s3.Client, error) {
	creds, err := coldCredentials(storage)
	if err != nil {
		return nil, err
	}
	awsCfg := aws.Config{
		Region:                     storage.S3ColdRegion,
		Credentials:                creds,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenSupported,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenSupported,
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if storage.S3ColdEndpoint != "" {
			o.BaseEndpoint = aws.String(storage.S3ColdEndpoint)
		}
		o.Region = storage.S3ColdRegion
		o.UsePathStyle = true
	}), nil
}

// coldCredentials resolves the cold tier's credential provider from
// the storage.s3_cold_{access,secret}_key_env fields. Like their hot
// counterparts (s3_access_key_env / s3_secret_key_env) these hold the
// NAME of an env var, never the credential itself.
//
//   - both names empty → aws.AnonymousCredentials{}. This is the
//     production shape: aws-public-blockchain is public-read.
//   - both names set → static credentials read from those env vars
//     at call time (so the binary picks up whatever the systemd
//     EnvironmentFile exports, matching buildS3Client's contract).
//     A named-but-unset/empty env var is a HARD ERROR, never a
//     silent downgrade to anonymous: "private bucket quietly read
//     anonymously" is precisely the failure shape this whole file
//     exists to eliminate, and it would surface as a confusing
//     AccessDenied hundreds of ledgers later instead of at startup.
//   - exactly one name set → error. config.StorageConfig.validate
//     rejects this at load time; the check is repeated here because
//     NewColdDataStore is reachable from callers that build a
//     StorageConfig by hand (tests, future operators).
func coldCredentials(storage config.StorageConfig) (aws.CredentialsProvider, error) {
	accessEnv, secretEnv := storage.S3ColdAccessKeyEnv, storage.S3ColdSecretKeyEnv
	switch {
	case accessEnv == "" && secretEnv == "":
		return aws.AnonymousCredentials{}, nil
	case accessEnv == "" || secretEnv == "":
		return nil, fmt.Errorf(
			"cold datastore: storage.s3_cold_access_key_env (%q) and storage.s3_cold_secret_key_env (%q) must be set together — "+
				"both empty means anonymous reads (correct for a public bucket), both set means static credentials",
			accessEnv, secretEnv)
	}
	accessKey, secretKey := os.Getenv(accessEnv), os.Getenv(secretEnv)
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf(
			"cold datastore: storage.s3_cold_access_key_env names %q and storage.s3_cold_secret_key_env names %q, "+
				"but %s is unset or empty in the environment — refusing to fall back to anonymous reads on a bucket "+
				"the operator configured credentials for",
			accessEnv, secretEnv, emptyEnvNames(accessEnv, accessKey, secretEnv, secretKey))
	}
	return credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""), nil
}

// emptyEnvNames renders which of the two named env vars actually
// came back empty, so the operator does not have to bisect.
func emptyEnvNames(accessEnv, accessKey, secretEnv, secretKey string) string {
	switch {
	case accessKey == "" && secretKey == "":
		return accessEnv + " and " + secretEnv
	case accessKey == "":
		return accessEnv
	default:
		return secretEnv
	}
}
