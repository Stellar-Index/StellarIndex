// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package archive

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"os"

	"github.com/stellar/go-stellar-sdk/support/datastore"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// compareSampleCap bounds the differences printed; the count is always exact.
const compareSampleCap = 20

// ledgerSource streams the bounded range [from, to] from one export.
type ledgerSource func(ctx context.Context, from, to uint32, cb func(xdr.LedgerCloseMeta) error) error

// entryChangeDiff is one disagreement. Position is the intra_ledger_seq of
// the differing row, or -1 when the ledger as a whole disagrees.
type entryChangeDiff struct {
	Ledger   uint32 `json:"ledger"`
	Position int    `json:"intra_ledger_seq"`
	Detail   string `json:"detail"`
}

type entryChangeReport struct {
	From        uint32            `json:"from"`
	To          uint32            `json:"to"`
	Ledgers     int               `json:"ledgers_compared"`
	RowsOurs    int               `json:"rows_ours"`
	RowsAWS     int               `json:"rows_aws"`
	Differences int               `json:"differences"`
	Sample      []entryChangeDiff `json:"sample"`
}

func (r *entryChangeReport) add(ledger uint32, pos int, detail string) {
	r.Differences++
	if len(r.Sample) < compareSampleCap {
		r.Sample = append(r.Sample, entryChangeDiff{Ledger: ledger, Position: pos, Detail: detail})
	}
}

