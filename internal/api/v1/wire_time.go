// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"strconv"
	"time"
)

// WireTime is the canonical JSON rendering of an instant on the v1
// wire: RFC 3339, always in UTC, always with a literal `Z` offset.
//
// It exists because a plain [time.Time] field marshals in whatever
// location the value happens to carry, and the values that reach a
// response struct do NOT reliably carry UTC. Rows read back from
// Postgres decode `timestamptz` into the PROCESS's local zone, so a
// handler that passes a stored timestamp straight into a `time.Time`
// json field emits the server's local offset — the same instant,
// rendered differently. Production served
// `2026-06-01T02:00:00+02:00` from /v1/price/at and
// `2017-01-17T01:00:00+01:00` from /v1/history/since-inception. Note
// the second offset: it moves with DST, so one series rendered two
// different offsets across the same grid.
//
// Every such string is schema-valid — `format: date-time` accepts an
// offset — so neither the OpenAPI contract test nor any status-code
// check could see it. The reader it hurts is the one who buckets by
// the literal string instead of the parsed instant: they mis-bucket
// by an hour, silently, on price history and trade history, which are
// the two surfaces where an hour matters most.
//
// This type fixes rendering, not storage. The underlying instant is
// unchanged, conversion is free, and for a value that was already UTC
// the bytes are IDENTICAL to what [time.Time] produced — so adopting
// it is not a response-shape change.
//
// Use it for every json-tagged timestamp field in this package. The
// rule is enforced by TestWireTime_NoRawTimeOnTheWire, which fails if
// a new response struct reaches for a raw [time.Time] instead.
type WireTime time.Time

// wireTimeLayout is [time.RFC3339Nano] pinned to a `Z` offset — the
// layout [time.Time.MarshalJSON] itself uses, minus the `Z07:00`
// escape hatch that lets a local offset through.
const wireTimeLayout = "2006-01-02T15:04:05.999999999Z"

// MarshalJSON renders the instant as an RFC 3339 UTC string.
//
// [time.Time.MarshalJSON] errors on years outside [0,9999] because
// they cannot be written as RFC 3339. A timestamp that far out of
// range is a corrupt read rather than a real observation, so rather
// than fail the entire response this renders JSON null — what an
// absent optional timestamp already renders as.
func (t WireTime) MarshalJSON() ([]byte, error) {
	u := time.Time(t).UTC()
	if y := u.Year(); y < 0 || y > 9999 {
		return []byte("null"), nil
	}
	b := make([]byte, 0, len(wireTimeLayout)+2)
	b = append(b, '"')
	b = u.AppendFormat(b, wireTimeLayout)
	b = append(b, '"')
	return b, nil
}

// UnmarshalJSON parses an RFC 3339 string back into UTC so a client —
// and this package's own tests — can round-trip a response.
func (t *WireTime) UnmarshalJSON(b []byte) error {
	raw := string(b)
	if raw == "null" {
		*t = WireTime(time.Time{})
		return nil
	}
	s, err := strconv.Unquote(raw)
	if err != nil {
		return err
	}
	if s == "" {
		*t = WireTime(time.Time{})
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return err
	}
	*t = WireTime(parsed.UTC())
	return nil
}

// Time returns the underlying instant, for handler-side arithmetic
// and comparison.
func (t WireTime) Time() time.Time { return time.Time(t) }

// IsZero reports whether the instant is the zero time — the test that
// every `omitempty`-tagged timestamp field in this package actually
// wants, since encoding/json never omits a struct-typed field.
func (t WireTime) IsZero() bool { return time.Time(t).IsZero() }

// Before and After compare two wire timestamps. They exist because
// WireTime is a DEFINED type over time.Time — it deliberately does not
// inherit time.Time's method set, so that a raw time.Time cannot be
// assigned onto a wire field by accident, which is the whole point of
// the type. Ordering, though, is something callers legitimately need and
// gains nothing from being awkward: without these, every comparison
// becomes a pair of .Time() conversions that read as noise.
func (t WireTime) Before(u WireTime) bool { return time.Time(t).Before(time.Time(u)) }

// After is Before's mirror; see its comment.
func (t WireTime) After(u WireTime) bool { return time.Time(t).After(time.Time(u)) }

// String renders the same text MarshalJSON does, without the quotes.
func (t WireTime) String() string { return time.Time(t).UTC().Format(wireTimeLayout) }

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
