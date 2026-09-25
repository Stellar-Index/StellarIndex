package timescale

import (
	"os"
	"strings"
	"testing"
)

// docCommentAbove returns the contiguous "//" comment block immediately
// preceding the line containing declLine, joined into one
// whitespace-normalized string. Used to test the doc comment on a
// specific declaration in isolation, rather than the whole file (which
// would also match unrelated code, e.g. a field or SQL clause that
// happens to share a substring with a stale doc phrase).
func docCommentAbove(t *testing.T, text, declLine string) string {
	t.Helper()
	lines := strings.Split(text, "\n")
	idx := -1
	for i, l := range lines {
		if strings.Contains(l, declLine) {
			idx = i
			break
		}
	}
	if idx == -1 {
		t.Fatalf("declaration %q not found", declLine)
	}
	var comment []string
	for i := idx - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "//") {
			break
		}
		comment = append([]string{strings.TrimSpace(strings.TrimPrefix(trimmed, "//"))}, comment...)
	}
	return strings.Join(comment, " ")
}

// TestOnConflictDocsMatchGenerationGuardedUpsert guards GH-1016: doc
// comments on writers backed by an INV-3 generation-guarded corrective
// ON CONFLICT ... DO UPDATE (migration 0110) must not describe the
// pre-migration DO NOTHING / no-op / positional-PK behaviour those
// upserts replaced. A doc that still claims "no-op" or omits a PK
// column the query actually conflicts on is stale in exactly the way
// #612/#934 were.
func TestOnConflictDocsMatchGenerationGuardedUpsert(t *testing.T) {
	cases := []struct {
		name           string
		file           string
		decl           string
		mustNotContain []string
		mustContain    []string
	}{
		{
			name:        "blend_emitter distribute doc",
			file:        "blend_emitter.go",
			decl:        "func (s *Store) InsertBlendEmitterDistribute",
			mustContain: []string{"generation"},
		},
		{
			name:           "blend_positions insert doc",
			file:           "blend_positions.go",
			decl:           "func (s *Store) InsertBlendPositionEvent",
			mustNotContain: []string{"no-op"},
			mustContain:    []string{"generation"},
		},
		{
			name: "soroswap_router_swaps struct doc",
			file: "soroswap_router_swaps.go",
			decl: "type SoroswapRouterSwap struct",
			// op_index alone is the pre-migration-0056 identity tuple;
			// call_sig is the PK discriminator migration 0056 added.
			mustContain: []string{"call_sig"},
		},
		{
			name:        "soroswap_router_swaps insert doc",
			file:        "soroswap_router_swaps.go",
			decl:        "func (s *Store) InsertSoroswapRouterSwap",
			mustContain: []string{"call_sig"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatalf("read %s: %v", tc.file, err)
			}
			doc := docCommentAbove(t, string(src), tc.decl)

			for _, frag := range tc.mustNotContain {
				if strings.Contains(doc, frag) {
					t.Errorf("%s: doc for %q still contains stale fragment %q:\n%s", tc.file, tc.decl, frag, doc)
				}
			}
			for _, frag := range tc.mustContain {
				if !strings.Contains(doc, frag) {
					t.Errorf("%s: doc for %q is missing expected fragment %q describing current ON CONFLICT identity:\n%s", tc.file, tc.decl, frag, doc)
				}
			}
		})
	}
}
