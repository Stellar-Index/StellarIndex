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

// ADR-0027 cold-tier datastore construction.
//
// This exists instead of datastore.NewDataStore because, through the SDK
// constructor, the cold tier can never authenticate: the SDK builds EVERY S3
// datastore via the AWS default credential chain and falls back to anonymous
// ONLY when the ambient chain is completely empty. We have two S3 backends
// with DIFFERENT credentials: HOT is local MinIO, whose root credentials r1
// exports in the standard AWS form (AWS_ACCESS_KEY_ID etc.); COLD is the
// public aws-public-blockchain bucket, which wants none. So on r1 the chain
// SUCCEEDS, the anonymous fallback never fires, and an SDK-built cold client
// presents MinIO's key to real AWS (InvalidAccessKeyId). ledgerstream's
// cold-init failure is non-fatal by design (WARN, degrade to hot-only), so
// the broken tier looks like a quiet log line.
//
// datastore.FromS3Client is exported, so NewColdDataStore builds the cold
// *s3.Client itself. It deliberately does NOT call config.LoadDefaultConfig:
// that also resolves AWS_ENDPOINT_URL into cfg.BaseEndpoint, so a cold tier
// without an explicit endpoint would inherit r1's MinIO endpoint. A cold
// client must inherit NOTHING from the ambient environment; every knob comes
// from the storage.s3_cold_* config block.

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
	client, err := NewColdS3Client(storage)
	if err != nil {
		return nil, err
	}
	return datastore.FromS3Client(ctx, client, storage.S3ColdBucketArchive)
}

// NewColdS3Client returns the cold tier's raw *s3.Client, for callers
// that need object metadata (ETag, Last-Modified) the DataStore hides.
func NewColdS3Client(storage config.StorageConfig) (*s3.Client, error) {
	if storage.S3ColdBucketArchive == "" {
		return nil, fmt.Errorf("cold datastore: storage.s3_cold_bucket_archive is empty (cold tiering disabled)")
	}
	// config.StorageConfig.validate rejects a bucket set without its
	// region/endpoint at load time; the check is repeated here for the
	// same reason coldCredentials repeats the key-pair check below —
	// this func is reachable from callers that build a StorageConfig by
	// hand (tests, future operators). Without it a bare bucket reaches
	// newColdS3Client with a zero-value Region, which SigV4 cannot sign
	// against.
	if storage.S3ColdRegion == "" {
		return nil, fmt.Errorf("cold datastore: storage.s3_cold_region is empty (required when s3_cold_bucket_archive is set)")
	}
	if storage.S3ColdEndpoint == "" {
		return nil, fmt.Errorf("cold datastore: storage.s3_cold_endpoint is empty (required when s3_cold_bucket_archive is set)")
	}
	return newColdS3Client(storage)
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
