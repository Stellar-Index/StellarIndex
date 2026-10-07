// Package timescale is the data-access layer over the TimescaleDB schema
// (migrations/README.md is the manifest). It owns the SQL; callers use
// [Store]'s methods, over pgx v5 through database/sql.
//
// Amounts cross as strings (NUMERIC ↔ canonical.Amount), never int64
// (ADR-0003). Timestamps are timestamptz in the database and UTC
// [time.Time] in Go. Rows are validated ([canonical.Trade.Validate],
// [canonical.OracleUpdate.Validate]) before any insert.
//
// [Store] is concrete with no mock layer: its tests run against a real
// Timescale via testcontainers-go (`make test-integration`).
package timescale
