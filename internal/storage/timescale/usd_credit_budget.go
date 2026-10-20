package timescale

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The credibility budget bounds what token/token prints can add to served USD volume. A rate alone licenses nothing: an asset whose
// only USD evidence is a thousand dollars of anchor-market turnover must not stamp nine figures of token/token volume, print after
// print. Served token/token USD charged to an asset over the trailing window stays at or below creditBudgetFactor x its anchor-market
// (USD-peg and XLM) turnover over the same closed hours; a print that would cross the line is refused (usd_volume NULL, label
// "refused"), never capped, so faking the backing costs a wash on the anchor market, where it shows.
const (
	creditBudgetFactor = 2
	creditBudgetWindow = 24 * time.Hour
)

// anchorBackingReporter is implemented by a resolver that can measure an asset's anchor-market turnover; a resolver without it (test
// fakes) serves token/token prints at min(legs) with no budget.
type anchorBackingReporter interface {
	AnchorBackingAt(ctx context.Context, asset canonical.Asset, at time.Time) (*big.Rat, bool, error)
}

// creditSeedFunc returns the token/token USD already served on the asset in [from, to) per hour bucket (unix seconds of the hour), so a
// restarted writer neither starts from zero nor holds the past for longer than the window.
type creditSeedFunc func(ctx context.Context, asset canonical.Asset, from, to time.Time) (map[int64]*big.Rat, error)

// usdCreditBudget is the per-asset sliding window of served token/token USD: one instance per writer (one writer per domain), and
// replays are sequential, so the window is exact up to batch order.
type usdCreditBudget struct {
	factor *big.Rat
	window time.Duration
	seed   creditSeedFunc

	mu     sync.Mutex
	served map[string]*creditLedger
	// charged maps each admitted trade ID to its hour, so a write retried after a failed INSERT is not charged twice.
	charged map[string]int64
	latest  time.Time
	admits  int
	// tally turns on refused/refusedUSD, the committed declines a dry run reports; the live path never sets it.
	tally      bool
	refused    int64
	refusedUSD *big.Rat
}

// creditLedger holds one asset's served USD per hour bucket (unix seconds of the hour).
type creditLedger struct {
	hours map[int64]*big.Rat
	last  int64
}

func newUSDCreditBudget(seed creditSeedFunc) *usdCreditBudget {
	return &usdCreditBudget{
		factor:     big.NewRat(creditBudgetFactor, 1),
		window:     creditBudgetWindow,
		seed:       seed,
		served:     map[string]*creditLedger{},
		charged:    map[string]int64{},
		refusedUSD: new(big.Rat),
	}
}

// creditKey folds every alias form of an asset (classic, SAC, the XLM spellings) onto one ledger.
func creditKey(a canonical.Asset) string {
	return strings.Join(assetFormNames(a), "|")
}

func assetFormNames(a canonical.Asset) []string {
	forms := canonical.AssetAliases(a)
	names := make([]string, 0, len(forms))
	for _, f := range forms {
		names = append(names, f.String())
	}
	sort.Strings(names)
	return names
}

func creditHour(at time.Time) int64 { return at.UTC().Truncate(time.Hour).Unix() }

