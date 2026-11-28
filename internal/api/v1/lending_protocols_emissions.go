package v1

import (
	"context"
	"math/big"
	"sort"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// lendingEmissionsUSDWindowReader is the Postgres side of /v1/lending/protocols' emissions block.
type lendingEmissionsUSDWindowReader interface {
	LendingEmissionsUSDCoverages(ctx context.Context) (map[string]timescale.LendingEmissionsUSDCoverage, error)
	LendingEmissionsUSDBucketSums(ctx context.Context, spans []timescale.LendingUSDSpan, valuationRev int16, streams []string) ([]timescale.LendingEmissionsUSDBucketSum, error)
}

var _ lendingEmissionsUSDWindowReader = (*timescale.Store)(nil)

// The emissions block's reasons beside not_valued, days_not_valued, valuation_stale, price_missing, all_excluded and the
// stored rows' own usd_reason and exclusions, which mean here what they mean on the revenue block and /emissions/history.
const (
	protocolEmissionsReadFailed  = "emissions_read_failed" // the stored days could not be read
	protocolEmissionsTimedOut    = "emissions_timed_out"   // the block's reads ran past protocolRevenueBudget
	protocolEmissionsUnavailable = "stream_unavailable"    // usd_reason: no valued pool serves the stream
	// amount_reason when the block cannot name one emission token for its rows: no amount is summed across tokens.
	protocolEmissionsTokenMixed    = "emission_token_mixed"            // rows are in different tokens
	protocolEmissionsTokenUnread   = "emission_token_not_stored"       // a row names no emission token
	protocolEmissionsDecimalsUnset = "emission_token_decimals_unknown" // a row names its token but not its decimals
)

// protocolEmissionStreams are the stored streams the block sums, in wire order. pool_claims_reserve is left out: its
// lines are a part of pool_claims, and summing both would count those claims twice.
var protocolEmissionStreams = [...]string{emisRowSchedule, emisRowPoolClaims, emisRowAllocBackstop, emisRowAllocPool, emisRowTierClaims}

// emisHistoryStream is the /emissions/history stream a stored stream comes from, the key of its coverage's unavailable.
var emisHistoryStream = map[string]string{
	emisRowSchedule: emisStreamReserves, emisRowPoolClaims: emisStreamPoolClaims, emisRowAllocBackstop: emisStreamBackstopAlloc,
	emisRowAllocPool: emisStreamBackstopAlloc, emisRowTierClaims: emisStreamBackstopClaims,
}

// LendingProtocolEmissions is a protocol's lending emissions over complete UTC days ending at through_day, summed from the
// stored day-by-day valuation (lending_emissions_usd_daily). Each stream is its own layer of the same tokens (scheduled
// to reserves, allocated by the backstop, claimed): they are never added together.
type LendingProtocolEmissions struct {
	// Degraded: a read behind the block failed or timed out; excluded names what it left out.
	Degraded bool `json:"degraded"`
	// EmissionToken is the one emission token every summed row names, null when they name none or several.
	EmissionToken         *string                        `json:"emission_token"`
	EmissionTokenDecimals *int                           `json:"emission_token_decimals"`
	Day                   LendingProtocolEmissionsWindow `json:"1d"`
	Week                  LendingProtocolEmissionsWindow `json:"7d"`
	Month                 LendingProtocolEmissionsWindow `json:"30d"`
	AllTime               LendingProtocolEmissionsWindow `json:"all_time"`
}

// LendingProtocolEmissionsWindow is one window's sums, [from_day, through_day] inclusive; both null when no pool is valued.
type LendingProtocolEmissionsWindow struct {
	FromDay    *string                             `json:"from_day"`
	ThroughDay *string                             `json:"through_day"`
	Streams    LendingProtocolEmissionStreams      `json:"streams"`
	Excluded   []LendingProtocolEmissionsExclusion `json:"excluded"`
}

// LendingProtocolEmissionStreams are the stored streams, named as stored.
type LendingProtocolEmissionStreams struct {
	ReserveSchedule       LendingProtocolEmissionAmounts `json:"reserve_schedule"`
	PoolClaims            LendingProtocolEmissionAmounts `json:"pool_claims"`
	BackstopAllocBackstop LendingProtocolEmissionAmounts `json:"backstop_alloc_backstop"`
	BackstopAllocPool     LendingProtocolEmissionAmounts `json:"backstop_alloc_pool"`
	BackstopClaims        LendingProtocolEmissionAmounts `json:"backstop_claims"`
}

// LendingProtocolEmissionAmounts is one stream's window: amount_usd is the exact sum rounded once to cents, amount the
// emission token's base units; each null with its reason when nothing of it can be summed.
type LendingProtocolEmissionAmounts struct {
	AmountUSD *string `json:"amount_usd"`
	// LowerBound: excluded leaves something out of amount_usd; AmountLowerBound: out of amount.
	LowerBound       bool    `json:"lower_bound"`
	USDReason        *string `json:"usd_reason"`
	Amount           *string `json:"amount"`
	AmountLowerBound bool    `json:"amount_lower_bound"`
	AmountReason     *string `json:"amount_reason"`
	// Approximate: some tokens summed are an approximate schedule's (BLNT's, rebuilt to dust); not a lower bound.
	Approximate bool `json:"approximate"`
}

// LendingProtocolEmissionsExclusion names what a window leaves out: a pool's (stream null for all its streams), or the
// pool list's (pool null). Count is days, stored rows or hours by reason.
type LendingProtocolEmissionsExclusion struct {
	Pool   *string `json:"pool"`
	Stream *string `json:"stream"`
	Reason string  `json:"reason"`
	Count  *int64  `json:"count"`
}

// emisPool is one listed pool as the block sees it, or why it is left out.
type emisPool struct{ pool, skip string }

// protocolEmissionsInput is everything one protocol's block is folded from.
type protocolEmissionsInput struct {
	listFailed bool
	pools      []emisPool
	covs       map[string]timescale.LendingEmissionsUSDCoverage
	sums       []timescale.LendingEmissionsUSDBucketSum
	verdicts   map[string]string // emission token → why a read-time verdict refuses it now
	// history is each stream's writer verdict now, as /emissions/history judges it; a stream absent is unverified.
	history map[string]historyVerdict
}

// emissionsCovered is a pool's coverage when it holds at least one day at the current valuation revision: coverage
// another valuer wrote cannot vouch that a missing row is a day with nothing to value.
func emissionsCovered(covs map[string]timescale.LendingEmissionsUSDCoverage, p emisPool) (timescale.LendingEmissionsUSDCoverage, bool) {
	c, ok := covs[p.pool]
	return c, ok && p.skip == "" && c.ValuationRev == lendingEmissionsValuationRev && !c.ThroughDay.Before(c.FloorDay)
}

func protocolEmissionsSpan(in protocolEmissionsInput) (floor, through time.Time, ok bool) {
	var cs []coveredDays
	for _, p := range in.pools {
		if c, covered := emissionsCovered(in.covs, p); covered {
			cs = append(cs, coveredDays{c.FloorDay, c.ThroughDay})
		}
	}
	return coveredSpan(cs)
}

// emisStreamAcc is one stream's window being summed.
type emisStreamAcc struct {
	usd, amount     *big.Rat
	serving         int  // valued pools inside the window serving the stream
	usdLeft, amtOut bool // a row was left out of amount_usd, of amount
	usdLB, amountLB bool
	approx          bool
}

func emisAdd(acc **big.Rat, s *string) bool {
	if s == nil {
		return true
	}
	x, ok := new(big.Rat).SetString(*s)
	if !ok {
		return false
	}
	if *acc == nil {
		*acc = new(big.Rat)
	}
	(*acc).Add(*acc, x)
	return true
}

// wire renders the stream; tokenReason is why the block cannot name one emission token, which nulls every amount.
func (a *emisStreamAcc) wire(covered int, tokenReason string) LendingProtocolEmissionAmounts {
	out := LendingProtocolEmissionAmounts{LowerBound: a.usdLB, AmountLowerBound: a.amountLB, Approximate: a.approx}
	switch {
	case covered == 0:
		out.USDReason, out.AmountReason = strRef(protocolRevenueNotValued), strRef(protocolRevenueNotValued)
		return out
	case a.serving == 0:
		out.USDReason, out.AmountReason = strRef(protocolEmissionsUnavailable), strRef(protocolEmissionsUnavailable)
		return out
	}
	switch {
	case a.usd == nil && a.usdLeft:
		out.USDReason = strRef(protocolRevenueAllExcluded)
	default:
		out.AmountUSD = strRef(ratString(ratOrZero(a.usd), 2))
	}
	switch {
	case tokenReason != "":
		out.AmountReason = strRef(tokenReason)
	case a.amount == nil && a.amtOut:
		out.AmountReason = strRef(protocolRevenueAllExcluded)
	default:
		if s, ok := ratExactDecimal(ratOrZero(a.amount)); ok {
			out.Amount = &s
		} else {
			out.AmountReason = strRef(protocolEmissionsReadFailed)
		}
	}
	return out
}

func ratOrZero(x *big.Rat) *big.Rat {
	if x == nil {
		return new(big.Rat)
	}
	return x
}

// emisWindowFold is one window being folded.
type emisWindowFold struct {
	w       LendingProtocolEmissionsWindow
	streams map[string]*emisStreamAcc
	covered int
	named   map[[3]string]int // (pool, stream, reason) → its index in w.Excluded
}

// exclude names what the window leaves out, merging counts per (pool, stream, reason); stream "" bounds every stream.
// amountToo: the tokens are left out as well as their USD.
func (f *emisWindowFold) exclude(pool, stream, reason string, count *int64, amountToo bool) {
	for name, a := range f.streams {
		if stream == "" || name == stream {
			a.usdLB, a.amountLB = true, a.amountLB || amountToo
		}
	}
	k := [3]string{pool, stream, reason}
	if i, ok := f.named[k]; ok {
		if e := &f.w.Excluded[i]; e.Count != nil && count != nil {
			*e.Count += *count
		}
		return
	}
	var n *int64
	if count != nil {
		n = countRef(*count)
	}
	f.named[k] = len(f.w.Excluded)
	f.w.Excluded = append(f.w.Excluded, LendingProtocolEmissionsExclusion{Pool: nonEmptyPtr(pool), Stream: nonEmptyPtr(stream), Reason: reason, Count: n})
}

// pools names every pool left out of, or short in, [from, through], and counts the pools valued inside it per stream.
func (f *emisWindowFold) pools(in protocolEmissionsInput, from, through time.Time) {
	if in.listFailed {
		f.exclude("", "", exclusionPoolListFailed, nil, true)
	}
	for _, p := range in.pools {
		c, covered := emissionsCovered(in.covs, p)
		switch {
		case p.skip != "":
			f.exclude(p.pool, "", p.skip, nil, true)
			continue
		case !covered:
			f.exclude(p.pool, "", protocolRevenueNotValued, nil, true)
			continue
		}
		overlap, missing := coveredDays{c.FloorDay, c.ThroughDay}.overlap(from, through)
		if missing > 0 {
			f.exclude(p.pool, "", protocolRevenueDaysNotValued, &missing, true)
		}
		if overlap == 0 {
			continue
		}
		f.covered++
		for _, s := range protocolEmissionStreams {
			if reason, off := c.Unavailable[emisHistoryStream[s]]; off {
				f.exclude(p.pool, s, reason, nil, true)
				continue
			}
			f.streams[s].serving++
		}
	}
}

// judge bounds each served stream whose writer is not verified back to its first ledger now, as /emissions/history
// does: a day stored with no row is only a day with nothing while the writer's history is verified.
func (f *emisWindowFold) judge(history map[string]historyVerdict) {
	for _, s := range protocolEmissionStreams {
		if f.streams[s].serving == 0 {
			continue
		}
		switch history[s] {
		case historyVerified:
		case historyVerdictStale:
			f.exclude("", s, earningsReasonHistoryVerdictStale, nil, true)
		case historyVerdictUnread:
			f.exclude("", s, earningsReasonHistoryVerdictUnread, nil, true)
		default:
			f.exclude("", s, earningsReasonHistoryUnverified, nil, true)
		}
	}
}

// group folds one stored group in: its tokens unless another revision stored them, its USD unless a reason, now or
// when stored, refuses it.
func (f *emisWindowFold) group(g timescale.LendingEmissionsUSDBucketSum, verdict string) {
	a, rows := f.streams[g.Stream], g.Rows
	if a == nil {
		return
	}
	if !g.Current {
		f.exclude(g.Pool, g.Stream, protocolRevenueStale, &rows, true)
		a.usdLeft, a.amtOut = true, true
		return
	}
	if g.StreamLowerBound {
		for _, r := range g.Excluded {
			f.exclude(g.Pool, g.Stream, r, &rows, true)
		}
		a.usdLB, a.amountLB = true, true
	}
	if !emisAdd(&a.amount, &g.Amount) {
		f.exclude(g.Pool, g.Stream, protocolEmissionsReadFailed, &rows, true)
		a.usdLeft, a.amtOut = true, true
		return
	}
	a.approx = a.approx || g.Approximate
	f.groupUSD(a, g, verdict)
}

func (f *emisWindowFold) groupUSD(a *emisStreamAcc, g timescale.LendingEmissionsUSDBucketSum, verdict string) {
	rows := g.Rows
	reason := verdict
	if g.USDReason != nil {
		reason = *g.USDReason
	}
	if reason == "" && !emisAdd(&a.usd, g.AmountUSD) {
		reason = protocolEmissionsReadFailed
	}
	if reason != "" {
		f.exclude(g.Pool, g.Stream, reason, &rows, false)
		a.usdLeft = true
		return
	}
	if g.LowerBound {
		hours := g.HoursUnvalued
		f.exclude(g.Pool, g.Stream, revenueUSDPriceMissing, &hours, false)
	}
}

func foldProtocolEmissions(in protocolEmissionsInput) LendingProtocolEmissions {
	floor, through, ok := protocolEmissionsSpan(in)
	var out LendingProtocolEmissions
	var tokenReason string
	out.EmissionToken, out.EmissionTokenDecimals, tokenReason = protocolEmissionToken(in)
	for i, w := range [...]*LendingProtocolEmissionsWindow{&out.Day, &out.Week, &out.Month, &out.AllTime} {
		*w = foldEmissionsWindow(in, protocolWindowFrom(i, floor, through), through, ok, i, tokenReason)
	}
	for _, e := range out.AllTime.Excluded { // all_time holds every pool and group the others do
		switch e.Reason {
		case exclusionPriceReadFailed, protocolEmissionsReadFailed, protocolEmissionsTimedOut, exclusionPoolListFailed,
			earningsReasonHistoryVerdictUnread:
			out.Degraded = true
		}
	}
	return out
}

// protocolEmissionToken is the one emission token the block's current rows are in, or why there is not one.
func protocolEmissionToken(in protocolEmissionsInput) (*string, *int, string) {
	var token *string
	var decimals *int
	seen, mixed := "", false
	for _, g := range in.sums {
		switch {
		case !g.Current:
			continue
		case g.EmissionToken == nil:
			return nil, nil, protocolEmissionsTokenUnread
		case g.EmissionTokenDecimals == nil:
			return nil, nil, protocolEmissionsDecimalsUnset
		}
		k := *g.EmissionToken + "\x00" + strconv.Itoa(*g.EmissionTokenDecimals)
		mixed = mixed || (seen != "" && k != seen)
		seen, token, decimals = k, g.EmissionToken, g.EmissionTokenDecimals
	}
	if mixed {
		return nil, nil, protocolEmissionsTokenMixed
	}
	return token, decimals, ""
}

// foldEmissionsWindow sums the window [from, through] holding buckets 0..maxBucket.
func foldEmissionsWindow(in protocolEmissionsInput, from, through time.Time, ok bool, maxBucket int, tokenReason string) LendingProtocolEmissionsWindow {
	f := &emisWindowFold{w: LendingProtocolEmissionsWindow{Excluded: []LendingProtocolEmissionsExclusion{}}, streams: map[string]*emisStreamAcc{}, named: map[[3]string]int{}}
	for _, s := range protocolEmissionStreams {
		f.streams[s] = &emisStreamAcc{}
	}
	if ok {
		f.w.FromDay, f.w.ThroughDay = strRef(from.Format(time.DateOnly)), strRef(through.Format(time.DateOnly))
	}
	f.pools(in, from, through)
	f.judge(in.history)
	for _, g := range emisWindowGroups(in, maxBucket) {
		f.group(g, in.verdicts[strOrNone(g.EmissionToken)])
	}
	s, wire := &f.w.Streams, func(stream string) LendingProtocolEmissionAmounts {
		return f.streams[stream].wire(f.covered, tokenReason)
	}
	s.ReserveSchedule, s.PoolClaims, s.BackstopClaims = wire(emisRowSchedule), wire(emisRowPoolClaims), wire(emisRowTierClaims)
	s.BackstopAllocBackstop, s.BackstopAllocPool = wire(emisRowAllocBackstop), wire(emisRowAllocPool)
	return f.w
}

// strOrNone is s, or "" for nil: a group naming no token is judged as the token "".
func strOrNone(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// emisWindowGroups are the groups of buckets 0..maxBucket of valued pools, in pool then stream order, so exclusions
// are named in a stable order.
func emisWindowGroups(in protocolEmissionsInput, maxBucket int) []timescale.LendingEmissionsUSDBucketSum {
	order := map[string]int{}
	for i, p := range in.pools {
		if _, covered := emissionsCovered(in.covs, p); covered {
			order[p.pool] = i
		}
	}
	var out []timescale.LendingEmissionsUSDBucketSum
	for _, g := range in.sums {
		if _, ok := order[g.Pool]; ok && g.Bucket <= maxBucket {
			out = append(out, g)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pool != out[j].Pool {
			return order[out[i].Pool] < order[out[j].Pool]
		}
		return out[i].Stream < out[j].Stream
	})
	return out
}

// protocolEmissionsPools are a protocol's listed pools. A pool whose state could not be read is kept: the block reads
// only its stored rows, and the read-time verdict is the emission token's, not the pool's.
func protocolEmissionsPools(protocol string, reads []protocolPoolRead) []emisPool {
	var out []emisPool
	for i := range reads {
		if reads[i].protocol == protocol {
			out = append(out, emisPool{pool: reads[i].pool})
		}
	}
	return out
}

// readProtocolEmissions builds every protocol's emissions block from one coverage read, one GROUP BY and one verdict per
// emission token, all within protocolRevenueBudget. A failed or timed-out read is named in the blocks and marks them
// degraded, which makes the build partial: served, never cached as good.
func (s *Server) readProtocolEmissions(ctx context.Context, reads []protocolPoolRead, blntListFailed bool) map[string]LendingProtocolEmissions {
	ins := map[string]*protocolEmissionsInput{}
	for _, pt := range lendingProtocols {
		ins[pt.protocol] = &protocolEmissionsInput{
			listFailed: pt.protocol == blnt.SourceName && blntListFailed, pools: protocolEmissionsPools(pt.protocol, reads),
		}
	}
	bctx, cancel := context.WithTimeout(ctx, protocolRevenueBudget)
	defer cancel()
	failed, reason := s.fillProtocolEmissions(bctx, ins), protocolEmissionsReadFailed
	if bctx.Err() != nil {
		failed, reason = true, protocolEmissionsTimedOut
	}
	out := map[string]LendingProtocolEmissions{}
	for p, in := range ins {
		if failed {
			for i := range in.pools {
				in.pools[i].skip = reason
			}
		}
		out[p] = foldProtocolEmissions(*in)
	}
	return out
}

// fillProtocolEmissions reads the coverage, the sums and the verdicts into ins; true when the stored days could not be
// read. Without a reader (no Postgres) the inputs stay empty and every pool reads not_valued.
func (s *Server) fillProtocolEmissions(ctx context.Context, ins map[string]*protocolEmissionsInput) bool {
	rd, ok := s.ProtocolStats.(lendingEmissionsUSDWindowReader)
	if !ok {
		return false
	}
	covs, err := rd.LendingEmissionsUSDCoverages(ctx)
	if err != nil {
		s.logger.Warn("lending protocols: emissions coverage read failed", "err", err)
		return true
	}
	var spans []timescale.LendingUSDSpan
	byPool := map[string]*protocolEmissionsInput{}
	for _, in := range ins {
		in.covs = covs
		_, through, ok := protocolEmissionsSpan(*in)
		for _, p := range in.pools {
			byPool[p.pool] = in
			if c, covered := emissionsCovered(covs, p); ok && covered {
				spans = append(spans, timescale.LendingUSDSpan{Pool: p.pool, FromDay: c.FloorDay, ThroughDay: minTime(c.ThroughDay, through), WindowEnd: through})
			}
		}
	}
	sums, err := rd.LendingEmissionsUSDBucketSums(ctx, spans, lendingEmissionsValuationRev, protocolEmissionStreams[:])
	if err != nil {
		s.logger.Warn("lending protocols: emissions sums read failed", "err", err)
		return true
	}
	for _, g := range sums {
		if in := byPool[g.Pool]; in != nil {
			in.sums = append(in.sums, g)
		}
	}
	history := s.projectionHistoryVerdicts(ctx, emissionsVerdictSources...)
	for p, in := range ins {
		in.verdicts = s.emissionsVerdicts(ctx, p, in.sums)
		poolSrc, bstopSrc := emissionsWriterSources(p == blnt.SourceName)
		in.history = map[string]historyVerdict{
			emisRowSchedule: history[poolSrc], emisRowPoolClaims: history[poolSrc], emisRowAllocBackstop: history[bstopSrc],
			emisRowAllocPool: history[bstopSrc], emisRowTierClaims: history[bstopSrc],
		}
	}
	return false
}

// emissionsVerdicts is each emission token's read-time refusal of its stored USD, as /emissions/history refuses it now:
// the token identity gate (not the protocol token's verified SAC), then the walk's verdict (withheld or
// price_read_failed), one walk per token over its latest stored day with USD.
func (s *Server) emissionsVerdicts(ctx context.Context, protocol string, sums []timescale.LendingEmissionsUSDBucketSum) map[string]string {
	last := map[string]time.Time{}
	var tokens []string
	for _, g := range sums {
		if g.AmountUSD == nil || !g.Current {
			continue
		}
		t := strOrNone(g.EmissionToken)
		d, seen := last[t]
		if !seen {
			tokens = append(tokens, t)
		}
		if !seen || g.LastValuedDay.After(d) {
			last[t] = g.LastValuedDay
		}
	}
	reasons := make([]string, len(tokens))
	forEachBounded(s.logger, len(tokens), readFanoutConcurrency, func(i int) {
		if _, why := s.emissionTokenIdentity(protocol, tokens[i]); why != "" {
			reasons[i] = why
		} else if d := last[tokens[i]]; !d.IsZero() {
			reasons[i] = usdReadRefusal(s.revenueDayRead(ctx, tokens[i], d, 0)) // distinct indices
		}
	})
	out := map[string]string{}
	for i, t := range tokens {
		if reasons[i] != "" {
			out[t] = reasons[i]
		}
	}
	return out
}
