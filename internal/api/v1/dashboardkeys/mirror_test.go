package dashboardkeys

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/dashboardauth"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// A dashboard key must authenticate against the validator the deployment
// runs. Under the default auth_backend=redis that is RedisAPIKeyValidator,
// which reads nothing but apikey:<hash>, so these tests mint through
// HandleCreate and Lookup the returned plaintext through it (GH-966).

func newMirroredHandlers(t *testing.T, keys platform.APIKeyStore, rdb redis.Cmdable) *Handlers {
	t.Helper()
	h, err := NewHandlers(Config{
		Keys:   keys,
		Mirror: auth.NewRedisAPIKeyStore(rdb),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewHandlers: %v", err)
	}
	return h
}

func newMiniRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func mintViaDashboard(t *testing.T, h *Handlers, sc dashboardauth.SessionContext, body createRequest) (createResponse, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	h.HandleCreate(w, sessionRequest(t, http.MethodPost, "/v1/dashboard/keys", body, sc))
	var resp createResponse
	if w.Code == http.StatusCreated {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp, w
}

func TestDashboardMint_AuthenticatesViaRedisValidator(t *testing.T) {
	rdb := newMiniRedis(t)
	h := newMirroredHandlers(t, newFakeKeyStore(), rdb)
	_, _, sc := newTestRig(t)
	resp, w := mintViaDashboard(t, h, sc, createRequest{
		Name:        "production",
		Scopes:      []string{"read"},
		IPAllowlist: []string{"198.51.100.7"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	sub, err := auth.NewRedisAPIKeyValidator(rdb).Lookup(context.Background(), resp.Plaintext)
	if err != nil {
		t.Fatalf("dashboard-minted key does not authenticate on the redis validator: %v", err)
	}
	if sub.KeyID != resp.Key.ID {
		t.Errorf("Subject.KeyID = %q, want the management row id %q", sub.KeyID, resp.Key.ID)
	}
	if want := auth.AccountIdentifier(sc.Account.Slug); sub.Identifier != want {
		t.Errorf("Subject.Identifier = %q, want %q", sub.Identifier, want)
	}
	if len(sub.Scopes) != 1 || sub.Scopes[0] != "read" {
		t.Errorf("Subject.Scopes = %v, want [read]: the mirror must not widen the key", sub.Scopes)
	}
	if want := sc.Account.Tier.MaxMonthlyQuota(); sub.MonthlyQuota != want {
		t.Errorf("Subject.MonthlyQuota = %d, want the tier cap %d", sub.MonthlyQuota, want)
	}
}

// The row stores 0 ("inherit the override") under an account override;
// the mirror must carry the override, never 0 (unmetered on Redis).
func TestDashboardMint_MirrorResolvesInheritedMonthlyQuota(t *testing.T) {
	rdb := newMiniRedis(t)
	h := newMirroredHandlers(t, newFakeKeyStore(), rdb)
	_, _, sc := newTestRig(t)
	sc.Account.MonthlyRequestQuotaOverride = 12_345
	resp, w := mintViaDashboard(t, h, sc, createRequest{Name: "inherit"})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	sub, err := auth.NewRedisAPIKeyValidator(rdb).Lookup(context.Background(), resp.Plaintext)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if sub.MonthlyQuota != 12_345 {
		t.Errorf("Subject.MonthlyQuota = %d, want the account override 12345", sub.MonthlyQuota)
	}
}

// The revoking handler carries no CacheInvalidator, so only the mirror
// revoke can kill the record: the wiring main.go gives every backend.
func TestDashboardRevoke_ThroughPostgresWiredHandler_KillsRedisRecord(t *testing.T) {
	rdb := newMiniRedis(t)
	store := newFakeKeyStore()
	_, _, sc := newTestRig(t)
	resp, w := mintViaDashboard(t, newMirroredHandlers(t, store, rdb), sc, createRequest{Name: "short-lived"})
	if w.Code != http.StatusCreated {
		t.Fatalf("mint status = %d, body = %s", w.Code, w.Body.String())
	}
	validator := auth.NewRedisAPIKeyValidator(rdb)
	if _, err := validator.Lookup(context.Background(), resp.Plaintext); err != nil {
		t.Fatalf("minted key does not authenticate before revoke: %v", err)
	}

	revoker := newMirroredHandlers(t, store, rdb)
	req := sessionRequest(t, http.MethodDelete, "/v1/dashboard/keys/"+resp.Key.ID, nil, sc)
	req.SetPathValue("id", resp.Key.ID)
	rw := httptest.NewRecorder()
	revoker.HandleRevoke(rw, req)
	if rw.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, body = %s", rw.Code, rw.Body.String())
	}
	if _, err := validator.Lookup(context.Background(), resp.Plaintext); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("Lookup after revoke = %v, want ErrUnauthorized: the revoked key still authenticates", err)
	}
}

type failingCreateStore struct {
	*fakeKeyStore
	err error
}

func (f failingCreateStore) Create(context.Context, platform.APIKey, int) (platform.APIKey, error) {
	return platform.APIKey{}, f.err
}

// plaintextRecorder captures the plaintext handed to the mirror so a test
// can probe the validator for it after the handler withheld it.
type plaintextRecorder struct {
	KeyMirror
	plaintext string
}

func (p *plaintextRecorder) CreateWithSecret(ctx context.Context, k auth.MirroredKey) error {
	p.plaintext = k.Plaintext
	return p.KeyMirror.CreateWithSecret(ctx, k)
}

// A mirror written ahead of a management row that then fails must be
// rolled back: a credential nobody can list or revoke must not survive.
func TestDashboardMint_PGFailure_RollsBackMirror(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"quota race": {platform.ErrAPIKeyQuotaExceeded, http.StatusConflict},
		"db error":   {errors.New("connection reset"), http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			rdb := newMiniRedis(t)
			h := newMirroredHandlers(t, failingCreateStore{newFakeKeyStore(), tc.err}, rdb)
			rec := &plaintextRecorder{KeyMirror: h.cfg.Mirror}
			h.cfg.Mirror = rec
			_, _, sc := newTestRig(t)
			_, w := mintViaDashboard(t, h, sc, createRequest{Name: "doomed"})
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, tc.want, w.Body.String())
			}
			if rec.plaintext == "" {
				t.Fatal("mirror was never written; the rollback path is untested")
			}
			_, err := auth.NewRedisAPIKeyValidator(rdb).Lookup(context.Background(), rec.plaintext)
			if !errors.Is(err, auth.ErrUnauthorized) {
				t.Errorf("Lookup after failed create = %v, want ErrUnauthorized (orphan credential left live)", err)
			}
		})
	}
}

type revokeFailingMirror struct {
	KeyMirror
	err error
}

func (m revokeFailingMirror) RevokeKeyByID(context.Context, string, string) error { return m.err }

// A key the mirror never held revokes cleanly; a mirror that cannot drop
// a live credential fails the revoke so the caller retries, rather than
// 204ing over a key that still works.
func TestDashboardRevoke_MirrorRemovalOutcome(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"never mirrored": {auth.ErrKeyNotFound, http.StatusNoContent},
		"redis down":     {errors.New("connection refused"), http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			h, store, sc := newTestRig(t)
			h.cfg.Mirror = revokeFailingMirror{err: tc.err}
			store.byID["k-mine"] = platform.APIKey{ID: "k-mine", AccountID: sc.Account.ID, Name: "mine"}
			req := sessionRequest(t, http.MethodDelete, "/v1/dashboard/keys/k-mine", nil, sc)
			req.SetPathValue("id", "k-mine")
			w := httptest.NewRecorder()
			h.HandleRevoke(w, req)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, tc.want, w.Body.String())
			}
			if store.byID["k-mine"].RevokedAt.IsZero() {
				t.Error("management row not revoked")
			}
		})
	}
}
