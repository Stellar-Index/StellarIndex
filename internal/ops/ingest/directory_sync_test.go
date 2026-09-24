package ingest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildDirectoryTarball assembles a gzip tarball mimicking the GitHub
// archive layout: <repo>-master/accounts/<ADDRESS>.json plus noise
// files the walker must skip.
func buildDirectoryTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const (
	testDirAddrG = "GDUY7J7A33TQWOSOQGDO776GGLM3UQERL4J3SPT56F6YS4ID7MLDERI4"
	testDirAddrC = "CA242XKXANKC46P53M355OPYWMHWPPTKQM5T5DNMOBWJMHOWDLNPJTN4"
)

func TestFetchDirectoryTarball_ParsesAccountsAndSkipsNoise(t *testing.T) {
	tarball := buildDirectoryTarball(t, map[string]string{
		"public-directory-master/README.md": "# not an account",
		"public-directory-master/accounts/" + testDirAddrG + ".json": `{
			"address": "` + testDirAddrG + `",
			"name": "SDF Growth 3", "tags": ["sdf", "custodian"],
			"domain": "stellar.org", "version": 3}`,
		"public-directory-master/accounts/" + testDirAddrC + ".json": `{
			"address": "` + testDirAddrC + `",
			"name": "Aquarius Pool", "tags": ["defi"], "domain": "aqua.network"}`,
		// Malformed rows: bad address, nameless, invalid JSON — all
		// skipped + counted, never fatal.
		"public-directory-master/accounts/BADADDR.json":                  `{"address": "BADADDR", "name": "x"}`,
		"public-directory-master/accounts/" + testDirAddrG + "2.json":    `{"address": "` + testDirAddrG + `", "name": ""}`,
		"public-directory-master/accounts/" + testDirAddrC + "junk.json": `{not json`,
		"public-directory-master/domains/blocked.json":                   `["evil.example"]`,
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	entries, skipped, err := fetchDirectoryTarball(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetchDirectoryTarball: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2: %+v", len(entries), entries)
	}
	if skipped != 3 {
		t.Errorf("skipped = %d, want 3 (bad address, empty name, invalid JSON)", skipped)
	}
	byAddr := map[string]bool{}
	for _, e := range entries {
		byAddr[e.Address] = true
		if e.Source != "stellar-expert" {
			t.Errorf("entry %s source = %q, want stellar-expert", e.Address, e.Source)
		}
		if e.Tags == nil {
			t.Errorf("entry %s Tags is nil — must be non-nil for the Postgres array binding", e.Address)
		}
	}
	if !byAddr[testDirAddrG] || !byAddr[testDirAddrC] {
		t.Errorf("missing expected addresses in %v", byAddr)
	}
}

// TestFetchDirectoryTarball_RefusesEmptyParse — a tarball with no
// account files (layout change, truncated fetch) must error rather
// than hand ReplaceDirectory an empty set.
func TestFetchDirectoryTarball_RefusesEmptyParse(t *testing.T) {
	tarball := buildDirectoryTarball(t, map[string]string{
		"public-directory-master/README.md": "layout moved",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	_, _, err := fetchDirectoryTarball(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v, want a refusal on 0 parsed entries", err)
	}
}

func TestFetchDirectoryTarball_HTTPErrorIsFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if _, _, err := fetchDirectoryTarball(context.Background(), srv.URL); err == nil {
		t.Fatal("non-200 fetch returned nil error")
	}
}

// TestDirectorySync_FailsClosedByDefault pins the ops write-gate
// unification (W8.15c): directory-sync used to WRITE unless you passed
// -dry-run — the unsafe default-WRITE convention. After the flip it
// previews by DEFAULT and mutates Postgres only on an explicit -write,
// announced by a loud stderr banner. This asserts the CORRECTED default
// (dry run, no writes) and that -write is the opt-in — the exact reversal
// the automated systemd caller now depends on. It compiles against both
// the pre- and post-fix directorySync signature, so reverting the gate
// makes the default assertion fail (no DRY-RUN banner is printed): the
// non-vacuous red.
//
// The banner is emitted after flag validation and BEFORE config load /
// any network fetch, so a nonexistent config + never-resolving https URL
// let the run announce its mode and then error out without touching
// Postgres or the network.
func TestDirectorySync_FailsClosedByDefault(t *testing.T) {
	const cfg = "/nonexistent/stellarindex-directory-sync-gate-test.toml"
	const url = "https://directory.invalid/archive.tar.gz"

	// DEFAULT: no -write → fail-closed dry run.
	_, stderrDefault := runDirectorySyncCapturingStderr(t, []string{"-config", cfg, "-url", url})
	if !strings.Contains(stderrDefault, "DRY RUN — no writes; pass -write to apply") {
		t.Errorf("default run must announce the fail-closed DRY RUN banner on stderr; got:\n%s", stderrDefault)
	}
	if strings.Contains(stderrDefault, "WRITING — applying changes") {
		t.Errorf("default run must NOT announce WRITING — it would mutate Postgres without an explicit opt-in; got:\n%s", stderrDefault)
	}

	// -write: explicit opt-in → WRITING.
	_, stderrWrite := runDirectorySyncCapturingStderr(t, []string{"-config", cfg, "-url", url, "-write"})
	if !strings.Contains(stderrWrite, "WRITING — applying changes") {
		t.Errorf("-write must announce the WRITING banner on stderr; got:\n%s", stderrWrite)
	}
	if strings.Contains(stderrWrite, "DRY RUN") {
		t.Errorf("-write must NOT report DRY RUN; got:\n%s", stderrWrite)
	}
}

// TestDirectorySync_WiresAJobHeartbeat — RLT-317: directory-sync had no
// signal of its own, only the generic stellarindex_systemd_unit_failed
// catch-all (a 15m+, exit-code-only ticket). Wiring the same
// opsutil.JobHeartbeat every other stellarindex-ops job uses gives it
// stellarindex_ops_job_run_failed for free off the existing alert rules
// (deploy/monitoring/rules/ingestion.yml) — with no bespoke metric or
// rule to invent. This drives directorySync all the way to a failed
// Postgres ping (a closed local port refuses instantly) and asserts the
// heartbeat textfile records that failure: before the fix, no such
// textfile is ever written because directorySync does not accept a
// -heartbeat flag at all and flag.Parse fails outright.
func TestDirectorySync_WiresAJobHeartbeat(t *testing.T) {
	tarball := buildDirectoryTarballN(t, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()
	// fetchDirectoryTarball builds its own *http.Client but leaves
	// Transport nil, which falls back to http.DefaultTransport — swap in
	// the test server's (trusting its self-signed cert) for the
	// duration of this test.
	origTransport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = origTransport }()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "stellarindex.toml")
	// Port 1 on loopback refuses instantly (nothing listens, no privilege
	// needed to attempt connect), so the run fails fast at the ping
	// without needing a real Postgres.
	cfgBody := "[region]\nid = \"r2\"\nname = \"Ashburn\"\n\n[stellar]\nnetwork = \"pubnet\"\n\n[storage]\npostgres_dsn = \"postgres://u:p@127.0.0.1:1/db?sslmode=disable\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o644); err != nil {
		t.Fatal(err)
	}
	hbPath := filepath.Join(dir, "directory-sync.prom")

	err := directorySync([]string{"-config", cfgPath, "-url", srv.URL, "-write", "-heartbeat", hbPath})
	if err == nil {
		t.Fatal("expected the run to fail at the Postgres ping (port 1 refuses)")
	}
	t.Logf("directorySync error (expected, from the closed port): %v", err)

	body, rerr := os.ReadFile(hbPath)
	if rerr != nil {
		t.Fatalf("heartbeat textfile was never written: %v", rerr)
	}
	text := string(body)
	if !strings.Contains(text, `stellarindex_ops_job_running{ops_job="directory-sync"} 0`) {
		t.Errorf("heartbeat must record running=0 after the run ended; got:\n%s", text)
	}
	if !strings.Contains(text, `stellarindex_ops_job_last_exit_ok{ops_job="directory-sync"} 0`) {
		t.Errorf("heartbeat must record last_exit_ok=0 for a failed run; got:\n%s", text)
	}
}

// runDirectorySyncCapturingStderr runs directorySync with os.Stderr
// redirected to a pipe and returns the run error plus everything written
// to stderr (where the write-gate banner lands).
func runDirectorySyncCapturingStderr(t *testing.T, args []string) (error, string) {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	runErr := directorySync(args)
	_ = w.Close()
	os.Stderr = orig
	return runErr, <-done
}

// buildDirectoryTarballN renders n well-formed account files, each
// body under one tar block, so every entry occupies exactly 1024
// bytes (header + one data block) of the uncompressed archive.
func buildDirectoryTarballN(t *testing.T, n int) []byte {
	t.Helper()
	files := make(map[string]string, n)
	for i := range n {
		letter := string(rune('A' + i%26))
		addr := "G" + strings.Repeat(letter, 55)
		files["public-directory-master/accounts/"+addr+".json"] = `{"address": "` + addr + `", "name": "Entry ` + letter + `", "tags": ["exchange"]}`
	}
	return buildDirectoryTarball(t, files)
}

// TestParseDirectoryTarball_RefusesTruncationAtBlockBoundary — the
// size bound is an exact multiple of tar's 512-byte block. Cut there,
// between two entries, tar.Reader sees a clean end of archive and the
// walk used to return the entries read so far with a nil error: a
// partial snapshot that ReplaceDirectory then pruned the table down
// to. The bound must be a refusal, not a shorter result.
func TestParseDirectoryTarball_RefusesTruncationAtBlockBoundary(t *testing.T) {
	tarball := buildDirectoryTarballN(t, 3) // 3 × 1024 B + 1024 B end-of-archive, uncompressed

	entries, _, err := parseDirectoryTarball(bytes.NewReader(tarball), 2*1024)
	if err == nil {
		t.Fatalf("parse at a 2048 B bound returned %d entries and nil error; want a refusal, got a silently truncated snapshot", len(entries))
	}
	if !strings.Contains(err.Error(), "size bound") {
		t.Errorf("err = %v, want the size-bound refusal", err)
	}
	if entries != nil {
		t.Errorf("entries = %v, want none on a refused snapshot", entries)
	}
}

// TestParseDirectoryTarball_RejectsCorruptGzipTrailer — tar.Reader
// stops at the end-of-archive marker, before the gzip trailer, so the
// CRC-32 the archive carries was never checked. A flipped CRC byte
// must fail the run.
func TestParseDirectoryTarball_RejectsCorruptGzipTrailer(t *testing.T) {
	tarball := buildDirectoryTarballN(t, 3)
	// gzip trailer: 4-byte CRC-32 then 4-byte ISIZE, last 8 bytes.
	tarball[len(tarball)-8] ^= 0xFF

	entries, _, err := parseDirectoryTarball(bytes.NewReader(tarball), directoryMaxTarballBytes)
	if err == nil {
		t.Fatalf("parse of a tarball with a corrupt CRC returned %d entries and nil error; want an integrity refusal", len(entries))
	}
	if !strings.Contains(err.Error(), "integrity") {
		t.Errorf("err = %v, want the tarball-integrity refusal", err)
	}
}

// TestParseDirectoryTarball_AcceptsIntactArchiveWithinTheBound — the
// integrity checks must not reject a well-formed archive that fits.
func TestParseDirectoryTarball_AcceptsIntactArchiveWithinTheBound(t *testing.T) {
	tarball := buildDirectoryTarballN(t, 3)
	entries, skipped, err := parseDirectoryTarball(bytes.NewReader(tarball), 8*1024)
	if err != nil {
		t.Fatalf("parseDirectoryTarball: %v", err)
	}
	if len(entries) != 3 || skipped != 0 {
		t.Errorf("entries=%d skipped=%d, want 3/0", len(entries), skipped)
	}
}
