//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// C3-039 (audit-2026-07-23) — the Stripe webhook dedupe race.
//
// `AppendStripeEvent` used to be an `INSERT … ON CONFLICT DO NOTHING
// RETURNING` UNION-ALL'd with a read of the existing row. Two deliveries of
// the same `stripe_event_id`:
//
//	A: inserts        → inserted=true            → nil (proceed)
//	B: insert no-ops  → reads processed_at=NULL  → nil (proceed)
//
// BOTH were told to process the same paid event. `processed_at` cannot
// arbitrate, because it is only stamped once the handler's work finishes —
// B does not even have to be simultaneous with A, only to arrive while A is
// still working, which Stripe's at-least-once delivery makes routine.
//
// These tests run against real Postgres because the defect IS the database's
// concurrency semantics: an in-memory fake would encode whatever behaviour we
// assumed, which is exactly the assumption that was wrong. They cover the
// claim's whole lifecycle, because a claim that is never RELEASED would
// re-introduce F-1322 (a stuck row silently never re-processed) — the bug the
// racy code was written to fix.
func TestStripeEventClaim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	billing := postgresstore.NewBillingStore(postgresstore.New(db))

	newEvent := func(id string) platform.StripeEvent {
		return platform.StripeEvent{
			StripeEventID: id,
			Type:          "checkout.session.completed",
			ReceivedAt:    time.Now().UTC(),
		}
	}

	// ── The headline race: a second delivery arriving mid-processing ──
	//
	// Sequential and fully deterministic — no goroutines needed, because the
	// window the bug lives in is "A has claimed the row and has not yet
	// stamped processed_at", which is every millisecond of the handler's
	// work. Pre-fix this returned nil and both deliveries provisioned.
	t.Run("SecondDeliveryDuringProcessingIsRefused", func(t *testing.T) {
		e := newEvent("evt_inflight_seq")
		if err := billing.AppendStripeEvent(ctx, e); err != nil {
			t.Fatalf("first delivery: got %v, want nil (it must claim the row)", err)
		}
		err := billing.AppendStripeEvent(ctx, e)
		if !errors.Is(err, platform.ErrEventInFlight) {
			t.Fatalf("second delivery while the first is still processing: got %v, want ErrEventInFlight "+
				"(nil here means BOTH deliveries provision the same paid event)", err)
		}
		// It must NOT be reported as already-processed: that maps to a 200
		// dup-ack, which takes the event out of Stripe's retry queue for
		// good, so a crash of the in-flight processor would strand a paid
		// customer.
		if errors.Is(err, platform.ErrAlreadyProcessed) {
			t.Error("in-flight was reported as already-processed — that dup-acks a 200 and kills the retry")
		}
	})

	// ── The genuinely concurrent form ──
	//
	// N goroutines racing on one fresh event id. EXACTLY one may be told to
	// proceed; every other must get ErrEventInFlight. This is the form that
	// also exercises the `ON CONFLICT DO NOTHING` snapshot hazard the
	// advisory lock closes: without the lock the loser's statement snapshot
	// predates the winner's commit, so its read arm sees no row at all.
	t.Run("ConcurrentDeliveriesElectExactlyOneProcessor", func(t *testing.T) {
		const racers = 8
		e := newEvent("evt_inflight_race")

		var (
			wg        sync.WaitGroup
			mu        sync.Mutex
			proceed   int
			inFlight  int
			processed int
			others    []error
			start     = make(chan struct{})
		)
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				err := billing.AppendStripeEvent(ctx, e)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					proceed++
				case errors.Is(err, platform.ErrEventInFlight):
					inFlight++
				case errors.Is(err, platform.ErrAlreadyProcessed):
					processed++
				default:
					others = append(others, err)
				}
			}()
		}
		close(start)
		wg.Wait()

		if len(others) > 0 {
			t.Fatalf("unexpected errors from concurrent claims: %v", others)
		}
		if proceed != 1 {
			t.Errorf("callers told to process = %d, want exactly 1 "+
				"(every extra one applies the same paid Stripe event again)", proceed)
		}
		if processed != 0 {
			t.Errorf("callers told already-processed = %d, want 0 (nothing has completed yet)", processed)
		}
		if inFlight != racers-1 {
			t.Errorf("callers told in-flight = %d, want %d", inFlight, racers-1)
		}
	})

	// ── Completion: a later delivery is a duplicate, not a retry ──
	t.Run("AfterProcessedIsAlreadyProcessed", func(t *testing.T) {
		e := newEvent("evt_completed")
		if err := billing.AppendStripeEvent(ctx, e); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := billing.MarkStripeEventProcessed(ctx, e.StripeEventID); err != nil {
			t.Fatalf("mark processed: %v", err)
		}
		if err := billing.AppendStripeEvent(ctx, e); !errors.Is(err, platform.ErrAlreadyProcessed) {
			t.Fatalf("after completion: got %v, want ErrAlreadyProcessed", err)
		}
	})

	// ── F-1322 must survive: a FAILED attempt releases the claim ──
	//
	// The bug the racy code existed to fix: a first delivery that errors
	// must not leave the event permanently dup-acked. The claim is released
	// by MarkStripeEventFailed, so the next Stripe retry re-claims
	// IMMEDIATELY rather than waiting out the dead attempt's lease.
	t.Run("FailedAttemptReleasesTheClaim", func(t *testing.T) {
		e := newEvent("evt_failed_then_retried")
		if err := billing.AppendStripeEvent(ctx, e); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := billing.MarkStripeEventFailed(ctx, e.StripeEventID, "redis blip during key upgrade"); err != nil {
			t.Fatalf("mark failed: %v", err)
		}
		if err := billing.AppendStripeEvent(ctx, e); err != nil {
			t.Fatalf("retry after a failed attempt: got %v, want nil (F-1322: an unfinished event stays reprocessable)", err)
		}
		if claimed := claimedAt(t, db, e.StripeEventID); !claimed.Valid {
			t.Error("re-claim did not stamp claimed_at — a third delivery would also proceed")
		}
	})

	// ── Dead-letter releases the claim so an operator re-send works now ──
	t.Run("DeadLetterReleasesTheClaim", func(t *testing.T) {
		e := newEvent("evt_dead_lettered")
		if err := billing.AppendStripeEvent(ctx, e); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := billing.MarkStripeEventDeadLettered(ctx, e.StripeEventID, platform.DeadLetterNoKeys); err != nil {
			t.Fatalf("mark dead-lettered: %v", err)
		}
		// An operator re-sends from the Stripe dashboard seconds later —
		// well inside the lease. It must be re-claimable, or the C3-016
		// recovery path is dead on arrival.
		if err := billing.AppendStripeEvent(ctx, e); err != nil {
			t.Fatalf("operator re-send after dead-letter: got %v, want nil", err)
		}
	})

	// ── Crash safety: a claim nobody will ever release expires ──
	//
	// A SIGKILLed processor releases nothing. Without a lease its claim
	// would wedge the event forever — a fresh way to reproduce the exact
	// F-1322 stall. Aged past the lease directly in SQL (the lease is
	// minutes; the test is not going to wait).
	t.Run("StaleClaimIsReclaimableAfterTheLease", func(t *testing.T) {
		e := newEvent("evt_crashed_processor")
		if err := billing.AppendStripeEvent(ctx, e); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := billing.AppendStripeEvent(ctx, e); !errors.Is(err, platform.ErrEventInFlight) {
			t.Fatalf("pre-condition: got %v, want ErrEventInFlight while the claim is fresh", err)
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE stripe_event_log SET claimed_at = now() - interval '1 hour' WHERE stripe_event_id = $1`,
			e.StripeEventID); err != nil {
			t.Fatalf("age the claim: %v", err)
		}
		if err := billing.AppendStripeEvent(ctx, e); err != nil {
			t.Fatalf("after the lease expired: got %v, want nil (a dead processor must not wedge the event)", err)
		}
	})

	// ── Distinct events never contend ──
	//
	// The advisory-lock key is per event id. If it were coarser, unrelated
	// webhook deliveries would serialise behind each other under load.
	t.Run("DistinctEventsAreIndependent", func(t *testing.T) {
		a, b := newEvent("evt_independent_a"), newEvent("evt_independent_b")
		if err := billing.AppendStripeEvent(ctx, a); err != nil {
			t.Fatalf("claim a: %v", err)
		}
		if err := billing.AppendStripeEvent(ctx, b); err != nil {
			t.Fatalf("claim b while a is in flight: got %v, want nil", err)
		}
	})
}

// claimedAt reads the raw claimed_at column for an event.
func claimedAt(t *testing.T, db *sql.DB, eventID string) sql.NullTime {
	t.Helper()
	var out sql.NullTime
	if err := db.QueryRow(
		`SELECT claimed_at FROM stripe_event_log WHERE stripe_event_id = $1`, eventID,
	).Scan(&out); err != nil {
		t.Fatalf("read claimed_at for %s: %v", eventID, err)
	}
	return out
}
