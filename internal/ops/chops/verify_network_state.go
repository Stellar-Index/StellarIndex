// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"iter"
	"math/big"
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
	networkCheckLiveState  = "livestate"

	// hotArchiveBatch bounds one ledger_entries_current key lookup.
	hotArchiveBatch = 500
	// hotArchiveMismatchSample is how many mismatched keys a run prints.
	hotArchiveMismatchSample = 20
)

// verifyNetworkState compares the lake's derived state with network-published state: hot-archive entries vs ledger_entries_current, and
// native XLM holdings plus fee_pool vs total_coins exactly. Exit code counts mismatches (comparing nothing counts 1), capped at 255; keys
// the lake lacks or that changed after the checkpoint are coverage, not faithfulness.
func verifyNetworkState(args []string) error {
	fs := flag.NewFlagSet("verify-network-state", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/stellarindex.toml", "path to stellarindex.toml — the network passphrase and history archive URL come from it, so it must load")
	chAddr := fs.String("ch-addr", "", "ClickHouse native address (default: cfg.storage.clickhouse_addr, then "+defaultCHAddr+")")
	archiveURL := fs.String("archive", "", "history archive URL (default: cfg.stellar.history_archive_url)")
	checkpoint := fs.Uint64("checkpoint", 0, "hot-archive checkpoint ledger; 0 = newest checkpoint at or below both the lake tip and the archive's latest")
	checksFlag := fs.String("checks", networkCheckHotArchive+","+networkCheckLumens, "comma-separated subset of checks: hotarchive | lumens | livestate (streams the whole live bucket list; opt-in)")
	textfile := fs.String("textfile", "", "node_exporter textfile path, e.g. /var/lib/node_exporter/textfile_collector/network_state_verify.prom; empty = none")
	liveDump := fs.String("livestate-dump", "", "livestate: also write every streamed live entry as entry_type, ledger key, holder, stroops, last-modified ledger (TSV) to this path")
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

	res, err := runNetworkChecks(ctx, reader, checks, networkCheckInputs{addr: addr, url: url, passphrase: passphrase, checkpoint: *checkpoint, liveDump: *liveDump})
	if err != nil {
		return err
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

// networkCheckInputs is what every check resolves its sides from.
type networkCheckInputs struct {
	addr, url, passphrase string
	checkpoint            uint64
	liveDump              string
}

// runNetworkChecks runs the requested checks in a fixed order and stops at the first that cannot complete.
func runNetworkChecks(ctx context.Context, reader *clickhouse.NetworkStateReader, checks map[string]bool, in networkCheckInputs) (networkStateResult, error) {
	var res networkStateResult
	var err error
	if checks[networkCheckHotArchive] {
		if res.hot, err = runHotArchiveCheck(ctx, reader, in.addr, in.url, in.passphrase, in.checkpoint); err != nil {
			return res, err
		}
	}
	if !checks[networkCheckLumens] && !checks[networkCheckLiveState] {
		return res, nil
	}
	canonical.InstallNetworkPassphrase(in.passphrase)
	sacID, err := canonical.NativeAsset().SacContractID()
	if err != nil {
		return res, fmt.Errorf("verify-network-state: native SAC id: %w", err)
	}
	if checks[networkCheckLumens] {
		if res.lumens, err = reader.LumenConservation(ctx, sacID); err != nil {
			return res, fmt.Errorf("verify-network-state: lumens: %w", err)
		}
	}
	if checks[networkCheckLiveState] {
		if res.live, err = runLiveStateCheck(ctx, reader, in.addr, in.url, in.passphrase, in.checkpoint, sacID, in.liveDump); err != nil {
			return res, err
		}
	}
	return res, nil
}

func parseNetworkChecks(raw string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, tok := range strings.Split(raw, ",") {
		switch tok = strings.TrimSpace(tok); tok {
		case "":
		case networkCheckHotArchive, networkCheckLumens, networkCheckLiveState:
			out[tok] = true
		default:
			return nil, fmt.Errorf("verify-network-state: -checks: unknown check %q (want %s|%s|%s)", tok, networkCheckHotArchive, networkCheckLumens, networkCheckLiveState)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("verify-network-state: -checks: no checks specified")
	}
	return out, nil
}

// runHotArchiveCheck resolves the checkpoint and reconciles its hot archive.
func runHotArchiveCheck(ctx context.Context, reader *clickhouse.NetworkStateReader, addr, url, passphrase string, want uint64) (*hotArchiveCounts, error) {
	arch, seq, lakeTip, err := openArchiveCheckpoint(ctx, addr, url, passphrase, want)
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

// openArchiveCheckpoint connects the history archive and resolves the checkpoint both it and the lake have reached.
func openArchiveCheckpoint(ctx context.Context, addr, url, passphrase string, want uint64) (*historyarchive.Archive, uint32, uint32, error) {
	arch, err := historyarchive.Connect(url, historyarchive.ArchiveOptions{
		NetworkPassphrase: passphrase,
		ConnectOptions:    storage.ConnectOptions{Context: ctx, UserAgent: "stellarindex-ops/verify-network-state"},
	})
	if err != nil {
		return nil, 0, 0, fmt.Errorf("verify-network-state: connect history archive %q: %w", url, err)
	}
	lakeTip, err := clickhouse.MaxLedger(ctx, addr)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("verify-network-state: lake tip: %w", err)
	}
	archTip, err := arch.GetLatestLedgerSequence()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("verify-network-state: archive tip: %w", err)
	}
	seq, err := pickHotArchiveCheckpoint(arch.GetCheckpointManager(), want, lakeTip, archTip)
	if err != nil {
		return nil, 0, 0, err
	}
	return arch, seq, lakeTip, nil
}

// runLiveStateCheck streams the checkpoint's whole live bucket list, sums native XLM per class and sets it against the lake's
// tally as of the same ledger, so a non-zero lumen residual is pinned to the class that carries it.
func runLiveStateCheck(ctx context.Context, reader *clickhouse.NetworkStateReader, addr, url, passphrase string, want uint64, sacID, dumpPath string) (*liveStateCounts, error) {
	arch, seq, lakeTip, err := openArchiveCheckpoint(ctx, addr, url, passphrase, want)
	if err != nil {
		return nil, err
	}
	sac, err := clickhouse.NativeSACID(sacID)
	if err != nil {
		return nil, err
	}
	hdr, err := arch.GetLedgerHeader(seq)
	if err != nil {
		return nil, fmt.Errorf("verify-network-state: archive header %d: %w", seq, err)
	}
	fmt.Fprintf(os.Stderr, "verify-network-state: live state at checkpoint %d from %s (lake tip %d)\n", seq, url, lakeTip)
	lake, err := reader.LumenConservationAt(ctx, sacID, seq)
	if err != nil {
		return nil, fmt.Errorf("verify-network-state: live state: lake tally: %w", err)
	}
	archive := clickhouse.NewLumenTally(seq, int64(hdr.Header.TotalCoins), int64(hdr.Header.FeePool))
	dump, closeDump, err := openLiveDump(dumpPath)
	if err != nil {
		return nil, err
	}
	n, err := tallyLiveState(ctx, func(ictx context.Context) iter.Seq2[xdr.LedgerEntry, error] {
		return liveEntries(ictx, arch, seq, sac)
	}, archive, sac, dump)
	if err != nil {
		_ = closeDump()
		return nil, err
	}
	if err := closeDump(); err != nil {
		return nil, fmt.Errorf("verify-network-state: livestate dump: %w", err)
	}
	return &liveStateCounts{checkpoint: seq, entries: n, archive: archive, lake: lake}, nil
}

// liveEntries yields the checkpoint bucket list.s live entries that can hold native XLM.
func liveEntries(ctx context.Context, arch historyarchive.ArchiveInterface, seq uint32, sac xdr.ContractId) iter.Seq2[xdr.LedgerEntry, error] {
	return func(yield func(xdr.LedgerEntry, error) bool) {
		entryWanted, keyWanted := liveStateFilters(sac)
		r, err := ingest.NewCheckpointChangeReader(ctx, arch, seq, ingest.WithFilter(entryWanted, keyWanted))
		if err != nil {
			yield(xdr.LedgerEntry{}, err)
			return
		}
		defer func() { _ = r.Close() }()
		for {
			ch, err := r.Read()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				yield(xdr.LedgerEntry{}, err)
				return
			}
			if ch.Post == nil {
				continue
			}
			if !yield(*ch.Post, nil) {
				return
			}
		}
	}
}

// liveStateFilters admit the entry types that can hold native XLM and, of contract data, only the native SAC.s rows, so the
// reader skips most of the bucket list and its seen-key set stays small. Both filters apply one rule, which keeps shadowing
// consistent between live and dead entries.
func liveStateFilters(sac xdr.ContractId) (func(xdr.LedgerEntry) bool, func(xdr.LedgerKey) bool) {
	wanted := func(t xdr.LedgerEntryType, contract *xdr.ScAddress) bool {
		switch t {
		case xdr.LedgerEntryTypeAccount, xdr.LedgerEntryTypeClaimableBalance, xdr.LedgerEntryTypeLiquidityPool:
			return true
		case xdr.LedgerEntryTypeContractData:
			return contract != nil && contract.Type == xdr.ScAddressTypeScAddressTypeContract &&
				contract.ContractId != nil && *contract.ContractId == sac
		}
		return false
	}
	entry := func(e xdr.LedgerEntry) bool {
		var c *xdr.ScAddress
		if e.Data.Type == xdr.LedgerEntryTypeContractData && e.Data.ContractData != nil {
			c = &e.Data.ContractData.Contract
		}
		return wanted(e.Data.Type, c)
	}
	key := func(k xdr.LedgerKey) bool {
		var c *xdr.ScAddress
		if k.Type == xdr.LedgerEntryTypeContractData && k.ContractData != nil {
			c = &k.ContractData.Contract
		}
		return wanted(k.Type, c)
	}
	return entry, key
}

// tallyLiveState folds every streamed entry into t, writes each to dump when one is given, and returns how many it read.
func tallyLiveState(ctx context.Context, open func(context.Context) iter.Seq2[xdr.LedgerEntry, error], t *clickhouse.LumenTally, sac xdr.ContractId, dump io.Writer) (uint64, error) {
	ictx, cancel := context.WithCancel(ctx)
	defer cancel()
	var n uint64
	for e, err := range open(ictx) {
		if err != nil {
			cancel()
			return n, fmt.Errorf("verify-network-state: read live state at %d: %w", t.Ledger, err)
		}
		if err := t.AddEntry(e, sac); err != nil {
			cancel()
			return n, err
		}
		if dump != nil {
			if err := writeLiveRow(dump, e, sac); err != nil {
				cancel()
				return n, err
			}
		}
		n++
		if n%5_000_000 == 0 {
			fmt.Fprintf(os.Stderr, "  ... %d live entries\n", n)
		}
	}
	return n, nil
}

// openLiveDump opens the per-entry dump, or returns a nil writer and a no-op close when no path was given.
func openLiveDump(path string) (io.Writer, func() error, error) {
	if path == "" {
		return nil, func() error { return nil }, nil
	}
	f, err := os.Create(path) //nolint:gosec // operator-supplied output path
	if err != nil {
		return nil, nil, fmt.Errorf("verify-network-state: livestate dump: %w", err)
	}
	w := bufio.NewWriterSize(f, 1<<20)
	return w, func() error { return errors.Join(w.Flush(), f.Close()) }, nil
}

// writeLiveRow appends one entry as entry_type, base64 ledger key, holder, native stroops and last-modified ledger, so the
// dump joins a ledger_entries_current export on key_xdr and names the rows whose balance or ledger differs.
func writeLiveRow(w io.Writer, e xdr.LedgerEntry, sac xdr.ContractId) error {
	key, err := e.LedgerKey()
	if err != nil {
		return fmt.Errorf("verify-network-state: livestate dump: ledger key: %w", err)
	}
	raw, err := key.MarshalBinary()
	if err != nil {
		return fmt.Errorf("verify-network-state: livestate dump: ledger key: %w", err)
	}
	class, amount, err := clickhouse.EntryLumens(e, sac)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n", class, base64.StdEncoding.EncodeToString(raw), liveHolder(e), amount, uint32(e.LastModifiedLedgerSeq))
	return err
}

// liveHolder names the entry's owner the way the explorer does: account and SAC holder as strkeys, balance and pool ids as hex.
func liveHolder(e xdr.LedgerEntry) string {
	switch e.Data.Type {
	case xdr.LedgerEntryTypeAccount:
		return e.Data.MustAccount().AccountId.Address()
	case xdr.LedgerEntryTypeClaimableBalance:
		if id := e.Data.MustClaimableBalance().BalanceId; id.V0 != nil {
			return hex.EncodeToString(id.V0[:])
		}
		return ""
	case xdr.LedgerEntryTypeLiquidityPool:
		id := e.Data.MustLiquidityPool().LiquidityPoolId
		return hex.EncodeToString(id[:])
	case xdr.LedgerEntryTypeContractData:
		if vec, ok := e.Data.MustContractData().Key.GetVec(); ok && vec != nil && len(*vec) == 2 {
			if a, ok := (*vec)[1].GetAddress(); ok {
				s, _ := a.String()
				return s
			}
		}
		return ""
	default:
		return ""
	}
}

// liveStateCounts holds both sides' tallies at one checkpoint.
type liveStateCounts struct {
	checkpoint uint32
	entries    uint64
	archive    *clickhouse.LumenTally // the checkpoint's bucket list and ledger header
	lake       *clickhouse.LumenTally // the lake as of the same ledger
}

type liveStateDiff struct {
	class         string
	archive, lake *big.Int
}

func (d liveStateDiff) delta() *big.Int { return new(big.Int).Sub(d.lake, d.archive) }

// diffs lists every class; the header fields are included so a lake header
// that disagrees with the archive's shows up beside the holdings.
func (n *liveStateCounts) diffs() []liveStateDiff {
	return []liveStateDiff{
		{"accounts", n.archive.Accounts, n.lake.Accounts},
		{"claimable_balances", n.archive.ClaimableBalances, n.lake.ClaimableBalances},
		{"liquidity_pools", n.archive.LiquidityPools, n.lake.LiquidityPools},
		{"contract_balances", n.archive.ContractBalances, n.lake.ContractBalances},
		{"fee_pool", big.NewInt(n.archive.FeePool), big.NewInt(n.lake.FeePool)},
		{"total_coins", big.NewInt(n.archive.TotalCoins), big.NewInt(n.lake.TotalCoins)},
	}
}

// failures counts the classes whose two sides differ, plus one for a stream
// that yielded nothing: a run that compared nothing must not read as clean.
func (n *liveStateCounts) failures() uint64 {
	var f uint64
	for _, d := range n.diffs() {
		if d.delta().Sign() != 0 {
			f++
		}
	}
	if n.entries == 0 {
		f++
	}
	return f
}

// pickHotArchiveCheckpoint returns want when both sides have reached it, or (want=0) the newest such checkpoint; one above the lake tip
// would compare against a lake that has not ingested its ledgers.
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
	key          string
	data         []byte // the archived entry's LedgerEntryData XDR
	lastModified uint32 // the archive's lastModifiedLedgerSeq stamp
}

// reconcileHotArchive compares each archived entry with the lake's row for its key, batching lookups per entry type. open builds the
// iterator on a context this function cancels before any early return.
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
	return archivedEntry{key: keyXDR, data: data, lastModified: uint32(e.LastModifiedLedgerSeq)}, entryType, nil
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
		n.mismatch(entryType, a, ours, "removed in the lake")
	default:
		var e xdr.LedgerEntry
		if err := xdr.SafeUnmarshalBase64(ours.EntryXDR, &e); err != nil {
			n.mismatch(entryType, a, ours, "lake entry undecodable")
			return
		}
		data, err := e.Data.MarshalBinary()
		if err != nil || !bytes.Equal(data, a.data) {
			n.mismatch(entryType, a, ours, "value differs")
			return
		}
		n.matched++
	}
}

