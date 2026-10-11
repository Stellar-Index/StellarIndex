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

// FreezeUnfreeze manually unfreezes an escalated freeze (ADR-0019); -reason is required because it overrides a safety control.
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

// newFreezeWriterForOps builds the freeze.Writer; it needs the ladder store so -list and Clear see the durable ladder,
// otherwise an aggregator tick between Clear and MarkRecovered rehydrates the marker and defeats the unfreeze.
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

// listOpenFreezes prints open freezes with STATE live, rehydrated (marker gone but durable ladder still holds, so
// still frozen to callers) or GONE; a marker-only probe would misreport rehydrated pairs as GONE.
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

// unfreezePair writes the audit row and tombstone first (no unfreeze without a record), then clears the Redis marker,
// the serving authority, before stamping the durable row; the reverse order would leave a closed row on a still-frozen pair.
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
		// The unfreeze has NOT stuck: the aggregator rehydrates the ladder from the still-open row and re-freezes.
		return fmt.Errorf("redis marker for %s/%s was cleared but the freeze_events row could not be stamped, so the unfreeze did NOT take: the aggregator will rehydrate the ladder from the still-open row and re-freeze the pair on its next tick. Fix Postgres and RE-RUN this command: %w",
			asset, quote, err)
	}
	return nil
}
