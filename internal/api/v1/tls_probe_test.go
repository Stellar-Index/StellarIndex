package v1

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// startTLSServer brings up an httptest TLS server whose leaf cert
// has the supplied NotAfter. Returns the host:port the test should
// pass to probeOneHost.
func startTLSServer(t *testing.T, notAfter time.Time) (string, func()) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	cert := tls.Certificate{
		Certificate: [][]byte{derBytes},
		PrivateKey:  priv,
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return u.Host, srv.Close
}

func TestProbeOneHost_OK(t *testing.T) {
	want := time.Now().Add(90 * 24 * time.Hour).Truncate(time.Second)
	host, cleanup := startTLSServer(t, want)
	t.Cleanup(cleanup)

	// httptest's TLS server uses a self-signed cert. probeOneHost
	// hits `InsecureSkipVerify=false` by default — so we expect
	// the dial to fail signature verification. The probe code
	// path should still emit a dial_error outcome. To get the
	// happy-path coverage, set InsecureSkipVerify in this test
	// via a focused override; the real production code keeps
	// strict verification.
	prev := tlsConfigOverride
	tlsConfigOverride = func(c *tls.Config) { c.InsecureSkipVerify = true } //nolint:gosec // test override only
	t.Cleanup(func() { tlsConfigOverride = prev })

	before := testutil.ToFloat64(obs.TLSCertProbeTotal.WithLabelValues(host, "ok"))
	got := probeOneHost(context.Background(), host, slog.Default())
	if got != "ok" {
		t.Fatalf("outcome = %q, want ok", got)
	}
	after := testutil.ToFloat64(obs.TLSCertProbeTotal.WithLabelValues(host, "ok"))
	if after-before != 1 {
		t.Errorf("counter delta = %v, want 1", after-before)
	}
	gauge := testutil.ToFloat64(obs.TLSCertNotAfterUnix.WithLabelValues(host))
	if gauge != float64(want.Unix()) {
		t.Errorf("gauge = %v, want %v (NotAfter)", gauge, want.Unix())
	}
}

// TestProbeOneHost_CertFailingVerificationIsStillRead is the
// CA2-A08-correct-6 regression. The production dial verifies the chain,
// so an expired or mis-issued leaf failed the handshake and was counted
// as a bare dial_error with its NotAfter never read: after a restart the
// host had no gauge at all, and the one condition the probe exists to
// catch was reported as "unreachable". The handshake error carries the
// unverified chain; the probe must record the leaf and say which failure.
func TestProbeOneHost_CertFailingVerificationIsStillRead(t *testing.T) {
	for _, tc := range []struct {
		name     string
		notAfter time.Time
		outcome  string
	}{
		{"expired", time.Now().Add(-10 * time.Minute).Truncate(time.Second), "cert_expired"},
		{"untrusted chain", time.Now().Add(90 * 24 * time.Hour).Truncate(time.Second), "cert_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, stop := startTLSServer(t, tc.notAfter)
			defer stop()
			before := testutil.ToFloat64(obs.TLSCertProbeTotal.WithLabelValues(host, tc.outcome))
			if got := probeOneHost(context.Background(), host, slog.New(slog.NewTextHandler(io.Discard, nil))); got != tc.outcome {
				t.Fatalf("outcome = %q, want %q", got, tc.outcome)
			}
			if after := testutil.ToFloat64(obs.TLSCertProbeTotal.WithLabelValues(host, tc.outcome)); after != before+1 {
				t.Errorf("%s counter %v → %v, want +1", tc.outcome, before, after)
			}
			if gauge := testutil.ToFloat64(obs.TLSCertNotAfterUnix.WithLabelValues(host)); gauge != float64(tc.notAfter.Unix()) {
				t.Errorf("not_after gauge = %v, want the leaf's %d", gauge, tc.notAfter.Unix())
			}
		})
	}
}

func TestProbeOneHost_DialError(t *testing.T) {
	// Port 1 on localhost — almost always closed. Expect dial_error.
	host := "127.0.0.1:1"
	before := testutil.ToFloat64(obs.TLSCertProbeTotal.WithLabelValues(host, "dial_error"))
	got := probeOneHost(context.Background(), host, slog.Default())
	if got != "dial_error" && got != "timeout" {
		// Some OS/sandbox combos can return timeout instead of
		// refused. Accept either failure outcome.
		t.Errorf("outcome = %q, want dial_error or timeout", got)
	}
	after := testutil.ToFloat64(obs.TLSCertProbeTotal.WithLabelValues(host, got))
	if after-before != 1 {
		t.Errorf("counter delta = %v, want 1", after-before)
	}
}

func TestHostnameOnly(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"api.stellarindex.io", "api.stellarindex.io"},
		{"api.stellarindex.io:443", "api.stellarindex.io"},
		{"127.0.0.1:8443", "127.0.0.1"},
		{"[::1]:8443", "::1"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := hostnameOnly(tc.in)
			if got != tc.want {
				t.Errorf("hostnameOnly(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRunTLSCertProbe_EmptyHostsIsNoOp(t *testing.T) {
	// Empty host list returns immediately rather than blocking on
	// the timer. Uses an already-canceled ctx to assert no time-
	// based progression.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RunTLSCertProbe(ctx, nil, slog.Default()); err != nil {
		// nil-host list path should return nil before ctx is even
		// consulted.
		t.Errorf("err = %v, want nil for empty hosts", err)
	}
}

// Sanity: the encoded help text mentions both metrics & the
// alert intent so dashboards / runbooks line up.
func TestTLSCertMetrics_HelpText(t *testing.T) {
	// Force a series creation so the registry knows about it.
	obs.TLSCertNotAfterUnix.WithLabelValues("synthetic").Set(0)
	obs.TLSCertProbeTotal.WithLabelValues("synthetic", "ok").Inc()

	gathered, err := obs.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	wantNames := map[string]bool{
		"stellarindex_tls_cert_not_after_unix": false,
		"stellarindex_tls_cert_probe_total":    false,
	}
	for _, mf := range gathered {
		if _, want := wantNames[mf.GetName()]; want {
			wantNames[mf.GetName()] = true
			if !strings.Contains(mf.GetHelp(), "F-0051") {
				t.Errorf("metric %q help missing F-0051 ref: %q", mf.GetName(), mf.GetHelp())
			}
		}
	}
	for name, found := range wantNames {
		if !found {
			t.Errorf("metric %q not found in Gather output", name)
		}
	}
}
