// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"math/big"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// # Why a classic asset needs a supply reading that is not a trustline sum
//
// The classic supply map ([Server.cachedClassicSupply]) sums trustline balances
// from the lake's current-state projection. A trustline is one of FOUR places a
// classic asset's supply can sit; the others are claimable balances,
// liquidity-pool reserves and SAC balances held by CONTRACT holders.
// `stellar.ledger_entries_current` fills `asset` for trustlines ONLY.
//
// # Why the fix reads flows rather than widening the state query
//
// A liquidity pool holds two assets in one row, and a SAC balance entry names its
// CONTRACT, never its asset. So the lake reads `stellar.supply_flows` (i128):
// Σmint − Σburn − Σclawback over the asset's SAC (as GET /v1/assets/{id}/supply).
//
// # Why it can only ever raise the served figure
//
// The trustline sum is a PROVABLE LOWER BOUND on issued supply. A flows total
// BELOW it means incompletely seeded flows, so [higherClassicSupply] keeps the
// trustline figure; cachedClassicSupply stops serving the map past classicSupplyMaxAge.

const (
	// classicLakeSupplyTTL bounds how long one asset's lake-flows reading is
	// reused. Supply moves on mints and burns, not on transfers, so it is
	// far more stable than a price; the TTL is long because the read is not.
	classicLakeSupplyTTL = 30 * time.Minute

	// classicLakeSupplyRetryGap rate-limits refresh ATTEMPTS, not refreshes.
	// Same lesson as classicSupplyRetryGap: once a heavy lake read starts
	// failing, retrying it per request is what turns one slow query into a
	// sustained outage.
	classicLakeSupplyRetryGap = 60 * time.Second

	// classicLakeSupplyBudget is the DETACHED refresh's own deadline. It
	// must exceed the API request timeout precisely because it does not run
	// on a request's context — that is the whole point of detaching.
	classicLakeSupplyBudget = 2 * time.Minute

	// classicLakeSupplyBatch caps how many contracts one refresh sums. The
	// cost of the read scales with the FLOW COUNT of the contracts in the
	// list, and a mature token carries millions of flows, so the list length
	// is the only handle the API has on it. A listing page asks about at
	// most 500 assets; it gets them a batch at a time across requests
	// instead of paying for all of them at once.
	classicLakeSupplyBatch = 32

	// classicLakeSupplyColdWait bounds how long a request blocks when NOTHING
	// it asked about is cached yet. Deliberately short, and deliberately only
	// on a fully cold answer: a warm cache never waits, and a request that
	// times out here still leaves the detached refresh running, so the next
	// one is served from cache.
	classicLakeSupplyColdWait = 750 * time.Millisecond

	// preciseSupplyMaxAge bounds how old an ADR-0011 supply observation may
	// be and still outrank the lake arms.
	//
	// The observer writes every asset on its watch-list every few minutes;
	// measured on r1 over a fourteen-day window the widest gap
	// between consecutive observations of any asset was 40 minutes. Six
	// hours is nine times that worst case, so a restart, a redeploy or a
	// slow pass never costs an asset its observation, while an observer
	// that has genuinely stopped hands the asset to the lake arms within
	// one working morning rather than indefinitely.
	//
	// The bound is load-bearing in BOTH directions and neither is
	// hypothetical. Too loose and a dead observer keeps publishing a figure
	// nobody is computing: an arm with no vintage bound served USDC at
	// 354,858,863.57 against 375,766,247.91 outstanding. Too tight and a healthy asset is
	// handed to an arm that read BLND +11.53% and PHO +156.79% high on the
	// same day (see [classicSupplyReading]).
	preciseSupplyMaxAge = 6 * time.Hour

	// classicLakeSupplyPrewarmGap spaces the background prewarm's batches.
	//
	// It is deliberately NOT classicLakeSupplyRetryGap. That gap exists to
	// stop a per-REQUEST retry storm from turning one slow supply_flows read
	// into a sustained outage, and its 60s is sized for an unbounded number
	// of concurrent callers. The prewarm is one serial driver: it runs a
	// single batch at a time, waits for it, and abandons the whole pass on
	// the first read that comes back with nothing (see
	// [Server.warmClassicLakeSupply]), so it cannot storm. What it still owes
	// ClickHouse is spacing between batches, which is what this is.
	classicLakeSupplyPrewarmGap = 2 * time.Second
)

