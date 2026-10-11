package v1

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TLSCertProbeInterval is the cadence at which [RunTLSCertProbe]
// re-probes a host's leaf cert. The expiry alert fires at 14 days remaining, so
// a 6 h cadence re-confirms the timestamp 56 times first and a single failed
// probe never starves the gauge.
const TLSCertProbeInterval = 6 * time.Hour

// tlsCertProbeTimeout caps each probe. Handshakes against Caddy typically take
// under 300 ms; 10 s absorbs a network blip without holding the goroutine open
// through a deeper outage.
const tlsCertProbeTimeout = 10 * time.Second

// RunTLSCertProbe blocks until ctx is cancelled, periodically
// probing the configured hostnames via TLS handshake and emitting
// the leaf cert's NotAfter as a Prometheus gauge.
//
// Caddy auto-renews Let's Encrypt 30 days before expiry; a failed renewal (DNS,
// rate limit, ACME quota) is otherwise silent until the cert expires. The
// `stellarindex_tls_cert_not_after_unix` gauge lets an alert catch a stuck
// renewal against `time()`.
//
// The gauge is NOT cleared on probe failure: the last-known value stays, and
// the paired `stellarindex_tls_cert_probe_total{outcome}` counter detects a
// sustained probe error. The first probe runs immediately so a fresh process
// has a gauge before the first interval.
func RunTLSCertProbe(ctx context.Context, hosts []string, logger *slog.Logger) error {
	if len(hosts) == 0 {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	// Every (host, outcome) series exists at 0 before the first probe, so
	// the alert's increase() sees a fresh process's first failure.
	for _, host := range hosts {
		for _, outcome := range tlsCertProbeOutcomes() {
			obs.TLSCertProbeTotal.WithLabelValues(host, outcome).Add(0)
		}
	}

	// Kick off an immediate first round so the gauge is populated
	// before the first interval tick.
	probeAllHosts(ctx, hosts, logger)

	ticker := time.NewTicker(TLSCertProbeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			probeAllHosts(ctx, hosts, logger)
		}
	}
}

func probeAllHosts(ctx context.Context, hosts []string, logger *slog.Logger) {
	for _, host := range hosts {
		probeOneHost(ctx, host, logger)
	}
}

// probeOneHost performs one TLS-dial + cert-extract attempt
// against `host`. Bound by tlsCertProbeTimeout. Returns the
// outcome label that was incremented on TLSCertProbeTotal so
// callers (tests) can assert without scraping Prometheus.
func probeOneHost(ctx context.Context, host string, logger *slog.Logger) string {
	dialCtx, cancel := context.WithTimeout(ctx, tlsCertProbeTimeout)
	defer cancel()

	addr := host
	// Append :443 if no explicit port — host may already include one
	// when operators configure a non-default test endpoint.
	if _, _, err := net.SplitHostPort(host); err != nil {
		addr = host + ":443"
	}

	cfg := &tls.Config{ServerName: hostnameOnly(host), MinVersion: tls.VersionTLS12}
	if tlsConfigOverride != nil {
		tlsConfigOverride(cfg)
	}
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: tlsCertProbeTimeout},
		Config:    cfg,
	}
	conn, err := dialer.DialContext(dialCtx, "tcp", addr)
	if err != nil {
		return recordTLSDialFailure(host, err, logger)
	}
	defer func() { _ = conn.Close() }()

	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		obs.TLSCertProbeTotal.WithLabelValues(host, "no_cert").Inc()
		logger.Warn("tls cert probe: connection is not *tls.Conn", "host", host)
		return "no_cert"
	}
	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		obs.TLSCertProbeTotal.WithLabelValues(host, "no_cert").Inc()
		logger.Warn("tls cert probe: no peer certificates", "host", host)
		return "no_cert"
	}

	leaf := state.PeerCertificates[0]
	obs.TLSCertNotAfterUnix.WithLabelValues(host).Set(float64(leaf.NotAfter.Unix()))
	obs.TLSCertProbeTotal.WithLabelValues(host, "ok").Inc()
	logger.Debug("tls cert probe ok",
		"host", host,
		"not_after", leaf.NotAfter.Format(time.RFC3339),
		"days_remaining", fmt.Sprintf("%.1f", time.Until(leaf.NotAfter).Hours()/24))
	return "ok"
}

// tlsCertProbeOutcomes is every outcome label probeOneHost can emit.
func tlsCertProbeOutcomes() []string {
	return []string{"ok", "dial_error", "timeout", "no_cert", "cert_invalid", "cert_expired"}
}

// recordTLSDialFailure classifies a failed handshake. A leaf that failed
// verification still reached us, and the error carries it: its NotAfter
// is recorded and the outcome says expired vs otherwise invalid, so a
// bad cert is never reported as a bare "unreachable" with no gauge.
func recordTLSDialFailure(host string, err error, logger *slog.Logger) string {
	var cve *tls.CertificateVerificationError
	if errors.As(err, &cve) && len(cve.UnverifiedCertificates) > 0 {
		leaf := cve.UnverifiedCertificates[0]
		obs.TLSCertNotAfterUnix.WithLabelValues(host).Set(float64(leaf.NotAfter.Unix()))
		outcome := "cert_invalid"
		if time.Now().After(leaf.NotAfter) {
			outcome = "cert_expired"
		}
		obs.TLSCertProbeTotal.WithLabelValues(host, outcome).Inc()
		logger.Warn("tls cert probe: certificate failed verification",
			"host", host, "err", err, "outcome", outcome, "not_after", leaf.NotAfter.Format(time.RFC3339))
		return outcome
	}
	outcome := "dial_error"
	if errors.Is(err, context.DeadlineExceeded) {
		outcome = "timeout"
	}
	obs.TLSCertProbeTotal.WithLabelValues(host, outcome).Inc()
	logger.Warn("tls cert probe failed", "host", host, "err", err, "outcome", outcome)
	return outcome
}

// tlsConfigOverride is a test-only hook: tests that exercise the
// probe against an httptest TLS server need to flip
// InsecureSkipVerify to accept the test's self-signed cert. Nil
// in production (the default tls.Config is used verbatim).
var tlsConfigOverride func(*tls.Config) //nolint:gochecknoglobals // test seam

// hostnameOnly strips the :port suffix if present. tls.Config's
// ServerName needs the hostname part only — passing host:port
// fails the SNI lookup.
func hostnameOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
