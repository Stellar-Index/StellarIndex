package clickhouse

import (
	"context"
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
)

// BlendReserveState is one Blend reserve's decoded current state +
// derived metrics (ADR-0039), read from the certified lake. ResData
// (the volatile state) is always present; ResConfig (the rate-model
// params + decimals) may not be captured (it's written rarely, often
// before the contract-storage capture window began) — Metrics.HasAPR
// reflects that, and Decimals falls back to 7 (the Stellar/SAC default).
type BlendReserveState struct {
	Pool     string
	Asset    string // reserve underlying token (C-strkey)
	Decimals uint32
	Data     blend.ReserveData
	Metrics  blend.ReserveMetrics
}

// blendReserveStateQuery is the batched current-state lookup for a
// pool's reserve entries — a PK-PREFIX PROBE on
// ledger_entries_current's (entry_type, key_xdr) sort key, bounded by
// construction to (2 instance durabilities + 1 ResData per reserve)
// keys. Identical in shape to the three sibling pool-state readers
// (SoroswapPairReserves, PhoenixPoolReserves, CometPoolReserves), which
// have always read the current-state projection.
//
// This reader used to be the outlier: it folded the LATEST entry per key
// out of stellar.ledger_entry_changes itself, with
//
//	WHERE entry_type = 'contract_data'
//	  AND ledger_seq > (SELECT max(ledger_seq) FROM …) - 250000
//	  AND key_xdr IN (?)
//	GROUP BY key_xdr
//	HAVING argMax(change_type, (ledger_seq, intra_ledger_seq)) != 'removed'
//
// — a 250,000-ledger (~14-day) window over a 150B-row / 6+TiB table. The
// key_xdr bloom skip-index cannot rescue that shape for THIS reader: a
// Blend ResData entry is rewritten on nearly every pool interaction, so
// one reserve's key alone matches tens of thousands of rows scattered
// across the window's granules (74,834 rows for the busiest pool's USDC
// reserve, measured on r1 2026-09-03 — see the fixture note in
// blend_pool_state_reader_test.go). The scan cost is a function of pool
// ACTIVITY over the window, not of how many reserves were asked for, so
// it was a multi-second FLOOR paid by every pool, largest and smallest
// alike (#504: 12.1s timeout on the largest pool, 9.31s on a small one).
//
// ledger_entries_current already holds exactly this answer as one row
// per live key, so the fold is not ours to do:
//
//   - VERSION RESOLUTION is the same composite the old argMax spelled
//     out. The table is ReplacingMergeTree(version) with
//     version = (ledger_seq << 32) | intra_ledger_seq, so FINAL keeps the
//     LAST change in canonical intra-ledger order. That is what
//     audit-2026-07-16 C2-4c requires: a ResData entry is commonly
//     rewritten several times inside ONE ledger, and a ledger_seq-only
//     tie-break serves an arbitrary MID-ledger reserve state.
//   - THE REMOVED-KEY DROP keeps only rows whose entry_xdr is NON-EMPTY,
//     applied to the row FINAL kept. (Spelled without the empty-string
//     literal on purpose: gofumpt's doc-comment reformatter rewrites a
//     doubled apostrophe to a typographic quote, silently, and the
//     result is fmt-STABLE — so the corruption survives every later
//     check. The SQL itself is in a raw string and is unaffected.) A 'removed' change carries only its key (see
//     entryChangeRow), so an empty entry_xdr on the winning row means
//     "this key's final change was a removal". The filter must NOT run
//     before the collapse — filtering removals out first is what let an
//     earlier same-ledger update RESURRECT a deleted key, the exact bug
//     the old HAVING existed to avoid — so
//     optimize_move_to_prewhere_if_final is pinned OFF rather than left
//     to the server default.
//
// max_threads / max_memory_usage are the shared guard rails the sibling
// readers pin (see ttlLivenessBatchQuery): the read is cheap, and a
// planner or layout shift must fail THIS query loudly rather than fan
// out on the shared host.
//
// LEGACY TIE ROWS. intra_ledger_seq is 0 on every row ingested before
// the C2-4c fix and on legacy rows until a full re-derive repopulates
// it, so two same-ledger changes to one key from that era tie under
// FINAL exactly as they tied under the old argMax — the survivor is
// arbitrary. That is not a regression (both shapes resolve it the same
// way), but it is worth knowing WHERE it bites hardest: the population
// this query newly admits — quiet reserves whose last write is old — is
// drawn disproportionately from precisely that era. A reserve last
// touched years ago is more likely to sit on unbroken ties than one
// rewritten this week. The fix is a re-derive of
// ledger_entry_changes.intra_ledger_seq, not a change here.
//
// Reading the projection RETIRES the capture-window caveat — a reserve
// whose ResData had not been touched for 14 days used to be reported
// absent, and a quiet reserve is not a dead one — but the old window was
// also acting as an accidental STALENESS bound, so the read is paired
// with an explicit archived-entry drop. See [dropArchivedBlendReserves].
const blendReserveStateQuery = `SELECT key_xdr, entry_xdr
	FROM stellar.ledger_entries_current FINAL
	WHERE entry_type = 'contract_data' AND key_xdr IN (?) AND entry_xdr != ''
	SETTINGS max_threads = 4, max_memory_usage = 8000000000, optimize_move_to_prewhere_if_final = 0`

