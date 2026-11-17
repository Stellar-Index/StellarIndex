package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/keys"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// directory-override clears a FALSE-POSITIVE scam flag in account_directory, which withholds the issuer's price on every gated surface; a hand UPDATE is overwritten by the next daily directory-sync. See timescale.DirectoryOperatorOverrideSource.
//
//	stellarindex-ops directory-override -config PATH -address G… -clear-scam-flag -reason TEXT [-actor NAME] -write
//	stellarindex-ops directory-override -config PATH -address G… -delete -reason TEXT [-actor NAME] -write
//
// Both writes append an audit_log row first and change nothing if that fails: the row's override_by/override_reason are mutable
// and -delete removes them.

// directoryOverrideStore is the storage seam; *timescale.Store satisfies it.
type directoryOverrideStore interface {
	DirectoryEntryByAddress(ctx context.Context, address string) (timescale.DirectoryEntry, bool, error)
	ClearDirectoryScamFlag(ctx context.Context, address, operator, reason string) (before, after timescale.DirectoryEntry, found bool, err error)
	DeleteDirectoryOverride(ctx context.Context, address string) (bool, error)
}

type directoryOverrideRequest struct {
	address       string
	clearScamFlag bool
	reason        string
	actor         string
	remove        bool
	write         bool
}

func (r directoryOverrideRequest) validate() error {
	if !directoryAddressRe.MatchString(r.address) {
		return fmt.Errorf("-address must be a G… or C… strkey (got %q)", r.address)
	}
	if r.clearScamFlag == r.remove {
		return errors.New("pass exactly one of -clear-scam-flag or -delete")
	}
	if strings.TrimSpace(r.reason) == "" {
		return errors.New("-reason is required: it is recorded in audit_log with the actor (for -clear-scam-flag, why the upstream flag is a false positive)")
	}
	if strings.TrimSpace(r.actor) == "" {
		return errors.New("an operator is required: pass -actor (the OS user could not be resolved)")
	}
	return nil
}

func directoryOverride(args []string) error {
	fs, gate := opsutil.NewMutatingFlagSet("directory-override")
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	address := fs.String("address", "", "G… account or C… contract strkey whose directory row to override (required)")
	clearScam := fs.Bool("clear-scam-flag", false, "Take the row over as operator-override with its scam-class tags removed; name, domain and every other tag are kept")
	reason := fs.String("reason", "", "Why (required): for -clear-scam-flag, why the upstream scam flag is a false positive, also stored as the row's override_reason. Recorded in audit_log")
	actor := fs.String("actor", "", "Who is acting; recorded in audit_log and, with -clear-scam-flag, as the row's override_by. Defaults to the OS user")
	remove := fs.Bool("delete", false, "Remove the operator override; the next directory-sync restores the upstream row")
	timeout := fs.Duration("timeout", time.Minute, "Wall-clock timeout for the whole run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	req := directoryOverrideRequest{address: *address, clearScamFlag: *clearScam, reason: *reason, remove: *remove, write: gate.Enabled()}
	a, err := opsutil.ResolveActor(*actor)
	if err != nil {
		return err
	}
	req.actor = a
	if err := req.validate(); err != nil {
		return err
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
		return err
	}
	defer func() { _ = store.Close() }()
	return runDirectoryOverride(ctx, store, postgresstore.NewAuditStore(postgresstore.New(store.DB())), os.Stdout, req)
}

func runDirectoryOverride(ctx context.Context, st directoryOverrideStore, audit keys.AuditSink, w io.Writer, req directoryOverrideRequest) error {
	if err := req.validate(); err != nil {
		return err
	}
	cur, found, err := st.DirectoryEntryByAddress(ctx, req.address)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("directory-override: %s has no account_directory row — nothing to override", req.address)
	}
	if req.remove {
		return runDirectoryOverrideDelete(ctx, st, audit, w, req, cur)
	}
	return runDirectoryOverrideClear(ctx, st, audit, w, req, cur)
}

// appendDirectoryOverrideAudit records the action before it is taken, so no override changes without a durable who/what/why.
func appendDirectoryOverrideAudit(ctx context.Context, audit keys.AuditSink, action string, req directoryOverrideRequest, cur timescale.DirectoryEntry) error {
	detail := map[string]any{"source_before": cur.Source, "tags_before": cur.Tags}
	if err := keys.AppendOpsAudit(ctx, audit, action, "directory-override", "account_directory", req.address, req.actor, req.reason, detail); err != nil {
		return fmt.Errorf("directory-override: audit_log append for %s failed, so NOTHING was changed: %w", req.address, err)
	}
	return nil
}

func runDirectoryOverrideClear(ctx context.Context, st directoryOverrideStore, audit keys.AuditSink, w io.Writer, req directoryOverrideRequest, cur timescale.DirectoryEntry) error {
	kept := timescale.DirectoryTagsWithoutScamFlags(cur.Tags)
	if len(kept) == len(cur.Tags) {
		return fmt.Errorf("%w: %s (tags %v)", timescale.ErrDirectoryNotScamFlagged, req.address, cur.Tags)
	}
	if !req.write {
		_, _ = fmt.Fprintf(w, "Would take over %s (source=%s → %s): tags %v → %v; name %q and domain %q kept; reason %q; by %q. Dry run — nothing written.\n",
			req.address, cur.Source, timescale.DirectoryOperatorOverrideSource, cur.Tags, kept, cur.Name, cur.Domain, req.reason, req.actor)
		return nil
	}
	if err := appendDirectoryOverrideAudit(ctx, audit, "directory.clear_scam_flag", req, cur); err != nil {
		return err
	}
	before, after, found, err := st.ClearDirectoryScamFlag(ctx, req.address, req.actor, req.reason)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("directory-override: %s vanished before the takeover — nothing written", req.address)
	}
	_, _ = fmt.Fprintf(w, "Override written for %s (source=%s → %s): tags %v → %v; reason %q; by %q. The price gate stops withholding within its cache TTL.\n",
		req.address, before.Source, after.Source, before.Tags, after.Tags, req.reason, req.actor)
	return nil
}

func runDirectoryOverrideDelete(ctx context.Context, st directoryOverrideStore, audit keys.AuditSink, w io.Writer, req directoryOverrideRequest, cur timescale.DirectoryEntry) error {
	if cur.Source != timescale.DirectoryOperatorOverrideSource {
		return fmt.Errorf("directory-override: %s is owned by source %q, not an operator override — nothing to delete", req.address, cur.Source)
	}
	if !req.write {
		_, _ = fmt.Fprintf(w, "Would delete the operator override for %s (tags %v); the next directory-sync restores the upstream row. Dry run — nothing written.\n",
			req.address, cur.Tags)
		return nil
	}
	if err := appendDirectoryOverrideAudit(ctx, audit, "directory.delete_override", req, cur); err != nil {
		return err
	}
	removed, err := st.DeleteDirectoryOverride(ctx, req.address)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("directory-override: override for %s vanished before the delete — nothing removed", req.address)
	}
	_, _ = fmt.Fprintf(w, "Override deleted for %s; the next directory-sync restores the upstream row.\n", req.address)
	return nil
}
