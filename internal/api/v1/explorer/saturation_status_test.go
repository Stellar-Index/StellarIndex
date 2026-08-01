package explorer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// saturationReader is a capReader whose AccountStateCached fails with a
// caller-chosen error, so a test can drive the saturation-vs-genuine-error
// branch of the AccountState handler's status mapping without a live lake.
type saturationReader struct {
	*capReader
	err error
}

func (r *saturationReader) AccountStateCached(context.Context, string) (clickhouse.AccountState, bool, error) {
	return clickhouse.AccountState{}, false, r.err
}

// TestAccountState_GateSaturationMapsTo503 pins recon-R3: when the lake
// reader's detached-refresh gate is SATURATED it returns
// clickhouse.ErrRefreshSaturated — a transient, retryable backpressure
// condition the GET /v1/accounts/{g} handler MUST surface as a 503 (retry),
// NOT the 500 `errors/internal` a genuine bug gets. A real internal error
// still maps to 500 so alerts and the 5xx SLA probe stay meaningful.
//
// Red without the fix: pre-fix the handler only special-cased a read deadline
// (readTimedOut), so clickhouse.ErrRefreshSaturated fell through to the 500
// branch and the saturation subtest fails with status 500.
func TestAccountState_GateSaturationMapsTo503(t *testing.T) {
	cases := []struct {
		name       string
		readerErr  error
		wantStatus int
	}{
		{
			"gate saturation is a retryable 503",
			clickhouse.ErrRefreshSaturated,
			http.StatusServiceUnavailable,
		},
		{
			"wrapped gate saturation is still 503",
			fmt.Errorf("account %s: %w", validTestAccount, clickhouse.ErrRefreshSaturated),
			http.StatusServiceUnavailable,
		},
		{
			"genuine internal error stays 500",
			errors.New("clickhouse: code 60, table stellar.ledger_entries_current does not exist"),
			http.StatusInternalServerError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rec problemRecord
			reader := &saturationReader{
				capReader: &capReader{probe: &deadlineProbe{}},
				err:       tc.readerErr,
			}
			h := newProbeHandler(reader, nil)
			h.WriteProblem = func(w http.ResponseWriter, _ *http.Request, typeURL, title string, status int, detail string) {
				rec = problemRecord{typeURL: typeURL, title: title, status: status, detail: detail, written: true}
				w.WriteHeader(status)
			}

			req := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+validTestAccount, nil)
			req.SetPathValue("g_strkey", validTestAccount)
			w := httptest.NewRecorder()
			h.AccountState(w, req)

			if !rec.written {
				t.Fatal("no problem+json written — the handler swallowed the error")
			}
			if rec.status != tc.wantStatus {
				t.Fatalf("problem status = %d (%q), want %d", rec.status, rec.typeURL, tc.wantStatus)
			}
			if w.Code != tc.wantStatus {
				t.Fatalf("response code = %d, want %d", w.Code, tc.wantStatus)
			}
			switch tc.wantStatus {
			case http.StatusServiceUnavailable:
				if rec.typeURL != "https://api.stellarindex.io/errors/account-state-timeout" {
					t.Fatalf("saturation problem type = %q, want the retryable account-state-timeout 503 type", rec.typeURL)
				}
			case http.StatusInternalServerError:
				// A real bug MUST keep the generic internal type so it stays
				// distinguishable from transient backpressure in dashboards.
				if rec.typeURL != "https://api.stellarindex.io/errors/internal" {
					t.Fatalf("genuine error problem type = %q, want errors/internal", rec.typeURL)
				}
			}
		})
	}
}
