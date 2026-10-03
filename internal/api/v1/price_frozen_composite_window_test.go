// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// windowMetaPairs is lkgPairs plus router meta keyed like it, so a test can
// tell which (pair, window) the handler asked the composite meta for.
type windowMetaPairs struct {
	lkgPairs
	metas map[string][]byte
}

func (w windowMetaPairs) LookupCompositeMeta(
	_ context.Context, base, quote canonical.Asset, window time.Duration,
) ([]byte, bool, error) {
	m, ok := w.metas[base.String()+"/"+quote.String()+"/"+strconv.Itoa(int(window/time.Second))]
	return m, ok, nil
}

type windowRecordingConfidence struct{ windows []time.Duration }

func (c *windowRecordingConfidence) LookupConfidence(
	_ context.Context, _, _ canonical.Asset, window time.Duration,
) (v1.PriceSnapshotConfidence, bool, error) {
	c.windows = append(c.windows, window)
	return v1.PriceSnapshotConfidence{}, false, nil
}

// A held value's confidence is the one its own window scored.
func TestPrice_FrozenHeldValueLooksUpConfidenceForItsOwnWindow(t *testing.T) {
	conf := &windowRecordingConfidence{}
	srv := v1.New(v1.Options{
		Prices:       movedBucketReader(),
		Freeze:       frozenPairs{xlmGBP: true},
		Triangulated: lkgPairs{xlmGBP + "/3600": heldLKG},
		Confidence:   conf,
	})
	ts := startHTTPTest(t, srv.Handler())

	if status, body := getBody(t, ts.URL+"/v1/price?asset=crypto:XLM&quote=fiat:GBP"); status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if len(conf.windows) == 0 || conf.windows[0] != time.Hour {
		t.Errorf("confidence looked up for windows %v, want the held 1h window", conf.windows)
	}
}

// The held value's router-quality flags describe the window that holds it,
// not the 5m default used before the held window is known.
func TestPrice_FrozenHeldValueCarriesItsOwnWindowCompositeFlags(t *testing.T) {
	srv := v1.New(v1.Options{
		Prices: movedBucketReader(),
		Freeze: frozenPairs{xlmGBP: true},
		Triangulated: windowMetaPairs{
			lkgPairs: lkgPairs{xlmGBP + "/3600": heldLKG},
			metas: map[string][]byte{
				xlmGBP + "/3600": []byte(`{"path_count":1,"combined_confidence":0.9,"diverged":true,"rerouted":false}`),
			},
		},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+"/v1/price?asset=crypto:XLM&quote=fiat:GBP")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	for _, want := range []string{`"window_seconds":3600`, `"frozen":true`, `"diverged":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
}
