// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// TestSpecDeclaresEveryXReasonTheServerRequires probes every write operation in the spec as an operator with no
// X-Reason; any that answers `missing-reason` must declare the header as an `in: header` parameter, since the
// client library and the generated clients only send what the spec declares.
func TestSpecDeclaresEveryXReasonTheServerRequires(t *testing.T) {
	body, err := os.ReadFile("../../../openapi/stellar-index.v1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Parameters []struct {
				Ref  string `yaml:"$ref"`
				Name string `yaml:"name"`
				In   string `yaml:"in"`
			} `yaml:"parameters"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	h := v1.New(v1.Options{Auth: fakeAuthMiddleware(operatorSubject()), Accounts: &fakeAccountStore{}}).Handler()
	pathParam := regexp.MustCompile(`\{[^}]+\}`)

	requiring := 0
	for path, item := range doc.Paths {
		for method, op := range item {
			method = strings.ToUpper(method)
			if method == http.MethodGet || method == http.MethodHead || method == "PARAMETERS" {
				continue
			}
			url := "/v1" + pathParam.ReplaceAllString(path, "kid_00000000000000a1")
			if !answersMissingReason(h, method, url) {
				continue
			}
			requiring++
			declared := false
			for _, p := range op.Parameters {
				declared = declared || (p.In == "header" && p.Name == "X-Reason")
			}
			if !declared {
				t.Errorf("%s %s answers 400 missing-reason without X-Reason but the spec does not declare it "+
					"as an `in: header` parameter", method, path)
			}
		}
	}
	if requiring < 7 {
		t.Fatalf("only %d operations demanded X-Reason; the probe no longer reaches the reason checks", requiring)
	}
}

// answersMissingReason reports whether one request draws the missing-reason problem; a handler that panics on
// the probe's empty stores did not reach a reason check.
func answersMissingReason(h http.Handler, method, url string) (yes bool) {
	defer func() {
		if recover() != nil {
			yes = false
		}
	}()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, url, strings.NewReader(`{"label":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		return false
	}
	var p struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	return strings.HasSuffix(p.Type, "/missing-reason")
}