// classicLakeSupplyReader is the narrow bulk capability this file needs from
// the token-supply reader. It is type-asserted off [Server.tokenSupply] rather
// than added to [TokenSupplyReader] so that every existing stub implementing
// that seam keeps compiling and simply opts out — the same optional-seam idiom
// [Server.latestPreciseSupply] uses on the assets reader.
//
// Production wiring: *clickhouse.SupplyReader, via
// clickhouse.NewSupplyReaderAuth in cmd/stellarindex-api/main.go.
type classicLakeSupplyReader interface {
	TokenSupplyForContracts(ctx context.Context, contractIDs []string) (map[string]clickhouse.TokenSupply, error)
}

// lakeSupplyEntry is one asset's cached lake-flows reading. An entry with an
// EMPTY value is a negative cache — "asked, and the lake had no usable answer"
// — and is as load-bearing as a positive one: without it, every asset the lake
// cannot answer for would be re-queried on every request forever.
type lakeSupplyEntry struct {
	value string
	at    time.Time
}

// classicSACContractID resolves a listing row's asset_id to the contract
// stellar.supply_flows is keyed by, for CLASSIC assets only.
//
// Native XLM is excluded deliberately, not incidentally: the XLM SAC's token
// supply is how much XLM is currently WRAPPED, a genuinely different quantity
// from the ledger header's total_coins, and asset_supply.go makes the same
// exclusion for the same reason. Soroban contract rows are excluded too —
// their supply already reaches the RWA contract arm through
// fillContractMarketCaps, and admitting them here would newly publish market
// caps for every SEP-41 token on the listing, which is a different change.
func classicSACContractID(assetID string) (string, bool) {
	asset, err := canonical.ParseAsset(assetID)
	if err != nil || asset.Type != canonical.AssetClassic {
		return "", false
	}
	sac, err := asset.SacContractID()
	if err != nil {
		return "", false
	}
	return sac, true
}

// higherClassicSupply returns the larger of two raw decimal supply readings,
// preferring `lake` on a tie or when only one is present, together with the
// basis that produced the winner.
//
// The asymmetry is the floor guard described at the top of this file: a lake
// figure below the trustline sum is incomplete seeding, so the trustline sum
// wins; a lake figure at or above it includes the claimable / LP / SAC-held
// supply the trustline sum cannot see, so the lake wins. An unparseable
// reading is treated as absent rather than as zero.
//
// It returns the basis rather than only the number because the two arms are
// not the same KIND of answer and the wire could not tell them apart: the lake
// sum covers all four holding domains, while the trustline sum is blind to
// three of them and is therefore a LOWER BOUND. An empty basis accompanies an
// empty value and means no reading at all.
func higherClassicSupply(lake, trustline string) (string, supply.Basis) {
	l, lok := new(big.Int).SetString(lake, 10)
	t, tok := new(big.Int).SetString(trustline, 10)
	switch {
	case !lok && !tok:
		return "", ""
	case !lok:
		return trustline, supply.BasisClassicTrustlineSum
	case !tok:
		return lake, supply.BasisClassicLakeFlows
	case l.Cmp(t) < 0:
		return trustline, supply.BasisClassicTrustlineSum
	default:
		return lake, supply.BasisClassicLakeFlows
	}
}

// classicSupplyReading resolves one listing row's circulating supply and names
// the basis that produced it.
//
// It is the ONE place the listing path's preference order lives
// ([Server.fillRowMarketCap], [Server.rwaFillMissingSupply] call it); two copies
// could publish a floor and a four-domain total under the same field name.
//
// Order: the ADR-0011 supply observation, then the lake-flows total, then the
// trustline sum, never below the trustline floor. A served figure below both the
// lake and Horizon looks like a reason to invert this, and is not: the lake arm
// over-counts when its flow history carries replayed mints whose burns are
// missing (BLND +11.53%, PHO +156.79%), which no completeness check in the
// reading can see (see [supply.BasisClassicLakeFlows]), while the observation
// matched Horizon.
//
// The observation's weakness is VINTAGE, so the read takes the observer's live
// row (timescale.Store.LatestSupplyObservations) bounded by [preciseSupplyMaxAge];
// an older observation is not offered, so a stale reading cannot outrank a live
// lake figure that disagrees with it.
func classicSupplyReading(
	assetID string, precise map[string]timescale.SupplyObservation, lake, broad map[string]string,
) (string, supply.Basis) {
	if obs, ok := precise[assetID]; ok && obs.CirculatingSupply != "" {
		// The observation publishes the basis the OBSERVER recorded
		// (issuer_exclusion, xlm_sdf_reserve_exclusion, override …), which
		// is the same basis /v1/assets/{asset_id} publishes for the same
		// asset. Inventing a listing-only name for it would put two labels
		// on one number.
		return obs.CirculatingSupply, supply.Basis(obs.Basis)
	}
	return higherClassicSupply(lake[assetID], broad[assetID])
}

