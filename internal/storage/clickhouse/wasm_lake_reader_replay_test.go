// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// replayStub answers ReplayCodeHistory's reads: the genesis watermark, the
// tx-key probe, the indexed timeline and the latest-instance lookup.
type replayStub struct {
	watermark    []any // nil = no row
	watermarkErr error
	timeline     [][]any
	latest       [][]any
}

func (s replayStub) conn(t *testing.T) *stubConn {
	t.Helper()
	c := &stubConn{}
	c.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "entry_history_watermark"):
			if s.watermarkErr != nil {
				return nil, s.watermarkErr
			}
			if s.watermark == nil {
				return &stubRows{}, nil
			}
			return &stubRows{data: [][]any{s.watermark}}, nil
		case strings.Contains(q, "SELECT tx_hash, intra_ledger_seq FROM stellar.contract_instance_changes LIMIT 1"):
			return &stubRows{data: [][]any{{"", uint32(0)}}}, nil
		case strings.Contains(q, "SELECT ledger_seq, close_time, wasm_hash"):
			return &stubRows{data: s.timeline}, nil
		case strings.Contains(q, "SELECT is_sac, wasm_hash"):
			return &stubRows{data: s.latest}, nil
		}
		t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}
	return c
}

func TestReplayCodeHistory(t *testing.T) {
	h := wasmHashN(0xAB)
	hashHex := hex.EncodeToString(h[:])
	base := time.Unix(1700000000, 0).UTC()
	oneVersion := [][]any{{uint32(100), base, hashHex}}

	t.Run("watermark 0 refuses", func(t *testing.T) {
		c := replayStub{watermark: []any{uint32(0)}, timeline: oneVersion}.conn(t)
		_, err := (&ExplorerReader{conn: c}).ReplayCodeHistory(context.Background(), testContractID)
		if !errors.Is(err, ErrInstanceHistoryIncomplete) {
			t.Fatalf("err = %v, want ErrInstanceHistoryIncomplete", err)
		}
		for _, q := range c.queries {
			if strings.Contains(q, "wasm_hash") {
				t.Fatalf("read a timeline without the genesis watermark: %s", q)
			}
		}
	})
	t.Run("watermark read error refuses", func(t *testing.T) {
		boom := errors.New("boom")
		c := replayStub{watermarkErr: boom, timeline: oneVersion}.conn(t)
		_, err := (&ExplorerReader{conn: c}).ReplayCodeHistory(context.Background(), testContractID)
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the watermark read error", err)
		}
	})
	t.Run("versions served", func(t *testing.T) {
		c := replayStub{watermark: []any{uint32(60_000_000)}, timeline: oneVersion}.conn(t)
		got, err := (&ExplorerReader{conn: c}).ReplayCodeHistory(context.Background(), testContractID)
		if err != nil || len(got) != 1 || got[0].WasmHash != hashHex || got[0].Ledger != 100 {
			t.Fatalf("got %+v, %v; want one version %s at 100", got, err, hashHex)
		}
	})
	t.Run("truncated refuses", func(t *testing.T) {
		rows := make([][]any, contractCodeHistoryMaxRows)
		for i := range rows {
			hi := wasmHashN(byte(i % 2))
			rows[i] = []any{uint32(i + 1), base, hex.EncodeToString(hi[:])}
		}
		c := replayStub{watermark: []any{uint32(60_000_000)}, timeline: rows}.conn(t)
		_, err := (&ExplorerReader{conn: c}).ReplayCodeHistory(context.Background(), testContractID)
		if !errors.Is(err, ErrCodeHistoryTruncated) {
			t.Fatalf("err = %v, want ErrCodeHistoryTruncated", err)
		}
	})
	t.Run("no instance rows refuses", func(t *testing.T) {
		c := replayStub{watermark: []any{uint32(60_000_000)}}.conn(t)
		_, err := (&ExplorerReader{conn: c}).ReplayCodeHistory(context.Background(), testContractID)
		if !errors.Is(err, ErrContractWasmUnresolved) {
			t.Fatalf("err = %v, want ErrContractWasmUnresolved", err)
		}
	})
	t.Run("SAC", func(t *testing.T) {
		c := replayStub{watermark: []any{uint32(60_000_000)}, latest: [][]any{{uint8(1), ""}}}.conn(t)
		_, err := (&ExplorerReader{conn: c}).ReplayCodeHistory(context.Background(), testContractID)
		if !errors.Is(err, ErrContractIsSAC) {
			t.Fatalf("err = %v, want ErrContractIsSAC", err)
		}
	})
}
