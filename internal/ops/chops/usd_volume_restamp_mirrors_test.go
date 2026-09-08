// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ─── `-tier xlm-quote` / `-tier cex-fx` — the two mirror tiers' walks ────
//
// The driver is pinned tier-agnostically by the xlm-base tests beside
// this file (plan, pre-flight, lock, policy dance, tracing, re-listing),
// and the per-row decisions are pinned in the storage package against the
// live valuation functions. What is pinned HERE is that the two new tiers
// drive that SAME walk — bracketing each compressed chunk, writing only
// through the guarded in-chunk apply, carrying the run's generation — and
// that each one names ITSELF in the header, the summary and the RESUME
// line, so an operator can never mistake one tier's report for another's.

// fakeMirrorChunkStore is the mirror tiers' seam double. It reuses the
// driver double for the chunk/policy/lock half and answers the shared
// row-list plan/apply pair, so the same dirty-row bookkeeping (a row is
// planned until an apply takes it) makes "a rerun skips finished chunks"
// observable without a database.
type fakeMirrorChunkStore struct {
	*fakeChunkStore
	// staleness records what tolerance the cex-fx planner was handed.
	staleness time.Duration
}

func newFakeMirrorChunkStore(chunks []timescale.TradeChunk, dirty ...time.Time) *fakeMirrorChunkStore {
	return &fakeMirrorChunkStore{fakeChunkStore: newFakeChunkStore(chunks, dirty...)}
}

func (f *fakeMirrorChunkStore) PlanXLMQuoteUSDVolumeRestamp(ctx context.Context, p timescale.RestampScanParams) (*timescale.RestampPlan, error) {
	return f.PlanXLMBaseUSDVolumeRestamp(ctx, p)
}

func (f *fakeMirrorChunkStore) PlanCEXFiatUSDVolumeRestamp(ctx context.Context, p timescale.RestampScanParams, maxStaleness time.Duration) (*timescale.RestampPlan, error) {
	f.staleness = maxStaleness
	return f.PlanXLMBaseUSDVolumeRestamp(ctx, p)
}

func (f *fakeMirrorChunkStore) ApplyUSDVolumeRestampPlan(ctx context.Context, plan *timescale.RestampPlan, generation int64, batch int) (int64, error) {
	return f.ApplyXLMBaseUSDVolumeRestamp(ctx, plan, generation, batch)
}

func (f *fakeMirrorChunkStore) ApplyUSDVolumeRestampPlanInChunk(ctx context.Context, c timescale.TradeChunk, plan *timescale.RestampPlan, generation int64, batch int) (int64, error) {
	return f.ApplyXLMBaseUSDVolumeRestampInChunk(ctx, c, plan, generation, batch)
}

// assertBracketedChunkWalk checks the property both mirror tiers inherit
// from the driver: every chunk bracketed once, in range order, with every
// write inside its own bracket and routed through the guarded in-chunk
// apply at the chunk batch.
func assertBracketedChunkWalk(t *testing.T, store *fakeChunkStore) {
	t.Helper()
	var brackets []string
	open := ""
	for _, l := range store.log {
		switch {
		case strings.HasPrefix(l, "decompress "):
			if open != "" {
				t.Fatalf("chunk %s decompressed while %s was still open", l, open)
			}
			open = strings.TrimPrefix(l, "decompress ")
			brackets = append(brackets, open)
		case strings.HasPrefix(l, "compress "):
			if name := strings.TrimPrefix(l, "compress "); name != open {
				t.Fatalf("compress %s while %q was open", name, open)
			}
			open = ""
		case strings.HasPrefix(l, "apply "):
			if open == "" {
				t.Fatalf("apply outside a bracket: %s\n%s", l, strings.Join(store.log, "\n"))
			}
			if !strings.HasSuffix(l, " in-chunk="+open) {
				t.Errorf("apply inside %s bypassed the guarded in-chunk apply: %s", open, l)
			}
			if !strings.Contains(l, "gen=1756800000") {
				t.Errorf("apply did not carry the run's generation (INV-3): %s", l)
			}
			if !strings.Contains(l, "batch=20000") {
				t.Errorf("apply inside a decompressed chunk used %s, want the chunk batch (20000)", l)
			}
		}
	}
	if want := []string{"_hyper_1_1_chunk", "_hyper_1_2_chunk", "_hyper_1_3_chunk"}; strings.Join(brackets, ",") != strings.Join(want, ",") {
		t.Errorf("brackets = %v, want %v", brackets, want)
	}
	if lock, pause := store.index("lock"), store.index("pause job"); lock < 0 || pause < 0 || lock > pause || pause > brackets0(store) {
		t.Errorf("order of lock (%d), pause (%d), first decompress (%d):\n%s", lock, pause, brackets0(store), strings.Join(store.log, "\n"))
	}
	wantTeardown(t, store)
}

