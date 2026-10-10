package accounts

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
	"github.com/Stellar-Index/StellarIndex/internal/ops/keys"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
	"github.com/Stellar-Index/StellarIndex/internal/storage/redisclient"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// FreezeUnfreeze is the OPERATOR half of ADR-0019's freeze lifecycle: the manual
// unfreeze that an ESCALATED freeze (one that climbed the whole extension ladder)
// waits for.
//
// It appends a "freeze.unfreeze" audit_log row (actor, reason) and writes the
// freeze:override tombstone, so the aggregator counts the release as an operator one.
// Then, in order:
//
//  1. delete the Redis marker (freeze.Writer.Clear), the serving path's authority for
//     `flags.frozen`, so the price republishes immediately;
//  2. stamp `recovered_at` on the open `freeze_events` row
//     (FreezeEventSink.MarkRecovered), the timeline /anomalies reads.
//
// Step 1 alone would be healed by the recovery worker's ~60 s poll; doing both makes
// the action complete when the command exits and says which half failed.
// Idempotent: clearing an absent marker is a no-op and MarkRecovered on a closed pair
// reports "no open row".
//
// Usage:
//
//	stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml -list
//	stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml \
//	    -asset native -quote 'USDC-GA5ZS…' -reason "oracle recovered, verified by hand"
//
// -reason is REQUIRED for a mutation (as X-Reason is on admin writes): an unfreeze
// overrides an automated safety control on a money surface, so who and why must be
// recorded.
func FreezeUnfreeze(args []string) error {
	fs := flag.NewFlagSet("freeze-unfreeze", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	list := fs.Bool("list", false, "list every currently-open freeze with its ladder state, and exit without changing anything")
	assetFlag := fs.String("asset", "", "asset to unfreeze, canonical wire form (native | CODE-ISSUER | C-strkey)")
	quoteFlag := fs.String("quote", "", "quote asset of the frozen pair, canonical wire form")
	reasonFlag := fs.String("reason", "", "why this freeze is being lifted (required for a mutation; recorded in audit_log)")
	actorFlag := fs.String("actor", "", "who is lifting it; recorded in audit_log. Defaults to the OS user.")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	reason, actor, err := resolveUnfreezeMutationInputs(*list, *assetFlag, *quoteFlag, *reasonFlag, *actorFlag)
	if err != nil {
		return err
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}
	// Checked before Postgres is opened: the marker is the serving path's
	// freeze authority, so without Redis there is nothing to list or clear.
	rdb := redisclient.Build(cfg.Storage)
	if rdb == nil {
		return errors.New("redis is not configured (storage.redis_addr / redis_sentinel_addrs both empty) — freeze-unfreeze requires Redis")
	}
	defer func() { _ = rdb.Close() }()

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage open: %w", err)
	}
	defer func() { _ = store.Close() }()

	sink := timescale.NewFreezeEventSink(store)
	writer, err := newFreezeWriterForOps(rdb, sink)
	if err != nil {
		return fmt.Errorf("freeze writer: %w", err)
	}

	if *list {
		// A marker-only reader alongside the ladder-aware writer, so the
		// STATE column can tell "Redis marker live" from "marker gone but
		// the durable ladder is still holding".
		looker, lerr := freeze.NewLooker(rdb)
		if lerr != nil {
			return fmt.Errorf("freeze looker: %w", lerr)
		}
		return listOpenFreezes(ctx, sink, writer, looker)
	}

	asset, err := canonical.ParseAsset(*assetFlag)
	if err != nil {
		return fmt.Errorf("-asset %q: %w", *assetFlag, err)
	}
	quote, err := canonical.ParseAsset(*quoteFlag)
	if err != nil {
		return fmt.Errorf("-quote %q: %w", *quoteFlag, err)
	}
	gate.Banner()
	audit := postgresstore.NewAuditStore(postgresstore.New(store.DB()))
	return unfreezePair(ctx, audit, sink, writer, unfreezeRequest{
		asset: asset, quote: quote, actor: actor, reason: reason, dryRun: gate.DryRun(),
	})
}

// resolveUnfreezeMutationInputs validates and resolves the reason/actor a
// mutation needs; -list needs neither and short-circuits to zero values.
func resolveUnfreezeMutationInputs(list bool, assetFlag, quoteFlag, reasonFlag, actorFlag string) (reason, actor string, err error) {
	if list {
		return "", "", nil
	}
	if assetFlag == "" || quoteFlag == "" {
		return "", "", errors.New("-asset and -quote are required (or pass -list to see what is frozen)")
	}
	reason = strings.TrimSpace(reasonFlag)
	if reason == "" {
		return "", "", errors.New("-reason is required: an unfreeze overrides an automated safety control on a money surface, so the record has to say why")
	}
	if err := keys.ValidateReason(reason); err != nil {
		return "", "", err
	}
	actor, err = opsutil.ResolveActor(actorFlag)
	if err != nil {
		return "", "", err
	}
	return reason, actor, nil
}

