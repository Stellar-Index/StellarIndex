package ingest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/metadata"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// sep1FailureReasons is the closed label set of stellarindex_sep1_refresh_failures. Every
// reason is emitted on every run, so a rate over one of them never starts from a missing series.
var sep1FailureReasons = []string{
	"timeout", "dns", "connect", "tls", "http", "redirect", "parse", "too_large", "too_deep", "ssrf",
	"invalid_domain", "payload", "domain", "lake", "write", "other",
}

type sep1FailureKey struct {
	reason  string
	reached bool
}

type sep1RunStats struct {
	attempted, stored, failed, ourFault int
	reached, regressed                  int
	failures                            map[sep1FailureKey]int
	aborted                             bool
}

func (s *sep1RunStats) record(c timescale.IssuerSep1Candidate, outcome sep1Outcome, reason string) {
	s.attempted++
	switch outcome {
	case sep1Stored:
		s.stored++
	case sep1Failed:
		s.failed++
	case sep1OurFault:
		s.ourFault++
	}
	if c.Reached {
		s.reached++
	}
	if outcome == sep1Stored {
		return
	}
	if c.Reached {
		s.regressed++
	}
	if s.failures == nil {
		s.failures = map[sep1FailureKey]int{}
	}
	s.failures[sep1FailureKey{reason: reason, reached: c.Reached}]++
}

// sep1FailureReason names why one resolver call failed, for the textfile's reason label.
// The context deadline is checked first: url.Error.Timeout does not see it, and a lookup
// cut short by it surfaces as a DNS error with IsTimeout set.
func sep1FailureReason(err error) string {
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var certInvalid x509.CertificateInvalidError
	var recordErr tls.RecordHeaderError
	var netErr net.Error
	var opErr *net.OpError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &dnsErr):
		if dnsErr.IsTimeout {
			return "timeout"
		}
		return "dns"
	case errors.Is(err, metadata.ErrSSRFBlocked):
		return "ssrf"
	case errors.Is(err, metadata.ErrHTTPStatus):
		return "http"
	case errors.Is(err, metadata.ErrRedirectRefused):
		return "redirect"
	case errors.Is(err, metadata.ErrTOMLParse):
		return "parse"
	case errors.Is(err, metadata.ErrTOMLTooLarge):
		return "too_large"
	case errors.Is(err, metadata.ErrTOMLTooDeep):
		return "too_deep"
	case errors.Is(err, metadata.ErrInvalidDomain):
		return "invalid_domain"
	case errors.As(err, &certErr), errors.As(err, &unknownCA), errors.As(err, &hostErr),
		errors.As(err, &certInvalid), errors.As(err, &recordErr), sep1TLSAlert(err):
		return "tls"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.As(err, &opErr):
		return "connect"
	}
	return "other"
}

// sep1TLSAlert reports a TLS alert, which crypto/tls delivers as a net.OpError whose Op is
// "remote error" or "local error" rather than as a typed certificate error.
func sep1TLSAlert(err error) bool {
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		return false
	}
	return opErr.Op == "remote error" || opErr.Op == "local error"
}

// stampSep1RefreshTextfile publishes the run's counters for node_exporter; a write failure is
// reported, never fatal, because the refresh itself has already happened.
func stampSep1RefreshTextfile(path string, s sep1RunStats, systemic bool, now time.Time) {
	if path == "" {
		return
	}
	if err := writeTextfileAtomic(path, sep1RefreshTextfile(s, systemic, now)); err != nil {
		fmt.Fprintf(os.Stderr, "sep1-refresh: WARN textfile: %v\n", err)
	}
}

func sep1RefreshTextfile(s sep1RunStats, systemic bool, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# HELP stellarindex_sep1_refresh_last_run_unix Unix time the most recent sep1-refresh run finished, an empty queue included.\n# TYPE stellarindex_sep1_refresh_last_run_unix gauge\nstellarindex_sep1_refresh_last_run_unix %d\n", now.Unix())
	fmt.Fprintf(&b, "# HELP stellarindex_sep1_refresh_attempted Issuers the most recent run attempted.\n# TYPE stellarindex_sep1_refresh_attempted gauge\nstellarindex_sep1_refresh_attempted %d\n", s.attempted)
	fmt.Fprintf(&b, "# HELP stellarindex_sep1_refresh_issuers Issuers of the most recent run by outcome: stored, failed (the domain's fault) or our_fault (lake or store error; backoff unwound).\n# TYPE stellarindex_sep1_refresh_issuers gauge\n")
	fmt.Fprintf(&b, "stellarindex_sep1_refresh_issuers{outcome=%q} %d\n", "stored", s.stored)
	fmt.Fprintf(&b, "stellarindex_sep1_refresh_issuers{outcome=%q} %d\n", "failed", s.failed)
	fmt.Fprintf(&b, "stellarindex_sep1_refresh_issuers{outcome=%q} %d\n", "our_fault", s.ourFault)
	fmt.Fprintf(&b, "# HELP stellarindex_sep1_refresh_reached Attempted issuers whose domain served a stellar.toml at its last attempt: the denominator a regression is judged against.\n# TYPE stellarindex_sep1_refresh_reached gauge\nstellarindex_sep1_refresh_reached %d\n", s.reached)
	fmt.Fprintf(&b, "# HELP stellarindex_sep1_refresh_regressed Reached issuers that failed in the most recent run.\n# TYPE stellarindex_sep1_refresh_regressed gauge\nstellarindex_sep1_refresh_regressed %d\n", s.regressed)
	fmt.Fprintf(&b, "# HELP stellarindex_sep1_refresh_failures Failed issuers of the most recent run by reason and by whether the domain had served a stellar.toml before; every reason is listed, 0 when none.\n# TYPE stellarindex_sep1_refresh_failures gauge\n")
	for _, reason := range sep1FailureReasons {
		for _, reached := range []bool{true, false} {
			fmt.Fprintf(&b, "stellarindex_sep1_refresh_failures{reason=%q,reached=%q} %d\n",
				reason, strconv.FormatBool(reached), s.failures[sep1FailureKey{reason: reason, reached: reached}])
		}
	}
	fmt.Fprintf(&b, "# HELP stellarindex_sep1_refresh_systemic Whether the most recent run judged its failures a fault on our side and unwound the backoff it had applied.\n# TYPE stellarindex_sep1_refresh_systemic gauge\nstellarindex_sep1_refresh_systemic %d\n", boolGauge(systemic))
	fmt.Fprintf(&b, "# HELP stellarindex_sep1_refresh_aborted Whether the most recent run hit its wall-clock deadline before finishing its queue.\n# TYPE stellarindex_sep1_refresh_aborted gauge\nstellarindex_sep1_refresh_aborted %d\n", boolGauge(s.aborted))
	return b.String()
}

func boolGauge(v bool) int {
	if v {
		return 1
	}
	return 0
}
