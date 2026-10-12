package archive

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// TestCheckpointAnchorDecision_AllMissedFailsRegardlessOfFlag: an all-missed range errors even without -fail-on-missed.
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

// TestCheckpointAnchorDecision_AllMatchedIsClean: all matched is clean regardless of failOnMissed.
func TestCheckpointAnchorDecision_AllMatchedIsClean(t *testing.T) {
	for _, failOnMissed := range []bool{false, true} {
		if err := checkpointAnchorDecision(10, 0, failOnMissed); err != nil {
			t.Errorf("failOnMissed=%v: expected nil error when all checkpoints matched, got %v", failOnMissed, err)
		}
	}
}

// TestCheckpointAnchorDecision_PartialMiss: some missed falls under the -fail-on-missed gate, not the all-missed rule.
func TestCheckpointAnchorDecision_PartialMiss(t *testing.T) {
	if err := checkpointAnchorDecision(8, 2, false); err != nil {
		t.Errorf("partial miss with -fail-on-missed=false should be clean, got %v", err)
	}
	err := checkpointAnchorDecision(8, 2, true)
	if err == nil {
		t.Fatal("partial miss WITH -fail-on-missed should fail, got nil")
	}
	if !strings.Contains(err.Error(), "fail-on-missed") {
		t.Errorf("expected 'fail-on-missed' in error, got: %v", err)
	}
}

// TestVerifyArchive_FailOnMissedDefaultsOn pins ADR-0017 X1.7: omitting the flag still exits non-zero on a partial miss.
func TestVerifyArchive_FailOnMissedDefaultsOn(t *testing.T) {
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = orig })
	parseErr := verifyArchive([]string{"-h"})
	_ = w.Close()
	os.Stderr = orig
	out, _ := io.ReadAll(r)
	if !errors.Is(parseErr, flag.ErrHelp) {
		t.Fatalf("verifyArchive(-h) = %v, want flag.ErrHelp", parseErr)
	}

	_, usage, ok := strings.Cut(string(out), "-fail-on-missed\n")
	if !ok {
		t.Fatalf("-fail-on-missed missing from usage:\n%s", out)
	}
	if next := strings.Index(usage, "\n  -"); next >= 0 {
		usage = usage[:next]
	}
	if !strings.Contains(usage, "(default true)") {
		t.Errorf("-fail-on-missed must default to true; usage entry:\n%s", usage)
	}
}

// TestCheckpointAnchorDecision_NoCheckpointsAttempted: 0/0 is not the all-missed failure, which needs checkpointsMissed > 0.
func TestCheckpointAnchorDecision_NoCheckpointsAttempted(t *testing.T) {
	if err := checkpointAnchorDecision(0, 0, false); err != nil {
		t.Errorf("0 matched / 0 missed should not trip the all-missed guard, got %v", err)
	}
}

// TestWatchdogGate: an unconditional systemd WATCHDOG=1 ping is blind to a HUNG walk, so the ping is withheld while the walk is armed but not advancing.
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

func TestPeerSampleCheckpointsAlwaysIncludesBothEnds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		first, last uint32
		sampleN     int
		want        []uint32
	}{
		{name: "single checkpoint", first: 63, last: 63, sampleN: 20, want: []uint32{63}},
		{name: "one sample", first: 63, last: 703, sampleN: 1, want: []uint32{63, 703}},
		{name: "zero samples", first: 63, last: 703, sampleN: 0, want: []uint32{63, 703}},
		{name: "negative samples", first: 63, last: 703, sampleN: -1, want: []uint32{63, 703}},
		{name: "two samples", first: 63, last: 319, sampleN: 2, want: []uint32{63, 191, 319}},
		{name: "more samples than checkpoints", first: 63, last: 255, sampleN: 20, want: []uint32{63, 127, 191, 255}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := peerSampleCheckpoints(tc.first, tc.last, tc.sampleN); !slices.Equal(got, tc.want) {
				t.Fatalf("peerSampleCheckpoints(%d,%d,%d) = %v, want %v", tc.first, tc.last, tc.sampleN, got, tc.want)
			}
		})
	}
}

// TestPeerCheckpointBounds pins Tier D's range: no fabricated lastCP for unbounded -to, no underflow below 64, `to` kept when a checkpoint.
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

