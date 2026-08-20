// Package pgarray adapts Postgres array columns to the database/sql API
// under the jackc/pgx v5 stdlib driver.
//
// Why this exists: unlike lib/pq, pgx's stdlib driver surfaces an array
// column to database/sql as its Postgres TEXT representation (e.g.
// "{a,b,c}"), which database/sql cannot assign to a plain Go slice —
// rows.Scan(&[]string{}) fails with "unsupported Scan, storing driver.Value
// type string into type *[]string". For query ARGUMENTS pgx encodes Go
// slices ([]string, [][]byte, …) as Postgres arrays natively, so no wrapper
// is needed on the arg side; only SCAN destinations need an adapter.
//
// These adapters implement [sql.Scanner] by delegating the parse to pgx's
// own pgtype array codec — no hand-rolled array-text parsing — so they are
// byte-for-byte faithful to how the wire value is produced, matching the
// former pq.StringArray / pq.ByteaArray semantics: SQL NULL decodes to a
// nil slice, and an empty array "{}" decodes to a non-nil zero-length slice.
package pgarray

import (
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
)

// typeMap holds the pgtype core codec registrations (arrays included). It is
// constructed once and thereafter only read from — [pgtype.Map.Scan] does
// not mutate the map for the core types — so it is safe for concurrent use
// across the pool's goroutines.
var typeMap = pgtype.NewMap()

// Strings returns a [sql.Scanner] that decodes a Postgres text[] / varchar[]
// value into *dst. A SQL NULL decodes to a nil slice; an empty array "{}"
// decodes to a non-nil zero-length slice.
func Strings(dst *[]string) sql.Scanner { return scanner[[]string]{oid: pgtype.TextArrayOID, dst: dst} }

// Bytea returns a [sql.Scanner] that decodes a Postgres bytea[] value into
// *dst, with the same NULL / empty semantics as [Strings].
func Bytea(dst *[][]byte) sql.Scanner { return scanner[[][]byte]{oid: pgtype.ByteaArrayOID, dst: dst} }

type scanner[T any] struct {
	oid uint32
	dst *T
}

// Scan implements [sql.Scanner]. pgx's stdlib driver hands the array over as
// the Postgres text representation (a string, or []byte); a NULL arrives as
// nil.
func (s scanner[T]) Scan(src any) error {
	if src == nil {
		var zero T
		*s.dst = zero
		return nil
	}
	var buf []byte
	switch v := src.(type) {
	case string:
		buf = []byte(v)
	case []byte:
		buf = v
	default:
		return fmt.Errorf("pgarray: cannot scan %T into array (expected the pgx stdlib text representation)", src)
	}
	return typeMap.Scan(s.oid, pgtype.TextFormatCode, buf, s.dst)
}
