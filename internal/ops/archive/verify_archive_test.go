package archive

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestCheckpointAnchorDecision_AllMissedFailsRegardlessOfFlag is the
// DAT-09 regression: checkpointsOK==0 && checkpointsMissed>0 (every
// checkpoint anchor missed) must return a non-nil error even when
// -fail-on-missed is NOT set — an all-missed range verified nothing
// against the cross-anchor archive and must not be certified
// complete or advance the checkpoint tier's high-water mark.
func TestCheckpointAnchorDecision_AllMissedFailsRegardlessOfFlag(t *testing.T) {
	for _, failOnMissed := range []bool{false, true} {
		err := checkpointAnchorDecision(0, 5, failOnMissed)
		if err == nil {
			t.Fatalf("failOnMissed=%v: expected a non-nil error when checkpointsOK=0 checkpointsMissed=5, got nil", failOnMissed)
		}
		if !strings.Contains(err.Error(), "inconclusive") {
			t.Errorf("failOnMissed=%v: expected 'inconclusive' in error, got: %v", failOnMissed, err)
		}
	}
}

// TestCheckpointAnchorDecision_AllMatchedIsClean: every checkpoint
// matched — clean regardless of failOnMissed.
func TestCheckpointAnchorDecision_AllMatchedIsClean(t *testing.T) {
	for _, failOnMissed := range []bool{false, true} {
		if err := checkpointAnchorDecision(10, 0, failOnMissed); err != nil {
			t.Errorf("failOnMissed=%v: expected nil error when all checkpoints matched, got %v", failOnMissed, err)
		}
	}
}

// TestCheckpointAnchorDecision_PartialMiss: SOME matched, some
// missed — the pre-existing -fail-on-missed gate applies (distinct
// from the DAT-09 all-missed case).
func TestCheckpointAnchorDecision_PartialMiss(t *testing.T) {
	if err := checkpointAnchorDecision(8, 2, false); err != nil {
		t.Errorf("partial miss without -fail-on-missed should be clean, got %v", err)
	}
	err := checkpointAnchorDecision(8, 2, true)
	if err == nil {
		t.Fatal("partial miss WITH -fail-on-missed should fail, got nil")
	}
	if !strings.Contains(err.Error(), "fail-on-missed") {
		t.Errorf("expected 'fail-on-missed' in error, got: %v", err)
	}
}

// TestCheckpointAnchorDecision_NoCheckpointsAttempted: 0/0 (the
// doCheckpoint gate wasn't even reached, or the range genuinely had
// zero checkpoint positions upstream) must not be treated as the
// all-missed failure — checkpointsMissed must be > 0 to trigger it.
func TestCheckpointAnchorDecision_NoCheckpointsAttempted(t *testing.T) {
	if err := checkpointAnchorDecision(0, 0, false); err != nil {
		t.Errorf("0 matched / 0 missed should not trip the all-missed guard, got %v", err)
	}
}

// TestWatchdogGate is the OBS-07 regression: the systemd WATCHDOG=1
// ping used to fire every 30s unconditionally, so it detected a
// crashed process but was blind to a HUNG one — the exact failure the
// unit's WatchdogSec=1h exists to catch. The ping must now be withheld
// while the walk is armed but not advancing.
func TestWatchdogGate(t *testing.T) {
	t.Parallel()
	var p verifyArchiveProgressTracker
	gate := newWatchdogGate(&p)

	// Not yet walking (startup, peer diff, archivist scan): ping
	// unconditionally — those phases verify no ledgers and are bounded
	// by their own timeouts.
	for i := 0; i < 3; i++ {
		if !gate.shouldPing() {
			t.Fatalf("tick %d: withheld the ping while the walk was not armed", i)
		}
	}

	// Walk armed and advancing: keep feeding the watchdog.
	p.WalkActive.Store(true)
	for i := 0; i < 3; i++ {
		p.Ledgers.Add(5000)
		if !gate.shouldPing() {
			t.Fatalf("tick %d: withheld the ping while ledgers were advancing", i)
		}
	}

	// Walk armed and WEDGED: withhold, every tick, so systemd's
	// WatchdogSec eventually acts.
	for i := 0; i < 3; i++ {
		if gate.shouldPing() {
			t.Fatalf("tick %d: fed the watchdog with zero ledgers verified since the last interval", i)
		}
	}

	// Recovery: progress resumes → pings resume.
	p.Ledgers.Add(1)
	if !gate.shouldPing() {
		t.Error("withheld the ping after the walk resumed making progress")
	}

	// Walk finished: back to unconditional.
	p.WalkActive.Store(false)
	if !gate.shouldPing() {
		t.Error("withheld the ping after the walk completed")
	}
}

