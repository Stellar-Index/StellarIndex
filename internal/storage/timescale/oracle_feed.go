package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// OracleFeedWindow is one on-chain feed contract's rounds for one asset inside [From, To], as applied at or before
// MaxLedger: what a contract calling the feed at that ledger could read.
type OracleFeedWindow struct {
	// LastRound is the feed's newest round timestamp in the window across all its assets (raw rows included, since
	// a round is the feed's last_timestamp whatever it priced); zero when the window holds none.
	LastRound time.Time
	// Source is the oracle_updates source of the feed's rows in the window; "" when it holds none.
	Source string
	// Quote is the feed's stored quote (its SEP-40 base), verbatim; every asset row must carry the same.
	Quote canonical.Asset
	// Prices is the asset's raw integer per round timestamp (Unix ms).
	Prices map[int64]canonical.Amount
}

// ErrOracleFeedAmbiguous marks a feed window that cannot be read as one feed: two sources for one contract, or two
// different prices for the asset in one round.
var ErrOracleFeedAmbiguous = errors.New("timescale: oracle feed window ambiguous")

// OracleFeedRounds reads contract's rounds for the asset (under every stored identity in assetKeys) with ts in
// [from, to] and ledger <= maxLedger. Keyed on the contract id, never the source name; both queries are bounded by ts
// so they prune to the window's chunks, and the asset read probes (asset, ts DESC).
func (s *Store) OracleFeedRounds(ctx context.Context, contract string, assetKeys []string, from, to time.Time, maxLedger uint32) (OracleFeedWindow, error) {
	out := OracleFeedWindow{Prices: map[int64]canonical.Amount{}}
	const lastQ = `
        SELECT ts, source, quote
          FROM oracle_updates -- totality: a raw row still marks the feed's round
         WHERE contract_id = $1
           AND ts >= $2 AND ts <= $3
           AND ledger <= $4
         ORDER BY ts DESC
         LIMIT 1
    `
	var quote string
	err := s.db.QueryRowContext(ctx, lastQ, contract, from, to, int64(maxLedger)).Scan(&out.LastRound, &out.Source, &quote)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return OracleFeedWindow{}, fmt.Errorf("timescale: OracleFeedRounds last round: %w", err)
	}
	out.LastRound = out.LastRound.UTC()
	if out.Quote, err = canonical.ParseAsset(quote); err != nil {
		return OracleFeedWindow{}, fmt.Errorf("timescale: OracleFeedRounds quote %q: %w", quote, err)
	}
	if len(assetKeys) == 0 {
		return out, nil
	}
	const rowsQ = `
        SELECT ts, source, quote, price
          FROM oracle_updates
         WHERE asset = ANY($1)
           AND contract_id = $2
           AND ts >= $3 AND ts <= $4
           AND ledger <= $5
         ORDER BY ts DESC
    `
	rows, err := s.db.QueryContext(ctx, rowsQ, assetKeys, contract, from, to, int64(maxLedger))
	if err != nil {
		return OracleFeedWindow{}, fmt.Errorf("timescale: OracleFeedRounds rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			ts            time.Time
			source, quote string
			price         canonical.Amount
		)
		if err := rows.Scan(&ts, &source, &quote, &price); err != nil {
			return OracleFeedWindow{}, fmt.Errorf("timescale: OracleFeedRounds scan: %w", err)
		}
		if err := out.add(ts, source, quote, price); err != nil {
			return OracleFeedWindow{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return OracleFeedWindow{}, fmt.Errorf("timescale: OracleFeedRounds rows: %w", err)
	}
	return out, nil
}

func (w *OracleFeedWindow) add(ts time.Time, source, quote string, p canonical.Amount) error {
	if source != w.Source {
		return fmt.Errorf("%w: sources %q and %q", ErrOracleFeedAmbiguous, w.Source, source)
	}
	q, err := canonical.ParseAsset(quote)
	if err != nil {
		return fmt.Errorf("timescale: OracleFeedRounds quote %q: %w", quote, err)
	}
	if q != w.Quote {
		return fmt.Errorf("%w: quotes %s and %s", ErrOracleFeedAmbiguous, w.Quote, q)
	}
	ms := ts.UnixMilli()
	if prev, ok := w.Prices[ms]; ok && prev.Cmp(p) != 0 {
		return fmt.Errorf("%w: two prices at %s", ErrOracleFeedAmbiguous, ts.UTC())
	}
	w.Prices[ms] = p
	return nil
}
