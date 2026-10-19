package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// tagSigner is the historical/recovery half of AMM signer attribution: the live sweeper covers a trailing 30-min window and a re-derive
// preserves a NULL signer. It back-tags trades.signer from stellar.transactions via first-wins timescale.TagTradesSigner; resumable via
// ingestion_cursors (source='tag-signer'), -resume only for the same -from/-to. Fail-closed (opsutil.WriteGate): dry run unless -write.
func tagSigner(args []string) error { //nolint:funlen,gocognit,gocyclo // linear windowed pass: flags → bounds → per-window read+tag + checkpoint (mirrors tagRoutedVia)
	fs, gate := opsutil.NewMutatingFlagSet("tag-signer")
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	from := fs.Uint("from", 0, "First ledger (inclusive) — required (the gap start)")
	to := fs.Uint("to", 0, "Last ledger (inclusive) — required (the gap end)")
	window := fs.Uint("window", 5_000, "Ledgers per read+tag window (each window is one lake read + one UPDATE)")
	chAddr := fs.String("ch-addr", "", "ClickHouse native address (default: config clickhouse_addr)")
	resume := fs.Bool("resume", true, "Resume from the ingestion_cursors checkpoint of a prior run with the same -from/-to")
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
	write := gate.Banner()

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

	const cursorSrc = "tag-signer"
	start, cursorSub := resumeRangeStart(ctx, store, cursorSrc, fromLedger, toLedger, *resume)
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
			tsFrom, tsTo := sigs[0].CloseTime, sigs[0].CloseTime
			for i, s := range sigs {
				tags[i] = timescale.SignerTag{Ledger: s.Ledger, TxHash: s.TxHash, Signer: s.Signer}
				if s.CloseTime.Before(tsFrom) {
					tsFrom = s.CloseTime
				}
				if s.CloseTime.After(tsTo) {
					tsTo = s.CloseTime
				}
			}
			// ts bound = the window's chunk span so the UPDATE prunes to it
			// (and decompresses only that window's compressed segments, the
			// same reason tag-routed-via runs windowed).
			tagged, terr := tagSignerWindow(ctx, store, write, tsFrom, tsTo.Add(time.Second), tags)
			if terr != nil {
				return fmt.Errorf("window %d..%d tag: %w", lo, hi, terr)
			}
			totalTagged += tagged
			fmt.Fprintf(os.Stderr, "tag-signer: window %d..%d %s %d trades (total %d)\n",
				lo, hi, opsutil.WriteModeVerb(write, "tagged", "WOULD tag (upper bound)"), tagged, totalTagged)
		}

		// A preview must not advance the resume checkpoint: the next run
		// would skip the windows it only LOOKED at.
		if write {
			if cerr := store.UpsertCursor(ctx, cursorSrc, cursorSub, hi); cerr != nil {
				return fmt.Errorf("checkpoint at ledger %d: %w", hi, cerr)
			}
		}
		if hi == toLedger {
			break
		}
		lo = hi + 1
	}

	fmt.Fprintf(os.Stderr, "tag-signer: done. %d trades %s across ledgers %d..%d\n",
		totalTagged, opsutil.WriteModeVerb(write, "tagged", "WOULD tag (upper bound)"), start, toLedger)
	return nil
}

// rangeCursorReader is the slice of the store resumeRangeStart needs.
type rangeCursorReader interface {
	GetCursor(ctx context.Context, source, sub string) (timescale.Cursor, error)
}

// resumeRangeStart returns the first ledger a windowed [from, to] pass processes and its checkpoint key; the key carries the range so a
// later repair never skips an earlier one.
func resumeRangeStart(ctx context.Context, store rangeCursorReader, src string, from, to uint32, resume bool) (uint32, string) {
	sub := opsutil.RangeCursorKey(from, to)
	if !resume {
		return from, sub
	}
	prior, err := store.GetCursor(ctx, src, sub)
	switch {
	case err == nil && prior.LastLedger >= from:
		fmt.Fprintf(os.Stderr, "%s: resuming at ledger %d (checkpoint last_ledger=%d)\n",
			src, prior.LastLedger+1, prior.LastLedger)
		return prior.LastLedger + 1, sub
	case err != nil && !errors.Is(err, timescale.ErrNotFound):
		fmt.Fprintf(os.Stderr, "%s: read cursor failed (%v) — starting from -from\n", src, err)
	}
	return from, sub
}

// tagSignerWindow applies one window's signer tags, or in the default preview reports how many it WOULD apply.
// The preview is an UPPER BOUND (tx signers the lake returned; TagTradesSigner is first-wins over signer IS NULL); an exact count needs a second query over the compressed chunks the windowing avoids decompressing.
func tagSignerWindow(ctx context.Context, store *timescale.Store, write bool,
	tsFrom, tsTo time.Time, tags []timescale.SignerTag,
) (int64, error) {
	if !write {
		return int64(len(tags)), nil
	}
	return store.TagTradesSigner(ctx, tsFrom, tsTo, tags)
}
