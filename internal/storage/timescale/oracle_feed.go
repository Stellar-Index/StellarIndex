package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// OracleFeedWindow is one on-chain feed contract's stored rounds, as applied at or before a ledger: what a contract
// calling the feed at that ledger could read, as far as our table holds it.
type OracleFeedWindow struct {
	// LastRound is the feed's newest stored round in [lastFrom, to] across all its assets (raw rows included, since a
	// round is the feed's last_timestamp whatever it priced); zero when that range holds none.
	LastRound time.Time
	// Source is the oracle_updates source of the feed's rows; "" when none was read.
	Source string
	// Quote is the feed's stored quote (its SEP-40 base), verbatim; every row must carry the same.
	Quote canonical.Asset
	// Prices is the asset's raw integer per round timestamp (Unix ms) in [from, to].
	Prices map[int64]canonical.Amount
}

// ErrOracleFeedAmbiguous marks a feed window that cannot be read as one feed: two sources for one contract, or two
// different prices for the asset in one round.
var ErrOracleFeedAmbiguous = errors.New("timescale: oracle feed window ambiguous")

// OracleFeedRounds reads contract's newest round in [lastFrom, to] and the asset's prices (under every stored identity
// in assetKeys) in [from, to], both at ledger <= maxLedger. Keyed on the contract id, never the source name; both
// queries are bounded by ts so they prune to the window's chunks, and the asset read probes (asset, ts DESC).
func (s *Store) OracleFeedRounds(ctx context.Context, contract string, assetKeys []string, lastFrom, from, to time.Time, maxLedger uint32) (OracleFeedWindow, error) {
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
	var (
		last          time.Time
		source, quote string
	)
	err := s.db.QueryRowContext(ctx, lastQ, contract, lastFrom, to, int64(maxLedger)).Scan(&last, &source, &quote)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return OracleFeedWindow{}, fmt.Errorf("timescale: OracleFeedRounds last round: %w", err)
	default:
		if err := out.adopt(source, quote); err != nil {
			return OracleFeedWindow{}, err
		}
		out.LastRound = last.UTC()
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
			ts    time.Time
			price canonical.Amount
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

// adopt takes the first row's source and quote and refuses any other.
func (w *OracleFeedWindow) adopt(source, quote string) error {
	q, err := canonical.ParseAsset(quote)
	if err != nil {
		return fmt.Errorf("timescale: OracleFeedRounds quote %q: %w", quote, err)
	}
	if w.Source == "" {
		w.Source, w.Quote = source, q
		return nil
	}
	if source != w.Source {
		return fmt.Errorf("%w: sources %q and %q", ErrOracleFeedAmbiguous, w.Source, source)
	}
	if q != w.Quote {
		return fmt.Errorf("%w: quotes %s and %s", ErrOracleFeedAmbiguous, w.Quote, q)
	}
	return nil
}

func (w *OracleFeedWindow) add(ts time.Time, source, quote string, p canonical.Amount) error {
	if err := w.adopt(source, quote); err != nil {
		return err
	}
	ms := ts.UnixMilli()
	if prev, ok := w.Prices[ms]; ok && prev.Cmp(p) != 0 {
		return fmt.Errorf("%w: two prices at %s", ErrOracleFeedAmbiguous, ts.UTC())
	}
	w.Prices[ms] = p
	return nil
}
