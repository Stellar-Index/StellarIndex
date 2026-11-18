package clickhouse

import (
	"context"
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
)

// BlendReserveState is one reserve's decoded current state and derived metrics (ADR-0039); when DecimalsFound is false the exponent is a placeholder and must never feed a published money figure.
type BlendReserveState struct {
	Pool          string
	Asset         string // reserve underlying token (C-strkey)
	Decimals      uint32
	DecimalsFound bool
	Data          blend.ReserveData
	Metrics       blend.ReserveMetrics
	Config        *blend.ReserveConfig // nil when the reserve's config is not known
}

// BlendPoolState is a lending pool's current state: its instance config, read even when the pool has no reserve state, and its reserves.
type BlendPoolState struct {
	Instance *PoolInstance // nil when the instance entry is missing, undecodable or archived
	Reserves []BlendReserveState
}

// PoolInstance is a lending pool's decoded instance storage: its PoolConfig, admin and backstop ("" when absent).
type PoolInstance struct {
	Config   blend.PoolConfig
	Admin    string
	Backstop string
}

// blendReserveStateQuery is a batched PK-prefix probe of ledger_entries_current's (entry_type, key_xdr) key; do not fold the latest entry
// out of ledger_entry_changes (ResData is rewritten on nearly every pool interaction). FINAL resolves versions ((ledger_seq << 32) |
// intra_ledger_seq); a winning row with empty entry_xdr is a 'removed' change, filtered AFTER the collapse (else an older same-ledger
// update resurrects it), hence optimize_move_to_prewhere_if_final = 0. Pre-intra_ledger_seq rows tie until re-derived. Quiet-reserve
// staleness is bounded by [dropArchivedBlendReserves]; thread/memory pins: see ttlLivenessBatchQuery.
const blendReserveStateQuery = `SELECT key_xdr, entry_xdr
	FROM stellar.ledger_entries_current FINAL
	WHERE entry_type = 'contract_data' AND key_xdr IN (?) AND entry_xdr != ''
	SETTINGS max_threads = 4, max_memory_usage = 8000000000, optimize_move_to_prewhere_if_final = 0`

// BlendPoolReserves reads a pool's CURRENT reserve state (ADR-0039) in one batched query; absence (no ResData, final removal, TTL-archived) means "unavailable", never zero.
func (r *ExplorerReader) BlendPoolReserves(ctx context.Context, pool string, version blend.PoolVersion, assets []string, configs map[string]blend.ReserveConfig) (BlendPoolState, error) {
	// V1 and V2 ResData share field names but not rate scales (off by 10^3 if guessed).
	if version != blend.PoolV1 && version != blend.PoolV2 {
		if _, err := contractIDFromStrkey(pool); err != nil {
			return BlendPoolState{}, fmt.Errorf("clickhouse: blend pool id %q: %w", pool, err)
		}
		return BlendPoolState{}, fmt.Errorf("clickhouse: blend pool %s: unknown pool version %d", pool, version)
	}
	return r.poolReserves(ctx, pool, assets, configs, blendModel(version))
}

// BLNTPoolReserves is BlendPoolReserves for a BLNT V3 pool: the same storage layout, BLNT's supply rate.
func (r *ExplorerReader) BLNTPoolReserves(ctx context.Context, pool string, assets []string, configs map[string]blnt.ReserveConfig) (BlendPoolState, error) {
	return r.poolReserves(ctx, pool, assets, configs, blntModel)
}

// reserveModel is how one protocol decodes ResData and derives APRs from the shared pool storage layout.
type reserveModel struct {
	decode  func(xdr.ScVal) (blend.ReserveData, error)
	metrics func(blend.ReserveData, blend.ReserveConfig, uint32) blend.ReserveMetrics
}

// blntModel is BLNT V3's reserve model: V2-scaled rates, the fee-adjusted supply rate.
var blntModel = reserveModel{decode: blnt.DecodeReserveData, metrics: blnt.Metrics}

