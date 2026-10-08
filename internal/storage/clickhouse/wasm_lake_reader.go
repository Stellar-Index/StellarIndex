package clickhouse

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// ErrContractWasmUnresolved: the contract's INSTANCE entry or its contract_code entry is not in the
// lake. A clean 404, not an error. contract_code is fully captured, so a code-hop miss means the
// hash is genuinely absent; the instance hop is the one that still misses.
var ErrContractWasmUnresolved = errors.New("clickhouse: contract wasm not resolvable from lake")

// ErrContractIsSAC: the instance is captured but its executable is a Stellar Asset Contract, which
// has no WASM, ever. Distinct from ErrContractWasmUnresolved so callers can say "SAC" rather than
// "not captured".
var ErrContractIsSAC = errors.New("clickhouse: contract is a stellar asset contract (no wasm)")

// WasmExport is one exported function of a Soroban contract. Types are the low-level wasm ABI; the
// NAMES are the contract's real API surface.
type WasmExport struct {
	Name    string   // exported symbol
	Params  []string // wasm value types: "i32"|"i64"|"f32"|"f64"
	Results []string // wasm value types
}

// ContractWasmInfo is the assembled per-contract wasm view. Wat/Decompiled are empty when wabt is
// not on PATH; hash, size and exports are always populated.
type ContractWasmInfo struct {
	ContractID string
	WasmHash   string // hex sha256 of the wasm module
	SizeBytes  int
	Exports    []WasmExport
	Wat        string // WAT disassembly; empty if wasm2wat unavailable
	Decompiled string // wasm-decompile pseudocode; empty if unavailable
	ToolNote   string // human note on which optional stages ran / why they didn't
}

// ContractWasm resolves a contract id to its wasm over ledger_entry_changes (ADR-0034): contract id
// -> wasm hash (INSTANCE entry), then hash -> bytes (contract_code entry). Exports are parsed
// natively; WAT and decompile are best-effort. Returns ErrContractWasmUnresolved when either hop
// misses.
func (r *ExplorerReader) ContractWasm(ctx context.Context, contractID string) (ContractWasmInfo, error) {
	wasmHash, err := r.resolveContractWasmHash(ctx, contractID)
	if err != nil {
		return ContractWasmInfo{}, err
	}

	info, err := r.wasmModuleView(ctx, wasmHash)
	if err != nil {
		return ContractWasmInfo{}, err
	}
	info.ContractID = contractID
	return info, nil
}

// ContractWasmHash is ContractWasm's first hop alone: the contract's CURRENT
// wasm hash as lower hex, without reading or disassembling the module.
// Returns ErrContractIsSAC / ErrContractWasmUnresolved exactly as ContractWasm.
func (r *ExplorerReader) ContractWasmHash(ctx context.Context, contractID string) (string, error) {
	h, err := r.resolveContractWasmHash(ctx, contractID)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h[:]), nil
}

func (r *ExplorerReader) resolveContractWasmHash(ctx context.Context, contractID string) (xdr.Hash, error) {
	dec, err := strkey.Decode(strkey.VersionByteContract, contractID)
	if err != nil {
		return xdr.Hash{}, fmt.Errorf("clickhouse: bad contract id %q: %w", contractID, err)
	}
	var cidHash xdr.Hash
	copy(cidHash[:], dec)

	wasmHash, ok, err := r.contractWasmHash(ctx, cidHash)
	if err != nil {
		return xdr.Hash{}, err
	}
	if !ok {
		return xdr.Hash{}, ErrContractWasmUnresolved
	}
	return wasmHash, nil
}

// wasmModuleFlightTimeout bounds one shared per-hash fill: the code read plus
// both wabt runs (wasmToolTimeout each).
const wasmModuleFlightTimeout = 30 * time.Second

var errWasmModuleFillPanicked = errors.New("clickhouse: wasm module fill panicked")

