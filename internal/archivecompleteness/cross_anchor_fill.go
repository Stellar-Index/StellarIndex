package archivecompleteness

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"
)

// Source is one upstream URL from which a missing checkpoint file is fetched at `<URL>/ledger/XX/YY/ZZ/ledger-XXYYZZWW.xdr.gz`.
type Source struct {
	// Name is a stable label used in metrics + logs (e.g. the
	// archive host name). Stays in lockstep with the
	// archive_completeness_repair_attempts_total metric label.
	Name string

	// URL is the source's archive root. Trailing slashes are
	// stripped; we append the standard
	// `ledger/XX/YY/ZZ/ledger-XXYYZZWW.xdr.gz` path under it.
	URL string
}

// DefaultCrossAnchorSources returns the fallback chain: SDF's three primary archives, then the tier-1 validator archives the captive-core config already trusts.
func DefaultCrossAnchorSources() []Source {
	return []Source{
		{Name: "sdf-core-live-001", URL: "https://history.stellar.org/prd/core-live/core_live_001"},
		{Name: "sdf-core-live-002", URL: "https://history.stellar.org/prd/core-live/core_live_002"},
		{Name: "sdf-core-live-003", URL: "https://history.stellar.org/prd/core-live/core_live_003"},
		{Name: "publicnode-bootes", URL: "https://bootes-history.publicnode.org"},
		{Name: "publicnode-lyra", URL: "https://lyra-history.publicnode.org"},
		{Name: "publicnode-hercules", URL: "https://hercules-history.publicnode.org"},
		{Name: "lobstr-v1", URL: "https://archive.v1.stellar.lobstr.co"},
		{Name: "lobstr-v2", URL: "https://archive.v2.stellar.lobstr.co"},
		{Name: "lobstr-v5", URL: "https://archive.v5.stellar.lobstr.co"},
	}
}

// CrossAnchorFiller fetches missing checkpoint files from the multi-source fallback chain into the local archive; Fill spawns Workers goroutines itself, so concurrent Fill calls are safe but each gets a full pool.
type CrossAnchorFiller struct {
	archiveRoot  string
	sources      []Source
	httpClient   *http.Client
	workers      int
	ownerUID     int
	ownerGID     int
	chownEnabled bool
}

// FillerOptions configures a [CrossAnchorFiller] at construction.
type FillerOptions struct {
	// ArchiveRoot is the SDF-layout archive root we write into.
	// Same path the cross-anchor checker scans (typically
	// `/srv/history-archive`).
	ArchiveRoot string

	// Sources is the ordered fallback chain. Empty slice falls
	// back to [DefaultCrossAnchorSources].
	Sources []Source

	// Network is the Stellar network this archive belongs to; empty means pubnet. A non-pubnet
	// value with empty Sources makes NewCrossAnchorFiller refuse: [DefaultCrossAnchorSources] are
	// all pubnet archives, so filling a test-net store from them would silently write pubnet
	// checkpoints into it. Test nets self-heal from their own galexie/captive-core.
	Network string

	// HTTPClient is the transport for source fetches; nil falls back to a 30s per-request
	// timeout client.
	HTTPClient *http.Client

	// Workers controls fetch parallelism; 0 falls back to 8, which avoided Cloudflare
	// rate-limiting in practice.
	Workers int

	// OwnerUser / OwnerGroup own each placed file; empty disables chown. Production uses
	// stellar:stellar so stellar-archivist reads the right ownership.
	OwnerUser  string
	OwnerGroup string
}

