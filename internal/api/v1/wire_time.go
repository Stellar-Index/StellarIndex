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

// wireTimeOrNil lifts a value-typed instant that may legitimately never
// have happened (an unverified email, a session that never logged in)
// onto the wire as absent rather than the zero instant. A struct-typed
// WireTime field ignores `omitempty` (it is never the empty value Go's
// encoding/json checks for), so the zero time renders as the literal
// string "0001-01-01T00:00:00Z" — which json.Unmarshal on the SDK's
// *time.Time side allocates a non-nil pointer for, indistinguishable
// from a real timestamp.
func wireTimeOrNil(t time.Time) *WireTime {
	if t.IsZero() {
		return nil
	}
	w := WireTime(t)
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
