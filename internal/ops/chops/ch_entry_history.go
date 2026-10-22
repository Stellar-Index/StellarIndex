package chops

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/entryhistory"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// ch-entry-history derives the sponsorship lifecycle (stellar.sponsorship_events) from the paired pre/post images of
// the entry changes that can carry a transition, windowed and resumable via its watermark and never written past the
// entry_index watermark; without -write it only counts.
func chEntryHistory(args []string) error {
	fs := flag.NewFlagSet("ch-entry-history", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	from := fs.Uint("from", 0, "first ledger (0 = resume from the watermark, or -floor-ledger on first run)")
	to := fs.Uint("to", 0, "last ledger (inclusive; 0 = current contiguous lake tip). Always clamped down to the contiguous tip and, with -write, to the entry_index watermark.")
	window := fs.Uint("window", 10_000, "ledgers per derive window")
	floorLedger := fs.Uint("floor-ledger", 1, "first-run start when no watermark exists; clamped up to the lake's first ledger")
	maxDecodeErrs := registerDecodeBudget(fs)
	totalsOnly := fs.Bool("totals-only", false, "refresh only stellar.sponsorship_totals_buckets from the derived events, from its watermark (or -from) through the sponsorship_events watermark (or -to)")
	proof := fs.String("proof", "", "comma-separated accounts: print each one's sponsorship counters against the derived in-force reserves and exit (reads only)")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *window == 0 {
		return fmt.Errorf("-window must be > 0")
	}
	if *floorLedger == 0 {
		return fmt.Errorf("-floor-ledger must be > 0 (the genesis ledger is 1)")
	}
	if *proof != "" {
		ctx, cancel := opsutil.SignalContext()
		defer cancel()
		return runSponsorshipProof(ctx, *chAddr, strings.Split(*proof, ","), os.Stdout)
	}
	gate.Banner()
	dryRun := gate.DryRun()

	ctx, cancel := opsutil.SignalContext()
	defer cancel()
	if *totalsOnly {
		return runTotalsOnly(ctx, *chAddr, uint32(*from), uint32(*to), uint32(*window), dryRun) //nolint:gosec // ledger numbers fit uint32
	}

	start, last, err := resolveDeriveRange(ctx, *chAddr, "ch-entry-history", uint32(*from), uint32(*to), uint32(*floorLedger), clickhouse.SponsorshipWatermark) //nolint:gosec // ledger numbers fit uint32
	if err != nil {
		return err
	}
	if last < start {
		fmt.Fprintf(os.Stderr, "ch-entry-history: nothing to derive (start %d, contiguous tip %d)\n", start, last)
		return nil
	}
	if !dryRun {
		// The account route proves in-force sponsorships against the entry index, so the events never run ahead of it.
		idx, err := clickhouse.EntryIndexWatermark(ctx, *chAddr)
		if err != nil {
			return fmt.Errorf("read entry_index watermark: %w", err)
		}
		if idx < last {
			last = idx
		}
		if last < start {
			fmt.Fprintf(os.Stderr, "ch-entry-history: nothing to derive (start %d, entry_index watermark %d)\n", start, last)
			return nil
		}
	}
	st := newEntryHistoryStats()
	if err := runEntryHistory(ctx, *chAddr, start, last, uint32(*window), dryRun, st); err != nil { //nolint:gosec // window fits uint32
		return err
	}
	st.report(start, last)
	return enforceDecodeBudget("ch-entry-history", st.skipped, *maxDecodeErrs)
}

// runEntryHistory derives [start, last] window by window, advancing the
// watermark after each written window, then refreshing its totals buckets.
func runEntryHistory(ctx context.Context, chAddr string, start, last, window uint32, dryRun bool, st *entryHistoryStats) error {
	runStart := time.Now()
	totals := sponsorshipTotalsFollower{addr: chAddr}
	if !dryRun {
		if err := totals.start(ctx, start, window); err != nil {
			return err
		}
	}
	for lo := start; ; {
		hi := last
		if last-lo >= window {
			hi = lo + window - 1
		}
		n, err := deriveSponsorshipWindow(ctx, chAddr, lo, hi, dryRun, st)
		if err != nil {
			return fmt.Errorf("window [%d,%d]: %w — resume with -from %d (or no -from: the watermark holds)", lo, hi, err, lo)
		}
		if !dryRun {
			if err := setSponsorshipWatermark(ctx, chAddr, lo, hi); err != nil {
				return fmt.Errorf("advance watermark to %d: %w", hi, err)
			}
			if err := totals.follow(ctx, lo, hi); err != nil {
				return fmt.Errorf("window [%d,%d] totals: %w", lo, hi, err)
			}
		}
		fmt.Fprintf(os.Stderr, "ch-entry-history: window [%d,%d] done — %d sponsorship events (elapsed %s)\n",
			lo, hi, n, time.Since(runStart).Round(time.Second))
		if hi >= last {
			return nil
		}
		lo = hi + 1
	}
}

// Seams so a window's derive is testable without a live lake.
var (
	streamSponsorshipCandidates = clickhouse.StreamSponsorshipCandidates
	insertSponsorshipEvents     = clickhouse.InsertSponsorshipEvents
	setSponsorshipWatermark     = clickhouse.SetSponsorshipWatermark
	sponsorshipTotalsWatermark  = clickhouse.SponsorshipTotalsWatermark
	sponsorshipEventsWatermark  = clickhouse.SponsorshipWatermark
	refreshSponsorshipTotals    = clickhouse.RefreshSponsorshipTotals
	setSponsorshipTotalsWM      = clickhouse.SetSponsorshipTotalsWatermark
	entryHistoryFlushRowCount   = 50_000
)

// deriveSponsorshipWindow projects one window, returning the events written (or that would be).
func deriveSponsorshipWindow(ctx context.Context, addr string, lo, hi uint32, dryRun bool, st *entryHistoryStats) (int64, error) {
	p := entryhistory.NewProjector()
	var (
		events  []clickhouse.SponsorshipEvent
		written int64
		skipped uint64
	)
	flush := func() error {
		if !dryRun {
			if _, err := insertSponsorshipEvents(ctx, addr, events); err != nil {
				return err
			}
		}
		written += int64(len(events))
		events = events[:0]
		return nil
	}
	err := streamSponsorshipCandidates(ctx, addr, lo, hi, func(r clickhouse.EntryHistorySourceRow) error {
		evs, err := p.ProjectSponsorships(r)
		if err != nil {
			skipped++
			if skipped <= 3 {
				fmt.Fprintf(os.Stderr, "ch-entry-history: skip ledger %d tx %s op %d change %d: %v\n", r.Ledger, r.TxHash, r.OpIndex, r.ChangeIndex, err)
			}
			return nil
		}
		st.record(evs)
		events = append(events, evs...)
		if len(events) >= entryHistoryFlushRowCount {
			return flush()
		}
		return nil
	})
	if err != nil {
		return written, err
	}
	if err := flush(); err != nil {
		return written, err
	}
	st.preImageMissing += p.PreImageMissing
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "ch-entry-history: window [%d,%d]: %d changes skipped (decode)\n", lo, hi, skipped)
	}
	st.skipped += skipped
	return written, nil
}

