package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/stellar/go-stellar-sdk/historyarchive"
	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/support/storage"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

const (
	defaultPubnetArchive    = "https://history.stellar.org/prd/core-live/core_live_001"
	defaultPubnetPassphrase = "Public Global Stellar Network ; September 2015"
)

// snapTally is the per-entry-type rollup of a checkpoint state read.
type snapTally struct {
	byType        map[xdr.LedgerEntryType]uint64
	total         uint64
	contractCode  uint64
	wasmInstances uint64
	sacInstances  uint64
	elapsed       time.Duration
	partial       bool

	// collect accumulates rows for InsertEntryChanges; scope widens the set (contracts, all, storage) and closeTime is metadata only.
	// A non-zero maxModLedger restricts collection to entries last modified BELOW it, the dormant tail live capture never saw.
	collect      bool
	scope        snapScope
	maxModLedger uint32
	closeTime    time.Time
	rows         []clickhouse.LedgerEntryChangeRow
}

// snapScope is the collection decision derived from the -scope flag. The zero
// value is the contract scope (contract_code + contract_data instances).
type snapScope struct {
	all     bool // + account/trustline/offer/data/claimable/liquidity_pool
	storage bool // dormant current-state fill: contract_data STORAGE + liquidity_pool
}

// parseSnapScope maps the -scope flag to a snapScope, rejecting unknown values
// (a silent fall-through to the contract scope has masked typos before).
func parseSnapScope(s string) (snapScope, error) {
	switch s {
	case "contracts":
		return snapScope{}, nil
	case "all":
		return snapScope{all: true}, nil
	case "storage":
		return snapScope{storage: true}, nil
	default:
		return snapScope{}, fmt.Errorf("unknown -scope %q (want contracts|all|storage)", s)
	}
}

// stateSnapshot reads a history-archive checkpoint's full current ledger-entry state and tallies it by entry type; the served projection only holds entries changed since ledger ~62M.
// Read-only unless -write, which inserts into ClickHouse ledger_entry_changes. -limit bounds the read, and -write refuses a -limit-truncated read (see writeSnapshot).
func stateSnapshot(args []string) error {
	o, err := parseStateSnapshotFlags(args)
	if err != nil {
		return err
	}
	url, passphrase, err := resolveArchiveTarget(o.cfgPath, o.archiveURL, o.write)
	if err != nil {
		return err
	}
	ctx, cancel := opsutil.SignalContext()
	defer cancel()
	arch, err := historyarchive.Connect(url, historyarchive.ArchiveOptions{
		NetworkPassphrase: passphrase,
		ConnectOptions:    storage.ConnectOptions{Context: ctx, UserAgent: "stellarindex-ops/state-snapshot"},
	})
	if err != nil {
		return fmt.Errorf("connect history archive %q: %w", url, err)
	}

	seq, err := resolveCheckpoint(arch, o.checkpoint)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "state-snapshot: reading checkpoint %d from %s (limit=%d, write=%v, dry-run=%v, scope=%s, max-modified-ledger=%d)\n",
		seq, url, o.limit, o.write, o.collect && !o.write, o.scopeName, o.maxModLedger)

	t, err := tallyCheckpoint(ctx, arch, seq, o.limit, o.collect, o.scope, o.maxModLedger)
	if err != nil {
		return err
	}
	printTally(seq, t)
	if o.collect {
		printWriteSet(t)
	}

	if o.write {
		return writeSnapshot(ctx, clickhouse.InsertEntryChanges, t, o.limit, o.scopeName, o.chAddr, o.throttle)
	}
	if o.collect {
		fmt.Printf("\n─── DRY RUN ─── %d entries would be written (scope=%s); nothing written.\n", len(t.rows), o.scopeName)
	}
	return nil
}

type stateSnapshotOpts struct {
	cfgPath, archiveURL, scopeName, chAddr string
	checkpoint, maxModLedger               uint32
	limit                                  uint64
	scope                                  snapScope
	throttle                               time.Duration
	// write inserts the collected set; collect gathers and prints the write
	// set. A bare run does neither: it is the bounded read-only tally.
	write, collect bool
}

