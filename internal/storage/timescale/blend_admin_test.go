// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
)

// A V1 pool's queue_set_reserve persists its partial config plus the
// names of the V2-only fields it lacked, and the persisted metadata
// reads back through the APY config reader.
func TestBuildAdminAttributes_V1QueueSetReserve(t *testing.T) {
	e := domain.BlendAdminEvent{
		Kind: domain.BlendEventQueueSetReserve,
		ReserveConfig: map[string]any{
			"index": uint64(1), "decimals": uint64(7), "util": uint64(8_000_000),
			"r_three": uint64(15_000_000), "reactivity": uint64(200),
		},
		ReserveConfigMissing: []string{"supply_cap", "enabled"},
	}
	b, err := json.Marshal(buildAdminAttributes(e))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Metadata json.RawMessage `json:"metadata"`
		Missing  []string        `json:"reserve_config_missing"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	if want := []string{"supply_cap", "enabled"}; !reflect.DeepEqual(got.Missing, want) {
		t.Errorf("reserve_config_missing = %v, want %v (attrs %s)", got.Missing, want, b)
	}
	cfg, err := blend.ParseReserveConfigMetadata(got.Metadata)
	if err != nil {
		t.Fatalf("ParseReserveConfigMetadata: %v", err)
	}
	if cfg.Util != 8_000_000 || cfg.RThree != 15_000_000 || !cfg.Enabled || cfg.SupplyCap != nil {
		t.Errorf("read-back config = %+v, want V1 params, enabled, nil cap", cfg)
	}

	e.ReserveConfigMissing = nil
	if _, ok := buildAdminAttributes(e)["reserve_config_missing"]; ok {
		t.Error("complete config must not carry reserve_config_missing")
	}
}
