// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"math/big"
	"net/http"
	"slices"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A registered pool whose state could not be read is named in the envelope's `unread`, so the listing does not pass for
// the complete set of pools; a fully read listing carries no `unread`.
func TestPoolReserves_ListingNamesUnreadPools(t *testing.T) {
	pairA, pairB, pairC := mkCStrkey(t, 1), mkCStrkey(t, 2), mkCStrkey(t, 3)
	tok0, tok1 := mkCStrkey(t, 10), mkCStrkey(t, 11)
	reader := &stubExplorerReader{pairStates: map[string]clickhouse.SoroswapPairState{
		pairA: {Pair: pairA, Token0: tok0, Token1: tok1, Reserve0: big.NewInt(10), Reserve1: big.NewInt(20), Ledger: 1},
	}}
	registered := []timescale.SoroswapPair{
		{PairStrkey: pairC, Token0Strkey: tok0, Token1Strkey: tok1},
		{PairStrkey: pairA, Token0Strkey: tok0, Token1Strkey: tok1},
		{PairStrkey: pairB, Token0Strkey: tok0, Token1Strkey: tok1},
	}
	resp := mustGet(t, poolReservesTestServer(t, reader, registered)+"/v1/pools/reserves")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data   []map[string]any `json:"data"`
		Unread []string         `json:"unread"`
	}
	mustDecode(t, resp, &body)
	want := []string{pairB, pairC}
	slices.Sort(want)
	if len(body.Data) != 1 || !slices.Equal(body.Unread, want) {
		t.Fatalf("data %d rows, unread %v; want 1 row and unread %v", len(body.Data), body.Unread, want)
	}

	resp = mustGet(t, poolReservesTestServer(t, reader, registered[1:2])+"/v1/pools/reserves")
	var full map[string]any
	mustDecode(t, resp, &full)
	if u, ok := full["unread"]; ok {
		t.Errorf("unread = %v on a fully read listing, want omitted", u)
	}
}
