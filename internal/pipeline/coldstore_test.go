package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/stellar/go-stellar-sdk/support/datastore"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// r1's /etc/default/stellarindex exports MinIO's root credentials in AWS env-var form for the hot tier;
// these constants reproduce that, and a cold client built in such a process must ignore all four.
const (
	ambientMinIOAccessKey = "minioadmin-hot-tier-key"
	ambientMinIOSecretKey = "minioadmin-hot-tier-secret"
	ambientMinIOEndpoint  = "http://127.0.0.1:9000"
	ambientMinIORegion    = "r1"
)

// setAmbientMinIOCredentials reproduces r1's environment and asserts the ambient chain really resolves to MinIO's credentials;
// an empty ambient chain would make the test pass trivially against the broken code (the SDK goes anonymous only when Retrieve fails).
func setAmbientMinIOCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", ambientMinIOAccessKey)
	t.Setenv("AWS_SECRET_ACCESS_KEY", ambientMinIOSecretKey)
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_REGION", ambientMinIORegion)
	t.Setenv("AWS_DEFAULT_REGION", ambientMinIORegion)
	t.Setenv("AWS_ENDPOINT_URL", ambientMinIOEndpoint)
	// Keep the developer's ~/.aws out of the resolution chain so the
	// test means the same thing on a laptop and in CI.
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/no-such-config")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/no-such-credentials")

	ambient, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		t.Fatalf("precondition: LoadDefaultConfig: %v", err)
	}
	creds, err := ambient.Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("precondition: ambient AWS chain must resolve (that is what makes the SDK skip its anonymous "+
			"fallback and is the whole bug); Retrieve returned %v", err)
	}
	if creds.AccessKeyID != ambientMinIOAccessKey {
		t.Fatalf("precondition: ambient AccessKeyID = %q, want %q — the test environment is not reproducing r1",
			creds.AccessKeyID, ambientMinIOAccessKey)
	}
}

func coldOnlyStorage() config.StorageConfig {
	return config.StorageConfig{
		// Hot block = local MinIO, exactly as on r1.
		S3Endpoint:      ambientMinIOEndpoint,
		S3Region:        ambientMinIORegion,
		S3BucketArchive: "galexie-archive",
		S3BucketLive:    "galexie-live",
		S3AccessKeyEnv:  "STELLARINDEX_S3_ACCESS_KEY",
		S3SecretKeyEnv:  "STELLARINDEX_S3_SECRET_KEY",
		// Cold block = the public AWS Open Data bucket. Region
		// us-east-2 (us-east-1 301s).
		S3ColdEndpoint:      "https://s3.us-east-2.amazonaws.com",
		S3ColdRegion:        "us-east-2",
		S3ColdBucketArchive: "aws-public-blockchain/v1.1/stellar/ledgers/pubnet",
	}
}

// TestNewColdS3Client_AnonymousDespiteAmbientCredentials: with MinIO credentials in the environment, an anonymous-configured cold client must not sign with them
// (symptom: InvalidAccessKeyId), nor inherit the ambient endpoint or region, as config.LoadDefaultConfig would.
func TestNewColdS3Client_AnonymousDespiteAmbientCredentials(t *testing.T) {
	setAmbientMinIOCredentials(t)
	storage := coldOnlyStorage()

	provider, err := coldCredentials(storage)
	if err != nil {
		t.Fatalf("coldCredentials: %v", err)
	}
	if !aws.IsCredentialsProvider(provider, (*aws.AnonymousCredentials)(nil)) {
		got, retrieveErr := provider.Retrieve(context.Background())
		t.Fatalf("cold credential provider = %T (AccessKeyID=%q, err=%v), want aws.AnonymousCredentials — "+
			"empty s3_cold_*_key_env must mean anonymous, never the ambient hot-tier keys",
			provider, got.AccessKeyID, retrieveErr)
	}

	client, err := newColdS3Client(storage)
	if err != nil {
		t.Fatalf("newColdS3Client: %v", err)
	}
	opts := client.Options()
	// s3.New's ignoreAnonymousAuth replaces the anonymous sentinel with nil, which tells the signing middleware to leave
	// the request unsigned. A non-nil provider here means the client WILL sign with whatever it holds.
	if opts.Credentials != nil {
		got, retrieveErr := opts.Credentials.Retrieve(context.Background())
		t.Errorf("cold client Credentials = %T (AccessKeyID=%q, err=%v), want nil (anonymous)",
			opts.Credentials, got.AccessKeyID, retrieveErr)
	}
	if opts.BaseEndpoint == nil || *opts.BaseEndpoint != storage.S3ColdEndpoint {
		t.Errorf("cold client BaseEndpoint = %v, want %q — must not inherit AWS_ENDPOINT_URL=%q",
			aws.ToString(opts.BaseEndpoint), storage.S3ColdEndpoint, ambientMinIOEndpoint)
	}
	if opts.Region != storage.S3ColdRegion {
		t.Errorf("cold client Region = %q, want %q — must not inherit AWS_REGION=%q",
			opts.Region, storage.S3ColdRegion, ambientMinIORegion)
	}
	if !opts.UsePathStyle {
		t.Errorf("cold client UsePathStyle = false, want true (parity with the SDK's NewS3DataStore)")
	}
}

