// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestEvaluateEachSource_OneFailureDoesNotWithholdLaterVerdicts (#805, #1202
// item 5): the per-source loop returned on the first error, so a failure in
// the FIRST catalogue source (soroswap) left every later source on its prior,
// stale verdict. Every selected source must still be evaluated, and the run
// must still fail, naming each source that errored.
func TestEvaluateEachSource_OneFailureDoesNotWithholdLaterVerdicts(t *testing.T) {
	cat := []reconSource{{name: "soroswap"}, {name: "aquarius"}, {name: "phoenix"}, {name: "blend"}}
	errSoroswap := errors.New("soroswap: projection: deadline")
	errPhoenix := errors.New("phoenix: served floor: boom")

	var evaluated []string
	err := evaluateEachSource(context.Background(), cat, "", 0, func(_ context.Context, src reconSource) error {
		evaluated = append(evaluated, src.name)
		switch src.name {
		case "soroswap":
			return errSoroswap
		case "phoenix":
			return errPhoenix
		}
		return nil
	})

	if want := []string{"soroswap", "aquarius", "phoenix", "blend"}; !slices.Equal(evaluated, want) {
		t.Errorf("evaluated %v, want every source %v — a failing source must not stop the pass", evaluated, want)
	}
	if !errors.Is(err, errSoroswap) || !errors.Is(err, errPhoenix) {
		t.Errorf("err = %v, want both source failures joined (the run must exit non-zero)", err)
	}
}

// TestEvaluateEachSource_HonoursTheSourceFilter: -source still limits the
// pass to one source.
func TestEvaluateEachSource_HonoursTheSourceFilter(t *testing.T) {
	cat := []reconSource{{name: "soroswap"}, {name: "aquarius"}, {name: "phoenix"}}
	var evaluated []string
	if err := evaluateEachSource(context.Background(), cat, "aquarius", 0, func(_ context.Context, src reconSource) error {
		evaluated = append(evaluated, src.name)
		return nil
	}); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !slices.Equal(evaluated, []string{"aquarius"}) {
		t.Errorf("evaluated %v, want only aquarius", evaluated)
	}
}

// TestEvaluateEachSource_StopsOnADoneContext: once the pass deadline has
// passed every remaining source would fail the same way, so the walk stops
// and reports what it did not evaluate.
func TestEvaluateEachSource_StopsOnADoneContext(t *testing.T) {
	cat := []reconSource{{name: "soroswap"}, {name: "aquarius"}, {name: "phoenix"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var evaluated []string
	err := evaluateEachSource(ctx, cat, "", 0, func(_ context.Context, src reconSource) error {
		evaluated = append(evaluated, src.name)
		cancel()
		return nil
	})
	if !slices.Equal(evaluated, []string{"soroswap"}) {
		t.Errorf("evaluated %v, want only soroswap before the context ended", evaluated)
	}
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "aquarius") {
		t.Errorf("err = %v, want context.Canceled naming the first unevaluated source", err)
	}
	if !strings.Contains(err.Error(), "phoenix") {
		t.Errorf("err = %v, want every skipped source named, including phoenix", err)
	}
}

// TestOrderForPass: in -pass mode a source that re-verifies from genesis runs
// after every incrementally-resuming source, so its cost cannot exhaust the
// deadline before the cheap sources publish.
func TestOrderForPass(t *testing.T) {
	cat := []reconSource{{name: "soroswap", genesis: 10}, {name: "sdex", genesis: 2}, {name: "aquarius", genesis: 10}, {name: "phoenix", genesis: 10}}
	names := func(srcs []reconSource) []string {
		out := make([]string, len(srcs))
		for i, s := range srcs {
			out[i] = s.name
		}
		return out
	}
	allOK := map[string]priorProjection{
		"soroswap": {known: true, ok: true}, "sdex": {known: true, ok: true},
		"aquarius": {known: true, ok: true}, "phoenix": {known: true, ok: true},
	}
	wm := map[string]uint32{"soroswap": 500, "sdex": 500, "aquarius": 500, "phoenix": 500}

	if got := names(orderForPass(cat, allOK, wm)); !slices.Equal(got, names(cat)) {
		t.Errorf("all incremental: got %v, want catalogue order %v", got, names(cat))
	}

	failing := maps.Clone(allOK)
	failing["sdex"] = priorProjection{known: true, ok: false}
	if got, want := names(orderForPass(cat, failing, wm)), []string{"soroswap", "aquarius", "phoenix", "sdex"}; !slices.Equal(got, want) {
		t.Errorf("failing prior: got %v, want %v", got, want)
	}

	unknown := maps.Clone(allOK)
	delete(unknown, "soroswap")
	failing2 := maps.Clone(unknown)
	failing2["aquarius"] = priorProjection{known: true, ok: false}
	if got, want := names(orderForPass(cat, failing2, wm)), []string{"sdex", "phoenix", "soroswap", "aquarius"}; !slices.Equal(got, want) {
		t.Errorf("unknown + failing prior: got %v, want %v (stable within each group)", got, want)
	}

	if got := names(cat); !slices.Equal(got, []string{"soroswap", "sdex", "aquarius", "phoenix"}) {
		t.Errorf("orderForPass mutated its input: %v", got)
	}
}

// TestOrderForPassCensusLastFromGenesis: when SDEX re-verifies from genesis
// alongside the SEP-41 sources, its census runs after them so a census that
// overruns the pass cannot leave sep41_transfers/sep41_supply without a verdict.
func TestOrderForPassCensusLastFromGenesis(t *testing.T) {
	cat := []reconSource{
		{name: "soroswap", genesis: 10},
		{name: "sdex", genesis: 2, census: true},
		{name: "sep41_transfers", genesis: 10},
		{name: "sep41_supply", genesis: 10},
	}
	prior := map[string]priorProjection{
		"soroswap": {known: true, ok: true},
		"sdex":     {known: true, ok: false},
	}
	wm := map[string]uint32{"soroswap": 500, "sdex": 500}
	got := make([]string, 0, len(cat))
	for _, s := range orderForPass(cat, prior, wm) {
		got = append(got, s.name)
	}
	if want := []string{"soroswap", "sep41_transfers", "sep41_supply", "sdex"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// An incrementally-resuming census stays in catalogue order: only its
	// full-history re-derive is the long one.
	prior["sdex"] = priorProjection{known: true, ok: true}
	got = got[:0]
	for _, s := range orderForPass(cat, prior, wm) {
		got = append(got, s.name)
	}
	if want := []string{"soroswap", "sdex", "sep41_transfers", "sep41_supply"}; !slices.Equal(got, want) {
		t.Errorf("incremental census: got %v, want %v", got, want)
	}
}

// TestEvaluateEachSource_PerSourceBudgetDoesNotStarveLaterSources: a source that
// outlives its own budget fails alone; the sources after it still run.
func TestEvaluateEachSource_PerSourceBudgetDoesNotStarveLaterSources(t *testing.T) {
	cat := []reconSource{{name: "comet"}, {name: "blend"}}
	var evaluated []string
	err := evaluateEachSource(context.Background(), cat, "", 20*time.Millisecond, func(ctx context.Context, src reconSource) error {
		evaluated = append(evaluated, src.name)
		if src.name == "comet" {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	})
	if !slices.Equal(evaluated, []string{"comet", "blend"}) {
		t.Errorf("evaluated %v, want blend to run after comet timed out", evaluated)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want comet's deadline reported (no fake verdict)", err)
	}
}
