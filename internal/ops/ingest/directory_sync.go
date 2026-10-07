package ingest

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// directory-sync mirrors the MIT-licensed
// github.com/stellar-expert/public-directory account labels into the
// `account_directory` table (migration 0136): one HTTPS GET of the
// repo tarball, parse accounts/*.json, upsert the full set, prune
// rows upstream removed (~18.5k entries when last measured).
//
// Run from a daily timer:
//
//	stellarindex-ops directory-sync -config /etc/stellarindex.toml
//
// The labels carry third-party attribution and are NOT display-only: a
// scam-class tag withholds the issuer's price (pricingguard.ScamGate)
// and a recognition tag admits it to the RWA surface. They never feed
// the currency catalogue. A false positive is corrected durably with
// `directory-override`, whose rows this sync never touches.
// A truncated, corrupt or oversized tarball fails the run
// (parseDirectoryTarball), `ReplaceDirectory` refuses an empty parse
// (a broken fetch must not prune the table), and per-file JSON errors
// are counted + reported, failing the run only if EVERYTHING failed.
// `-sha256` with a commit-pinned `-url` refuses any tarball but the
// reviewed one, and a committed run's prune / flag / un-flag counts
// are exported as stellarindex_directory_sync_rows_changed{kind}.
const (
	directorySource     = "stellar-expert"
	directoryDefaultURL = "https://github.com/stellar-expert/public-directory/archive/refs/heads/master.tar.gz"

	// directoryMaxTarballBytes bounds both the compressed and the
	// decompressed read. The tarball is ~4 MB compressed today; 256 MB
	// of headroom is an order-of-magnitude guard against a
	// decompression bomb from a compromised -url, not a tight fit. A
	// stream that reaches the bound is REFUSED, never truncated: see
	// boundedReader.
	directoryMaxTarballBytes = 256 << 20
)

var errDirectoryTarballTooLarge = errors.New("directory-sync: tarball reached the size bound")