func blendModel(version blend.PoolVersion) reserveModel {
	return reserveModel{
		decode:  func(v xdr.ScVal) (blend.ReserveData, error) { return blend.DecodeReserveData(v, version) },
		metrics: blend.Metrics,
	}
}

func (r *ExplorerReader) poolReserves(ctx context.Context, pool string, assets []string, configs map[string]blend.ReserveConfig, model reserveModel) (BlendPoolState, error) {
	poolID, err := contractIDFromStrkey(pool)
	if err != nil {
		return BlendPoolState{}, fmt.Errorf("clickhouse: lending pool id %q: %w", pool, err)
	}
	keys, refByKey, err := blendReserveKeys(poolID, assets)
	if err != nil {
		return BlendPoolState{}, err
	}

	rows, err := r.conn.Query(ctx, blendReserveStateQuery, keys)
	if err != nil {
		return BlendPoolState{}, fmt.Errorf("clickhouse: blend reserves lookup: %w", err)
	}
	defer func() { _ = rows.Close() }()

	dataByAsset, inst, matched, err := scanReserveParts(rows, refByKey, model.decode)
	if err != nil {
		return BlendPoolState{}, err
	}
	// Drop archived entries before anything is reported: the projection keeps a lapsed
	// entry's last value. Verdicts come from the shared SWR cache, so no per-request scan;
	// only a cold cache waits, on the caller's deadline (handler maps it to a 503).
	instArchived, err := dropArchivedBlendReserves(ctx, r.ttlVerdicts, dataByAsset, refByKey, matched)
	if err != nil {
		return BlendPoolState{}, fmt.Errorf("clickhouse: blend reserves ttl liveness: %w", err)
	}
	if instArchived {
		inst = nil
	}
	return BlendPoolState{Instance: inst, Reserves: assembleReserveStates(pool, assets, dataByAsset, configs, inst, model.metrics)}, nil
}

// assembleBlendReserveStates builds states in the caller's asset order. Without the config: config-free metrics and placeholder decimals 7
// (DecimalsFound=false). A nil inst withholds APR (HasAPR=false) rather than serving the gross rate as the supply APR.
func assembleBlendReserveStates(
	pool string, assets []string,
	dataByAsset map[string]*blend.ReserveData,
	configs map[string]blend.ReserveConfig,
	inst *PoolInstance,
) []BlendReserveState {
	return assembleReserveStates(pool, assets, dataByAsset, configs, inst, blend.Metrics)
}

