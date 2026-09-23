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
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// directory-override is the operator's lever for a FALSE-POSITIVE scam
// flag in account_directory: the flag withholds the issuer's price
// across every gated surface, and a hand `UPDATE` is overwritten by the
// next daily directory-sync. See timescale.DirectoryOperatorOverrideSource.
//
//	stellarindex-ops directory-override -config PATH -address G… -clear-scam-flag -reason TEXT -write
//	stellarindex-ops directory-override -config PATH -address G… -delete -write

// directoryOverrideStore is the storage seam; *timescale.Store satisfies it.
type directoryOverrideStore interface {
	DirectoryEntryByAddress(ctx context.Context, address string) (timescale.DirectoryEntry, bool, error)
	ClearDirectoryScamFlag(ctx context.Context, address, reason string) (before, after timescale.DirectoryEntry, found bool, err error)
	DeleteDirectoryOverride(ctx context.Context, address string) (bool, error)
}

type directoryOverrideRequest struct {
	address       string
	clearScamFlag bool
	reason        string
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
	if r.clearScamFlag && strings.TrimSpace(r.reason) == "" {
		return errors.New("-clear-scam-flag needs -reason: why the upstream flag is a false positive is stored with the override for review")
	}
	return nil
}

func directoryOverride(args []string) error {
	fs, gate := opsutil.NewMutatingFlagSet("directory-override")
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	address := fs.String("address", "", "G… account or C… contract strkey whose directory row to override (required)")
	clearScam := fs.Bool("clear-scam-flag", false, "Take the row over as operator-override with its scam-class tags removed; name, domain and every other tag are kept")
	reason := fs.String("reason", "", "Why the upstream scam flag is a false positive (required with -clear-scam-flag); stored as the row's override_reason")
	remove := fs.Bool("delete", false, "Remove the operator override; the next directory-sync restores the upstream row")
	timeout := fs.Duration("timeout", time.Minute, "Wall-clock timeout for the whole run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	req := directoryOverrideRequest{address: *address, clearScamFlag: *clearScam, reason: *reason, remove: *remove, write: gate.Enabled()}
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
	return runDirectoryOverride(ctx, store, os.Stdout, req)
}

func runDirectoryOverride(ctx context.Context, st directoryOverrideStore, w io.Writer, req directoryOverrideRequest) error {
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
		return runDirectoryOverrideDelete(ctx, st, w, req, cur)
	}
	return runDirectoryOverrideClear(ctx, st, w, req, cur)
}

func runDirectoryOverrideClear(ctx context.Context, st directoryOverrideStore, w io.Writer, req directoryOverrideRequest, cur timescale.DirectoryEntry) error {
	kept := timescale.DirectoryTagsWithoutScamFlags(cur.Tags)
	if len(kept) == len(cur.Tags) {
		return fmt.Errorf("%w: %s (tags %v)", timescale.ErrDirectoryNotScamFlagged, req.address, cur.Tags)
	}
	if !req.write {
		_, _ = fmt.Fprintf(w, "Would take over %s (source=%s → %s): tags %v → %v; name %q and domain %q kept; reason %q. Dry run — nothing written.\n",
			req.address, cur.Source, timescale.DirectoryOperatorOverrideSource, cur.Tags, kept, cur.Name, cur.Domain, req.reason)
		return nil
	}
	before, after, found, err := st.ClearDirectoryScamFlag(ctx, req.address, req.reason)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("directory-override: %s vanished before the takeover — nothing written", req.address)
	}
	_, _ = fmt.Fprintf(w, "Override written for %s (source=%s → %s): tags %v → %v; reason %q. The price gate stops withholding within its cache TTL.\n",
		req.address, before.Source, after.Source, before.Tags, after.Tags, req.reason)
	return nil
}

func runDirectoryOverrideDelete(ctx context.Context, st directoryOverrideStore, w io.Writer, req directoryOverrideRequest, cur timescale.DirectoryEntry) error {
	if cur.Source != timescale.DirectoryOperatorOverrideSource {
		return fmt.Errorf("directory-override: %s is owned by source %q, not an operator override — nothing to delete", req.address, cur.Source)
	}
	if !req.write {
		_, _ = fmt.Fprintf(w, "Would delete the operator override for %s (tags %v); the next directory-sync restores the upstream row. Dry run — nothing written.\n",
			req.address, cur.Tags)
		return nil
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
