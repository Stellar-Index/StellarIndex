package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

type recordingRegisterAudit struct {
	mu      sync.Mutex
	entries []platform.AuditEntry
}

func (a *recordingRegisterAudit) Append(_ context.Context, e platform.AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
	return nil
}

func registerControlsServer(t *testing.T, opts v1.Options) (*httptest.Server, *fakeRegisterAccountStore) {
	t.Helper()
	accounts := newFakeRegisterAccountStore()
	opts.Auth = fakeAuthMiddleware(auth.Subject{})
	opts.RegisterAccounts = accounts
	opts.APIKeyBudgets = v1.APIKeyBudgetStores{Platform: newFakeRegisterKeyStore()}
	ts := httptest.NewServer(v1.New(opts).Handler())
	t.Cleanup(ts.Close)
	return ts, accounts
}

func registrationOutcome(outcome string) float64 {
	return testutil.ToFloat64(obs.RegistrationsTotal.WithLabelValues(outcome))
}

func problemType(t *testing.T, resp *http.Response) string {
	t.Helper()
	var p struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	return p.Type
}

// The operator switch refuses every registration before anything is created or counted against a throttle.
func TestRegister_DisabledIssuesNothing(t *testing.T) {
	ipThrottle := &fakeRegisterIPThrottle{}
	ts, accounts := registerControlsServer(t, v1.Options{RegisterDisabled: true, SignupIPThrottle: ipThrottle})
	before := registrationOutcome(obs.RegistrationDisabled)

	resp := postRegister(t, ts.URL, "application/json", "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := problemType(t, resp); !strings.HasSuffix(got, "/registration-unavailable") {
		t.Errorf("problem type = %q, want registration-unavailable", got)
	}
	if len(accounts.created) != 0 || len(ipThrottle.ips) != 0 {
		t.Errorf("a switched-off route created %d account(s) and spent %d throttle unit(s)", len(accounts.created), len(ipThrottle.ips))
	}
	if got := registrationOutcome(obs.RegistrationDisabled) - before; got != 1 {
		t.Errorf("disabled counter moved by %v, want 1", got)
	}
}

// Invariant: once the all-caller budget is spent no account or key is issued, whatever address asks. The budget is
// counted under one key, and a request the per-IP throttle refuses never spends it.
func TestRegister_AllCallerBudget(t *testing.T) {
	t.Run("spent budget issues nothing", func(t *testing.T) {
		budget := &fakeRegisterIPThrottle{err: auth.ErrSignupRateLimited}
		ts, accounts := registerControlsServer(t, v1.Options{SignupIPThrottle: &fakeRegisterIPThrottle{}, RegisterBudget: budget})
		before := registrationOutcome(obs.RegistrationBudgetExhausted)

		resp := postRegister(t, ts.URL, "application/json", "")
		wait, err := strconv.Atoi(resp.Header.Get("Retry-After"))
		if resp.StatusCode != http.StatusServiceUnavailable || err != nil || wait < 1 || wait > 3600 {
			t.Fatalf("status = %d, Retry-After = %q; want 503 and the seconds left in the clock hour", resp.StatusCode, resp.Header.Get("Retry-After"))
		}
		if got := problemType(t, resp); !strings.HasSuffix(got, "/registration-unavailable") {
			t.Errorf("problem type = %q, want registration-unavailable", got)
		}
		if len(accounts.created) != 0 {
			t.Errorf("%d account(s) created past the budget", len(accounts.created))
		}
		if len(budget.ips) != 1 || budget.ips[0] != "all" {
			t.Errorf("budget counted under %q, want one key shared by every caller", budget.ips)
		}
		if got := registrationOutcome(obs.RegistrationBudgetExhausted) - before; got != 1 {
			t.Errorf("budget_exhausted counter moved by %v, want 1", got)
		}
	})

	t.Run("a per-IP refusal does not spend it", func(t *testing.T) {
		budget := &fakeRegisterIPThrottle{}
		ts, _ := registerControlsServer(t, v1.Options{
			SignupIPThrottle: &fakeRegisterIPThrottle{err: auth.ErrSignupRateLimited}, RegisterBudget: budget,
		})
		if resp := postRegister(t, ts.URL, "application/json", ""); resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want the per-IP 429", resp.StatusCode)
		}
		if len(budget.ips) != 0 {
			t.Error("one throttled address spent the budget every other caller shares")
		}
	})

	t.Run("a sustained throttle outage fails closed", func(t *testing.T) {
		ts, accounts := registerControlsServer(t, v1.Options{RegisterBudget: &fakeRegisterIPThrottle{err: auth.ErrThrottleUnavailable}})
		closed := obs.RateLimitFailClosedTotal.WithLabelValues(obs.RateLimiterRegisterBudget)
		before := testutil.ToFloat64(closed)
		resp := postRegister(t, ts.URL, "application/json", "")
		if resp.StatusCode != http.StatusServiceUnavailable || len(accounts.created) != 0 {
			t.Fatalf("status = %d with %d account(s) created; want 503 and none", resp.StatusCode, len(accounts.created))
		}
		if got := testutil.ToFloat64(closed) - before; got != 1 {
			t.Errorf("fail-closed counter for the budget moved by %v, want 1", got)
		}
	})
}

// A registration leaves a counted, attributable record: the created counter and an audit row naming the account, the
// key and the caller's address.
func TestRegister_SuccessIsCountedAndAudited(t *testing.T) {
	audit := &recordingRegisterAudit{}
	ts, _ := registerControlsServer(t, v1.Options{Audit: audit, RegisterBudget: &fakeRegisterIPThrottle{}})
	before := registrationOutcome(obs.RegistrationCreated)

	resp := postRegister(t, ts.URL, "application/json", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	res := decodeRegisterResult(t, resp)
	if got := registrationOutcome(obs.RegistrationCreated) - before; got != 1 {
		t.Errorf("created counter moved by %v, want 1", got)
	}
	if len(audit.entries) != 1 {
		t.Fatalf("%d audit row(s), want 1", len(audit.entries))
	}
	e := audit.entries[0]
	if e.Action != "account.register" || e.AccountID.String() != res.AccountID || e.TargetID != res.KeyID || e.IP == nil {
		t.Errorf("audit row = %+v; want account.register naming account %s, key %s and the caller IP", e, res.AccountID, res.KeyID)
	}
}
