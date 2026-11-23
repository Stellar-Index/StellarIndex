package chops

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"
	"golang.org/x/sync/errgroup"

	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// maxTargetedLedgers caps a -ledgers file. Mainnet has a few hundred upgrade ledgers; a file far past that is a wrong
// file (a full sequence dump), and the cap keeps it from becoming a multi-day walk.
const maxTargetedLedgers = 2000

// targetedObjectSpan: listed ledgers closer than this stream as one range, so a Galexie object (64 ledgers per file at
// most on our buckets) holding two of them is fetched once; the ledgers between are decoded and skipped, never written.
const targetedObjectSpan = 64

// backfillPlan is what one ch-backfill run walks. keep is nil for a -from/-to range (every ledger is written) and the
// listed set for -ledgers (only those are written); lo/hi bound the run for bucket resolution.
type backfillPlan struct {
	ranges []opsutil.RangeChunk
	keep   map[uint32]struct{}
	lo, hi uint32
}

// want is the number of ledgers the run must write for it to be complete.
func (p backfillPlan) want() int64 {
	if p.keep != nil {
		return int64(len(p.keep))
	}
	return int64(p.hi) - int64(p.lo) + 1
}

// label names the walk in progress and completion lines.
func (p backfillPlan) label() string {
	if p.keep == nil {
		return fmt.Sprintf("ledgers [%d,%d]", p.lo, p.hi)
	}
	return fmt.Sprintf("%d listed ledger(s) in [%d,%d]", len(p.keep), p.lo, p.hi)
}

// writes reports whether ledger seq is one the run writes.
func (p backfillPlan) writes(seq uint32) bool {
	if p.keep == nil {
		return true
	}
	_, ok := p.keep[seq]
	return ok
}

// coverage fails a run that wrote fewer ledgers than planned; a range keeps backfillCoverage's messages.
func (p backfillPlan) coverage(streamed int64, bucket string, interrupted bool) error {
	if p.keep == nil {
		return backfillCoverage(p.lo, p.hi, streamed, bucket, interrupted)
	}
	want := p.want()
	switch {
	case interrupted:
		return fmt.Errorf("INTERRUPTED after %d of %d listed ledgers in [%d,%d] from %q — re-run the same -ledgers file (idempotent under ReplacingMergeTree)",
			streamed, want, p.lo, p.hi, bucket)
	case streamed < want:
		return fmt.Errorf("wrote only %d of %d listed ledgers in [%d,%d] from %q — the rest are missing from the bucket or failed to extract (see the per-ledger errors above); the list is NOT complete",
			streamed, want, p.lo, p.hi, bucket)
	}
	return nil
}

// resolveBackfillPlan turns -from/-to or -ledgers into the walk. The two are exclusive so a list cannot silently widen
// into the range around it.
func resolveBackfillPlan(from, to uint, ledgersFile string, parallel int) (backfillPlan, error) {
	if ledgersFile == "" {
		if from == 0 || to == 0 || to < from {
			return backfillPlan{}, errors.New("-config, -from, -to are required; -to must be >= -from")
		}
		return backfillPlan{ranges: opsutil.SplitRange(uint32(from), uint32(to), parallel), lo: uint32(from), hi: uint32(to)}, nil
	}
	if from != 0 || to != 0 {
		return backfillPlan{}, errors.New("-ledgers cannot be combined with -from/-to")
	}
	f, err := os.Open(ledgersFile) //nolint:gosec // operator-supplied path is the flag's purpose
	if err != nil {
		return backfillPlan{}, fmt.Errorf("-ledgers: %w", err)
	}
	defer func() { _ = f.Close() }()
	seqs, err := readLedgerList(f)
	if err != nil {
		return backfillPlan{}, fmt.Errorf("-ledgers %s: %w", ledgersFile, err)
	}
	keep := make(map[uint32]struct{}, len(seqs))
	for _, s := range seqs {
		keep[s] = struct{}{}
	}
	return backfillPlan{ranges: coalesceLedgers(seqs), keep: keep, lo: seqs[0], hi: seqs[len(seqs)-1]}, nil
}

// readLedgerList parses one ledger sequence per line; `#` starts a comment, blank lines are skipped. The result is
// sorted and deduplicated; an empty list, a sequence below 2 (genesis is not streamable) or more than
// maxTargetedLedgers distinct sequences is an error.
func readLedgerList(r io.Reader) ([]uint32, error) {
	var seqs []uint32
	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		text, _, _ := strings.Cut(sc.Text(), "#")
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		n, err := strconv.ParseUint(text, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("line %d: %q is not a ledger sequence", line, text)
		}
		if n < 2 {
			return nil, fmt.Errorf("line %d: ledger %d is below 2, the first streamable ledger", line, n)
		}
		seqs = append(seqs, uint32(n))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	slices.Sort(seqs)
	seqs = slices.Compact(seqs)
	switch {
	case len(seqs) == 0:
		return nil, errors.New("no ledger sequences listed")
	case len(seqs) > maxTargetedLedgers:
		return nil, fmt.Errorf("%d ledgers listed, more than the %d a targeted run accepts — this is not an upgrade list; use -from/-to for a range",
			len(seqs), maxTargetedLedgers)
	}
	return seqs, nil
}