// stampCirculatingSupply publishes `circ` on a listing row together with the
// basis that produced it.
//
// Nil-guarded on CirculatingSupply for the reason [Server.fillRowMarketCap]
// already was: a row whose supply another branch attached deliberately (the
// dust-suppressed and ticker-collision paths both do) must not have it
// overwritten here. The basis travels WITH the value, so a row can never carry
// a figure from one arm and a basis from another.
func stampCirculatingSupply(row *AssetDetail, circ string, basis supply.Basis) {
	if row.CirculatingSupply != nil || circ == "" {
		return
	}
	c := circ
	row.CirculatingSupply = &c
	if basis != "" {
		b := basis.String()
		row.SupplyBasis = &b
	}
}

// classicLakeSupply returns asset_id → circulating supply derived from the
// lake's mint/burn flows, for whichever of `rows` it can answer for.
//
// Best-effort at every step, like every other supply overlay on this path: no
// reader wired, no bulk capability, a read error, or a cold cache all yield a
// smaller map (or none), never an error and never a failed response. The
// caller falls back to the trustline sum, which is what it serves today.
//
// The refresh is DETACHED for the reason refreshClassicSupply is: a lake sum
// that outgrows the request timeout must not be able to latch the cache into
// permanent failure by dying on a caller's context before it can record the
// attempt.
func (s *Server) classicLakeSupply(ctx context.Context, rows []AssetDetail) map[string]string {
	if s.TokenSupply == nil {
		return nil
	}
	rd, ok := s.TokenSupply.(classicLakeSupplyReader)
	if !ok {
		return nil
	}
	wanted := classicLakeSupplyCandidates(rows)
	if len(wanted) == 0 {
		return nil
	}

	// contextcheck: readClassicLakeSupply deliberately does NOT take the
	// caller's ctx — it may launch the detached refresh, which must outlive
	// this request. The waiting below is what honours ctx here.
	out, missing, flight := s.readClassicLakeSupply(rd, wanted) //nolint:contextcheck // the detachment is the point; see this function's doc.
	// Wait only on a FULLY cold answer — a warm cache never blocks a request,
	// and a partially warm one is already better than the fallback.
	if len(out) > 0 || len(missing) == 0 || flight == nil {
		return out
	}
	timer := time.NewTimer(classicLakeSupplyColdWait)
	defer timer.Stop()
	select {
	case <-flight:
		warmed, _, _ := s.readClassicLakeSupply(rd, wanted) //nolint:contextcheck // same detachment as above; the flight it could start is deliberate.
		return warmed
	case <-timer.C:
		return out
	case <-ctx.Done():
		return out
	}
}

// classicLakeSupplyCandidates reduces a page of listing rows to the asset_id →
// SAC contract id map this file can act on, in a deterministic order.
func classicLakeSupplyCandidates(rows []AssetDetail) map[string]string {
	wanted := make(map[string]string, len(rows))
	for i := range rows {
		if _, dup := wanted[rows[i].AssetID]; dup {
			continue
		}
		if contractID, ok := classicSACContractID(rows[i].AssetID); ok {
			wanted[rows[i].AssetID] = contractID
		}
	}
	return wanted
}

// readClassicLakeSupply serves what the cache holds for `wanted`, and kicks a
// detached refresh for what it does not. Returns the served map, the asset ids
// still missing, and the in-flight refresh's completion channel (nil when no
// refresh is running).
func (s *Server) readClassicLakeSupply(
	rd classicLakeSupplyReader, wanted map[string]string,
) (map[string]string, []string, chan struct{}) {
	s.lakeSupplyMu.Lock()
	defer s.lakeSupplyMu.Unlock()

	out := make(map[string]string, len(wanted))
	for assetID := range wanted {
		if value, live := s.liveLakeSupplyLocked(assetID); live && value != "" {
			out[assetID] = value
		}
	}
	missing := s.missingLakeSupplyLocked(wanted) // sorted: deterministic batching across concurrent requests
	if len(missing) == 0 {
		return out, nil, nil
	}

	if s.lakeSupplyFlight != nil {
		return out, missing, s.lakeSupplyFlight
	}
	if time.Since(s.lakeSupplyAttemptAt) < classicLakeSupplyRetryGap {
		return out, missing, nil
	}
	s.lakeSupplyAttemptAt = time.Now() // advances on failure too
	flight := make(chan struct{})
	s.lakeSupplyFlight = flight
	// G118 is the intended behaviour, not a defect: detaching from the
	// request context is what keeps a slow lake sum from being killed at the
	// request deadline and retried, unbounded, by the next caller.
	go s.refreshClassicLakeSupply(rd, classicLakeSupplyBatchOf(missing, wanted), flight) //nolint:gosec,contextcheck // G118 + contextcheck: the detachment is deliberate — see this function's doc.
	return out, missing, flight
}

