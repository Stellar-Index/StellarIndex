package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// fakeAccountStore is the handler-level test double for
// [v1.AccountStore]. Records arguments + returns a canned record so
// the handler test exercises the wire shape without pulling in
// miniredis.
type fakeAccountStore struct {
	gotReq    auth.CreateAPIKeyRequest
	rec       auth.APIKeyRecord
	plain     string
	err       error
	calls     int
	listed    map[string][]auth.APIKeyRecord // identifier → keys
	listErr   error
	listCalls int
	revokeErr error
}

func (f *fakeAccountStore) Create(_ context.Context, req auth.CreateAPIKeyRequest) (auth.APIKeyRecord, string, error) {
	f.calls++
	f.gotReq = req
	if f.err != nil {
		return auth.APIKeyRecord{}, "", f.err
	}
	// Record the mint so a later list sees it, as the real store does.
	if f.listed == nil {
		f.listed = map[string][]auth.APIKeyRecord{}
	}
	f.listed[req.Identifier] = append(f.listed[req.Identifier], f.rec)
	return f.rec, f.plain, nil
}

// CreateCapped mirrors the real store's contract: count un-revoked keys,
// refuse at the ceiling, else Create.
func (f *fakeAccountStore) CreateCapped(ctx context.Context, req auth.CreateAPIKeyRequest, maxActive int) (auth.APIKeyRecord, string, error) {
	existing, err := f.ListKeysForIdentifier(ctx, req.Identifier)
	if err != nil {
		return auth.APIKeyRecord{}, "", fmt.Errorf("%w: %w", auth.ErrKeyQuotaUnavailable, err)
	}
	active := 0
	for _, k := range existing {
		if k.RevokedAt.IsZero() {
			active++
		}
	}
	if active >= maxActive {
		return auth.APIKeyRecord{}, "", &auth.KeyQuotaExceededError{Active: active, Max: maxActive}
	}
	return f.Create(ctx, req)
}

func (f *fakeAccountStore) ListKeysForIdentifier(_ context.Context, identifier string) ([]auth.APIKeyRecord, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.listed == nil {
		return nil, nil
	}
	return f.listed[identifier], nil
}

func (f *fakeAccountStore) RevokeKeyByID(_ context.Context, _ string, _ string) error {
	if f.revokeErr != nil {
		return f.revokeErr
	}
	return nil
}

// fakeAuthMiddleware returns a middleware that stamps the supplied
// Subject onto the request context. Standing in for the real auth
// middleware so handler tests can run without configuring a
// validator + Redis.
//
// Pass the zero Subject to leave the context bare (simulates an
// anonymous request that didn't go through any auth layer at all).
func fakeAuthMiddleware(s auth.Subject) middleware.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if s.Tier == "" && s.Identifier == "" {
				next.ServeHTTP(w, r)
				return
			}
			r = r.WithContext(auth.WithSubject(r.Context(), s))
			next.ServeHTTP(w, r)
		})
	}
}

