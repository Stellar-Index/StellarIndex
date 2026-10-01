// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/sources/upshift"
)

// lakeRow is one contract_events row as the fake lake sees it: sym stands in
// for the topic_0_sym column the ClickHouse exclusion filters on.
type lakeRow struct {
	sym string
	ev  events.Event
}

// fakeLakeReads applies the same filters the ClickHouse queries do and
// records which read ran.
func fakeLakeReads(rows []lakeRow, calls *[]string) chRebuildEventReads {
	return chRebuildEventReads{
		firehose: func(_ context.Context, _, _ uint32, excludeTopic0 []string, fn func(events.Event) error) error {
			*calls = append(*calls, "firehose")
			for _, r := range rows {
				if slices.Contains(excludeTopic0, r.sym) {
					continue
				}
				if err := fn(r.ev); err != nil {
					return err
				}
			}
			return nil
		},
		scoped: func(_ context.Context, _, _ uint32, contractIDs []string, _ bool, fn func(events.Event) error) error {
			*calls = append(*calls, "scoped")
			for _, r := range rows {
				if !slices.Contains(contractIDs, r.ev.ContractID) {
					continue
				}
				if err := fn(r.ev); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// passDecoder matches every event and emits one row tagged with its source.
type passDecoder struct{ src string }

func (passDecoder) Matches(events.Event) bool { return true }
func (d passDecoder) Decode(events.Event) ([]consumer.Event, error) {
	return []consumer.Event{fakeProtoEvent{src: d.src}}, nil
}

// upshiftShareTransfer is the real earnUSDC share transfer at ledger
// 63,812,811 (upshift's TestGolden_earnUSDCShareTransfer_ledger63812811).
func upshiftShareTransfer() events.Event {
	return events.Event{
		Type:           "contract",
		ContractID:     upshift.MainnetVaultEarnUSDC,
		Ledger:         63812811,
		LedgerClosedAt: "2026-08-05T16:03:50Z",
		TxHash:         "f2b68531502335f15cac27bcc9cb0709cf3fafb080db42a9ea7832ad44d31faf",
		Topic: []string{
			upshift.TopicSymbolTransfer,
			"AAAAEgAAAAAAAAAAmi+a9UaXijZfqxN4jF3566W++n3vOVYIT5pT9hCzWoo=",
			"AAAAEgAAAAGTzb0DoGVb8KBG2JTMthbzdZBAiVqaOpSajh6l+9Q/1Q==",
		},
		Value: "AAAAEQAAAAEAAAACAAAADwAAAAZhbW91bnQAAAAAAAoAAAAAAAAAAAAAWjb2X43p" +
			"AAAADwAAAAt0b19tdXhlZF9pZAAAAAAB",
	}
}

// TestCHRebuildEventPass_UpshiftShareTransferReachesTheDecoder pins that the
// catalogue's upshift entry is re-derived through a read the firehose topic
// exclusion does not apply to: `transfer` is one of upshift's own served kinds.
func TestCHRebuildEventPass_UpshiftShareTransferReachesTheDecoder(t *testing.T) {
	cat, _, err := buildReconciliationCatalogue(config.Config{})
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: %v", err)
	}
	var calls []string
	reads := fakeLakeReads([]lakeRow{{sym: upshift.EventTransfer, ev: upshiftShareTransfer()}}, &calls)
	enabled := func(name string) bool { return name == upshift.SourceName }

	buf, err := runCHRebuildEventPass(context.Background(), reads, 63812811, 63812811, cat, enabled, nil)
	if err != nil {
		t.Fatalf("runCHRebuildEventPass: %v", err)
	}
	if len(buf) != 1 {
		t.Fatalf("buffered %d events (reads %v), want the 1 upshift share transfer", len(buf), calls)
	}
	got, ok := buf[0].(upshift.Event)
	if !ok || got.Kind != upshift.EventTransfer {
		t.Fatalf("buffered %#v, want an upshift.Event of kind %q", buf[0], upshift.EventTransfer)
	}
	if !slices.Equal(calls, []string{"scoped"}) {
		t.Errorf("reads = %v, want only the contract-scoped read (no firehose scan for a scoped-only run)", calls)
	}
}

// TestCHRebuildEventPass_RoutesEachSourceToOneRead pins that a mixed run
// decodes every event exactly once: a firehose-topic source is dropped from
// the firehose read, not read by both.
func TestCHRebuildEventPass_RoutesEachSourceToOneRead(t *testing.T) {
	const vault, pool = "CVAULT", "CPOOL"
	cat := []reconSource{
		{name: "vault", dec: passDecoder{src: "vault"}, contractIDs: []string{vault}, firehoseTopics: true},
		{name: "dex", dec: passDecoder{src: "dex"}},
	}
	rows := []lakeRow{
		{sym: "deposit", ev: events.Event{ContractID: vault}},
		{sym: "transfer", ev: events.Event{ContractID: vault}},
		{sym: "swap", ev: events.Event{ContractID: pool}},
		{sym: "transfer", ev: events.Event{ContractID: pool}},
	}
	var calls []string
	all := func(string) bool { return true }

	buf, err := runCHRebuildEventPass(context.Background(), fakeLakeReads(rows, &calls), 1, 2, cat, all, nil)
	if err != nil {
		t.Fatalf("runCHRebuildEventPass: %v", err)
	}
	perSource := map[string]int{}
	for _, e := range buf {
		perSource[e.Source()]++
	}
	// vault: deposit + transfer via the scoped read only. dex: the two
	// non-excluded firehose rows (deposit, swap); both transfers are excluded.
	if perSource["vault"] != 2 || perSource["dex"] != 2 {
		t.Errorf("per-source = %v, want vault=2 dex=2", perSource)
	}
	if !slices.Equal(calls, []string{"firehose", "scoped"}) {
		t.Errorf("reads = %v, want [firehose scoped]", calls)
	}
}

func TestCHRebuildEventPass_RefusesAnUnscopedFirehoseTopicSource(t *testing.T) {
	cat := []reconSource{{name: "vault", dec: passDecoder{src: "vault"}, firehoseTopics: true}}
	var calls []string
	_, err := runCHRebuildEventPass(context.Background(), fakeLakeReads(nil, &calls), 1, 2, cat, func(string) bool { return true }, nil)
	if err == nil || !strings.Contains(err.Error(), "no contract prefilter") {
		t.Fatalf("err = %v, want a no-contract-prefilter refusal", err)
	}
	if len(calls) != 0 {
		t.Errorf("reads = %v, want none before the refusal", calls)
	}
}
