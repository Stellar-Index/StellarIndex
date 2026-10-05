// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// soroswap-router is BackfillPerWASM, so backfill-router's gate has no skip:
// a lake it cannot read refuses the walk.
func TestBackfillRouterGate_UnreachableLakeRefuses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var cfg config.Config
	cfg.Storage.ClickHouseAddr = "127.0.0.1:1"
	err := gateBackfillRouter(ctx, cfg, nil, 60_000_000, 60_100_000)
	if err == nil || !strings.Contains(err.Error(), "wasm replay gate") {
		t.Fatalf("err = %v, want a wasm replay gate refusal", err)
	}
}