// TestPeerArchiveTip: an unbounded -to resolves to the LOWEST peer tip so a not-yet-uploaded checkpoint is not sampled as divergence.
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

// TestFetchHistoryCheckpoint_RejectsZeroCurrentLedger: a 200 decoding to the zero value must fail, not report peers agree on nothing.
func TestFetchHistoryCheckpoint_RejectsZeroCurrentLedger(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A missing-key error envelope that still 200s, e.g. behind a
		// misconfigured CDN — the observed real-world shape.
		_, _ = fmt.Fprint(w, `{"error":"NoSuchKey"}`)
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	_, err := fetchHistoryCheckpoint(client, srv.URL)
	if err == nil {
		t.Fatal("fetchHistoryCheckpoint: got nil error for a CurrentLedger=0 body, want an error")
	}
}

// TestPeerCheckpointQuorum_RequiresMajority: quorum is a strict majority of the configured peers, not a flat floor of 2.
func TestPeerCheckpointQuorum_RequiresMajority(t *testing.T) {
	t.Parallel()
	cases := []struct {
		numPeers, quorum int
	}{
		{2, 2},
		{7, 4},
		{3, 2},
	}
	for _, tc := range cases {
		got := peerCheckpointQuorum(tc.numPeers)
		if got != tc.quorum {
			t.Errorf("peerCheckpointQuorum(%d) = %d, want %d", tc.numPeers, got, tc.quorum)
		}
	}
	// 2 of 7 responders must fall below quorum(7)=4.
	if 2 >= peerCheckpointQuorum(7) {
		t.Fatalf("2 responders must be below quorum(7)=%d", peerCheckpointQuorum(7))
	}
}

// TestPeerArchiveTip_ExcludesStalePeer: a peer trailing the freshest by more than peerTipStalenessWindow must not cap the verified window.
func TestPeerArchiveTip_ExcludesStalePeer(t *testing.T) {
	t.Parallel()
	newPeer := func(t *testing.T, currentLedger uint32) string {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, `{"currentLedger":%d,"currentBuckets":[]}`, currentLedger)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	client := &http.Client{Timeout: 5 * time.Second}

	fresh := newPeer(t, 60_000_000)
	// Abandoned three months ago, still serving a frozen file — far
	// more than peerTipStalenessWindow behind the fresh peer.
	stale := newPeer(t, 58_000_000)

	tip, ok := peerArchiveTip(client, []string{fresh, stale})
	if !ok {
		t.Fatal("peerArchiveTip: ok = false, want a resolved tip")
	}
	if tip != 60_000_000 {
		t.Errorf("tip = %d, want 60000000 (the fresh peer; the stale one must be excluded)", tip)
	}
}

// TestDefaultTier1Peers_AllHTTPS: Tier D detects on-path forks, so a plaintext http:// peer is forgeable by the attacker it targets.
func TestDefaultTier1Peers_AllHTTPS(t *testing.T) {
	t.Parallel()
	for _, p := range defaultTier1Peers {
		if !strings.HasPrefix(p, "https://") {
			t.Errorf("defaultTier1Peers contains a non-https URL: %s", p)
		}
	}
}

