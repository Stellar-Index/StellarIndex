// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestPriceAuthority_SpecEnumIsTheGoVocabulary — GlobalAssetView's
// price_authority enum carries every aggregate.PriceAuthority constant,
// in declaration order: a label the handler can stamp but the spec omits
// is a value the closed union rejects.
func TestPriceAuthority_SpecEnumIsTheGoVocabulary(t *testing.T) {
	got := specPropertyEnum(t, "GlobalAssetView", "price_authority")
	want := goFileStringConsts(t, filepath.Join(repoRoot(t), "internal", "aggregate", "global.go"), "PriceAuthority")
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("openapi GlobalAssetView.price_authority enum = %v\n  aggregate.PriceAuthority consts        = %v\n"+
			"edit both (and regenerate docs/reference/api, examples/postman and "+
			"web/explorer/src/api/types.ts from the spec)", got, want)
	}
}
