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
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// # Why a classic asset needs a supply reading that is not a trustline sum
//
// The broad-coverage classic supply map ([Server.cachedClassicSupply]) is
// `sum(balance) … WHERE entry_type = 'trustline'` over the lake's current-state
// projection. A trustline is one of FOUR places a classic asset's supply can
// sit — the other three are claimable balances, liquidity-pool reserves, and
// the balances a Stellar Asset Contract holds for CONTRACT (C-address) holders
// in its own contract_data. `stellar.ledger_entries_current` populates its
// `asset` column for trustlines ONLY (internal/storage/clickhouse/
// extract_entry_changes.go, ownerAndAsset: "asset is empty for everything but
// trustlines"), so a query keyed on `asset = 'CODE-ISSUER'` cannot see the
// other three AT ALL. It is not undercounting by accident; it is blind by
// construction.
//
// Measured against Horizon on the /v1/rwa/assets set (2026-09-11), the supply
// invisible to the trustline sum was CETES +36.605%, TESOURO +10.594%,
// USTRY +10.272%, USDY +1.274% — and 99.9% of it sat in SAC contract_data,
// with claimable balances and LP reserves rounding to nothing. The served USDY
// figure of $528.9M against the independent Dune reading of $535.6M is exactly
// that 1.274% residual.
//
// # Why the fix reads flows rather than fixing the state query
//
// The three missing domains cannot be recovered by widening that query:
//
//   - A liquidity pool holds TWO assets and TWO reserves in one row, so one
//     (asset, balance) column pair structurally cannot represent it.
//   - A SAC Balance entry names its CONTRACT, never its asset. Recovering the
//     asset means mapping contract → asset, which is a one-way hash in that
//     direction; it is only derivable FORWARD, from the asset.
//
// So the projector cannot stamp what is not in the entry, and the lake's
// 600M-row contract_data slice would have to be decoded to get at a number the
// lake already holds elsewhere: `stellar.supply_flows`, the decode-at-ingest
// CAP-67 / SEP-41 mint/burn/clawback log whose amounts are already i128-decoded
// at write time. Σmint − Σburn − Σclawback over a classic asset's SAC contract
// is its supply REGARDLESS of which of the four domains currently holds it —
// a flow does not know where the tokens came to rest. That is the same figure
// GET /v1/assets/{asset_id}/supply already serves for this exact asset
// (asset_supply.go, resolveSupplyContractID → the deterministic SAC address),
// and the same reader; this file stops the listing surfaces from publishing a
// smaller, blinder number than the endpoint next door.
//
// # Why it can only ever raise the served figure
//
// Every trustline balance was minted, so the trustline sum is a PROVABLE LOWER
// BOUND on a classic asset's issued supply. A flows total BELOW that bound is
// therefore proof that the contract's flows are incompletely seeded, not
// evidence that the trustline sum is too high — so [higherClassicSupply] keeps
// the trustline figure in that case. The consequence is the safety property
// this change rests on: no asset's served circulating supply can go DOWN as a
// result of it. It closes a known understatement and cannot open a new one.

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
// preferring `lake` on a tie or when only one is present.
//
// The asymmetry is the floor guard described at the top of this file: a lake
// figure below the trustline sum is incomplete seeding, so the trustline sum
// wins; a lake figure at or above it includes the claimable / LP / SAC-held
// supply the trustline sum cannot see, so the lake wins. An unparseable
// reading is treated as absent rather than as zero.
func higherClassicSupply(lake, trustline string) string {
	l, lok := new(big.Int).SetString(lake, 10)
	t, tok := new(big.Int).SetString(trustline, 10)
	switch {
	case !lok && !tok:
		return ""
	case !lok:
		return trustline
	case !tok:
		return lake
	case l.Cmp(t) < 0:
		return trustline
	default:
		return lake
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
	if s.tokenSupply == nil {
		return nil
	}
	rd, ok := s.tokenSupply.(classicLakeSupplyReader)
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

// PrewarmClassicLakeSupply fills the lake-flows supply cache out of band, so
// the supply /v1/assets and /v1/rwa/assets publish does not depend on how
// recently somebody looked.
//
// # The defect this closes
//
// Everything above this line warms itself from the REQUEST path: a listing
// request finds nothing cached, kicks a detached refresh for
// [classicLakeSupplyBatch] of the assets it asked about, and serves the
// trustline sum for the rest. That shape converges under sustained traffic —
// the served figures then match Horizon's all-component totals to within
// 0.012% — and it never converges without it. Convergence was the unstated
// assumption, and it does not hold on a service with no consumer traffic:
// entries expire unread at [classicLakeSupplyTTL] and the listing falls back
// to the trustline-only sum, which is blind to claimable balances, LP reserves
// and SAC-held supply by construction (see the top of this file). Measured on
// r1 2026-09-12, ~19 h after the last request:
//
//	PYUSD  served  3,149,454   lake  11,778,001   (73% understated)
//	XRF    served 21,895,149   lake 118,333,629   (82% understated)
//
// The readings and the preference chain were already right. Only the warming
// was wrong, so this changes only the warming: the source ranking is untouched
// (precise/supply_1d still outranks the lake, which still cannot fall below
// the trustline floor), and every failure path still degrades to exactly what
// is served today.
//
// # Why it lives in the API process rather than a job
//
// s.lakeSupply is per-process memory with no external store behind it, so no
// timer, worker or cron outside this process can fill it. Moving the warming
// out would mean materialising the sums into a table and teaching the read
// path about it — a strictly larger change with its own staleness contract.
//
// # Which assets it covers
//
// `opts` is the set of listing shapes the caller already keeps warm —
// production passes assetListingPrewarmOptions(), the SAME set
// prewarmAssetListings warms — and the population is whatever those pages
// return. Both alternatives are worse: "every classic asset" is 450k+ SAC
// lookups for rows no page shows, and a hand-maintained asset list here would
// drift from the pages callers actually receive. Asking the listing which
// assets it serves cannot drift from the listing.
//
// Best-effort throughout, like every other supply overlay on this path: no
// token-supply reader, no bulk capability, no assets reader, a listing error
// or a lake error each leave the cache exactly as it was.
func (s *Server) PrewarmClassicLakeSupply(ctx context.Context, opts []timescale.ListAssetsOptions) {
	if s.tokenSupply == nil || s.assetsReader == nil {
		return
	}
	rd, ok := s.tokenSupply.(classicLakeSupplyReader)
	if !ok {
		return
	}
	wanted := s.classicLakeSupplyPrewarmSet(ctx, opts)
	if len(wanted) == 0 {
		return
	}
	s.warmClassicLakeSupply(ctx, rd, wanted)
}

// classicLakeSupplyPrewarmSet asks the listing which assets it serves for
// `opts` and reduces the answer with the REQUEST PATH's own candidate
// derivation.
//
// Both halves are the drift guard, and they are the whole reason this function
// exists rather than a list of asset ids. The rows come from
// [Server.listAssetsExtAt], the call handleAssetList makes; the asset_id → SAC
// map comes from [classicLakeSupplyCandidates], the function
// [Server.classicLakeSupply] calls on the rows it is about to answer for; and
// the row → detail projection is [assetDetailFromAssetRow], the one the
// handler uses. Nothing about the population or the cache key is restated
// here, so none of it can drift the way three earlier prewarms in this
// codebase drifted on Order, Sources and Limit — each of which warmed a
// phantom slot while every real request still paid the cold fill.
//
// The handler truncates the overfetch row before it fills market caps, so the
// (Limit+1)th asset of each shape is warmed without being asked about on that
// page. That is a superset, not a phantom: the extra row is the first row of
// the caller's next page, and it costs nothing because it rides in a batch
// that was going to run anyway.
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