// liveLakeSupplyLocked reports one asset's cached reading and whether that
// entry is still live. An entry with an EMPTY value is live and NEGATIVE —
// "asked, and the lake had no usable answer" — which is why this returns a
// second boolean instead of letting "" stand for absent. Caller holds
// lakeSupplyMu.
//
// Extracted so the request path and the prewarm share ONE notion of what a
// live entry is. Two copies of a TTL comparison is how a prewarm ends up
// refilling slots the handler already considers warm (or, worse, skipping
// slots the handler considers cold).
func (s *Server) liveLakeSupplyLocked(assetID string) (string, bool) {
	entry, cached := s.lakeSupply[assetID]
	if !cached || time.Since(entry.at) >= classicLakeSupplyTTL {
		return "", false
	}
	return entry.value, true
}

// missingLakeSupplyLocked returns the asset ids in `wanted` with no live cache
// entry, sorted so that concurrent callers cut identical batches out of it.
// Caller holds lakeSupplyMu.
func (s *Server) missingLakeSupplyLocked(wanted map[string]string) []string {
	missing := make([]string, 0, len(wanted))
	for assetID := range wanted {
		if _, live := s.liveLakeSupplyLocked(assetID); !live {
			missing = append(missing, assetID)
		}
	}
	sort.Strings(missing)
	return missing
}

// classicLakeSupplyBatchOf takes the first [classicLakeSupplyBatch] entries of
// `missing` as an asset_id → contract batch.
//
// One place, deliberately: the batch size is the only handle either caller has
// on what a supply_flows sum costs, and a background driver that quietly cut a
// bigger batch than the request path would be a second, unreviewed load
// profile against the same table.
func classicLakeSupplyBatchOf(missing []string, wanted map[string]string) map[string]string {
	batch := make(map[string]string, classicLakeSupplyBatch)
	for _, assetID := range missing {
		if len(batch) >= classicLakeSupplyBatch {
			break
		}
		batch[assetID] = wanted[assetID]
	}
	return batch
}

// refreshClassicLakeSupply sums one batch of contracts' lake flows on a
// DETACHED context and folds the result into the cache.
//
// Every asset in the batch gets an entry, including the ones the lake had no
// usable answer for: an asset that is absent from the result, carries a nil
// total, or reports Incomplete (Σ(burn+clawback) > Σmint, which means the
// flows are incompletely seeded rather than that supply is negative — the same
// refusal /v1/assets/{asset_id}/supply and fillContractMarketCaps already
// make) is cached as an empty value so it is not re-queried until the TTL
// lapses. A read ERROR caches nothing: that is a fact about the lake being
// unavailable, not about the assets.
func (s *Server) refreshClassicLakeSupply(rd classicLakeSupplyReader, batch map[string]string, done chan struct{}) {
	// Deferred so a panic in the lake sum cannot leave the single-flight
	// marker set — which would freeze the cache for the life of the process,
	// since a non-nil flight is never replaced.
	defer s.endClassicLakeSupplyFlight(done)
	defer worker.Recover(s.logger, "api-classic-lake-supply-refresh")

	ctx, cancel := context.WithTimeout(context.Background(), classicLakeSupplyBudget)
	defer cancel()

	byContract := make(map[string]string, len(batch))
	ids := make([]string, 0, len(batch))
	for assetID, contractID := range batch {
		byContract[contractID] = assetID
		ids = append(ids, contractID)
	}
	sort.Strings(ids) // deterministic SQL for logs and tests

	sums, err := rd.TokenSupplyForContracts(ctx, ids)
	if err != nil {
		s.logger.Warn("classic lake-supply refresh failed (serving trustline sums)",
			"contracts", len(ids), "err", err)
		return
	}

	now := time.Now()
	s.lakeSupplyMu.Lock()
	defer s.lakeSupplyMu.Unlock()
	if s.lakeSupply == nil {
		s.lakeSupply = make(map[string]lakeSupplyEntry, len(batch))
	}
	for assetID, contractID := range batch {
		entry := lakeSupplyEntry{at: now}
		if sup, found := sums[contractID]; found && sup.Total != nil && !sup.Incomplete && sup.Total.Sign() > 0 {
			entry.value = sup.Total.String()
		}
		s.lakeSupply[assetID] = entry
	}
}

