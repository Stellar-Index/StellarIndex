// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// A pool token whose scale was not read (no METADATA, or the batch read failed) serves no decimals and a base-unit reserve:
// a stamped 7 would have a client divide an 18-dp reserve by 10^7.
func TestBuildPoolReservesRow_UnreadScaleIsAbsent(t *testing.T) {
	st := clickhouse.SoroswapPairState{Pair: "CPAIR", Token0: "CT0", Token1: "CT1", Reserve0: big.NewInt(1000), Reserve1: big.NewInt(2000), Ledger: 9}
	declared := map[string]clickhouse.TokenDisplayMeta{"CT0": {Decimals: 6, HasMeta: true}}
	for _, tc := range []struct {
		name       string
		displays   map[string]clickhouse.TokenDisplayMeta
		displaysOK bool
		want0      string
	}{
		{"token declares no scale", declared, true, `"decimals":6`},
		{"metadata read failed", nil, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := buildPoolReservesRow(st, tc.displays, tc.displaysOK)
			if row.Token1.Decimals != nil {
				t.Errorf("token1 decimals = %d, want absent", *row.Token1.Decimals)
			}
			if row.MidPrice0In1 != nil || row.MidPrice1In0 != nil {
				t.Errorf("mid prices served with an unread scale")
			}
			b, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			body := string(b)
			if n := strings.Count(body, `"decimals"`); (tc.want0 == "" && n != 0) || (tc.want0 != "" && (n != 1 || !strings.Contains(body, tc.want0))) {
				t.Errorf("wire decimals wrong (want only %q): %s", tc.want0, body)
			}
		})
	}
}
