// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The restamp tool must exist under its runbook name and sit in the WRITING half of the dispatcher.
// The verifier half must never resolve a verb that mutates rows.
func TestUSDVolumeRestamp_IsDispatchable(t *testing.T) {
	if _, ok := lakeMutatorVerb("usd-volume-restamp"); !ok {
		t.Fatal(`lakeMutatorVerb("usd-volume-restamp") = not found — the W5.3 restamp tool is not dispatchable`)
	}
	if _, ok := verifierVerb("usd-volume-restamp"); ok {
		t.Fatal("usd-volume-restamp resolved as a VERIFIER verb, but it writes trades.usd_volume")
	}
}

// A money-column writer gets no "whole history" default and never touches today's still-being-written chunk. The window
// check runs before any DB is opened, so no DB is needed.
func TestUSDVolumeRestamp_RefusesUnboundedOrLiveWindows(t *testing.T) {
	now := time.Date(2026, 8, 28, 13, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		from, to string
		wantErr  string
	}{
		{"no window", "", "", "-from and -to are required"},
		{"half window", "2026-05-12", "", "-from and -to are required"},
		{"reversed", "2026-07-22", "2026-05-12", "is before -from"},
		{"reaches today", "2026-08-01", "2026-08-28", "reaches today"},
		{"future", "2026-09-01", "2026-09-02", "reaches today"},
		{"bad format", "12/05/2026", "2026-07-22", "want YYYY-MM-DD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := resolveRestampWindow(tc.from, tc.to, now)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}

	from, to, err := resolveRestampWindow("2026-05-12", "2026-07-22", now)
	if err != nil {
		t.Fatal(err)
	}
	if from.Format(time.DateOnly) != "2026-05-12" || to.Format(time.DateOnly) != "2026-07-22" {
		t.Errorf("window = [%s, %s]", from, to)
	}
	if from.Location() != time.UTC || to.Location() != time.UTC {
		t.Error("window days are not UTC")
	}
	// Yesterday is the newest closed day and must be accepted.
	if _, _, err := resolveRestampWindow("2026-08-27", "2026-08-27", now); err != nil {
		t.Errorf("yesterday rejected: %v", err)
	}
}

func TestRestampSourceAllowList(t *testing.T) {
	if got := restampSourceAllowList(""); got != nil {
		t.Errorf("empty -sources = %v, want nil (all sources)", got)
	}
	got := restampSourceAllowList(" sdex, soroswap ,")
	if len(got) != 2 || !got["sdex"] || !got["soroswap"] {
		t.Errorf("allow list = %v", got)
	}
}

// An estimated tier's -sources value naming a source outside the tier's own registry must be refused, not silently
// dropped by restampScanSources into an empty, clean-looking scan.
func TestValidateRestampSourceAllowList_unknownNameRefused(t *testing.T) {
	// sdex is a DEX source; -tier cex-fx only recognises CEX sources.
	err := validateRestampSourceAllowList(restampTierCEXFX, restampSourceAllowList("sdex"))
	if err == nil {
		t.Fatal("validateRestampSourceAllowList(cex-fx, {sdex}) = nil — a DEX-only source on the CEX tier would scan nothing and exit 0")
	}
	if !strings.Contains(err.Error(), "sdex") {
		t.Errorf("refusal must name the offending value; got: %v", err)
	}
	if !strings.Contains(err.Error(), restampTierCEXFX) {
		t.Errorf("refusal must name the tier; got: %v", err)
	}

	// A plain typo on the DEX-anchored tiers is refused the same way.
	if err := validateRestampSourceAllowList(restampTierXLMBase, restampSourceAllowList("soroswapp")); err == nil {
		t.Fatal("validateRestampSourceAllowList(xlm-base, {soroswapp}) = nil — a typo would scan nothing and exit 0")
	}
}

// TestValidateRestampSourceAllowList_knownNamesAccepted: a real source on
// its own tier, and an unset -sources (nil allow-list, meaning "all"),
// pass through untouched.
func TestValidateRestampSourceAllowList_knownNamesAccepted(t *testing.T) {
	if err := validateRestampSourceAllowList(restampTierXLMBase, restampSourceAllowList("sdex")); err != nil {
		t.Errorf("known DEX source on xlm-base refused: %v", err)
	}
	if err := validateRestampSourceAllowList(restampTierCEXFX, nil); err != nil {
		t.Errorf("nil allow-list (all sources) refused: %v", err)
	}
	// The exact tier is data-driven, not registry-checked here.
	if err := validateRestampSourceAllowList(restampTierExact, restampSourceAllowList("not-a-real-source")); err != nil {
		t.Errorf("exact tier must not be registry-gated by this guard: %v", err)
	}
}

type fakePrices1mRetention struct {
	armed bool
	err   error
}

func (f fakePrices1mRetention) Prices1mRetentionArmed(context.Context) (bool, error) {
	return f.armed, f.err
}

// An armed prices_1m retention policy refuses every -write (the CAGG follow-up would rebuild TWAP rungs from dropped
// rows) and every estimated-tier run (a dropped range would read as "anchor declined"); only a dry exact-tier run reads
// neither.
func TestCheckRestampPrices1mRetention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		tier    string
		write   bool
		refused bool
	}{
		{restampTierExact, false, false},
		{restampTierExact, true, true},
		{restampTierXLMBase, false, true},
		{restampTierXLMBase, true, true},
		{restampTierXLMQuote, false, true},
		{restampTierCEXFX, false, true},
		{restampTierCEXFX, true, true},
	}
	for _, c := range cases {
		err := checkRestampPrices1mRetention(ctx, fakePrices1mRetention{armed: true}, c.tier, c.write)
		if c.refused != (err != nil) {
			t.Errorf("armed, tier %s write=%v: err = %v, want refused=%v", c.tier, c.write, err, c.refused)
		}
		if err != nil && !strings.Contains(err.Error(), "0156_prices_1m_retention.up.sql") {
			t.Errorf("tier %s write=%v: refusal %q does not name the disarm procedure", c.tier, c.write, err)
		}
		if err := checkRestampPrices1mRetention(ctx, fakePrices1mRetention{}, c.tier, c.write); err != nil {
			t.Errorf("disarmed, tier %s write=%v: err = %v, want nil", c.tier, c.write, err)
		}
	}
	boom := errors.New("jobs catalogue unreadable")
	if err := checkRestampPrices1mRetention(ctx, fakePrices1mRetention{err: boom}, restampTierExact, true); !errors.Is(err, boom) {
		t.Errorf("an unreadable policy state must fail closed with its cause, got %v", err)
	}
}