func TestXLMQuoteChunkRestamp_BracketsEachChunkAndNamesItsOwnTier(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeMirrorChunkStore(chunks, from.Add(3*time.Hour), from.AddDate(0, 0, 5), to.Add(2*time.Hour))
	opts, copts, out := chunkTestOptions(true)
	var errw strings.Builder
	copts.Err = &errw

	if err := runXLMQuoteChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(store.done) != 3 {
		t.Fatalf("restamped %d row(s), want 3:\n%s", len(store.done), strings.Join(store.log, "\n"))
	}
	assertBracketedChunkWalk(t, store.fakeChunkStore)

	if want := "usd-volume-restamp: tier=xlm-quote mode=chunks"; !strings.Contains(errw.String(), want) {
		t.Errorf("run header lacks %q:\n%s", want, errw.String())
	}
	got := out.String()
	for _, want := range []string{
		"chunk 1/3 _timescaledb_internal._hyper_1_1_chunk",
		"changed 1 row(s) (planned 1)",
		"restamped 3 row(s) in [2026-01-05, 2026-01-18] (tier xlm-quote)",
		"CALL refresh_continuous_aggregate('prices_1m'",
		"CALL refresh_continuous_aggregate('twap_1d'",
		"acceptance: stellarindex-ops verify-usd-volume -config /etc/stellarindex.toml -day 2026-01-18 -days 14",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("write-run output lacks %q:\n%s", want, got)
		}
	}
	// The #372 tier's own measured flag guidance is not this tier's.
	if strings.Contains(got, "FLAG GUIDANCE") {
		t.Errorf("the xlm-quote summary carries the xlm-base tier's flag guidance:\n%s", got)
	}
}

func TestCEXFiatChunkRestamp_BracketsEachChunkAndCarriesTheAsOfTolerance(t *testing.T) {
	chunks, from, to := threeChunks()
	store := newFakeMirrorChunkStore(chunks, from.Add(3*time.Hour), from.AddDate(0, 0, 5), to.Add(2*time.Hour))
	opts, copts, out := chunkTestOptions(true)
	var errw strings.Builder
	copts.Err = &errw
	const tolerance = 48 * time.Hour

	if err := runCEXFiatChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts, tolerance); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(store.done) != 3 {
		t.Fatalf("restamped %d row(s), want 3:\n%s", len(store.done), strings.Join(store.log, "\n"))
	}
	assertBracketedChunkWalk(t, store.fakeChunkStore)

	// The tolerance reaches the planner AND the operator: a run at a
	// narrowed tolerance leaves different rows unpriced, so a report that
	// did not say so could not be read.
	if store.staleness != tolerance {
		t.Errorf("planner was handed a %s tolerance, want %s", store.staleness, tolerance)
	}
	if want := "tier=cex-fx mode=chunks"; !strings.Contains(errw.String(), want) {
		t.Errorf("run header lacks %q:\n%s", want, errw.String())
	}
	if want := "fx_max_staleness=48h0m0s"; !strings.Contains(errw.String(), want) {
		t.Errorf("run header lacks the as-of tolerance %q:\n%s", want, errw.String())
	}
	got := out.String()
	for _, want := range []string{
		"restamped 3 row(s) in [2026-01-05, 2026-01-18] (tier cex-fx)",
		"CALL refresh_continuous_aggregate('prices_1m'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("write-run output lacks %q:\n%s", want, got)
		}
	}
}

