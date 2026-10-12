// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"path/filepath"
)

// The script rewrites `trades` years behind tip, beyond any trades continuous aggregate's refresh lookback, so without
// an explicit refresh served OHLC / volume / TWAP keep the pre-repair rows. These tests execute the shipped script
// (stubs in ch_rebuild_projected_script_test.go).

func (r scriptRun) caggRefreshes() []scriptCall {
	var out []scriptCall
	for _, c := range r.calls {
		if c.isCAGGRefresh() {
			out = append(out, c)
		}
	}
	return out
}

func staleCAGGFile(dir string) string {
	return filepath.Join(dir, "state", "rebuild-done-windows.txt.caggs")
}
