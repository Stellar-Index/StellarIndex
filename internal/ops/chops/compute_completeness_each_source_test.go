// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
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
	err := evaluateEachSource(context.Background(), cat, "", func(src reconSource) error {
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
	if err := evaluateEachSource(context.Background(), cat, "aquarius", func(src reconSource) error {
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
	err := evaluateEachSource(ctx, cat, "", func(src reconSource) error {
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
}