// NewCrossAnchorFiller constructs a Filler; it errors when chown is requested but the user/group lookup fails (operator config issue, fail loud at startup).
func NewCrossAnchorFiller(opts FillerOptions) (*CrossAnchorFiller, error) {
	if opts.ArchiveRoot == "" {
		return nil, errors.New("archivecompleteness: ArchiveRoot is required")
	}
	st, err := os.Stat(opts.ArchiveRoot)
	if err != nil {
		return nil, fmt.Errorf("archivecompleteness: stat ArchiveRoot %q: %w", opts.ArchiveRoot, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("archivecompleteness: ArchiveRoot %q is not a directory", opts.ArchiveRoot)
	}

	sources := opts.Sources
	if len(sources) == 0 {
		if opts.Network != "" && opts.Network != "pubnet" {
			return nil, fmt.Errorf(
				"archivecompleteness: refusing to cross-anchor-fill a %q archive from the built-in PUBNET sources — "+
					"filling from pubnet archives would write pubnet checkpoints into a test-net store (corruption). "+
					"Test nets self-heal archive gaps from their own galexie/captive-core; pass explicit Sources only if "+
					"you truly have test-net cross-anchor mirrors", opts.Network)
		}
		sources = DefaultCrossAnchorSources()
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = 8
	}

	f := &CrossAnchorFiller{
		archiveRoot: opts.ArchiveRoot,
		sources:     sources,
		httpClient:  httpClient,
		workers:     workers,
		// os.Chown treats -1 as "leave this half unchanged"; start
		// both here so a half-blank OwnerUser/OwnerGroup override
		// can't fall back to Go's int zero-value (uid/gid 0 = root).
		ownerUID: -1,
		ownerGID: -1,
	}

	if opts.OwnerUser != "" {
		u, err := user.Lookup(opts.OwnerUser)
		if err != nil {
			return nil, fmt.Errorf("archivecompleteness: lookup user %q: %w", opts.OwnerUser, err)
		}
		uid, err := strconv.Atoi(u.Uid)
		if err != nil {
			return nil, fmt.Errorf("archivecompleteness: parse uid for %q: %w", opts.OwnerUser, err)
		}
		f.ownerUID = uid
		f.chownEnabled = true
	}
	if opts.OwnerGroup != "" {
		g, err := user.LookupGroup(opts.OwnerGroup)
		if err != nil {
			return nil, fmt.Errorf("archivecompleteness: lookup group %q: %w", opts.OwnerGroup, err)
		}
		gid, err := strconv.Atoi(g.Gid)
		if err != nil {
			return nil, fmt.Errorf("archivecompleteness: parse gid for %q: %w", opts.OwnerGroup, err)
		}
		f.ownerGID = gid
		f.chownEnabled = true
	}
	return f, nil
}

// FillResult summarises the outcome of [CrossAnchorFiller.Fill].
type FillResult struct {
	// Filled is the count of checkpoints successfully placed.
	Filled int

	// Failed lists the checkpoint sequences that exhausted every
	// source without a successful fetch. Caller should log these
	// at WARN level and either retry later or escalate.
	Failed []FillFailure

	// PerSourceSuccess counts successful fetches by source name.
	// Helps operators see which upstream is healthy vs degraded.
	PerSourceSuccess map[string]int

	// PerSourceAttempt counts every try by source name, the winning fetch plus each source that
	// failed before it. It is the denominator of the per-source repair failure ratio
	// (`archive_completeness_repair_attempts_total`); PerSourceSuccess cannot be, because a
	// source that always fails would never appear in it.
	PerSourceAttempt map[string]int

	// PerSourceFailure counts failed tries by source name, the numerator of that ratio. Without
	// real per-source labels, failures would land under a synthetic `multi-source-exhausted`
	// label that never meets the attempt labels, leaving the
	// `stellarindex_archive_repair_source_degraded` alert unfireable.
	PerSourceFailure map[string]int
}

// FillFailure describes one missing checkpoint the filler could not recover.
type FillFailure struct {
	Seq    uint32
	Reason string
}

// Fill places each missing checkpoint via the shuffled source chain (first 200 with a valid non-empty gzip body wins, placed atomically via `<final>.new` then rename); ctx cancellation keeps placed files and stops new fetches.
func (f *CrossAnchorFiller) Fill(ctx context.Context, missing []uint32) FillResult {
	acc := newFillAccumulator()
	if len(missing) == 0 {
		return acc.result()
	}

	type job struct{ seq uint32 }
	jobs := make(chan job, f.workers)

	var wg sync.WaitGroup
	for i := 0; i < f.workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID))) //nolint:gosec // shuffling for load-spread; not security-relevant
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				tries, err := f.fetchOne(ctx, j.seq, rng)
				acc.record(j.seq, tries, err)
			}
		}(i)
	}

	for _, seq := range missing {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return acc.result()
		case jobs <- job{seq: seq}:
		}
	}
	close(jobs)
	wg.Wait()

	return acc.result()
}