// TestVerifyArchive_PeersTier_HonoursFromLastVerified: `-tier peers -from-last-verified` samples from effectiveFrom, not the raw -from.
func TestVerifyArchive_PeersTier_HonoursFromLastVerified(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(wd, "..", "..", "..", "configs", "example.toml")
	if _, statErr := os.Stat(cfgPath); statErr != nil {
		t.Skipf("example.toml not at %s: %v", cfgPath, statErr)
	}

	var mu sync.Mutex
	var seenSeqs []uint32
	newPeer := func() string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			base := filepath.Base(r.URL.Path) // history-XXXXXXXX.json
			hex := strings.TrimSuffix(strings.TrimPrefix(base, "history-"), ".json")
			seq64, parseErr := strconv.ParseUint(hex, 16, 32)
			if parseErr != nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			mu.Lock()
			seenSeqs = append(seenSeqs, uint32(seq64))
			mu.Unlock()
			_, _ = fmt.Fprintf(w, `{"currentLedger":%d,"currentBuckets":[]}`, seq64)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	peerA, peerB := newPeer(), newPeer()
	archiveRoot := t.TempDir()
	writeSelfCheckpointJSON(t, archiveRoot, 60_000_063, `{"currentLedger":60000063,"currentBuckets":[]}`)
	// Tier D samples the last checkpoint in range even at -peer-samples 1.
	writeSelfCheckpointJSON(t, archiveRoot, 60_000_191, `{"currentLedger":60000191,"currentBuckets":[]}`)

	// Prior state: Tier D already verified up to ledger 60,000,000.
	// -from-last-verified with -safety-overlap 0 must therefore start
	// sampling at 60,000,001, nowhere near genesis (checkpoint 63).
	stateFile := filepath.Join(t.TempDir(), "state.json")
	priorState := VerifyArchiveState{
		Tiers: map[string]VerifyArchiveTierState{
			"peers": {LastVerifiedLedger: 60_000_000, LastVerifiedAt: time.Now()},
		},
	}
	if err := writeVerifyArchiveState(stateFile, priorState); err != nil {
		t.Fatalf("writeVerifyArchiveState: %v", err)
	}

	args := []string{
		"-config", cfgPath,
		"-bucket", "test-bucket",
		"-tier", "peers",
		"-from-last-verified",
		"-state-file", stateFile,
		"-safety-overlap", "0",
		"-peers", peerA + "," + peerB,
		"-archive-root", archiveRoot,
		"-peer-samples", "1",
		"-to", "60000200",
	}
	if err := verifyArchive(args); err != nil {
		t.Fatalf("verifyArchive: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seenSeqs) == 0 {
		t.Fatal("no peer was queried")
	}
	for _, seq := range seenSeqs {
		if seq <= 60_000_000 {
			t.Errorf("Tier D queried checkpoint %d — -from-last-verified was ignored "+
				"(raw -from=%d/checkpoint 63 would have been sampled instead of the "+
				"incremental watermark)", seq, 2)
		}
	}
}

type fakeSampleLister map[string]string // key -> etag

func (f fakeSampleLister) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	out := &s3.ListObjectsV2Output{}
	for k, etag := range f {
		if strings.HasPrefix(k, aws.ToString(in.Prefix)) {
			out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k), ETag: aws.String(`"` + etag + `"`), Size: aws.Int64(10)})
		}
	}
	return out, nil
}

func (f fakeSampleLister) GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return nil, errors.New("the SDF sample compares listings only")
}

func TestVerifySDFSample(t *testing.T) {
	t.Parallel()
	k := func(seq uint32) string { return sdfSampleSchema.GetObjectKeyFromSequenceNumber(seq) }
	local := mirrorStore{api: fakeSampleLister{k(10): "aa", k(11): "bb", k(12): "cc"}}
	upstream := mirrorStore{api: fakeSampleLister{k(10): "aa", k(11): "XX", k(12): "cc", k(13): "dd"}}
	// 10 matches, 11 differs, 13 is missing locally, 14 is absent upstream.
	res, err := verifySDFSample(context.Background(), local, upstream, []uint32{10, 11, 13, 14})
	if err != nil {
		t.Fatal(err)
	}
	if res.sampled != 3 || res.matched != 1 || res.missingUpstream != 1 || len(res.findings) != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
	causes := res.findings[0].cause + "," + res.findings[1].cause
	if causes != mirrorCauseLocalDiffers+",missing-local" {
		t.Fatalf("causes = %s", causes)
	}
}

func TestPickSampleLedgers(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	got := pickSampleLedgers(100, 200, 20, rng)
	if len(got) != 20 {
		t.Fatalf("len = %d", len(got))
	}
	for i, l := range got {
		if l < 100 || l > 200 || (i > 0 && l <= got[i-1]) {
			t.Fatalf("bad sample %v", got)
		}
	}
	if all := pickSampleLedgers(5, 7, 10, rng); len(all) != 3 {
		t.Fatalf("small range = %v", all)
	}
}

// tierDCheckpointJSON renders a history-XXXXXXXX.json whose single
// bucket hash is curr.
func tierDCheckpointJSON(seq uint32, curr string) string {
	return fmt.Sprintf(`{"currentLedger":%d,"currentBuckets":[{"curr":%q,"snap":"00","next":{"state":0}}]}`, seq, curr)
}

