package timescale

import (
	"context"
	"fmt"
	"time"
)

// SignerTag maps one (ledger, tx_hash) to its transaction source account, back-filled into
// trades.signer by the sweeper. Signer is the tx fee-payer/initiator from the lake's
// stellar.transactions; decoded events carry no source account, so the decode path cannot set it.
type SignerTag struct {
	Ledger uint32
	TxHash string
	Signer string
}

// AMMSignerSources are the AMM/Soroban trade sources whose `taker` is the on-chain caller, not
// necessarily the human, so the tx source account is the initiator worth tagging. SDEX carries a
// real `maker`. Exported so the indexer's sweeper gate reads the same list; keep it in lockstep
// with the decoders that leave `maker` empty.
var AMMSignerSources = []string{"comet", "soroswap", "aquarius", "phoenix", "sushiswap_v3"}

// TagTradesSigner back-fills trades.signer for the supplied (ledger, tx_hash) tags. FIRST-WINS:
// it only touches AMM rows whose signer IS NULL, so it is idempotent and never overwrites a tag.
// `signer` is deliberately absent from the trades INSERT / ON CONFLICT DO UPDATE (trades.go), so a
// tagged value survives the projector's re-derive upsert, as with routed_via.
//
// Empty-string signers are skipped (the WHERE filter on s.signer) so an unresolved tx leaves the row
// NULL for a later pass rather than pinning a blank.
//
// [from, to) is the trades ts bound covering the tags and is REQUIRED: without it the UPDATE
// scans every chunk, including compressed ones, and trips the per-DML tuple-decompression limit
// (SQLSTATE 53400). Callers pass the close-time span of the tagged txs.
func (s *Store) TagTradesSigner(ctx context.Context, from, to time.Time, tags []SignerTag) (int64, error) {
	if len(tags) == 0 {
		return 0, nil
	}
	ledgers := make([]int64, len(tags))
	txHashes := make([]string, len(tags))
	signers := make([]string, len(tags))
	for i, t := range tags {
		ledgers[i] = int64(t.Ledger)
		txHashes[i] = t.TxHash
		signers[i] = t.Signer
	}
	const q = `
        UPDATE trades t
           SET signer = s.signer
          FROM (
              SELECT unnest($1::bigint[]) AS ledger,
                     unnest($2::text[])   AS tx_hash,
                     unnest($3::text[])   AS signer
          ) s
         WHERE t.ts     >= $5
           AND t.ts     <  $6
           AND t.ledger  = s.ledger
           AND t.tx_hash = s.tx_hash
           AND s.signer <> ''
           AND t.source  = ANY($4::text[])
           AND t.signer IS NULL
    `
	res, err := s.db.ExecContext(ctx, q, ledgers, txHashes, signers, AMMSignerSources, from.UTC(), to.UTC())
	if err != nil {
		return 0, fmt.Errorf("timescale: TagTradesSigner (%d tags): %w", len(tags), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("timescale: TagTradesSigner rows-affected: %w", err)
	}
	return n, nil
}

// UntaggedAMMSignerLedgerRange returns the inclusive ledger span of AMM trades in [from, to) that
// still lack a signer, so the sweeper scopes its lake read to those ledgers; ok=false means
// nothing needs tagging and the lake read is skipped.
//
// At steady state only the last minute or two is untagged, so the span is small; projector lag or
// a restart widens it up to the lookback and self-heals on the next sweep. The ts window plus the
// AMM source filter make this a min/max over recent chunks, not a scan.
func (s *Store) UntaggedAMMSignerLedgerRange(ctx context.Context, from, to time.Time) (minLedger, maxLedger uint32, ok bool, err error) {
	const q = `
        SELECT min(ledger), max(ledger)
          FROM trades
         WHERE ts >= $1 AND ts < $2
           AND source = ANY($3::text[])
           AND signer IS NULL
    `
	var lo, hi *int64
	if serr := s.db.QueryRowContext(ctx, q, from.UTC(), to.UTC(), AMMSignerSources).Scan(&lo, &hi); serr != nil {
		return 0, 0, false, fmt.Errorf("timescale: UntaggedAMMSignerLedgerRange: %w", serr)
	}
	if lo == nil || hi == nil {
		return 0, 0, false, nil
	}
	return uint32(*lo), uint32(*hi), true, nil
}