// endClassicLakeSupplyFlight releases the single-flight marker and wakes the
// cold-start waiters. Deferred by refreshClassicLakeSupply so it runs on EVERY
// exit, panic included.
func (s *Server) endClassicLakeSupplyFlight(done chan struct{}) {
	s.lakeSupplyMu.Lock()
	s.lakeSupplyFlight = nil
	s.lakeSupplyMu.Unlock()
	close(done)
}

// PrewarmClassicLakeSupply fills the lake-flows supply cache out of band, so the
// supply /v1/assets and /v1/rwa/assets publish does not depend on how recently
// somebody looked.
//
// Request-path warming converges only under traffic. Without it, entries expire
// at [classicLakeSupplyTTL] and the listing falls back to the trustline-only
// sum, which misses claimable balances, LP reserves and SAC-held supply. It
// lives in the API process because s.lakeSupply is per-process memory.
//
// Coverage is the pages `opts` names plus the /v1/rwa/assets membership: asking
// each surface which assets it serves cannot drift from what it serves, and
// "every classic asset" would be 450k+ SAC lookups. RWA members are chosen by
// attestation, not rank, so listing pages alone left them cold.
//
// Best-effort: a missing reader or capability, or a listing or lake error,
// leaves the cache as it was.
func (s *Server) PrewarmClassicLakeSupply(ctx context.Context, opts []timescale.ListAssetsOptions) {
	if s.TokenSupply == nil || s.AssetsReader == nil {
		return
	}
	rd, ok := s.TokenSupply.(classicLakeSupplyReader)
	if !ok {
		return
	}
	wanted := s.classicLakeSupplyPrewarmSet(ctx, opts)
	for assetID, contractID := range s.rwaClassicPrewarmSet(ctx) {
		wanted[assetID] = contractID
	}
	for assetID, contractID := range s.stablecoinPrewarmSet() {
		wanted[assetID] = contractID
	}
	if len(wanted) == 0 {
		return
	}
	s.warmClassicLakeSupply(ctx, rd, wanted)
}

// rwaClassicPrewarmSet asks the RWA surface which classic assets it serves, so
// a member that never appears on a ranked listing page is warmed anyway.
//
// It goes through [Server.cachedRWAMembership], so it behaves like a request:
// a stale set is served as-is while a detached, single-flighted rebuild is
// kicked behind it, and a cache that has NEVER been built waits for the
// in-flight first build (bounded by ctx) rather than returning nothing. Only
// a never-built cache whose build attempt is gapped out, or a ctx that ends
// first, yields no members — and then the sweep covers the listing alone.
//
// The reduction is the REQUEST PATH's own ([classicLakeSupplyCandidates]), the
// same one the listing side uses, so this cannot warm an asset the read path
// would not look up: native and contract-issued members drop out here, and the
// contract arm has its own reader.
func (s *Server) rwaClassicPrewarmSet(ctx context.Context) map[string]string {
	m := s.cachedRWAMembership(ctx)
	if len(m.members) == 0 {
		return nil
	}
	// Only AssetID is read by the reduction — it derives the SAC from the
	// id itself — so nothing else is filled in, rather than filling fields
	// that would look load-bearing and are not.
	details := make([]AssetDetail, 0, len(m.members))
	for _, mem := range m.members {
		details = append(details, AssetDetail{AssetID: mem.code + "-" + mem.issuer})
	}
	return classicLakeSupplyCandidates(details)
}