func parseStateSnapshotFlags(args []string) (stateSnapshotOpts, error) {
	fs, gate := opsutil.NewMutatingFlagSet("state-snapshot")
	cfgPath := fs.String("config", "/etc/stellarindex.toml", "config path (optional for a public-archive read)")
	archiveURL := fs.String("archive", "", "history archive URL (default: cfg.Stellar.HistoryArchiveURL)")
	checkpoint := fs.Uint("checkpoint", 0, "checkpoint ledger (default: latest checkpoint)")
	limit := fs.Uint64("limit", 2_000_000, "max entries to read (0 = full snapshot)")
	scope := fs.String("scope", "contracts", "write scope: 'contracts' (G1: code+instances), 'all' (G2/G3: +account/trustline/offer/data/claimable/LP), or 'storage' (contract_data STORAGE + LP — the dormant SAC/Blend/LP current-state fill)")
	maxModLedger := fs.Uint("max-modified-ledger", 0, "collect only entries last modified BELOW this ledger (0 = all); use ~62000000 to write only the dormant tail the live-capture floor never captured")
	chAddr := fs.String("ch", "127.0.0.1:9300", "ClickHouse native address for -write (r1 native port is 9300; 9000 is MinIO)")
	throttleMS := fs.Uint("throttle-ms", 50, "pause between insert chunks (keeps the live CH gentle)")
	if err := fs.Parse(args); err != nil {
		return stateSnapshotOpts{}, err
	}
	sc, err := parseSnapScope(*scope)
	if err != nil {
		return stateSnapshotOpts{}, err
	}
	return stateSnapshotOpts{
		cfgPath: *cfgPath, archiveURL: *archiveURL, scopeName: *scope, chAddr: *chAddr,
		checkpoint:   uint32(*checkpoint),   //nolint:gosec // operator-supplied ledger
		maxModLedger: uint32(*maxModLedger), //nolint:gosec // operator-supplied ledger
		limit:        *limit, scope: sc,
		throttle: time.Duration(*throttleMS) * time.Millisecond,
		// An explicit -dry-run sizes the write set, which a bare run does not.
		write: gate.Enabled(), collect: gate.Enabled() || gate.DryRunStated(),
	}, nil
}

// entryChangeInserter is clickhouse.InsertEntryChanges' shape, split out so
// writeSnapshot's refusal is testable without ClickHouse.
type entryChangeInserter func(ctx context.Context, addr string, rows []clickhouse.LedgerEntryChangeRow, throttle time.Duration) (int, error)

// writeSnapshot inserts the collected write set. A partial read is refused: -limit stops the bucket-list walk at an arbitrary prefix,
// and writing it would publish a fraction of the checkpoint (2M of ~48M entries at the default) as the whole.
func writeSnapshot(ctx context.Context, insert entryChangeInserter, t *snapTally, limit uint64, scope, chAddr string, throttle time.Duration) error {
	if t.partial {
		return fmt.Errorf("refusing -write: -limit %d stopped the checkpoint read after %d entries, so the %d collected rows are a bucket-list-order prefix, not the snapshot — re-run with -limit 0 (-dry-run sizes a bounded read without writing)",
			limit, t.total, len(t.rows))
	}
	fmt.Fprintf(os.Stderr, "state-snapshot: writing %d entries (scope=%s) → %s ledger_entry_changes ...\n",
		len(t.rows), scope, chAddr)
	n, werr := insert(ctx, chAddr, t.rows, throttle)
	if werr != nil {
		return fmt.Errorf("write entries (wrote %d of %d): %w", n, len(t.rows), werr)
	}
	fmt.Printf("\n✅ wrote %d entries into ledger_entry_changes (scope=%s).\n", n, scope)
	fmt.Printf("   The entry readers + ledger_entries_current MV pick these up.\n")
	return nil
}

// printWriteSet reports the total plus a per-entry-type breakdown, so an operator can size a fill and confirm -max-modified-ledger
// targeted the dormant tail.
func printWriteSet(t *snapTally) {
	byType := make(map[string]int, 8)
	for i := range t.rows {
		byType[t.rows[i].EntryType]++
	}
	types := make([]string, 0, len(byType))
	for typ := range byType {
		types = append(types, typ)
	}
	sort.Strings(types)
	fmt.Printf("\n--- write set: %d entries ---\n", len(t.rows))
	for _, typ := range types {
		fmt.Printf("  %-24s %d\n", typ, byType[typ])
	}
}

// resolveArchiveTarget picks the archive URL and passphrase from config, falling back to the public pubnet archive for reads.
// A write never guesses: nothing checks the archive network matches the target ClickHouse's, so an unresolved network aborts the run.
func resolveArchiveTarget(cfgPath, override string, write bool) (url, passphrase string, err error) {
	url, passphrase = override, defaultPubnetPassphrase
	cfg, cfgErr := config.LoadWithEnv(cfgPath)
	switch {
	case cfgErr == nil:
		if url == "" {
			url = cfg.Stellar.HistoryArchiveURL
		}
		if p := cfg.Stellar.Passphrase(); p != "" {
			passphrase = p
		}
	case write:
		return "", "", fmt.Errorf("state-snapshot -write requires a resolvable -config (network passphrase + archive URL) to avoid writing the wrong network's checkpoint into ClickHouse: %w", cfgErr)
	default:
		fmt.Fprintf(os.Stderr, "state-snapshot: config load failed (%v) — using public-archive defaults\n", cfgErr)
	}
	if url == "" {
		if write {
			return "", "", errors.New("state-snapshot -write requires an archive URL (config Stellar.HistoryArchiveURL or -archive); refusing to default to the public archive for a write")
		}
		url = defaultPubnetArchive
	}
	return url, passphrase, nil
}

// resolveCheckpoint returns the requested checkpoint ledger, or the latest
// checkpoint when 0.
func resolveCheckpoint(arch *historyarchive.Archive, want uint32) (uint32, error) {
	if want != 0 {
		return want, nil
	}
	latest, err := arch.GetLatestLedgerSequence()
	if err != nil {
		return 0, fmt.Errorf("latest ledger: %w", err)
	}
	return arch.GetCheckpointManager().PrevCheckpoint(latest), nil
}

