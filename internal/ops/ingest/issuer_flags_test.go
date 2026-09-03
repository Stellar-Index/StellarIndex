package ingest

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The `issuer-flags` drain against merged issuers (#374).
//
// Reading only LIVE AccountEntry rows left every issuer that has merged its
// account away permanently unresolved — r1 2026-09-03: 10,239 of 59,241, and
// of the first 1,000 by primary key, 985 are merged accounts (a `removed` row
// in the current-state projection), 1 is live again, 14 are below the
// projection's floor. The lake CAN recover the merged ones: the same probe
// resolved 985/985 pre-images out of their 782 removal ledgers in 0.874s,
// and 979 of those pre-images still carry the account's self-declared
// home_domain (`stellarkraken.com`, `stellarbrunch.com`, …) — the identity
// claim that must NOT ride along with the flags.
//
// Real r1 keys and ledgers are used as fixtures throughout.
const (
	// Merged away at ledger 54,564,588; pre-image flags 0, and the dead
	// account still declares `stellarbrunch.com`.
	mergedIssuerA       = "GA2PQOJ26IP24ECRXEZ4BE6BEIB4HNDWSA2E6JVPFIP6KO6BKOEAZ6XW"
	mergedIssuerALedger = uint32(54564588)
	// Merged away at ledger 56,082,413; pre-image flags 10 =
	// AUTH_REVOCABLE|AUTH_CLAWBACK, declaring `xcrypto.exchange`.
	mergedIssuerB       = "GA2P3HKTQFBZBWOWPLESMV5AEHJLWJKNAYV5HLXOPBRWUEHFR64FQTKN"
	mergedIssuerBLedger = uint32(56082413)
	// Unresolved on r1 but LIVE in the lake at ledger 64,228,661 — an
	// account re-created at an address that had been merged.
	liveIssuer       = "GAEVD52W5E4Q2KTVQXC76ZSZYBEXXR3GGQZAIEP6BW3JMBTUBCHRY6UM"
	liveIssuerLedger = uint32(64228661)
	// Merged before the current-state projection's floor (r1: no `removed`
	// row below ledger 38,000,000), so neither reader can see it.
	absentIssuer = "GBDHN5EGVGFT5YSBUCLMZJKPUKLR4KHYS2AVMHJXAFPBBXR7KE7BF7YW"
)

// stubIssuerFlagsStore records what the drain asked for and what it wrote.
type stubIssuerFlagsStore struct {
	needFlags   []string
	needRecheck []string
	persisted   [][]timescale.IssuerAuthFlags
	persistErr  error

	flagsLimit   int
	recheckLimit int
}

func (s *stubIssuerFlagsStore) IssuerGStrkeysNeedingFlags(_ context.Context, limit int) ([]string, error) {
	s.flagsLimit = limit
	return s.needFlags, nil
}

func (s *stubIssuerFlagsStore) IssuerGStrkeysNeedingRecheck(_ context.Context, limit int) ([]string, error) {
	s.recheckLimit = limit
	return s.needRecheck, nil
}

func (s *stubIssuerFlagsStore) PersistIssuerAuthFlags(_ context.Context, flags []timescale.IssuerAuthFlags) (int, error) {
	if s.persistErr != nil {
		return 0, s.persistErr
	}
	cp := append([]timescale.IssuerAuthFlags(nil), flags...)
	s.persisted = append(s.persisted, cp)
	return len(cp), nil
}

// allPersisted flattens every persist batch into one g_strkey-keyed map.
func (s *stubIssuerFlagsStore) allPersisted() map[string]timescale.IssuerAuthFlags {
	out := map[string]timescale.IssuerAuthFlags{}
	for _, batch := range s.persisted {
		for _, f := range batch {
			out[f.GStrkey] = f
		}
	}
	return out
}

// stubIssuerFlagsReader answers from two fixed maps and records the exact
// key slice each reader was handed — the fallback must only ever be offered
// the MISSES, never a key the live reader already answered.
type stubIssuerFlagsReader struct {
	live      map[string]clickhouse.AccountAuthFlags
	lastKnown map[string]clickhouse.AccountAuthFlags

	liveCalls      [][]string
	lastKnownCalls [][]string
	lastKnownErr   error
}

func (r *stubIssuerFlagsReader) BulkAccountAuthFlags(_ context.Context, gs []string) (map[string]clickhouse.AccountAuthFlags, error) {
	r.liveCalls = append(r.liveCalls, append([]string(nil), gs...))
	return pick(r.live, gs), nil
}

func (r *stubIssuerFlagsReader) RemovedAccountsLastKnownAuthFlags(_ context.Context, gs []string) (map[string]clickhouse.AccountAuthFlags, error) {
	r.lastKnownCalls = append(r.lastKnownCalls, append([]string(nil), gs...))
	if r.lastKnownErr != nil {
		return nil, r.lastKnownErr
	}
	return pick(r.lastKnown, gs), nil
}

func pick(src map[string]clickhouse.AccountAuthFlags, gs []string) map[string]clickhouse.AccountAuthFlags {
	out := map[string]clickhouse.AccountAuthFlags{}
	for _, g := range gs {
		if f, ok := src[g]; ok {
			out[g] = f
		}
	}
	return out
}

func liveReading(ledger uint32, flags uint32, domain string) clickhouse.AccountAuthFlags {
	return clickhouse.AccountAuthFlags{
		Required:   flags&0x1 != 0,
		Revocable:  flags&0x2 != 0,
		Immutable:  flags&0x4 != 0,
		Clawback:   flags&0x8 != 0,
		HomeDomain: domain,
		Source:     clickhouse.AuthFlagsSourceLive,
		AsOfLedger: ledger,
	}
}

// lastKnownReading mirrors what RemovedAccountsLastKnownAuthFlags returns:
// the pre-image's flags, the removal ledger, and NO home_domain.
func lastKnownReading(ledger uint32, flags uint32) clickhouse.AccountAuthFlags {
	return clickhouse.AccountAuthFlags{
		Required:   flags&0x1 != 0,
		Revocable:  flags&0x2 != 0,
		Immutable:  flags&0x4 != 0,
		Clawback:   flags&0x8 != 0,
		Source:     clickhouse.AuthFlagsSourceLastKnownBeforeRemoval,
		AsOfLedger: ledger,
	}
}

func runOpts() issuerFlagsOpts {
	return issuerFlagsOpts{limit: 5000, batch: 500, out: io.Discard}
}

// TestRunIssuerFlags_RecoversMergedIssuersWithProvenance is the central
// case: the drain must fall through to the last-known reader for the misses,
// persist what it finds WITH its provenance, and count the three outcomes
// apart. Before this change a merged issuer was counted `absent` and never
// read a second time, so all 10,239 stayed unresolved for good.
func TestRunIssuerFlags_RecoversMergedIssuersWithProvenance(t *testing.T) {
	store := &stubIssuerFlagsStore{needFlags: []string{mergedIssuerA, liveIssuer, mergedIssuerB, absentIssuer}}
	reader := &stubIssuerFlagsReader{
		live: map[string]clickhouse.AccountAuthFlags{
			liveIssuer: liveReading(liveIssuerLedger, 0, "congress-card.org"),
		},
		lastKnown: map[string]clickhouse.AccountAuthFlags{
			mergedIssuerA: lastKnownReading(mergedIssuerALedger, 0),
			mergedIssuerB: lastKnownReading(mergedIssuerBLedger, 0xA),
		},
	}
	if err := runIssuerFlags(context.Background(), store, reader, runOpts()); err != nil {
		t.Fatalf("runIssuerFlags: %v", err)
	}

	got := store.allPersisted()
	if len(got) != 3 {
		t.Fatalf("persisted %d row(s), want 3 (2 merged + 1 live; the pre-floor one is absent): %v", len(got), got)
	}

	a := got[mergedIssuerA]
	if a.Source != timescale.AuthFlagsSourceLastKnownBeforeRemoval {
		t.Errorf("%s source = %q, want %q", mergedIssuerA, a.Source, timescale.AuthFlagsSourceLastKnownBeforeRemoval)
	}
	if a.AsOfLedger == nil || *a.AsOfLedger != mergedIssuerALedger {
		t.Errorf("%s as-of = %v, want %d (its removal ledger)", mergedIssuerA, a.AsOfLedger, mergedIssuerALedger)
	}
	if a.HomeDomain != "" {
		t.Errorf("%s home_domain = %q, want empty — a merged account's self-declared identity is not persistable", mergedIssuerA, a.HomeDomain)
	}

	// Flags 10 = AUTH_REVOCABLE|AUTH_CLAWBACK must survive the recovery
	// intact: the whole point is the VALUE, not merely a non-null row.
	b := got[mergedIssuerB]
	if b.Required || !b.Revocable || b.Immutable || !b.Clawback {
		t.Errorf("%s flags = (req=%v rev=%v imm=%v claw=%v), want (false true false true) from mask 0xA",
			mergedIssuerB, b.Required, b.Revocable, b.Immutable, b.Clawback)
	}
	if b.AsOfLedger == nil || *b.AsOfLedger != mergedIssuerBLedger {
		t.Errorf("%s as-of = %v, want %d", mergedIssuerB, b.AsOfLedger, mergedIssuerBLedger)
	}

	l := got[liveIssuer]
	if l.Source != timescale.AuthFlagsSourceLive {
		t.Errorf("%s source = %q, want %q", liveIssuer, l.Source, timescale.AuthFlagsSourceLive)
	}
	if l.AsOfLedger == nil || *l.AsOfLedger != liveIssuerLedger {
		t.Errorf("%s as-of = %v, want %d", liveIssuer, l.AsOfLedger, liveIssuerLedger)
	}
	if l.HomeDomain != "congress-card.org" {
		t.Errorf("%s home_domain = %q, want it carried through — a LIVE account's domain is still checkable", liveIssuer, l.HomeDomain)
	}

	if _, ok := got[absentIssuer]; ok {
		t.Errorf("%s was persisted, but neither reader resolved it", absentIssuer)
	}
}

// TestRunIssuerFlags_FallbackIsOfferedOnlyTheMisses — the fallback must never
// be handed a key the live reader already answered. Offering the whole chunk
// would widen a partition-pruned read over the 150-billion-row changes log
// for no gain, and would put a live account one code change away from being
// resolved to its own pre-image.
func TestRunIssuerFlags_FallbackIsOfferedOnlyTheMisses(t *testing.T) {
	store := &stubIssuerFlagsStore{needFlags: []string{mergedIssuerA, liveIssuer, absentIssuer}}
	reader := &stubIssuerFlagsReader{
		live: map[string]clickhouse.AccountAuthFlags{
			liveIssuer: liveReading(liveIssuerLedger, 0, ""),
		},
		lastKnown: map[string]clickhouse.AccountAuthFlags{
			mergedIssuerA: lastKnownReading(mergedIssuerALedger, 0),
		},
	}
	if err := runIssuerFlags(context.Background(), store, reader, runOpts()); err != nil {
		t.Fatalf("runIssuerFlags: %v", err)
	}
	if len(reader.lastKnownCalls) != 1 {
		t.Fatalf("last-known reader called %d time(s), want 1", len(reader.lastKnownCalls))
	}
	want := []string{mergedIssuerA, absentIssuer}
	got := reader.lastKnownCalls[0]
	if len(got) != len(want) {
		t.Fatalf("fallback keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fallback keys = %v, want %v (misses only, chunk order)", got, want)
		}
	}
}