// An in-place -write restamp refuses to start beside another -write run,
// exactly as a chunk run does, and releases the lock when it finishes.
func TestHoldInPlaceRestampLock(t *testing.T) {
	ctx := context.Background()

	held := newFakeChunkStore(nil)
	held.lockHeld = true
	if _, err := holdInPlaceRestampLock(ctx, held, true, false, io.Discard); !errors.Is(err, timescale.ErrUSDVolumeRestampLockHeld) {
		t.Fatalf("in-place -write beside a live run: err = %v, want ErrUSDVolumeRestampLockHeld", err)
	}

	free := newFakeChunkStore(nil)
	finish, err := holdInPlaceRestampLock(ctx, free, true, false, io.Discard)
	if err != nil {
		t.Fatalf("in-place -write: %v", err)
	}
	if free.index("lock") < 0 || free.index("unlock") >= 0 {
		t.Fatalf("lock not held for the walk:\n%s", strings.Join(free.log, "\n"))
	}
	runErr := errors.New("walk failed")
	if got := finish(runErr); !errors.Is(got, runErr) {
		t.Fatalf("finish dropped the run error: %v", got)
	}
	if free.index("unlock") < 0 {
		t.Fatalf("finish did not release the lock:\n%s", strings.Join(free.log, "\n"))
	}

	for _, tc := range []struct {
		name          string
		write, chunks bool
	}{{"dry run", false, false}, {"chunk run (locks in beginChunkWriteRun)", true, true}} {
		s := newFakeChunkStore(nil)
		s.lockHeld = true
		finish, err := holdInPlaceRestampLock(ctx, s, tc.write, tc.chunks, io.Discard)
		if err != nil || finish(nil) != nil || len(s.log) != 0 {
			t.Fatalf("%s: err = %v, store log = %v; want no lock taken", tc.name, err, s.log)
		}
	}
}