// tallyCheckpoint streams the checkpoint's bucket list and rolls it up by entry
// type (read-only). limit>0 stops early for a bounded proof.
func tallyCheckpoint(ctx context.Context, arch *historyarchive.Archive, seq uint32, limit uint64, collect bool, scope snapScope, maxModLedger uint32) (*snapTally, error) {
	reader, err := ingest.NewCheckpointChangeReader(ctx, arch, seq)
	if err != nil {
		return nil, fmt.Errorf("checkpoint change reader @ %d: %w", seq, err)
	}
	defer func() { _ = reader.Close() }()

	t := &snapTally{byType: map[xdr.LedgerEntryType]uint64{}, collect: collect, scope: scope, maxModLedger: maxModLedger, closeTime: time.Now().UTC()}
	start := time.Now()
	for {
		ch, rerr := reader.Read()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("read entry %d: %w", t.total, rerr)
		}
		if ch.Post == nil { // a snapshot is all live entries → Post-only
			continue
		}
		t.observe(ch.Type, ch.Post)
		if t.total%1_000_000 == 0 {
			fmt.Fprintf(os.Stderr, "  ... %d entries (%s)\n", t.total, time.Since(start).Round(time.Second))
		}
		if limit > 0 && t.total >= limit {
			t.partial = true
			fmt.Fprintf(os.Stderr, "  (limit reached — partial tally)\n")
			break
		}
	}
	t.elapsed = time.Since(start)
	return t, nil
}

// observe folds one live entry into the tally, classifying contract instances as WASM vs SAC (SACs have no WASM; WASM ones point at a
// contract_code blob).
func (t *snapTally) observe(typ xdr.LedgerEntryType, post *xdr.LedgerEntry) {
	t.byType[typ]++
	t.total++
	isInstance := false
	switch typ {
	case xdr.LedgerEntryTypeContractCode:
		t.contractCode++
	case xdr.LedgerEntryTypeContractData:
		if cd, ok := post.Data.GetContractData(); ok && cd.Key.Type == xdr.ScValTypeScvLedgerKeyContractInstance {
			isInstance = true
			if inst, iok := cd.Val.GetInstance(); iok {
				switch inst.Executable.Type {
				case xdr.ContractExecutableTypeContractExecutableStellarAsset:
					t.sacInstances++
				case xdr.ContractExecutableTypeContractExecutableWasm:
					t.wasmInstances++
				}
			}
		}
	}
	if t.collect && t.withinModWindow(uint32(post.LastModifiedLedgerSeq)) && t.shouldCollect(typ, isInstance) { //nolint:gosec // ledger seq fits uint32
		if row, ok := clickhouse.SnapshotEntryRow(post, t.closeTime); ok {
			t.rows = append(t.rows, row)
		}
	}
}

// withinModWindow reports whether an entry last modified at ledgerSeq is in the collection window (0 = everything).
// Rewriting an entry is idempotent (ReplacingMergeTree), so the bound is a cost control, not a correctness gate.
func (t *snapTally) withinModWindow(ledgerSeq uint32) bool {
	return t.maxModLedger == 0 || ledgerSeq < t.maxModLedger
}

// shouldCollect decides whether an entry type is in the write scope: contract_code and instances always; account-state/supply types for scope=all;
// contract_data STORAGE for scope=storage (ledger_entries_current only projects changes after the ~62M floor); LP for all and storage (ADR-0039). ttl and config_setting are never written.
func (t *snapTally) shouldCollect(typ xdr.LedgerEntryType, isInstance bool) bool {
	switch typ {
	case xdr.LedgerEntryTypeContractCode:
		return true
	case xdr.LedgerEntryTypeContractData:
		return isInstance || t.scope.storage
	case xdr.LedgerEntryTypeLiquidityPool:
		return t.scope.all || t.scope.storage
	case xdr.LedgerEntryTypeAccount, xdr.LedgerEntryTypeTrustline,
		xdr.LedgerEntryTypeOffer, xdr.LedgerEntryTypeData,
		xdr.LedgerEntryTypeClaimableBalance:
		return t.scope.all
	default:
		return false
	}
}

func printTally(seq uint32, t *snapTally) {
	note := " (full snapshot)"
	if t.partial {
		note = " (PARTIAL — limit hit; pass -limit 0 for the full snapshot)"
	}
	fmt.Printf("\n=== checkpoint %d state tally — %d entries in %s%s ===\n",
		seq, t.total, t.elapsed.Round(time.Second), note)
	for typ, c := range t.byType {
		fmt.Printf("  %-24s %d\n", typ.String(), c)
	}
	fmt.Printf("\ncontract_code blobs:      %d   (ledger_entries_current has ~257)\n", t.contractCode)
	fmt.Printf("contract WASM instances:  %d\n", t.wasmInstances)
	fmt.Printf("contract SAC instances:   %d\n", t.sacInstances)
	fmt.Printf("\nNext (DATA-TRUTH-PLAN G1–G3): re-run with a writer to stage these into\n")
	fmt.Printf("a shadow table, reconcile vs ledger_entries_current, merge.\n")
}
