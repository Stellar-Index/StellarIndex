package dashboardwebhooks

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// TestSpecEventTypeEnumsMatchPlatform keeps every OpenAPI enum that
// enumerates webhook event types (one spanning more than one event family,
// e.g. incident.* and price.*) equal to platform.WebhookEventTypes(). The
// spec is what the explorer's generated types and customers read, so a
// type missing there is as unsubscribable as one missing from
// validateEvents (GH-1348). Single-family enums — a payload's own `event`
// field — are subsets by design and are not checked.
func TestSpecEventTypeEnumsMatchPlatform(t *testing.T) {
	spec, err := os.ReadFile("../../../../openapi/stellar-index.v1.yaml")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	want := make([]string, 0, len(platform.WebhookEventTypes()))
	for _, e := range platform.WebhookEventTypes() {
		want = append(want, string(e))
	}
	slices.Sort(want)

	checked := 0
	for _, enum := range specEnums(strings.Split(string(spec), "\n")) {
		if !spansEventFamilies(enum.values) {
			continue
		}
		checked++
		got := slices.Clone(enum.values)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("openapi line %d: event-type enum %v, want %v", enum.line, got, want)
		}
	}
	if checked == 0 {
		t.Fatal("found no event-type enum in the spec — the parser is not looking at the right shape")
	}
}

type specEnum struct {
	line   int
	values []string
}

// specEnums extracts every `enum:` list, inline (`enum: [a, b]`) or block
// (`enum:` followed by `- a` lines).
func specEnums(lines []string) []specEnum {
	var out []specEnum
	for i, ln := range lines {
		idx := strings.Index(ln, "enum:")
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(ln[idx+len("enum:"):])
		if open := strings.Index(rest, "["); open >= 0 {
			if end := strings.Index(rest, "]"); end > open {
				var vals []string
				for _, v := range strings.Split(rest[open+1:end], ",") {
					vals = append(vals, strings.Trim(strings.TrimSpace(v), `"'`))
				}
				out = append(out, specEnum{line: i + 1, values: vals})
			}
			continue
		}
		var vals []string
		for _, next := range lines[i+1:] {
			item := strings.TrimSpace(next)
			if !strings.HasPrefix(item, "- ") {
				break
			}
			vals = append(vals, strings.Trim(strings.TrimSpace(item[2:]), `"'`))
		}
		out = append(out, specEnum{line: i + 1, values: vals})
	}
	return out
}

// spansEventFamilies reports whether values are webhook event types from
// at least two families (the part before the dot).
func spansEventFamilies(values []string) bool {
	families := map[string]bool{}
	for _, v := range values {
		if !platform.IsWebhookEventType(v) {
			continue
		}
		family, _, _ := strings.Cut(v, ".")
		families[family] = true
	}
	return len(families) > 1
}
