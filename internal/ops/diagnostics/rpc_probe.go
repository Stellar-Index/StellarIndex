package diagnostics

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/stellarrpc"
)

// rpcProbe runs a one-shot diagnostic against a stellar-rpc endpoint:
// getHealth, getLatestLedger, getNetwork, getVersionInfo, getFeeStats.
// Prints a human-readable report to stdout + returns the first fatal
// error (e.g. endpoint unreachable). Stale-rpc is not fatal — it's
// reported in the staleness line.
func rpcProbe(endpoint string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c := stellarrpc.New(endpoint, stellarrpc.WithTimeout(5*time.Second))
	fmt.Printf("stellar-rpc probe — %s\n\n", c.Endpoint())

	// getVersionInfo first — cheapest and tells us we can reach the
	// thing at all. On failure, print actionable context before
	// propagating the error so an operator running `rpc-probe` at
	// 3 AM sees WHY the connection failed rather than just a Go
	// error string.
	vi, err := c.VersionInfo(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n❌ cannot reach stellar-rpc at %s\n", endpoint)
		fmt.Fprintf(os.Stderr, "   error: %v\n", err)
		fmt.Fprintf(os.Stderr, "\n   Likely causes, in order:\n")
		fmt.Fprintf(os.Stderr, "     1. URL scheme/port wrong (expected http://<host>:8000)\n")
		fmt.Fprintf(os.Stderr, "     2. stellar-rpc process not running on that host\n")
		fmt.Fprintf(os.Stderr, "     3. Firewall / NetworkPolicy blocks outbound\n")
		fmt.Fprintf(os.Stderr, "     4. DNS for %q doesn't resolve\n", endpoint)
		return fmt.Errorf("getVersionInfo: %w", err)
	}
	fmt.Printf("  version:         %s\n", vi.Version)
	fmt.Printf("  commitHash:      %s\n", vi.CommitHash)
	fmt.Printf("  captiveCore:     %s\n", vi.CaptiveCoreVersion)
	fmt.Printf("  protocolVersion: %d\n\n", vi.ProtocolVersion)

	net, err := c.Network(ctx)
	if err != nil {
		return fmt.Errorf("getNetwork: %w", err)
	}
	fmt.Printf("  network:         %s (protocol %d)\n\n", net.Passphrase, net.ProtocolVersion)

	// Health returns an error envelope on stale — report, don't fail.
	if _, err := c.Health(ctx); err != nil {
		fmt.Printf("  health:          ⚠ %v\n", err)
	} else {
		fmt.Printf("  health:          ✓ healthy\n")
	}

	l, err := c.LatestLedger(ctx)
	if err != nil {
		return fmt.Errorf("getLatestLedger: %w", err)
	}
	fmt.Printf("  latestLedger:    %d (closeTime %s, id %s…)\n\n", l.Sequence, l.CloseTime, shortHex(l.ID, 12))

	fs, err := c.FeeStats(ctx)
	if err != nil {
		fmt.Printf("  getFeeStats:     ⚠ %v\n", err)
	} else {
		fmt.Printf("  fees (classic):  min=%s mode=%s p99=%s (%d ledgers)\n",
			fs.InclusionFee.Min, fs.InclusionFee.Mode, fs.InclusionFee.P99, fs.InclusionFee.LedgerCount)
		fmt.Printf("  fees (soroban):  min=%s mode=%s p99=%s (%d ledgers)\n",
			fs.SorobanInclusionFee.Min, fs.SorobanInclusionFee.Mode, fs.SorobanInclusionFee.P99,
			fs.SorobanInclusionFee.LedgerCount)
	}

	// Range of events available — 1-event probe from just before tip
	// so we know the retention window.
	start := l.Sequence - 1
	er, err := c.GetEvents(ctx, start, 0, nil, &stellarrpc.Pagination{Limit: 1})
	if err != nil {
		fmt.Printf("\n  getEvents:       ⚠ %v\n", err)
	} else {
		window := er.LatestLedger - er.OldestLedger
		fmt.Printf("\n  events window:   oldest=%d  latest=%d  (~%d ledgers ≈ %.1f d at 5s cadence)\n",
			er.OldestLedger, er.LatestLedger, window, float64(window)*5/86400)
		if len(er.Events) > 0 {
			fmt.Printf("  sample event:    contract=%s… type=%s topics=%d\n",
				shortHex(er.Events[0].ContractID, 12), er.Events[0].Type, len(er.Events[0].Topic))
		}

		// getTransaction round-trip against the sample event's tx
		// hash. Proves the RPC's tx retention window covers at least
		// the current tip — sources rely on this to decode tx-level
		// context (observer account, envelope XDR).
		if len(er.Events) > 0 && er.Events[0].TxHash != "" {
			tx, err := c.GetTransaction(ctx, er.Events[0].TxHash)
			switch {
			case err != nil:
				fmt.Printf("  getTransaction:  ⚠ %v\n", err)
			case tx.Status == stellarrpc.TxStatusNotFound:
				// Should not happen for a tx we JUST saw in getEvents,
				// but surfaces any retention-window mismatch.
				fmt.Printf("  getTransaction:  ⚠ tx %s… not found (retention window mismatch)\n",
					shortHex(er.Events[0].TxHash, 8))
			default:
				fmt.Printf("  getTransaction:  ✓ status=%s ledger=%d appOrder=%d\n",
					tx.Status, tx.Ledger, tx.ApplicationOrder)
			}
		}
	}

	fmt.Println()
	return nil
}

// shortHex returns the first `n` characters of s, or s if it is
// already shorter. Guards the probe against panicking on a
// malformed-RPC response whose ID/hash is shorter than expected —
// a diagnostic binary should never crash on bad input, it should
// report whatever it got.
func shortHex(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ─── backfill-external ──────────────────────────────────────────
