package v1

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Per-caller producer quota: the global ceiling alone lets one address loop pairs x window_seconds, aborting each connection,
// and fill every slot with detached junk producers (surviving for tipProducerLinger) so other callers get 503. Connection caps
// cannot see it; without the quota check and mint charge in acquireFor the attacker is admitted every time.

// attackerCaller / bystanderCaller are documentation-range addresses
// (RFC 5737), not anything routable.
const (
	attackerCaller  = "203.0.113.7"
	bystanderCaller = "198.51.100.9"
)

// The quota is only as good as its key. IPv6 callers must aggregate to
// their /64: a quota keyed on the full /128 is bypassed by
// rotating the low bits of a prefix the caller already controls.
func TestTipProducerCaller_KeysOnTheRotatableBlock(t *testing.T) {
	callerFor := func(remoteAddr string) string {
		req := httptest.NewRequest(http.MethodGet, "/v1/price/tip/stream?asset=native", nil)
		req.RemoteAddr = remoteAddr
		return tipProducerCaller(req)
	}

	v6a := callerFor("[2001:db8:1:2::dead]:51234")
	v6b := callerFor("[2001:db8:1:2::beef]:51235")
	if v6a != v6b {
		t.Errorf("two addresses in one /64 keyed as %q and %q; an IPv6 caller "+
			"rotating within its own prefix would get a fresh quota per address",
			v6a, v6b)
	}
	if want := "2001:db8:1:2::"; v6a != want {
		t.Errorf("IPv6 caller key = %q, want the /64 prefix %q", v6a, want)
	}

	if got, want := callerFor("203.0.113.7:51234"), "203.0.113.7"; got != want {
		t.Errorf("IPv4 caller key = %q, want the exact address %q", got, want)
	}
	if got, want := callerFor("[2001:db8:1:3::1]:51234"), "2001:db8:1:3::"; got != want {
		t.Errorf("a DIFFERENT /64 keyed as %q, want %q — distinct subscribers must "+
			"not share one quota bucket", got, want)
	}

	// Fail-closed: an unresolvable caller shares one bucket rather than
	// being exempted, and NEVER resolves to the unattributed identity the
	// quota does not apply to.
	if got := callerFor(""); got != unknownTipCaller {
		t.Errorf("unresolvable caller key = %q, want %q", got, unknownTipCaller)
	}
	for _, addr := range []string{"", "203.0.113.7:51234", "[2001:db8::1]:1", "not-an-addr"} {
		if got := callerFor(addr); got == unattributedTipCaller {
			t.Errorf("RemoteAddr %q resolved to the unattributed caller, which the "+
				"quota does not apply to — the HTTP path must always be charged", addr)
		}
	}
}
