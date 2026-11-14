package dashboardauth

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

type ctxCheckingAudit struct {
	platform.AuditStore
	rows int
}

func (a *ctxCheckingAudit) Append(ctx context.Context, _ platform.AuditEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.rows++
	return nil
}

// TestCredentialAudit_SurvivesClientDisconnect: the change is already saved, so hanging up must not lose its row.
func TestCredentialAudit_SurvivesClientDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPatch, "/v1/dashboard/webhooks/x", nil).WithContext(ctx)
	sink := &ctxCheckingAudit{}
	RecordSessionCredentialEvent(r, sink, slog.Default(), time.Now(), SessionContext{}, CredentialEvent{
		Action: AuditActionWebhookUpdate, TargetKind: "customer_webhook", TargetID: "x",
	})
	if sink.rows != 1 {
		t.Fatalf("audit rows after a cancelled request = %d, want 1", sink.rows)
	}
}