// TestColdDataStoreFactory_SendsUnsignedRequests drives the production wiring against an in-process S3 stand-in and asserts the bucket probe carries no Authorization header.
// The control routes the same config through the SDK's datastore.NewDataStore, which signs with the ambient MinIO key.
func TestColdDataStoreFactory_SendsUnsignedRequests(t *testing.T) {
	setAmbientMinIOCredentials(t)

	var (
		mu    sync.Mutex
		auths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` +
			`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
			`<Name>aws-public-blockchain</Name><Prefix>v1.1/stellar/ledgers/pubnet</Prefix>` +
			`<KeyCount>0</KeyCount><MaxKeys>1</MaxKeys><IsTruncated>false</IsTruncated>` +
			`</ListBucketResult>`))
	}))
	defer srv.Close()

	storage := coldOnlyStorage()
	storage.S3ColdEndpoint = srv.URL
	cfg := config.Config{
		Stellar: config.StellarConfig{Network: "pubnet"},
		Storage: storage,
	}

	lsCfg := LedgerstreamConfig(cfg, storage.S3BucketArchive)
	if lsCfg.ColdDataStoreFactory == nil {
		t.Fatal("LedgerstreamConfig did not wire ColdDataStoreFactory; ledgerstream would fall back to " +
			"datastore.NewDataStore and re-introduce the ambient-credential bug")
	}

	ctx := context.Background()
	cold, err := lsCfg.ColdDataStoreFactory(ctx)
	if err != nil {
		t.Fatalf("ColdDataStoreFactory: %v", err)
	}
	defer func() { _ = cold.Close() }()

	mu.Lock()
	fixed := append([]string(nil), auths...)
	auths = nil
	mu.Unlock()

	if len(fixed) == 0 {
		t.Fatal("cold datastore construction issued no request to the stand-in endpoint")
	}
	for i, a := range fixed {
		if a != "" {
			t.Errorf("cold request %d carried Authorization=%q, want unsigned (anonymous) — a signed cold "+
				"request is the InvalidAccessKeyId production failure", i, a)
		}
	}

	// Control: the SDK path this fix replaces, same config, same
	// endpoint. If this stops signing, the test above has lost its
	// teeth and the assertion must be re-derived.
	sdkCold, err := datastore.NewDataStore(ctx, lsCfg.ColdDataStore)
	if err != nil {
		t.Fatalf("control: datastore.NewDataStore: %v", err)
	}
	defer func() { _ = sdkCold.Close() }()

	mu.Lock()
	viaSDK := append([]string(nil), auths...)
	mu.Unlock()

	if len(viaSDK) == 0 {
		t.Fatal("control: SDK path issued no request")
	}
	if !strings.Contains(viaSDK[0], "Credential="+ambientMinIOAccessKey+"/") {
		t.Fatalf("control: SDK path Authorization=%q does not carry the ambient MinIO key — the test no longer "+
			"reproduces the bug it guards against, so the assertions above are vacuous", viaSDK[0])
	}
}

// TestColdCredentials_StaticFromNamedEnvVars covers the private-bucket shape: credentials come from the
// s3_cold_*_key_env variables at call time, not from the ambient AWS_* pair, which is present and different.
func TestColdCredentials_StaticFromNamedEnvVars(t *testing.T) {
	setAmbientMinIOCredentials(t)
	t.Setenv("STELLARINDEX_S3_COLD_ACCESS_KEY", "cold-tier-access-key")
	t.Setenv("STELLARINDEX_S3_COLD_SECRET_KEY", "cold-tier-secret-key")

	storage := coldOnlyStorage()
	storage.S3ColdAccessKeyEnv = "STELLARINDEX_S3_COLD_ACCESS_KEY"
	storage.S3ColdSecretKeyEnv = "STELLARINDEX_S3_COLD_SECRET_KEY"

	provider, err := coldCredentials(storage)
	if err != nil {
		t.Fatalf("coldCredentials: %v", err)
	}
	creds, err := provider.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if creds.AccessKeyID != "cold-tier-access-key" {
		t.Errorf("AccessKeyID = %q, want %q (ambient AWS_ACCESS_KEY_ID was %q)",
			creds.AccessKeyID, "cold-tier-access-key", ambientMinIOAccessKey)
	}
	if creds.SecretAccessKey != "cold-tier-secret-key" {
		t.Errorf("SecretAccessKey = %q, want %q", creds.SecretAccessKey, "cold-tier-secret-key")
	}
	if creds.SessionToken != "" {
		t.Errorf("SessionToken = %q, want empty", creds.SessionToken)
	}

	client, err := newColdS3Client(storage)
	if err != nil {
		t.Fatalf("newColdS3Client: %v", err)
	}
	clientCreds, err := client.Options().Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatalf("client Retrieve: %v", err)
	}
	if clientCreds.AccessKeyID != "cold-tier-access-key" {
		t.Errorf("cold client AccessKeyID = %q, want %q", clientCreds.AccessKeyID, "cold-tier-access-key")
	}
}

