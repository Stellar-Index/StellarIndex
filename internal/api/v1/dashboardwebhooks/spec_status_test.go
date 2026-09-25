package dashboardwebhooks

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// TestSpecDeclaresParseAndAuthoriseStatuses keeps the published contract
// of every /dashboard/webhooks/{id}* operation in step with
// parseAndAuthorise, which answers 400 for a malformed id and 404 for an
// absent or cross-account one. DELETE once promised "204 on absent" and
// declared neither, so a generated client read a retried delete's 404 as
// an undeclared error (CA2-A03-correct-2).
func TestSpecDeclaresParseAndAuthoriseStatuses(t *testing.T) {
	raw, err := os.ReadFile("../../../../openapi/stellar-index.v1.yaml")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			Responses map[string]any `yaml:"responses"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	checked := 0
	for path, ops := range spec.Paths {
		if !strings.HasPrefix(path, "/dashboard/webhooks/{id}") {
			continue
		}
		for method, op := range ops {
			if method == "parameters" {
				continue
			}
			checked++
			for _, status := range []string{"400", "404"} {
				if _, ok := op.Responses[status]; !ok {
					t.Errorf("%s %s does not declare %s, which parseAndAuthorise returns", strings.ToUpper(method), path, status)
				}
			}
		}
	}
	if checked < 3 {
		t.Fatalf("checked %d /dashboard/webhooks/{id} operations, want >= 3 (PATCH, DELETE, deliveries)", checked)
	}
}

// TestHandleDelete_AbsentID404 pins the behaviour the spec now documents:
// deleting an id this account does not have is 404, never a 204.
func TestHandleDelete_AbsentID404(t *testing.T) {
	h, _, sc := newTestRig(t)
	absent := uuid.New().String()
	req := sessionReq(t, http.MethodDelete, "/v1/dashboard/webhooks/"+absent, nil, sc)
	req.SetPathValue("id", absent)
	w := httptest.NewRecorder()
	h.HandleDelete(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}
