package ingest

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	externalcoingecko "github.com/Stellar-Index/StellarIndex/internal/sources/external/coingecko"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// coingeckoLiveMarginPolls is how many poll intervals from a stored live row a vendor point still
// counts as inside live coverage; at two, a single missed poll does not open a hole to fill.
const coingeckoLiveMarginPolls = 2

// coingeckoHistoryBudget bounds the vendor walk: one paced call per pair plus any 429 cooldowns.
const coingeckoHistoryBudget = 2 * time.Hour

// backfillCoinGecko fills a hole in the coingecko reference series with the vendor's retrospective
// points, written through InsertOracleUpdate exactly as the live sink writes. Points inside live
// coverage are not written, and no divergence check is back-written.
func backfillCoinGecko(cfgPath, pairSym string, from, to time.Time, dryRun bool) error {
	cfg, err := config.LoadWithEnv(cfgPath)
	if err != nil {
		return err
	}
	p, pairs, err := coingeckoHistoryPlan(cfg.External.CoinGecko, pairSym)
	if err != nil {
		return err
	}

	fetchCtx, fetchCancel := context.WithTimeout(context.Background(), coingeckoHistoryBudget)
	defer fetchCancel()
	updates, err := p.History(fetchCtx, pairs, from, to)
	if err != nil {
		return fmt.Errorf("backfill-external: %w", err)
	}
	fmt.Fprintf(os.Stderr, "backfill-external: source=coingecko fetched %d point(s) for %d pair(s) in [%s, %s)\n",
		len(updates), len(pairs), from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))

	ctx, cancel := context.WithTimeout(context.Background(), externalInsertBudget)
	defer cancel()
	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer func() { _ = store.Close() }()

	margin := coingeckoLiveMarginPolls * p.PollInterval()
	live, err := store.OracleStampsInWindow(ctx, externalcoingecko.SourceName, from.Add(-margin), to.Add(margin), externalcoingecko.HistoryTxHashPrefix)
	if err != nil {
		return fmt.Errorf("backfill-external: live coverage probe: %w", err)
	}
	kept, covered := dropNearLive(updates, live, margin)
	if dryRun {
		fmt.Fprintf(os.Stderr, "backfill-external: would write %d of %d point(s); %d inside live coverage (within %v of a live row)\n",
			len(kept), len(updates), covered, margin)
		return nil
	}
	for i, u := range kept {
		if err := store.InsertOracleUpdate(ctx, u); err != nil {
			return fmt.Errorf("backfill-external: wrote %d of %d point(s) before: %w", i, len(kept), err)
		}
	}
	fmt.Fprintf(os.Stderr, "backfill-external: wrote %d of %d point(s); %d inside live coverage (within %v of a live row) not written\n",
		len(kept), len(updates), covered, margin)

	// The oracle_prices_* policies only roll forward, so a past window stays unmaterialised until refreshed.
	plan := timescale.PlanCAGGRefresh(timescale.OracleCAGGs, func(c timescale.CAGGSpec) (time.Time, time.Time) {
		return timescale.PadRefreshWindow(from, to, c.MinWindow)
	})
	for _, st := range plan {
		if err := timescale.RunCAGGRefreshStep(ctx, store, st, false); err != nil {
			return fmt.Errorf("backfill-external: rows written; refresh %s: %w", st.View, err)
		}
	}
	fmt.Fprintf(os.Stderr, "backfill-external: refreshed %d oracle aggregate(s) over the window\n", len(plan))
	return nil
}

// coingeckoHistoryPlan builds the poller and pair set exactly as the indexer wires the live
// poller; pairSym "all" keeps every pair, otherwise one "BASE/QUOTE" pair.
func coingeckoHistoryPlan(vc config.CoinGeckoVenueConfig, pairSym string) (*externalcoingecko.Poller, []canonical.Pair, error) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		return nil, nil, fmt.Errorf("verified-currency catalogue: %w", err)
	}
	ids := cat.CoinGeckoIDs()
	p := externalcoingecko.NewPoller()
	if vc.PollInterval > 0 {
		p.Interval = vc.PollInterval
	}
	if len(ids) > 0 {
		p.TickerToID = ids
	}
	p.APIKey = strings.TrimSpace(vc.APIKey)
	p.DemoAPIKey = strings.TrimSpace(vc.DemoAPIKey)

	pairs, _ := external.AggregatorPairs(ids)
	if strings.EqualFold(pairSym, "all") {
		return p, pairs, nil
	}
	for _, pr := range pairs {
		if strings.EqualFold(pr.Base.Code+"/"+pr.Quote.Code, pairSym) {
			return p, []canonical.Pair{pr}, nil
		}
	}
	return nil, nil, fmt.Errorf("-pair %q: not an aggregator pair (use \"all\" or BASE/QUOTE, e.g. XLM/USD)", pairSym)
}

// dropNearLive drops every update within margin of a stored row of the same (asset, quote), so a
// fill never interleaves vendor points into a span the live poller covered.
func dropNearLive(updates []canonical.OracleUpdate, live []timescale.OracleStamp, margin time.Duration) (kept []canonical.OracleUpdate, covered int) {
	byPair := map[string][]time.Time{}
	for _, s := range live {
		k := s.Asset + "|" + s.Quote
		byPair[k] = append(byPair[k], s.TS)
	}
	for _, ts := range byPair {
		slices.SortFunc(ts, time.Time.Compare)
	}
	for _, u := range updates {
		ts := byPair[u.Asset.String()+"|"+u.Quote.String()]
		lo := u.Timestamp.Add(-margin)
		i := sort.Search(len(ts), func(i int) bool { return !ts[i].Before(lo) })
		if i < len(ts) && !ts[i].After(u.Timestamp.Add(margin)) {
			covered++
			continue
		}
		kept = append(kept, u)
	}
	return kept, covered
}