// BlendPoolReserves reads the CURRENT reserve STATE for a Blend pool
// from the lake (ADR-0039): the volatile ResData entry (b_rate/d_rate/
// supplies) per reserve asset, plus the pool's instance entry (backstop
// rate), fetched in a SINGLE batched `key_xdr IN (...)` query against
// the current-state projection (see [blendReserveStateQuery]). The
// rate-model CONFIG comes from the caller (`configs`, sourced from
// blend_admin queue_set_reserve events) — the on-chain ResConfig
// storage entry is usually uncaptured (set at reserve init, never
// rewritten), so APY is computed from the event-derived config when
// present, and omitted otherwise (BaseMetrics).
//
// ABSENCE means "reserves unavailable", never zero — the same contract
// the sibling readers publish. A reserve is absent when it has no
// captured ResData, when its final change was a removal, and when its
// entry has been TTL-ARCHIVED (a lapsed pool's last-known reserves are
// not current liquidity; see [dropArchivedBlendReserves]). An archived
// pool INSTANCE takes every reserve under it with it.
func (r *ExplorerReader) BlendPoolReserves(ctx context.Context, pool string, assets []string, configs map[string]blend.ReserveConfig) ([]BlendReserveState, error) {
	poolID, err := contractIDFromStrkey(pool)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: blend pool id %q: %w", pool, err)
	}

	keys, refByKey := blendReserveKeys(poolID, assets)
	if len(keys) == 0 {
		return nil, nil
	}

	rows, err := r.conn.Query(ctx, blendReserveStateQuery, keys)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: blend reserves lookup: %w", err)
	}
	defer func() { _ = rows.Close() }()

	dataByAsset, bstop, matched, err := scanBlendReserveParts(rows, refByKey)
	if err != nil {
		return nil, err
	}
	// Archived entries are dropped BEFORE anything here becomes a reported
	// figure — the projection keeps a lapsed entry's last-known value, and
	// the handler prices whatever it is handed. See
	// [dropArchivedBlendReserves].
	//
	// This does not reintroduce a per-request scan: verdicts come from the
	// shared stale-while-revalidate cache (the same one SoroswapPairReserves
	// uses), whose recompute is detached and whose underlying read is a
	// primary-key lookup on the slim ttl_live_until projection since v0.21.4.
	// Only a COLD cache waits, and it waits on the caller's deadline — which
	// the wrapping here maps to the handler's retryable `lending-timeout` 503
	// (handlerTimedOut unwraps DeadlineExceeded) rather than a 500.
	if err := dropArchivedBlendReserves(ctx, r.ttlVerdicts, dataByAsset, refByKey, matched); err != nil {
		return nil, fmt.Errorf("clickhouse: blend reserves ttl liveness: %w", err)
	}

	// Assemble in the caller's asset order. ResData (the state) is
	// mandatory; the rate-model config is optional — with it we report
	// APY + the real decimals, without it supplied/borrowed/utilization
	// (config-free) + default decimals 7, APY omitted (HasAPR=false).
	out := make([]BlendReserveState, 0, len(assets))
	for _, asset := range assets {
		rd := dataByAsset[asset]
		if rd == nil {
			continue
		}
		decimals := uint32(7)
		var metrics blend.ReserveMetrics
		if cfg, ok := configs[asset]; ok {
			decimals = cfg.Decimals
			metrics = blend.Metrics(*rd, cfg, bstop)
		} else {
			metrics = blend.BaseMetrics(*rd)
		}
		out = append(out, BlendReserveState{
			Pool:     pool,
			Asset:    asset,
			Decimals: decimals,
			Data:     *rd,
			Metrics:  metrics,
		})
	}
	return out, nil
}

