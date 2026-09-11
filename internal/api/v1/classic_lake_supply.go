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

	out, missing, flight := s.readClassicLakeSupply(rd, wanted)
	// Wait only on a FULLY cold answer — a warm cache never blocks a request,
	// and a partially warm one is already better than the fallback.
	if len(out) > 0 || len(missing) == 0 || flight == nil {
		return out
	}
	timer := time.NewTimer(classicLakeSupplyColdWait)
	defer timer.Stop()
	select {
	case <-flight:
		warmed, _, _ := s.readClassicLakeSupply(rd, wanted)
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
	missing := make([]string, 0, len(wanted))
	for assetID := range wanted {
		entry, cached := s.lakeSupply[assetID]
		if cached && time.Since(entry.at) < classicLakeSupplyTTL {
			if entry.value != "" {
				out[assetID] = entry.value
			}
			continue
		}
		missing = append(missing, assetID)
	}
	if len(missing) == 0 {
		return out, nil, nil
	}
	sort.Strings(missing) // deterministic batching across concurrent requests

	if s.lakeSupplyFlight != nil {
		return out, missing, s.lakeSupplyFlight
	}
	if time.Since(s.lakeSupplyAttemptAt) < classicLakeSupplyRetryGap {
		return out, missing, nil
	}
	batch := make(map[string]string, classicLakeSupplyBatch)
	for _, assetID := range missing {
		if len(batch) >= classicLakeSupplyBatch {
			break
		}
		batch[assetID] = wanted[assetID]
	}
	s.lakeSupplyAttemptAt = time.Now() // advances on failure too
	flight := make(chan struct{})
	s.lakeSupplyFlight = flight
	// G118 is the intended behaviour, not a defect: detaching from the
	// request context is what keeps a slow lake sum from being killed at the
	// request deadline and retried, unbounded, by the next caller.
	go s.refreshClassicLakeSupply(rd, batch, flight) //nolint:gosec,contextcheck // G118 + contextcheck: the detachment is deliberate — see this function's doc.
	return out, missing, flight
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
