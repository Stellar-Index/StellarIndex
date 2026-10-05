// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// unreachableLakeConfig points ClickHouse at a port nothing listens on.
func unreachableLakeConfig() config.Config {
	var cfg config.Config
	cfg.Storage.ClickHouseAddr = "127.0.0.1:1"
	return cfg
}

// The per-WASM gate has no skip: a BackfillPerWASM source (cctp) whose lake
// cannot be read is refused, never admitted on the static policy alone.
func TestCHRebuildWriteGate_UnreachableLakeRefusesPerWASMSource(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := gateCHRebuildLake(ctx, unreachableLakeConfig(), nil, []string{"cctp"}, 62_200_000, 62_300_000)
	if err == nil || !strings.Contains(err.Error(), "wasm replay gate") {
		t.Fatalf("err = %v, want a wasm replay gate refusal", err)
	}
}

func TestProjectedRebuildGate_UnreachableLakeRefusesPerWASMSource(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := gateProjectedRebuild(ctx, unreachableLakeConfig(), nil, "cctp", 62_200_000, 62_300_000)
	if err == nil || !strings.Contains(err.Error(), "wasm replay gate") {
		t.Fatalf("err = %v, want a wasm replay gate refusal", err)
	}
}
