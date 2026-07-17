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

// BlendPoolReserves reads the CURRENT reserve STATE for a Blend pool
// from the lake (ADR-0039): the volatile ResData entry (b_rate/d_rate/
// supplies) per reserve asset, plus the pool's instance entry (backstop
// rate), fetched in a SINGLE batched `key_xdr IN (...)` query. The
// rate-model CONFIG comes from the caller (`configs`, sourced from
// blend_admin queue_set_reserve events) — the on-chain ResConfig
// storage entry is usually uncaptured (set at reserve init, never
// rewritten), so APY is computed from the event-derived config when
// present, and omitted otherwise (BaseMetrics). Reserves with no
// captured ResData are absent.
func (r *ExplorerReader) BlendPoolReserves(ctx context.Context, pool string, assets []string, configs map[string]blend.ReserveConfig) ([]BlendReserveState, error) {
	poolID, err := contractIDFromStrkey(pool)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: blend pool id %q: %w", pool, err)
	}

	keys, refByKey := blendReserveKeys(poolID, assets)
	if len(keys) == 0 {
		return nil, nil
	}

	// One batched lookup: latest entry_xdr per key (argMax over the
	// ReplacingMergeTree versions), bounded to a recent ledger window
	// (partition-pruned, intDiv 1M). The key_xdr bloom skip-index exists
	// now, but ResData is REWRITTEN on nearly every pool interaction, so
	// its key sits in thousands of scattered granules — the bloom prunes
	// little (unbounded ~15s) while the window bound stays ~6s. (The
	// bloom's big win is rarely-written keys: the wasm-hash / code-history
	// readers' instance keys went ~21s → ~0.7s.) An active pool's latest
	// reserve state is always in-window; a reserve untouched for longer
	// is reported as absent (consistent with "captured window").
	//
	// The version key is the COMPOSITE (ledger_seq, intra_ledger_seq), not
	// ledger_seq alone (audit-2026-07-16 C2-4c): a Blend ResData entry is
	// commonly rewritten several times within ONE ledger (multiple pool ops in
	// the same close), and ledger_seq ties those rows so a single-column argMax
	// served an ARBITRARY mid-ledger reserve state (stale b_rate/d_rate/
	// supplies). intra_ledger_seq — the canonical within-ledger walk position —
	// breaks the tie so the LAST change wins. The removed-key drop moves from a
	// pre-aggregation `entry_xdr != ''` filter (which excluded a same-ledger
	// removal from the argMax and could thus RESURRECT a key deleted later in
	// the same ledger) to a HAVING on the WINNING row's change_type, so a key
	// whose final intra-ledger change is a removal correctly drops out.
	const reserveWindowLedgers = 250_000 // ~14 days at 5s
	const q = `SELECT key_xdr, argMax(entry_xdr, (ledger_seq, intra_ledger_seq)) AS latest_xdr
		FROM stellar.ledger_entry_changes
		WHERE entry_type = 'contract_data'
		  AND ledger_seq > (SELECT max(ledger_seq) FROM stellar.ledger_entry_changes) - ?
		  AND key_xdr IN (?)
		GROUP BY key_xdr
		HAVING argMax(change_type, (ledger_seq, intra_ledger_seq)) != 'removed'`
	rows, err := r.conn.Query(ctx, q, uint32(reserveWindowLedgers), keys)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: blend reserves lookup: %w", err)
	}
	defer func() { _ = rows.Close() }()

	dataByAsset, bstop, err := scanBlendReserveParts(rows, refByKey)
	if err != nil {
		return nil, err
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
// per-asset ResData + the pool's backstop rate.
func scanBlendReserveParts(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}, refByKey map[string]keyRef,
) (map[string]*blend.ReserveData, uint32, error) {
	dataByAsset := make(map[string]*blend.ReserveData)
	var bstop uint32
	for rows.Next() {
		var keyXDR, b64 string
		if err := rows.Scan(&keyXDR, &b64); err != nil {
			return nil, 0, fmt.Errorf("clickhouse: scan blend reserve: %w", err)
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
		case "ResData":
			if rd, err := blend.DecodeReserveData(val); err == nil {
				rdCopy := rd
				dataByAsset[ref.asset] = &rdCopy
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("clickhouse: blend reserves rows: %w", err)
	}
	return dataByAsset, bstop, nil
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
