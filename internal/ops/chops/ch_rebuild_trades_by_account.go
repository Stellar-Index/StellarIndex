package chops

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// chRebuildTradesByAccountConflicts is the rest of the invocation -trades-by-account is checked against.
type chRebuildTradesByAccountConflicts struct {
	write, sdex, sdexGaps, sdexReconcile, sep41, contractCalls, bulkTrades, recordDirty, replace bool
	sources, contracts                                                                           string
}

// checkCHRebuildTradesByAccountFlags refuses a -trades-by-account run that would record a window it did not fully decode: a recorded
// window is the API's proof that every trade in it is present, so any narrowing of the trade set would make the archive silently partial.
func checkCHRebuildTradesByAccountFlags(on bool, c chRebuildTradesByAccountConflicts) error {
	if !on {
		if c.replace {
			return errors.New("ch-rebuild: -replace applies only to -trades-by-account")
		}
		return nil
	}
	var errs []error
	if !c.write {
		errs = append(errs, errors.New("needs -write (it writes ClickHouse)"))
	}
	if !c.sdex {
		errs = append(errs, errors.New("needs -sdex (sdex trades are part of the full trade set)"))
	}
	for _, f := range []struct {
		set  bool
		name string
	}{
		{c.sources != "", "-sources"},
		{c.contracts != "", "-contracts"},
		{c.sdexGaps, "-sdex-gaps"},
		{c.sdexReconcile, "-sdex-reconcile"},
		{c.sep41, "-sep41"},
		{c.contractCalls, "-contract-calls"},
		{c.bulkTrades, "-bulk-trades"},
		{c.recordDirty, "-record-dirty-window"},
	} {
		if f.set {
			errs = append(errs, fmt.Errorf("cannot be combined with %s (it would record a window it did not fully decode)", f.name))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("ch-rebuild -trades-by-account: %w", errors.Join(errs...))
}

// scopeCatalogueToTradeSources keeps the catalogue entries whose trades tradeOf reads and refuses a catalogue missing one: the window
// would record a source set the API does not count, after a heavy decode.
func scopeCatalogueToTradeSources(cat []reconSource) ([]reconSource, error) {
	want := make(map[string]bool, len(clickhouse.TradesByAccountSources))
	for _, n := range clickhouse.TradesByAccountSources {
		want[n] = true
	}
	var out []reconSource
	for _, src := range cat {
		if want[src.name] {
			out = append(out, src)
			delete(want, src.name)
		}
	}
	if len(want) > 0 {
		missing := make([]string, 0, len(want))
		for n := range want {
			missing = append(missing, n)
		}
		return nil, fmt.Errorf("ch-rebuild -trades-by-account: catalogue lacks trade source(s) %s", clickhouse.TradesSourcesKey(missing))
	}
	return out, nil
}

// tradeSourcesKey is the source-set key of the catalogue a run decoded, stored on the window it records.
func tradeSourcesKey(cat []reconSource) string {
	names := make([]string, len(cat))
	for i, src := range cat {
		names[i] = src.name
	}
	return clickhouse.TradesSourcesKey(names)
}

// writeCHRebuildTradesByAccount lands the buffered trades in stellar.trades_by_account and records [lo,hi] only after every row was sent.
func writeCHRebuildTradesByAccount(
	ctx context.Context,
	w io.Writer,
	prepare func(context.Context, []canonical.Trade) ([]canonical.Trade, []string, error),
	insert func(context.Context, []clickhouse.TradeByAccountRow) (int64, error),
	record func(ctx context.Context, lo, hi uint32, rows int64) error,
	buf []consumer.Event,
	lo, hi uint32,
) error {
	var trades []canonical.Trade
	for _, ev := range buf {
		if t, ok := tradeOf(ev); ok {
			trades = append(trades, t)
		}
	}
	rows, usd, err := prepare(ctx, trades)
	if err != nil {
		return fmt.Errorf("ch-rebuild -trades-by-account: prepare: %w", err)
	}
	var out []clickhouse.TradeByAccountRow
	for i, t := range rows {
		fan, ferr := clickhouse.FanOutTradeByAccount(t, usd[i])
		if ferr != nil {
			return fmt.Errorf("ch-rebuild -trades-by-account [%d,%d]: window NOT recorded: %w", lo, hi, ferr)
		}
		out = append(out, fan...)
	}
	sent, err := insert(ctx, out)
	if err != nil {
		return fmt.Errorf("ch-rebuild -trades-by-account [%d,%d]: %d of %d rows sent, window NOT recorded: %w", lo, hi, sent, len(out), err)
	}
	if err := record(ctx, lo, hi, sent); err != nil {
		return fmt.Errorf("ch-rebuild -trades-by-account [%d,%d]: rows sent but window NOT recorded: %w", lo, hi, err)
	}
	_, err = fmt.Fprintf(w, "\n=== ch-rebuild [%d,%d] trades-by-account ===\ntrades %d (storable %d) -> account rows %d; window recorded\n",
		lo, hi, len(trades), len(rows), sent)
	return err
}
