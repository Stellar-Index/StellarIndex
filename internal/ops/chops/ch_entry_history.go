package chops

import (
	"context"
	"flag"
	"fmt"
	"os"
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
	gate.Banner()
	dryRun := gate.DryRun()

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

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
// watermark after each written window.
func runEntryHistory(ctx context.Context, chAddr string, start, last, window uint32, dryRun bool, st *entryHistoryStats) error {
	runStart := time.Now()
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
