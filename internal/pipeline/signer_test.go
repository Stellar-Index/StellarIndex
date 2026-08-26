package pipeline

import (
	"context"
	"log/slog"
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
