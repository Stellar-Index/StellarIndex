// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"strings"
	"testing"
)

// TestWindowedCHBackfills_RefuseImplicitFullRun pins GH-1192: every windowed
// ClickHouse backfill must refuse a bare invocation (no -from/-to/-full)
// rather than silently starting the whole ledger-2..tip history outside
// run-heavy-job.sh. ch-txindex-backfill already enforced this (W8.15); its
// two siblings did not, despite doc comments claiming parity. The refusal
// must fire before any ClickHouse connection is attempted, so these run
// without a live lake.
func TestWindowedCHBackfills_RefuseImplicitFullRun(t *testing.T) {
	cases := []struct {
		name string
		run  func(args []string) error
	}{
		{"ch-contract-ledgers-backfill", chContractLedgersBackfill},
		{"ch-instance-backfill", chInstanceBackfill},
		{"ch-txindex-backfill", func(args []string) error {
			_, err := parseTxIndexBackfillFlags(args)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run([]string{"-write"})
			if err == nil {
				t.Fatalf("%s: bare invocation (no -from/-to/-full) was accepted — it must refuse the implicit full-history backfill", tc.name)
			}
			if !strings.Contains(err.Error(), "full-history") {
				t.Fatalf("%s: refusal error should explain the full-history footgun, got: %v", tc.name, err)
			}
		})
	}
}
