// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package orchestrator

// The producer's ceilings. A baseline window of D days holds at most
// D×1440 one-minute buckets, i.e. D×1440−1 bucket-to-bucket returns, so
// baselineAgeDays can never read above (43,199+1)/1440 = 30.
const (
	maxDay30Returns    = 30*1440 - 1
	maxBaselineAgeDays = float64(maxDay30Returns+1) / 1440
)
