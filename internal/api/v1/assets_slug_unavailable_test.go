// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// failingSlugAssetReader is a classic-slug resolver whose registry read fails.
type failingSlugAssetReader struct{ v1.AssetReader }

func (failingSlugAssetReader) ClassicAssetBySlug(context.Context, string) (string, string, bool, error) {
	return "", "", false, errors.New("registry read timed out")
}

// A slug the registry could not be asked about is not an invalid id: it answers a retryable 503, while a slug the registry
// read and did not find stays 400.
func TestAssetGet_SlugReadErrorIsUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reader v1.AssetReader
		want   int
	}{
		{"read error", failingSlugAssetReader{&stubAssetReader{}}, http.StatusServiceUnavailable},
		{"miss", slugStubAssetReader{AssetReader: &stubAssetReader{}}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := startHTTPTest(t, v1.New(v1.Options{Assets: tc.reader}).Handler())
			resp := mustGet(t, ts.URL+"/v1/assets/nope-gzzzz")
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.want, body)
			}
			if retry := resp.Header.Get("Retry-After"); (tc.want == http.StatusServiceUnavailable) != (retry != "") {
				t.Errorf("Retry-After = %q on a %d", retry, tc.want)
			}
		})
	}
}
