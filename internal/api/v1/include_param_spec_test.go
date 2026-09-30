package v1

import (
	"regexp"
	"strings"
	"testing"
)

// TestIncludeParamSpecCoversHandlerValues pins the spec's `include` query
// parameter to the values each handler's include switch accepts. The lists
// mirror handleMarkets and handleSources; a spec-validating client must be
// able to send every value, alone and combined.
func TestIncludeParamSpecCoversHandlerValues(t *testing.T) {
	doc := loadSpecDoc(t)
	for path, accepted := range map[string][]string{
		"/markets": {"sparkline", "inception"},
		"/sources": {"stats", "sparkline", "sparkline7d"},
	} {
		re := regexp.MustCompile(specQueryParamPattern(t, doc, path, "include"))
		for _, v := range append(accepted, strings.Join(accepted, ",")) {
			if !re.MatchString(v) {
				t.Errorf("spec GET %s ?include= pattern rejects handler-accepted %q", path, v)
			}
		}
		if re.MatchString("bogus") {
			t.Errorf("spec GET %s ?include= pattern accepts an unknown value", path)
		}
	}
}

func specQueryParamPattern(t *testing.T, doc map[string]any, path, param string) string {
	t.Helper()
	paths, _ := doc["paths"].(map[string]any)
	item, _ := paths[path].(map[string]any)
	get, _ := item["get"].(map[string]any)
	params, _ := get["parameters"].([]any)
	for _, p := range params {
		pm, _ := p.(map[string]any)
		if pm["name"] == param && pm["in"] == "query" {
			schema, _ := pm["schema"].(map[string]any)
			pat, _ := schema["pattern"].(string)
			if pat == "" {
				t.Fatalf("spec GET %s ?%s has no schema pattern", path, param)
			}
			return pat
		}
	}
	t.Fatalf("spec GET %s has no query parameter %q", path, param)
	return ""
}
