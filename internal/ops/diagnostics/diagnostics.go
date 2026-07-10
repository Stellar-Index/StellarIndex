// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

// Package diagnostics holds the stellarindex-ops operator diagnostic
// subcommands: `rpc-probe`, `verify-decoders`, `verify-external`,
// `hubble-check`, `hubble-soroban-events`. Extracted from
// cmd/stellarindex-ops (maintainability audit 2026-07-01, D1 finding
// M1-5); main.go's dispatch table calls Run below.
package diagnostics

import (
	"fmt"
)

// Run is the internal/ops/diagnostics package's entry point — see
// discovery.Run's doc comment for the calling convention shared by
// every internal/ops/* package post-split. args[0] is the subcommand
// verb (one of the five this package owns); args[1:] are its flags.
func Run(args []string) error {
	switch args[0] {
	case "rpc-probe":
		endpoint := "http://127.0.0.1:8000"
		if len(args) > 1 {
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
