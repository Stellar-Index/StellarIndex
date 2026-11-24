package clickhouse

import (
	"context"
	"sync"
	"testing"
	"time"
)

// waitParked blocks until n AcquireClassWait callers are parked on class.
func waitParked(t *testing.T, g *RefreshGate, class string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		g.mu.Lock()
		got := g.waiting[class]
		g.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("parked waiters on %s = %d, want %d", class, got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRefreshGate_AcquireClassWait_BoundedQueue: with the class share held, waiters up to classWaitersPerSlot per slot park and are
// admitted as slots free, never exceeding the share; the next waiter past the queue cap sheds at once, counted on the class bound.
func TestRefreshGate_AcquireClassWait_BoundedQueue(t *testing.T) {
	const class = "contract_detail_wait_test"
	g := NewRefreshGate(DefaultDetachedRefreshLimit) // client-keyed share 2, queue cap 8
	share := g.classCap(class)
	queue := classWaitersPerSlot * share
	for range share {
		if !g.TryAcquireClass(class) {
			t.Fatal("class share should admit")
		}
	}

	var (
		wg               sync.WaitGroup
		mu               sync.Mutex
		maxHeld          = share
		admitted, refuse int
	)
	for range queue {
		wg.Go(func() {
			if !g.AcquireClassWait(context.Background(), class) {
				mu.Lock()
				refuse++
				mu.Unlock()
				return
			}
			g.mu.Lock()
			held := g.classes[class]
			g.mu.Unlock()
			mu.Lock()
			admitted++
			maxHeld = max(maxHeld, held)
			mu.Unlock()
			g.ReleaseClass(class)
		})
	}
	waitParked(t, g, class, queue)

	before := gateSaturated(class, "class")
	start := time.Now()
	if g.AcquireClassWait(context.Background(), class) {
		t.Fatal("a waiter past the queue cap must shed, not queue")
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("over-cap shed took %v; it must not wait", el)
	}
	if got := gateSaturated(class, "class") - before; got != 1 {
		t.Fatalf("over-cap shed counted %v, want 1", got)
	}

	for range share {
		g.ReleaseClass(class)
	}
	wg.Wait()
	if admitted != queue || refuse != 0 {
		t.Fatalf("admitted %d refused %d of %d queued waiters", admitted, refuse, queue)
	}
	if maxHeld > share {
		t.Fatalf("max held = %d, want <= class share %d", maxHeld, share)
	}
	if len(g.sem) != 0 || g.client != 0 || g.waiting[class] != 0 {
		t.Fatalf("gate not drained: sem=%d client=%d waiting=%d", len(g.sem), g.client, g.waiting[class])
	}
}

// TestRefreshGate_AcquireClassWait_DeadlineSheds: a parked waiter whose ctx ends sheds and leaves the queue.
func TestRefreshGate_AcquireClassWait_DeadlineSheds(t *testing.T) {
	const class = "contract_detail_wait_test"
	g := NewRefreshGate(4) // client share 1
	if !g.TryAcquireClass(class) {
		t.Fatal("share should admit")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool)
	go func() { done <- g.AcquireClassWait(ctx, class) }()
	waitParked(t, g, class, 1)
	cancel()
	if <-done {
		t.Fatal("a waiter whose ctx ended must shed")
	}
	if n := g.waiting[class]; n != 0 {
		t.Fatalf("cancelled waiter left %d parked", n)
	}
}
