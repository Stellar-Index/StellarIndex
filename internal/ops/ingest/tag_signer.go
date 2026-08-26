package ingest

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// tagSigner is the HISTORICAL / recovery half of AMM signer attribution
// (migration 0150), the mirror of tag-routed-via. The live sweeper
// (internal/pipeline.RunSignerTagger) only covers a trailing 30-min window, so
// an indexer/ClickHouse outage or a projector lag longer than that leaves
// those trades' `signer` permanently NULL (a re-derive PRESERVES the column,
// it does not re-tag). This walks a ledger range in windows, reading each
// window's tx source accounts from the lake (stellar.transactions) and
// back-tagging trades.signer via the SAME first-wins timescale.TagTradesSigner
// primitive the live sweeper uses — so historical and live tagging cannot
// drift. Idempotent + resumable: already-tagged rows never match
// (signer IS NULL), and progress checkpoints into ingestion_cursors as
// (source='tag-signer', sub_source='signer') after each completed window.
func tagSigner(args []string) error { //nolint:funlen,gocognit,gocyclo // linear windowed pass: flags → bounds → per-window read+tag + checkpoint (mirrors tagRoutedVia)
	fs := flag.NewFlagSet("tag-signer", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	from := fs.Uint("from", 0, "First ledger (inclusive) — required (the gap start)")
	to := fs.Uint("to", 0, "Last ledger (inclusive) — required (the gap end)")
	window := fs.Uint("window", 5_000, "Ledgers per read+tag window (each window is one lake read + one UPDATE)")
	chAddr := fs.String("ch-addr", "", "ClickHouse native address (default: config clickhouse_addr)")
	resume := fs.Bool("resume", true, "Resume from the saved ingestion_cursors checkpoint")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	if *from == 0 || *to == 0 {
		return errors.New("-from and -to are required (the ledger gap to backfill)")
	}
	if *to < *from {
		return fmt.Errorf("-to (%d) must be >= -from (%d)", *to, *from)
	}
	if *window == 0 {
		return errors.New("-window must be > 0")
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

	addr := *chAddr
	if addr == "" {
		addr = cfg.Storage.ClickHouseAddr
	}
	if addr == "" {
		addr = "127.0.0.1:9300"
	}
	lake, err := clickhouse.NewExplorerReaderAuth(ctx, addr,
		cfg.Storage.ClickHouseServingUser, cfg.Storage.ClickHouseServingPassword)
	if err != nil {
		return fmt.Errorf("clickhouse open %s: %w", addr, err)
	}
	defer func() { _ = lake.Close() }()

	fromLedger, toLedger := uint32(*from), uint32(*to)

	const (
		cursorSrc = "tag-signer"
		cursorSub = "signer"
	)
	start := fromLedger
	if *resume {
		prior, gerr := store.GetCursor(ctx, cursorSrc, cursorSub)
		switch {
		case gerr == nil && prior.LastLedger >= fromLedger:
			start = prior.LastLedger + 1
			fmt.Fprintf(os.Stderr, "tag-signer: resuming at ledger %d (checkpoint last_ledger=%d)\n",
				start, prior.LastLedger)
		case gerr != nil && !errors.Is(gerr, timescale.ErrNotFound):
			fmt.Fprintf(os.Stderr, "tag-signer: read cursor failed (%v) — starting from -from\n", gerr)
		}
	}
	if start > toLedger {
		fmt.Fprintf(os.Stderr, "tag-signer: checkpoint already past -to (%d > %d) — nothing to do\n",
			start, toLedger)
		return nil
	}

	fmt.Fprintf(os.Stderr, "tag-signer: ledgers %d..%d, window %d\n", start, toLedger, *window)

	var totalTagged int64
	for lo := start; lo <= toLedger; {
		hi := lo + uint32(*window) - 1
		if hi > toLedger || hi < lo { // hi<lo guards uint32 overflow
			hi = toLedger
		}
		if ctx.Err() != nil {
			return fmt.Errorf("interrupted at window %d..%d (checkpoint saved through %d)", lo, hi, lo-1)
		}

		sigs, rerr := lake.TxSignersForLedgerRange(ctx, lo, hi)
		if rerr != nil {
			return fmt.Errorf("window %d..%d lake read: %w", lo, hi, rerr)
		}
		if len(sigs) > 0 {
			tags := make([]timescale.SignerTag, len(sigs))
			for i, s := range sigs {
				tags[i] = timescale.SignerTag{Ledger: s.Ledger, TxHash: s.TxHash, Signer: s.Signer}
			}
			tagged, terr := store.TagTradesSigner(ctx, tags)
			if terr != nil {
				return fmt.Errorf("window %d..%d tag: %w", lo, hi, terr)
			}
			totalTagged += tagged
			fmt.Fprintf(os.Stderr, "tag-signer: window %d..%d tagged %d trades (total %d)\n",
				lo, hi, tagged, totalTagged)
		}

		if cerr := store.UpsertCursor(ctx, cursorSrc, cursorSub, hi); cerr != nil {
			return fmt.Errorf("checkpoint at ledger %d: %w", hi, cerr)
		}
		if hi == toLedger {
			break
		}
		lo = hi + 1
	}

	fmt.Fprintf(os.Stderr, "tag-signer: done. %d trades tagged across ledgers %d..%d\n",
		totalTagged, start, toLedger)
	return nil
}