// TestMirrorChunkRestamp_ResumeLineNamesTheTierAndItsFlags: a stopped run
// prints the command that continues IT — the same tier, the same
// generation, the same population flags. A RESUME line that named the
// wrong tier would send the operator to restamp a different 18M rows.
func TestMirrorChunkRestamp_ResumeLineNamesTheTierAndItsFlags(t *testing.T) {
	chunks, from, to := threeChunks()
	opts, copts, out := chunkTestOptions(true)

	for _, tc := range []struct {
		tier string
		run  func(store *fakeMirrorChunkStore, copts chunkRestampOptions) error
		want []string
	}{
		{
			tier: "xlm-quote",
			run: func(store *fakeMirrorChunkStore, copts chunkRestampOptions) error {
				return runXLMQuoteChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts)
			},
			want: []string{"RESUME:", "-tier xlm-quote -chunks", "-generation 1756800000"},
		},
		{
			tier: "cex-fx",
			run: func(store *fakeMirrorChunkStore, copts chunkRestampOptions) error {
				return runCEXFiatChunkRestamp(context.Background(), store, "/etc/stellarindex.toml", from, to, opts, copts, 72*time.Hour)
			},
			want: []string{"RESUME:", "-tier cex-fx -chunks", "-generation 1756800000", "-fx-max-staleness 72h0m0s"},
		},
	} {
		t.Run(tc.tier, func(t *testing.T) {
			out.Reset()
			store := newFakeMirrorChunkStore(chunks, from.Add(3*time.Hour), from.AddDate(0, 0, 5), to.Add(2*time.Hour))
			store.recompressUnderneath = "_hyper_1_2_chunk"
			copts := copts
			copts.Out = out
			err := tc.run(store, copts)
			if err == nil {
				t.Fatal("a chunk re-compressed underneath the run did not stop it")
			}
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("resume line lacks %q:\n%s", want, out.String())
				}
			}
			wantTeardown(t, store.fakeChunkStore)
		})
	}
}

// TestValidateRestampTierFlags_MirrorTiers is the flag contract for the
// two new tiers: they take the estimated tiers' flags, `-tier exact`
// still refuses them, `-fx-max-staleness` belongs to cex-fx ALONE, and an
// unknown tier names every accepted spelling. A flag silently ignored on
// a money-column writer is the failure this closes.
func TestValidateRestampTierFlags_MirrorTiers(t *testing.T) {
	t.Parallel()
	for _, tier := range []string{restampTierXLMQuote, restampTierCEXFX} {
		if err := validateRestampTierFlags(tier, map[string]bool{"report": true, "batch": true, "chunks": true, "chunk-batch": true, "fill-null": true}); err != nil {
			t.Errorf("-tier %s with the estimated tiers' flags was refused: %v", tier, err)
		}
	}
	// -fx-max-staleness: cex-fx only, in BOTH directions.
	if err := validateRestampTierFlags(restampTierCEXFX, map[string]bool{"fx-max-staleness": true}); err != nil {
		t.Errorf("-fx-max-staleness with -tier cex-fx was refused: %v", err)
	}
	for _, tier := range []string{restampTierExact, restampTierXLMBase, restampTierXLMQuote} {
		err := validateRestampTierFlags(tier, map[string]bool{"fx-max-staleness": true})
		if err == nil || !strings.Contains(err.Error(), "-fx-max-staleness") {
			t.Errorf("-fx-max-staleness with -tier %s: err = %v, want a refusal naming the flag", tier, err)
		}
	}
	// The estimated-tier flags are still refused with -tier exact.
	for _, f := range restampEstimatedOnlyFlags {
		err := validateRestampTierFlags(restampTierExact, map[string]bool{f: true})
		if err == nil || !strings.Contains(err.Error(), "-"+f) {
			t.Errorf("-%s with -tier exact: err = %v, want a refusal naming the flag", f, err)
		}
	}
	// An unknown tier lists all four.
	err := validateRestampTierFlags("xlm", nil)
	for _, want := range restampTiers {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("unknown-tier refusal does not name %q: %v", want, err)
		}
	}
}

// TestValidateRestampFXStaleness: the as-of tolerance may be narrowed,
// never widened — a quote the live insert path declines as stale must
// never become a backfilled value — and never zeroed, which would refuse
// every row while reporting them as unpriceable.
func TestValidateRestampFXStaleness(t *testing.T) {
	t.Parallel()
	for _, ok := range []time.Duration{time.Hour, 24 * time.Hour, timescale.CEXFiatMaxQuoteStaleness} {
		if err := validateRestampFXStaleness(ok); err != nil {
			t.Errorf("-fx-max-staleness %s was refused: %v", ok, err)
		}
	}
	for _, bad := range []time.Duration{0, -time.Hour, timescale.CEXFiatMaxQuoteStaleness + time.Second} {
		if err := validateRestampFXStaleness(bad); err == nil {
			t.Errorf("-fx-max-staleness %s was accepted", bad)
		}
	}
}
