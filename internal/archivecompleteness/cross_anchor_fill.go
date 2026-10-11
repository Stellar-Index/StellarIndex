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
	"strings"
	"sync"
	"time"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"
)

// Source is one upstream URL from which a missing checkpoint file
// can be fetched. The full URL we GET is
// `<URL>/ledger/XX/YY/ZZ/ledger-XXYYZZWW.xdr.gz`.
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

// DefaultCrossAnchorSources returns the canonical fallback chain for the
// cross-anchor archive: SDF's three primary archives, then the tier-1
// validator archives the captive-core config already trusts.
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

// CrossAnchorFiller fetches missing checkpoint files from the multi-source fallback chain and
// writes them into the local archive in the SDF history-archive layout.
//
// Fill spawns Workers goroutines itself; concurrent Fill calls are safe but each gets a full pool.
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

// NewCrossAnchorFiller constructs a Filler. Returns an error if
// chown is requested but the user/group lookup fails (operator
// config issue — fail loud at startup).
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

// FillFailure describes one missing checkpoint that the filler
// couldn't recover.
type FillFailure struct {
	Seq    uint32
	Reason string
}

// Fill places each missing checkpoint via the multi-source fallback chain and returns once every
// entry has succeeded or exhausted the chain.
//
// Source order is shuffled per checkpoint to spread burst load instead of hammering source[0].
// The first source returning 200 with a non-empty, valid gzip body wins and is placed atomically
// via `<final>.new` then rename; if all fail, a [FillFailure] records the last error.
//
// ctx is checked between checkpoints: cancellation keeps already-placed files (the next run skips
// them) and stops fetching new ones.
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
	finalPath := filepath.Join(f.archiveRoot, relPath)
	tmpPath := finalPath + ".new"

	// Create the parent directory BEFORE the first GET so a bad destination fails fast even when
	// the HTTP source is healthy.
	//
	// 0755, not 0750: the daemon runs as root but the verifier (stellar-archivist / verify-archive
	// -tier checkpoint) runs as a non-root service user. A 0750 root:root parent makes every placed
	// file unreadable to it, since the chown never covers the parent, and the ADR-0017 anchor check
	// then fails with EACCES, which the verifier treats as a hard mismatch rather than a missing file.
	//
	// nolint:gosec // G301 wants <=0750. Deliberate: this is public blockchain history read by a
	// verifier running as a different user than the chown target, and every byte comes from public
	// unauthenticated mirrors, so no secret is exposed.
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil { //nolint:gosec // see note above
		return tries, fmt.Errorf("mkdir parent: %w", err)
	}
	f.applyDirOwnership(filepath.Dir(finalPath))

	shuffled := make([]Source, len(f.sources))
	copy(shuffled, f.sources)
	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

	var lastErr error
	for _, src := range shuffled {
		if ctx.Err() != nil {
			return tries, ctx.Err()
		}
		tries.attempted = append(tries.attempted, src.Name)
		err := f.fetchAndValidate(ctx, src, relPath, tmpPath, seq)
		if err == nil {
			if err := os.Rename(tmpPath, finalPath); err != nil {
				_ = os.Remove(tmpPath)
				// The bytes arrived fine; the local placement broke.
				// Count it against the source anyway — the checkpoint
				// is still missing and the operator needs the attempt
				// and the failure to balance.
				tries.failed = append(tries.failed, src.Name)
				return tries, fmt.Errorf("rename %q → %q: %w", tmpPath, finalPath, err)
			}
			f.applyOwnership(finalPath)
			tries.winner = src.Name
			return tries, nil
		}
		_ = os.Remove(tmpPath)
		tries.failed = append(tries.failed, src.Name)
		lastErr = fmt.Errorf("%s: %w", src.Name, err)
	}
	if lastErr == nil {
		lastErr = errors.New("no sources configured")
	}
	return tries, lastErr
}

// fetchAndValidate performs one source GET into tmpPath, validating gzip integrity AND checkpoint
// content: a stale mirror serving a different checkpoint must not be placed. On failure tmpPath
// is left in an unspecified state and the caller cleans it up.
func (f *CrossAnchorFiller) fetchAndValidate(ctx context.Context, src Source, relPath, tmpPath string, seq uint32) error {
	url := src.URL + "/" + relPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := f.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Drain a little of the body for diagnostics; bound it.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("http %d", resp.StatusCode)
	}

	// Cap the download at 16 MiB: checkpoint files are ~5-250 KiB, so anything larger is
	// suspicious and failing beats filling the disk.
	const maxSize = 16 << 20
	out, err := os.Create(tmpPath) //nolint:gosec // path constructed from validated archiveRoot + checkpoint hex
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	written, err := io.Copy(out, io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		_ = out.Close()
		return fmt.Errorf("write tmp: %w", err)
	}
	if written > maxSize {
		_ = out.Close()
		return fmt.Errorf("response too large (>%d bytes)", maxSize)
	}
	if written == 0 {
		_ = out.Close()
		return errors.New("empty response body")
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close tmp: %w", err)
	}

	// gzip-validate the placed file. Cheap insurance against a
	// truncated download or a 200-with-html-body case.
	n, err := validateGzip(tmpPath)
	if err != nil {
		return fmt.Errorf("gzip validate: %w", err)
	}
	if n == 0 {
		return errors.New("gzip validate: decompressed to 0 bytes")
	}

	// Valid gzip does not prove this is checkpoint seq: confirm the XDR holds an entry with that
	// LedgerSeq so a wrong or corrupt-but-decompressible body falls through to the next source.
	if err := validateCheckpointContent(tmpPath, seq); err != nil {
		return fmt.Errorf("checkpoint content validate: %w", err)
	}
	return nil
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

// maxCheckpointEntries bounds how many LedgerHeaderHistoryEntry records validateCheckpointContent
// reads. A real checkpoint holds exactly 64, so a hostile or corrupt stream never containing the
// target seq cannot spin forever.
const maxCheckpointEntries = 64

// validateCheckpointContent confirms the gzip'd XDR file at path decodes to
// LedgerHeaderHistoryEntry records including one whose LedgerSeq equals wantSeq. Valid gzip alone
// does not prove the RIGHT checkpoint: a stale mirror can serve 200 plus valid gzip for the wrong
// ledger range.
func validateCheckpointContent(path string, wantSeq uint32) error {
	f, err := os.Open(path) //nolint:gosec // path constructed from validated archiveRoot + checkpoint hex
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
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

// applyOwnership chowns the placed file when chown is enabled. Best-effort: the bytes are correct,
// so a failure must not fail the fetch (a follow-up chown -R can fix ownership).
func (f *CrossAnchorFiller) applyOwnership(path string) {
	if !f.chownEnabled {
		return
	}
	_ = os.Chown(path, f.ownerUID, f.ownerGID)
}

// applyDirOwnership chowns the checkpoint's parent directories up to the archive root. Chowning
// the file alone is not enough: a reader needs +x on every directory on the path, including a
// freshly created ledger/XX/YY/ZZ prefix. Best-effort, like applyOwnership.
func (f *CrossAnchorFiller) applyDirOwnership(dir string) {
	if !f.chownEnabled {
		return
	}
	root := filepath.Clean(f.archiveRoot)
	for d := filepath.Clean(dir); strings.HasPrefix(d, root) && d != root; d = filepath.Dir(d) {
		_ = os.Chown(d, f.ownerUID, f.ownerGID)
	}
}