// newAccountTestServer wires a Server with a controlled subject +
// optional account store. Subject's zero value means "anonymous /
// no auth attached" — the handlers should respond 401 for those
// requests.
func newAccountTestServer(t *testing.T, subject auth.Subject, store v1.AccountStore) *httptest.Server {
	t.Helper()
	srv := v1.New(v1.Options{
		Auth:     fakeAuthMiddleware(subject),
		Accounts: store,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// TestAccountMe_Unauthenticated covers the 401 path. /me is

// accountDo sends one request to ts; an empty body sends none.
func accountDo(t *testing.T, ts *httptest.Server, method, path, body string) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// decodeData decodes the {"data": ...} wrapper of a response into T.
func decodeData[T any](t *testing.T, resp *http.Response, wantStatus int) T {
	t.Helper()
	if resp.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d", resp.StatusCode, wantStatus)
	}
	var wrapper struct {
		Data T `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrapper); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return wrapper.Data
}

// An anonymous request is refused before any handler or store work, with a
// problem+json body rather than a default echo.
func TestAccount_Unauthenticated(t *testing.T) {
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/account/me", ""},
		{http.MethodGet, "/v1/account/usage", ""},
		{http.MethodGet, "/v1/account/keys", ""},
		{http.MethodPost, "/v1/account/keys", `{"label":"x"}`},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			store := &fakeAccountStore{}
			ts := newAccountTestServer(t, auth.Subject{}, store)
			resp := accountDo(t, ts, tc.method, tc.path, tc.body)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
				t.Errorf("content-type = %q, want problem+json", ct)
			}
			if store.calls != 0 || store.listCalls != 0 {
				t.Errorf("store touched on 401: %d creates, %d lists", store.calls, store.listCalls)
			}
		})
	}
}

// Authenticated key routes that must fail with a specific status. A nil
// store models the binary starting without Redis; validation failures must
// not reach the store, and no failure may surface a plaintext-shaped string.
func TestAccountKeys_ErrorStatuses(t *testing.T) {
	for _, tc := range []struct {
		name      string
		store     *fakeAccountStore // nil: no store wired
		method    string
		body      string
		want      int
		wantCalls int
	}{
		{"create without a store", nil, http.MethodPost, `{"label":"x"}`, http.StatusServiceUnavailable, 0},
		{"create with an empty body", &fakeAccountStore{}, http.MethodPost, "", http.StatusBadRequest, 0},
		{"create with an empty object", &fakeAccountStore{}, http.MethodPost, "{}", http.StatusBadRequest, 0},
		{"create with an empty label", &fakeAccountStore{}, http.MethodPost, `{"label":""}`, http.StatusBadRequest, 0},
		{
			"create with a 129-char label", &fakeAccountStore{}, http.MethodPost,
			`{"label":"` + strings.Repeat("a", 129) + `"}`, http.StatusBadRequest, 0,
		},
		{"create with malformed JSON", &fakeAccountStore{}, http.MethodPost, "{not-json", http.StatusBadRequest, 0},
		{
			"create when the store fails", &fakeAccountStore{err: errors.New("redis down")},
			http.MethodPost, `{"label":"x"}`, http.StatusInternalServerError, 1,
		},
		{"list without a store", nil, http.MethodGet, "", http.StatusServiceUnavailable, 0},
		{
			"list when the store fails", &fakeAccountStore{listErr: errors.New("redis blip")},
			http.MethodGet, "", http.StatusInternalServerError, 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var store v1.AccountStore
			if tc.store != nil {
				store = tc.store
			}
			ts := newAccountTestServer(t, auth.Subject{Identifier: "owner-42", Tier: auth.TierAPIKey}, store)
			resp := accountDo(t, ts, tc.method, "/v1/account/keys", tc.body)
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			raw, _ := io.ReadAll(resp.Body)
			if strings.Contains(string(raw), "sip_") {
				t.Error("response body contains a plaintext-shaped string")
			}
			if tc.store != nil && tc.store.calls != tc.wantCalls {
				t.Errorf("Create called %d times, want %d", tc.store.calls, tc.wantCalls)
			}
		})
	}
}

// Field-level assertions guard the /me wire shape against a rename that would
// silently break clients.
func TestAccountMe_Authenticated(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	ts := newAccountTestServer(t, auth.Subject{
		Identifier:      "owner-42",
		Tier:            auth.TierAPIKey,
		KeyID:           "kid_abc123",
		Label:           "ci-bot",
		RateLimitPerMin: 600,
		CreatedAt:       now,
	}, nil)

	got := decodeData[v1.Account](t, accountDo(t, ts, http.MethodGet, "/v1/account/me", ""), http.StatusOK)
	if got.KeyID != "kid_abc123" || got.Label != "ci-bot" || got.Tier != "apikey" || got.RateLimitPerMin != 600 {
		t.Errorf("account = %+v, want kid_abc123 / ci-bot / apikey / 600", got)
	}
	if !got.CreatedAt.Time().Equal(now) {
		t.Errorf("CreatedAt = %v", got.CreatedAt)
	}
}

// A session caller's /v1/account/me serves what auth enforces on a
// default-minted key, derived through platform's cascade: for a partner
// comped to 5,000/min that is 5,000, never the 100,000 tier ceiling.
func TestAccountMe_SessionEffectiveLimits(t *testing.T) {
	acct := platform.Account{
		Tier:                        platform.TierPartner,
		RateLimitPerMinOverride:     5000,
		MonthlyRequestQuotaOverride: 200_000,
	}
	srv := v1.New(v1.Options{
		Auth: fakeAuthMiddleware(auth.Subject{}),
		SessionPeeker: &fakeSessionPeeker{ok: true, info: v1.SessionInfo{
			AccountID:                  "acct-1",
			AccountSlug:                "acme",
			AccountTier:                string(acct.Tier),
			AccountStatus:              "active",
			AccountRateLimitPerMin:     acct.EffectiveRateLimitPerMin(),
			AccountMonthlyRequestQuota: acct.EffectiveMonthlyQuota(),
		}},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	got := decodeData[v1.Account](t, accountDo(t, ts, http.MethodGet, "/v1/account/me", ""), http.StatusOK)
	if got.AccountInfo == nil {
		t.Fatal("AccountInfo is nil")
	}
	if got.AccountInfo.RateLimitPerMin != 5000 {
		t.Errorf("AccountInfo.RateLimitPerMin = %d, want 5000 (the comped override; 100000 is the tier ceiling)", got.AccountInfo.RateLimitPerMin)
	}
	if got.AccountInfo.MonthlyRequestQuota != 200_000 {
		t.Errorf("AccountInfo.MonthlyRequestQuota = %d, want 200000", got.AccountInfo.MonthlyRequestQuota)
	}
}

// The counter store is not wired, so usage is an empty array: the wire shape
// is locked ahead of real counters.
func TestAccountUsage_EmptyList(t *testing.T) {
	ts := newAccountTestServer(t, auth.Subject{Identifier: "owner-9", Tier: auth.TierAPIKey}, nil)
	if rows := getUsageRows(t, ts); len(rows) != 0 {
		t.Errorf("data should be empty array, got %d entries", len(rows))
	}
}

// An authenticated caller lists every key under their Identifier, oldest first.
func TestAccountKeysList_HappyPath(t *testing.T) {
	subj := auth.Subject{Identifier: "signup-acme", Tier: auth.TierAPIKey}
	store := &fakeAccountStore{
		listed: map[string][]auth.APIKeyRecord{
			"signup-acme": {
				{KeyID: "kid_first", Identifier: "signup-acme", Tier: auth.TierAPIKey, RateLimitPerMin: 1000, Label: "first", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
				{KeyID: "kid_second", Identifier: "signup-acme", Tier: auth.TierAPIKey, RateLimitPerMin: 10000, Label: "rotated", CreatedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)},
			},
		},
	}
	ts := newAccountTestServer(t, subj, store)

	keys := decodeData[[]v1.Account](t, accountDo(t, ts, http.MethodGet, "/v1/account/keys", ""), http.StatusOK)
	if len(keys) != 2 {
		t.Fatalf("data len = %d, want 2", len(keys))
	}
	if keys[0].KeyID != "kid_first" || keys[1].KeyID != "kid_second" {
		t.Errorf("keys = [%q %q], want oldest first (kid_first, kid_second)", keys[0].KeyID, keys[1].KeyID)
	}
	if keys[1].RateLimitPerMin != 10000 {
		t.Errorf("rotated key RateLimitPerMin = %d, want 10000", keys[1].RateLimitPerMin)
	}
	if store.listCalls != 1 {
		t.Errorf("store called %d times, want 1", store.listCalls)
	}
}

// A caller with no keys gets an empty list.
func TestAccountKeysList_Empty(t *testing.T) {
	ts := newAccountTestServer(t, auth.Subject{Identifier: "signup-empty", Tier: auth.TierAPIKey},
		&fakeAccountStore{listed: map[string][]auth.APIKeyRecord{}})
	if keys := decodeData[[]v1.Account](t, accountDo(t, ts, http.MethodGet, "/v1/account/keys", ""), http.StatusOK); len(keys) != 0 {
		t.Errorf("data len = %d, want 0", len(keys))
	}
}

// ─── /v1/account/usage rollup path ────────────────────────────────

// fakeUsageRollupReader is the handler-level double for
// [v1.UsageRollupReader].
type fakeUsageRollupReader struct {
	gotSubject string
	gotDays    int
	rows       []v1.UsageEndpointDay
	err        error
}

func (f *fakeUsageRollupReader) ReadRollup(_ context.Context, subject string, days int) ([]v1.UsageEndpointDay, error) {
	f.gotSubject = subject
	f.gotDays = days
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

// fakeUsageReader is the legacy per-day-totals double for
// [v1.UsageReader].
type fakeUsageReader struct {
	days []v1.UsageDay
	err  error
}

func (f *fakeUsageReader) Read(_ context.Context, _ string, _ int) ([]v1.UsageDay, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.days, nil
}

// newUsageTestServer wires a Server with both usage seams so the
// preference / fallback contract is testable end-to-end.
func newUsageTestServer(t *testing.T, subject auth.Subject, rollup v1.UsageRollupReader, legacy v1.UsageReader) *httptest.Server {
	t.Helper()
	srv := v1.New(v1.Options{
		Auth:              fakeAuthMiddleware(subject),
		UsageRollupReader: rollup,
		UsageReader:       legacy,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func getUsageRows(t *testing.T, ts *httptest.Server) []v1.UsageRow {
	t.Helper()
	resp, err := http.Get(ts.URL + "/v1/account/usage")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data []v1.UsageRow `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	return env.Data
}

// TestAccountUsage_RollupRows — the rollup reader backs the wire
// shape: one row per (day, endpoint) with endpoint + errors +
// throttled populated, keyed by the same subject derivation the
// tracker middleware writes under — the OWNER ACCOUNT (id:<Identifier>),
// not the credential, so the endpoint covers every key the account holds
// and survives a key rotation.
func TestAccountUsage_RollupRows(t *testing.T) {
	rollup := &fakeUsageRollupReader{rows: []v1.UsageEndpointDay{
		{Date: "2026-07-02", Endpoint: "/v1/price", Requests: 120, Errors: 3, Throttled: 0},
		{Date: "2026-07-03", Endpoint: "/v1/assets/{asset_id}", Requests: 40, Errors: 1, Throttled: 7},
	}}
	ts := newUsageTestServer(t, auth.Subject{
		Identifier: "owner-9",
		KeyID:      "kid_9",
		Tier:       auth.TierAPIKey,
	}, rollup, &fakeUsageReader{days: []v1.UsageDay{{Date: "2026-07-03", Requests: 999}}})

	rows := getUsageRows(t, ts)
	if rollup.gotSubject != "id:owner-9" {
		t.Errorf("subject = %q, want id:owner-9 (must match the tracker's derivation: the owner account, not the KeyID)", rollup.gotSubject)
	}
	if rollup.gotDays != 30 {
		t.Errorf("days = %d, want 30", rollup.gotDays)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2 entries", rows)
	}
	want := v1.UsageRow{Date: "2026-07-03", Endpoint: "/v1/assets/{asset_id}", Requests: 40, Errors: 1, Throttled: 7}
	if rows[1] != want {
		t.Errorf("row[1] = %+v, want %+v", rows[1], want)
	}
	// The legacy 999-request day must NOT appear — rollup wins.
	for _, r := range rows {
		if r.Requests == 999 {
			t.Error("legacy fallback rows leaked into a successful rollup response")
		}
	}
}

// A rollup read failure or zero rollup rows (fresh deployment, worker not yet
// swept) degrades to the legacy per-day totals, not a 5xx.
func TestAccountUsage_RollupFallsBackToLegacy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rollup *fakeUsageRollupReader
		legacy v1.UsageDay
	}{
		{"rollup error", &fakeUsageRollupReader{err: errors.New("pg down")}, v1.UsageDay{Date: "2026-07-03", Requests: 55}},
		{"rollup empty", &fakeUsageRollupReader{}, v1.UsageDay{Date: "2026-07-01", Requests: 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newUsageTestServer(t, auth.Subject{Identifier: "owner-9", KeyID: "kid_9", Tier: auth.TierAPIKey},
				tc.rollup, &fakeUsageReader{days: []v1.UsageDay{tc.legacy}})
			rows := getUsageRows(t, ts)
			if len(rows) != 1 || int64(rows[0].Requests) != tc.legacy.Requests || rows[0].Endpoint != "" {
				t.Errorf("rows = %+v, want the single legacy day (%d requests, no endpoint)", rows, tc.legacy.Requests)
			}
		})
	}
}

// TestAccountUser_UnverifiedEmailOmitsTheTimestampRatherThanZeroing —
// a user who never verified their email or never logged in must render
// as an ABSENT field on the wire, not the zero instant serialized as a
// literal string, which the SDK's *time.Time side would happily
// unmarshal into a non-nil pointer indistinguishable from a real time.
func TestAccountUser_UnverifiedEmailOmitsTheTimestampRatherThanZeroing(t *testing.T) {
	u := v1.AccountUser{ID: "usr_1", Email: "new@example.com"}
	body, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, present := raw["email_verified_at"]; present {
		t.Errorf("email_verified_at present in %s, want omitted for a zero instant", body)
	}
	if _, present := raw["last_login_at"]; present {
		t.Errorf("last_login_at present in %s, want omitted for a zero instant", body)
	}

	verified := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	w := v1.WireTime(verified)
	u.EmailVerifiedAt = &w
	body, err = json.Marshal(u)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := raw["email_verified_at"]; got != "2026-03-01T00:00:00Z" {
		t.Errorf("email_verified_at = %v, want 2026-03-01T00:00:00Z", got)
	}
}

// TestAccountUsage_BillableSameMeaningOnBothShapes pins that the
// handler: `billable` carries the rollup's quota-counted units, and a
// legacy row — the rollup-gap backfill inside a per-endpoint response
// and the whole-response fallback alike — reports its billable total
// as `billable`, so summing one column reconciles on either shape.
func TestAccountUsage_BillableSameMeaningOnBothShapes(t *testing.T) {
	subject := auth.Subject{Identifier: "owner-b", KeyID: "kid_b", Tier: auth.TierAPIKey}
	legacy := &fakeUsageReader{days: []v1.UsageDay{
		{Date: "2026-07-01", Requests: 30},
		{Date: "2026-07-02", Requests: 999},
	}}
	rollup := &fakeUsageRollupReader{rows: []v1.UsageEndpointDay{
		{Date: "2026-07-02", Endpoint: "/v1/price", Requests: 17, Billable: 12, Errors: 7, Throttled: 3},
	}}

	got := map[string]v1.UsageRow{}
	for _, r := range getUsageRows(t, newUsageTestServer(t, subject, rollup, legacy)) {
		got[r.Date+"|"+r.Endpoint] = r
	}
	if r := got["2026-07-02|/v1/price"]; r.Billable != 12 || r.Requests != 17 {
		t.Errorf("rollup row = %+v, want billable 12 / requests 17", r)
	}
	if r := got["2026-07-01|"]; r.Billable != 30 {
		t.Errorf("backfilled legacy row = %+v, want billable 30", r)
	}

	rows := getUsageRows(t, newUsageTestServer(t, subject, &fakeUsageRollupReader{}, legacy))
	if len(rows) != 2 || rows[0].Billable != 30 || rows[1].Billable != 999 {
		t.Errorf("legacy fallback rows = %+v, want billable 30 and 999", rows)
	}
}

// TestAccountUsage_RollupBackfillsMissingDay. A day the
// rollup worker never produced a row for at all (an outage gap, not
// a legitimate zero-traffic day) must be filled in from the legacy
// per-day reader rather than silently dropped from the trailing
// 30-day window. Guards against readUsageRollup returning ok=true as soon
// as len(days) > 0, which stops handleAccountUsage consulting the legacy
// reader and makes a gap day vanish from the response.
func TestAccountUsage_RollupBackfillsMissingDay(t *testing.T) {
	rollup := &fakeUsageRollupReader{rows: []v1.UsageEndpointDay{
		{Date: "2026-07-03", Endpoint: "/v1/price", Requests: 40, Errors: 1, Throttled: 7},
	}}
	legacy := &fakeUsageReader{days: []v1.UsageDay{
		{Date: "2026-07-01", Requests: 12},  // rollup worker outage — no rollup row for this day
		{Date: "2026-07-03", Requests: 999}, // rollup already covers this day; must NOT leak
	}}
	ts := newUsageTestServer(t, auth.Subject{
		Identifier: "owner-9",
		Tier:       auth.TierAPIKey,
	}, rollup, legacy)

	rows := getUsageRows(t, ts)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2 (1 rollup row + 1 backfilled legacy day)", rows)
	}
	var gotBackfill bool
	for _, r := range rows {
		switch r.Date {
		case "2026-07-01":
			gotBackfill = true
			want := v1.UsageRow{Date: "2026-07-01", Requests: 12, Billable: 12}
			if r != want {
				t.Errorf("backfilled row = %+v, want %+v", r, want)
			}
		case "2026-07-03":
			if r.Requests == 999 {
				t.Error("legacy total leaked over a day the rollup reader already covered")
			}
		}
	}
	if !gotBackfill {
		t.Errorf("missing rollup day 2026-07-01 was not backfilled from the legacy reader; rows = %+v", rows)
	}
}

