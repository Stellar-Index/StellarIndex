package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/redisclient"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// freezeUnfreeze is the OPERATOR half of ADR-0019's freeze lifecycle.
//
// ADR-0019 is explicit that an ESCALATED freeze — one that climbed the whole
// 4 × 30-minute extension ladder without earning its auto-unfreeze — "stays
// active until manual unfreeze". Every other piece of that lifecycle is
// implemented (the orchestrator fires and extends, the policy escalates, the
// recovery worker closes the durable row when a marker lapses) except the
// manual unfreeze itself, which had no code path anywhere in the binary set.
// The only way to end an escalated freeze was to `redis-cli DEL` the marker
// by hand — untyped, unlogged, un-mirrored into `freeze_events`, and easy to
// get wrong on an asset id that the canonical parser would have rejected.
//
// This is that path, and it does BOTH halves in the right order:
//
//  1. delete the Redis marker (freeze.Writer.Clear) — the serving path's
//     authority for `flags.frozen`, so the price republishes immediately
//     rather than after the remaining hold TTL elapses;
//  2. stamp `recovered_at` on the open `freeze_events` row
//     (FreezeEventSink.MarkRecovered) — the durable timeline the explorer
//     /anomalies view reads.
//
// Doing only (1) works eventually — the recovery worker polls and would
// close the row within ~60 s — but doing both here means the operator's
// action is complete and observable the moment the command exits, and the
// command is honest about which half failed if one does.
//
// Idempotent: clearing an absent marker is a no-op by contract, and
// MarkRecovered on an already-closed pair reports "no open row" rather than
// erroring the run.
//
// Usage:
//
//	stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml -list
//	stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml \
//	    -asset native -quote 'USDC-GA5ZS…' -reason "oracle recovered, verified by hand"
//
// -reason is REQUIRED for a mutation, mirroring the X-Reason discipline the
// admin API applies to every privileged write: an unfreeze overrides an
// automated safety control on a money surface, and "who and why" has to be
// in the record, not in someone's memory.
func freezeUnfreeze(args []string) error {
	fs := flag.NewFlagSet("freeze-unfreeze", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	list := fs.Bool("list", false, "list every currently-open freeze with its ladder state, and exit without changing anything")
	assetFlag := fs.String("asset", "", "asset to unfreeze, canonical wire form (native | CODE-ISSUER | C-strkey)")
	quoteFlag := fs.String("quote", "", "quote asset of the frozen pair, canonical wire form")
	reason := fs.String("reason", "", "why this freeze is being lifted (required for a mutation; recorded in the run log)")
	dryRun := fs.Bool("dry-run", false, "resolve and report what would be unfrozen without touching Redis or Postgres")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	if !*list {
		if *assetFlag == "" || *quoteFlag == "" {
			return errors.New("-asset and -quote are required (or pass -list to see what is frozen)")
		}
		if strings.TrimSpace(*reason) == "" {
			return errors.New("-reason is required: an unfreeze overrides an automated safety control on a money surface, so the record has to say why")
		}
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage open: %w", err)
	}
	defer func() { _ = store.Close() }()

	rdb := redisclient.Build(cfg.Storage)
	defer func() { _ = rdb.Close() }()

	// The TTL is irrelevant here — this command only ever Clears and
	// LoadStates, never Marks — but NewWriter requires a positive one.
	writer, err := freeze.NewWriter(rdb, time.Minute)
	if err != nil {
		return fmt.Errorf("freeze writer: %w", err)
	}
	sink := timescale.NewFreezeEventSink(store)

	if *list {
		return listOpenFreezes(ctx, sink, writer)
	}

	asset, err := canonical.ParseAsset(*assetFlag)
	if err != nil {
		return fmt.Errorf("-asset %q: %w", *assetFlag, err)
	}
	quote, err := canonical.ParseAsset(*quoteFlag)
	if err != nil {
		return fmt.Errorf("-quote %q: %w", *quoteFlag, err)
	}
	return unfreezePair(ctx, sink, writer, asset, quote, *reason, *dryRun)
}

// openFreezeLister / freezeRecoverer are the two store behaviours this
// command needs, declared as interfaces so the flow is unit-testable
// without Postgres — the same seam pattern internal/aggregate/freeze uses
// for its own Recovery worker.
type openFreezeLister interface {
	ListOpen(ctx context.Context) ([]freeze.OpenFreezePair, error)
}

type freezeRecoverer interface {
	MarkRecovered(ctx context.Context, asset, quote canonical.Asset) error
}

// freezeStateReader reads the ladder state a pair's Redis marker carries.
type freezeStateReader interface {
	LoadState(ctx context.Context, asset, quote canonical.Asset) (freeze.State, bool, error)
}

// listOpenFreezes prints every pair the durable mirror still records as
// firing, together with whether its Redis marker is still alive and how far
// up the extension ladder it got.
//
// The marker-present column is the operationally important one: a pair with
// an OPEN row and NO marker is already effectively unfrozen on the serving
// path and is just waiting for the recovery worker; a pair with both is
// live. An ESCALATED pair is one that will never clear on its own.
func listOpenFreezes(ctx context.Context, lister openFreezeLister, states freezeStateReader) error {
	open, err := lister.ListOpen(ctx)
	if err != nil {
		return fmt.Errorf("list open freezes: %w", err)
	}
	if len(open) == 0 {
		fmt.Println("freeze-unfreeze: no open freeze_events rows")
		return nil
	}
	fmt.Printf("%-34s %-34s %-7s %-6s %-10s %s\n", "ASSET", "QUOTE", "MARKER", "EXTS", "ESCALATED", "HOLD UNTIL")
	for _, p := range open {
		state, present, serr := states.LoadState(ctx, p.Asset, p.Quote)
		marker := "live"
		switch {
		case serr != nil:
			marker = "err"
		case !present:
			marker = "GONE"
		}
		hold := "-"
		if !state.HoldUntil.IsZero() {
			hold = state.HoldUntil.UTC().Format(time.RFC3339)
		}
		fmt.Printf("%-34s %-34s %-7s %-6d %-10v %s\n",
			p.Asset.String(), p.Quote.String(), marker, state.ExtensionsUsed, state.Escalated, hold)
		if serr != nil {
			fmt.Fprintf(os.Stderr, "freeze-unfreeze: marker read failed for %s/%s: %v\n", p.Asset, p.Quote, serr)
		}
	}
	return nil
}

// freezeClearer deletes a pair's Redis freeze marker.
type freezeClearer interface {
	Clear(ctx context.Context, asset, quote canonical.Asset) error
}

// unfreezePair performs the two-step manual unfreeze and reports each half.
//
// Order matters: the Redis marker goes FIRST because it is the serving
// path's authority — clearing it is what actually republishes the price.
// If the durable stamp then fails, the operator is told loudly and the run
// exits non-zero, but the pair IS unfrozen and the recovery worker will
// close the row on its next poll; the reverse order would leave a closed
// timeline row next to a pair that is still frozen to every API caller,
// which is the dishonest direction.
func unfreezePair(ctx context.Context, recoverer freezeRecoverer, clearer freezeClearer, asset, quote canonical.Asset, reason string, dryRun bool) error {
	if dryRun {
		fmt.Printf("freeze-unfreeze: DRY RUN — would clear the Redis marker and stamp recovered_at for %s/%s (reason: %s)\n",
			asset.String(), quote.String(), reason)
		return nil
	}
	fmt.Fprintf(os.Stderr, "freeze-unfreeze: lifting freeze on %s/%s — reason: %s\n",
		asset.String(), quote.String(), reason)

	if err := clearer.Clear(ctx, asset, quote); err != nil {
		return fmt.Errorf("clear redis freeze marker for %s/%s (NOTHING was changed): %w", asset, quote, err)
	}
	fmt.Printf("freeze-unfreeze: redis marker cleared for %s/%s — the serving path is unfrozen\n", asset.String(), quote.String())

	switch err := recoverer.MarkRecovered(ctx, asset, quote); {
	case err == nil:
		fmt.Printf("freeze-unfreeze: freeze_events row closed for %s/%s\n", asset.String(), quote.String())
	case errors.Is(err, timescale.ErrNotFound):
		// Already closed (a prior unfreeze, or the recovery worker got
		// there first). Not a failure — the end state is the intended one.
		fmt.Printf("freeze-unfreeze: no OPEN freeze_events row for %s/%s — already closed\n", asset.String(), quote.String())
	default:
		return fmt.Errorf("redis marker for %s/%s WAS cleared (the pair is unfrozen) but the freeze_events row could not be stamped — the recovery worker will close it on its next poll: %w",
			asset, quote, err)
	}
	return nil
}
