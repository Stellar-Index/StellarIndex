// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"iter"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/historyarchive"
	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/support/storage"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

const (
	networkCheckHotArchive = "hotarchive"
	networkCheckLumens     = "lumens"

	// hotArchiveBatch bounds one ledger_entries_current key lookup.
	hotArchiveBatch = 500
	// hotArchiveMismatchSample is how many mismatched keys a run prints.
	hotArchiveMismatchSample = 20
)

// verifyNetworkState is the stellarindex-ops `verify-network-state`
// subcommand: it compares the lake's derived current state with state the
// network itself publishes, which no other verifier does (ADR-0033's checks
// compare our rows with our own events; verify-archive checks headers only).
//
//  1. hotarchive — every archived entry in the history archive's hot-archive
//     bucket list at a checkpoint must match ledger_entries_current's row for
//     that key. Protocol upgrades can rewrite hot-archive entries without any
//     ledger-meta change, so this is the only place such a rewrite shows.
//  2. lumens — native XLM held in accounts, claimable balances, liquidity
//     pools and native-SAC contract balances, plus the header's fee_pool,
//     must equal the header's total_coins exactly.
//
// Exit code = hot-archive mismatches (+1 for a run that compared nothing)
// + 1 for a non-zero lumen residual, capped at 255. Keys the lake does not
// hold, or changed after the checkpoint, are reported but not counted: they
// are coverage, not faithfulness. -textfile writes network_state_verify.prom
// only after every requested check completed.
func verifyNetworkState(args []string) error {
	fs := flag.NewFlagSet("verify-network-state", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/stellarindex.toml", "path to stellarindex.toml — the network passphrase and history archive URL come from it, so it must load")
	chAddr := fs.String("ch-addr", "", "ClickHouse native address (default: cfg.storage.clickhouse_addr, then "+defaultCHAddr+")")
	archiveURL := fs.String("archive", "", "history archive URL (default: cfg.stellar.history_archive_url)")
	checkpoint := fs.Uint64("checkpoint", 0, "hot-archive checkpoint ledger; 0 = newest checkpoint at or below both the lake tip and the archive's latest")
	checksFlag := fs.String("checks", networkCheckHotArchive+","+networkCheckLumens, "comma-separated subset of checks: hotarchive | lumens")
	textfile := fs.String("textfile", "", "node_exporter textfile path, e.g. /var/lib/node_exporter/textfile_collector/network_state_verify.prom; empty = none")
	if err := fs.Parse(args); err != nil {
		return err
	}
	checks, err := parseNetworkChecks(*checksFlag)
	if err != nil {
		return err
	}
	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return fmt.Errorf("verify-network-state: load -config (the network to compare against comes from it): %w", err)
	}
	passphrase := cfg.Stellar.Passphrase()
	url := *archiveURL
	if url == "" {
		url = cfg.Stellar.HistoryArchiveURL
	}
	addr := resolveCHAddr(*chAddr, *cfgPath)

	ctx, cancel := opsutil.SignalContext()
	defer cancel()
	reader, err := clickhouse.NewNetworkStateReader(ctx, addr)
	if err != nil {
		return fmt.Errorf("verify-network-state: open clickhouse: %w", err)
	}
	defer func() { _ = reader.Close() }()

	var res networkStateResult
	if checks[networkCheckHotArchive] {
		if res.hot, err = runHotArchiveCheck(ctx, reader, addr, url, passphrase, *checkpoint); err != nil {
			return err
		}
	}
	if checks[networkCheckLumens] {
		canonical.InstallNetworkPassphrase(passphrase)
		sac, err := canonical.NativeAsset().SacContractID()
		if err != nil {
			return fmt.Errorf("verify-network-state: native SAC id: %w", err)
		}
		if res.lumens, err = reader.LumenConservation(ctx, sac); err != nil {
			return fmt.Errorf("verify-network-state: lumens: %w", err)
		}
	}

	fmt.Print(res.report())
	if *textfile != "" {
		if err := writeAtomic(*textfile, res.renderProm(time.Now())); err != nil {
			return fmt.Errorf("verify-network-state: write textfile: %w", err)
		}
	}
	if total := res.failures(); total > 0 {
		return &opsutil.ExitCodeError{Code: int(min(total, 255))} //nolint:gosec // capped at 255
	}
	return nil
}

