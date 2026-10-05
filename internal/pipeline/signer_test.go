package pipeline

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type fakeLake struct {
	sigs   []clickhouse.TxSigner
	onRead func(minLedger, maxLedger uint32)
}

func (f *fakeLake) TxSignersForLedgerRange(_ context.Context, minLedger, maxLedger uint32) ([]clickhouse.TxSigner, error) {
	if f.onRead != nil {
		f.onRead(minLedger, maxLedger)
	}
	return f.sigs, nil
}

type fakeTagger struct {
	minL, maxL uint32
	ok         bool
	onTag      func([]timescale.SignerTag)
}

func (f *fakeTagger) UntaggedAMMSignerLedgerRange(_ context.Context, _, _ time.Time) (uint32, uint32, bool, error) {
	return f.minL, f.maxL, f.ok, nil
}

func (f *fakeTagger) TagTradesSigner(_ context.Context, _, _ time.Time, tags []timescale.SignerTag) (int64, error) {
	if f.onTag != nil {
		f.onTag(tags)
	}
	return int64(len(tags)), nil
}

// TestRunSignerTagger_TagsFromLake pins the sweep orchestration: it reads the
// untagged AMM ledger range, scopes the lake read to exactly that range, and
// feeds the lake's (ledger, tx_hash, signer) rows to the tagger.
func TestRunSignerTagger_TagsFromLake(t *testing.T) {
	readRange := make(chan [2]uint32, 4)
	lake := &fakeLake{
		sigs:   []clickhouse.TxSigner{{Ledger: 100, TxHash: "abc", Signer: "GSIGNER"}},
		onRead: func(lo, hi uint32) { readRange <- [2]uint32{lo, hi} },
	}
	tagged := make(chan []timescale.SignerTag, 4)
	store := &fakeTagger{minL: 100, maxL: 102, ok: true, onTag: func(tags []timescale.SignerTag) { tagged <- tags }}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunSignerTagger(ctx, slog.Default(), lake, store, 10*time.Millisecond, time.Minute)

	select {
	case r := <-readRange:
		if r != [2]uint32{100, 102} {
			t.Fatalf("lake read range = %v, want the untagged span [100,102]", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sweeper never read the lake")
	}
	select {
	case got := <-tagged:
		if len(got) != 1 || got[0].Signer != "GSIGNER" || got[0].Ledger != 100 || got[0].TxHash != "abc" {
			t.Fatalf("tagged = %+v, want the lake's one signer", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sweeper never tagged")
	}
}

// TestRunSignerTagger_SkipsLakeWhenNothingUntagged is the perf guard: when no
// AMM trade needs a signer (ok=false), the sweep must NOT touch the lake — the
// whole point of scoping to the untagged range.
func TestRunSignerTagger_SkipsLakeWhenNothingUntagged(t *testing.T) {
	lake := &fakeLake{onRead: func(_, _ uint32) { t.Error("lake read must be skipped when nothing is untagged") }}
	store := &fakeTagger{ok: false}

	ctx, cancel := context.WithCancel(context.Background())
	go RunSignerTagger(ctx, slog.Default(), lake, store, 5*time.Millisecond, time.Minute)
	time.Sleep(60 * time.Millisecond) // let several sweeps run
	cancel()
}

// A trade whose tx is absent from the lake pins the untagged minimum; once
// its clamped slice yields nothing new the sweep must move past it instead
// of re-reading the same slice until the lookback expires.
func TestRunSignerTagger_SkipsPastUntaggableLedger(t *testing.T) {
	const first, last = uint32(100), uint32(400)
	var mu sync.Mutex
	tagged := map[uint32]bool{}
	var maxRead uint32

	lake := &fakeLakeFn{fn: func(lo, hi uint32) []clickhouse.TxSigner {
		mu.Lock()
		defer mu.Unlock()
		maxRead = max(maxRead, hi)
		var out []clickhouse.TxSigner
		for l := lo; l <= hi; l++ {
			if l != first { // ledger `first` has no lake row
				out = append(out, clickhouse.TxSigner{Ledger: l, TxHash: "h", Signer: "G"})
			}
		}
		return out
	}}
	store := &statefulTagger{mu: &mu, tagged: tagged, first: first, last: last}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunSignerTagger(ctx, slog.Default(), lake, store, 5*time.Millisecond, time.Hour)

	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		done := tagged[last]
		mu.Unlock()
		if done {
			return
		}
		select {
		case <-deadline:
			mu.Lock()
			defer mu.Unlock()
			t.Fatalf("wedged on the untaggable ledger: highest ledger read = %d, want >= %d", maxRead, last)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

type fakeLakeFn struct {
	fn func(lo, hi uint32) []clickhouse.TxSigner
}

func (f *fakeLakeFn) TxSignersForLedgerRange(_ context.Context, lo, hi uint32) ([]clickhouse.TxSigner, error) {
	return f.fn(lo, hi), nil
}

// statefulTagger's lowest untagged ledger is always `first`, which the lake
// never has a row for.
type statefulTagger struct {
	mu          *sync.Mutex
	tagged      map[uint32]bool
	first, last uint32
}

func (s *statefulTagger) UntaggedAMMSignerLedgerRange(_ context.Context, _, _ time.Time) (uint32, uint32, bool, error) {
	return s.first, s.last, true, nil
}

func (s *statefulTagger) TagTradesSigner(_ context.Context, _, _ time.Time, tags []timescale.SignerTag) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, tg := range tags {
		if !s.tagged[tg.Ledger] {
			s.tagged[tg.Ledger] = true
			n++
		}
	}
	return n, nil
}