// coalesceLedgers merges sorted, deduplicated sequences into ranges, joining two when the second starts fewer than
// targetedObjectSpan ledgers after the first ends.
func coalesceLedgers(seqs []uint32) []opsutil.RangeChunk {
	var out []opsutil.RangeChunk
	for _, s := range seqs {
		if n := len(out); n > 0 && s-out[n-1].To < targetedObjectSpan {
			out[n-1].To = s
			continue
		}
		out = append(out, opsutil.RangeChunk{From: s, To: s})
	}
	return out
}

// assignRanges deals ranges round-robin to at most parallel workers, each of which walks its share with one Sink.
func assignRanges(ranges []opsutil.RangeChunk, parallel int) [][]opsutil.RangeChunk {
	n := max(1, min(parallel, len(ranges)))
	out := make([][]opsutil.RangeChunk, n)
	for i, r := range ranges {
		out[i%n] = append(out[i%n], r)
	}
	return out
}

// listModeAllowed: the list walk writes nothing, so it must be stated as a dry run, and it takes a range, not a list.
func listModeAllowed(listUpgrades, dryRun, changesOnly bool) error {
	switch {
	case !listUpgrades:
		return nil
	case !dryRun:
		return errors.New("-list-upgrade-ledgers writes nothing; pass -dry-run, not -write")
	case changesOnly:
		return errors.New("-list-upgrade-ledgers writes no tables; -changes-only does not apply")
	}
	return nil
}

// runListUpgradeLedgers is the -list-upgrade-ledgers run over a range or a -ledgers list: a short walk fails closed
// like a backfill, so a printed list is complete for what was walked.
func runListUpgradeLedgers(ctx context.Context, lsCfg ledgerstream.Config, plan backfillPlan, parallel int, bucket string, prog *backfillProgress) error {
	fmt.Fprintf(os.Stderr, "ch-backfill: listing upgrade ledgers in %s from %q (nothing written)\n", plan.label(), bucket)
	found, err := listUpgradeLedgers(ctx, ledgerstream.Stream, lsCfg, plan, parallel, prog.record)
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("ch-backfill: list (%d done): %w", prog.total(), err)
	}
	prog.reportRate()
	if cerr := plan.coverage(prog.total(), bucket, ctx.Err() != nil); cerr != nil {
		return fmt.Errorf("ch-backfill: %w", cerr)
	}
	if werr := writeLedgerList(os.Stdout, found, plan.label(), bucket); werr != nil {
		return fmt.Errorf("ch-backfill: %w", werr)
	}
	prog.markOK()
	fmt.Fprintf(os.Stderr, "ch-backfill: done — %d upgrade ledger(s) in %s\n", len(found), plan.label())
	return nil
}

// upgradeLedger is one ledger carrying upgrades and the entry changes they made: the rows a targeted write must leave
// at op_index -2 for it.
type upgradeLedger struct {
	seq      uint32
	upgrades int
	changes  int
}

// listUpgradeLedgers walks the plan read-only and returns every planned ledger whose close meta carries a ledger
// upgrade, sorted. It never extracts rows, so it costs the fetch + decode of each ledger.
func listUpgradeLedgers(ctx context.Context, stream streamFunc, lsCfg ledgerstream.Config, plan backfillPlan, parallel int,
	logProgress func(workerIdx int, seq uint32, supplySkips map[string]int),
) ([]upgradeLedger, error) {
	var (
		mu    sync.Mutex
		found []upgradeLedger
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(1, parallel))
	for i, r := range plan.ranges {
		g.Go(func() error {
			return stream(gctx, lsCfg, r.From, r.To, func(lcm sdkxdr.LedgerCloseMeta) error {
				if !plan.writes(lcm.LedgerSequence()) {
					return nil
				}
				if ups := lcm.UpgradesProcessing(); len(ups) > 0 {
					u := upgradeLedger{seq: lcm.LedgerSequence(), upgrades: len(ups)}
					for _, up := range ups {
						u.changes += len(up.Changes)
					}
					mu.Lock()
					found = append(found, u)
					mu.Unlock()
				}
				logProgress(i, lcm.LedgerSequence(), nil)
				return nil
			})
		})
	}
	err := g.Wait()
	slices.SortFunc(found, func(a, b upgradeLedger) int { return cmp.Compare(a.seq, b.seq) })
	return found, err
}

// writeLedgerList prints found in the -ledgers file format; each line's comment is the expected upgrade-row count.
func writeLedgerList(w io.Writer, found []upgradeLedger, walked, bucket string) error {
	// bufio.Writer keeps its first error and Flush returns it, so the per-line results carry nothing new.
	bw := bufio.NewWriter(w)
	_, _ = fmt.Fprintf(bw, "# %d ledger(s) with a ledger upgrade in %s from %q\n", len(found), walked, bucket)
	for _, u := range found {
		_, _ = fmt.Fprintf(bw, "%d # upgrades=%d changes=%d\n", u.seq, u.upgrades, u.changes)
	}
	return bw.Flush()
}
