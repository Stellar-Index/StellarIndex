// Package pgarray adapts Postgres array columns to database/sql under the pgx v5 stdlib driver.
//
// pgx's stdlib driver surfaces an array column as its TEXT form ("{a,b,c}"), which database/sql
// cannot scan into a plain Go slice ("unsupported Scan, storing driver.Value type string into
// type *[]string"). Query ARGUMENTS need no wrapper (pgx encodes Go slices natively); only SCAN
// destinations do.
//
// The adapters implement [sql.Scanner] by delegating to pgx's pgtype array codec rather than
// hand-parsing array text. SQL NULL decodes to a nil slice, and "{}" to a non-nil empty slice.
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

// Strings returns a [sql.Scanner] decoding a Postgres text[] / varchar[] into *dst: SQL NULL gives a nil slice, an empty array "{}" a non-nil zero-length slice.
func Strings(dst *[]string) sql.Scanner { return scanner[[]string]{oid: pgtype.TextArrayOID, dst: dst} }

// Bytea returns a [sql.Scanner] decoding a Postgres bytea[] into *dst, with the same NULL / empty semantics as [Strings].
func Bytea(dst *[][]byte) sql.Scanner { return scanner[[][]byte]{oid: pgtype.ByteaArrayOID, dst: dst} }

type scanner[T any] struct {
	oid uint32
	dst *T
}

// Scan implements [sql.Scanner]; pgx's stdlib driver hands the array over as Postgres text (a string or []byte), and a NULL as nil.
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
