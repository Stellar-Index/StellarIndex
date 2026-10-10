package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/cmd/stellarindex-api/internal/wiring"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type fakeLakeReader struct{ closed chan struct{} }

func (f *fakeLakeReader) Close() error {
	if f.closed != nil {
		close(f.closed)
	}
	return nil
}

func fastClickHouseBootBackoff(t *testing.T) {
	t.Helper()
	oldMin, oldMax, oldAttempt := clickhouseBootDialBackoffMin, clickhouseBootDialBackoffMax, clickhouseBootDialMinAttempt
	clickhouseBootDialBackoffMin, clickhouseBootDialBackoffMax, clickhouseBootDialMinAttempt = time.Millisecond, 4*time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		clickhouseBootDialBackoffMin, clickhouseBootDialBackoffMax, clickhouseBootDialMinAttempt = oldMin, oldMax, oldAttempt
	})
}

// A ClickHouse that starts accepting connections a few seconds after the
// API boots must end up wired and reported healthy, not latched down.
func TestDialClickHouseAtBoot_LateClickHouseIsWiredAndReadsHealthy(t *testing.T) {
	fastClickHouseBootBackoff(t)
	refused := errors.New("dial tcp 127.0.0.1:9300: connect: connection refused")
	calls := 0
	want := &fakeLakeReader{}
	dial := func(context.Context) (*fakeLakeReader, error) {
		calls++
		if calls < 3 {
			return nil, refused
		}
		return want, nil
	}

	got, err := dialClickHouseAtBoot(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), "explorer", "127.0.0.1:9300", time.Now().Add(time.Minute), dial)
	if err != nil {
		t.Fatalf("a ClickHouse reachable on the third attempt was given up on after %d attempt(s): %v — the checker would latch down for the process lifetime", calls, err)
	}
	if got != want || calls != 3 {
		t.Fatalf("got (%p, %d calls), want (%p, 3)", got, calls, want)
	}

	checks := wiring.ClickhouseReadyChecks("127.0.0.1:9300", nil, err, clickhouseBootDialBudget)
	if len(checks) != 1 {
		t.Fatalf("registered %d checks, want 1", len(checks))
	}
	if c, ok := checks[0].(wiring.ClickhouseChecker); !ok || c.DialErr != nil {
		t.Fatalf("a dial that recovered inside the window registered the failed-dial checker: %#v", checks[0])
	}
}

// A ClickHouse that stays down must not hold the API's boot past the
// window, and must surface the real dial error.
func TestDialClickHouseAtBoot_GivesUpAtTheDeadline(t *testing.T) {
	fastClickHouseBootBackoff(t)
	refused := errors.New("connection refused")
	var calls atomic.Int32
	start := time.Now()
	_, err := dialClickHouseAtBoot(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), "supply", "x", start.Add(50*time.Millisecond), func(context.Context) (*fakeLakeReader, error) {
		calls.Add(1)
		return nil, refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the last dial error", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("dial loop ran %s past a 50ms window", elapsed)
	}
	if n := calls.Load(); n < 2 {
		t.Fatalf("dialled %d time(s) inside the window, want a retry", n)
	}
}

// SIGTERM during boot must not wait out the retry window.
func TestDialClickHouseAtBoot_StopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	start := time.Now()
	_, err := dialClickHouseAtBoot(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), "supply", "x", time.Now().Add(time.Hour), func(context.Context) (*fakeLakeReader, error) {
		calls.Add(1)
		return nil, errors.New("refused")
	})
	if err == nil || calls.Load() > 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("cancelled boot: err=%v calls=%d after %s, want a prompt error after at most one attempt", err, calls.Load(), time.Since(start))
	}
}

// SIGTERM arriving during a backoff wait must end the retry promptly, not
// after the wait.
func TestDialClickHouseAtBoot_StopsOnCancelMidRetry(t *testing.T) {
	oldMin := clickhouseBootDialBackoffMin
	clickhouseBootDialBackoffMin = time.Hour
	t.Cleanup(func() { clickhouseBootDialBackoffMin = oldMin })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	start := time.Now()
	_, err := dialClickHouseAtBoot(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), "supply", "x", time.Now().Add(2*time.Hour), func(context.Context) (*fakeLakeReader, error) {
		calls++
		time.AfterFunc(50*time.Millisecond, cancel)
		return nil, errors.New("refused")
	})
	if err == nil || calls != 1 {
		t.Fatalf("cancelled mid-retry: err=%v calls=%d, want an error after exactly one attempt", err, calls)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancelled retry took %s to return; it waited out the backoff instead of the ctx", elapsed)
	}
}

// A dial that ignores its ctx must not stretch the window, and a success
// that lands after the window must be closed, not leaked.
func TestDialClickHouseAtBoot_HangingDialEndsAtTheDeadline(t *testing.T) {
	release := make(chan struct{})
	late := &fakeLakeReader{closed: make(chan struct{})}
	start := time.Now()
	_, err := dialClickHouseAtBoot(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), "explorer", "x", start.Add(100*time.Millisecond), func(context.Context) (*fakeLakeReader, error) {
		<-release
		return late, nil
	})
	close(release)
	select {
	case <-late.closed:
	case <-time.After(5 * time.Second):
		t.Error("a dial that succeeded after the window was never closed")
	}
	if err == nil {
		t.Fatal("a dial that never answered returned success")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a hanging dial held boot for %s past a 100ms window", elapsed)
	}
}