// TestRunIssuerFlags_LiveEntryOutranksAStalePreImage — an account re-created
// at an address that was once merged is LIVE, and a lake that still returns
// its pre-image must not win. Ordering, not deduplication, is what guarantees
// this: the fallback is only ever offered the misses.
func TestRunIssuerFlags_LiveEntryOutranksAStalePreImage(t *testing.T) {
	store := &stubIssuerFlagsStore{needFlags: []string{liveIssuer}}
	reader := &stubIssuerFlagsReader{
		live: map[string]clickhouse.AccountAuthFlags{
			liveIssuer: liveReading(liveIssuerLedger, 0, "congress-card.org"),
		},
		// Deliberately ALSO answerable from the changes log, as a lake with
		// a stale `removed` row would be.
		lastKnown: map[string]clickhouse.AccountAuthFlags{
			liveIssuer: lastKnownReading(54564497, 0xF),
		},
	}
	if err := runIssuerFlags(context.Background(), store, reader, runOpts()); err != nil {
		t.Fatalf("runIssuerFlags: %v", err)
	}
	got := store.allPersisted()[liveIssuer]
	if got.Source != timescale.AuthFlagsSourceLive {
		t.Errorf("source = %q, want %q — the live AccountEntry is the authority on its own account",
			got.Source, timescale.AuthFlagsSourceLive)
	}
	if got.AsOfLedger == nil || *got.AsOfLedger != liveIssuerLedger {
		t.Errorf("as-of = %v, want %d (the live entry's ledger, not the stale removal ledger)", got.AsOfLedger, liveIssuerLedger)
	}
	if got.Required || got.Revocable || got.Immutable || got.Clawback {
		t.Errorf("flags = (%v %v %v %v), want all false — the pre-image's 0xF must not win",
			got.Required, got.Revocable, got.Immutable, got.Clawback)
	}
}

