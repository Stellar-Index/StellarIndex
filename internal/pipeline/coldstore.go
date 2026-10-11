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

// ADR-0027 cold-tier datastore construction. The SDK builds every S3 datastore via the AWS default credential chain, falling back to anonymous only if the chain is EMPTY.
// On r1 the chain holds MinIO's keys, so an SDK-built cold client would sign real-AWS requests with them (InvalidAccessKeyId), and ledgerstream's non-fatal cold-init failure hides it as a log line.
// So NewColdDataStore uses datastore.FromS3Client and never config.LoadDefaultConfig (it also resolves AWS_ENDPOINT_URL): the cold client inherits NOTHING from the environment.

// NewColdDataStore builds the ADR-0027 cold-tier DataStore from storage.s3_cold_* with explicit credentials, not the SDK's ambient chain (see the note above).
// Otherwise identical to NewS3DataStore (path-style, region, FromS3Client tail), so the SDK's bucket-probe diagnostics are preserved.
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
	// Repeat the region check: config validation rejects this at load, but hand-built StorageConfigs reach here, and a zero Region cannot be SigV4-signed.
	if storage.S3ColdRegion == "" {
		return nil, fmt.Errorf("cold datastore: storage.s3_cold_region is empty (required when s3_cold_bucket_archive is set)")
	}
	if storage.S3ColdEndpoint == "" {
		return nil, fmt.Errorf("cold datastore: storage.s3_cold_endpoint is empty (required when s3_cold_bucket_archive is set)")
	}
	return newColdS3Client(storage)
}

// newColdS3Client builds the *s3.Client from a zero-valued aws.Config so nothing leaks in from the environment.
// The checksum knobs match what config.LoadDefaultConfig would resolve, keeping wire behaviour identical to the SDK's client.
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

// coldCredentials resolves the cold tier's credentials from storage.s3_cold_{access,secret}_key_env, which hold env var NAMES, never the secret.
// Both empty: anonymous (public-read bucket). Both set: static creds read at call time; a named-but-unset var is a HARD ERROR, never a silent downgrade to anonymous.
// Exactly one set: error (validation rejects it at load; hand-built configs reach here too).
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
