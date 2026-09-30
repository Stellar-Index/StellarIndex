package archive

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

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

// TestVerifyArchivePeers_SelfIsAParticipant pins that Tier D compares
// OUR archive with the peers' consensus: canonical peers agreeing with
// each other must not pass a run whose local bytes diverge or are absent.
// The range [64,191] samples checkpoints 127 and 191.
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

// TestVerifyArchivePeers_RejectsNonHTTPPeer pins that a file:// peer is
// refused up front instead of failing every fetch and being skipped.
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
