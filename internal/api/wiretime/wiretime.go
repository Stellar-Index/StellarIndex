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

// Time is the canonical JSON rendering of an instant on a client-facing
// wire: RFC 3339, always in UTC, always with a literal `Z` offset.
//
// It exists because a plain [time.Time] field marshals in whatever
// location the value happens to carry, and the values that reach a
// response struct do NOT reliably carry UTC. Rows read back from
// Postgres decode `timestamptz` into the PROCESS's local zone, so a
// handler that passes a stored timestamp straight into a `time.Time`
// json field emits the server's local offset — the same instant,
// rendered differently. Production served a `+02:00` offset from
// /v1/price/at and a `+01:00` one from /v1/history/since-inception.
// Note the second offset: it moves with DST, so one series rendered two
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
// Use it for every json-tagged timestamp field a client reads. The rule
// is enforced over all of internal/api by v1's
// TestWireTime_NoRawTimeOnTheWire, which fails if a new response struct
// reaches for a raw [time.Time] instead.
type Time time.Time

// layout is [time.RFC3339Nano] pinned to a `Z` offset — the
// layout [time.Time.MarshalJSON] itself uses, minus the `Z07:00`
// escape hatch that lets a local offset through.
const layout = "2006-01-02T15:04:05.999999999Z"

// MarshalJSON renders the instant as an RFC 3339 UTC string.
//
// [time.Time.MarshalJSON] errors on years outside [0,9999] because
// they cannot be written as RFC 3339. A timestamp that far out of
// range is a corrupt read rather than a real observation, so rather
// than fail the entire response this renders JSON null — what an
// absent optional timestamp already renders as.
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

// UnmarshalJSON parses an RFC 3339 string back into UTC so a client —
// and the handlers' own tests — can round-trip a response.
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

// Time returns the underlying instant, for handler-side arithmetic
// and comparison.
func (t Time) Time() time.Time { return time.Time(t) }

// IsZero reports whether the instant is the zero time — the test that
// every `omitempty`-tagged wire timestamp field actually
// wants, since encoding/json never omits a struct-typed field.
func (t Time) IsZero() bool { return time.Time(t).IsZero() }

// Before and After compare two wire timestamps. They exist because
// Time is a DEFINED type over time.Time — it deliberately does not
// inherit time.Time's method set, so that a raw time.Time cannot be
// assigned onto a wire field by accident, which is the whole point of
// the type. Ordering, though, is something callers legitimately need and
// gains nothing from being awkward: without these, every comparison
// becomes a pair of .Time() conversions that read as noise.
func (t Time) Before(u Time) bool { return time.Time(t).Before(time.Time(u)) }

// After is Before's mirror; see its comment.
func (t Time) After(u Time) bool { return time.Time(t).After(time.Time(u)) }

// String renders the same text MarshalJSON does, without the quotes.
func (t Time) String() string { return time.Time(t).UTC().Format(layout) }

// NilIfZero lifts t onto the wire as an optional field: nil for the zero
// time, so an `omitempty` pointer is genuinely absent rather than year 1.
func NilIfZero(t time.Time) *Time {
	if t.IsZero() {
		return nil
	}
	w := Time(t)
	return &w
}