// admit reports whether value may be served for trade id charged to asset at `at`: the asset's served window plus value must not exceed
// factor x backing. commit records the charge; a dry run leaves the window as it was. A trade already charged is admitted again as is.
func (b *usdCreditBudget) admit(ctx context.Context, id string, asset canonical.Asset, at time.Time, value, backing *big.Rat, commit bool) (bool, error) {
	key := creditKey(asset)
	if err := b.ensureLedger(ctx, key, asset, at); err != nil {
		return false, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.charged[id]; ok {
		return true, nil
	}
	b.sweep(at)
	led := b.served[key]
	if led == nil {
		// Swept between ensureLedger and here: its seed rows were all older than the sweep floor.
		led = &creditLedger{hours: map[int64]*big.Rat{}, last: creditHour(at)}
		b.served[key] = led
	}
	total := led.sum(b.windowStart(at), at)
	total.Add(total, value)
	if total.Cmp(new(big.Rat).Mul(backing, b.factor)) > 0 {
		if commit && b.tally {
			b.refused++
			b.refusedUSD.Add(b.refusedUSD, value)
		}
		return false, nil
	}
	if commit {
		led.add(at, value)
		b.charged[id] = creditHour(at)
	}
	return true, nil
}

// windowStart is the first hour bucket in the window ending at the hour holding `at`: 24 buckets, the current one included.
func (b *usdCreditBudget) windowStart(at time.Time) time.Time {
	return at.UTC().Truncate(time.Hour).Add(time.Hour - b.window)
}

// ensureLedger creates the asset's ledger, seeded from the trades already written. The seed query runs without the lock so one asset's
// round trip does not stall every other writer.
func (b *usdCreditBudget) ensureLedger(ctx context.Context, key string, asset canonical.Asset, at time.Time) error {
	b.mu.Lock()
	_, ok := b.served[key]
	b.mu.Unlock()
	if ok {
		return nil
	}
	led := &creditLedger{hours: map[int64]*big.Rat{}, last: creditHour(at)}
	if b.seed != nil {
		prior, err := b.seed(ctx, asset, b.windowStart(at), at)
		if err != nil {
			return err
		}
		for h, v := range prior {
			led.add(time.Unix(h, 0), v)
		}
	}
	b.mu.Lock()
	if _, ok := b.served[key]; !ok {
		b.served[key] = led
	}
	b.mu.Unlock()
	return nil
}

func (l *creditLedger) add(at time.Time, v *big.Rat) {
	h := creditHour(at)
	if cur := l.hours[h]; cur != nil {
		cur.Add(cur, v)
	} else {
		l.hours[h] = new(big.Rat).Set(v)
	}
	if h > l.last {
		l.last = h
	}
}

// sum totals the buckets from the hour holding `from` through the hour holding `to`.
func (l *creditLedger) sum(from, to time.Time) *big.Rat {
	total := new(big.Rat)
	lo, hi := creditHour(from), creditHour(to)
	for h, v := range l.hours {
		if h >= lo && h <= hi {
			total.Add(total, v)
		}
	}
	return total
}

// sweep bounds memory: every 1024 admits it drops the hours, ledgers and charged IDs two windows behind the newest print seen.
func (b *usdCreditBudget) sweep(at time.Time) {
	if at.After(b.latest) {
		b.latest = at
	}
	b.admits++
	if b.admits%1024 != 0 {
		return
	}
	floor := creditHour(b.latest.Add(-2 * b.window))
	for id, h := range b.charged {
		if h < floor {
			delete(b.charged, id)
		}
	}
	for key, led := range b.served {
		if led.last < floor {
			delete(b.served, key)
			continue
		}
		for h := range led.hours {
			if h < floor {
				delete(led.hours, h)
			}
		}
	}
}

// creditLegs is one token/token print seen from its rated leg: ratedVal is rated's amount x rate; other is valued here when it rates too.
type creditLegs struct {
	rated       canonical.Asset
	ratedVal    *big.Rat
	other       canonical.Asset
	otherAmount canonical.Amount
	decimals    int
	at          time.Time
	id          string
}

// creditBoundUSDVolume values a token/token print: the smaller of its rated legs (a thin leg cannot borrow a deep leg's backing), charged
// to the leg with the deeper anchor market and admitted only within that asset's credibility budget. refused is the budget's decline.
// Without a budget, or a resolver that reports backing, the legacy [boundUSDVolume] ceiling applies instead.
func creditBoundUSDVolume(
	ctx context.Context, r USDVolumeFXResolver, budget *usdCreditBudget, legs creditLegs, commit bool,
) (*big.Rat, bool, error) {
	rep, ok := r.(anchorBackingReporter)
	if !ok || budget == nil {
		v, err := boundUSDVolume(ctx, r, legs.ratedVal, legs.other, legs.otherAmount, legs.decimals, legs.at)
		return v, false, err
	}
	otherVal, err := fxLegValue(ctx, r, legs.other, legs.otherAmount, legs.decimals, legs.at)
	if err != nil {
		return nil, false, err
	}
	value := legs.ratedVal
	if otherVal != nil && otherVal.Cmp(value) < 0 {
		value = otherVal
	}
	// One possibly self-authored leg keeps the legacy ceiling whatever the budget allows.
	if otherVal == nil && value.Cmp(singleLegMaxUSDVolume) > 0 {
		return nil, false, nil
	}
	charged, backing, err := creditChargeLeg(ctx, rep, legs, otherVal != nil)
	if err != nil {
		return nil, false, err
	}
	admitted, err := budget.admit(ctx, legs.id, charged, legs.at, value, backing, commit)
	if err != nil {
		return nil, false, err
	}
	if !admitted {
		return nil, true, nil
	}
	return value, false, nil
}

// creditChargeLeg picks the rated leg with the larger anchor backing (the rated leg alone when the other is unrated) and returns that
// backing, zero when no anchor market traded.
func creditChargeLeg(ctx context.Context, rep anchorBackingReporter, legs creditLegs, otherRated bool) (canonical.Asset, *big.Rat, error) {
	backing, ok, err := rep.AnchorBackingAt(ctx, legs.rated, legs.at)
	if err != nil {
		return legs.rated, nil, err
	}
	if !ok {
		backing = new(big.Rat)
	}
	if !otherRated {
		return legs.rated, backing, nil
	}
	otherBacking, ok, err := rep.AnchorBackingAt(ctx, legs.other, legs.at)
	if err != nil {
		return legs.rated, nil, err
	}
	if ok && otherBacking.Cmp(backing) > 0 {
		return legs.other, otherBacking, nil
	}
	return legs.rated, backing, nil
}

// servedTokenTokenUSD seeds a credibility ledger from the trades already written: usd_volume per hour on prints in [from, to) where the
// asset is a leg and neither leg is an anchor. Prints charged to the other leg, or valued by an exact tier, count too: the strict side.
func (s *Store) servedTokenTokenUSD(anchors []string) creditSeedFunc {
	const q = `SELECT extract(epoch FROM date_trunc('hour', ts))::bigint, sum(usd_volume)::text FROM trades
	            WHERE ts >= $1::timestamptz AND ts < $2::timestamptz AND usd_volume IS NOT NULL
	              AND (base_asset = ANY($3::text[]) OR quote_asset = ANY($3::text[]))
	              AND NOT (base_asset = ANY($4::text[])) AND NOT (quote_asset = ANY($4::text[]))
	            GROUP BY 1`
	return func(ctx context.Context, asset canonical.Asset, from, to time.Time) (map[int64]*big.Rat, error) {
		rows, err := s.db.QueryContext(ctx, q, from, to, assetFormNames(asset), anchors)
		if err != nil {
			return nil, fmt.Errorf("timescale: credibility seed for %s: %w", asset, err)
		}
		defer func() { _ = rows.Close() }()
		out := map[int64]*big.Rat{}
		for rows.Next() {
			var hour int64
			var text string
			if err := rows.Scan(&hour, &text); err != nil {
				return nil, fmt.Errorf("timescale: credibility seed for %s: %w", asset, err)
			}
			v, ok := new(big.Rat).SetString(text)
			if !ok {
				return nil, fmt.Errorf("timescale: credibility seed for %s: unparsable sum %q", asset, text)
			}
			out[hour] = v
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("timescale: credibility seed for %s: %w", asset, err)
		}
		return out, nil
	}
}

// ChargeTradesUSDCredit charges the credibility budget for batch exactly as BatchInsertTrades would (same filter, order and dedupe),
// without writing, and counts the declines. Only a dry run may call it: the charges consume budget no stored row accounts for.
func (s *Store) ChargeTradesUSDCredit(ctx context.Context, batch []canonical.Trade) error {
	b := s.creditBudget
	if b == nil {
		return nil
	}
	b.mu.Lock()
	b.tally = true
	b.mu.Unlock()
	rows := slices.Clone(s.filterStorableTrades(batch))
	sortTradesByConflictKey(rows)
	for _, t := range dedupeSortedTradesByConflictKey(rows) {
		if _, _, _, err := tradeUSDVolumeCredited(ctx, t, s.usdVolumeQuoteSpec, s.usdVolumeFXResolver, b, true); err != nil {
			return err
		}
	}
	return nil
}

// CreditRefusals returns how many prints the credibility budget declined while charging, and their USD total; zero without a budget.
func (s *Store) CreditRefusals() (int64, *big.Rat) {
	b := s.creditBudget
	if b == nil {
		return 0, new(big.Rat)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.refused, new(big.Rat).Set(b.refusedUSD)
}
