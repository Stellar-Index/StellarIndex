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
var ErrSoleWriterCAGGWindow = errors.New("persist_per_source = false (ADR-0032 Phase 4) would stop feeding continuous aggregates: " +
	"their refresh start_offset is shorter than the projector's stall bound " + ProjectorStallBound.String())

// CAGGWindowReader lists continuous-aggregate refresh windows;
// *timescale.Store implements it.
type CAGGWindowReader interface {
	CAGGRefreshWindows(ctx context.Context) ([]timescale.CAGGRefreshWindow, error)
}

// VerifySoleWriterCAGGCoverage is the pre-flip gate for
// [SinkModeSkipProjected]. In every other mode the dispatcher still writes
// DEX trades and oracle updates live, so the aggregates are fed on time
// and this returns nil. In Phase 4 the projector is their only writer and
// it can deliver rows up to [ProjectorStallBound] late; an aggregate whose
// refresh lookback is shorter never materializes them. Low projector lag
// does not prove the flip safe — a single lake hole does the damage — so
// the indexer refuses Phase 4 until every aggregate's lookback covers the
// bound. Every aggregate is checked, not a list of projector-written
// tables, so a new one cannot slip past a stale list; one with no refresh
// policy, or a database reporting none at all, also fails.
func VerifySoleWriterCAGGCoverage(ctx context.Context, r CAGGWindowReader, mode SinkMode) error {
	if mode != SinkModeSkipProjected {
		return nil
	}
	windows, err := r.CAGGRefreshWindows(ctx)
	if err != nil {
		return fmt.Errorf("phase-4 cagg coverage: %w", err)
	}
	if len(windows) == 0 {
		return fmt.Errorf("%w: the database reports no continuous aggregates, so coverage is unproven", ErrSoleWriterCAGGWindow)
	}
	var short []string
	for _, w := range windows {
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
