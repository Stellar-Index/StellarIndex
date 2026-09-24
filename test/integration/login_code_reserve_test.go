//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// The verify-code handler compares a submitted code only against the
// tokens ReserveLoginCodeCandidates hands it, and only after
// RegisterFailedLoginCode has admitted the attempt. Both caps therefore
// hold under a concurrent burst only if those two statements serialise
// in Postgres — which is what this pins, with real row locks.
func TestLoginCodeReservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tokens := postgresstore.NewTokenStore(postgresstore.New(db))

	const maxAttempts = 5
	mint := func(t *testing.T, email string, n int, purpose platform.TokenPurpose, ttl time.Duration) [][]byte {
		t.Helper()
		var hashes [][]byte
		for i := 0; i < n; i++ {
			h := sha256.Sum256([]byte(email + string(purpose) + ttl.String() + string(rune('a'+i))))
			if err := tokens.CreateMagicLinkToken(ctx, platform.MagicLinkToken{
				TokenHash: h[:], Email: email, Purpose: purpose,
				ExpiresAt: time.Now().UTC().Add(ttl), RequestedIP: net.ParseIP("203.0.113.9"),
			}); err != nil {
				t.Fatalf("mint: %v", err)
			}
			hashes = append(hashes, h[:])
		}
		return hashes
	}
	attemptsOf := func(t *testing.T, hash []byte) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT attempts FROM magic_link_tokens WHERE token_hash = $1`, hash).Scan(&n); err != nil {
			t.Fatalf("read attempts: %v", err)
		}
		return n
	}
	burst := func(t *testing.T, email string, racers int) int {
		t.Helper()
		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			handOuts int
		)
		errs := make(chan error, racers)
		start := make(chan struct{})
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				got, err := tokens.ReserveLoginCodeCandidates(ctx, email, maxAttempts)
				if err != nil {
					errs <- err
					return
				}
				if len(got) > 0 {
					mu.Lock()
					handOuts++
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent reserve: %v", err)
		}
		return handOuts
	}

	t.Run("ChargesBeforeReturningAndSkipsIneligibleRows", func(t *testing.T) {
		const email = "reserve-shape@example.com"
		live := mint(t, email, 1, platform.TokenPurposeLogin, time.Hour)[0]
		expired := mint(t, email, 1, platform.TokenPurposeLogin, -time.Minute)[0]
		consumed := mint(t, email, 1, platform.TokenPurposeLogin, 2*time.Hour)[0]
		if _, err := tokens.ConsumeMagicLinkToken(ctx, consumed); err != nil {
			t.Fatalf("consume: %v", err)
		}
		other := mint(t, "someone-else@example.com", 1, platform.TokenPurposeLogin, time.Hour)[0]

		got, err := tokens.ReserveLoginCodeCandidates(ctx, email, maxAttempts)
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if len(got) != 1 || string(got[0].TokenHash) != string(live) {
			t.Fatalf("reserved %d rows, want exactly the one live token", len(got))
		}
		if got[0].Attempts != 1 {
			t.Errorf("returned Attempts = %d, want 1 (post-charge)", got[0].Attempts)
		}
		for name, h := range map[string][]byte{"expired": expired, "consumed": consumed, "other email": other} {
			if n := attemptsOf(t, h); n != 0 {
				t.Errorf("%s token charged: attempts = %d, want 0", name, n)
			}
		}
	})

	t.Run("ConcurrentBurstGetsExactlyTheTokenCap", func(t *testing.T) {
		const email = "reserve-burst@example.com"
		h := mint(t, email, 1, platform.TokenPurposeLogin, time.Hour)[0]
		if got := burst(t, email, 50); got != maxAttempts {
			t.Errorf("%d of 50 concurrent reservations were handed the token, want exactly %d", got, maxAttempts)
		}
		if n := attemptsOf(t, h); n != maxAttempts {
			t.Errorf("attempts = %d after the burst, want %d", n, maxAttempts)
		}
	})

	// Several live tokens per address is the normal case (a user who asks
	// twice); every call locks all of them, so lock order matters.
	t.Run("ConcurrentBurstOverSeveralTokensNeitherDeadlocksNorOvercharges", func(t *testing.T) {
		const email = "reserve-multi@example.com"
		hashes := mint(t, email, 3, platform.TokenPurposeLogin, time.Hour)
		if got := burst(t, email, 50); got != maxAttempts {
			t.Errorf("%d of 50 concurrent reservations were handed candidates, want exactly %d", got, maxAttempts)
		}
		for i, h := range hashes {
			if n := attemptsOf(t, h); n != maxAttempts {
				t.Errorf("token %d: attempts = %d, want %d", i, n, maxAttempts)
			}
		}
	})

	// The handler admits an attempt when the post-increment count it gets
	// back is <= the cap, so concurrent charges must each see a distinct
	// count: exactly maxFailures of a burst may come back within budget.
	t.Run("ConcurrentChargesAdmitExactlyTheDurableCap", func(t *testing.T) {
		const (
			email       = "charge-burst@example.com"
			maxFailures = 10
			racers      = 50
		)
		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			admitted int
		)
		errs := make(chan error, racers)
		start := make(chan struct{})
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				state, err := tokens.RegisterFailedLoginCode(ctx, email, maxFailures, 24*time.Hour, 24*time.Hour)
				if err != nil {
					errs <- err
					return
				}
				if state.FailedCount <= maxFailures {
					mu.Lock()
					admitted++
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent charge: %v", err)
		}
		if admitted != maxFailures {
			t.Errorf("%d of %d concurrent charges came back within budget, want exactly %d",
				admitted, racers, maxFailures)
		}
	})
}