// mismatch records one and samples it with both sides' ledger stamps, so a
// stale lake row can be placed against the ingest history without a re-run.
func (n *hotArchiveCounts) mismatch(entryType string, a archivedEntry, ours clickhouse.CurrentEntry, reason string) {
	n.mismatches++
	if len(n.sample) < hotArchiveMismatchSample {
		n.sample = append(n.sample, fmt.Sprintf("%s %s: %s (lake row at ledger %d, archive last modified %d)",
			entryType, a.key, reason, ours.LedgerSeq, a.lastModified))
	}
}

// networkStateResult holds whichever checks ran; nil = not requested.
type networkStateResult struct {
	hot    *hotArchiveCounts
	lumens *clickhouse.LumenTally
	live   *liveStateCounts
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
	if r.live != nil {
		total += r.live.failures()
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
	if l := r.live; l != nil {
		fmt.Fprintf(&w, "\n=== live state @ checkpoint %d (stroops; diff = lake - archive) ===\n", l.checkpoint)
		fmt.Fprintf(&w, "entries=%d archive_residual=%s lake_residual=%s\n", l.entries, l.archive.Residual(), l.lake.Residual())
		for _, d := range l.diffs() {
			fmt.Fprintf(&w, "%s archive=%s lake=%s diff=%s\n", d.class, d.archive, d.lake, d.delta())
		}
		if l.entries == 0 {
			fmt.Fprintln(&w, "  FAIL: the live bucket list yielded no entries")
		}
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
	b.WriteString("# HELP stellarindex_network_state_verify_failures Failures verify-network-state found per check (hot-archive mismatches; 1 for a non-zero lumen residual; live-state classes that differ from the lake). Emitted only for checks that ran.\n")
	b.WriteString("# TYPE stellarindex_network_state_verify_failures gauge\n")
	if r.hot != nil {
		fmt.Fprintf(&b, "stellarindex_network_state_verify_failures{check=%q} %d\n", "hot_archive", r.hot.failures())
	}
	if r.lumens != nil {
		fmt.Fprintf(&b, "stellarindex_network_state_verify_failures{check=%q} %d\n", "lumens", r.lumenFailures())
	}
	if r.live != nil {
		fmt.Fprintf(&b, "stellarindex_network_state_verify_failures{check=%q} %d\n", "live_state", r.live.failures())
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
	if l := r.live; l != nil {
		b.WriteString("# HELP stellarindex_network_state_verify_live_state_diff_stroops Lake minus archive native XLM per class at the checked checkpoint.\n")
		b.WriteString("# TYPE stellarindex_network_state_verify_live_state_diff_stroops gauge\n")
		for _, d := range l.diffs() {
			fmt.Fprintf(&b, "stellarindex_network_state_verify_live_state_diff_stroops{class=%q} %s\n", d.class, d.delta())
		}
	}
	b.WriteString("# HELP stellarindex_network_state_verify_last_run_unix When verify-network-state last completed every requested check.\n")
	b.WriteString("# TYPE stellarindex_network_state_verify_last_run_unix gauge\n")
	fmt.Fprintf(&b, "stellarindex_network_state_verify_last_run_unix %d\n", now.Unix())
	return b.String()
}
