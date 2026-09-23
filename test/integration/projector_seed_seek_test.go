//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// seekCase is one filter shape the projector's first-event seek must answer
// exactly as the matching stream would.
type seekCase struct {
	name                        string
	from, to                    uint32
	contracts, topics, excludes []string
	want                        uint32
	wantFound                   bool
}

// TestFirstSorobanEventLedger_MatchesStream executes the Postgres seek the
// projector seeds a never-run source from, and pins it to StreamSorobanEvents'
// row set for the same filters (the seed must never skip a row the scan would
// have returned).
func TestFirstSorobanEventLedger_MatchesStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	t0 := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	// mkSyntheticRow: the seed picks the contract; odd seeds carry a NULL
	// topic_0_sym, even seeds "synthetic_event".
	rows := []sorobanevents.Row{
		mkSyntheticRow(t, 1000, t0, 1),
		mkSyntheticRow(t, 1100, t0.Add(time.Second), 2),
		mkSyntheticRow(t, 1200, t0.Add(2*time.Second), 4),
		mkSyntheticRow(t, 1300, t0.Add(3*time.Second), 1),
	}
	if err := store.InsertSorobanEventsBatch(ctx, rows); err != nil {
		t.Fatalf("InsertSorobanEventsBatch: %v", err)
	}
	c1, c2, c4 := rows[0].ContractID, rows[1].ContractID, rows[2].ContractID

	for _, tc := range []seekCase{
		{name: "unfiltered", to: 2000, want: 1000, wantFound: true},
		{name: "contract", to: 2000, contracts: []string{c2}, want: 1100, wantFound: true},
		{name: "contract above from", from: 1001, to: 2000, contracts: []string{c1}, want: 1300, wantFound: true},
		{name: "topic", to: 2000, topics: []string{"synthetic_event"}, want: 1100, wantFound: true},
		{name: "exclude keeps NULL topic", to: 2000, excludes: []string{"synthetic_event"}, want: 1000, wantFound: true},
		{name: "exclude above from", from: 1001, to: 2000, excludes: []string{"synthetic_event"}, want: 1300, wantFound: true},
		{name: "all filters", to: 2000, contracts: []string{c2, c4}, topics: []string{"synthetic_event"}, excludes: []string{"other"}, want: 1100, wantFound: true},
		{name: "below first row", to: 999},
		{name: "to bounds the seek", to: 1199, contracts: []string{c4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, found, err := store.FirstSorobanEventLedger(ctx, tc.from, tc.to, tc.contracts, tc.topics, tc.excludes)
			if err != nil {
				t.Fatalf("FirstSorobanEventLedger: %v", err)
			}
			if found != tc.wantFound || got != tc.want {
				t.Fatalf("FirstSorobanEventLedger = (%d, %v), want (%d, %v)", got, found, tc.want, tc.wantFound)
			}
			var streamFirst uint32
			streamFound := false
			if err := store.StreamSorobanEvents(ctx, tc.from, tc.to, tc.contracts, tc.topics, tc.excludes,
				func(r sorobanevents.Row) error {
					if !streamFound || r.Ledger < streamFirst {
						streamFirst, streamFound = r.Ledger, true
					}
					return nil
				}); err != nil {
				t.Fatalf("StreamSorobanEvents: %v", err)
			}
			if streamFound != found || streamFirst != got {
				t.Fatalf("seek (%d, %v) disagrees with stream's first row (%d, %v)", got, found, streamFirst, streamFound)
			}
		})
	}
}

// TestFirstContractEventLedgerFiltered_MatchesStream is the ClickHouse
// (feed-switch) twin of the Postgres seek test.
func TestFirstContractEventLedgerFiltered_MatchesStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	chAddr := clickhouseAddr(t)

	// A ledger range and contracts no other test writes, since the CH
	// container is shared across the package.
	const base = 7_340_000
	contractA, contractB := seekContract(t, 0xA1), seekContract(t, 0xB2)
	seekSeedEvent(t, ctx, chAddr, base+100, contractA, "swap")
	seekSeedEvent(t, ctx, chAddr, base+200, contractB, "transfer")
	seekSeedEvent(t, ctx, chAddr, base+300, contractB, "swap")

	both := []string{contractA, contractB}
	for _, tc := range []seekCase{
		{name: "contract", from: base, to: base + 1000, contracts: []string{contractB}, want: base + 200, wantFound: true},
		{name: "contract and topic", from: base, to: base + 1000, contracts: []string{contractB}, topics: []string{"swap"}, want: base + 300, wantFound: true},
		{name: "exclude", from: base + 101, to: base + 1000, contracts: both, excludes: []string{"transfer"}, want: base + 300, wantFound: true},
		{name: "above from", from: base + 101, to: base + 1000, contracts: both, want: base + 200, wantFound: true},
		{name: "to bounds the seek", from: base, to: base + 199, contracts: []string{contractB}},
		{name: "no match", from: base, to: base + 1000, contracts: []string{contractA}, topics: []string{"transfer"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, found, err := chstore.FirstContractEventLedgerFiltered(ctx, chAddr, tc.from, tc.to, tc.contracts, tc.topics, tc.excludes)
			if err != nil {
				t.Fatalf("FirstContractEventLedgerFiltered: %v", err)
			}
			if found != tc.wantFound || got != tc.want {
				t.Fatalf("FirstContractEventLedgerFiltered = (%d, %v), want (%d, %v)", got, found, tc.want, tc.wantFound)
			}
			var streamFirst uint32
			streamFound := false
			if err := chstore.StreamContractEventsFiltered(ctx, chAddr, tc.from, tc.to, tc.contracts, tc.topics, tc.excludes,
				false, false, false, func(ev events.Event) error {
					if !streamFound || ev.Ledger < streamFirst {
						streamFirst, streamFound = ev.Ledger, true
					}
					return nil
				}); err != nil {
				t.Fatalf("StreamContractEventsFiltered: %v", err)
			}
			if streamFound != found || streamFirst != got {
				t.Fatalf("seek (%d, %v) disagrees with stream's first row (%d, %v)", got, found, streamFirst, streamFound)
			}
		})
	}
}

func seekContract(t *testing.T, seed byte) string {
	t.Helper()
	var raw [32]byte
	raw[0], raw[1] = seed, 0x5E
	s, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	return s
}

func seekSeedEvent(t *testing.T, ctx context.Context, chAddr string, ledger uint32, contract, topic string) {
	t.Helper()
	closeTime := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC).Add(time.Duration(ledger) * 5 * time.Second)
	sink, err := chstore.Open(ctx, chAddr, 100)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	defer func() { _ = sink.Close(ctx) }()
	ext := chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: ledger, CloseTime: closeTime, LedgerHash: "aa", PrevHash: "bb",
			ProtocolVersion: 22, BucketListHash: "cc", TxCount: 1, OpCount: 1,
		},
		Events: []chstore.ContractEventRow{{
			LedgerSeq:        ledger,
			CloseTime:        closeTime,
			TxHash:           fmt.Sprintf("%064x", ledger),
			ContractID:       contract,
			EventType:        "contract",
			TopicCount:       1,
			Topic0Sym:        topic,
			TopicsXDR:        []string{prB64(t, prSymbol(topic))},
			DataXDR:          prB64(t, prSymbol("x")),
			OpArgsXDR:        []string{},
			InSuccessfulCall: 1,
		}},
	}
	if err := sink.Add(ctx, ext); err != nil {
		t.Fatalf("sink add (ledger %d): %v", ledger, err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush (ledger %d): %v", ledger, err)
	}
}