// keyRef maps a built storage key back to what it is.
type keyRef struct {
	asset string
	kind  string // "ResData" | "ResConfig" | "Instance"
}

// blendReserveKeys builds the storage keys BlendPoolReserves fetches —
// the pool instance entry (for the backstop rate) + the ResData entry
// per reserve asset — and a reverse index from key to (asset, kind).
// (ResConfig is NOT fetched from the lake; the rate config comes from
// blend_admin events — see BlendPoolReserves.)
func blendReserveKeys(poolID xdr.ContractId, assets []string) ([]string, map[string]keyRef) {
	refByKey := make(map[string]keyRef)
	keys := make([]string, 0, len(assets)+2)
	if instanceKeys, err := instanceKeyXDR(xdr.Hash(poolID)); err == nil {
		for _, k := range instanceKeys {
			refByKey[k] = keyRef{kind: "Instance"}
			keys = append(keys, k)
		}
	}
	for _, asset := range assets {
		assetID, err := contractIDFromStrkey(asset)
		if err != nil {
			continue
		}
		k, err := poolDataKeyXDR(poolID, "ResData", assetID)
		if err != nil {
			continue
		}
		refByKey[k] = keyRef{asset: asset, kind: "ResData"}
		keys = append(keys, k)
	}
	return keys, refByKey
}