// boundedReader is io.LimitReader with the bound made fatal. A plain
// LimitReader reports a clean EOF at the bound, and 256 MiB is an
// exact multiple of tar's 512-byte block, so a stream cut there
// between two entries handed tar.Reader a well-formed end of archive:
// a partial, non-empty snapshot that ReplaceDirectory would then
// accept and prune the table down to.
type boundedReader struct {
	r    io.Reader
	left int64
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errDirectoryTarballTooLarge
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

// directoryAddressRe matches the strkey forms the table's CHECK
// accepts. Entries outside it (malformed upstream rows) are skipped
// and counted rather than failing the whole chunk's INSERT.
var directoryAddressRe = regexp.MustCompile(`^[GC][A-Z2-7]{55}$`)

type directoryFileEntry struct {
	Address string   `json:"address"`
	Name    string   `json:"name"`
	Domain  string   `json:"domain"`
	Tags    []string `json:"tags"`
}

func directorySync(args []string) error {
	fs := flag.NewFlagSet("directory-sync", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	url := fs.String("url", directoryDefaultURL, "Tarball URL of the public-directory repo (https only)")
	timeout := fs.Duration("timeout", 5*time.Minute, "Wall-clock timeout for the whole run")
	acceptChurn := fs.Bool("accept-churn", false, "Accept a snapshot beyond the churn ceiling (prunes, newly scam-flags or un-flags more rows than one day plausibly does) — only for a known upstream mass change")
	pinSHA256 := fs.String("sha256", "", "Hex SHA-256 the fetched tarball must match; set it with -url pinned to a commit archive so only reviewed upstream content is synced")
	textfile := fs.String("textfile", "", "node_exporter textfile for a committed sync's per-kind change counts. Empty = "+opsutil.DefaultTextfileDir+"/"+directoryTextfileName+" when that directory exists (r1), otherwise none")
	heartbeat := fs.String("heartbeat", "", "node_exporter textfile path for the run-outcome/progress gauges. Empty = "+opsutil.DefaultTextfileDir+"/ops_job_directory_sync.prom when that directory exists (r1), otherwise no heartbeat at all")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	if err := checkDirectorySourceFlags(*url, *pinSHA256); err != nil {
		return err
	}
	gate.Banner()
	dryRun := gate.DryRun()

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	entries, skipped, err := fetchDirectoryTarball(ctx, *url, *pinSHA256)
	if err != nil {
		return err
	}
	fmt.Printf("Parsed %d directory entries (%d skipped: bad address/JSON).\n", len(entries), skipped)

	if dryRun {
		fmt.Println("Dry run — nothing written.")
		return nil
	}

	// Without a heartbeat, the only signal a stalled/failed sync has is the generic
	// stellarindex_systemd_unit_failed catch-all, which covers nothing
	// but a nonzero process exit and takes 15m+ to ticket. Wiring the
	// same JobHeartbeat every other stellarindex-ops job uses gives
	// directory-sync a dedicated, near-immediate
	// stellarindex_ops_job_run_failed (deploy/monitoring/rules/ingestion.yml)
	// plus an entries-synced gauge (progress_total), for free, off the
	// existing alert rules — no bespoke metric or rule needed. Started
	// before store.Open so a Postgres connect/ping failure is captured
	// too, not just a failed write.
	hb := opsutil.NewJobHeartbeat("directory-sync", *heartbeat, nil)
	if hb.Enabled() {
		fmt.Printf("directory-sync: heartbeat -> %s\n", hb.Path())
	}
	hb.Start()
	exitOK := false
	defer func() { hb.Stop(exitOK) }()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	limit := timescale.DefaultDirectoryChurnLimit
	if *acceptChurn {
		limit = timescale.DirectoryChurnUnbounded
	}
	res, err := store.ReplaceDirectoryWithin(ctx, directorySource, entries, limit)
	if errors.Is(err, timescale.ErrDirectoryChurnExceeded) {
		// Nonzero exit fails the systemd unit, which
		// stellarindex_systemd_unit_failed tickets; the numbers land in
		// journald beside this hint. hb.Stop(exitOK) below also flips
		// stellarindex_ops_job_last_exit_ok, which tickets far sooner.
		return fmt.Errorf("%w — nothing written; if upstream really changed this much, re-run with -accept-churn", err)
	}
	if err != nil {
		return err
	}
	hb.Progress(uint64(res.Upserted+res.Existing), 0) //nolint:gosec // non-negative row counts
	exitOK = true
	stampDirectorySyncTextfile(directoryTextfilePath(*textfile), res)
	fmt.Printf("Synced: %d upserted, %d pruned, %d newly scam-flagged, %d un-flagged, %d held before, %d shadowed by another owner (source=%s).\n",
		res.Upserted, res.Pruned, res.NewlyFlagged, res.Unflagged, res.Existing, res.Shadowed, directorySource)
	return nil
}

// directoryFetchTimeout bounds the directory tarball download. Generous
// because the tarball is a few MB over the public internet, but finite:
// an unbounded fetch is a hang, not a slow success.
const directoryFetchTimeout = 2 * time.Minute

// fetchDirectoryTarball downloads the repo tarball and parses every
// accounts/*.json blob into DirectoryEntry rows.
func fetchDirectoryTarball(ctx context.Context, url, wantSHA256 string) (entries []timescale.DirectoryEntry, skipped int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	// http.DefaultClient has NO timeout. A tarball fetch against a
	// third-party host that accepts the connection and then stops sending
	// would hang this command forever, and an operator command that
	// never returns looks like a slow network, not a bug.
	//
	// ctx still bounds it when the caller supplies a deadline; this makes
	// the bound unconditional.
	client := &http.Client{Timeout: directoryFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("directory-sync: fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("directory-sync: fetch: HTTP %d from %s", resp.StatusCode, url)
	}

	return parsePinnedDirectoryTarball(resp.Body, directoryMaxTarballBytes, wantSHA256)
}

var directorySHA256Re = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// checkDirectorySourceFlags validates -url and -sha256 before anything
// is fetched.
func checkDirectorySourceFlags(url, wantSHA256 string) error {
	if !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("-url must be https:// (got %q)", url)
	}
	if wantSHA256 != "" && !directorySHA256Re.MatchString(wantSHA256) {
		return fmt.Errorf("-sha256 must be 64 hex characters (got %q)", wantSHA256)
	}
	return nil
}

// parsePinnedDirectoryTarball is parseDirectoryTarball plus, when
// wantSHA256 is set, a SHA-256 check over every compressed byte: any
// tarball other than the pinned one is refused before an entry is
// returned. gzip can stop short of trailing bytes, so the rest of the
// stream is hashed too, under the same size bound.
func parsePinnedDirectoryTarball(body io.Reader, maxBytes int64, wantSHA256 string) (entries []timescale.DirectoryEntry, skipped int, err error) {
	if wantSHA256 == "" {
		return parseDirectoryTarball(body, maxBytes)
	}
	h := sha256.New()
	tee := io.TeeReader(body, h)
	if entries, skipped, err = parseDirectoryTarball(tee, maxBytes); err != nil {
		return nil, 0, err
	}
	if _, err = io.Copy(io.Discard, &boundedReader{r: tee, left: maxBytes}); err != nil {
		return nil, 0, fmt.Errorf("directory-sync: tarball digest: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, wantSHA256) {
		return nil, 0, fmt.Errorf("directory-sync: tarball sha256 %s does not match the pinned -sha256 %s — refusing", got, strings.ToLower(wantSHA256))
	}
	return entries, skipped, nil
}

// parseDirectoryTarball walks a gzip tarball of the repo, bounding
// both the compressed and decompressed streams at maxBytes, and
// verifies the gzip trailer before returning: a truncated, corrupt or
// oversized snapshot is an error, never a shorter entry set.
func parseDirectoryTarball(body io.Reader, maxBytes int64) (entries []timescale.DirectoryEntry, skipped int, err error) {
	gz, err := gzip.NewReader(&boundedReader{r: body, left: maxBytes})
	if err != nil {
		return nil, 0, fmt.Errorf("directory-sync: gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	plain := &boundedReader{r: gz, left: maxBytes}
	tr := tar.NewReader(plain)
	for {
		hdr, herr := tr.Next()
		if errors.Is(herr, io.EOF) {
			break
		}
		if herr != nil {
			return nil, 0, fmt.Errorf("directory-sync: tar: %w", herr)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// Tarball paths are <repo>-<branch>/accounts/<ADDRESS>.json.
		dir, file := path.Split(hdr.Name)
		if !strings.HasSuffix(dir, "/accounts/") || !strings.HasSuffix(file, ".json") {
			continue
		}
		e, ok := parseDirectoryFile(tr)
		if !ok {
			skipped++
			continue
		}
		entries = append(entries, e)
	}
	// tar.Reader stops at the end-of-archive marker without reading
	// the gzip trailer, so a CRC or length mismatch (and a stream cut
	// inside the trailer) went unverified. Draining to EOF makes gzip
	// check both; it is the only integrity signal an unpinned branch
	// tarball carries.
	if _, err := io.Copy(io.Discard, plain); err != nil {
		return nil, 0, fmt.Errorf("directory-sync: tarball integrity: %w", err)
	}
	if len(entries) == 0 {
		return nil, skipped, fmt.Errorf("directory-sync: parsed 0 entries (%d skipped) — refusing; upstream layout changed or fetch was truncated", skipped)
	}
	return entries, skipped, nil
}

// directoryTextfileName is the node_exporter textfile a committed sync's
// change counts land in, beside its ops_job heartbeat.
const directoryTextfileName = "directory_sync.prom"

// directoryTextfilePath resolves -textfile: an explicit path wins; empty
// means the default textfile directory when it exists, otherwise none.
func directoryTextfilePath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if st, err := os.Stat(opsutil.DefaultTextfileDir); err == nil && st.IsDir() {
		return filepath.Join(opsutil.DefaultTextfileDir, directoryTextfileName)
	}
	return ""
}

// stampDirectorySyncTextfile records what a committed sync changed; a
// failure to stamp is a warning, never the run's outcome.
func stampDirectorySyncTextfile(path string, res timescale.DirectorySyncResult) {
	if path == "" {
		return
	}
	if err := writeTextfileAtomic(path, renderDirectorySyncTextfile(directorySource, res)); err != nil {
		fmt.Fprintf(os.Stderr, "directory-sync: WARN textfile: %v\n", err)
	}
}

// renderDirectorySyncTextfile renders stellarindex_directory_sync_rows_changed.
// An un-flag re-publishes a withheld price, so it is exported for
// alerting rather than left in journald.
func renderDirectorySyncTextfile(source string, res timescale.DirectorySyncResult) string {
	var b strings.Builder
	b.WriteString("# HELP stellarindex_directory_sync_rows_changed Rows the most recent committed directory-sync changed, by kind: pruned, newly_flagged (scam-class tag added: price withheld) and unflagged (scam-class tag cleared or row pruned: price no longer withheld).\n# TYPE stellarindex_directory_sync_rows_changed gauge\n")
	for _, k := range []struct {
		kind string
		n    int64
	}{{"pruned", res.Pruned}, {"newly_flagged", res.NewlyFlagged}, {"unflagged", res.Unflagged}} {
		fmt.Fprintf(&b, "stellarindex_directory_sync_rows_changed{source=%q,kind=%q} %d\n", source, k.kind, k.n)
	}
	return b.String()
}

// parseDirectoryFile decodes one accounts/<ADDRESS>.json blob,
// returning ok=false for malformed JSON, a bad address, or an empty
// name (a nameless label is useless downstream).
func parseDirectoryFile(r io.Reader) (timescale.DirectoryEntry, bool) {
	var f directoryFileEntry
	if err := json.NewDecoder(r).Decode(&f); err != nil {
		return timescale.DirectoryEntry{}, false
	}
	if !directoryAddressRe.MatchString(f.Address) || f.Name == "" {
		return timescale.DirectoryEntry{}, false
	}
	tags := f.Tags
	if tags == nil {
		tags = []string{}
	}
	return timescale.DirectoryEntry{
		Address: f.Address,
		Name:    f.Name,
		Domain:  f.Domain,
		Tags:    tags,
		Source:  directorySource,
	}, true
}
