package v1_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// Self-service key-mint quota.
//
// POST /v1/account/keys minted on every call with no count check, so
// one authenticated caller could mint live credentials in a loop until
// the store filled. These pin the ceiling, its wire shape, and the two
// boundaries (one under, disabled).

// newAccountQuotaTestServer wires the self-service surface with an
// explicit per-identifier key quota.
func newAccountQuotaTestServer(t *testing.T, subject auth.Subject, store v1.AccountStore, quota int) *httptest.Server {
	t.Helper()
	srv := v1.New(v1.Options{
		Auth:            fakeAuthMiddleware(subject),
		Accounts:        store,
		AccountKeyQuota: quota,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// seedKeys builds n active key records for identifier.
func seedKeys(identifier string, n int) []auth.APIKeyRecord {
	out := make([]auth.APIKeyRecord, 0, n)
	for i := range n {
		out = append(out, auth.APIKeyRecord{
			KeyID:      fmt.Sprintf("kid_%s_%d", identifier, i),
			Identifier: identifier,
			Tier:       auth.TierAPIKey,
		})
	}
	return out
}

// TestAccountKeysCreate_QuotaExceeded is the core regression: a caller
// already at the ceiling is refused with 409 and the store's Create is
// never reached.
func TestAccountKeysCreate_QuotaExceeded(t *testing.T) {
	store := &fakeAccountStore{
		listed: map[string][]auth.APIKeyRecord{"owner-42": seedKeys("owner-42", 3)},
	}
	ts := newAccountQuotaTestServer(t, auth.Subject{
		Identifier: "owner-42",
		Tier:       auth.TierAPIKey,
	}, store, 3)

	resp, err := http.Post(ts.URL+"/v1/account/keys", "application/json",
		strings.NewReader(`{"label":"one-too-many"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (quota reached)", resp.StatusCode)
	}
	if store.calls != 0 {
		t.Errorf("Create was called %d times; a refused mint must never issue a credential", store.calls)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var problem struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Type != "https://api.stellarindex.io/errors/key-quota-exceeded" {
		t.Errorf("problem type = %q, want .../errors/key-quota-exceeded", problem.Type)
	}
	if problem.Status != http.StatusConflict {
		t.Errorf("problem status = %d, want 409", problem.Status)
	}
	if !strings.Contains(problem.Detail, "3 active API keys") || !strings.Contains(problem.Detail, "max 3") {
		t.Errorf("problem detail = %q, want the current count + ceiling so the caller knows what to revoke", problem.Detail)
	}
}

// TestAccountKeysCreate_UnderQuotaMints pins the boundary below the
// ceiling: the mint still happens, and revoked records do not consume
// quota.
func TestAccountKeysCreate_UnderQuotaMints(t *testing.T) {
	keys := seedKeys("owner-42", 3)
	keys[0].RevokedAt = keys[0].CreatedAt.AddDate(0, 0, 1) // revoked — must not count
	store := &fakeAccountStore{
		listed: map[string][]auth.APIKeyRecord{"owner-42": keys},
		rec:    auth.APIKeyRecord{KeyID: "kid_new", Label: "fresh"},
		plain:  "sip_freshplaintext",
	}
	ts := newAccountQuotaTestServer(t, auth.Subject{
		Identifier: "owner-42",
		Tier:       auth.TierAPIKey,
	}, store, 3)

	resp, err := http.Post(ts.URL+"/v1/account/keys", "application/json",
		strings.NewReader(`{"label":"fresh"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (2 active < quota 3)", resp.StatusCode)
	}
	if store.calls != 1 {
		t.Errorf("Create calls = %d, want 1", store.calls)
	}
}

// TestAccountKeysCreate_QuotaDefaultApplies pins that an unset
// Options.AccountKeyQuota still enforces a ceiling — the whole defect
// was an unbounded default.
func TestAccountKeysCreate_QuotaDefaultApplies(t *testing.T) {
	store := &fakeAccountStore{
		listed: map[string][]auth.APIKeyRecord{"owner-42": seedKeys("owner-42", 25)},
	}
	ts := newAccountTestServer(t, auth.Subject{
		Identifier: "owner-42",
		Tier:       auth.TierAPIKey,
	}, store)

	resp, err := http.Post(ts.URL+"/v1/account/keys", "application/json",
		strings.NewReader(`{"label":"twenty-sixth"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (default quota of 25 reached)", resp.StatusCode)
	}
	if store.calls != 0 {
		t.Errorf("Create calls = %d, want 0", store.calls)
	}
}

// TestAccountKeysCreate_QuotaDisabled pins the operator escape hatch: a
// negative quota turns the check off entirely (and skips the count read).
func TestAccountKeysCreate_QuotaDisabled(t *testing.T) {
	store := &fakeAccountStore{
		listed: map[string][]auth.APIKeyRecord{"owner-42": seedKeys("owner-42", 500)},
		rec:    auth.APIKeyRecord{KeyID: "kid_new"},
		plain:  "sip_x",
	}
	ts := newAccountQuotaTestServer(t, auth.Subject{
		Identifier: "owner-42",
		Tier:       auth.TierAPIKey,
	}, store, -1)

	resp, err := http.Post(ts.URL+"/v1/account/keys", "application/json",
		strings.NewReader(`{"label":"unbounded-by-config"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (quota disabled)", resp.StatusCode)
	}
	if store.listCalls != 0 {
		t.Errorf("list calls = %d, want 0 (disabled check must not read the store)", store.listCalls)
	}
}

// TestAccountKeysCreate_QuotaReadFailureFailsClosed pins the direction
// of the degrade: if the count can't be read, the mint is refused. The
// check exists because an unbounded mint is the abuse, so "couldn't
// count, mint anyway" would hand the abuser the bypass.
func TestAccountKeysCreate_QuotaReadFailureFailsClosed(t *testing.T) {
	store := &fakeAccountStore{listErr: errors.New("redis blip")}
	ts := newAccountQuotaTestServer(t, auth.Subject{
		Identifier: "owner-42",
		Tier:       auth.TierAPIKey,
	}, store, 3)

	resp, err := http.Post(ts.URL+"/v1/account/keys", "application/json",
		strings.NewReader(`{"label":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (fail closed)", resp.StatusCode)
	}
	if store.calls != 0 {
		t.Errorf("Create calls = %d, want 0 — a mint must not proceed on an unverifiable quota", store.calls)
	}
}

// TestAccountKeysCreate_SEP10SubjectRefused: a SEP-10 identifier is a
// keypair anyone can generate offline, so the per-identifier cap does not
// bound it, and sep10 mode never honours the minted key anyway. The mint
// is refused before the store is touched, even with the quota disabled.
func TestAccountKeysCreate_SEP10SubjectRefused(t *testing.T) {
	store := &fakeAccountStore{
		rec:   auth.APIKeyRecord{KeyID: "kid_new"},
		plain: "sip_shouldnotissue",
	}
	ts := newAccountQuotaTestServer(t, auth.Subject{
		Identifier: "GAB123",
		Tier:       auth.TierSEP10,
	}, store, -1)

	resp, err := http.Post(ts.URL+"/v1/account/keys", "application/json",
		strings.NewReader(`{"label":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (SEP-10 subjects are not issued API keys)", resp.StatusCode)
	}
	if store.calls != 0 || store.listCalls != 0 {
		t.Errorf("store Create calls = %d, list calls = %d; want 0 — a refused mint must not reach the store",
			store.calls, store.listCalls)
	}
	var problem struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Type != "https://api.stellarindex.io/errors/key-mint-not-available" {
		t.Errorf("problem type = %q, want .../errors/key-mint-not-available", problem.Type)
	}
}

// TestAccountKeysCreate_OperatorRequiresReason — an operator-tier
// self-mint without X-Reason is refused BEFORE the store is touched,
// mirroring /v1/admin/keys. The store's Create is never called.
func TestAccountKeysCreate_OperatorRequiresReason(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_child"}, plain: "sip_child"}
	sink := &recordingAuditSink{}
	ts := newAdminTestServer(t, operatorSelfSubject(), store, sink)

	resp := doWithReason(t, http.MethodPost, ts.URL+"/v1/account/keys", "", `{"label":"rotate"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (operator self-mint without X-Reason)", resp.StatusCode)
	}
	if store.calls != 0 {
		t.Errorf("Create called %d times, want 0 (no reason, no credential)", store.calls)
	}
	if len(sink.entries) != 0 {
		t.Errorf("audit entries = %d, want 0", len(sink.entries))
	}
}

// TestAccountKeysCreate_OperatorSelfMintIsAudited — with X-Reason the
// operator keeps tier inheritance (the documented rotation contract) and
// the mint lands one key.mint audit row naming the actor key, the minted
// key and the reason.
func TestAccountKeysCreate_OperatorSelfMintIsAudited(t *testing.T) {
	store := &fakeAccountStore{
		rec:   auth.APIKeyRecord{KeyID: "kid_child", Label: "rotate", Tier: auth.TierOperator},
		plain: "sip_child",
	}
	sink := &recordingAuditSink{}
	ts := newAdminTestServer(t, operatorSelfSubject(), store, sink)

	resp := doWithReason(t, http.MethodPost, ts.URL+"/v1/account/keys", "quarterly rotation", `{"label":"rotate"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if store.gotReq.Tier != auth.TierOperator {
		t.Errorf("Create.Tier = %q, want operator (rotation keeps tier inheritance)", store.gotReq.Tier)
	}
	if store.gotReq.Identifier != "operator:staff-1" {
		t.Errorf("Create.Identifier = %q, want the caller's own identifier", store.gotReq.Identifier)
	}
	if len(sink.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1 key.mint row for an operator self-mint", len(sink.entries))
	}
	e := sink.entries[0]
	if e.Action != "key.mint" || e.ActorKind != platform.ActorStaff ||
		e.TargetKind != "api_key" || e.TargetID != "kid_child" {
		t.Errorf("audit entry = %+v", e)
	}
	for _, want := range []string{`"actor_key_id":"kid_operator1"`, `"reason":"quarterly rotation"`, `"route":"/v1/account/keys"`, `"tier":"operator"`} {
		if !strings.Contains(string(e.Metadata), want) {
			t.Errorf("audit metadata missing %s: %s", want, e.Metadata)
		}
	}
	if e.Timestamp.IsZero() || e.UserAgent == "" {
		t.Errorf("audit entry missing request stamps: ts=%v ua=%q", e.Timestamp, e.UserAgent)
	}
}

// TestAccountKeysCreate_CustomerNeedsNoReason pins the blast radius: a
// customer-tier caller is NOT an admin write — no X-Reason required, no
// staff audit row.
func TestAccountKeysCreate_CustomerNeedsNoReason(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_c"}, plain: "sip_c"}
	sink := &recordingAuditSink{}
	ts := newAdminTestServer(t, auth.Subject{Identifier: "owner-42", Tier: auth.TierAPIKey, KeyID: "kid_owner"}, store, sink)

	resp := doWithReason(t, http.MethodPost, ts.URL+"/v1/account/keys", "", `{"label":"ci"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (customer mint unchanged)", resp.StatusCode)
	}
	if len(sink.entries) != 0 {
		t.Errorf("audit entries = %d, want 0 for a customer self-mint", len(sink.entries))
	}
}

// TestAccountKeysCreate_ChildInheritsExpiry — POST /v1/account/keys from a
// time-boxed key mints a child with the same expiry, not a permanent one.
func TestAccountKeysCreate_ChildInheritsExpiry(t *testing.T) {
	parentExpiry := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)

	sub := mintChildKeySubject(t, auth.Subject{
		Identifier: "staff-rotation",
		Tier:       auth.TierAPIKey,
		KeyID:      "kid_parent",
		ExpiresAt:  parentExpiry,
	})
	if !sub.ExpiresAt.Equal(parentExpiry) {
		t.Fatalf("child Subject.ExpiresAt = %v, want %v (inherited from the time-boxed parent; "+
			"zero means the child never expires)", sub.ExpiresAt, parentExpiry)
	}
}

// TestAccountKeysCreate_IdempotencyKeyReplaysInsteadOfMinting: the SDK's
// CreateKey retried after a client timeout, with the same Idempotency-Key,
// must get the original key back rather than a second live credential.
func TestAccountKeysCreate_IdempotencyKeyReplaysInsteadOfMinting(t *testing.T) {
	store := &fakeAccountStore{
		rec:   auth.APIKeyRecord{KeyID: "kid_first", KeyPrefix: "fx_first", Label: "ci"},
		plain: "fixture-plaintext-one",
	}
	ts := newAccountQuotaTestServer(t, auth.Subject{
		Identifier: "owner-7",
		KeyID:      "kid_caller",
		Tier:       auth.TierAPIKey,
	}, store, 25)

	post := func() (int, string, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/account/keys", strings.NewReader(`{"label":"ci"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "sdk-retry-0001")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var env struct {
			Data struct {
				Plaintext string `json:"plaintext"`
			} `json:"data"`
		}
		_ = json.Unmarshal(raw, &env)
		return resp.StatusCode, env.Data.Plaintext, resp.Header.Get("Idempotency-Replayed")
	}

	status1, plain1, _ := post()
	if status1 != http.StatusCreated {
		t.Fatalf("first mint status = %d, want 201", status1)
	}
	// A second mint would return this instead — so a non-replayed retry
	// is visible in the body as well as in the store's call count.
	store.plain = "fixture-plaintext-two"

	status2, plain2, replayed := post()
	if status2 != http.StatusCreated || plain2 != plain1 || replayed != "true" {
		t.Errorf("retry: status %d plaintext %q replayed %q; want 201 replaying %q", status2, plain2, replayed, plain1)
	}
	if store.calls != 1 {
		t.Errorf("store minted %d keys for two requests sharing one Idempotency-Key, want 1", store.calls)
	}
}

// TestAccountKeysCreate_ChildInheritsMonthlyQuota — a child minted from
// a metered key is metered at the same ceiling. Proven red on the
// unfixed tree: Subject.MonthlyQuota came back 0.
func TestAccountKeysCreate_ChildInheritsMonthlyQuota(t *testing.T) {
	const parentQuota int64 = 250_000

	sub := mintChildKeySubject(t, auth.Subject{
		Identifier:   "acct-metered",
		Tier:         auth.TierAPIKey,
		KeyID:        "kid_parent",
		MonthlyQuota: parentQuota,
	})
	if sub.MonthlyQuota != parentQuota {
		t.Fatalf("child Subject.MonthlyQuota = %d, want %d (inherited from the metered parent; "+
			"a zero cap short-circuits middleware.MonthlyQuota and the child bills unmetered)",
			sub.MonthlyQuota, parentQuota)
	}
}

// TestAccountKeysCreate_UncappedParentMintsUncappedChild is the
// negative pin the fix must not cross: the ceiling is OPT-IN, so a
// caller without one must not have one invented for its child. Guards
// against "default the child to the tier ceiling", which would convert
// the documented opt-in contract into on-by-default and 429 keys that
// never had a cap.
func TestAccountKeysCreate_UncappedParentMintsUncappedChild(t *testing.T) {
	sub := mintChildKeySubject(t, auth.Subject{
		Identifier: "acct-uncapped",
		Tier:       auth.TierAPIKey,
		KeyID:      "kid_parent",
	})
	if sub.MonthlyQuota != 0 {
		t.Fatalf("child Subject.MonthlyQuota = %d, want 0 for an uncapped parent (the cap is opt-in; "+
			"inventing one here 429s keys that never had a ceiling)", sub.MonthlyQuota)
	}
}

// TestAccountKeysCreate_SEP10TierForbidden — a SEP-10 subject is not an
// account and must not mint child keys; the store is never touched.
func TestAccountKeysCreate_SEP10TierForbidden(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_child"}, plain: "sip_child"}
	subject := auth.Subject{
		Identifier: "GCLIENTWALLETADDRESS",
		Tier:       auth.TierSEP10,
	}
	ts := newAdminTestServer(t, subject, store, &recordingAuditSink{})

	resp := doWithReason(t, http.MethodPost, ts.URL+"/v1/account/keys", "", `{"label":"wallet"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (sep10 subject minting a key)", resp.StatusCode)
	}
	if store.calls != 0 {
		t.Errorf("Create called %d times, want 0", store.calls)
	}
}

// TestAccountKeysCreate_APIKeyTierStillMints — the tier gate admits the
// customer tier.
func TestAccountKeysCreate_APIKeyTierStillMints(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_child"}, plain: "sip_child"}
	subject := auth.Subject{
		Identifier: "acct:customer-1",
		Tier:       auth.TierAPIKey,
		KeyID:      "kid_parent",
	}
	ts := newAdminTestServer(t, subject, store, &recordingAuditSink{})

	resp := doWithReason(t, http.MethodPost, ts.URL+"/v1/account/keys", "", `{"label":"rotate"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if store.calls != 1 {
		t.Errorf("Create called %d times, want 1", store.calls)
	}
}

// TestAccountKeysCreate_ChildRecordHasIdleTTL — the record behind a key
// minted through the endpoint carries a Redis TTL, so an abandoned
// self-service key cannot occupy the keyspace forever.
func TestAccountKeysCreate_ChildRecordHasIdleTTL(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	subject := auth.Subject{Identifier: "acct:customer-ttl", Tier: auth.TierAPIKey, KeyID: "kid_parent"}
	ts := newAccountTestServer(t, subject, auth.NewRedisAPIKeyStore(rdb))
	resp := postJSONNoReason(t, ts.URL+"/v1/account/keys", `{"label":"rotated"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var body struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode mint response: %v", err)
	}
	sum := sha256.Sum256([]byte(body.Data.Plaintext))
	key := cachekeys.APIKey(hex.EncodeToString(sum[:])).String()
	if !mr.Exists(key) {
		t.Fatalf("minted record %s not found", key)
	}
	if ttl := mr.TTL(key); ttl <= 0 || ttl > auth.MirroredKeyIdleTTL {
		t.Fatalf("minted record TTL = %s, want in (0, %s]", ttl, auth.MirroredKeyIdleTTL)
	}
}

// TestAccountKeysCreate_InheritsEmailVerification — the child carries
// the parent's verification stamp into the store request. Proven red on
// origin/main: CreateAPIKeyRequest had no such field and the child was
// born unverified.
func TestAccountKeysCreate_InheritsEmailVerification(t *testing.T) {
	verifiedAt := time.Date(2026, 8, 1, 9, 30, 0, 0, time.UTC)
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_child"}, plain: "sip_child"}
	ts := newAccountTestServer(t, auth.Subject{
		Identifier:      "signup-0123456789abcdef",
		Tier:            auth.TierAPIKey,
		KeyID:           "kid_parent",
		EmailVerifiedAt: verifiedAt,
	}, store)

	resp := postJSONNoReason(t, ts.URL+"/v1/account/keys", `{"label":"rotated"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if !store.gotReq.EmailVerifiedAt.Equal(verifiedAt) {
		t.Fatalf("Create.EmailVerifiedAt = %v, want %v (inherited from the verified parent; "+
			"an unverified signup-* child is 403'd by RequireEmailVerified forever)",
			store.gotReq.EmailVerifiedAt, verifiedAt)
	}
}

// TestAccountKeysCreate_UnverifiedParentStaysUnverified — negative pin:
// the stamp is copied, never invented. (The gate blocks an unverified
// signup parent before it reaches this handler; this pins the handler's
// own behaviour independent of the middleware.)
func TestAccountKeysCreate_UnverifiedParentStaysUnverified(t *testing.T) {
	store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_child"}, plain: "sip_child"}
	ts := newAccountTestServer(t, auth.Subject{
		Identifier: "signup-0123456789abcdef",
		Tier:       auth.TierAPIKey,
		KeyID:      "kid_parent",
	}, store)
	resp := postJSONNoReason(t, ts.URL+"/v1/account/keys", `{"label":"rotated"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if !store.gotReq.EmailVerifiedAt.IsZero() {
		t.Fatalf("Create.EmailVerifiedAt = %v, want zero for an unverified parent", store.gotReq.EmailVerifiedAt)
	}
}

// A mint inherits the caller's identifier, tier and rate limit, and returns
// the plaintext once.
func TestAccountKeysCreate_Happy(t *testing.T) {
	store := &fakeAccountStore{
		rec: auth.APIKeyRecord{
			KeyID:     "kid_new",
			Label:     "ci-bot-2",
			CreatedAt: time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC),
		},
		plain: "sip_freshly_minted",
	}
	ts := newAccountTestServer(t, auth.Subject{
		Identifier:      "owner-42",
		Tier:            auth.TierAPIKey,
		RateLimitPerMin: 600,
	}, store)

	got := decodeData[v1.KeyCreated](t, accountDo(t, ts, http.MethodPost, "/v1/account/keys", `{"label":"ci-bot-2"}`), http.StatusCreated)
	if got.Plaintext != "sip_freshly_minted" || got.KeyID != "kid_new" {
		t.Errorf("created = %+v, want plaintext echoed and kid_new", got)
	}
	if store.calls != 1 {
		t.Errorf("Create called %d times, want 1", store.calls)
	}
	req := store.gotReq
	if req.Identifier != "owner-42" || req.Tier != auth.TierAPIKey || req.RateLimitPerMin != 600 || req.Label != "ci-bot-2" {
		t.Errorf("Create request = %+v, want identifier/tier/rate limit inherited from the caller", req)
	}
}

// Delegation clamp: a caller narrowed to ["account"] that omits scopes must
// mint a child with the same scopes, never a full-access (empty) key; a
// full-access caller still mints full access.
func TestAccountKeysCreate_OmittedScopesInheritTheCallers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scopes []string
		want   []string
	}{
		{"scoped caller", []string{"account"}, []string{"account"}},
		{"full-access caller", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeAccountStore{rec: auth.APIKeyRecord{KeyID: "kid_child"}, plain: "sip_child"}
			ts := newAccountTestServer(t, auth.Subject{
				Identifier: "owner-scoped",
				Tier:       auth.TierAPIKey,
				Scopes:     tc.scopes,
			}, store)

			resp := accountDo(t, ts, http.MethodPost, "/v1/account/keys", `{"label":"child"}`)
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("status = %d, want 201", resp.StatusCode)
			}
			if store.calls != 1 {
				t.Fatalf("Create called %d times, want 1", store.calls)
			}
			if !slices.Equal(store.gotReq.Scopes, tc.want) {
				t.Errorf("minted scopes = %v, want %v", store.gotReq.Scopes, tc.want)
			}
		})
	}
}

// A scoped caller cannot mint a child with a scope it does not hold: an
// ["account"] key requesting ["admin"] is rejected 403 before the store, and
// the refusal is counted.
func TestAccountKeysCreate_ScopedCallerCannotExceed(t *testing.T) {
	before := testutil.ToFloat64(obs.MintScopeClampRefusedTotal.WithLabelValues("/v1/account/keys"))

	store := &fakeAccountStore{
		rec:   auth.APIKeyRecord{KeyID: "kid_x", Label: "x"},
		plain: "sip_x",
	}
	ts := newAccountTestServer(t, auth.Subject{
		Identifier: "owner-scoped",
		Tier:       auth.TierOperator,
		Scopes:     []string{"account"},
	}, store)

	// Operator tier needs X-Reason; supply it so the scope clamp is what's tested.
	resp := doWithReason(t, http.MethodPost, ts.URL+"/v1/account/keys", "scope clamp test", `{"label":"x","scopes":["admin"]}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if store.calls != 0 {
		t.Errorf("Create called %d times, want 0 (escalation must be rejected before mint)", store.calls)
	}
	if got, want := testutil.ToFloat64(obs.MintScopeClampRefusedTotal.WithLabelValues("/v1/account/keys")), before+1; got != want {
		t.Errorf("mint_scope_clamp_refused_total{route=\"/v1/account/keys\"} = %v, want %v", got, want)
	}
}

// TestAccountKeysCreate_WithScopes pins the self-service scope
// plumbing: valid scopes flow into CreateAPIKeyRequest (deduped),
// unknown scopes 400 before touching the store.
func TestAccountKeysCreate_WithScopes(t *testing.T) {
	subject := auth.Subject{Identifier: "cust-42", Tier: auth.TierAPIKey, KeyID: "kid_a"}
	store := &fakeAccountStore{
		rec:   auth.APIKeyRecord{KeyID: "kid_new", Scopes: []string{"read"}},
		plain: "sip_plain",
	}
	ts := newAccountTestServer(t, subject, store)

	resp := postJSON(t, ts.URL+"/v1/account/keys",
		`{"label":"ci-bot","scopes":["read","read"]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if len(store.gotReq.Scopes) != 1 || store.gotReq.Scopes[0] != "read" {
		t.Errorf("Scopes = %v, want deduped [read]", store.gotReq.Scopes)
	}
	var env struct {
		Data v1.KeyCreated `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Data.Scopes) != 1 || env.Data.Scopes[0] != "read" {
		t.Errorf("response scopes = %v", env.Data.Scopes)
	}

	// Unknown scope → 400, store untouched.
	store2 := &fakeAccountStore{}
	ts2 := newAccountTestServer(t, subject, store2)
	resp2 := postJSON(t, ts2.URL+"/v1/account/keys",
		`{"label":"ci-bot","scopes":["everything"]}`)
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for unknown scope", resp2.StatusCode)
	}
	if store2.calls != 0 {
		t.Errorf("store.Create called despite invalid scope")
	}
}