// One-writer contract for a `trades` re-stamp: usd_volume's live writer is the indexer at the ledgerstream cursor, so a
// restamp is legitimate only BEHIND it; overlap lets one derive_generation silently beat the other on a money column.
func TestCheckRestampLiveOverlap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		haveLive  bool
		live, top uint32
		allow     bool
		wantErr   string
	}{
		{name: "tail well past the window", haveLive: true, live: 64_200_000, top: 61_000_000},
		{name: "tail exactly at the window top", haveLive: true, live: 61_000_000, top: 61_000_000},
		{
			name: "tail one ledger short", haveLive: true, live: 60_999_999, top: 61_000_000,
			wantErr: "live ledgerstream cursor is 60999999",
		},
		{
			name: "indexer resyncing far behind", haveLive: true, live: 1_000, top: 61_000_000,
			wantErr: "below the top of the requested window",
		},
		{
			name: "no cursor at all is a refusal", haveLive: false, top: 61_000_000,
			wantErr: "the indexer has never run",
		},
		{name: "explicit override", haveLive: true, live: 1_000, top: 61_000_000, allow: true},
		{name: "window holds no on-chain rows", haveLive: false, top: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkRestampLiveOverlap(tc.haveLive, tc.live, tc.top, tc.allow)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("err = nil, want a refusal containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "-allow-live-overlap") {
				t.Errorf("the refusal must name its override; got %v", err)
			}
		})
	}
}

// TestValidateRestampTierFlags: an unknown -tier is rejected, and an xlm-base-only flag with
// -tier exact is an ERROR, not a silent no-op; ignoring -report or -min-rel-delta would mislead
// the operator into a write run or full rewrite on a money column.
func caseValidateRestampTierFlags(t *testing.T) {
	t.Parallel()
	if err := validateRestampTierFlags(restampTierExact, nil); err != nil {
		t.Fatalf("bare -tier exact: %v", err)
	}
	if err := validateRestampTierFlags(restampTierXLMBase, map[string]bool{"report": true, "batch": true}); err != nil {
		t.Fatalf("-tier xlm-base with its own flags: %v", err)
	}
	err := validateRestampTierFlags("estimated", nil)
	if err == nil || !strings.Contains(err.Error(), `want one of "exact", "xlm-base", "xlm-quote", "cex-fx"`) {
		t.Fatalf("unknown tier: err = %v", err)
	}
	for _, f := range restampEstimatedOnlyFlags {
		err := validateRestampTierFlags(restampTierExact, map[string]bool{f: true})
		if err == nil || !strings.Contains(err.Error(), "-"+f) {
			t.Errorf("-tier exact with -%s: err = %v, want a refusal naming the flag", f, err)
		}
	}
}

// The write filter is a fraction and "no filter" is the default: the anchor re-derives the correct value, so leaving a
// money row 0.5% wrong for tidiness is not the default posture.
func TestParseMinRelDelta(t *testing.T) {
	t.Parallel()
	for _, empty := range []string{"", "  ", "0", "0.0"} {
		got, err := parseMinRelDelta(empty)
		if err != nil || got != nil {
			t.Errorf("parseMinRelDelta(%q) = %v, %v; want nil, nil", empty, got, err)
		}
	}
	got, err := parseMinRelDelta("0.01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Cmp(big.NewRat(1, 100)) != 0 {
		t.Errorf("parseMinRelDelta(0.01) = %s, want 1/100", got.RatString())
	}
	if _, err := parseMinRelDelta("-0.01"); err == nil {
		t.Error("a negative -min-rel-delta must be refused")
	}
	if _, err := parseMinRelDelta("one percent"); err == nil {
		t.Error("a non-numeric -min-rel-delta must be refused")
	}
}

// TestResumeMinRelDeltaRoundTrips: the RESUME line's -min-rel-delta must
// parse back to the exact threshold the run used, or the resumed half
// filters a different write set from the half it continues.
func TestResumeMinRelDeltaRoundTrips(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"0.001", "0.0000004", "0.0012345", "1/3", "2", "0.1234567890123"} {
		want, err := parseMinRelDelta(in)
		if err != nil || want == nil {
			t.Fatalf("parseMinRelDelta(%q) = %v, %v", in, want, err)
		}
		flags := xlmBaseResumeFlags(xlmBaseRestampOptions{Slice: time.Hour, MinRelDelta: want})
		_, val, ok := strings.Cut(flags, "-min-rel-delta ")
		if !ok {
			t.Fatalf("%q: RESUME flags %q carry no -min-rel-delta", in, flags)
		}
		val, _, _ = strings.Cut(val, " ")
		got, err := parseMinRelDelta(val)
		if err != nil {
			t.Fatalf("%q: RESUME value %q does not parse: %v", in, val, err)
		}
		if got == nil || got.Cmp(want) != 0 {
			t.Errorf("%q: RESUME renders -min-rel-delta %q, which parses to %v; want exactly %s", in, val, got, want.RatString())
		}
	}
}