// The window runs before the listener is up, so a ClickHouse that stays
// down must not keep /v1/readyz dark past the deploy probe's first attempt;
// otherwise a lake outage rolls back a healthy API binary.
func TestClickhouseBootDialBudget_FitsInsideTheDeployProbeGrace(t *testing.T) {
	raw, err := os.ReadFile("../../.github/workflows/deploy.yml")
	if err != nil {
		t.Fatalf("read deploy workflow: %v", err)
	}
	m := regexp.MustCompile(`(?s)health_grace_seconds:.*?default:\s*'(\d+)'`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("deploy.yml has no health_grace_seconds default; this guard is reading the wrong file")
	}
	grace, _ := strconv.Atoi(string(m[1]))
	// Leave half the grace for the rest of boot (Postgres, Redis, caches).
	if limit := time.Duration(grace) * time.Second / 2; clickhouseBootDialBudget > limit {
		t.Fatalf("clickhouseBootDialBudget = %s holds the listener past %s (half the %ds deploy probe grace): a ClickHouse outage during a deploy would fail the probe and roll back the API", clickhouseBootDialBudget, limit, grace)
	}
}

// The real driver: a server that accepts TCP but never sends its hello (a
// ClickHouse still starting) holds clickhouse-go's handshake for the
// reader's 10s DialTimeout whatever the ctx says. The boot window must cut
// it off anyway, or it holds the API's listener past the deploy probe.
func TestDialClickHouseAtBoot_RealDriverSilentServerEndsAtTheDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var accepted []net.Conn
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted = append(accepted, c)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-acceptDone
		for _, c := range accepted {
			_ = c.Close()
		}
	})

	addr := ln.Addr().String()
	start := time.Now()
	_, err = dialClickHouseAtBoot(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), "explorer", addr, start.Add(300*time.Millisecond), func(ctx context.Context) (*clickhouse.ExplorerReader, error) {
		return clickhouse.NewExplorerReaderAuth(ctx, addr, "", "")
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a server that never completed the handshake was reported wired")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("real clickhouse-go dial held boot for %s past a 300ms window: %v", elapsed, err)
	}
}

// Refused until 60% of the window: both readers must still wire, so the
// last attempt has to start before the deadline and the explorer dial must
// not wait for the supply dial to use the window up.
func TestDialLakeReadersAtBoot_ClickHouseUpAtSixtyPercentWiresBoth(t *testing.T) {
	oldMin, oldMax, oldAttempt := clickhouseBootDialBackoffMin, clickhouseBootDialBackoffMax, clickhouseBootDialMinAttempt
	clickhouseBootDialBackoffMin, clickhouseBootDialBackoffMax, clickhouseBootDialMinAttempt = 100*time.Millisecond, 1500*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() {
		clickhouseBootDialBackoffMin, clickhouseBootDialBackoffMax, clickhouseBootDialMinAttempt = oldMin, oldMax, oldAttempt
	})
	const window = 700 * time.Millisecond
	start := time.Now()
	up := start.Add(window * 6 / 10)
	var supplyCalls, explorerCalls atomic.Int32
	dialer := func(calls *atomic.Int32) func(context.Context) (*fakeLakeReader, error) {
		return func(ctx context.Context) (*fakeLakeReader, error) {
			calls.Add(1)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if time.Now().Before(up) {
				return nil, errors.New("connection refused")
			}
			return &fakeLakeReader{}, nil
		}
	}

	s, sErr, e, eErr := dialLakeReadersAtBoot(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), "x", start.Add(window), dialer(&supplyCalls), dialer(&explorerCalls))
	elapsed := time.Since(start)
	if sErr != nil || s == nil {
		t.Errorf("supply reader not wired after %d attempt(s): %v", supplyCalls.Load(), sErr)
	}
	if eErr != nil || e == nil {
		t.Errorf("explorer reader not wired after %d attempt(s): %v — the readiness checker latches down", explorerCalls.Load(), eErr)
	}
	if elapsed > window+time.Second {
		t.Errorf("wiring both readers took %s, past the %s window", elapsed, window)
	}
}

// A supply dial that hangs through the whole window must not cost the
// explorer reader, which drives the readiness checker, its attempts.
func TestDialLakeReadersAtBoot_HangingSupplyDoesNotStarveExplorer(t *testing.T) {
	oldMin, oldMax, oldAttempt := clickhouseBootDialBackoffMin, clickhouseBootDialBackoffMax, clickhouseBootDialMinAttempt
	clickhouseBootDialBackoffMin, clickhouseBootDialBackoffMax, clickhouseBootDialMinAttempt = 100*time.Millisecond, 1500*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() {
		clickhouseBootDialBackoffMin, clickhouseBootDialBackoffMax, clickhouseBootDialMinAttempt = oldMin, oldMax, oldAttempt
	})
	const window = 700 * time.Millisecond
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	start := time.Now()
	up := start.Add(window * 6 / 10)
	hangingSupply := func(context.Context) (*fakeLakeReader, error) {
		<-release
		return nil, errors.New("released")
	}
	explorer := func(ctx context.Context) (*fakeLakeReader, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if time.Now().Before(up) {
			return nil, errors.New("connection refused")
		}
		return &fakeLakeReader{}, nil
	}

	_, sErr, e, eErr := dialLakeReadersAtBoot(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), "x", start.Add(window), hangingSupply, explorer)
	if sErr == nil {
		t.Error("a supply dial that never returned was reported wired")
	}
	if eErr != nil || e == nil {
		t.Errorf("explorer reader not wired while the supply dial hung: %v — the readiness checker latches down", eErr)
	}
	if elapsed := time.Since(start); elapsed > window+time.Second {
		t.Errorf("boot dials took %s, past the %s window", elapsed, window)
	}
}
