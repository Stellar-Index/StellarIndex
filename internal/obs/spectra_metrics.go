// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package obs

import "github.com/prometheus/client_golang/prometheus"

// SpectraUnlistedInfrastructureTotal counts Spectra registry *_change events
// naming infrastructure outside the audited set; `kind` is the change class.
var SpectraUnlistedInfrastructureTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "stellarindex_spectra_unlisted_infrastructure_total",
		Help: "Spectra registry *_change events naming a factory, router, order engine or token WASM outside the hand-kept audited set; each leaves the gate incomplete.",
	},
	[]string{"kind"},
)

// Pre-seeded so the alert's increase() sees the first event: a series born
// at 1 has no earlier sample to rise from.
func init() {
	Registry.MustRegister(SpectraUnlistedInfrastructureTotal)
	for _, kind := range []string{
		"factory_change", "router_change", "limit_order_engine_change",
		"pt_wasm_hash_change", "yt_wasm_hash_change",
	} {
		SpectraUnlistedInfrastructureTotal.WithLabelValues(kind)
	}
}