// fillAccumulator collects per-checkpoint worker outcomes into the
// shared [FillResult] under one mutex. Extracted from Fill so the
// aggregation rules live in one readable place rather than inline in
// the worker goroutine.
type fillAccumulator struct {
	mu         sync.Mutex
	filled     int
	failures   []FillFailure
	perSource  map[string]int
	perAttempt map[string]int
	perFailure map[string]int
}

func newFillAccumulator() *fillAccumulator {
	return &fillAccumulator{
		perSource:  map[string]int{},
		perAttempt: map[string]int{},
		perFailure: map[string]int{},
	}
}

// record folds one fetchOne walk into the totals. Every source the walk touched counts as an
// attempt under its real name, including ones that failed before a later source succeeded;
// otherwise an always-failing source would never enter the failure-ratio denominator.
func (a *fillAccumulator) record(seq uint32, tries fetchTries, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, name := range tries.attempted {
		a.perAttempt[name]++
	}
	for _, name := range tries.failed {
		a.perFailure[name]++
	}
	if err != nil {
		a.failures = append(a.failures, FillFailure{Seq: seq, Reason: err.Error()})
		return
	}
	a.filled++
	a.perSource[tries.winner]++
}

func (a *fillAccumulator) result() FillResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	return FillResult{
		Filled:           a.filled,
		Failed:           a.failures,
		PerSourceSuccess: a.perSource,
		PerSourceAttempt: a.perAttempt,
		PerSourceFailure: a.perFailure,
	}
}

// fetchTries records which sources one [CrossAnchorFiller.fetchOne]
// walk actually touched. attempted lists every source a GET was
// issued against (in walk order), failed the subset that errored,
// and winner the source that placed the file ("" when none did).
type fetchTries struct {
	winner    string
	attempted []string
	failed    []string
}

// fetchOne fetches one checkpoint via the shuffled fallback chain.
// Returns the per-source try record — populated even on the error
// paths, because a source that fails every time must still show up
// in the attempt counter — plus an error describing the last failure.
func (f *CrossAnchorFiller) fetchOne(ctx context.Context, seq uint32, rng *rand.Rand) (fetchTries, error) {
	var tries fetchTries

	hex := hexSeq8(seq)
	relPath := fmt.Sprintf("ledger/%s/%s/%s/ledger-%s.xdr.gz", hex[0:2], hex[2:4], hex[4:6], hex)
	relDir := filepath.Dir(relPath)

	// Every write goes through an os.Root on the archive root. The tree belongs to the archive's own
	// user while this runs as root: a path joined by hand follows whatever link that user planted
	// (a "<file>.new" link, or a prefix directory swapped for one) and root then truncates and
	// chowns the target. The root refuses any step that leaves the archive.
	root, err := os.OpenRoot(f.archiveRoot)
	if err != nil {
		return tries, fmt.Errorf("open archive root: %w", err)
	}
	defer func() { _ = root.Close() }()

	// Create the parent directory BEFORE the first GET so a bad destination fails fast.
	//
	// 0755, not 0750: the daemon runs as root but the verifier runs as a non-root service user, and
	// a 0750 root:root parent makes every placed file unreadable to it (the chown never covers the
	// parent), so the ADR-0017 anchor check fails with EACCES, which it treats as a hard mismatch.
	if err := root.MkdirAll(relDir, 0o755); err != nil {
		return tries, fmt.Errorf("mkdir parent: %w", err)
	}
	f.applyDirOwnership(root, relDir)

	shuffled := make([]Source, len(f.sources))
	copy(shuffled, f.sources)
	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

	var lastErr error
	for _, src := range shuffled {
		if ctx.Err() != nil {
			return tries, ctx.Err()
		}
		tries.attempted = append(tries.attempted, src.Name)
		tmpRel, err := f.fetchAndValidate(ctx, root, src, relPath, seq)
		if err == nil {
			if err := root.Rename(tmpRel, relPath); err != nil {
				_ = root.Remove(tmpRel)
				// The bytes arrived fine; the local placement broke.
				// Count it against the source anyway — the checkpoint
				// is still missing and the operator needs the attempt
				// and the failure to balance.
				tries.failed = append(tries.failed, src.Name)
				return tries, fmt.Errorf("rename %q → %q: %w", tmpRel, relPath, err)
			}
			tries.winner = src.Name
			return tries, nil
		}
		tries.failed = append(tries.failed, src.Name)
		lastErr = fmt.Errorf("%s: %w", src.Name, err)
	}
	if lastErr == nil {
		lastErr = errors.New("no sources configured")
	}
	return tries, lastErr
}

