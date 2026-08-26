package timescale

import (
	"context"
	"fmt"
	"time"
)

// SignerTag is one (ledger, tx_hash) → transaction-source-account mapping
// the signer sweeper back-fills into trades.signer. Signer is the tx
// fee-payer / initiator, sourced from the lake's stellar.transactions
// (the projector-replayed events carry no source account, so it cannot be
// set on the decode path — see migration 0150).
type SignerTag struct {
	Ledger uint32
	TxHash string
	Signer string
}

// AMMSignerSources are the AMM/Soroban trade sources whose `taker` is the
// on-chain caller (the event's to/sender/caller/user), NOT necessarily the
// human — so the tx source account is the missing initiator worth tagging.
// SDEX carries a real `maker`; classic trades have no contract tx to
// attribute. Exported so the indexer's sweeper gate reads the SAME list
// (no drift) — kept in lockstep with the decoders that leave `maker` empty.
var AMMSignerSources = []string{"comet", "soroswap", "aquarius", "phoenix"}

// TagTradesSigner back-fills trades.signer for the supplied
// (ledger, tx_hash) → source-account tags. FIRST-WINS: it only touches AMM
// rows whose signer IS NULL, so it is idempotent + re-runnable and never
// overwrites an already-tagged value. `signer` is DELIBERATELY absent from
// the trades INSERT / ON CONFLICT DO UPDATE (trades.go), so a value tagged
// here survives the projector's ~5s re-derive UPSERT — exactly the
// routed_via first-wins contract.
//
// Empty-string signers in the batch are ignored (WHERE s.signer <> ”), so
// a tx whose source could not be resolved leaves the row NULL for a later
// pass rather than pinning it to a blank.
func (s *Store) TagTradesSigner(ctx context.Context, tags []SignerTag) (int64, error) {
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
         WHERE t.ledger  = s.ledger
           AND t.tx_hash = s.tx_hash
           AND s.signer <> ''
           AND t.source  = ANY($4::text[])
           AND t.signer IS NULL
    `
	res, err := s.db.ExecContext(ctx, q, ledgers, txHashes, signers, AMMSignerSources)
	if err != nil {
		return 0, fmt.Errorf("timescale: TagTradesSigner (%d tags): %w", len(tags), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("timescale: TagTradesSigner rows-affected: %w", err)
	}
	return n, nil
}

// UntaggedAMMSignerLedgerRange returns the inclusive ledger span of AMM trades
// in [from, to) that still lack a signer, so the sweeper can scope its lake
// read to exactly those ledgers instead of every recent transaction. ok=false
// when nothing needs tagging (the sweeper then skips the lake read entirely).
//
// At steady state the sweep tags each window every tick, so only the last
// minute or two of trades are ever untagged and the span (and the lake read it
// bounds) is small; a projector lag or a restart widens it up to the lookback
// and self-heals on the next clean sweep. Bounded by the ts window + the AMM
// source filter — a min/max aggregate over recent trades chunks, not a scan.
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