// newFreezeWriterForOps builds the freeze.Writer this command reads and clears
// through. Extracted from [FreezeUnfreeze] so the WIRING is unit-testable.
//
// The ladder store (migration 0119) matters twice:
//
//  1. `-list` reads the ladder via [freeze.Writer.LoadState]; without the store that
//     is Redis-only, so after a Redis flush an escalated freeze prints as
//     `MARKER GONE` with `EXTS 0 / ESCALATED false`, understating the ladder.
//
//  2. [freeze.Writer.Clear] RETIRES the durable ladder (zero State, hold_until NULL)
//     as well as deleting the marker. Without the store, an aggregator tick between
//     Clear and MarkRecovered sees "marker gone, durable ladder live", rehydrates and
//     re-writes the marker, silently defeating the unfreeze. With it the ladder
//     retires at the same instant, so the override wins.
//
// The TTL is irrelevant (this only Clears and LoadStates) but NewWriter requires a
// positive one.
func newFreezeWriterForOps(rdb freeze.RedisCache, ladder freeze.LadderStore) (*freeze.Writer, error) {
	return freeze.NewWriter(rdb, time.Minute,
		freeze.WithLadderStore(ladder, 0), // 0 → freeze.DefaultLadderGrace
	)
}

// openFreezeLister / freezeRecoverer are the two store behaviours this
// command needs, declared as interfaces so the flow is unit-testable
// without Postgres — the same seam pattern internal/aggregate/freeze uses
// for its own Recovery worker.
type openFreezeLister interface {
	ListOpen(ctx context.Context) ([]freeze.OpenFreezePair, error)
}

type freezeRecoverer interface {
	MarkRecovered(ctx context.Context, asset, quote canonical.Asset, releasedBy string) error
}

// freezeStateReader reads the ladder state a pair's Redis marker carries.
type freezeStateReader interface {
	LoadState(ctx context.Context, asset, quote canonical.Asset) (freeze.State, bool, error)
}

// markerPresenceReader reports whether the pair's REDIS marker specifically
// is alive — as opposed to [freezeStateReader.LoadState], which since
// migration 0119 answers the broader "is this pair frozen by EITHER
// authority". freeze.Looker satisfies it.
type markerPresenceReader interface {
	FrozenForPair(ctx context.Context, asset, quote canonical.Asset) (bool, error)
}

// listOpenFreezes prints every pair the durable mirror still records as
// firing, how far up the extension ladder it got, and — the operationally
// important column — WHICH authority is holding it.
//
// The STATE column has three values, and since migration 0119 collapsing
// them would be actively misleading:
//
//	live        the Redis marker is present. Normal running freeze.
//	rehydrated  the marker is GONE but the durable ladder is still inside
//	            its hold, so Redis lost the marker and the aggregator will
//	            re-write it on its next tick. The pair IS still frozen to
//	            every API caller. A row of these right after a Redis
//	            restart is the expected shape; a slow trickle with Redis
//	            healthy means markers are being evicted or expiring early.
//	GONE        neither authority holds it — already unfrozen on the
//	            serving path, just waiting for the recovery worker to close
//	            the row.
//
// Reporting a rehydrated pair as `live` would be defensible (it is frozen);
// reporting it as `GONE` — which is what a marker-only probe would say —
// is not, because an operator reads GONE as "nothing to do here" on exactly
// the pair whose marker just evaporated. An ESCALATED pair is one that will
// never clear on its own.
func listOpenFreezes(ctx context.Context, lister openFreezeLister, states freezeStateReader, markers markerPresenceReader) error {
	open, err := lister.ListOpen(ctx)
	if err != nil {
		return fmt.Errorf("list open freezes: %w", err)
	}
	if len(open) == 0 {
		fmt.Println("freeze-unfreeze: no open freeze_events rows")
		return nil
	}
	fmt.Printf("%-34s %-34s %-11s %-6s %-10s %s\n", "ASSET", "QUOTE", "STATE", "EXTS", "ESCALATED", "HOLD UNTIL")
	for _, p := range open {
		state, frozen, serr := states.LoadState(ctx, p.Asset, p.Quote)
		markerLive, merr := markers.FrozenForPair(ctx, p.Asset, p.Quote)
		status := "GONE"
		switch {
		case serr != nil || merr != nil:
			status = "err"
		case markerLive:
			status = "live"
		case frozen:
			status = "rehydrated"
		}
		hold := "-"
		if !state.HoldUntil.IsZero() {
			hold = state.HoldUntil.UTC().Format(time.RFC3339)
		}
		fmt.Printf("%-34s %-34s %-11s %-6d %-10v %s\n",
			p.Asset.String(), p.Quote.String(), status, state.ExtensionsUsed, state.Escalated, hold)
		if serr != nil {
			fmt.Fprintf(os.Stderr, "freeze-unfreeze: ladder read failed for %s/%s: %v\n", p.Asset, p.Quote, serr)
		}
		if merr != nil {
			fmt.Fprintf(os.Stderr, "freeze-unfreeze: marker read failed for %s/%s: %v\n", p.Asset, p.Quote, merr)
		}
	}
	return nil
}

