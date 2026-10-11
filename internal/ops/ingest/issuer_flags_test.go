package ingest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Fixtures for the issuer-flags drain against merged issuers: merged accounts (a removed row in the current-state projection)
// are recoverable from their removal-ledger pre-images, but the self-declared home_domain in those pre-images must NOT ride along with the flags.
// Real r1 keys and ledgers are used throughout.
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
	needFlags     []string
	needRecheck   []string
	needChainRead []timescale.IssuerAuthFlagsOnRecord
	persisted     [][]timescale.IssuerAuthFlags
	persistErr    error

	flagsLimit        int
	recheckLimit      int
	chainRecheckLimit int
}

func (s *stubIssuerFlagsStore) IssuerGStrkeysNeedingFlags(_ context.Context, limit int) ([]string, error) {
	s.flagsLimit = limit
	return s.needFlags, nil
}

func (s *stubIssuerFlagsStore) IssuerGStrkeysNeedingRecheck(_ context.Context, limit int) ([]string, error) {
	s.recheckLimit = limit
	return s.needRecheck, nil
}

func (s *stubIssuerFlagsStore) IssuersNeedingChainRecheck(_ context.Context, limit int) ([]timescale.IssuerAuthFlagsOnRecord, error) {
	s.chainRecheckLimit = limit
	return s.needChainRead, nil
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

// TestRunIssuerFlags_RecoversMergedIssuersWithProvenance: misses fall through to the last-known reader, persist with provenance, and the three outcomes are counted apart (merged is not absent).
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

// TestRunIssuerFlags_FallbackIsOfferedOnlyTheMisses: the fallback is never handed a key the live reader answered, or a live account could resolve to its own pre-image.
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

// TestRunIssuerFlags_LiveEntryOutranksAStalePreImage: an account re-created at a once-merged address is live; ordering (fallback sees only misses) guarantees its pre-image never wins.
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

// TestRunIssuerFlags_RecheckRevivesARecreatedIssuer: the re-check pass keeps provenance from being a one-way latch, since last_known_before_removal rows never re-enter the primary queue.
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

// TestRunIssuerFlags_RecheckQueueIsBounded: the re-check queue honours the same -limit as the primary queue.
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

// TestRunIssuerFlags_FallbackErrorIsFatal: a failed lake read is not an empty one; swallowing it would count merged issuers as absent.
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

// deadlineIssuerFlagsReader answers the first `answer` live reads, then blocks
// every later one until its context ends and fails with the context's error,
// as a ClickHouse query cut off by -timeout does.
type deadlineIssuerFlagsReader struct {
	stubIssuerFlagsReader
	answer int
}

func (r *deadlineIssuerFlagsReader) BulkAccountAuthFlags(ctx context.Context, gs []string) (map[string]clickhouse.AccountAuthFlags, error) {
	if len(r.liveCalls) < r.answer {
		return r.stubIssuerFlagsReader.BulkAccountAuthFlags(ctx, gs)
	}
	r.liveCalls = append(r.liveCalls, append([]string(nil), gs...))
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestRunIssuerFlags_DeadlineMidReadIsAGracefulStop: a timeout inside a lake read is the same resumable stop as between batches.
func TestRunIssuerFlags_DeadlineMidReadIsAGracefulStop(t *testing.T) {
	store := &stubIssuerFlagsStore{needFlags: []string{liveIssuer, mergedIssuerA}}
	reader := &deadlineIssuerFlagsReader{stubIssuerFlagsReader{
		live: map[string]clickhouse.AccountAuthFlags{liveIssuer: liveReading(liveIssuerLedger, 0, "")},
	}, 1}
	out := runUntilDeadline(t, store, reader)
	if got := store.allPersisted(); len(got) != 1 || got[liveIssuer].GStrkey != liveIssuer {
		t.Errorf("persisted %v, want only the batch read before the deadline", got)
	}
	if !strings.Contains(out, "stopping early (queue is resumable)") {
		t.Errorf("output does not report the early stop:\n%s", out)
	}
}

// TestRunIssuerFlags_CancelMidReadIsFatal: only the run's own deadline is a graceful stop; a cancelled context is not swallowed.
func TestRunIssuerFlags_CancelMidReadIsFatal(t *testing.T) {
	store := &stubIssuerFlagsStore{needFlags: []string{liveIssuer, mergedIssuerA}}
	reader := &deadlineIssuerFlagsReader{}
	o := runOpts()
	o.batch = 1

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	if err := runIssuerFlags(ctx, store, reader, o); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// runUntilDeadline runs the drain with batch 1 under a short deadline and
// returns its output; the deadline must be a graceful stop.
func runUntilDeadline(t *testing.T, store *stubIssuerFlagsStore, reader issuerFlagsReader) string {
	t.Helper()
	var out bytes.Buffer
	o := runOpts()
	o.batch = 1
	o.out = &out

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := runIssuerFlags(ctx, store, reader, o); err != nil {
		t.Fatalf("runIssuerFlags: %v — a deadline mid-read must stop the run gracefully", err)
	}
	return out.String()
}

// TestRunIssuerFlags_DeadlineMidRecheckCountsOnlyReadRows: a batch cut off by the deadline must not be reported as examined.
func TestRunIssuerFlags_DeadlineMidRecheckCountsOnlyReadRows(t *testing.T) {
	store := &stubIssuerFlagsStore{needRecheck: []string{mergedIssuerA, mergedIssuerB}}
	out := runUntilDeadline(t, store, &deadlineIssuerFlagsReader{answer: 1})

	want := "re-check processed 1 of 2 last-known row(s) — revived_to_live=0 still_merged=1"
	if !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
}

// TestRunIssuerFlags_DeadlineMidChainRecheckCountsOnlyReadRows: every examined row lands in exactly one of corrected/agreed/unread, even on a cut-off run.
func TestRunIssuerFlags_DeadlineMidChainRecheckCountsOnlyReadRows(t *testing.T) {
	store := &stubIssuerFlagsStore{needChainRead: []timescale.IssuerAuthFlagsOnRecord{
		onRecord(absentIssuer, 0, "somewhere.example", timescale.AuthFlagsSourceLive, 64100000),
		onRecord(mergedIssuerA, 0, "stellarbrunch.com", timescale.AuthFlagsSourceLive, 50000000),
	}}
	out := runUntilDeadline(t, store, &deadlineIssuerFlagsReader{answer: 1})

	want := "chain re-check processed 1 of 2 filled row(s) — corrected=0 agreed=0 unread=1"
	if !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
}

// Fixtures for the chain re-check pass: a filled, live-sourced row is invisible to the primary queue and the merged re-check,
// so a lapsed home_domain would keep being fetched by SEP-1 refresh and could be inherited by whoever registers it next.
// Real keys: the ex-apay ETH issuer moved to ultracapital.xyz.
const (
	// A filled, live-sourced row on r1 whose stored domain has lapsed.
	lapsedDomainIssuer       = "GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55"
	lapsedDomainIssuerLedger = uint32(64228661)
)

// onRecord builds one persisted row the way the served tier holds it.
func onRecord(g string, flags uint32, domain, source string, asOf uint32) timescale.IssuerAuthFlagsOnRecord {
	req, rev, imm, claw := flags&0x1 != 0, flags&0x2 != 0, flags&0x4 != 0, flags&0x8 != 0
	rec := timescale.IssuerAuthFlagsOnRecord{
		GStrkey:    g,
		Required:   &req,
		Revocable:  &rev,
		Immutable:  &imm,
		Clawback:   &claw,
		HomeDomain: domain,
		Source:     source,
	}
	if asOf > 0 {
		l := asOf
		rec.AsOfLedger = &l
	}
	return rec
}

// TestRunIssuerFlags_ChainRecheckCorrectsALapsedHomeDomain: a filled live row whose account now declares a different domain must be re-read and corrected.
func TestRunIssuerFlags_ChainRecheckCorrectsALapsedHomeDomain(t *testing.T) {
	store := &stubIssuerFlagsStore{
		needChainRead: []timescale.IssuerAuthFlagsOnRecord{
			onRecord(lapsedDomainIssuer, 0x1, "lapsed-former.example",
				timescale.AuthFlagsSourceLive, 64100000),
		},
	}
	reader := &stubIssuerFlagsReader{
		live: map[string]clickhouse.AccountAuthFlags{
			lapsedDomainIssuer: liveReading(lapsedDomainIssuerLedger, 0x1, "ultracapital.xyz"),
		},
	}
	if err := runIssuerFlags(context.Background(), store, reader, runOpts()); err != nil {
		t.Fatalf("runIssuerFlags: %v", err)
	}

	got, ok := store.allPersisted()[lapsedDomainIssuer]
	if !ok {
		t.Fatalf("the filled row was never re-read; persisted = %v — a lapsed domain is only "+
			"correctable on-chain if something re-offers a row the drain has already filled",
			store.allPersisted())
	}
	if got.HomeDomain != "ultracapital.xyz" {
		t.Errorf("home_domain = %q, want ultracapital.xyz — the account's own current entry, "+
			"not the copy taken before it moved", got.HomeDomain)
	}
	if got.Source != timescale.AuthFlagsSourceLive {
		t.Errorf("source = %q, want %q", got.Source, timescale.AuthFlagsSourceLive)
	}
	if got.AsOfLedger == nil || *got.AsOfLedger != lapsedDomainIssuerLedger {
		t.Errorf("as-of = %v, want %d (the entry read now, not the one on record)",
			got.AsOfLedger, lapsedDomainIssuerLedger)
	}
}

// TestRunIssuerFlags_ChainRecheckWritesOnlyWhatTheChainChanged: re-offering every filled row is affordable only because an unchanged run writes nothing, and written means rows corrected.
func TestRunIssuerFlags_ChainRecheckWritesOnlyWhatTheChainChanged(t *testing.T) {
	const agreeing = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	store := &stubIssuerFlagsStore{
		needChainRead: []timescale.IssuerAuthFlagsOnRecord{
			onRecord(agreeing, 0x2, "aqua.network", timescale.AuthFlagsSourceLive, 64100000),
			onRecord(lapsedDomainIssuer, 0x1, "lapsed-former.example",
				timescale.AuthFlagsSourceLive, 64100000),
			// Not in the lake's current-state projection at all.
			onRecord(absentIssuer, 0, "somewhere.example", timescale.AuthFlagsSourceLive, 64100000),
		},
	}
	reader := &stubIssuerFlagsReader{
		live: map[string]clickhouse.AccountAuthFlags{
			agreeing:           liveReading(64100000, 0x2, "aqua.network"),
			lapsedDomainIssuer: liveReading(lapsedDomainIssuerLedger, 0x1, "ultracapital.xyz"),
		},
	}
	if err := runIssuerFlags(context.Background(), store, reader, runOpts()); err != nil {
		t.Fatalf("runIssuerFlags: %v", err)
	}

	got := store.allPersisted()
	if len(got) != 1 {
		t.Fatalf("persisted %d row(s), want exactly 1 — only the row the chain moved past: %v", len(got), got)
	}
	if _, ok := got[lapsedDomainIssuer]; !ok {
		t.Errorf("persisted %v, want the corrected row %s", got, lapsedDomainIssuer)
	}
	if _, ok := got[absentIssuer]; ok {
		t.Errorf("%s was rewritten, but the live reader did not answer for it — absence from the "+
			"current-state projection is a merged account AND a coverage gap, so this pass may not act on it",
			absentIssuer)
	}
}

// TestRunIssuerFlags_ChainRecheckClearsADomainTheChainNoLongerDeclares: an empty reading from a live entry is the account declaring none, so the pass must write it or the lapsed name stays queued.
func TestRunIssuerFlags_ChainRecheckClearsADomainTheChainNoLongerDeclares(t *testing.T) {
	store := &stubIssuerFlagsStore{
		needChainRead: []timescale.IssuerAuthFlagsOnRecord{
			onRecord(lapsedDomainIssuer, 0x1, "lapsed-former.example",
				timescale.AuthFlagsSourceLive, 64100000),
		},
	}
	reader := &stubIssuerFlagsReader{
		live: map[string]clickhouse.AccountAuthFlags{
			lapsedDomainIssuer: liveReading(64100000, 0x1, ""),
		},
	}
	if err := runIssuerFlags(context.Background(), store, reader, runOpts()); err != nil {
		t.Fatalf("runIssuerFlags: %v", err)
	}
	got, ok := store.allPersisted()[lapsedDomainIssuer]
	if !ok {
		t.Fatalf("persisted %v, want %s rewritten — the chain no longer declares the stored domain",
			store.allPersisted(), lapsedDomainIssuer)
	}
	if got.Source != timescale.AuthFlagsSourceLive || got.HomeDomain != "" {
		t.Errorf("persisted source=%q home_domain=%q, want a live reading declaring none", got.Source, got.HomeDomain)
	}
}

// TestRunIssuerFlags_ChainRecheckLeavesAnUnchangedDeclaredNoneAlone: a row holding no domain agrees with a live entry declaring none, so nothing is written.
func TestRunIssuerFlags_ChainRecheckLeavesAnUnchangedDeclaredNoneAlone(t *testing.T) {
	store := &stubIssuerFlagsStore{
		needChainRead: []timescale.IssuerAuthFlagsOnRecord{
			onRecord(lapsedDomainIssuer, 0x1, "", timescale.AuthFlagsSourceLive, 64100000),
		},
	}
	reader := &stubIssuerFlagsReader{
		live: map[string]clickhouse.AccountAuthFlags{
			lapsedDomainIssuer: liveReading(64100000, 0x1, ""),
		},
	}
	if err := runIssuerFlags(context.Background(), store, reader, runOpts()); err != nil {
		t.Fatalf("runIssuerFlags: %v", err)
	}
	if got := store.allPersisted(); len(got) != 0 {
		t.Errorf("persisted %v, want nothing — the row already agrees with the chain", got)
	}
}

// TestRunIssuerFlags_ChainRecheckRelabelsAnIssuerThatMergedAfterFilling: a live-labelled row whose account merged must take the last-known reader's flags and no domain; a key neither reader answers stays untouched.
func TestRunIssuerFlags_ChainRecheckRelabelsAnIssuerThatMergedAfterFilling(t *testing.T) {
	store := &stubIssuerFlagsStore{
		needChainRead: []timescale.IssuerAuthFlagsOnRecord{
			onRecord(mergedIssuerA, 0, "stellarbrunch.com", timescale.AuthFlagsSourceLive, 50000000),
			onRecord(absentIssuer, 0, "somewhere.example", timescale.AuthFlagsSourceLive, 64100000),
		},
	}
	reader := &stubIssuerFlagsReader{
		lastKnown: map[string]clickhouse.AccountAuthFlags{
			mergedIssuerA: lastKnownReading(mergedIssuerALedger, 0),
		},
	}
	if err := runIssuerFlags(context.Background(), store, reader, runOpts()); err != nil {
		t.Fatalf("runIssuerFlags: %v", err)
	}
	persisted := store.allPersisted()
	got, ok := persisted[mergedIssuerA]
	if !ok {
		t.Fatalf("persisted %v, want %s relabelled — it merged after its row was filled", persisted, mergedIssuerA)
	}
	if got.Source != timescale.AuthFlagsSourceLastKnownBeforeRemoval || got.HomeDomain != "" ||
		got.AsOfLedger == nil || *got.AsOfLedger != mergedIssuerALedger {
		t.Errorf("persisted %+v, want a last-known reading as of %d with no home_domain", got, mergedIssuerALedger)
	}
	if _, ok := persisted[absentIssuer]; ok {
		t.Errorf("%s was rewritten, but neither reader answered for it", absentIssuer)
	}
}

// TestRunIssuerFlags_ChainRecheckClearsAMergedRowsStoredDomain: a last-known row still holding a domain stored while live is re-written to clear the identity.
func TestRunIssuerFlags_ChainRecheckClearsAMergedRowsStoredDomain(t *testing.T) {
	store := &stubIssuerFlagsStore{
		needChainRead: []timescale.IssuerAuthFlagsOnRecord{
			onRecord(mergedIssuerA, 0, "stellarbrunch.com",
				timescale.AuthFlagsSourceLastKnownBeforeRemoval, mergedIssuerALedger),
		},
	}
	reader := &stubIssuerFlagsReader{
		lastKnown: map[string]clickhouse.AccountAuthFlags{
			mergedIssuerA: lastKnownReading(mergedIssuerALedger, 0),
		},
	}
	if err := runIssuerFlags(context.Background(), store, reader, runOpts()); err != nil {
		t.Fatalf("runIssuerFlags: %v", err)
	}
	if got, ok := store.allPersisted()[mergedIssuerA]; !ok || got.HomeDomain != "" {
		t.Errorf("persisted %v, want %s re-written with no home_domain", store.allPersisted(), mergedIssuerA)
	}
}

// TestRunIssuerFlags_ChainRecheckFillsAnUnlabelledRow: a pre-migration row with flags but no provenance is written once and stamped, since unknown provenance is not live.
func TestRunIssuerFlags_ChainRecheckFillsAnUnlabelledRow(t *testing.T) {
	store := &stubIssuerFlagsStore{
		needChainRead: []timescale.IssuerAuthFlagsOnRecord{
			onRecord(lapsedDomainIssuer, 0x1, "ultracapital.xyz", "", 0),
		},
	}
	reader := &stubIssuerFlagsReader{
		live: map[string]clickhouse.AccountAuthFlags{
			lapsedDomainIssuer: liveReading(lapsedDomainIssuerLedger, 0x1, "ultracapital.xyz"),
		},
	}
	if err := runIssuerFlags(context.Background(), store, reader, runOpts()); err != nil {
		t.Fatalf("runIssuerFlags: %v", err)
	}
	got, ok := store.allPersisted()[lapsedDomainIssuer]
	if !ok {
		t.Fatalf("the unlabelled row was not stamped; persisted = %v", store.allPersisted())
	}
	if got.Source != timescale.AuthFlagsSourceLive {
		t.Errorf("source = %q, want %q — an absent label is UNKNOWN, never a claim that the "+
			"reading is current", got.Source, timescale.AuthFlagsSourceLive)
	}
}

// TestRunIssuerFlags_ChainRecheckHasItsOwnBound: the pass must not inherit -limit (primary-key order would re-read the same head forever); it has its own knob and runs last.
func TestRunIssuerFlags_ChainRecheckHasItsOwnBound(t *testing.T) {
	store := &stubIssuerFlagsStore{}
	reader := &stubIssuerFlagsReader{}
	o := runOpts()
	o.limit = 250
	o.chainRecheckLimit = 40
	if err := runIssuerFlags(context.Background(), store, reader, o); err != nil {
		t.Fatalf("runIssuerFlags: %v", err)
	}
	if store.flagsLimit != 250 {
		t.Errorf("primary queue limit = %d, want 250", store.flagsLimit)
	}
	if store.chainRecheckLimit != 40 {
		t.Errorf("chain re-check limit = %d, want 40 — the pass must be bounded independently of -limit",
			store.chainRecheckLimit)
	}
}

// TestRunIssuerFlags_ChainRecheckDryRunWritesNothing: the write gate covers the third pass too.
func TestRunIssuerFlags_ChainRecheckDryRunWritesNothing(t *testing.T) {
	store := &stubIssuerFlagsStore{
		needChainRead: []timescale.IssuerAuthFlagsOnRecord{
			onRecord(lapsedDomainIssuer, 0x1, "lapsed-former.example",
				timescale.AuthFlagsSourceLive, 64100000),
		},
	}
	reader := &stubIssuerFlagsReader{
		live: map[string]clickhouse.AccountAuthFlags{
			lapsedDomainIssuer: liveReading(lapsedDomainIssuerLedger, 0x1, "ultracapital.xyz"),
		},
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

// TestRunIssuerFlags_ChainRecheckStartsAtTheDaysBatch: each day's run starts one batch further on and wraps, so a timeout never leaves the same tail unexamined.
func TestRunIssuerFlags_ChainRecheckStartsAtTheDaysBatch(t *testing.T) {
	keys := []string{"GA", "GB", "GC", "GD", "GE"}
	recs := make([]timescale.IssuerAuthFlagsOnRecord, 0, len(keys))
	for _, g := range keys {
		recs = append(recs, onRecord(g, 0x1, "same.example", timescale.AuthFlagsSourceLive, 100))
	}
	// batch 2 over 5 rows = 3 batches; day d starts at row 2*(d mod 3) and wraps.
	for day, wantFirst := range map[int][]string{
		0: {"GA", "GB"},
		1: {"GC", "GD"},
		2: {"GE", "GA"},
		3: {"GA", "GB"},
	} {
		store := &stubIssuerFlagsStore{needChainRead: recs}
		reader := &stubIssuerFlagsReader{}
		o := runOpts()
		o.batch, o.day = 2, day
		if err := runIssuerFlags(context.Background(), store, reader, o); err != nil {
			t.Fatalf("day %d: runIssuerFlags: %v", day, err)
		}
		if len(reader.liveCalls) != 3 {
			t.Fatalf("day %d: %d live reads, want 3 (one per batch): %v", day, len(reader.liveCalls), reader.liveCalls)
		}
		if got := fmt.Sprint(reader.liveCalls[0]); got != fmt.Sprint(wantFirst) {
			t.Errorf("day %d: first batch read = %s, want %s", day, got, fmt.Sprint(wantFirst))
		}
		seen := map[string]int{}
		for _, call := range reader.liveCalls {
			for _, g := range call {
				seen[g]++
			}
		}
		for _, g := range keys {
			if seen[g] != 1 {
				t.Errorf("day %d: %s read %d time(s), want exactly 1 — the rotation must still cover every row", day, g, seen[g])
			}
		}
	}
}
