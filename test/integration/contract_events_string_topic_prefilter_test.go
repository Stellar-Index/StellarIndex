//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestClickHouseStringTopicPrefilterAdmitsPhoenixPool is the live-ClickHouse
// proof for the lake half of F048.
//
// StreamContractEventsFiltered's topic[0] prefilter used to be
// `topic_0_sym IN (…)` alone. extract.go fills that column from
// `Topics[0].GetSym()` — Symbol ONLY — so it is EMPTY for every event whose
// topic[0] is an ScvString. Phoenix's factory publishes
// ("create","liquidity_pool") as two Strings, so every consumer that asks the
// lake for creationSym "create" (seed-protocol-contracts, and the -ch
// re-derive's gatedPrefilter walk) matched ZERO rows over a lake that holds
// those events from ledger 51,572,026 — the walk looked clean and admitted
// nothing.
//
// The row inserted below is the REAL r1 capture, byte-for-byte: contract id,
// both topic blobs, the body and the empty topic_0_sym are copied from
// test/fixtures/phoenix/factory-create/
// factory_2026-07-02_ledgers_63293663-63293708.jsonl (ledger 63,293,708, the
// factory's create of CBENABXP…). Only the ledger/tx coordinates are moved
// into this test's private range.
//
// The assertion runs the whole loop the defect broke — lake row → SQL
// prefilter → streamed event → decoder → identity gate — and ends on the one
// observation that cannot be faked: the registry's live-upsert hook receiving
// the announced pool. Reverting topic0Predicate to `topic_0_sym IN (…)` makes
// the stream return 1 row instead of 2 and seeds nothing.
func TestClickHouseStringTopicPrefilterAdmitsPhoenixPool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		lo = uint32(70_300_001)
		hi = uint32(70_300_003)
		// The real phoenix factory, and the pool the captured event
		// announces (body decoded: a single ScvAddress).
		factoryContract = phoenix.MainnetFactory
		announcedPool   = "CBENABXP6C4C7WG6KB7JQOTDS5GIIXF3IX3PIYNZFCDZDWUHITO2HZ4S"
		// Verbatim from the capture: ScvString("create"),
		// ScvString("liquidity_pool") and the ScvAddress body.
		realCreateTopic0 = "AAAADgAAAAZjcmVhdGUAAA=="
		realCreateTopic1 = "AAAADgAAAA5saXF1aWRpdHlfcG9vbAAA"
		realCreateBody   = "AAAAEgAAAAFI0Abv8Lgv2N5Qfpg6Y5dMhFy7Rfb0Ybkoh5Hah0Tdow=="
		// The factory's other captured event, ("Factory","Updated Config"):
		// also String topics, also empty topic_0_sym. It must NOT match.
		decoyTopic0 = "AAAADgAAAAdGYWN0b3J5AA=="
		decoyTopic1 = "AAAADgAAAA5VcGRhdGVkIENvbmZpZwAA"
		decoyBody   = "AAAAAQ=="

		txCreate = "1111111111111111111111111111111111111111111111111111111111111111"
		txSymbol = "2222222222222222222222222222222222222222222222222222222222222222"
		txDecoy  = "3333333333333333333333333333333333333333333333333333333333333333"
	)
	closeTime := time.Date(2026, 7, 2, 10, 24, 5, 0, time.UTC)

	row := func(ledger uint32, tx string, topics []string, topic0Sym, body string) chstore.ContractEventRow {
		return chstore.ContractEventRow{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: tx, OpIndex: 0, EventIndex: 0,
			ContractID: factoryContract, EventType: "contract",
			TopicCount: uint8(len(topics)), //nolint:gosec // two topics, fixed above.
			Topic0Sym:  topic0Sym, TopicsXDR: topics, DataXDR: body,
			OpArgsXDR: []string{}, InSuccessfulCall: 1,
		}
	}

	ext := chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: lo, CloseTime: closeTime, LedgerHash: "aa11aa11", PrevHash: "bb22bb22",
			ProtocolVersion: 23, TxCount: 3, OpCount: 3, SorobanEventCount: 3,
		},
		Events: []chstore.ContractEventRow{
			// (1) The real String-topic create: topic_0_sym EMPTY, exactly as
			// extract.go writes it.
			row(lo, txCreate, []string{realCreateTopic0, realCreateTopic1}, "", realCreateBody),
			// (2) A Symbol-topic "create" from the same emitter — the arm that
			// already worked. Pinned so widening the predicate cannot silently
			// drop the encoding every other gated source relies on.
			row(lo+1, txSymbol, []string{scval.MustEncodeSymbol("create"), realCreateTopic1},
				"create", realCreateBody),
			// (3) A different String topic[0] from the same emitter: must be
			// excluded, or the predicate has stopped filtering.
			row(lo+2, txDecoy, []string{decoyTopic0, decoyTopic1}, "", decoyBody),
		},
	}

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })
	if err := sink.Add(ctx, ext); err != nil {
		t.Fatalf("sink add: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}

	// The prefilter as seed-protocol-contracts and gatedPrefilter issue it:
	// scope to the factory, ask for creationSym "create".
	var streamed []events.Event
	if err := chstore.StreamContractEventsFiltered(ctx, addr, lo, hi,
		[]string{factoryContract}, []string{phoenix.EventActionCreate}, nil,
		true, false, false,
		func(ev events.Event) error {
			streamed = append(streamed, ev)
			return nil
		}); err != nil {
		t.Fatalf("StreamContractEventsFiltered: %v", err)
	}

	if len(streamed) != 2 {
		t.Fatalf("prefilter on creationSym %q returned %d events, want 2 (the ScvString create AND "+
			"the ScvSymbol create; the ScvString one has an EMPTY topic_0_sym, so a "+
			"topic_0_sym-only predicate returns just 1 and the phoenix factory walk admits nothing)",
			phoenix.EventActionCreate, len(streamed))
	}
	byTx := map[string]events.Event{}
	for _, ev := range streamed {
		byTx[ev.TxHash] = ev
	}
	if _, ok := byTx[txCreate]; !ok {
		t.Errorf("the real ScvString ((\"create\",\"liquidity_pool\")) event did not survive the "+
			"prefilter; streamed tx hashes = %v", streamedTxHashes(byTx))
	}
	if _, ok := byTx[txSymbol]; !ok {
		t.Errorf("the ScvSymbol(\"create\") event did not survive the prefilter — widening the "+
			"predicate must not drop the topic_0_sym arm; streamed tx hashes = %v", streamedTxHashes(byTx))
	}
	if _, ok := byTx[txDecoy]; ok {
		t.Error("the (\"Factory\",\"Updated Config\") event survived a prefilter for \"create\" — " +
			"the predicate has stopped filtering")
	}

	// Close the loop: the streamed lake row must actually admit the pool.
	// Seeding is the only observable a decoder that merely RECOGNISES the
	// event cannot produce.
	var seeded []string
	dec := phoenix.NewDecoder(contractid.WithHook(func(child, factory string, ledger uint32) {
		if factory != factoryContract {
			t.Errorf("seeded %s with provenance factory %s, want %s", child, factory, factoryContract)
		}
		seeded = append(seeded, child)
	}))
	createEvent, ok := byTx[txCreate]
	if !ok {
		t.Fatal("cannot run the admission leg: the create event was filtered out above")
	}
	if !dec.Matches(createEvent) {
		t.Fatalf("phoenix decoder rejects the factory's create event as streamed from the lake")
	}
	if _, err := dec.Decode(createEvent); err != nil {
		t.Fatalf("decode streamed create event: %v", err)
	}
	if len(seeded) != 1 || seeded[0] != announcedPool {
		t.Fatalf("lake-streamed create event seeded %v, want exactly [%s] — the announced pool must "+
			"reach the identity gate for a factory-created pool's swaps to be attributed (F048)",
			seeded, announcedPool)
	}
}

// streamedTxHashes returns the streamed tx hashes, for a readable
// failure message.
func streamedTxHashes(m map[string]events.Event) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