// TestPeerCheckpointBounds is the OBS-07 regression for Tier D's
// sample range. The old form fabricated `lastCP = firstCP + 640` for
// an unbounded -to, so a default `-tier peers` run sampled ledgers
// 63..703 — pure genesis — and printed "peer cross-check OK". It also
// underflowed for to < 64 and dropped `to` itself when `to` was
// exactly a checkpoint.
func TestPeerCheckpointBounds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                string
		from, to            uint32
		wantFirst, wantLast uint32
		wantErr             bool
	}{
		{name: "genesis defaults", from: 2, to: 703, wantFirst: 63, wantLast: 703},
		{name: "to is itself a checkpoint", from: 2, to: 127, wantFirst: 63, wantLast: 127},
		{name: "to just below a checkpoint", from: 2, to: 126, wantFirst: 63, wantLast: 63},
		{name: "from is itself a checkpoint", from: 63, to: 127, wantFirst: 63, wantLast: 127},
		{name: "from just above a checkpoint", from: 64, to: 191, wantFirst: 127, wantLast: 191},
		{name: "trailing-edge range", from: 60_000_000, to: 60_000_128, wantFirst: 60_000_063, wantLast: 60_000_127},
		// The underflow: (to/64*64)-1 wrapped to 4294967295 and sailed
		// past the "no checkpoints" guard.
		{name: "to below the first checkpoint", from: 2, to: 62, wantErr: true},
		{name: "to zero", from: 2, to: 0, wantErr: true},
		{name: "inverted range", from: 500, to: 100, wantErr: true},
		{name: "range between two checkpoints", from: 64, to: 126, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			first, last, err := peerCheckpointBounds(tc.from, tc.to)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("peerCheckpointBounds(%d,%d) = (%d,%d), want an error", tc.from, tc.to, first, last)
				}
				return
			}
			if err != nil {
				t.Fatalf("peerCheckpointBounds(%d,%d): %v", tc.from, tc.to, err)
			}
			if first != tc.wantFirst || last != tc.wantLast {
				t.Errorf("peerCheckpointBounds(%d,%d) = (%d,%d), want (%d,%d)",
					tc.from, tc.to, first, last, tc.wantFirst, tc.wantLast)
			}
			if first%64 != 63 || last%64 != 63 {
				t.Errorf("bounds (%d,%d) are not checkpoint ledgers (seq mod 64 == 63)", first, last)
			}
		})
	}
}

// TestPeerArchiveTip: an unbounded -to resolves against the peers'
// published tip, and takes the LOWEST of them so a checkpoint one peer
// hasn't uploaded yet isn't sampled as a divergence.
func TestPeerArchiveTip(t *testing.T) {
	t.Parallel()
	newPeer := func(t *testing.T, currentLedger uint32) string {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/.well-known/stellar-history.json" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = fmt.Fprintf(w, `{"currentLedger":%d,"currentBuckets":[]}`, currentLedger)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	client := &http.Client{Timeout: 5 * time.Second}

	ahead := newPeer(t, 60_000_255)
	behind := newPeer(t, 60_000_063)
	tip, ok := peerArchiveTip(client, []string{ahead, behind})
	if !ok {
		t.Fatal("peerArchiveTip: ok = false, want a resolved tip")
	}
	if tip != 60_000_063 {
		t.Errorf("tip = %d, want 60000063 (the LOWEST published tip)", tip)
	}

	// The resolved tip must place the sample window at the trailing
	// edge, not at genesis (the actual defect).
	first, last, err := peerCheckpointBounds(2, tip)
	if err != nil {
		t.Fatalf("peerCheckpointBounds: %v", err)
	}
	if last != 60_000_063 {
		t.Errorf("last sampled checkpoint = %d, want 60000063 (the trailing edge)", last)
	}
	if first != 63 {
		t.Errorf("first sampled checkpoint = %d, want 63", first)
	}

	// No peer answering must be reported, never silently guessed.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(dead.Close)
	if _, ok := peerArchiveTip(client, []string{dead.URL}); ok {
		t.Error("peerArchiveTip: ok = true for a peer that never answered")
	}
}