// TestColdCredentials_NamedButUnsetEnvFailsLoudly pins fail-closed: named env vars mean a private bucket, so a missing value must refuse, not retry anonymously.
func TestColdCredentials_NamedButUnsetEnvFailsLoudly(t *testing.T) {
	cases := []struct {
		name       string
		accessVal  string
		secretVal  string
		wantNamed  string
		wantAbsent string
	}{
		{"both unset", "", "", "STELLARINDEX_S3_COLD_ACCESS_KEY and STELLARINDEX_S3_COLD_SECRET_KEY", ""},
		{"access unset", "", "s", "STELLARINDEX_S3_COLD_ACCESS_KEY", "STELLARINDEX_S3_COLD_SECRET_KEY is unset"},
		{"secret unset", "a", "", "STELLARINDEX_S3_COLD_SECRET_KEY", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("STELLARINDEX_S3_COLD_ACCESS_KEY", c.accessVal)
			t.Setenv("STELLARINDEX_S3_COLD_SECRET_KEY", c.secretVal)

			storage := coldOnlyStorage()
			storage.S3ColdAccessKeyEnv = "STELLARINDEX_S3_COLD_ACCESS_KEY"
			storage.S3ColdSecretKeyEnv = "STELLARINDEX_S3_COLD_SECRET_KEY"

			provider, err := coldCredentials(storage)
			if err == nil {
				t.Fatalf("coldCredentials returned provider %T and no error; want an error rather than a "+
					"silent downgrade to anonymous", provider)
			}
			if !strings.Contains(err.Error(), c.wantNamed) {
				t.Errorf("error %q does not name the empty env var %q", err, c.wantNamed)
			}
			if _, cerr := newColdS3Client(storage); cerr == nil {
				t.Error("newColdS3Client returned no error; the credential failure must propagate")
			}
			if _, derr := NewColdDataStore(context.Background(), storage); derr == nil {
				t.Error("NewColdDataStore returned no error; the credential failure must propagate")
			}
		})
	}
}

// TestColdCredentials_HalfConfiguredPairIsRejected is the belt to
// config validation's braces: NewColdDataStore is reachable from
// callers that build a StorageConfig in code, so it must not guess.
func TestColdCredentials_HalfConfiguredPairIsRejected(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, access, secret string }{
		{"access only", "STELLARINDEX_S3_COLD_ACCESS_KEY", ""},
		{"secret only", "", "STELLARINDEX_S3_COLD_SECRET_KEY"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			storage := coldOnlyStorage()
			storage.S3ColdAccessKeyEnv = c.access
			storage.S3ColdSecretKeyEnv = c.secret
			provider, err := coldCredentials(storage)
			if err == nil {
				t.Fatalf("coldCredentials returned %T and no error for a half-configured pair", provider)
			}
			if !strings.Contains(err.Error(), "must be set together") {
				t.Errorf("error %q should explain the both-or-neither rule", err)
			}
		})
	}
}

// TestNewColdDataStore_RequiresBucket guards the disabled-tier case:
// with no cold bucket configured there is nothing to build, and
// FromS3Client would otherwise probe a bucket named "".
func TestNewColdDataStore_RequiresBucket(t *testing.T) {
	t.Parallel()
	storage := coldOnlyStorage()
	storage.S3ColdBucketArchive = ""
	if _, err := NewColdDataStore(context.Background(), storage); err == nil {
		t.Fatal("NewColdDataStore returned no error with an empty s3_cold_bucket_archive")
	}
}

// TestNewColdDataStore_RequiresRegion: a bucket without a region must be rejected by name here, not fail inside the SDK's endpoint-rule evaluation after the bucket probe.
// The empty-endpoint case lives in config.TestValidate_RejectsBadFields, since reproducing it would reach live AWS.
func TestNewColdDataStore_RequiresRegion(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` +
			`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
			`<Name>aws-public-blockchain</Name><Prefix>v1.1/stellar/ledgers/pubnet</Prefix>` +
			`<KeyCount>0</KeyCount><MaxKeys>1</MaxKeys><IsTruncated>false</IsTruncated>` +
			`</ListBucketResult>`))
	}))
	defer srv.Close()

	storage := coldOnlyStorage()
	storage.S3ColdEndpoint = srv.URL
	storage.S3ColdRegion = ""

	_, err := NewColdDataStore(context.Background(), storage)
	if err == nil {
		t.Fatal("NewColdDataStore returned no error with an empty s3_cold_region alongside a set " +
			"s3_cold_bucket_archive")
	}
	if !strings.Contains(err.Error(), "s3_cold_region") {
		t.Errorf("err = %q, want it to name storage.s3_cold_region directly (not the AWS SDK's "+
			"generic endpoint-resolution failure three layers down)", err)
	}
}
