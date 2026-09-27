// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/wiretime"
)

// WireTime is the canonical JSON rendering of an instant on the v1 wire:
// RFC 3339, always UTC, always a literal `Z`. See [wiretime.Time], which
// lives in a leaf package so v1's sub-packages can use it too.
type WireTime = wiretime.Time

// wireTimePtr lifts an optional instant onto the wire, preserving
// "absent" as nil rather than collapsing it to the zero time.
func wireTimePtr(t *time.Time) *WireTime {
	if t == nil {
		return nil
	}
	w := WireTime(*t)
	return &w
}

// wireTimeUnptr is [wireTimePtr]'s inverse: it hands an optional wire
// instant back to handler-side code that works in [time.Time],
// preserving nil.
func wireTimeUnptr(t *WireTime) *time.Time {
	if t == nil {
		return nil
	}
	u := time.Time(*t)
	return &u
}
