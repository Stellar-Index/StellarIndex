// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"strings"
	"testing"
)

// TestOracleUpdatesForMEVScanQueryShape guards the oracle
// capture-totality exclusion on the MEV oracle scan: `raw:` (unmapped,
// orientation-unknown) rows are record-layer only and must be dropped
// before they reach the detectors as evidence. Behaviour against real Timescale is pinned in
// test/integration/oracle_raw_consumers_test.go.
func TestOracleUpdatesForMEVScanQueryShape(t *testing.T) {
	q := oracleUpdatesForMEVScanQuery
	if !strings.Contains(q, "asset NOT LIKE 'raw:%'") {
		t.Error("OracleUpdatesForMEVScan must exclude raw: rows (asset NOT LIKE 'raw:%') — raw rows are never MEV evidence")
	}
	if !strings.Contains(q, "ledger > 0") || !strings.Contains(q, "ts > $1") {
		t.Error("OracleUpdatesForMEVScan must keep the on-chain (ledger > 0) and since-bounded predicates")
	}
}
