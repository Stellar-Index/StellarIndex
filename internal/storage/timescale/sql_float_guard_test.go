// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"regexp"
	"testing"
)

// The ways SQL text can route an amount through IEEE-754 (ADR-0003): a
// float cast in any spelling (::float, ::float8, ::real, ::double
// precision, CAST(... AS real)), the float8()/float4() call form, and a
// float literal such as 1e6. Case-insensitive, as Postgres is.
var (
	floatSQLCast = regexp.MustCompile(
		`(?i)::\s*(float[48]?|real|double)\b|\bas\s+(float[48]?|real|double)\b|\bfloat[48]\s*\(|\bdouble\s+precision\b`)
	floatSQLLiteral = regexp.MustCompile(`(?i)\b\d+(\.\d+)?e[+-]?\d+\b`)
)

// floatSQLViolations returns every float construct in q.
func floatSQLViolations(q string) []string {
	out := floatSQLCast.FindAllString(q, -1)
	return append(out, floatSQLLiteral.FindAllString(q, -1)...)
}

// assertNoFloatSQL is the one ADR-0003 no-float check for query text;
// shape tests call it rather than keeping their own blocklist.
func assertNoFloatSQL(t *testing.T, name, q string) {
	t.Helper()
	for _, bad := range floatSQLViolations(q) {
		t.Errorf("%s must never use float arithmetic (ADR-0003); found %q", name, bad)
	}
}

func TestFloatSQLViolations(t *testing.T) {
	for _, q := range []string{
		"SELECT usd_volume::float FROM t",
		"SELECT usd_volume::float8 FROM t",
		"SELECT usd_volume::REAL FROM t",
		"SELECT usd_volume:: double precision FROM t",
		"SELECT float8(usd_volume) FROM t",
		"SELECT FLOAT4 (usd_volume) FROM t",
		"SELECT CAST(usd_volume AS real) FROM t",
		"SELECT CAST(usd_volume AS double precision) FROM t",
		"SELECT amount / 1e6 FROM t",
		"SELECT amount / 1E+07 FROM t",
		"SELECT amount * 2.5e-3 FROM t",
	} {
		if len(floatSQLViolations(q)) == 0 {
			t.Errorf("float construct not detected in %q", q)
		}
	}
	for _, q := range []string{
		"SELECT round(usd_volume, 2)::text, amount / 1000000::numeric FROM t",
		"SELECT realized_pnl, float_col, is_real FROM t WHERE ts > now() - $2::interval",
		"SELECT sum(x)::numeric AS total, count(*)::bigint FROM t",
	} {
		if got := floatSQLViolations(q); len(got) != 0 {
			t.Errorf("exact-NUMERIC query %q flagged as float: %q", q, got)
		}
	}
}