// newTierDPeer serves every history/…/history-<hex>.json with bucket
// hash curr, like a canonical tier-1 archive.
func newTierDPeer(t *testing.T, curr string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Path)
		hexSeq, ok := strings.CutPrefix(strings.TrimSuffix(name, ".json"), "history-")
		seq, err := strconv.ParseUint(hexSeq, 16, 32)
		if !ok || err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprint(w, tierDCheckpointJSON(uint32(seq), curr))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// writeSelfCheckpoint writes <root>/history/XX/YY/ZZ/history-<hex>.json.
func writeSelfCheckpoint(t *testing.T, root string, seq uint32, curr string) {
	t.Helper()
	writeSelfCheckpointJSON(t, root, seq, tierDCheckpointJSON(seq, curr))
}

func writeSelfCheckpointJSON(t *testing.T, root string, seq uint32, body string) {
	t.Helper()
	hexSeq := fmt.Sprintf("%08x", seq)
	dir := filepath.Join(root, "history", hexSeq[0:2], hexSeq[2:4], hexSeq[4:6])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "history-"+hexSeq+".json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write self checkpoint: %v", err)
	}
}

// TestVerifyArchivePeers_SelfIsAParticipant: Tier D compares OUR archive with the peers' consensus, so agreeing peers must not pass a run whose local bytes diverge or are absent (range [64,191] samples 127 and 191).
func TestVerifyArchivePeers_SelfIsAParticipant(t *testing.T) {
	t.Parallel()
	peers := strings.Join([]string{newTierDPeer(t, "aa"), newTierDPeer(t, "aa"), newTierDPeer(t, "aa")}, ",")

	cases := []struct {
		name         string
		setup        func(t *testing.T, root string)
		failOnMissed bool
		wantErr      string // empty → run must pass
	}{
		{
			name: "self matches consensus",
			setup: func(t *testing.T, root string) {
				writeSelfCheckpoint(t, root, 127, "aa")
				writeSelfCheckpoint(t, root, 191, "aa")
			},
			failOnMissed: true,
		},
		{
			name: "self diverges while peers agree",
			setup: func(t *testing.T, root string) {
				writeSelfCheckpoint(t, root, 127, "aa")
				writeSelfCheckpoint(t, root, 191, "bb")
			},
			failOnMissed: true,
			wantErr:      "diverges from peer consensus at 1 checkpoint",
		},
		{
			name:         "self absent entirely",
			setup:        func(*testing.T, string) {},
			failOnMissed: false,
			wantErr:      "matched no consensus-verified checkpoint",
		},
		{
			name: "self checkpoint above mirror high-water is fill lag",
			setup: func(t *testing.T, root string) {
				writeMirrorCheckpoint(t, root, 127)
				writeSelfCheckpoint(t, root, 127, "aa")
			},
			failOnMissed: true,
		},
		{
			name: "self checkpoint missing inside mirror coverage",
			setup: func(t *testing.T, root string) {
				writeMirrorCheckpoint(t, root, 127)
				writeMirrorCheckpoint(t, root, 191)
				writeSelfCheckpoint(t, root, 191, "aa")
			},
			failOnMissed: true,
			wantErr:      "missing from our archive",
		},
		{
			name: "self checkpoint missing inside mirror coverage tolerated without -fail-on-missed",
			setup: func(t *testing.T, root string) {
				writeMirrorCheckpoint(t, root, 127)
				writeMirrorCheckpoint(t, root, 191)
				writeSelfCheckpoint(t, root, 191, "aa")
			},
			failOnMissed: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			tc.setup(t, root)
			err := verifyArchivePeers(64, 191, peers, 2, root, tc.failOnMissed)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("verifyArchivePeers: %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("verifyArchivePeers err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestVerifyArchivePeers_RejectsNonHTTPPeer: a file:// peer is refused up front, not skipped after failing every fetch.
func TestVerifyArchivePeers_RejectsNonHTTPPeer(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSelfCheckpoint(t, root, 127, "aa")
	writeSelfCheckpoint(t, root, 191, "aa")
	peers := strings.Join([]string{newTierDPeer(t, "aa"), newTierDPeer(t, "aa"), "file:///srv/history-archive"}, ",")
	err := verifyArchivePeers(64, 191, peers, 2, root, true)
	if err == nil || !strings.Contains(err.Error(), "not an http(s) archive URL") {
		t.Fatalf("verifyArchivePeers err = %v, want a rejection of the file:// peer", err)
	}
}
