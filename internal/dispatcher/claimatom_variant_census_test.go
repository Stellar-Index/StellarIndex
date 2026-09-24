// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package dispatcher_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// claimAtomVariantSwitchSites are the only non-test files allowed to name a
// ClaimAtom discriminant. Everything else destructures an atom through
// internal/sdexclaim, so a new variant is taught to one place and the
// census, the lake counter and the classic-movements path-payment leg
// cannot disagree about which atoms they understand.
var claimAtomVariantSwitchSites = map[string]string{
	"internal/sdexclaim/sdexclaim.go": "the canonical destructuring (parts)",
	"internal/sources/sdex/decode.go": "the trade decoder, which needs each variant's maker identity " +
		"(seller strkey vs pool id); its drop behaviour is pinned against sdexclaim by " +
		"TestClaimAtomCount_LockStepWithDecoder",
}

// TestClaimAtomVariantSwitchesAreCentralised walks every Go source in the
// module rather than a list of known copies, so a new variant switch is
// caught wherever it is written.
func TestClaimAtomVariantSwitchesAreCentralised(t *testing.T) {
	t.Parallel()

	got := map[string]bool{}
	for _, p := range goSourcesContaining(t, "ClaimAtomTypeClaimAtomType") {
		rel := filepath.ToSlash(strings.TrimPrefix(p, filepath.Join("..", "..")+string(filepath.Separator)))
		got[rel] = true
		if _, ok := claimAtomVariantSwitchSites[rel]; !ok {
			t.Errorf("%s switches on ClaimAtom variants itself; destructure through "+
				"internal/sdexclaim (BoughtSide, IsRealTrade) so a new variant is handled in one place", rel)
		}
	}
	for site, why := range claimAtomVariantSwitchSites {
		if !got[site] {
			t.Errorf("allowlisted %s (%s) no longer names a ClaimAtom variant — drop it from "+
				"claimAtomVariantSwitchSites so the list only shrinks", site, why)
		}
	}
}

// goSourcesContaining returns the non-test Go files under the module's
// cmd/, internal/ and pkg/ trees whose source contains needle, as paths
// relative to this package directory.
func goSourcesContaining(t *testing.T, needle string) []string {
	t.Helper()
	root := filepath.Join("..", "..")
	var out []string
	scanned := 0
	for _, top := range []string{"cmd", "internal", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			if bytes.Contains(src, []byte(needle)) {
				out = append(out, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no Go sources — the census would pass vacuously")
	}
	sort.Strings(out)
	return out
}