// fetchAndValidate performs one source GET into a new, exclusively created temp file beside the
// checkpoint, validating gzip integrity AND checkpoint content: a stale mirror serving a different
// checkpoint must not be placed. It returns the temp file's root-relative name, already owned by
// the archive's user, for the caller to rename; on failure the temp file is removed.
func (f *CrossAnchorFiller) fetchAndValidate(ctx context.Context, root *os.Root, src Source, relPath string, seq uint32) (string, error) {
	tmpRel, err := f.fetchToTemp(ctx, root, src, relPath, seq)
	if err != nil && tmpRel != "" {
		_ = root.Remove(tmpRel)
	}
	return tmpRel, err
}

// fetchToTemp is fetchAndValidate without the cleanup: a non-empty name is returned with an error
// once the temp file exists.
func (f *CrossAnchorFiller) fetchToTemp(ctx context.Context, root *os.Root, src Source, relPath string, seq uint32) (string, error) {
	url := src.URL + "/" + relPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	resp, err := f.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Drain a little of the body for diagnostics; bound it.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}

	tmpRel, out, err := createTemp(root, relPath)
	if err != nil {
		return "", fmt.Errorf("create tmp: %w", err)
	}
	defer func() { _ = out.Close() }()

	// Cap the download at 16 MiB: checkpoint files are ~5-250 KiB, so anything larger is
	// suspicious and failing beats filling the disk.
	const maxSize = 16 << 20
	written, err := io.Copy(out, io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return tmpRel, fmt.Errorf("write tmp: %w", err)
	}
	if written > maxSize {
		return tmpRel, fmt.Errorf("response too large (>%d bytes)", maxSize)
	}
	if written == 0 {
		return tmpRel, errors.New("empty response body")
	}

	// Validate through the descriptor that was written, never by re-opening the name.
	// gzip-validate first: cheap insurance against a truncated download or a 200-with-html-body.
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return tmpRel, fmt.Errorf("rewind tmp: %w", err)
	}
	n, err := validateGzipReader(out)
	if err != nil {
		return tmpRel, fmt.Errorf("gzip validate: %w", err)
	}
	if n == 0 {
		return tmpRel, errors.New("gzip validate: decompressed to 0 bytes")
	}

	// Valid gzip does not prove this is checkpoint seq: confirm the XDR holds an entry with that
	// LedgerSeq so a wrong or corrupt-but-decompressible body falls through to the next source.
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return tmpRel, fmt.Errorf("rewind tmp: %w", err)
	}
	if err := validateCheckpointStream(io.NopCloser(out), seq); err != nil {
		return tmpRel, fmt.Errorf("checkpoint content validate: %w", err)
	}

	// Ownership is set on the descriptor. Best-effort: the bytes are correct, so a failure must
	// not fail the fetch (a follow-up chown -R can fix ownership).
	if f.chownEnabled {
		_ = out.Chown(f.ownerUID, f.ownerGID)
	}
	if err := out.Close(); err != nil {
		return tmpRel, fmt.Errorf("close tmp: %w", err)
	}
	return tmpRel, nil
}