// wasmModuleView assembles everything keyed by the wasm hash. The contract->hash hop stays per
// request (upgrades move it); hash->bytes->disassembly is immutable, so it is memoised and run as
// one detached flight per hash that no single waiter can cancel.
func (r *ExplorerReader) wasmModuleView(ctx context.Context, wasmHash xdr.Hash) (ContractWasmInfo, error) {
	key := hex.EncodeToString(wasmHash[:])
	//nolint:contextcheck // the fill is shared by every waiter, so no single caller's cancellation may abort it
	ch := r.wasmFlight.DoChan(key, func() (val any, err error) {
		// singleflight re-raises a panic on a goroutine nothing can recover.
		defer func() {
			if rec := recover(); rec != nil {
				worker.Report(nil, "explorer-wasm-module-fill", rec)
				val, err = nil, errWasmModuleFillPanicked
			}
		}()
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wasmModuleFlightTimeout)
		defer cancel()
		return r.fillWasmModule(fctx, wasmHash, key)
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return ContractWasmInfo{}, res.Err
		}
		info, _ := res.Val.(ContractWasmInfo)
		return info, nil
	case <-ctx.Done():
		return ContractWasmInfo{}, ctx.Err()
	}
}

func (r *ExplorerReader) fillWasmModule(ctx context.Context, wasmHash xdr.Hash, key string) (ContractWasmInfo, error) {
	mod, ok := r.moduleCache.get(key)
	if !ok {
		code, found, err := r.wasmCodeByHash(ctx, wasmHash)
		if err != nil {
			return ContractWasmInfo{}, err
		}
		if !found {
			return ContractWasmInfo{}, ErrContractWasmUnresolved
		}
		exports, perr := parseWasmExports(code)
		mod = wasmModuleEntry{code: code, exports: exports}
		if perr != nil {
			// A parse miss is non-fatal: still serve the resolved hash + size.
			mod.parseNote = "export parse: " + perr.Error() + "; "
		}
		r.moduleCache.put(key, mod, time.Now())
	}
	info := ContractWasmInfo{
		WasmHash:  key,
		SizeBytes: len(mod.code),
		Exports:   mod.exports,
		ToolNote:  mod.parseNote,
	}
	r.buildWasmDisassembly(ctx, &info, mod.code)
	return info, nil
}

// contractWasmHash reads the contract's executable wasm hash; ok=false when no instance entry is
// captured. It matches the fixed instance key_xdr (instanceKeyXDR) rather than decoding every
// contract_data row, newest-first so in-place upgrades win.
func (r *ExplorerReader) contractWasmHash(ctx context.Context, cid xdr.Hash) (xdr.Hash, bool, error) {
	// Index-first: the genesis-complete contract_instance_changes timeline resolves contracts whose
	// instance entry predates live capture.
	if r.instanceChangesIndexAvailable(ctx) {
		h, ok, err := r.contractWasmHashIndexed(ctx, cid)
		if (err == nil && ok) || errors.Is(err, ErrContractIsSAC) {
			// A RESOLVED hash or a SAC verdict is authoritative. An index
			// Only a positive verdict is authoritative: the availability probe is a table-global
			// LIMIT-1 check and cannot see partial backfill, so an index miss may only mean "not
			// reached yet".
			return h, ok, err
		}
		// Index miss or error: fall back to the legacy read rather than trust an incomplete index.
	}
	return r.contractWasmHashLegacy(ctx, cid)
}