// classicLakeSupplyPrewarmSet asks the listing which assets it serves for
// `opts` and reduces the answer with the REQUEST PATH's own candidate
// derivation.
//
// Both halves are the drift guard: rows come from [Server.listAssetsExtAt] (as
// handleAssetList), the asset_id → SAC map from [classicLakeSupplyCandidates] (as
// [Server.classicLakeSupply]), and the projection is [assetDetailFromAssetRow].
// Nothing about the population or cache key is restated, so a prewarm cannot warm
// a phantom slot while real requests still pay the cold fill.
//
// The (Limit+1)th overfetch asset is warmed too: a superset, not a phantom, being
// the first row of the next page, and it rides a batch that ran anyway.
func (s *Server) classicLakeSupplyPrewarmSet(
	ctx context.Context, opts []timescale.ListAssetsOptions,
) map[string]string {
	wanted := make(map[string]string)
	for _, o := range opts {
		// Checked per shape, not just per batch: a cold boot pass reads a
		// dozen listing pages before it cuts its first batch, and this
		// goroutine is tracked by the shutdown WaitGroup.
		if ctx.Err() != nil {
			return wanted
		}
		rows, _, _, err := s.listAssetsExtAt(ctx, o)
		if err != nil {
			s.logger.Debug("classic lake-supply prewarm: listing read failed",
				"limit", o.Limit, "order", o.Order, "err", err)
			continue
		}
		details := make([]AssetDetail, 0, len(rows))
		for _, row := range rows {
			details = append(details, assetDetailFromAssetRow(row))
		}
		for assetID, contractID := range classicLakeSupplyCandidates(details) {
			wanted[assetID] = contractID
		}
	}
	return wanted
}

// warmClassicLakeSupply fills `wanted` one bounded batch at a time until every
// asset in it has a cache entry.
//
// Serial by construction — one batch in flight, then a pause, then the next —
// because the cost of a supply_flows sum scales with the FLOW COUNT of the
// contracts in it and a mature token carries millions of rows.
//
// It stops on the first batch that made no progress. That test is exact rather
// than approximate: [Server.refreshClassicLakeSupply] writes an entry for
// EVERY asset in its batch on success (an empty one where the lake had no
// usable answer) and writes nothing at all on a read error, so "the missing
// set did not shrink after a batch of ours" is precisely "the lake read
// failed". The answer to a failing supply_flows read is to stop and let the
// next sweep retry, not to walk the rest of the population into the same
// failure — the lesson classicLakeSupplyRetryGap encodes for the request path.
func (s *Server) warmClassicLakeSupply(
	ctx context.Context, rd classicLakeSupplyReader, wanted map[string]string,
) {
	// One iteration per asset is a ceiling no healthy pass comes near
	// (ceil(len/classicLakeSupplyBatch) is), and it is what stops a cache
	// expiring underneath a long pass from turning this into a spin.
	for range len(wanted) {
		remaining, keepGoing := s.warmClassicLakeSupplyBatch(ctx, rd, wanted)
		if remaining == 0 || !keepGoing {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(classicLakeSupplyPrewarmGap):
		}
	}
}

// warmClassicLakeSupplyBatch starts (or joins) one refresh and waits for it.
// Returns how many of `wanted` are still uncached afterwards, and whether the
// pass should continue.
//
// The retry gap is not consulted — see [classicLakeSupplyPrewarmGap] — but it
// is ADVANCED, because a prewarm batch is an attempt on the same table and
// leaving the request path free to kick a competing refresh the instant this
// one lands is the stampede the gap exists to prevent.
func (s *Server) warmClassicLakeSupplyBatch(
	ctx context.Context, rd classicLakeSupplyReader, wanted map[string]string,
) (int, bool) {
	s.lakeSupplyMu.Lock()
	missing := s.missingLakeSupplyLocked(wanted)
	if len(missing) == 0 {
		s.lakeSupplyMu.Unlock()
		return 0, false
	}
	before := len(missing)
	flight, mine := s.lakeSupplyFlight, false
	if flight == nil {
		flight = make(chan struct{})
		mine = true
		s.lakeSupplyAttemptAt = time.Now()
		s.lakeSupplyFlight = flight
		// Detached for the same reason the request path detaches, and
		// STARTED rather than called inline so the wait below can honour
		// ctx: a shutdown returns this goroutine at once and still lets the
		// refresh finish and record what it learned.
		go s.refreshClassicLakeSupply(rd, classicLakeSupplyBatchOf(missing, wanted), flight) //nolint:gosec,contextcheck // G118 + contextcheck: the detachment is deliberate — see refreshClassicLakeSupply's doc.
	}
	s.lakeSupplyMu.Unlock()

	select {
	case <-flight:
	case <-ctx.Done():
		return before, false
	}

	s.lakeSupplyMu.Lock()
	after := len(s.missingLakeSupplyLocked(wanted))
	s.lakeSupplyMu.Unlock()
	// A flight this pass merely JOINED was filling whatever batch its own
	// caller chose, which may not overlap `wanted` at all — so no progress
	// there says nothing about the lake's health. Only a batch of our own
	// that came back with nothing cached is evidence of a failed read.
	return after, after < before || !mine
}
