// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/wiretime"
)

// WireTime is the canonical v1 wire rendering of an instant: RFC 3339, always UTC, literal `Z`; see [wiretime.Time].
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

// wireTimeOrNil lifts an instant that may never have happened onto the wire as absent: a struct-typed WireTime ignores `omitempty`, so
// the zero time would render "0001-01-01T00:00:00Z".
func wireTimeOrNil(t time.Time) *WireTime {
	if t.IsZero() {
		return nil
	}
	w := WireTime(t)
	return &w
}

// wireTimeUnptr is [wireTimePtr]'s inverse, preserving nil.
func wireTimeUnptr(t *WireTime) *time.Time {
	if t == nil {
		return nil
	}
	u := time.Time(*t)
	return &u
}
