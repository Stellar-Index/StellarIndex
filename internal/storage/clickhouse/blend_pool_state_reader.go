package clickhouse

import (
	"context"
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
)

// BlendReserveState is one reserve's decoded current state and derived metrics
// (ADR-0039). ResData is always present; ResConfig (rate model, decimals) may be
// uncaptured, so Metrics.HasAPR reports it and Decimals falls back to 7.
//
// DecimalsFound reports whether Decimals came from the reserve's config.
// When false the exponent is a placeholder: a caller must not publish a
// decimalised money figure from it (see [ContractStorageSupply.DecimalsFound]).
type BlendReserveState struct {
	Pool          string
	Asset         string // reserve underlying token (C-strkey)
	Decimals      uint32
	DecimalsFound bool
	Data          blend.ReserveData
	Metrics       blend.ReserveMetrics
}

// blendReserveStateQuery is a batched PK-prefix probe of ledger_entries_current's
// (entry_type, key_xdr) sort key, like the sibling pool-state readers. Do not fold the
// latest entry per key out of ledger_entry_changes: a ResData entry is rewritten on
// nearly every pool interaction, so scan cost scales with pool activity.
// FINAL resolves versions: version = (ledger_seq << 32) | intra_ledger_seq, so the
// last change in intra-ledger order wins. A 'removed' change carries only its key, so
// a winning row with empty entry_xdr is a removal; that filter must run AFTER the
// collapse (before it, an older same-ledger update resurrects a deleted key), hence
// optimize_move_to_prewhere_if_final = 0. Rows stamped before intra_ledger_seq existed
// tie arbitrarily until it is re-derived. Quiet reserves are not dropped by a scan
// window, so staleness is bounded by [dropArchivedBlendReserves]. The thread and
// memory pins are the siblings' guard rails (see ttlLivenessBatchQuery).
const blendReserveStateQuery = `SELECT key_xdr, entry_xdr
	FROM stellar.ledger_entries_current FINAL
	WHERE entry_type = 'contract_data' AND key_xdr IN (?) AND entry_xdr != ''
	SETTINGS max_threads = 4, max_memory_usage = 8000000000, optimize_move_to_prewhere_if_final = 0`

// BlendPoolReserves reads the CURRENT reserve state for a pool (ADR-0039): ResData per
// asset plus the instance entry (backstop rate) in one batched query. The rate-model
// config comes from the caller (blend_admin queue_set_reserve events); without it
// APY is omitted (BaseMetrics). version fixes the ResData rate scale; unknown is an
// error. ABSENCE means "unavailable", never zero: no ResData, a final removal, or a
// TTL-archived entry (an archived pool instance takes all its reserves).
func (r *ExplorerReader) BlendPoolReserves(ctx context.Context, pool string, version blend.PoolVersion, assets []string, configs map[string]blend.ReserveConfig) ([]BlendReserveState, error) {
	poolID, err := contractIDFromStrkey(pool)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: blend pool id %q: %w", pool, err)
	}
	// V1 and V2 ResData share field names but not rate scales (off by 10^3 if guessed).
	if version != blend.PoolV1 && version != blend.PoolV2 {
		return nil, fmt.Errorf("clickhouse: blend pool %s: unknown pool version %d", pool, version)
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

	dataByAsset, bstop, matched, err := scanBlendReserveParts(rows, refByKey, version)
	if err != nil {
		return nil, err
	}
	// Drop archived entries before anything is reported: the projection keeps a lapsed
	// entry's last value. Verdicts come from the shared SWR cache, so no per-request scan;
	// only a cold cache waits, on the caller's deadline (handler maps it to a 503).
	if err := dropArchivedBlendReserves(ctx, r.ttlVerdicts, dataByAsset, refByKey, matched); err != nil {
		return nil, fmt.Errorf("clickhouse: blend reserves ttl liveness: %w", err)
	}

	return assembleBlendReserveStates(pool, assets, dataByAsset, configs, bstop), nil
}