// freezeClearer records the operator-override tombstone and deletes a
// pair's Redis freeze marker; freeze.Writer implements both.
type freezeClearer interface {
	RecordOverride(ctx context.Context, asset, quote canonical.Asset, actor, reason string) error
	Clear(ctx context.Context, asset, quote canonical.Asset) error
}

// unfreezeRequest is one manual unfreeze: the pair, who asked, and why.
type unfreezeRequest struct {
	asset, quote  canonical.Asset
	actor, reason string
	dryRun        bool
}

// unfreezePair performs the two-step manual unfreeze and reports each half.
//
// The audit_log row and the override tombstone go first, and a failure of
// either stops the run before the serving path changes: an unfreeze with
// no record of who and why is the defect this ordering exists to prevent.
// The row records the operator's decision, so a later half that fails is
// reported here and the row still says who tried.
//
// Order matters: the Redis marker goes FIRST because it is the serving
// path's authority — clearing it is what actually republishes the price.
// If the durable stamp then fails, the operator is told loudly and the run
// exits non-zero, but the pair IS unfrozen and the recovery worker will
// close the row on its next poll; the reverse order would leave a closed
// timeline row next to a pair that is still frozen to every API caller,
// which is the dishonest direction.
func unfreezePair(ctx context.Context, audit keys.AuditSink, recoverer freezeRecoverer, clearer freezeClearer, req unfreezeRequest) error {
	asset, quote := req.asset, req.quote
	if req.dryRun {
		fmt.Printf("freeze-unfreeze: DRY RUN — would audit, clear the Redis marker and stamp recovered_at for %s/%s (actor: %s, reason: %s)\n",
			asset.String(), quote.String(), req.actor, req.reason)
		return nil
	}
	fmt.Fprintf(os.Stderr, "freeze-unfreeze: lifting freeze on %s/%s — actor: %s, reason: %s\n",
		asset.String(), quote.String(), req.actor, req.reason)

	target := asset.String() + "/" + quote.String()
	if err := keys.AppendOpsAudit(ctx, audit, "freeze.unfreeze", "freeze-unfreeze", "freeze_pair", target, req.actor, req.reason, nil); err != nil {
		return fmt.Errorf("audit_log append for %s failed, so NOTHING was changed: %w", target, err)
	}
	if err := clearer.RecordOverride(ctx, asset, quote, req.actor, req.reason); err != nil {
		return fmt.Errorf("record the override tombstone for %s (NOTHING was changed on the serving path): %w", target, err)
	}
	if err := clearer.Clear(ctx, asset, quote); err != nil {
		return fmt.Errorf("clear redis freeze marker for %s/%s (NOTHING was changed): %w", asset, quote, err)
	}
	fmt.Printf("freeze-unfreeze: redis marker cleared for %s/%s — the serving path is unfrozen\n", asset.String(), quote.String())

	switch err := recoverer.MarkRecovered(ctx, asset, quote, "operator:"+req.actor); {
	case err == nil:
		fmt.Printf("freeze-unfreeze: freeze_events row closed for %s/%s\n", asset.String(), quote.String())
	case errors.Is(err, timescale.ErrNotFound):
		// Already closed (a prior unfreeze, or the recovery worker got
		// there first). Not a failure — the end state is the intended one.
		fmt.Printf("freeze-unfreeze: no OPEN freeze_events row for %s/%s — already closed\n", asset.String(), quote.String())
	default:
		// Since migration 0119 the recovery worker is NOT a fallback here.
		// The durable row is still open, so the aggregator's next tick reads
		// the missing marker as "Redis lost it", rehydrates the ladder and
		// re-writes the marker — and the sweep then correctly declines to
		// close a row whose hold is live. The unfreeze has NOT stuck, and
		// nothing will make it stick on its own.
		return fmt.Errorf("redis marker for %s/%s was cleared but the freeze_events row could not be stamped, so the unfreeze did NOT take: the aggregator will rehydrate the ladder from the still-open row and re-freeze the pair on its next tick. Fix Postgres and RE-RUN this command: %w",
			asset, quote, err)
	}
	return nil
}