// TestAccountUsage_RollupBackfill_LegacyUnwired — no legacy reader
// wired: the rollup rows still return (unwired backfill degrades to
// no backfill, not an error), same posture as the rollup path itself.
func TestAccountUsage_RollupBackfill_LegacyUnwired(t *testing.T) {
	rollup := &fakeUsageRollupReader{rows: []v1.UsageEndpointDay{
		{Date: "2026-07-03", Endpoint: "/v1/price", Requests: 40},
	}}
	ts := newUsageTestServer(t, auth.Subject{
		Identifier: "owner-9",
		Tier:       auth.TierAPIKey,
	}, rollup, nil)

	rows := getUsageRows(t, ts)
	if len(rows) != 1 || rows[0].Date != "2026-07-03" {
		t.Errorf("rows = %+v, want the single rollup row unchanged", rows)
	}
}

// TestAccountUsage_SessionAuthenticated — a
// magic-link dashboard session with NO API key attached must read
// its account's usage, not 401. A handleAccountUsage that gated
// solely on auth.SubjectFrom, which a session-only request never
// populates (only the API-key auth middleware calls auth.WithSubject
// in production) — every signed-in dashboard user got a 401 the
// frontend silently swallowed into an empty usage page.
func TestAccountUsage_SessionAuthenticated(t *testing.T) {
	rollup := &fakeUsageRollupReader{rows: []v1.UsageEndpointDay{
		{Date: "2026-07-02", Endpoint: "/v1/price", Requests: 10, Errors: 0, Throttled: 0},
	}}
	srv := v1.New(v1.Options{
		// Anonymous — the request carries a session cookie, not an
		// API key, so no auth.Subject reaches the context.
		Auth:              fakeAuthMiddleware(auth.Subject{}),
		SessionPeeker:     &fakeSessionPeeker{ok: true, info: v1.SessionInfo{AccountSlug: "acme-labs"}},
		UsageRollupReader: rollup,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/v1/account/usage")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a dashboard session must not 401 on /v1/account/usage)", resp.StatusCode)
	}
	// Must read under the SAME account-scoped key an API key minted
	// on this account would write under (middleware.UsageKeyForSubject's
	// Identifier branch), so the dashboard sees the account's real
	// usage rather than an unrelated / empty bucket.
	if rollup.gotSubject != "id:acct:acme-labs" {
		t.Errorf("subject = %q, want id:acct:acme-labs", rollup.gotSubject)
	}
}

// TestAccountUsage_NoSessionNoSubject_Unauthenticated — a request
// with neither a session nor an API key still 401s: the session path
// must not become a universal bypass.
func TestAccountUsage_NoSessionNoSubject_Unauthenticated(t *testing.T) {
	srv := v1.New(v1.Options{
		Auth:          fakeAuthMiddleware(auth.Subject{}),
		SessionPeeker: &fakeSessionPeeker{ok: false},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/v1/account/usage")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}