// assembleBlendReserveStates builds states in the caller's asset order. Without the
// config: config-free metrics and placeholder decimals 7 (DecimalsFound=false). APR
// also needs the backstop rate: a nil bstop withholds it (HasAPR=false) rather than
// serving the gross rate as the supply APR.
func assembleBlendReserveStates(
	pool string, assets []string,
	dataByAsset map[string]*blend.ReserveData,
	configs map[string]blend.ReserveConfig,
	bstop *uint32,
) []BlendReserveState {
	out := make([]BlendReserveState, 0, len(assets))
	for _, asset := range assets {
		rd := dataByAsset[asset]
		if rd == nil {
			continue
		}
		decimals := uint32(7)
		decimalsFound := false
		metrics := blend.BaseMetrics(*rd)
		if cfg, ok := configs[asset]; ok {
			decimals = cfg.Decimals
			decimalsFound = true
			if bstop != nil {
				metrics = blend.Metrics(*rd, cfg, *bstop)
			}
		}
		out = append(out, BlendReserveState{
			Pool:          pool,
			Asset:         asset,
			Decimals:      decimals,
			DecimalsFound: decimalsFound,
			Data:          *rd,
			Metrics:       metrics,
		})
	}
	return out
}

// keyRef maps a built storage key back to what it is.
type keyRef struct {
	asset string
	kind  string // "ResData" | "ResConfig" | "Instance"
}

// blendReserveKeys builds the keys to fetch (instance entry plus ResData per asset)
// and a key -> (asset, kind) index. ResConfig is not fetched.
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

// scanBlendReserveParts decodes rows into per-asset ResData and the backstop rate, and
// reports the keys that produced a row (matched). The TTL filter is scoped to matched:
// judging keys the lake never returned could over-drop reserves we would have served.
func scanBlendReserveParts(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}, refByKey map[string]keyRef, version blend.PoolVersion,
) (dataByAsset map[string]*blend.ReserveData, bstop *uint32, matched []string, err error) {
	dataByAsset = make(map[string]*blend.ReserveData)
	for rows.Next() {
		var keyXDR, b64 string
		if err := rows.Scan(&keyXDR, &b64); err != nil {
			return nil, nil, nil, fmt.Errorf("clickhouse: scan blend reserve: %w", err)
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
			if rate, ok := backstopRateFromInstance(val); ok {
				bstop = &rate
			}
			matched = append(matched, keyXDR)
		case "ResData":
			if rd, err := blend.DecodeReserveData(val, version); err == nil {
				rdCopy := rd
				dataByAsset[ref.asset] = &rdCopy
				matched = append(matched, keyXDR)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("clickhouse: blend reserves rows: %w", err)
	}
	return dataByAsset, bstop, matched, nil
}

// dropArchivedBlendReserves removes reserve state whose entry is TTL-archived. Soroban
// evicts an entry once its TTL lapses but ledger_entries_current keeps its last value,
// so an archived reserve would be priced into tvl_usd as live: phantom liquidity (see
// dropArchivedPairs). FAIL-OPEN: only a positively lapsed liveUntilLedgerSeq drops
// anything; no TTL row, unknown wire shape or no verdict cache yields TTLUnknown and
// KEEPS the reserve. An archived pool INSTANCE drops every reserve under it (same
// rule as dropArchivedPhoenixPools).
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

// contractDataValue returns the ContractData value ScVal of a base64 LedgerEntry.
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

// backstopRateFromInstance pulls PoolConfig.bstop_rate (7 decimals) from the instance
// storage map. ok=false on a missing Config: 0 is a legal rate, so a miss must not
// read as 0 (that serves the gross rate as supply APR).
func backstopRateFromInstance(val xdr.ScVal) (rate uint32, ok bool) {
	inst, isInst := val.GetInstance()
	if !isInst || inst.Storage == nil {
		return 0, false
	}
	for _, e := range *inst.Storage {
		if e.Key.Type == xdr.ScValTypeScvSymbol && e.Key.Sym != nil && string(*e.Key.Sym) == "Config" {
			pc, err := blend.DecodePoolConfig(e.Val)
			if err != nil {
				return 0, false
			}
			return pc.BstopRate, true
		}
	}
	return 0, false
}

// poolDataKeyXDR builds the base64 LedgerKey of a persistent PoolDataKey::<variant>
// (asset) entry, matching key_xdr verbatim: Vec[Symbol(variant), Address(asset)].
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
