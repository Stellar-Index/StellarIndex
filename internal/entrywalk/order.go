// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

// Package entrywalk holds the within-block change order shared by the live
// entry-change walk (dispatcher.walkLedgerEntryChanges) and the lake extract
// (clickhouse.extractLedgerEntryChanges), so both number a ledger alike.
package entrywalk

import (
	"bytes"
	"slices"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// Canonical returns one LedgerEntryChanges block stably sorted by ledger key.
//
// stellar-core lists a block's keys in hash-map iteration order, so two
// exports of the same ledger can disagree on it; a key's own changes (state
// before updated/removed, restored before updated) are always in commit
// order, and a stable sort keeps that while fixing the order across keys.
// The input is never modified: it is the ledger meta other readers share.
func Canonical(changes []xdr.LedgerEntryChange) []xdr.LedgerEntryChange {
	if len(changes) < 2 {
		return changes
	}
	keys := make([][]byte, len(changes))
	sorted := true
	for i := range changes {
		keys[i] = changeKey(changes[i])
		if i > 0 && bytes.Compare(keys[i-1], keys[i]) > 0 {
			sorted = false
		}
	}
	if sorted {
		return changes
	}
	idx := make([]int, len(changes))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int { return bytes.Compare(keys[a], keys[b]) })
	out := make([]xdr.LedgerEntryChange, len(changes))
	for i, j := range idx {
		out[i] = changes[j]
	}
	return out
}

// changeKey is the XDR encoding of the change's ledger key; nil for a change
// carrying none, which sorts first and keeps its relative order.
func changeKey(c xdr.LedgerEntryChange) []byte {
	key, err := c.LedgerKey()
	if err != nil {
		return nil
	}
	b, err := key.MarshalBinary()
	if err != nil {
		return nil
	}
	return b
}
