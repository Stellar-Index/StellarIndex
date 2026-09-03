package ingest

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// directory-sync mirrors the MIT-licensed
// github.com/stellar-expert/public-directory account labels into the
// `account_directory` table (migration 0136): one HTTPS GET of the
// repo tarball, parse accounts/*.json, upsert the full set, prune
// rows upstream removed. ~18.5k entries as of 2026-08.
//
// Run from a daily timer:
//
//	stellarindex-ops directory-sync -config /etc/stellarindex.toml
//
// The labels are DISPLAY data with third-party attribution — they
// never feed verification, the currency catalogue, or the in-repo
// scam list. `ReplaceDirectory` refuses an empty parse (a broken
// fetch must not prune the table), and per-file JSON errors are
// counted + reported, failing the run only if EVERYTHING failed.
const (
	directorySource     = "stellar-expert"
	directoryDefaultURL = "https://github.com/stellar-expert/public-directory/archive/refs/heads/master.tar.gz"

	// directoryMaxTarballBytes bounds the decompressed read. The
	// tarball is ~4 MB compressed today; 256 MB of headroom is an
	// order-of-magnitude guard against a decompression bomb from a
	// compromised -url, not a tight fit.
	directoryMaxTarballBytes = 256 << 20
)

// directoryAddressRe matches the strkey forms the table's CHECK
// accepts. Entries outside it (malformed upstream rows) are skipped
// and counted rather than failing the whole chunk's INSERT.
var directoryAddressRe = regexp.MustCompile(`^[GC][A-Z2-7]{55}$`)

type directoryFileEntry struct {
	Address string   `json:"address"`
	Name    string   `json:"name"`
	Domain  string   `json:"domain"`
	Tags    []string `json:"tags"`
}

func directorySync(args []string) error {
	fs := flag.NewFlagSet("directory-sync", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	url := fs.String("url", directoryDefaultURL, "Tarball URL of the public-directory repo (https only)")
	timeout := fs.Duration("timeout", 5*time.Minute, "Wall-clock timeout for the whole run")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	if !strings.HasPrefix(*url, "https://") {
		return fmt.Errorf("-url must be https:// (got %q)", *url)
	}
	gate.Banner()
	dryRun := gate.DryRun()

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	entries, skipped, err := fetchDirectoryTarball(ctx, *url)
	if err != nil {
		return err
	}
	fmt.Printf("Parsed %d directory entries (%d skipped: bad address/JSON).\n", len(entries), skipped)

	if dryRun {
		fmt.Println("Dry run — nothing written.")
		return nil
	}

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	upserted, pruned, err := store.ReplaceDirectory(ctx, directorySource, entries)
	if err != nil {
		return err
	}
	fmt.Printf("Synced: %d upserted, %d pruned (source=%s).\n", upserted, pruned, directorySource)
	return nil
}

// directoryFetchTimeout bounds the directory tarball download. Generous
// because the tarball is a few MB over the public internet, but finite:
// an unbounded fetch is a hang, not a slow success.
const directoryFetchTimeout = 2 * time.Minute

// fetchDirectoryTarball downloads the repo tarball and parses every
// accounts/*.json blob into DirectoryEntry rows.
func fetchDirectoryTarball(ctx context.Context, url string) (entries []timescale.DirectoryEntry, skipped int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	// http.DefaultClient has NO timeout. A tarball fetch against a
	// third-party host that accepts the connection and then stops sending
	// would hang this command forever — same class as the kraken REST
	// call fixed alongside this (#371 F5), and the reason that one was
	// worth finding: an operator command that never returns looks like a
	// slow network, not a bug.
	//
	// ctx still bounds it when the caller supplies a deadline; this makes
	// the bound unconditional.
	client := &http.Client{Timeout: directoryFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("directory-sync: fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("directory-sync: fetch: HTTP %d from %s", resp.StatusCode, url)
	}

	gz, err := gzip.NewReader(io.LimitReader(resp.Body, directoryMaxTarballBytes))
	if err != nil {
		return nil, 0, fmt.Errorf("directory-sync: gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(io.LimitReader(gz, directoryMaxTarballBytes))
	for {
		hdr, herr := tr.Next()
		if errors.Is(herr, io.EOF) {
			break
		}
		if herr != nil {
			return nil, 0, fmt.Errorf("directory-sync: tar: %w", herr)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// Tarball paths are <repo>-<branch>/accounts/<ADDRESS>.json.
		dir, file := path.Split(hdr.Name)
		if !strings.HasSuffix(dir, "/accounts/") || !strings.HasSuffix(file, ".json") {
			continue
		}
		e, ok := parseDirectoryFile(tr)
		if !ok {
			skipped++
			continue
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return nil, skipped, fmt.Errorf("directory-sync: parsed 0 entries (%d skipped) — refusing; upstream layout changed or fetch was truncated", skipped)
	}
	return entries, skipped, nil
}

// parseDirectoryFile decodes one accounts/<ADDRESS>.json blob,
// returning ok=false for malformed JSON, a bad address, or an empty
// name (a nameless label is useless downstream).
func parseDirectoryFile(r io.Reader) (timescale.DirectoryEntry, bool) {
	var f directoryFileEntry
	if err := json.NewDecoder(r).Decode(&f); err != nil {
		return timescale.DirectoryEntry{}, false
	}
	if !directoryAddressRe.MatchString(f.Address) || f.Name == "" {
		return timescale.DirectoryEntry{}, false
	}
	tags := f.Tags
	if tags == nil {
		tags = []string{}
	}
	return timescale.DirectoryEntry{
		Address: f.Address,
		Name:    f.Name,
		Domain:  f.Domain,
		Tags:    tags,
		Source:  directorySource,
	}, true
}
