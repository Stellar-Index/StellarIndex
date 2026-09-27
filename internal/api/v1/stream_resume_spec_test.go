package v1

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// streamResumeHandlers maps each spec path that may declare a
// Last-Event-ID parameter to the handler source that must read it.
var streamResumeHandlers = map[string]string{
	"/price/stream":     "price_stream.go",
	"/price/tip/stream": "price_tip_stream.go",
}

// TestSpec_LastEventIDDeclaredOnlyWhereAHandlerResumes — the spec once
// declared Last-Event-ID on /observations/stream and /ledger/stream,
// whose per-connection handlers never read it, so a spec-generated client
// implementing resume silently lost everything published while it was
// disconnected. A declaration now needs an entry above, and the entry's
// handler must call streaming.LastEventIDFrom.
func TestSpec_LastEventIDDeclaredOnlyWhereAHandlerResumes(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "openapi", "stellar-index.v1.yaml")) //nolint:gosec // repo-relative path
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	var declared []string
	for path, methods := range doc.Paths {
		for _, node := range methods {
			var op struct {
				Parameters []struct {
					Name string `yaml:"name"`
					In   string `yaml:"in"`
				} `yaml:"parameters"`
			}
			if node.Kind != yaml.MappingNode || node.Decode(&op) != nil {
				continue
			}
			for _, p := range op.Parameters {
				if strings.EqualFold(p.Name, "Last-Event-ID") {
					declared = append(declared, path)
				}
			}
		}
	}
	sort.Strings(declared)
	if len(declared) == 0 {
		t.Fatal("no operation declares Last-Event-ID; the spec parse found nothing to check")
	}
	for _, path := range declared {
		src, ok := streamResumeHandlers[path]
		if !ok {
			t.Errorf("%s declares Last-Event-ID but no handler is registered as resuming it", path)
			continue
		}
		code, err := os.ReadFile(src) //nolint:gosec // package-relative fixture path
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(code), "streaming.LastEventIDFrom(r)") {
			t.Errorf("%s declares Last-Event-ID but %s never calls streaming.LastEventIDFrom", path, src)
		}
	}
}
