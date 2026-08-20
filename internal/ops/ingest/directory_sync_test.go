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
			t.Errorf("entry %s Tags is nil — must be non-nil for pq.Array", e.Address)
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
