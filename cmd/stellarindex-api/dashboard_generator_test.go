package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// The one secret keys both the login-code HMAC and the passkey ceremony
// cookie, so the unset-env warning must name both casualties: an operator
// reading only "codes" would not connect it to looping passkey sign-ins.
func TestBuildDashboardGenerator_UnsetSecretWarningNamesPasskeyCeremonies(t *testing.T) {
	const env = "STELLARINDEX_TEST_DASHBOARD_CODE_SECRET_UNSET"
	t.Setenv(env, "")
	var buf bytes.Buffer
	g := buildDashboardGenerator(config.DashboardConfig{CodeSecretEnv: env}, slog.New(slog.NewTextHandler(&buf, nil)))
	if len(g.Secret) != 0 {
		t.Fatal("no env set, yet the generator carries a secret")
	}
	out := buf.String()
	for _, want := range []string{"sign-in codes", "passkey ceremonies", "more than one API instance"} {
		if !strings.Contains(out, want) {
			t.Errorf("unset-secret warning does not mention %q; got %q", want, out)
		}
	}
}