// contractWasmHashLegacy resolves via ledger_entries_current: the fallback for deployments without
// contract_instance_changes.
func (r *ExplorerReader) contractWasmHashLegacy(ctx context.Context, cid xdr.Hash) (xdr.Hash, bool, error) {
	keys, err := instanceKeyXDR(cid)
	if err != nil {
		return xdr.Hash{}, false, err
	}
	// ledger_entries_current, not the changes log: the current-state MV folds every insert (immune
	// to snapshot-row merge loss) and (entry_type, key_xdr) is a PK-prefix lookup.
	const q = `SELECT entry_xdr FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'contract_data' AND key_xdr IN (?) AND entry_xdr != ''
		ORDER BY ledger_seq DESC`
	rows, err := r.conn.Query(ctx, q, keys)
	if err != nil {
		return xdr.Hash{}, false, fmt.Errorf("clickhouse: contract_data scan: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var b64 string
		if err := rows.Scan(&b64); err != nil {
			return xdr.Hash{}, false, fmt.Errorf("clickhouse: scan contract_data: %w", err)
		}
		var entry xdr.LedgerEntry
		if xdr.SafeUnmarshalBase64(b64, &entry) != nil {
			continue
		}
		cd, ok := entry.Data.GetContractData()
		if !ok || cd.Key.Type != xdr.ScValTypeScvLedgerKeyContractInstance {
			continue
		}
		inst, ok := cd.Val.GetInstance()
		if !ok {
			continue
		}
		switch inst.Executable.Type {
		case xdr.ContractExecutableTypeContractExecutableWasm:
			if inst.Executable.WasmHash != nil {
				return *inst.Executable.WasmHash, true, rows.Err()
			}
		case xdr.ContractExecutableTypeContractExecutableStellarAsset:
			// Newest-first, so this is the current executable: report SAC distinctly, not as
			// unresolved.
			return xdr.Hash{}, false, ErrContractIsSAC
		}
	}
	return xdr.Hash{}, false, rows.Err()
}

// ContractInstanceState is the lake's evidence about a contract's instance
// ledger entry.
type ContractInstanceState struct {
	// Known: positive lake evidence the instance entry exists or existed (a TTL row or a resolvable
	// executable). False means "not in the captured window", not "never deployed".
	Known bool
	// LiveUntil is the newest liveUntilLedgerSeq recorded for the instance
	// key; 0 when no TTL row is held. Judge it with [TTLVerdictAt].
	LiveUntil uint32
}

// ContractInstanceState resolves the contract's instance-entry evidence: a
// primary-key read on stellar.ttl_live_until for the instance key, falling
// back to the executable resolution only when no TTL row proves existence.
func (r *ExplorerReader) ContractInstanceState(ctx context.Context, contractID string) (ContractInstanceState, error) {
	dec, err := strkey.Decode(strkey.VersionByteContract, contractID)
	if err != nil {
		return ContractInstanceState{}, fmt.Errorf("clickhouse: bad contract id %q: %w", contractID, err)
	}
	var cid xdr.Hash
	copy(cid[:], dec)
	keys, err := instanceKeyXDR(cid)
	if err != nil {
		return ContractInstanceState{}, err
	}
	liveUntil, err := ttlLiveUntilBatch(ctx, r.conn, keys)
	if err != nil {
		return ContractInstanceState{}, fmt.Errorf("clickhouse: instance ttl lookup: %w", err)
	}
	var st ContractInstanceState
	for _, lu := range liveUntil {
		st.LiveUntil = max(st.LiveUntil, lu)
	}
	if st.LiveUntil > 0 {
		st.Known = true
		return st, nil
	}
	_, ok, err := r.contractWasmHash(ctx, cid)
	if errors.Is(err, ErrContractIsSAC) {
		return ContractInstanceState{Known: true}, nil
	}
	if err != nil {
		return ContractInstanceState{}, err
	}
	return ContractInstanceState{Known: ok}, nil
}

// ContractCodeVersion is one entry in a contract's code-upgrade timeline: the
// ledger at which the contract's instance began pointing at WasmHash.
type ContractCodeVersion struct {
	Ledger    uint32
	CloseTime time.Time
	WasmHash  string
}

// contractCodeHistoryMaxRows caps instance-change rows read: a contract rewriting its instance
// storage can match millions of rows, all transferred and XDR-decoded. 10k is orders above real
// timelines.
const contractCodeHistoryMaxRows = 10_000

// contractCodeHistoryQuery is ContractCodeHistory's SQL. The cap applies newest-first, then
// re-sorts ascending, so truncation drops the OLDEST changes, never the current executable.
// key_xdr is not a sort-key prefix on ledger_entry_changes, so the scan is pinned by
// explorerScanSettings. Order by intra_ledger_seq first: change_index restarts per transaction.
const contractCodeHistoryQuery = `SELECT ledger_seq, close_time, entry_xdr FROM (
			SELECT ledger_seq, close_time, entry_xdr, intra_ledger_seq, change_index, ingested_at
			FROM stellar.ledger_entry_changes
			WHERE entry_type = 'contract_data' AND key_xdr IN (?) AND entry_xdr != ''
			ORDER BY ledger_seq DESC, intra_ledger_seq DESC, change_index DESC, ingested_at DESC
			LIMIT ?
		) ORDER BY ledger_seq ASC, intra_ledger_seq ASC, change_index ASC, ingested_at ASC` + explorerScanSettings

// ContractCodeHistory returns the contract's WASM-hash timeline (ADR-0038 Phase C): each distinct
// executable in order, so an `update_contract` upgrade shows as a new version. Empty (not an error)
// when there is no wasm instance write: never deployed, a SAC, or not a contract.
func (r *ExplorerReader) ContractCodeHistory(ctx context.Context, contractID string) ([]ContractCodeVersion, error) {
	dec, err := strkey.Decode(strkey.VersionByteContract, contractID)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: bad contract id %q: %w", contractID, err)
	}
	var cidHash xdr.Hash
	copy(cidHash[:], dec)

	// Index-first as in contractWasmHash: the availability probe is table-global, so an EMPTY
	// indexed result cannot be told from "backfill not here yet" and is not trusted; only non-empty
	// is. A contract with index rows but no wasm (a SAC) is covered, so its empty timeline is
	// authoritative.
	if r.instanceChangesIndexAvailable(ctx) {
		out, _, err := r.contractCodeHistoryIndexed(ctx, cidHash)
		if err != nil || len(out) > 0 {
			return out, err
		}
		indexed, err := r.contractInInstanceIndex(ctx, cidHash)
		if err != nil || indexed {
			return nil, err
		}
		// A genesis-complete index makes the miss authoritative: the
		// timeline is read from ledger 1, so skip the key_xdr scan.
		if r.instanceGenesisCovers(ctx, 1) {
			return nil, nil
		}
	}
	return r.contractCodeHistoryLegacy(ctx, cidHash)
}

