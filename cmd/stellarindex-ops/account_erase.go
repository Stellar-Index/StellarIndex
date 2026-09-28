package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/accounterasure"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
	"github.com/Stellar-Index/StellarIndex/internal/storage/redisclient"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// accountErase is the operator path for an erasure request received
// outside the dashboard, and for finishing one whose post-commit Redis
// cleanup did not complete (GH #809). It calls the same
// accounterasure.Eraser as DELETE /v1/dashboard/account.
//
//	stellarindex-ops account-erase -config PATH -account-id UUID [-write]
//	stellarindex-ops account-erase -config PATH -finish-slug SLUG [-write]
//
// Dry-run unless -write: it prints what would be removed. Output carries
// counts and ids only, never an address.
func accountErase(args []string) error {
	fs := flag.NewFlagSet("account-erase", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	idStr := fs.String("account-id", "", "Account uuid to erase")
	finish := fs.String("finish-slug", "", "Slug of an already-erased account whose Redis cleanup to finish")
	gate := opsutil.RegisterWriteGate(fs)
	timeout := fs.Duration("timeout", 2*time.Minute, "Overall deadline")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	if (*idStr == "") == (*finish == "") {
		return errors.New("exactly one of -account-id or -finish-slug is required")
	}
	var id uuid.UUID
	if *idStr != "" {
		var err error
		if id, err = uuid.Parse(*idStr); err != nil {
			return fmt.Errorf("-account-id: %w", err)
		}
	}
	gate.Banner()

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	defer func() { _ = store.Close() }()
	e := &accounterasure.Eraser{
		Store:  postgresstore.NewAccountStore(postgresstore.New(store.DB())),
		Logger: opsutil.MkBackfillLogger(),
	}
	if rdb := redisclient.Build(cfg.Storage); rdb != nil {
		defer func() { _ = rdb.Close() }()
		if err := rdb.Ping(ctx).Err(); err != nil {
			return fmt.Errorf("redis ping: %w", err)
		}
		e.Redis = rdb
	}
	return runAccountErase(ctx, os.Stderr, e, id, *finish, gate.DryRun())
}

// accountEraseRunner is the part of *accounterasure.Eraser the command
// drives; tests substitute it.
type accountEraseRunner interface {
	Erase(ctx context.Context, id uuid.UUID, actor platform.ActorKind) (accounterasure.Report, error)
	FinishBySlug(ctx context.Context, slug string) (accounterasure.Report, error)
}

// accountErasePlanner is the dry-run read.
type accountErasePlanner interface {
	PlanErasure(ctx context.Context, id uuid.UUID) (postgresstore.ErasurePlan, error)
	SlugErased(ctx context.Context, slug string) (bool, error)
}

func runAccountErase(
	ctx context.Context, out io.Writer, e *accounterasure.Eraser, id uuid.UUID, finish string, dryRun bool,
) error {
	return runAccountEraseWith(ctx, out, e, e.Store, id, finish, dryRun)
}

func runAccountEraseWith(
	ctx context.Context, out io.Writer, run accountEraseRunner, plan accountErasePlanner,
	id uuid.UUID, finish string, dryRun bool,
) error {
	if dryRun {
		if finish != "" {
			ok, err := plan.SlugErased(ctx, finish)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("slug %q was never erased; -finish-slug would refuse it", finish)
			}
			_, _ = fmt.Fprintf(out, "DRY RUN — would delete the Redis keys and usage counters of erased slug %q\n", finish)
			return nil
		}
		p, err := plan.PlanErasure(ctx, id)
		if errors.Is(err, platform.ErrNotFound) {
			_, _ = fmt.Fprintf(out, "account %s does not exist (already erased?)\n", id)
			return nil
		}
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "DRY RUN — would erase account %s: %d member(s), %d Postgres key(s)\n",
			id, len(p.UserIDs), len(p.KeyIDs))
		return nil
	}
	if finish != "" {
		rep, err := run.FinishBySlug(ctx, finish)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "account-erase: finished slug %q: %d Redis key record(s), %d other Redis key(s), %d late usage row(s)\n",
			finish, rep.RedisKeys, rep.RedisDeleted, rep.LateUsageRows)
		return nil
	}
	rep, err := run.Erase(ctx, id, platform.ActorStaff)
	if err != nil {
		return err
	}
	if rep.AlreadyErased {
		_, _ = fmt.Fprintf(out, "account-erase: account %s was already erased; nothing to do\n", id)
		return nil
	}
	c := rep.Counts
	_, _ = fmt.Fprintf(out, "account-erase: erased %s: %d user(s), %d key(s), %d webhook(s), %d audit row(s) scrubbed, "+
		"%d Redis key record(s)\n", id, c.Users, c.APIKeys, c.Webhooks, c.AuditRowsScrubbed, rep.RedisKeys)
	return nil
}
