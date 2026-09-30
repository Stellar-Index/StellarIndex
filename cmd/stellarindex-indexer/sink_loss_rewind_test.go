// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeRewinder records RewindCursor calls against a scripted cursor.
type fakeRewinder struct {
	fakeCursorStore
	rewindErr  error
	rewinds    int
	rewoundTo  uint32
	rewoundSrc string
	rewoundSub string
}

func (f *fakeRewinder) RewindCursor(_ context.Context, source, sub string, last uint32) (uint32, error) {
	f.rewinds++
	f.rewoundSrc, f.rewoundSub, f.rewoundTo = source, sub, last
	return f.cursor.LastLedger, f.rewindErr
}

func TestRewindCursorForSinkLoss(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cases := []struct {
		name            string
		cursor          uint32
		getErr          error
		rewindErr       error
		loss            pipeline.ShutdownLoss
		producerStopped bool
		wantRewound     bool
		wantRewinds     int
		wantTo          uint32
	}{
		{name: "abandoned on-chain trades rewind below the lowest", cursor: 1250, loss: pipeline.ShutdownLoss{Rows: 3, MinLedger: 1200}, producerStopped: true, wantRewound: true, wantRewinds: 1, wantTo: 1199},
		{name: "mixed loss still rewinds for the on-chain part", cursor: 1250, loss: pipeline.ShutdownLoss{Rows: 4, MinLedger: 1200, LedgerUnknown: true}, producerStopped: true, wantRewound: true, wantRewinds: 1, wantTo: 1199},
		{name: "no loss", cursor: 1250, producerStopped: true},
		{name: "ledger-less loss only", cursor: 1250, loss: pipeline.ShutdownLoss{Rows: 2, LedgerUnknown: true}, producerStopped: true},
		{name: "producer still running", cursor: 1250, loss: pipeline.ShutdownLoss{Rows: 3, MinLedger: 1200}},
		{name: "cursor never passed the loss", cursor: 1199, loss: pipeline.ShutdownLoss{Rows: 3, MinLedger: 1200}, producerStopped: true},
		{name: "cursor read fails", getErr: errors.New("conn refused"), loss: pipeline.ShutdownLoss{Rows: 3, MinLedger: 1200}, producerStopped: true},
		{name: "rewind fails, not fatal", cursor: 1250, rewindErr: errors.New("conn refused"), loss: pipeline.ShutdownLoss{Rows: 3, MinLedger: 1200}, producerStopped: true, wantRewinds: 1, wantTo: 1199},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeRewinder{
				fakeCursorStore: fakeCursorStore{cursor: timescale.Cursor{LastLedger: tc.cursor}, err: tc.getErr},
				rewindErr:       tc.rewindErr,
			}
			got := rewindCursorForSinkLoss(context.Background(), f, tc.loss, tc.producerStopped, logger)
			if got != tc.wantRewound {
				t.Errorf("rewound = %v, want %v", got, tc.wantRewound)
			}
			if f.rewinds != tc.wantRewinds {
				t.Fatalf("RewindCursor calls = %d, want %d", f.rewinds, tc.wantRewinds)
			}
			if tc.wantRewinds > 0 && (f.rewoundTo != tc.wantTo || f.rewoundSrc != cursorSource || f.rewoundSub != "") {
				t.Errorf("RewindCursor(%q, %q, %d), want (%q, \"\", %d)", f.rewoundSrc, f.rewoundSub, f.rewoundTo, cursorSource, tc.wantTo)
			}
		})
	}
}

// TestIndexerActsOnSinkShutdownLoss pins the wiring: PersistEvents'
// ShutdownLoss must be kept and handed to the rewind once the sink is
// done. Discarding it leaves an abandoned trade behind a cursor that
// has already moved past it.
func TestIndexerActsOnSinkShutdownLoss(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	isCall := func(e ast.Expr, name string) bool {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			return fn.Sel.Name == name
		case *ast.Ident:
			return fn.Name == name
		}
		return false
	}
	var assigned, discarded, rewoundOnSinkDone bool
	ast.Inspect(file, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if len(s.Rhs) == 1 && isCall(s.Rhs[0], "PersistEvents") {
				assigned = true
			}
		case *ast.ExprStmt:
			if isCall(s.X, "PersistEvents") {
				discarded = true
			}
		case *ast.CommClause:
			recv, ok := s.Comm.(*ast.ExprStmt)
			if !ok {
				return true
			}
			u, ok := recv.X.(*ast.UnaryExpr)
			if !ok || u.Op != token.ARROW {
				return true
			}
			if id, ok := u.X.(*ast.Ident); !ok || id.Name != "sinkDone" {
				return true
			}
			for _, st := range s.Body {
				if es, ok := st.(*ast.ExprStmt); ok && isCall(es.X, "rewindCursorForSinkLoss") {
					rewoundOnSinkDone = true
				}
			}
		}
		return true
	})
	if discarded || !assigned {
		t.Error("main.go discards pipeline.PersistEvents' ShutdownLoss — an abandoned on-chain trade is then only a counter and a log line")
	}
	if !rewoundOnSinkDone {
		t.Error("the `case <-sinkDone:` arm does not call rewindCursorForSinkLoss — the cursor stays past trades the sink abandoned")
	}
}