// ErrInstanceHistoryIncomplete: contract_instance_changes carries no
// genesis-complete watermark, so no timeline read from it can prove which
// WASM ran at a historical ledger.
var ErrInstanceHistoryIncomplete = errors.New("clickhouse: contract_instance_changes genesis watermark missing: run stellarindex-ops ch-instance-backfill to completion")

// ErrCodeHistoryTruncated: the timeline hit contractCodeHistoryMaxRows,
// which drops the OLDEST versions.
var ErrCodeHistoryTruncated = errors.New("clickhouse: contract code history truncated at the row cap (oldest versions dropped)")

// ReplayCodeHistory is ContractCodeHistory for the replay WASM gate: it reads only the
// genesis-complete instance index and never answers an unproven timeline. Errors:
// ErrInstanceHistoryIncomplete (no watermark), ErrContractIsSAC, ErrContractWasmUnresolved (no
// rows), ErrCodeHistoryTruncated.
func (r *ExplorerReader) ReplayCodeHistory(ctx context.Context, contractID string) ([]ContractCodeVersion, error) {
	dec, err := strkey.Decode(strkey.VersionByteContract, contractID)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: bad contract id %q: %w", contractID, err)
	}
	var cid xdr.Hash
	copy(cid[:], dec)

	wm, err := r.instanceGenesisWatermark(ctx)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: read instance genesis watermark: %w", err)
	}
	if wm == 0 {
		return nil, ErrInstanceHistoryIncomplete
	}
	out, truncated, err := r.contractCodeHistoryIndexed(ctx, cid)
	switch {
	case err != nil:
		return nil, err
	case truncated:
		return nil, ErrCodeHistoryTruncated
	case len(out) > 0:
		return out, nil
	}
	if _, _, err := r.contractWasmHashIndexed(ctx, cid); err != nil {
		return nil, err // ErrContractIsSAC included
	}
	return nil, ErrContractWasmUnresolved
}

// instanceGenesisWatermarkQuery is shared by the reader and the writer.
const instanceGenesisWatermarkQuery = `SELECT max(thru_ledger) FROM stellar.entry_history_watermark WHERE name = ?`

// instanceGenesisWatermark is the genesis-complete thru_ledger
// ch-instance-backfill recorded, 0 when absent.
func (r *ExplorerReader) instanceGenesisWatermark(ctx context.Context) (uint32, error) {
	var wm uint32
	err := r.conn.QueryRow(ctx, instanceGenesisWatermarkQuery, ContractInstanceChangesTable).Scan(&wm)
	return wm, err
}

// instanceGenesisCovers reports whether ch-instance-backfill recorded a
// genesis-complete watermark at or above ledger. Absent, unreadable or lower
// all answer false, so the caller keeps its scan.
func (r *ExplorerReader) instanceGenesisCovers(ctx context.Context, ledger uint32) bool {
	wm, err := r.instanceGenesisWatermark(ctx)
	return err == nil && wm > 0 && ledger <= wm
}

// contractInInstanceIndexQuery names only the primary-key prefix, so it
// serves both key shapes.
const contractInInstanceIndexQuery = `SELECT 1 FROM stellar.contract_instance_changes
		  WHERE contract_hash = ?
		  LIMIT 1`

