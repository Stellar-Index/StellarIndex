//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestDistinctTopicShapes_NonSymbolTopic0 is the GH-807 proof on a real
// ClickHouse: events whose topic[0] is not a Symbol all carry an empty topic_0_sym,
// so keying on (contract, topic_0_sym) alone collapsed them into one shape and
// one exemplar. They must split on topics_xdr[1], topics_xdr[2] and arity,
// while Symbol shapes keep their (contract, topic_0_sym) identity.
func TestDistinctTopicShapes_NonSymbolTopic0(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")

	// A ledger range and contract nothing else in the suite writes.
	const lo, contract = uint32(145_000_000), "CGH807SHAPEFIXTURE"
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer ccancel()
		if err := dialClickHouse(t, cctx, "stellar").Exec(cctx, fmt.Sprintf(`ALTER TABLE stellar.contract_events
			DELETE WHERE contract_id = '%s' SETTINGS mutations_sync = 2`, contract)); err != nil {
			t.Errorf("purge shape fixture events: %v", err)
		}
	})

	type ev struct {
		sym    string
		topics []string
	}
	fixture := []ev{
		{"", []string{"T0", "T1a"}},
		{"", []string{"T0", "T1b"}},       // differs from the first only in topics_xdr[2]
		{"", []string{"T0", "T1a", "T2"}}, // differs only in arity
		{"swap", []string{"SWAP", "P1"}},  // Symbol shapes ignore topic[1] ...
		{"swap", []string{"SWAP", "P2"}},  // ... so these two are one shape
	}
	for i, e := range fixture {
		q := fmt.Sprintf(`INSERT INTO stellar.contract_events
			(ledger_seq, close_time, tx_hash, op_index, event_index, contract_id, event_type,
			 topic_count, topic_0_sym, topics_xdr, data_xdr, op_args_xdr, in_successful_call)
			VALUES (%d, '2026-08-01 00:00:00', 'gh807tx', 0, %d, '%s', 'contract', %d, '%s', ['%s'], 'D%d', [], 1)`,
			lo+uint32(i), i, contract, len(e.topics), e.sym, strings.Join(e.topics, "','"), i)
		if err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("insert fixture event %d: %v", i, err)
		}
	}

	shapes, err := chstore.DistinctTopicShapes(ctx, addr, lo, lo+uint32(len(fixture)), nil)
	if err != nil {
		t.Fatalf("DistinctTopicShapes: %v", err)
	}
	var got []string
	for _, s := range shapes {
		if s.ContractID != contract {
			continue
		}
		got = append(got, fmt.Sprintf("%s|%s|%d|%s", s.Topic0Sym, strings.Join(s.Topics, ","), s.Count, s.DataXDR))
	}
	// Sorted by count desc, then (topic_0_sym, t0, t1, arity); each exemplar is
	// its own shape's event, and the Symbol shape's is its latest (argMax by ledger).
	want := []string{
		"swap|SWAP,P2|2|D4",
		"|T0,T1a|1|D0",
		"|T0,T1a,T2|1|D2",
		"|T0,T1b|1|D1",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("shapes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