// TestRunIssuerFlags_RecheckRevivesARecreatedIssuer — the re-check pass is
// what stops the provenance column being a one-way latch. A
// `last_known_before_removal` row has auth_required SET, so the primary
// queue (`auth_required IS NULL`) can never see it again; without this pass a
// re-created issuer would serve its pre-removal flags for good.
func TestRunIssuerFlags_RecheckRevivesARecreatedIssuer(t *testing.T) {
	store := &stubIssuerFlagsStore{
		needRecheck: []string{mergedIssuerA, liveIssuer},
	}
	reader := &stubIssuerFlagsReader{
		live: map[string]clickhouse.AccountAuthFlags{
			// Re-created since the row was written, with a flag SET.
			liveIssuer: liveReading(liveIssuerLedger, 0x1, "congress-card.org"),
		},
	}
	if err := runIssuerFlags(context.Background(), store, reader, runOpts()); err != nil {
		t.Fatalf("runIssuerFlags: %v", err)
	}

	got := store.allPersisted()
	if len(got) != 1 {
		t.Fatalf("persisted %d row(s), want exactly 1 — a still-merged row has not changed and must not be rewritten: %v", len(got), got)
	}
	rev := got[liveIssuer]
	if rev.Source != timescale.AuthFlagsSourceLive {
		t.Errorf("source = %q, want %q — the re-created account is live again", rev.Source, timescale.AuthFlagsSourceLive)
	}
	if rev.AsOfLedger == nil || *rev.AsOfLedger != liveIssuerLedger {
		t.Errorf("as-of = %v, want %d", rev.AsOfLedger, liveIssuerLedger)
	}
	if !rev.Required {
		t.Errorf("auth_required = false, want true — the LIVE flags must replace the pre-removal ones")
	}
	if _, ok := got[mergedIssuerA]; ok {
		t.Errorf("%s was rewritten, but it is still merged and nothing about it changed", mergedIssuerA)
	}
}

