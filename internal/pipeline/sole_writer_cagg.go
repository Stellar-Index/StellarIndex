// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ProjectorStallBound is the longest the projector can hold a projected
// event behind a lake hole before writing it: one ch-live-catchup period
// (deploy/systemd/ch-live-catchup.timer, 10 minutes) plus a 5-minute margin
// for the catch-up's own drain and clock skew — the bound migration 0165
// sized prices_1m's lookback to.
const ProjectorStallBound = 15 * time.Minute

// ErrSoleWriterCAGGWindow is returned by [VerifySoleWriterCAGGCoverage]
// when a continuous aggregate would not re-aggregate rows the projector
// writes late.
var ErrSoleWriterCAGGWindow = errors.New("rows only the projector writes would miss continuous aggregates: " +
	"their refresh start_offset is shorter than the projector's stall bound " + ProjectorStallBound.String())

// CAGGWindowReader lists continuous-aggregate refresh windows;
// *timescale.Store implements it.
type CAGGWindowReader interface {
	CAGGRefreshWindows(ctx context.Context) ([]timescale.CAGGRefreshWindow, error)
}

// VerifySoleWriterCAGGCoverage refuses a sink mode in which the projector
// is the only writer of a table some continuous aggregate reads with a
// refresh lookback shorter than [ProjectorStallBound]: the projector can
// deliver rows that late, and the policy never materializes them. Low
// projector lag does not prove a mode safe — a single lake hole does the
// damage.
//
// In Phase 4 ([SinkModeSkipProjected]) every aggregate is checked, not a
// list of projector-written tables, so a new one cannot slip past a stale
// list. In Phase 3 ([SinkModeSkipSoleWriter]) the dispatcher still writes
// the other projected sources live, so only aggregates over a
// [ProjectorSpec.SoleWriter] source's tables are checked. A missing
// refresh policy, or a database reporting no aggregates at all, also fails.
func VerifySoleWriterCAGGCoverage(ctx context.Context, r CAGGWindowReader, mode SinkMode) error {
	tables, all, err := soleWrittenTables(mode)
	if err != nil {
		return err
	}
	if !all && len(tables) == 0 {
		return nil
	}
	windows, err := r.CAGGRefreshWindows(ctx)
	if err != nil {
		return fmt.Errorf("sole-writer cagg coverage: %w", err)
	}
	if len(windows) == 0 {
		return fmt.Errorf("%w: the database reports no continuous aggregates, so coverage is unproven", ErrSoleWriterCAGGWindow)
	}
	var short []string
	for _, w := range windows {
		if !all && w.Hypertable != "" && !tables[w.Hypertable] {
			continue
		}
		switch {
		case !w.HasPolicy:
			short = append(short, w.View+" (no refresh policy)")
		case w.Unbounded:
		case w.StartOffset < ProjectorStallBound:
			short = append(short, fmt.Sprintf("%s (start_offset %s)", w.View, w.StartOffset))
		}
	}
	if len(short) > 0 {
		return fmt.Errorf("%w: %s", ErrSoleWriterCAGGWindow, strings.Join(short, ", "))
	}
	return nil
}

// soleWrittenTables returns the tables only the projector writes under
// mode: in Phase 3, those the gap-detector targets name for each
// SoleWriter source. all is true in Phase 4, where every table is.
func soleWrittenTables(mode SinkMode) (tables map[string]bool, all bool, err error) {
	switch mode {
	case SinkModeSkipProjected:
		return nil, true, nil
	case SinkModeSkipSoleWriter:
	default:
		return nil, false, nil
	}
	tables = map[string]bool{}
	for i := range specs {
		if p := specs[i].Projector; p == nil || !p.SoleWriter {
			continue
		}
		found := false
		for _, t := range timescale.DefaultGapDetectorTargets {
			if t.SourceNetKey() == specs[i].Name {
				tables[t.Table], found = true, true
			}
		}
		if !found {
			return nil, false, fmt.Errorf("%w: sole-writer source %s has no gap-detector target naming its tables, so coverage is unproven",
				ErrSoleWriterCAGGWindow, specs[i].Name)
		}
	}
	return tables, false, nil
}
