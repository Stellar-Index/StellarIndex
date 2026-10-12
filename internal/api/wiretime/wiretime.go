// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// Package wiretime is the canonical JSON rendering of an instant on every
// client-facing wire under internal/api. It is a leaf so the v1 handlers,
// their dashboard sub-packages and the SSE producers can all share it.
package wiretime

import (
	"strconv"
	"time"
)

// Time is the canonical JSON rendering of an instant on a client-facing wire: RFC 3339, always UTC, literal `Z`. A plain time.Time marshals in its own location, and Postgres decodes `timestamptz` into the process's local zone, leaking a DST-dependent offset; TestWireTime_NoRawTimeOnTheWire enforces its use over internal/api.
type Time time.Time

// layout is [time.RFC3339Nano] pinned to a `Z` offset — the
// layout [time.Time.MarshalJSON] itself uses, minus the `Z07:00`
// escape hatch that lets a local offset through.
const layout = "2006-01-02T15:04:05.999999999Z"

// MarshalJSON renders the instant as an RFC 3339 UTC string; years outside [0,9999] are a corrupt read, so it renders null rather than failing the whole response.
func (t Time) MarshalJSON() ([]byte, error) {
	u := time.Time(t).UTC()
	if y := u.Year(); y < 0 || y > 9999 {
		return []byte("null"), nil
	}
	b := make([]byte, 0, len(layout)+2)
	b = append(b, '"')
	b = u.AppendFormat(b, layout)
	b = append(b, '"')
	return b, nil
}

// UnmarshalJSON parses an RFC 3339 string back into UTC so clients and handler tests can round-trip a response.
func (t *Time) UnmarshalJSON(b []byte) error {
	raw := string(b)
	if raw == "null" {
		*t = Time(time.Time{})
		return nil
	}
	s, err := strconv.Unquote(raw)
	if err != nil {
		return err
	}
	if s == "" {
		*t = Time(time.Time{})
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return err
	}
	*t = Time(parsed.UTC())
	return nil
}

// Time returns the underlying instant, for handler-side arithmetic and comparison.
func (t Time) Time() time.Time { return time.Time(t) }

// IsZero reports whether the instant is the zero time, the test `omitempty` wire fields need since encoding/json never omits a struct-typed field.
func (t Time) IsZero() bool { return time.Time(t).IsZero() }

// Before compares two wire timestamps; Time deliberately does not inherit time.Time's method set (so a raw time.Time cannot be assigned onto a wire field by accident), so ordering is provided here.
func (t Time) Before(u Time) bool { return time.Time(t).Before(time.Time(u)) }

// After is Before's mirror; see its comment.
func (t Time) After(u Time) bool { return time.Time(t).After(time.Time(u)) }

// String renders the same text MarshalJSON does, without the quotes.
func (t Time) String() string { return time.Time(t).UTC().Format(layout) }

// NilIfZero lifts t onto the wire as an optional field: nil for the zero time, so an `omitempty` pointer is absent rather than year 1.
func NilIfZero(t time.Time) *Time {
	if t.IsZero() {
		return nil
	}
	w := Time(t)
	return &w
}
