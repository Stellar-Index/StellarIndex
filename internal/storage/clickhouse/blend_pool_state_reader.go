package clickhouse

import (
	"context"
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/StellarIndex/stellar-index/internal/sources/blend"
)

// BlendReserveState is one Blend reserve's decoded current state +
// derived metrics (ADR-0039), read from the certified lake.
type BlendReserveState struct {
	Pool    string
	Asset   string // reserve underlying token (C-strkey)
	Data    blend.ReserveData
	Config  blend.ReserveConfig
	Metrics blend.ReserveMetrics
}

// BlendPoolReserves reads the CURRENT reserve state for a Blend pool
// from the lake (ADR-0039): for each reserve asset it point-looks-up
// the latest ResData + ResConfig contract_data entries by exact
// key_xdr, decodes them, reads the pool's backstop take rate from
// instance storage, and derives TVL/utilisation/APY metrics. Assets
// the caller doesn't supply, or reserves with no captured entry, are
// simply absent from the result.
func (r *ExplorerReader) BlendPoolReserves(ctx context.Context, pool string, assets []string) ([]BlendReserveState, error) {
	poolID, err := contractIDFromStrkey(pool)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: blend pool id %q: %w", pool, err)
	}
	bstop := r.blendBackstopRate(ctx, poolID) // 0 when unavailable → supply APR = gross

	out := make([]BlendReserveState, 0, len(assets))
	for _, asset := range assets {
		assetID, err := contractIDFromStrkey(asset)
		if err != nil {
			continue // skip non-contract reserve identifiers
		}
		dataKey, err := poolDataKeyXDR(poolID, "ResData", assetID)
		if err != nil {
			continue
		}
		cfgKey, err := poolDataKeyXDR(poolID, "ResConfig", assetID)
		if err != nil {
			continue
		}
		dataVal, ok, err := r.latestContractDataVal(ctx, dataKey)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		cfgVal, ok, err := r.latestContractDataVal(ctx, cfgKey)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		rd, err := blend.DecodeReserveData(dataVal)
		if err != nil {
			continue
		}
		rc, err := blend.DecodeReserveConfig(cfgVal)
		if err != nil {
			continue
		}
		out = append(out, BlendReserveState{
			Pool:    pool,
			Asset:   asset,
			Data:    rd,
			Config:  rc,
			Metrics: blend.Metrics(rd, rc, bstop),
		})
	}
	return out, nil
}

// blendBackstopRate reads the pool's PoolConfig.bstop_rate (7 decimals)
// from the contract INSTANCE storage map (Symbol "Config"). Returns 0
// on any miss — the supply-APR computation then reports the gross rate
// (the caller can note the backstop take is unaccounted).
func (r *ExplorerReader) blendBackstopRate(ctx context.Context, poolID xdr.ContractId) uint32 {
	keys, err := instanceKeyXDR(xdr.Hash(poolID))
	if err != nil {
		return 0
	}
	const q = `SELECT entry_xdr FROM stellar.ledger_entry_changes
		WHERE entry_type = 'contract_data' AND key_xdr IN (?) AND entry_xdr != ''
		ORDER BY ledger_seq DESC, ingested_at DESC LIMIT 1`
	rows, err := r.conn.Query(ctx, q, keys)
	if err != nil {
		return 0
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return 0
	}
	var b64 string
	if err := rows.Scan(&b64); err != nil {
		return 0
	}
	var entry xdr.LedgerEntry
	if xdr.SafeUnmarshalBase64(b64, &entry) != nil {
		return 0
	}
	cd, ok := entry.Data.GetContractData()
	if !ok {
		return 0
	}
	inst, ok := cd.Val.GetInstance()
	if !ok || inst.Storage == nil {
		return 0
	}
	for _, e := range *inst.Storage {
		if e.Key.Type == xdr.ScValTypeScvSymbol && e.Key.Sym != nil && string(*e.Key.Sym) == "Config" {
			pc, err := blend.DecodePoolConfig(e.Val)
			if err != nil {
				return 0
			}
			return pc.BstopRate
		}
	}
	return 0
}

// latestContractDataVal point-looks-up the newest contract_data entry
// matching keyXDR and returns its decoded value ScVal. ok=false when no
// entry matches (reserve not captured / pruned).
func (r *ExplorerReader) latestContractDataVal(ctx context.Context, keyXDR string) (xdr.ScVal, bool, error) {
	const q = `SELECT entry_xdr FROM stellar.ledger_entry_changes
		WHERE entry_type = 'contract_data' AND key_xdr = ? AND entry_xdr != ''
		ORDER BY ledger_seq DESC, ingested_at DESC LIMIT 1`
	rows, err := r.conn.Query(ctx, q, keyXDR)
	if err != nil {
		return xdr.ScVal{}, false, fmt.Errorf("clickhouse: contract_data lookup: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return xdr.ScVal{}, false, rows.Err()
	}
	var b64 string
	if err := rows.Scan(&b64); err != nil {
		return xdr.ScVal{}, false, fmt.Errorf("clickhouse: scan contract_data: %w", err)
	}
	var entry xdr.LedgerEntry
	if xdr.SafeUnmarshalBase64(b64, &entry) != nil {
		return xdr.ScVal{}, false, nil
	}
	cd, ok := entry.Data.GetContractData()
	if !ok {
		return xdr.ScVal{}, false, nil
	}
	return cd.Val, true, nil
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