// createTemp exclusively creates a randomly named temp file beside relPath inside root.
func createTemp(root *os.Root, relPath string) (string, *os.File, error) {
	var lastErr error
	for range 8 {
		name := fmt.Sprintf("%s.%016x.new", relPath, rand.Uint64()) //nolint:gosec // G404: uniqueness, not secrecy; O_EXCL is the guard
		out, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return name, out, nil
		}
		lastErr = err
		if !errors.Is(err, os.ErrExist) {
			break
		}
	}
	return "", nil, lastErr
}

// maxDecompressedBytes caps decompressed size during gzip validation. A checkpoint holds 64
// LedgerHeaderHistoryEntry records (~16 KB decompressed), so 4 MiB is generous yet stops zip bombs.
const maxDecompressedBytes = 4 << 20

// validateGzip decompresses the file, confirms the gzip footer is intact, and returns the
// decompressed byte count. Callers must reject an empty-but-valid stream themselves (an empty
// file must not count as a checkpoint).
//
// Decompression is bounded to [maxDecompressedBytes] so a tiny malicious payload cannot expand to
// gigabytes (G110 zip-bomb DoS).
func validateGzip(path string) (int64, error) {
	f, err := os.Open(path) //nolint:gosec // path constructed from validated archiveRoot
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	return validateGzipReader(f)
}

// validateGzipReader is [validateGzip] over an open stream.
func validateGzipReader(f io.Reader) (int64, error) {
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	defer func() { _ = gz.Close() }()
	// Read one byte past the cap so exceeding it is distinguishable from hitting it exactly.
	limited := io.LimitReader(gz, maxDecompressedBytes+1)
	n, err := io.Copy(io.Discard, limited)
	if err != nil {
		return n, err
	}
	if n > maxDecompressedBytes {
		return n, fmt.Errorf("decompressed size > %d bytes (zip-bomb guard)", maxDecompressedBytes)
	}
	return n, nil
}

// maxCheckpointEntries bounds how many LedgerHeaderHistoryEntry records validateCheckpointStream
// reads. A real checkpoint holds exactly 64, so a hostile or corrupt stream never containing the
// target seq cannot spin forever.
const maxCheckpointEntries = 64

// validateCheckpointStream confirms the gzip'd XDR stream decodes to LedgerHeaderHistoryEntry records
// including one whose LedgerSeq equals wantSeq, and closes it. Valid gzip alone does not prove the RIGHT
// checkpoint: a stale mirror can serve 200 plus valid gzip for the wrong ledger range.
func validateCheckpointStream(f io.ReadCloser, wantSeq uint32) error {
	stream, err := sdkxdr.NewGzStream(f)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("open gz stream: %w", err)
	}
	defer func() { _ = stream.Close() }()

	var entry sdkxdr.LedgerHeaderHistoryEntry
	for i := 0; i < maxCheckpointEntries; i++ {
		if rerr := stream.ReadOne(&entry); rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return fmt.Errorf("stream ended after %d entries without ledger %d", i, wantSeq)
			}
			return fmt.Errorf("read entry %d: %w", i, rerr)
		}
		if uint32(entry.Header.LedgerSeq) == wantSeq {
			return nil
		}
	}
	return fmt.Errorf("exceeded %d entries without finding ledger %d", maxCheckpointEntries, wantSeq)
}

// applyDirOwnership chowns the checkpoint's parent directories up to the archive root. Chowning
// the file alone is not enough: a reader needs +x on every directory on the path, including a
// freshly created ledger/XX/YY/ZZ prefix. Best-effort. Lchown inside the root: a prefix directory
// swapped for a link is itself re-owned, never the link's target.
func (f *CrossAnchorFiller) applyDirOwnership(root *os.Root, relDir string) {
	if !f.chownEnabled {
		return
	}
	for d := filepath.Clean(relDir); d != "." && d != string(filepath.Separator); d = filepath.Dir(d) {
		_ = root.Lchown(d, f.ownerUID, f.ownerGID)
	}
}
