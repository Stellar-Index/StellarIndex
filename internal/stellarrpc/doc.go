// Package stellarrpc is a minimal JSON-RPC client for stellar-rpc, kept
// small and mockable instead of the SDK client.
//
// Production ingest never uses it (AGENTS.md invariant 6; lint-imports.sh
// rule A/no-rpc-in-ingest). Its callers are the `stellarindex-ops
// rpc-probe` diagnostic, the soroswap factory seed at boot
// (internal/sources/soroswap/factory_seed.go) and the scripts/dev
// fixture-capture scripts.
//
// It wraps getHealth, getLatestLedger, getNetwork, getVersionInfo,
// getEvents, getLedgers, getTransaction(s) and getFeeStats, and returns
// raw XDR: decoding belongs to the caller.
package stellarrpc