// TestRunIssuerFlags_RecheckQueueIsBounded — the re-check queue is bounded by
// the same -limit as the primary queue, so a run cannot silently become
// unbounded work on a 10k-row residue.
func TestRunIssuerFlags_RecheckQueueIsBounded(t *testing.T) {
	store := &stubIssuerFlagsStore{}
	reader := &stubIssuerFlagsReader{}
	o := runOpts()
	o.limit = 250
	if err := runIssuerFlags(context.Background(), store, reader, o); err != nil {
		t.Fatalf("runIssuerFlags: %v", err)
	}
	if store.flagsLimit != 250 {
		t.Errorf("primary queue limit = %d, want 250", store.flagsLimit)
	}
	if store.recheckLimit != 250 {
		t.Errorf("re-check queue limit = %d, want 250 — an unbounded second pass would defeat -limit", store.recheckLimit)
	}
}

// TestRunIssuerFlags_DryRunWritesNothing — the write gate covers BOTH passes.
func TestRunIssuerFlags_DryRunWritesNothing(t *testing.T) {
	store := &stubIssuerFlagsStore{
		needFlags:   []string{mergedIssuerA},
		needRecheck: []string{liveIssuer},
	}
	reader := &stubIssuerFlagsReader{
		live:      map[string]clickhouse.AccountAuthFlags{liveIssuer: liveReading(liveIssuerLedger, 0, "")},
		lastKnown: map[string]clickhouse.AccountAuthFlags{mergedIssuerA: lastKnownReading(mergedIssuerALedger, 0)},
	}
	o := runOpts()
	o.dryRun = true
	if err := runIssuerFlags(context.Background(), store, reader, o); err != nil {
		t.Fatalf("runIssuerFlags: %v", err)
	}
	if len(store.persisted) != 0 {
		t.Errorf("dry run persisted %d batch(es), want 0", len(store.persisted))
	}
}

// TestRunIssuerFlags_FallbackErrorIsFatal — a lake read that FAILS is not the
// same as one that finds nothing. Swallowing it would count real merged
// issuers as `absent` and quietly reproduce the defect this fixes.
func TestRunIssuerFlags_FallbackErrorIsFatal(t *testing.T) {
	boom := errors.New("clickhouse: connection reset")
	store := &stubIssuerFlagsStore{needFlags: []string{mergedIssuerA}}
	reader := &stubIssuerFlagsReader{lastKnownErr: boom}
	err := runIssuerFlags(context.Background(), store, reader, runOpts())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the reader's error", err)
	}
	if len(store.persisted) != 0 {
		t.Errorf("persisted %d batch(es) after a failed read, want 0", len(store.persisted))
	}
}