// entryHistoryStats counts the derive's events and what it could not pair or decode.
type entryHistoryStats struct {
	events                   map[string]int64
	skipped, preImageMissing uint64
}

func newEntryHistoryStats() *entryHistoryStats {
	return &entryHistoryStats{events: map[string]int64{}}
}

func (s *entryHistoryStats) record(evs []clickhouse.SponsorshipEvent) {
	for _, e := range evs {
		s.events[e.Event]++
	}
}

func (s *entryHistoryStats) report(from, to uint32) {
	fmt.Fprintf(os.Stderr, "ch-entry-history: [%d,%d] sponsorship events started=%d revoked=%d ended=%d; removals without a pre-image=%d\n",
		from, to, s.events[entryhistory.SponsorshipStarted], s.events[entryhistory.SponsorshipRevoked], s.events[entryhistory.SponsorshipEnded], s.preImageMissing)
	if s.skipped > 0 {
		fmt.Fprintf(os.Stderr, "  skipped (undecodable)=%d\n", s.skipped)
	}
}

// runSponsorshipProof prints, per account, the entry's own sponsorship counters beside the in-force reserves
// derived through the watermark, and whether they agree.
func runSponsorshipProof(ctx context.Context, chAddr string, accounts []string, out io.Writer) error {
	rd, err := clickhouse.NewExplorerReader(ctx, chAddr)
	if err != nil {
		return err
	}
	defer func() { _ = rd.Close() }()
	wm, err := rd.EntryHistoryWatermark(ctx)
	if err != nil {
		return err
	}
	for _, g := range accounts {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		p, err := sponsorshipProofFor(ctx, rd, g, wm)
		if err != nil {
			return fmt.Errorf("%s: %w", g, err)
		}
		verdict := "not comparable"
		if p.Match != nil {
			verdict = map[bool]string{true: "match", false: "MISMATCH"}[*p.Match]
		}
		if _, err := fmt.Fprintf(out, "%s: entry@%d sponsoring=%d sponsored=%d derived@%d sponsoring=%d sponsored=%d %s\n",
			g, p.LastModifiedLedger, p.NumSponsoring, p.NumSponsored, wm, p.DerivedSponsoring, p.DerivedSponsored, verdict); err != nil {
			return err
		}
	}
	return nil
}

