package dashboardauth

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/notify"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// stubFailSender is a notify.Sender whose Send always fails — the shape of a
// Resend outage from the login handler's point of view.
type stubFailSender struct{ err error }

func (s stubFailSender) Send(context.Context, notify.Message) error { return s.err }

func notifyCount(t *testing.T, template, result string) float64 {
	t.Helper()
	return testutil.ToFloat64(obs.NotifySendsTotal.WithLabelValues(template, result))
}