// scanBlendReserveParts decodes the batched lookup's rows into the
// per-asset ResData + the pool's backstop rate, and reports which of the
// requested keys actually produced a row (`matched`).
//
// `matched` is what the TTL-liveness filter is scoped to. Judging keys
// the lake never returned would let a TTL row with no companion
// contract_data entry drop reserves the reader would otherwise have
// served — an over-drop, the mirror of the phantom-liquidity bug and
// harder to notice. Only entries we are actually about to REPORT get
// classified.
func scanBlendReserveParts(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}, refByKey map[string]keyRef,
) (dataByAsset map[string]*blend.ReserveData, bstop uint32, matched []string, err error) {
	dataByAsset = make(map[string]*blend.ReserveData)
	for rows.Next() {
		var keyXDR, b64 string
		if err := rows.Scan(&keyXDR, &b64); err != nil {
			return nil, 0, nil, fmt.Errorf("clickhouse: scan blend reserve: %w", err)
		}
		ref, ok := refByKey[keyXDR]
		if !ok {
			continue
		}
		val, ok := contractDataValue(b64)
		if !ok {
			continue
		}
		switch ref.kind {
		case "Instance":
			bstop = backstopRateFromInstance(val)
			matched = append(matched, keyXDR)
		case "ResData":
			if rd, err := blend.DecodeReserveData(val); err == nil {
				rdCopy := rd
				dataByAsset[ref.asset] = &rdCopy
				matched = append(matched, keyXDR)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, nil, fmt.Errorf("clickhouse: blend reserves rows: %w", err)
	}
	return dataByAsset, bstop, matched, nil
}

// dropArchivedBlendReserves removes reserve state whose lake entry has
// been TTL-ARCHIVED — the staleness bound this reader owes its callers,
// and the reason the three sibling pool-state readers are not parity in
// SHAPE alone (SoroswapPairReserves → dropArchivedPairs,
// PhoenixPoolReserves / CometPoolReserves → ClassifyTTLLiveness).
//
// Soroban evicts a contract_data entry once its TTL lapses, but
// ledger_entries_current keeps its last-known value forever. An archived
// reserve therefore reads as a perfectly good ResData row, and the API
// multiplies it by today's USD price into `tvl_usd` and stamps the
// current watermark with flags.stale=false — a fabricated TVL for a dead
// pool, published as current. This is the same phantom-liquidity class
// dropArchivedPairs documents ("phantom depth on every surface that
// consumes this") and that PHO's +157% supply divergence came from.
//
// It also replaces a bound that used to exist by accident. The
// pre-#504 250,000-ledger window was doing DOUBLE DUTY: an archived
// entry has had no writes since it lapsed, so the window dropped it as a
// side effect of being narrow. Reading the current-state projection
// removes that window — correctly, since a QUIET reserve is not a dead
// one — so the staleness bound has to be stated explicitly rather than
// inherited from a scan bound.
//
// FAIL-OPEN, exactly as the siblings: only a positively-resolved lapsed
// liveUntilLedgerSeq drops anything. A key with no TTL row, an
// unrecognised TTL wire shape, or a reader built without a verdict cache
// (tests) all yield TTLUnknown, which KEEPS the reserve — under-reporting
// live liquidity is the same class of error in the other direction, and
// a silent over-drop is harder to spot than a residual over-count.
//
// An archived pool INSTANCE drops every reserve under it: the pool
// contract itself is no longer live ledger state, so its reserves cannot
// be current liquidity whatever their own TTLs say. Same "ANY archived
// key sinks the pool" rule as dropArchivedPhoenixPools.
func dropArchivedBlendReserves(
	ctx context.Context,
	verdicts *ttlLivenessCache,
	dataByAsset map[string]*blend.ReserveData,
	refByKey map[string]keyRef,
	matched []string,
) error {
	if len(dataByAsset) == 0 || len(matched) == 0 {
		return nil
	}
	liveness, err := verdicts.resolve(ctx, matched)
	if err != nil {
		return err
	}
	for _, k := range matched {
		if liveness[k] != TTLArchived {
			continue
		}
		switch refByKey[k].kind {
		case "Instance":
			clear(dataByAsset)
			return nil
		case "ResData":
			delete(dataByAsset, refByKey[k].asset)
		}
	}
	return nil
}

// contractDataValue unmarshals a base64 LedgerEntry and returns its
// ContractData value ScVal.
func contractDataValue(b64 string) (xdr.ScVal, bool) {
	var entry xdr.LedgerEntry
	if xdr.SafeUnmarshalBase64(b64, &entry) != nil {
		return xdr.ScVal{}, false
	}
	cd, ok := entry.Data.GetContractData()
	if !ok {
		return xdr.ScVal{}, false
	}
	return cd.Val, true
}

// backstopRateFromInstance pulls PoolConfig.bstop_rate (7 decimals)
// from a contract instance entry's storage map (Symbol "Config"). 0 on
// any miss → the supply-APR is then the gross rate (backstop take
// unaccounted).
func backstopRateFromInstance(val xdr.ScVal) uint32 {
	inst, ok := val.GetInstance()
	if !ok || inst.Storage == nil {
		return 0
	}
	for _, e := range *inst.Storage {
		if e.Key.Type == xdr.ScValTypeScvSymbol && e.Key.Sym != nil && string(*e.Key.Sym) == "Config" {
			if pc, err := blend.DecodePoolConfig(e.Val); err == nil {
				return pc.BstopRate
			}
			return 0
		}
	}
	return 0
}

// poolDataKeyXDR builds the base64 LedgerKey for a Blend
// PoolDataKey::<variant>(asset) persistent contract_data entry under
// the pool contract — matching the `key_xdr` column verbatim. The
// #[contracttype] enum variant encodes as Vec[Symbol(variant),
// Address(asset)].
func poolDataKeyXDR(poolID xdr.ContractId, variant string, assetID xdr.ContractId) (string, error) {
	pid := poolID
	sym := xdr.ScSymbol(variant)
	assetAddr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &assetID}
	vec := &xdr.ScVec{
		{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
		{Type: xdr.ScValTypeScvAddress, Address: &assetAddr},
	}
	key := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vec}
	lk := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &pid},
			Key:        key,
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	return xdr.MarshalBase64(lk)
}

// contractIDFromStrkey decodes a C-strkey into an xdr.ContractId.
func contractIDFromStrkey(c string) (xdr.ContractId, error) {
	raw, err := strkey.Decode(strkey.VersionByteContract, c)
	if err != nil {
		return xdr.ContractId{}, fmt.Errorf("not a contract strkey: %w", err)
	}
	var id xdr.ContractId
	copy(id[:], raw)
	return id, nil
}