func sponsorshipProofFor(ctx context.Context, rd *clickhouse.ExplorerReader, g string, wm uint32) (entryhistory.SponsorshipProof, error) {
	st, err := rd.AccountState(ctx, g)
	if err != nil {
		return entryhistory.SponsorshipProof{}, err
	}
	sponsoring, err := rd.SponsorshipTotals(ctx, g, clickhouse.SponsorshipSideSponsor, wm)
	if err != nil {
		return entryhistory.SponsorshipProof{}, err
	}
	sponsored, err := rd.SponsorshipTotals(ctx, g, clickhouse.SponsorshipSideSponsored, wm)
	if err != nil {
		return entryhistory.SponsorshipProof{}, err
	}
	return entryhistory.ProveSponsorships(st, wm, sponsoring, sponsored), nil
}

// sponsorshipTotalsFollower keeps the totals buckets in step with the derive. On a host whose events predate the
// buckets the first advance is refused, and it turns off rather than claim buckets with history missing below them.
type sponsorshipTotalsFollower struct {
	addr string
	off  bool
}

// start closes any gap a stopped run left between the totals watermark and the events below the derive's start.
func (f *sponsorshipTotalsFollower) start(ctx context.Context, start, window uint32) error {
	wm, err := sponsorshipTotalsWatermark(ctx, f.addr)
	if err != nil {
		return fmt.Errorf("read sponsorship_totals watermark: %w", err)
	}
	ev, err := sponsorshipEventsWatermark(ctx, f.addr)
	if err != nil {
		return fmt.Errorf("read sponsorship_events watermark: %w", err)
	}
	if end := min(start-1, ev); wm > 0 && wm < end {
		return f.run(ctx, wm+1, end, window)
	}
	return nil
}

func (f *sponsorshipTotalsFollower) follow(ctx context.Context, lo, hi uint32) error {
	if f.off {
		return nil
	}
	err := f.run(ctx, lo, hi, hi-lo+1)
	if errors.Is(err, clickhouse.ErrSponsorshipTotalsUnfilled) {
		f.off = true
		fmt.Fprintf(os.Stderr, "ch-entry-history: sponsorship totals buckets unfilled (%v); fill them with -totals-only -write\n", err)
		return nil
	}
	return err
}

// run refreshes [lo, last] window by window, advancing the totals watermark after each.
func (f *sponsorshipTotalsFollower) run(ctx context.Context, lo, last, window uint32) error {
	for {
		hi := last
		if last-lo >= window {
			hi = lo + window - 1
		}
		if err := refreshSponsorshipTotals(ctx, f.addr, lo, hi); err != nil {
			return err
		}
		if err := setSponsorshipTotalsWM(ctx, f.addr, lo, hi); err != nil {
			return fmt.Errorf("advance sponsorship_totals watermark to %d: %w", hi, err)
		}
		fmt.Fprintf(os.Stderr, "ch-entry-history: totals [%d,%d] done\n", lo, hi)
		if hi >= last {
			return nil
		}
		lo = hi + 1
	}
}

// runTotalsOnly fills the totals buckets from already-derived events, resumable from their watermark.
func runTotalsOnly(ctx context.Context, addr string, from, to, window uint32, dryRun bool) error {
	ev, err := sponsorshipEventsWatermark(ctx, addr)
	if err != nil {
		return fmt.Errorf("read sponsorship_events watermark: %w", err)
	}
	wm, err := sponsorshipTotalsWatermark(ctx, addr)
	if err != nil {
		return fmt.Errorf("read sponsorship_totals watermark: %w", err)
	}
	lo, last := wm+1, ev
	if from > 0 {
		lo = from
	}
	if to > 0 && to < last {
		last = to
	}
	if last < lo {
		fmt.Fprintf(os.Stderr, "ch-entry-history: totals: nothing to refresh (from %d, events watermark %d)\n", lo, ev)
		return nil
	}
	if dryRun {
		fmt.Fprintf(os.Stderr, "ch-entry-history: totals: would refresh [%d,%d] in %d-ledger windows\n", lo, last, window)
		return nil
	}
	f := sponsorshipTotalsFollower{addr: addr}
	if err := f.run(ctx, lo, last, window); err != nil {
		return fmt.Errorf("totals: %w — resume with no -from (the watermark holds)", err)
	}
	return nil
}
