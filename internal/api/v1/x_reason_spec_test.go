// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// xReasonProbePanics lists the write operations that panic on the probe's empty stores, so never reach a reason
// check. Each needs a reason; a route that starts or stops panicking fails the test until this list is updated.
var xReasonProbePanics = map[string]string{}

type specParam struct {
	Ref  string `yaml:"$ref"`
	Name string `yaml:"name"`
	In   string `yaml:"in"`
}

// TestSpecDeclaresEveryXReasonTheServerRequires probes every write operation in the spec as an operator with no
// X-Reason; any that answers `missing-reason` must declare the header as an `in: header` parameter (inline, by
// `$ref`, or at path level), since the client library and the generated clients only send what the spec declares.
func TestSpecDeclaresEveryXReasonTheServerRequires(t *testing.T) {
	body, err := os.ReadFile("../../../openapi/stellar-index.v1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths      map[string]map[string]yaml.Node `yaml:"paths"`
		Components struct {
			Parameters map[string]specParam `yaml:"parameters"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	h := v1.New(v1.Options{Auth: fakeAuthMiddleware(operatorSubject()), Accounts: &fakeAccountStore{}}).Handler()
	pathParam := regexp.MustCompile(`\{[^}]+\}`)
	declaresXReason := func(where string, params []specParam) bool {
		for _, p := range params {
			if name, ok := strings.CutPrefix(p.Ref, "#/components/parameters/"); ok {
				c, found := doc.Components.Parameters[name]
				if !found {
					t.Errorf("%s: unresolved parameter $ref %q", where, p.Ref)
				}
				p = c
			}
			if p.In == "header" && p.Name == "X-Reason" {
				return true
			}
		}
		return false
	}

	requiring := 0
	panicked := map[string]bool{}
	for path, item := range doc.Paths {
		var shared []specParam
		if n, ok := item["parameters"]; ok {
			if err := n.Decode(&shared); err != nil {
				t.Fatalf("%s parameters: %v", path, err)
			}
		}
		for method, node := range item {
			method = strings.ToUpper(method)
			if !slices.Contains([]string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}, method) {
				continue
			}
			var op struct {
				Parameters []specParam `yaml:"parameters"`
			}
			if err := node.Decode(&op); err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
			url := "/v1" + pathParam.ReplaceAllString(path, "kid_00000000000000a1")
			missing, panicValue := answersMissingReason(h, method, url)
			name := method + " " + path
			if panicValue != nil {
				panicked[name] = true
				if _, listed := xReasonProbePanics[name]; !listed {
					t.Errorf("%s panicked on the probe (%v), so its X-Reason contract is unchecked; "+
						"make the probe reach it or list it in xReasonProbePanics with a reason", name, panicValue)
				}
				continue
			}
			if !missing {
				continue
			}
			requiring++
			if !declaresXReason(name, append(slices.Clone(shared), op.Parameters...)) {
				t.Errorf("%s answers 400 missing-reason without X-Reason but the spec does not declare it "+
					"as an `in: header` parameter", name)
			}
		}
	}
	for name := range xReasonProbePanics {
		if !panicked[name] {
			t.Errorf("%s is listed in xReasonProbePanics but no longer panics; remove it", name)
		}
	}
	if requiring < 7 {
		t.Fatalf("only %d operations demanded X-Reason; the probe no longer reaches the reason checks", requiring)
	}
}

// answersMissingReason reports whether one request draws the missing-reason problem, and the value of any panic the
// handler raised on the probe's empty stores (such a handler did not reach a reason check).
func answersMissingReason(h http.Handler, method, url string) (yes bool, panicValue any) {
	defer func() {
		if v := recover(); v != nil {
			yes, panicValue = false, fmt.Sprint(v)
		}
	}()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, url, strings.NewReader(`{"label":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		return false, nil
	}
	var p struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	return strings.HasSuffix(p.Type, "/missing-reason"), nil
}
