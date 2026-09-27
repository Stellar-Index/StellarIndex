// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package customerwebhook

import (
	"os"
	"regexp"
	"strconv"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestDeliveryFailingAlertBelowSingleLaneCeiling: one endpoint's rows are
// delivered serially, each bounded by defaultHTTPTimeout, so a black-holing
// endpoint produces at most 1/defaultHTTPTimeout network_errors per second.
// A delivery_failing threshold at or above that can never fire for the
// sustained-down endpoint the alert describes; require it to fire once one
// lane is half saturated with timeouts, in both rule trees.
func TestDeliveryFailingAlertBelowSingleLaneCeiling(t *testing.T) {
	ceiling := 1 / defaultHTTPTimeout.Seconds()
	for _, path := range []string{
		"../../deploy/monitoring/rules/api.yml",
		"../../configs/prometheus/rules.r1/api.yml",
	} {
		threshold := deliveryFailingThreshold(t, path)
		if threshold > ceiling/2 {
			t.Errorf("%s: delivery_failing threshold %.3f/s exceeds half the single-lane ceiling (1/%s = %.3f/s); a black-holed endpoint cannot trip it",
				path, threshold, defaultHTTPTimeout, ceiling)
		}
	}
}

var trailingGT = regexp.MustCompile(`>\s*([0-9.]+)\s*$`)

func deliveryFailingThreshold(t *testing.T, path string) float64 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Groups []struct {
			Rules []struct {
				Alert string `yaml:"alert"`
				Expr  string `yaml:"expr"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, g := range doc.Groups {
		for _, r := range g.Rules {
			if r.Alert != "stellarindex_customer_webhook_delivery_failing" {
				continue
			}
			m := trailingGT.FindStringSubmatch(r.Expr)
			if m == nil {
				t.Fatalf("%s: delivery_failing expr has no trailing `> N`: %q", path, r.Expr)
			}
			v, err := strconv.ParseFloat(m[1], 64)
			if err != nil {
				t.Fatalf("%s: threshold %q: %v", path, m[1], err)
			}
			return v
		}
	}
	t.Fatalf("%s: stellarindex_customer_webhook_delivery_failing not found", path)
	return 0
}