func parseNetworkChecks(raw string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, tok := range strings.Split(raw, ",") {
		switch tok = strings.TrimSpace(tok); tok {
		case "":
		case networkCheckHotArchive, networkCheckLumens:
			out[tok] = true
		default:
			return nil, fmt.Errorf("verify-network-state: -checks: unknown check %q (want %s|%s)", tok, networkCheckHotArchive, networkCheckLumens)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("verify-network-state: -checks: no checks specified")
	}
	return out, nil
}

// runHotArchiveCheck resolves the checkpoint and reconciles its hot archive.
func runHotArchiveCheck(ctx context.Context, reader *clickhouse.NetworkStateReader, addr, url, passphrase string, want uint64) (*hotArchiveCounts, error) {
	arch, err := historyarchive.Connect(url, historyarchive.ArchiveOptions{
		NetworkPassphrase: passphrase,
		ConnectOptions:    storage.ConnectOptions{Context: ctx, UserAgent: "stellarindex-ops/verify-network-state"},
	})
	if err != nil {
		return nil, fmt.Errorf("verify-network-state: connect history archive %q: %w", url, err)
	}
	lakeTip, err := clickhouse.MaxLedger(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("verify-network-state: lake tip: %w", err)
	}
	archTip, err := arch.GetLatestLedgerSequence()
	if err != nil {
		return nil, fmt.Errorf("verify-network-state: archive tip: %w", err)
	}
	seq, err := pickHotArchiveCheckpoint(arch.GetCheckpointManager(), want, lakeTip, archTip)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "verify-network-state: hot archive at checkpoint %d from %s (lake tip %d)\n", seq, url, lakeTip)
	n, err := reconcileHotArchive(ctx, func(ictx context.Context) iter.Seq2[xdr.LedgerEntry, error] {
		return ingest.NewHotArchiveIterator(ictx, arch, seq)
	}, seq, reader.CurrentEntries)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// pickHotArchiveCheckpoint returns want when it is a checkpoint both sides
// have reached, or (want=0) the newest such checkpoint. A checkpoint above
// the lake tip would compare the network's state with a lake that has not
// yet ingested the ledgers that produced it.
func pickHotArchiveCheckpoint(cm historyarchive.CheckpointManager, want uint64, lakeTip, archTip uint32) (uint32, error) {
	limit := min(lakeTip, archTip)
	if want == 0 {
		return cm.PrevCheckpoint(limit), nil
	}
	seq, err := toLedgerSeq("-checkpoint", want)
	if err != nil {
		return 0, err
	}
	if !cm.IsCheckpoint(seq) {
		return 0, fmt.Errorf("verify-network-state: -checkpoint %d is not a checkpoint ledger", seq)
	}
	if seq > limit {
		return 0, fmt.Errorf("verify-network-state: -checkpoint %d is above the lake tip %d or archive tip %d", seq, lakeTip, archTip)
	}
	return seq, nil
}

// hotArchiveLookup is NetworkStateReader.CurrentEntries' shape.
type hotArchiveLookup func(ctx context.Context, entryType string, keys []string) (map[string]clickhouse.CurrentEntry, error)

// hotArchiveCounts classifies every archived entry at one checkpoint.
type hotArchiveCounts struct {
	checkpoint uint32
	archived   uint64
	matched    uint64
	newer      uint64 // the lake's row changed after the checkpoint (e.g. restored)
	absent     uint64 // the lake holds no row for the key
	mismatches uint64
	sample     []string // "entry_type key: reason", first hotArchiveMismatchSample
}

// failures counts mismatches, plus one for a run that archived entries yet
// compared none of them: a run that verified nothing must not read as clean.
func (n *hotArchiveCounts) failures() uint64 {
	if n.archived > 0 && n.matched+n.mismatches == 0 {
		return n.mismatches + 1
	}
	return n.mismatches
}

type archivedEntry struct {
	key  string
	data []byte // the archived entry's LedgerEntryData XDR
}

// reconcileHotArchive compares each archived entry with the lake's row for
// its key, batching lookups per entry type. open builds the entry iterator
// on a context this function cancels before any early return.
func reconcileHotArchive(ctx context.Context, open func(context.Context) iter.Seq2[xdr.LedgerEntry, error], checkpoint uint32, lookup hotArchiveLookup) (hotArchiveCounts, error) {
	n := hotArchiveCounts{checkpoint: checkpoint}
	ictx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Leaving the range loop makes the SDK iterator wait for its stream
	// goroutine, which blocks on a full buffer until ictx is done; a deferred
	// cancel would run only after that wait, so cancel first.
	fail := func(err error) (hotArchiveCounts, error) {
		cancel()
		return n, err
	}
	batches := map[string][]archivedEntry{}
	flush := func(entryType string) error {
		batch := batches[entryType]
		delete(batches, entryType)
		keys := make([]string, len(batch))
		for i, a := range batch {
			keys[i] = a.key
		}
		ours, err := lookup(ctx, entryType, keys)
		if err != nil {
			return fmt.Errorf("verify-network-state: hot archive lookup: %w", err)
		}
		for _, a := range batch {
			row, ok := ours[a.key]
			n.classify(entryType, a, row, ok)
		}
		return nil
	}
	for e, err := range open(ictx) {
		if err != nil {
			return fail(fmt.Errorf("verify-network-state: read hot archive at %d: %w", checkpoint, err))
		}
		a, entryType, err := toArchivedEntry(e)
		if err != nil {
			return fail(err)
		}
		n.archived++
		batches[entryType] = append(batches[entryType], a)
		if len(batches[entryType]) >= hotArchiveBatch {
			if err := flush(entryType); err != nil {
				return fail(err)
			}
		}
	}
	rest := make([]string, 0, len(batches))
	for t := range batches {
		rest = append(rest, t)
	}
	sort.Strings(rest)
	for _, t := range rest {
		if err := flush(t); err != nil {
			return n, err
		}
	}
	return n, nil
}

func toArchivedEntry(e xdr.LedgerEntry) (archivedEntry, string, error) {
	key, err := e.LedgerKey()
	if err != nil {
		return archivedEntry{}, "", fmt.Errorf("verify-network-state: archived entry key: %w", err)
	}
	entryType, keyXDR, err := clickhouse.LedgerKeyColumns(key)
	if err != nil {
		return archivedEntry{}, "", err
	}
	data, err := e.Data.MarshalBinary()
	if err != nil {
		return archivedEntry{}, "", fmt.Errorf("verify-network-state: marshal archived entry: %w", err)
	}
	return archivedEntry{key: keyXDR, data: data}, entryType, nil
}

// classify compares the entry body only: the archive and the lake may stamp
// lastModifiedLedgerSeq differently for the same state.
func (n *hotArchiveCounts) classify(entryType string, a archivedEntry, ours clickhouse.CurrentEntry, found bool) {
	switch {
	case !found:
		n.absent++
	case ours.LedgerSeq > n.checkpoint:
		n.newer++
	case ours.ChangeType == "removed":
		n.mismatch(entryType, a.key, "removed in the lake")
	default:
		var e xdr.LedgerEntry
		if err := xdr.SafeUnmarshalBase64(ours.EntryXDR, &e); err != nil {
			n.mismatch(entryType, a.key, "lake entry undecodable")
			return
		}
		data, err := e.Data.MarshalBinary()
		if err != nil || !bytes.Equal(data, a.data) {
			n.mismatch(entryType, a.key, fmt.Sprintf("value differs (lake row at ledger %d)", ours.LedgerSeq))
			return
		}
		n.matched++
	}
}

func (n *hotArchiveCounts) mismatch(entryType, key, reason string) {
	n.mismatches++
	if len(n.sample) < hotArchiveMismatchSample {
		n.sample = append(n.sample, fmt.Sprintf("%s %s: %s", entryType, key, reason))
	}
}

// networkStateResult holds whichever checks ran; nil = not requested.
type networkStateResult struct {
	hot    *hotArchiveCounts
	lumens *clickhouse.LumenTally
}

func (r networkStateResult) lumenFailures() uint64 {
	if r.lumens != nil && r.lumens.Residual().Sign() != 0 {
		return 1
	}
	return 0
}

func (r networkStateResult) failures() uint64 {
	var total uint64
	if r.hot != nil {
		total += r.hot.failures()
	}
	return total + r.lumenFailures()
}

func (r networkStateResult) report() string {
	var w strings.Builder
	if h := r.hot; h != nil {
		fmt.Fprintf(&w, "\n=== hot archive @ checkpoint %d ===\n", h.checkpoint)
		fmt.Fprintf(&w, "archived=%d matched=%d mismatched=%d absent_from_lake=%d changed_after_checkpoint=%d\n",
			h.archived, h.matched, h.mismatches, h.absent, h.newer)
		for _, s := range h.sample {
			fmt.Fprintf(&w, "  MISMATCH %s\n", s)
		}
		if h.failures() > h.mismatches {
			fmt.Fprintln(&w, "  FAIL: the hot archive holds entries but none could be compared with the lake")
		}
	}
	if t := r.lumens; t != nil {
		fmt.Fprintf(&w, "\n=== lumen conservation @ ledger %d (stroops) ===\n", t.Ledger)
		fmt.Fprintf(&w, "accounts=%s claimable_balances=%s liquidity_pools=%s contract_balances=%s fee_pool=%d total_coins=%d preimages=%d\n",
			t.Accounts, t.ClaimableBalances, t.LiquidityPools, t.ContractBalances, t.FeePool, t.TotalCoins, t.Preimages)
		fmt.Fprintf(&w, "residual (held + fee_pool - total_coins) = %s\n", t.Residual())
	}
	verdict := "PASSED"
	if r.failures() > 0 {
		verdict = "FAILED"
	}
	fmt.Fprintf(&w, "verify-network-state: summary total_failures=%d (%s)\n", r.failures(), verdict)
	return w.String()
}

// renderProm renders network_state_verify.prom; a check that did not run
// emits no failures series. Pure.
func (r networkStateResult) renderProm(now time.Time) string {
	var b strings.Builder
	b.WriteString("# HELP stellarindex_network_state_verify_failures Failures verify-network-state found per check (hot-archive mismatches; 1 for a non-zero lumen residual). Emitted only for checks that ran.\n")
	b.WriteString("# TYPE stellarindex_network_state_verify_failures gauge\n")
	if r.hot != nil {
		fmt.Fprintf(&b, "stellarindex_network_state_verify_failures{check=%q} %d\n", "hot_archive", r.hot.failures())
	}
	if r.lumens != nil {
		fmt.Fprintf(&b, "stellarindex_network_state_verify_failures{check=%q} %d\n", "lumens", r.lumenFailures())
	}
	if h := r.hot; h != nil {
		b.WriteString("# HELP stellarindex_network_state_verify_hot_archive_entries Hot-archive entries at the checked checkpoint, by comparison outcome.\n")
		b.WriteString("# TYPE stellarindex_network_state_verify_hot_archive_entries gauge\n")
		for _, c := range []struct {
			class string
			v     uint64
		}{{"matched", h.matched}, {"mismatched", h.mismatches}, {"absent", h.absent}, {"newer", h.newer}} {
			fmt.Fprintf(&b, "stellarindex_network_state_verify_hot_archive_entries{class=%q} %d\n", c.class, c.v)
		}
	}
	b.WriteString("# HELP stellarindex_network_state_verify_last_run_unix When verify-network-state last completed every requested check.\n")
	b.WriteString("# TYPE stellarindex_network_state_verify_last_run_unix gauge\n")
	fmt.Fprintf(&b, "stellarindex_network_state_verify_last_run_unix %d\n", now.Unix())
	return b.String()
}