// TestRatPercent renders the report's percentages without float math
// (ADR-0003 keeps money-adjacent arithmetic out of float space).
func TestRatPercent(t *testing.T) {
	t.Parallel()
	if got := ratPercent(nil); got != "0%" {
		t.Errorf("ratPercent(nil) = %q, want 0%%", got)
	}
	if got := ratPercent(big.NewRat(1, 100)); got != "1.0000%" {
		t.Errorf("ratPercent(1/100) = %q, want 1.0000%%", got)
	}
	if got := ratPercent(big.NewRat(1, 3)); got != "33.3333%" {
		t.Errorf("ratPercent(1/3) = %q, want 33.3333%%", got)
	}
}

// TestRestampFlagValidation groups the restamp flag-combination checks as named subtests.
func TestRestampFlagValidation(t *testing.T) {
	t.Run("ValidateRestampTierFlags", caseValidateRestampTierFlags)
	t.Run("ValidateRestampTierFlags_ChunkModeIsAvailableToBothTiers", caseValidateRestampTierFlags_ChunkModeIsAvailableToBothTiers)
	t.Run("ValidateRestampTierFlags_MirrorTiers", caseValidateRestampTierFlags_MirrorTiers)
	t.Run("ValidateRestampChunkFlags", caseValidateRestampChunkFlags)
}

// The driver is pinned by the xlm-base tests and per-row decisions in the storage package; here the xlm-quote and cex-fx
// tiers drive the same walk (bracketed chunks, writes only through the guarded in-chunk apply, run generation carried)
// and each names itself in header, summary and RESUME line.

// Mirror tiers' seam double: reuses the driver double for chunk/policy/lock and answers the shared row-list plan/apply
// pair; dirty-row bookkeeping (planned until an apply takes it) makes "a rerun skips finished chunks" observable without
// a database.
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

// Checks the property both mirror tiers inherit from the driver: every chunk bracketed once in range order, with every
// write inside its own bracket and routed through the guarded in-chunk apply at the chunk batch.
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
	// The xlm-base tier's own measured flag guidance is not this tier's.
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

// A stopped run prints the command that continues IT: same tier, same generation, same population flags. A RESUME line
// naming the wrong tier would send the operator to restamp a different 18M rows.
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

// Flag contract for the mirror tiers: they take the estimated tiers' flags, `-tier exact` still refuses them, `-fx-max-
// staleness` belongs to cex-fx alone, and an unknown tier names every accepted spelling. A flag silently ignored on a
// money-column writer is the failure this closes.
func caseValidateRestampTierFlags_MirrorTiers(t *testing.T) {
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

// The as-of tolerance may be narrowed, never widened (a quote the live insert path declines as stale must never become a
// backfilled value), and never zeroed, which would refuse every row while reporting them unpriceable.
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

// printReport (usd_volume_restamp_xlmbase.go) is the shared report chokepoint every estimated tier finishes through; its
// scan/decline labels are templated on the running tier, so the header must not hard-code "tier xlm-base" or an xlm-
// quote/cex-fx banner shows the wrong tier.
func TestPrintReport_NamesTheRunningTierNotTheXLMBaseOne(t *testing.T) {
	store := newFakeMirrorChunkStore(nil)
	run := newEstimatedRestampRun(xlmBaseRestampOptions{Slice: time.Hour, Batch: 1}, xlmQuoteTierProfile(store))

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	orig := os.Stdout
	os.Stdout = w
	run.printReport(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	os.Stdout = orig
	_ = w.Close()
	got := <-done
	_ = r.Close()

	if !strings.Contains(got, "REPORT — tier xlm-quote —") {
		t.Errorf("xlm-quote report header does not name its own tier:\n%s", got)
	}
	if strings.Contains(got, "REPORT — tier xlm-base —") {
		t.Errorf("xlm-quote report header still hard-codes the xlm-base tier:\n%s", got)
	}
}
