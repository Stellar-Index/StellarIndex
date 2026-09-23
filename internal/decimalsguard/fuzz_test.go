package decimalsguard

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"testing"
)

// TestSkewDecades pins the logged order-of-magnitude skew on both sides of
// the standard 7 dp: always |7 - decimals|, never negative.
func TestSkewDecades(t *testing.T) {
	for _, tc := range []struct {
		decimals uint32
		want     int
	}{
		{0, 7}, {6, 1}, {7, 0}, {8, 1}, {9, 2}, {18, 11}, {38, 31},
	} {
		if got := skewDecades(tc.decimals); got != tc.want {
			t.Errorf("skewDecades(%d) = %d, want %d", tc.decimals, got, tc.want)
		}
	}
}

// FuzzSkewDecades: skewDecades agrees with an exact big.Int |d - 7| over
// the whole uint32 domain (no wrap on the int conversion).
func FuzzSkewDecades(f *testing.F) {
	for _, d := range []uint32{0, 6, 7, 8, 18, 1<<31 - 1, 1 << 31, ^uint32(0)} {
		f.Add(d)
	}
	f.Fuzz(func(t *testing.T, d uint32) {
		want := new(big.Int).Sub(new(big.Int).SetUint64(uint64(d)), big.NewInt(StandardDecimals))
		want.Abs(want)
		if got := skewDecades(d); big.NewInt(int64(got)).Cmp(want) != 0 {
			t.Fatalf("skewDecades(%d) = %d, want %s", d, got, want)
		}
	})
}

// FuzzReconcileRow drives one persisted row through Reconcile against
// every (persisted, lake, found, read-error, write-error) combination and
// checks the repair contract: an unreadable/underivable lake never touches
// the row; agreement is silent; a mismatch is repaired TOWARD the lake —
// deleted when the lake says 7 (the table forbids 7), upserted to the
// lake's exact value otherwise — and a failed repair leaves the row as it
// was so the next tick retries.
func FuzzReconcileRow(f *testing.F) {
	f.Add(int32(6), uint32(9), true, false, false)
	f.Add(int32(6), uint32(7), true, false, false)
	f.Add(int32(9), uint32(9), true, false, false)
	f.Add(int32(6), uint32(9), false, false, false)
	f.Add(int32(6), uint32(9), true, true, false)
	f.Add(int32(6), uint32(7), true, false, true)
	f.Add(int32(6), uint32(18), true, false, true)
	f.Add(int32(-1), ^uint32(0), true, false, false)
	f.Fuzz(func(t *testing.T, persisted int32, lake uint32, found, readErr, writeErr bool) {
		const asset = "fake-fuzz-reconcile"
		store := newFakeStore(row(asset, int(persisted)))
		res := &fakeResolver{decimals: map[string]uint32{}}
		if found {
			res.decimals[asset] = lake
		}
		if readErr {
			res.err = errors.New("lake down")
		}
		if writeErr {
			store.upsertErr = errors.New("pg down")
			store.deleteErr = errors.New("pg down")
		}
		g := New(&fakeReader{}, res, Options{Writer: store, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

		before := mismatchVal(asset)
		if err := g.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		counted := mismatchVal(asset) - before
		got, stillThere := store.rows[asset]

		readable := found && !readErr
		mismatch := readable && int64(lake) != int64(persisted)
		switch {
		case !readable || !mismatch:
			if store.upserts+store.deletes != 0 || !stillThere || got.Decimals != int(persisted) || counted != 0 {
				t.Fatalf("no-op case touched the row: upserts=%d deletes=%d row=%+v counted=%v",
					store.upserts, store.deletes, got, counted)
			}
		case lake == StandardDecimals:
			if store.deletes != 1 || store.upserts != 0 || counted != 1 {
				t.Fatalf("lake=7 must delete once: deletes=%d upserts=%d counted=%v", store.deletes, store.upserts, counted)
			}
			if stillThere == !writeErr {
				t.Fatalf("after delete (writeErr=%v) row present=%v", writeErr, stillThere)
			}
		default:
			if store.upserts != 1 || store.deletes != 0 || counted != 1 {
				t.Fatalf("lake=%d must upsert once: upserts=%d deletes=%d counted=%v", lake, store.upserts, store.deletes, counted)
			}
			want := int(lake)
			if writeErr {
				want = int(persisted)
			}
			if !stillThere || got.Decimals != want {
				t.Fatalf("row after repair = %+v (present=%v), want decimals %d", got, stillThere, want)
			}
		}

		// A confirmed lake reading — agreement or a WRITTEN repair — must
		// reach the resolved cache so the next Sweep agrees with it.
		d, cached := g.resolved[asset]
		wantCached := readable && (!mismatch || !writeErr)
		if cached != wantCached || (cached && d != lake) {
			t.Fatalf("resolved cache = (%d, %v), want (%d, %v)", d, cached, lake, wantCached)
		}
	})
}
