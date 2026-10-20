// Package stellarrpc is a minimal, mockable JSON-RPC client for stellar-rpc.
//
// Production ingest never uses it (AGENTS.md invariant 6; lint-imports.sh rule A/no-rpc-in-ingest).
// Callers are the `stellarindex-ops rpc-probe` diagnostic, the soroswap factory seed at boot and
// the scripts/dev fixture-capture scripts. It returns raw XDR; decoding belongs to the caller.
package stellarrpc