func assembleReserveStates(
	pool string, assets []string,
	dataByAsset map[string]*blend.ReserveData,
	configs map[string]blend.ReserveConfig,
	inst *PoolInstance,
	metricsFn func(blend.ReserveData, blend.ReserveConfig, uint32) blend.ReserveMetrics,
) []BlendReserveState {
	out := make([]BlendReserveState, 0, len(assets))
	for _, asset := range assets {
		rd := dataByAsset[asset]
		if rd == nil {
			continue
		}
		decimals := uint32(7) // decimals-7: placeholder; DecimalsFound=false and every reader gates on it before using Decimals
		decimalsFound := false
		metrics := blend.BaseMetrics(*rd)
		var config *blend.ReserveConfig
		if cfg, ok := configs[asset]; ok {
			decimals = cfg.Decimals
			decimalsFound = true
			config = &cfg
			if inst != nil {
				metrics = metricsFn(*rd, cfg, inst.Config.BstopRate)
			}
		}
		out = append(out, BlendReserveState{
			Pool:          pool,
			Asset:         asset,
			Decimals:      decimals,
			DecimalsFound: decimalsFound,
			Data:          *rd,
			Metrics:       metrics,
			Config:        config,
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
func blendReserveKeys(poolID xdr.ContractId, assets []string) ([]string, map[string]keyRef, error) {
	refByKey := make(map[string]keyRef)
	keys := make([]string, 0, len(assets)+2)
	instanceKeys, err := instanceKeyXDR(xdr.Hash(poolID))
	if err != nil {
		return nil, nil, fmt.Errorf("clickhouse: lending pool instance key: %w", err)
	}
	for _, k := range instanceKeys {
		refByKey[k] = keyRef{kind: "Instance"}
		keys = append(keys, k)
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
	return keys, refByKey, nil
}

type reserveRows interface {
	Next() bool
	Scan(...any) error
	Err() error
}

// scanReserveParts decodes rows into per-asset ResData and the pool instance, and reports the keys that produced a row (matched). The
// TTL filter is scoped to matched: judging keys the lake never returned could over-drop reserves we would have served.
func scanReserveParts(rows reserveRows, refByKey map[string]keyRef, decode func(xdr.ScVal) (blend.ReserveData, error),
) (dataByAsset map[string]*blend.ReserveData, inst *PoolInstance, matched []string, err error) {
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
			if pi, ok := poolInstanceFromStorage(val); ok {
				inst = &pi
			}
			matched = append(matched, keyXDR)
		case "ResData":
			if rd, err := decode(val); err == nil {
				rdCopy := rd
				dataByAsset[ref.asset] = &rdCopy
				matched = append(matched, keyXDR)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("clickhouse: blend reserves rows: %w", err)
	}
	return dataByAsset, inst, matched, nil
}

// dropArchivedBlendReserves removes reserve state whose entry is TTL-archived: ledger_entries_current keeps the last value after Soroban
// evicts, which would price phantom liquidity into tvl_usd. FAIL-OPEN: only a positively lapsed liveUntilLedgerSeq drops; TTLUnknown KEEPS
// the reserve. An archived pool INSTANCE drops every reserve under it and is reported (instArchived).
func dropArchivedBlendReserves(
	ctx context.Context,
	verdicts *ttlLivenessCache,
	dataByAsset map[string]*blend.ReserveData,
	refByKey map[string]keyRef,
	matched []string,
) (instArchived bool, err error) {
	if len(matched) == 0 {
		return false, nil
	}
	liveness, err := verdicts.resolve(ctx, matched)
	if err != nil {
		return false, err
	}
	for _, k := range matched {
		if liveness[k] != TTLArchived {
			continue
		}
		switch refByKey[k].kind {
		case "Instance":
			clear(dataByAsset)
			return true, nil
		case "ResData":
			delete(dataByAsset, refByKey[k].asset)
		}
	}
	return false, nil
}

// contractDataValue returns the ContractData value ScVal of a base64 LedgerEntry.
func contractDataValue(b64 string) (xdr.ScVal, bool) {
	v, _, ok := contractDataEntry(b64)
	return v, ok
}

// contractDataEntry decodes a contract_data LedgerEntry into its value and LastModifiedLedgerSeq.
func contractDataEntry(b64 string) (xdr.ScVal, uint32, bool) {
	var entry xdr.LedgerEntry
	if xdr.SafeUnmarshalBase64(b64, &entry) != nil {
		return xdr.ScVal{}, 0, false
	}
	cd, ok := entry.Data.GetContractData()
	if !ok {
		return xdr.ScVal{}, 0, false
	}
	return cd.Val, uint32(entry.LastModifiedLedgerSeq), true
}

// poolInstanceFromStorage decodes the instance storage's "Config" PoolConfig and "Admin" address. ok=false on a missing or
// undecodable Config: 0 is a legal backstop rate, so a miss must not read as 0 (that serves the gross rate as supply APR).
func poolInstanceFromStorage(val xdr.ScVal) (PoolInstance, bool) {
	inst, isInst := val.GetInstance()
	if !isInst || inst.Storage == nil {
		return PoolInstance{}, false
	}
	var out PoolInstance
	found := false
	for _, e := range *inst.Storage {
		if e.Key.Type != xdr.ScValTypeScvSymbol || e.Key.Sym == nil {
			continue
		}
		switch string(*e.Key.Sym) {
		case "Config":
			pc, err := blend.DecodePoolConfig(e.Val)
			if err != nil {
				return PoolInstance{}, false
			}
			out.Config, found = pc, true
		case "Admin":
			if a, err := scval.AsAddressStrkey(e.Val); err == nil {
				out.Admin = a
			}
		case "Backstop":
			if a, err := scval.AsAddressStrkey(e.Val); err == nil {
				out.Backstop = a
			}
		}
	}
	return out, found
}

// poolDataKeyXDR builds the base64 LedgerKey of a persistent PoolDataKey::<variant>(asset) entry: Vec[Symbol(variant), Address(asset)].
func poolDataKeyXDR(poolID xdr.ContractId, variant string, assetID xdr.ContractId) (string, error) {
	return contractDataKeyXDR(poolID, variant, contractAddressVal(assetID))
}

// contractDataKeyXDR builds the base64 LedgerKey of a persistent `<variant>(arg)` enum key, matching key_xdr verbatim:
// Vec[Symbol(variant), arg].
func contractDataKeyXDR(contract xdr.ContractId, variant string, arg xdr.ScVal) (string, error) {
	sym := xdr.ScSymbol(variant)
	vec := &xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &sym}, arg}
	lk := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &contract},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vec},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	return xdr.MarshalBase64(lk)
}

func contractAddressVal(id xdr.ContractId) xdr.ScVal {
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}
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

// historyModel is the live endpoint's reserve model for a row's protocol.
func historyModel(protocol string) (reserveModel, bool) {
	switch protocol {
	case LendingBlendV1:
		return blendModel(blend.PoolV1), true
	case LendingBlendV2:
		return blendModel(blend.PoolV2), true
	case LendingBLNT:
		return blntModel, true
	}
	return reserveModel{}, false
}

// point decodes one ResData row with the config and instance in force; ok=false when any entry it needs fails to decode.
func (in reserveHistoryIn) point(pool, asset string, row stateRow) (LendingReservePoint, bool) {
	p := LendingReservePoint{CloseTime: row.close.UTC(), StateLedger: row.ledger, Removed: row.entry == ""}
	if p.Removed {
		return p, true
	}
	model, ok := historyModel(row.protocol)
	if !ok {
		return p, false
	}
	val, ok := contractDataValue(row.entry)
	if !ok {
		return p, false
	}
	rd, err := model.decode(val)
	if err != nil {
		return p, false
	}
	var configs map[string]blend.ReserveConfig
	c, cfgUncertain := inForce(in.cfg, row)
	p.ConfigUncertain = cfgUncertain
	if c != nil && c.entry != "" && !cfgUncertain {
		v, ok := contractDataValue(c.entry)
		if !ok {
			return p, false
		}
		cfg, err := blend.DecodeReserveConfig(v)
		if err != nil {
			return p, false
		}
		configs, p.ConfigLedger = map[string]blend.ReserveConfig{asset: cfg}, c.ledger
	}
	inst, ok := in.instance(row)
	if !ok {
		return p, false
	}
	if _, instUncertain := inForce(in.inst, row); instUncertain && configs != nil {
		// The borrow rate needs no pool config; the instance only sets the backstop's share of it, so only SupplyAPR is unknown.
		p.PoolConfigUncertain = true
	}
	st := assembleReserveStates(pool, []string{asset}, map[string]*blend.ReserveData{asset: &rd}, configs, inst, model.metrics)
	p.State = st[0]
	return p, true
}

// instance decodes the pool instance in force at row; nil when none is or it does not decode as a pool, ok=false when its
// entry is not contract data.
func (in reserveHistoryIn) instance(row stateRow) (*PoolInstance, bool) {
	i, _ := inForce(in.inst, row)
	if i == nil || i.entry == "" {
		return nil, true
	}
	v, ok := contractDataValue(i.entry)
	if !ok {
		return nil, false
	}
	if pi, ok := poolInstanceFromStorage(v); ok {
		return &pi, true
	}
	return nil, true
}