// contractInInstanceIndex reports whether the instance index holds any row
// for the contract, i.e. the backfill has reached it.
func (r *ExplorerReader) contractInInstanceIndex(ctx context.Context, cid xdr.Hash) (bool, error) {
	rows, err := r.conn.Query(ctx, contractInInstanceIndexQuery, hex.EncodeToString(cid[:]))
	if err != nil {
		return false, fmt.Errorf("clickhouse: instance index presence: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		return true, nil
	}
	return false, rows.Err()
}

// contractCodeHistoryLegacy scans the changes log when contract_instance_changes is absent, empty,
// or has not reached this contract yet.
func (r *ExplorerReader) contractCodeHistoryLegacy(ctx context.Context, cidHash xdr.Hash) ([]ContractCodeVersion, error) {
	keys, err := instanceKeyXDR(cidHash)
	if err != nil {
		return nil, err
	}

	rows, err := r.conn.Query(ctx, contractCodeHistoryQuery, keys, contractCodeHistoryMaxRows)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: contract code history: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ContractCodeVersion
	var lastHash string
	for rows.Next() {
		var (
			seq       uint32
			closeTime time.Time
			b64       string
		)
		if err := rows.Scan(&seq, &closeTime, &b64); err != nil {
			return nil, fmt.Errorf("clickhouse: scan code history: %w", err)
		}
		var entry xdr.LedgerEntry
		if xdr.SafeUnmarshalBase64(b64, &entry) != nil {
			continue
		}
		cd, ok := entry.Data.GetContractData()
		if !ok || cd.Key.Type != xdr.ScValTypeScvLedgerKeyContractInstance {
			continue
		}
		inst, ok := cd.Val.GetInstance()
		if !ok || inst.Executable.Type != xdr.ContractExecutableTypeContractExecutableWasm ||
			inst.Executable.WasmHash == nil {
			continue
		}
		h := hex.EncodeToString(inst.Executable.WasmHash[:])
		if h == lastHash {
			continue // unchanged executable — not an upgrade
		}
		lastHash = h
		out = append(out, ContractCodeVersion{Ledger: seq, CloseTime: closeTime, WasmHash: h})
	}
	return out, rows.Err()
}

// contractCodeHistoryIndexedQuery reads the keyed instance-executable timeline
// (contract_instance_changes.sql); contract_hash is its primary-key prefix. Consecutive identical
// executables collapse SERVER-side before the cap, so the cap bounds executable changes (A->B->A
// kept) and preserves the newest.
// Order by intra_ledger_seq first; change_index restarts per transaction and only breaks ties (and
// legacy rows with intra_ledger_seq 0).
const contractCodeHistoryIndexedQuery = `SELECT ledger_seq, close_time, wasm_hash FROM (
			SELECT ledger_seq, close_time, wasm_hash, intra_ledger_seq, change_index FROM (
				SELECT ledger_seq, close_time, wasm_hash, intra_ledger_seq, change_index,
					lagInFrame(wasm_hash, 1, '') OVER (
						ORDER BY ledger_seq, intra_ledger_seq, change_index
						ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) AS prev_hash
				FROM stellar.contract_instance_changes
				WHERE contract_hash = ? AND is_sac = 0 AND wasm_hash != ''
			)
			WHERE wasm_hash != prev_hash
			ORDER BY ledger_seq DESC, intra_ledger_seq DESC, change_index DESC
			LIMIT ?
		) ORDER BY ledger_seq ASC, intra_ledger_seq ASC, change_index ASC` + explorerScanSettings

// contractCodeHistoryIndexedQueryOldKey serves a table still on the
// pre-intra_ledger_seq shape (instanceChangesTxKeyed false) until its
// rebuild cut-over.
const contractCodeHistoryIndexedQueryOldKey = `SELECT ledger_seq, close_time, wasm_hash FROM (
			SELECT ledger_seq, close_time, wasm_hash, change_index FROM (
				SELECT ledger_seq, close_time, wasm_hash, change_index,
					lagInFrame(wasm_hash, 1, '') OVER (
						ORDER BY ledger_seq, change_index
						ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) AS prev_hash
				FROM stellar.contract_instance_changes
				WHERE contract_hash = ? AND is_sac = 0 AND wasm_hash != ''
			)
			WHERE wasm_hash != prev_hash
			ORDER BY ledger_seq DESC, change_index DESC
			LIMIT ?
		) ORDER BY ledger_seq ASC, change_index ASC` + explorerScanSettings

// contractWasmHashIndexedQuery reads the ledger-final instance write; the
// order is contractCodeHistoryIndexedQuery's, newest first.
const contractWasmHashIndexedQuery = `SELECT is_sac, wasm_hash FROM stellar.contract_instance_changes
		  WHERE contract_hash = ?
		  ORDER BY ledger_seq DESC, intra_ledger_seq DESC, change_index DESC
		  LIMIT 1`

const contractWasmHashIndexedQueryOldKey = `SELECT is_sac, wasm_hash FROM stellar.contract_instance_changes
		  WHERE contract_hash = ?
		  ORDER BY ledger_seq DESC, change_index DESC
		  LIMIT 1`

// contractCodeHistoryIndexed is the fast path over the keyed index (no XDR decode). The Go loop
// re-checks the hash boundary so pre-merge RMT duplicates cannot surface twice.
func (r *ExplorerReader) contractCodeHistoryIndexed(ctx context.Context, cid xdr.Hash) ([]ContractCodeVersion, bool, error) {
	q := contractCodeHistoryIndexedQuery
	if !r.instanceChangesTxKeyed(ctx) {
		q = contractCodeHistoryIndexedQueryOldKey
	}
	rows, err := r.conn.Query(ctx, q, hex.EncodeToString(cid[:]), contractCodeHistoryMaxRows)
	if err != nil {
		return nil, false, fmt.Errorf("clickhouse: contract code history (indexed): %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ContractCodeVersion
	var lastHash string
	n := 0
	for rows.Next() {
		n++
		var (
			seq       uint32
			closeTime time.Time
			h         string
		)
		if err := rows.Scan(&seq, &closeTime, &h); err != nil {
			return nil, false, fmt.Errorf("clickhouse: scan code history (indexed): %w", err)
		}
		if h == lastHash {
			continue // unchanged executable — not an upgrade
		}
		lastHash = h
		out = append(out, ContractCodeVersion{Ledger: seq, CloseTime: closeTime, WasmHash: h})
	}
	return out, n >= contractCodeHistoryMaxRows, rows.Err()
}

// contractWasmHashIndexed resolves the current executable from the newest instance write. ok=false
// with nil error is NOT an authoritative not-found (the index may not have reached this contract);
// callers fall back to the legacy read. ErrContractIsSAC mirrors the legacy verdict.
func (r *ExplorerReader) contractWasmHashIndexed(ctx context.Context, cid xdr.Hash) (xdr.Hash, bool, error) {
	q := contractWasmHashIndexedQuery
	if !r.instanceChangesTxKeyed(ctx) {
		q = contractWasmHashIndexedQueryOldKey
	}
	rows, err := r.conn.Query(ctx, q, hex.EncodeToString(cid[:]))
	if err != nil {
		return xdr.Hash{}, false, fmt.Errorf("clickhouse: instance index wasm hash: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return xdr.Hash{}, false, rows.Err()
	}
	var (
		isSAC   uint8
		hashHex string
	)
	if err := rows.Scan(&isSAC, &hashHex); err != nil {
		return xdr.Hash{}, false, fmt.Errorf("clickhouse: scan instance index: %w", err)
	}
	if isSAC == 1 {
		return xdr.Hash{}, false, ErrContractIsSAC
	}
	raw, err := hex.DecodeString(hashHex)
	if err != nil || len(raw) != 32 {
		return xdr.Hash{}, false, fmt.Errorf("clickhouse: instance index bad wasm hash %q", hashHex)
	}
	var h xdr.Hash
	copy(h[:], raw)
	return h, true, nil
}

// instanceKeyXDR returns the base64 LedgerKey for a contract's instance entry, one per durability
// (persistent + temporary): key_xdr is stored verbatim, so querying both keeps the match exact.
func instanceKeyXDR(cid xdr.Hash) ([]string, error) {
	contractID := xdr.ContractId(cid)
	durabilities := []xdr.ContractDataDurability{
		xdr.ContractDataDurabilityPersistent,
		xdr.ContractDataDurabilityTemporary,
	}
	out := make([]string, 0, len(durabilities))
	for _, d := range durabilities {
		key := xdr.LedgerKey{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.LedgerKeyContractData{
				Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &contractID},
				Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
				Durability: d,
			},
		}
		b64, err := xdr.MarshalBase64(key)
		if err != nil {
			return nil, fmt.Errorf("clickhouse: marshal instance key: %w", err)
		}
		out = append(out, b64)
	}
	return out, nil
}

// codeKeyXDR returns the key_xdr for a contract_code entry. The key is a bare wasm hash, so unlike
// instanceKeyXDR there is no durability variant.
func codeKeyXDR(hash xdr.Hash) (string, error) {
	var k xdr.LedgerKey
	if err := k.SetContractCode(hash); err != nil {
		return "", fmt.Errorf("clickhouse: contract_code key: %w", err)
	}
	b64, err := xdr.MarshalBase64(k)
	if err != nil {
		return "", fmt.Errorf("clickhouse: marshal contract_code key: %w", err)
	}
	return b64, nil
}

// wasmCodeByHashQuery pins the lookup to the code entry's LedgerKey on ledger_entries_current, not
// the changes log: (entry_type, key_xdr) is its full PK, so hit and miss are cheap mark-range
// reads, whereas scanning ledger_entry_changes cannot finish inside explorerReadTimeout.
// Every contract_code key in the change log exists in current-state and the Code payload is
// content-addressed (sha256 == key hash), so any row yields the same bytes.
// NO FINAL: a non-empty-entry_xdr filter applied after FINAL dedup lets a 'removed' row win and then vanish,
// turning held code into a 404.
// LIMIT 4, not 1: up to 3 pre-merge rows per key, so the cc.Hash guard can skip an undecodable one.
// No explorerScanSettings: this is a keyed point read.
const wasmCodeByHashQuery = `SELECT entry_xdr FROM stellar.ledger_entries_current
	WHERE entry_type = 'contract_code' AND key_xdr = ? AND entry_xdr != ''
	LIMIT 4`

// wasmCodeByHash returns the raw wasm bytes for a code hash from the
// contract_code entries, or ok=false when that hash isn't captured.
func (r *ExplorerReader) wasmCodeByHash(ctx context.Context, hash xdr.Hash) ([]byte, bool, error) {
	key, err := codeKeyXDR(hash)
	if err != nil {
		return nil, false, err
	}
	rows, err := r.conn.Query(ctx, wasmCodeByHashQuery, key)
	if err != nil {
		return nil, false, fmt.Errorf("clickhouse: contract_code lookup: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var b64 string
		if err := rows.Scan(&b64); err != nil {
			return nil, false, fmt.Errorf("clickhouse: scan contract_code: %w", err)
		}
		var entry xdr.LedgerEntry
		if xdr.SafeUnmarshalBase64(b64, &entry) != nil {
			continue
		}
		cc, ok := entry.Data.GetContractCode()
		if !ok || cc.Hash != hash {
			continue
		}
		return []byte(cc.Code), true, rows.Err()
	}
	return nil, false, rows.Err()
}

// SACClassicAssetName resolves a contract to the classic asset its SAC wraps ("native" or
// "CODE:GISSUER"). found=false when no instance is in the lake or the executable is not
// StellarAsset: only core mints that executable, so it is the trust anchor, unlike a WASM
// contract's METADATA name.
func (r *ExplorerReader) SACClassicAssetName(ctx context.Context, contractID string) (string, bool, error) {
	raw, err := strkey.Decode(strkey.VersionByteContract, contractID)
	if err != nil {
		return "", false, fmt.Errorf("clickhouse: SACClassicAssetName: bad contract id: %w", err)
	}
	var cid xdr.Hash
	copy(cid[:], raw)
	keys, err := instanceKeyXDR(cid)
	if err != nil {
		return "", false, err
	}
	// Same table as contractWasmHashLegacy.
	const q = `SELECT entry_xdr FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'contract_data' AND key_xdr IN (?) AND entry_xdr != ''
		ORDER BY ledger_seq DESC LIMIT 1`
	rows, err := r.conn.Query(ctx, q, keys)
	if err != nil {
		return "", false, fmt.Errorf("clickhouse: SAC instance scan: %w", err)
	}
	defer func() { _ = rows.Close() }()
	// LIMIT 1 newest-first: the newest instance decides; no older row can change a non-SAC verdict.
	if !rows.Next() {
		return "", false, rows.Err()
	}
	var b64 string
	if err := rows.Scan(&b64); err != nil {
		return "", false, fmt.Errorf("clickhouse: scan SAC instance: %w", err)
	}
	name, ok := sacNameFromInstanceEntry(b64)
	return name, ok, rows.Err()
}

// sacNameFromInstanceEntry returns the SAC metadata name iff the executable is the core-minted
// StellarAsset type (a WASM contract cannot claim it).
func sacNameFromInstanceEntry(b64 string) (string, bool) {
	var entry xdr.LedgerEntry
	if xdr.SafeUnmarshalBase64(b64, &entry) != nil {
		return "", false
	}
	cd, ok := entry.Data.GetContractData()
	if !ok {
		return "", false
	}
	inst, ok := cd.Val.GetInstance()
	if !ok || inst.Executable.Type != xdr.ContractExecutableTypeContractExecutableStellarAsset || inst.Storage == nil {
		return "", false
	}
	for _, kv := range *inst.Storage {
		sym, ok := kv.Key.GetSym()
		if !ok || string(sym) != "METADATA" || kv.Val.Type != xdr.ScValTypeScvMap || kv.Val.Map == nil {
			continue
		}
		for _, e := range **kv.Val.Map {
			if ksym, ok := e.Key.GetSym(); !ok || string(ksym) != "name" {
				continue
			}
			if name, ok := e.Val.GetStr(); ok {
				return string(name), true
			}
		}
	}
	return "", false
}

// SACAssetFromEvents infers the classic asset from the trailing sep0011_asset String topic of
// CAP-67 events; the last-resort path for SACs whose instance was never captured. The caller MUST
// re-derive the SAC address from the returned asset: the topic is attacker-influenceable on non-SAC
// contracts.
func (r *ExplorerReader) SACAssetFromEvents(ctx context.Context, contractID string) (string, bool, error) {
	// Bound the scan by the contract's active ledgers: `contract_id = ? ORDER BY ledger_seq DESC
	// LIMIT 1` on a quiet contract walks the whole key range backwards, and this sits on the /wasm
	// 404 path.
	if r.contractLedgersIndexAvailable(ctx) {
		// Probe the newest few active ledgers, not just the latest: that one could carry only a
		// shorter-topic event.
		const probeLedgers = 8
		ledgers, lerr := r.contractActiveLedgers(ctx, contractID, 0, probeLedgers)
		if lerr == nil {
			if len(ledgers) == 0 {
				// Authoritative: the index covers every contract with
				// events, so no active ledgers means nothing to inspect.
				return "", false, nil
			}
			const boundedQ = `SELECT topics_xdr[length(topics_xdr)] FROM stellar.contract_events
		WHERE contract_id = ? AND ledger_seq IN (?) AND length(topics_xdr) >= 3
		ORDER BY ledger_seq DESC LIMIT 1`
			// A miss is an ANSWER, not a reason to fall back: the unbounded scan is the cost this
			// path avoids and non-SACs would pay it every time.
			// Accepted residual: a real SAC whose 8 newest active ledgers carry only <3-topic
			// events reads as non-SAC. It affects only 404-branch labels, never a wrong positive
			// (the caller re-derives and rejects mismatches). Widen the probe window rather than
			// falling back to the unbounded scan.
			if name, ok, qerr := r.sacAssetFromEventsQuery(ctx, boundedQ, contractID, ledgers); qerr == nil {
				return name, ok, nil
			}
			// Only a query ERROR falls through to the unbounded form,
			// so a broken index degrades to slow rather than to wrong.
		}
	}
	const q = `SELECT topics_xdr[length(topics_xdr)] FROM stellar.contract_events
		WHERE contract_id = ? AND length(topics_xdr) >= 3
		ORDER BY ledger_seq DESC LIMIT 1`
	return r.sacAssetFromEventsQuery(ctx, q, contractID)
}

// sacAssetFromEventsQuery runs one SAC-name probe and decodes it; extra args bind after contractID.
func (r *ExplorerReader) sacAssetFromEventsQuery(ctx context.Context, q, contractID string, extra ...any) (string, bool, error) {
	args := append([]any{contractID}, extra...)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return "", false, fmt.Errorf("clickhouse: SACAssetFromEvents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return "", false, rows.Err()
	}
	var b64 string
	if err := rows.Scan(&b64); err != nil {
		return "", false, err
	}
	var sv xdr.ScVal
	if xdr.SafeUnmarshalBase64(b64, &sv) != nil {
		return "", false, rows.Err()
	}
	str, ok := sv.GetStr()
	if !ok {
		return "", false, rows.Err()
	}
	return string(str), true, rows.Err()
}
