package v1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Per-caller producer quota (audit-2026-09-02 F054 / K010).
//
// The GLOBAL ceiling bounds the total but partitions it by nothing, so
// one unauthenticated address looping the key space — ~9 real pairs ×
// window_seconds 1..60, aborting each connection as soon as the headers
// arrive — filled every slot with junk producers and 503'd every other
// caller's first request for an unwatched pair. The connection caps
// cannot see it: the producer is detached by design and survives the
// aborted connection for tipProducerLinger, which is exactly the window
// the flood exploits.
//
// Proven red against the unfixed registry: with the quota check and the
// mint charge removed from acquireFor, the attacker below is admitted on
// every attempt and running() reaches the full attempt count.

// attackerCaller / bystanderCaller are documentation-range addresses
// (RFC 5737), not anything routable.
const (
	attackerCaller  = "203.0.113.7"
	bystanderCaller = "198.51.100.9"
)

func TestTipProducerRegistry_OneCallerCannotMonopoliseTheGlobalPool(t *testing.T) {
	const (
		ceiling  = 64
		quota    = 8
		attempts = 40
	)
	// A long linger is the flood's own shape: the producer outlives the
	// aborted connection, so releasing does NOT give the slot back.
	reg := &tipProducerRegistry{
		maxProducers: ceiling,
		maxPerCaller: quota,
		lingerFor:    time.Hour,
	}

	admitted, quotaRefusals := 0, 0
	for w := 1; w <= attempts; w++ {
		key := tipProducerKey{asset: "native", quote: "fiat:USD", window: w}
		release, outcome := reg.acquireFor(key, attackerCaller, nil,
			func(ctx context.Context) { <-ctx.Done() })
		switch outcome {
		case tipProducerAdmitted:
			admitted++
			// The attack shape: abort immediately.
			release()
		case tipProducerAtCallerQuota:
			quotaRefusals++
			if release != nil {
				t.Fatal("a refused acquire must not hand back a release func — " +
					"calling it would discharge a producer this caller never took")
			}
		case tipProducerAtGlobalCeiling:
			t.Fatalf("window %d hit the GLOBAL ceiling at %d producers; one caller "+
				"reached the shared pool's bound, which is the finding", w, reg.running())
		}
	}

	if admitted != quota {
		t.Errorf("one caller minted %d producers, want exactly its quota %d", admitted, quota)
	}
	if got := reg.running(); got != quota {
		t.Errorf("running() = %d, want %d — every slot past the quota is a slot "+
			"this caller took from everyone else", got, quota)
	}
	if got := reg.mintedFor(attackerCaller); got != quota {
		t.Errorf("mintedFor(attacker) = %d, want %d — the charge must survive the "+
			"aborted connection for as long as the producer's entry does", got, quota)
	}
	if quotaRefusals != attempts-quota {
		t.Errorf("per-caller refusals = %d, want %d", quotaRefusals, attempts-quota)
	}
	if got := reg.refusedPerCallerCount(); got != uint64(attempts-quota) {
		t.Errorf("refusedPerCallerCount() = %d, want %d — a flood must be visible, "+
			"not merely survived", got, attempts-quota)
	}

	// The whole point: a bystander's unwatched pair is still served.
	release, outcome := reg.acquireFor(
		tipProducerKey{asset: "native", quote: "fiat:EUR", window: 1},
		bystanderCaller, nil, func(ctx context.Context) { <-ctx.Done() })
	if outcome != tipProducerAdmitted {
		t.Fatalf("a bystander's new pair was refused (%s) while one address held "+
			"%d producers — that 503 is the harm the quota exists to prevent",
			outcome, quota)
	}
	release()
}

// The charge is held for the registry ENTRY's life, not the connection's.
// Releasing while the producer lingers must NOT give the slot back: the
// linger is precisely the window the abort-loop flood runs in.
func TestTipProducerRegistry_CallerSlotReturnsOnlyWhenTheEntryLeaves(t *testing.T) {
	const quota = 2
	reg := &tipProducerRegistry{maxPerCaller: quota, lingerFor: 20 * time.Millisecond}
	start := func(ctx context.Context) { <-ctx.Done() }

	for w := 1; w <= quota; w++ {
		release, outcome := reg.acquireFor(
			tipProducerKey{asset: "native", quote: "fiat:USD", window: w},
			attackerCaller, nil, start)
		if outcome != tipProducerAdmitted {
			t.Fatalf("window %d refused below the quota (%s)", w, outcome)
		}
		release()
	}

	// Still lingering → still charged.
	if _, outcome := reg.acquireFor(
		tipProducerKey{asset: "native", quote: "fiat:USD", window: quota + 1},
		attackerCaller, nil, start,
	); outcome != tipProducerAtCallerQuota {
		t.Fatalf("outcome = %s while this caller's producers were still lingering; "+
			"want %s — releasing the connection must not return the slot",
			outcome, tipProducerAtCallerQuota)
	}

	// Once the entries actually leave, the slots come back.
	if !waitFor(2*time.Second, func() bool { return reg.mintedFor(attackerCaller) == 0 }) {
		t.Fatalf("mintedFor(attacker) = %d after the linger expired, want 0 — "+
			"the charge leaked and the caller is permanently locked out",
			reg.mintedFor(attackerCaller))
	}
	release, outcome := reg.acquireFor(
		tipProducerKey{asset: "native", quote: "fiat:USD", window: quota + 1},
		attackerCaller, nil, start)
	if outcome != tipProducerAdmitted {
		t.Fatalf("outcome = %s after the linger expired, want %s", outcome, tipProducerAdmitted)
	}
	release()
}

// Joining an ALREADY-RUNNING producer costs nothing to serve and must
// never be charged — it is the page-reload case the linger exists for,
// and charging it would turn a popular pair's own audience away.
func TestTipProducerRegistry_JoiningAnExistingProducerIsNeverCharged(t *testing.T) {
	reg := &tipProducerRegistry{maxPerCaller: 1, lingerFor: time.Hour}
	start := func(ctx context.Context) { <-ctx.Done() }
	key := tipProducerKey{asset: "native", quote: "fiat:USD", window: 5}

	first, outcome := reg.acquireFor(key, attackerCaller, nil, start)
	if outcome != tipProducerAdmitted {
		t.Fatalf("first acquire = %s, want %s", outcome, tipProducerAdmitted)
	}
	defer first()

	second, outcome := reg.acquireFor(key, attackerCaller, nil, start)
	if outcome != tipProducerAdmitted {
		t.Fatalf("the same caller's SECOND viewer of its own pair = %s, want %s",
			outcome, tipProducerAdmitted)
	}
	defer second()
	if got := reg.mintedFor(attackerCaller); got != 1 {
		t.Errorf("mintedFor(attacker) = %d after joining its own producer, want 1", got)
	}

	other, outcome := reg.acquireFor(key, bystanderCaller, nil, start)
	if outcome != tipProducerAdmitted {
		t.Fatalf("a bystander joining a running producer = %s, want %s",
			outcome, tipProducerAdmitted)
	}
	defer other()
	if got := reg.mintedFor(bystanderCaller); got != 0 {
		t.Errorf("mintedFor(bystander) = %d for a JOIN, want 0 — only mints are charged", got)
	}
}

// The quota is only as good as its key. IPv6 callers must aggregate to
// their /64 (SEC-15): a quota keyed on the full /128 is bypassed by
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
