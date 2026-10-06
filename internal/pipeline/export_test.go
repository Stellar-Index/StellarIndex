// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pipeline

// SwapSpecsForTest replaces the spec list, and the event index built from
// it, until restore runs. Not safe for parallel tests.
func SwapSpecsForTest(ss []SourceSpec) (restore func()) {
	oldSpecs, oldRoles := specs, eventRoles
	specs, eventRoles = ss, indexEventRoles(ss)
	return func() { specs, eventRoles = oldSpecs, oldRoles }
}
