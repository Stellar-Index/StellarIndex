// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

// Package diagnostics holds the stellarindex-ops operator diagnostic subcommands: `rpc-probe`, `verify-decoders`,
// `verify-external`, `hubble-check`, `hubble-soroban-events`. cmd/stellarindex-ops's dispatch table calls Run below.
package diagnostics

import (
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// Run is the entry point of the diagnostics ops package; see discovery.Run for the shared calling convention. args[0] is the subcommand verb.
func Run(args []string) error {
	switch args[0] {
	case "rpc-probe":
		endpoint := "http://127.0.0.1:8000"
		if len(args) > 1 {
			if opsutil.IsHelpArg(args[1]) {
				return opsutil.Usage("usage: rpc-probe [endpoint]  (default " + endpoint + ")")
			}
			endpoint = args[1]
		}
		return rpcProbe(endpoint)
	case "verify-decoders":
		return verifyDecoders(args[1:])
	case "verify-external":
		return verifyExternal(args[1:])
	case "hubble-check":
		return hubbleCheck(args[1:])
	case "hubble-soroban-events":
		return hubbleSorobanEvents(args[1:])
	default:
		return fmt.Errorf("internal/ops/diagnostics: unknown subcommand %q", args[0])
	}
}