// compareEntryChanges is `compare-entry-changes`: extract the
// stellar.ledger_entry_changes rows for [-from, -to] from our galexie export
// and from the cold tier (the AWS public export) and report every row that
// differs. Read-only; exits non-zero when any row differs.
func compareEntryChanges(args []string) error {
	fs := flag.NewFlagSet("compare-entry-changes", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/stellarindex.toml", "Path to stellarindex.toml")
	from := fs.Uint("from", 0, "First ledger to compare (inclusive)")
	to := fs.Uint("to", 0, "Last ledger to compare (inclusive)")
	bucket := fs.String("bucket", "", "Our export's bucket (default storage.s3_bucket_archive)")
	window := fs.Uint("window", 100, "Ledgers held in memory per export at once")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == 0 || *to < *from || *to > uint(^uint32(0)) || *window == 0 {
		return fmt.Errorf("invalid range: -from %d -to %d -window %d (from >= 1, from <= to, window >= 1)", *from, *to, *window)
	}
	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !cfg.Storage.ColdTieringEnabled() {
		return fmt.Errorf("cold tier not configured — set storage.s3_cold_bucket_archive in %s", *cfgPath)
	}
	ours, aws := exportSources(cfg, *bucket)

	ctx, cancel := opsutil.SignalContext()
	defer cancel()
	rep, err := compareEntryChangeRange(ctx, ours, aws, cfg.Stellar.Passphrase(), uint32(*from), uint32(*to), uint32(*window)) //nolint:gosec // both bounded to uint32 above.
	if err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(rep); err != nil {
		return err
	}
	if rep.Differences > 0 {
		return fmt.Errorf("%d entry-change differences between the two exports", rep.Differences)
	}
	return nil
}

// exportSources opens our export through the SDK client and the cold tier
// through pipeline.NewColdDataStore, which must not inherit the ambient
// (MinIO) AWS credentials.
func exportSources(cfg config.Config, bucket string) (ours, aws ledgerSource) {
	if bucket == "" {
		bucket = cfg.Storage.S3BucketArchive
	}
	oursCfg := opsutil.NewBoundedLedgerStreamConfig(cfg, bucket, 1)
	awsCfg := ledgerstream.Config{
		DataStore: pipeline.LedgerstreamConfig(cfg, cfg.Storage.S3BucketArchive).ColdDataStore,
		Buffered:  oursCfg.Buffered,
	}
	ours = func(ctx context.Context, from, to uint32, cb func(xdr.LedgerCloseMeta) error) error {
		store, err := datastore.NewDataStore(ctx, oursCfg.DataStore)
		if err != nil {
			return fmt.Errorf("our datastore: %w", err)
		}
		return ledgerstream.StreamStore(ctx, oursCfg, store, from, to, cb)
	}
	aws = func(ctx context.Context, from, to uint32, cb func(xdr.LedgerCloseMeta) error) error {
		store, err := pipeline.NewColdDataStore(ctx, cfg.Storage)
		if err != nil {
			return fmt.Errorf("cold datastore: %w", err)
		}
		return ledgerstream.StreamStore(ctx, awsCfg, store, from, to, cb)
	}
	return ours, aws
}

// compareEntryChangeRange walks [from, to] in windows so memory stays bounded
// by the window, not the range: per window it digests every row of each
// export, then compares position by position.
func compareEntryChangeRange(ctx context.Context, ours, aws ledgerSource, passphrase string, from, to, window uint32) (entryChangeReport, error) {
	rep := entryChangeReport{From: from, To: to, Sample: []entryChangeDiff{}}
	for lo := uint64(from); lo <= uint64(to); lo += uint64(window) {
		hi := min(lo+uint64(window)-1, uint64(to))
		a, err := digestEntryChanges(ctx, ours, passphrase, uint32(lo), uint32(hi)) //nolint:gosec // lo, hi <= to, a uint32.
		if err != nil {
			return rep, fmt.Errorf("our export: %w", err)
		}
		b, err := digestEntryChanges(ctx, aws, passphrase, uint32(lo), uint32(hi)) //nolint:gosec // as above.
		if err != nil {
			return rep, fmt.Errorf("AWS export: %w", err)
		}
		for seq := lo; seq <= hi; seq++ {
			rep.compareLedger(uint32(seq), a, b) //nolint:gosec // seq <= hi.
		}
	}
	return rep, nil
}

func (r *entryChangeReport) compareLedger(seq uint32, ours, aws map[uint32][]uint64) {
	o, oOK := ours[seq]
	a, aOK := aws[seq]
	switch {
	case !oOK:
		r.add(seq, -1, "missing from our export")
		return
	case !aOK:
		r.add(seq, -1, "missing from the AWS export")
		return
	}
	r.Ledgers++
	r.RowsOurs += len(o)
	r.RowsAWS += len(a)
	if len(o) != len(a) {
		r.add(seq, -1, fmt.Sprintf("row count %d (ours) vs %d (AWS)", len(o), len(a)))
	}
	for i := range min(len(o), len(a)) {
		if o[i] != a[i] {
			r.add(seq, i, "row differs")
		}
	}
}

// digestEntryChanges extracts each ledger exactly as the lake writer does
// and keeps one digest per row, indexed by intra_ledger_seq.
func digestEntryChanges(ctx context.Context, src ledgerSource, passphrase string, from, to uint32) (map[uint32][]uint64, error) {
	out := make(map[uint32][]uint64, to-from+1)
	err := src(ctx, from, to, func(lcm xdr.LedgerCloseMeta) error {
		ext, err := clickhouse.ExtractLedger(lcm, passphrase)
		if err != nil {
			return err
		}
		d := make([]uint64, len(ext.Changes))
		for i := range ext.Changes {
			d[i] = entryChangeDigest(&ext.Changes[i])
		}
		out[lcm.LedgerSequence()] = d
		return nil
	})
	return out, err
}

func entryChangeDigest(r *clickhouse.LedgerEntryChangeRow) uint64 {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d\x00%d\x00%s\x00%d\x00%d\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d",
		r.LedgerSeq, r.CloseTime.UnixNano(), r.TxHash, r.OpIndex, r.ChangeIndex, r.IntraLedgerSeq,
		r.ChangeType, r.EntryType, r.KeyXDR, r.EntryXDR, r.AccountID, r.Asset, r.Balance)
	return h.Sum64()
}
