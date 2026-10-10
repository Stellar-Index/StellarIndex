// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"path/filepath"
)

// The script rewrites `trades` over a range years behind tip, and no
// trades continuous aggregate's refresh policy looks back that far. Without
// an explicit refresh the served OHLC / volume / TWAP keep the pre-repair
// rows for good. These tests EXECUTE the shipped script (see
// ch_rebuild_projected_script_test.go for the stubs).

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
