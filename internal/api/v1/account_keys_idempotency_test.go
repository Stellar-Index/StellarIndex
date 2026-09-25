package v1_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

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
