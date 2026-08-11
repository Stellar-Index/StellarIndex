package clickhouse

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// ErrContractWasmUnresolved is returned by ContractWasm when the contract's
// wasm could not be assembled from the lake — either the contract's
// contract_data INSTANCE entry isn't captured (so we can't learn its wasm
// hash) or the referenced contract_code entry isn't captured (so we have the
// hash but not the bytes). It's a clean "not found" (404), NOT an error.
//
// The "live-only capture window" explanation this comment used to give is
// STALE for contract_code, and following it wastes an operator's time
// waiting for a backfill that has already run. Measured on r1 2026-08-04:
// ledger_entries_current holds all 4,534 distinct contract_code keys, from
// Soroban activation (ledger 50,457,427, 2024-02-20) to tip, with zero
// removals — and every contract_code key present in ledger_entry_changes
// across all 14 Soroban partitions is present there too (7,777 rows, 0
// missing). A miss on the CODE hop therefore means the hash genuinely is
// not in the lake, not that it predates capture.
//
// The INSTANCE hop is the one that still misses in practice: of the 40
// busiest contracts by event count in a recent window, only 18 had an
// instance entry at all. Callers map this to 404.
var ErrContractWasmUnresolved = errors.New("clickhouse: contract wasm not resolvable from lake")

// ErrContractIsSAC is returned by ContractWasm when the contract's instance
// IS captured but its executable is a Stellar Asset Contract (the built-in
// SAC host logic), not a user-uploaded WASM module. SACs — the asset
// contracts behind `native`, USDC, and every classic asset, which are among
// the busiest contracts on the network — have no WASM to show, ever; a
// backfill will never produce one. Distinct from ErrContractWasmUnresolved
// so the API/UI can say "this is a SAC, no WASM" instead of "not captured
// yet" (audit 2026-06-19 item 13). Callers map this to a 404 with a SAC note.
var ErrContractIsSAC = errors.New("clickhouse: contract is a stellar asset contract (no wasm)")

// WasmExport is one exported function of a Soroban contract — its name and
// the i32/i64/f32/f64 param + result value types parsed from the wasm type
// section. For a Soroban contract the exported function names are the
// contract's public entry points (e.g. "register", "swap", "deposit"); the
// param/result types are the low-level wasm ABI (i64-tagged host values), not
// the Rust-level signature, but the NAMES are the contract's real API surface.
type WasmExport struct {
	Name    string   // exported symbol
	Params  []string // wasm value types: "i32"|"i64"|"f32"|"f64"
	Results []string // wasm value types
}

// ContractWasmInfo is the assembled per-contract wasm view: the resolved hash,
// the byte size, the natively-parsed export table, and (best-effort) the WAT
// disassembly + wasm-decompile pseudocode. Wat/Decompiled are empty when the
// wabt tooling (wasm2wat / wasm-decompile) isn't on PATH — the metadata +
// exports are always populated (pure-Go, no tool dependency).
type ContractWasmInfo struct {
	ContractID string
	WasmHash   string // hex sha256 of the wasm module
	SizeBytes  int
	Exports    []WasmExport
	Wat        string // WAT disassembly; empty if wasm2wat unavailable
	Decompiled string // wasm-decompile pseudocode; empty if unavailable
	ToolNote   string // human note on which optional stages ran / why they didn't
}

// ContractWasm resolves a contract id to its on-chain wasm and returns the
// assembled metadata view. Resolution is a two-hop walk over the certified
// lake's ledger_entry_changes (ADR-0034 substrate):
//
//  1. contract id → wasm hash: find the contract's contract_data INSTANCE
//     entry (ScvLedgerKeyContractInstance) and read its
//     executable.wasm_hash.
//  2. wasm hash → bytes: find the contract_code entry with that hash and read
//     ContractCodeEntry.code (the raw wasm module).
//
// The export table is parsed natively (pure Go, no tooling). WAT + decompile
// are filled best-effort by buildWasmDisassembly (wabt binaries if present).
//
// Returns ErrContractWasmUnresolved (a clean 404) when either hop misses in
// the captured window — historical deploy-time entries are largely outside the
// live ledger_entry_changes capture (extract.go G12-03 note).
func (r *ExplorerReader) ContractWasm(ctx context.Context, contractID string) (ContractWasmInfo, error) {
	dec, err := strkey.Decode(strkey.VersionByteContract, contractID)
	if err != nil {
		return ContractWasmInfo{}, fmt.Errorf("clickhouse: bad contract id %q: %w", contractID, err)
	}
	var cidHash xdr.Hash
	copy(cidHash[:], dec)

	wasmHash, ok, err := r.contractWasmHash(ctx, cidHash)
	if err != nil {
		return ContractWasmInfo{}, err
	}
	if !ok {
		return ContractWasmInfo{}, ErrContractWasmUnresolved
	}

	code, ok, err := r.wasmCodeByHash(ctx, wasmHash)
	if err != nil {
		return ContractWasmInfo{}, err
	}
	if !ok {
		return ContractWasmInfo{}, ErrContractWasmUnresolved
	}

	exports, perr := parseWasmExports(code)
	info := ContractWasmInfo{
		ContractID: contractID,
		WasmHash:   hex.EncodeToString(wasmHash[:]),
		SizeBytes:  len(code),
		Exports:    exports,
	}
	if perr != nil {
		// A parse miss is non-fatal: still serve the resolved hash + size.
		info.ToolNote = "export parse: " + perr.Error() + "; "
	}
	buildWasmDisassembly(ctx, &info, code)
	return info, nil
}

// contractWasmHash finds the contract's contract_data INSTANCE entry and reads
// its executable wasm hash. ok=false when no instance entry for this contract
// is in the captured window.
//
// The query is pinned to the contract's INSTANCE ledger key, computed
// deterministically (instanceKeyXDR): the key for the
// ScvLedgerKeyContractInstance entry of a given contract is a single, fixed
// base64 LedgerKey, so matching on key_xdr turns what would be a full decode of
// every contract_data row (millions — and slow to exhaust on a miss) into a
// precise equality predicate. Rows are ordered newest-first so the current
// executable wins under in-place contract upgrades; the per-contract result is
// cached hard (the wasm for a hash is immutable).
func (r *ExplorerReader) contractWasmHash(ctx context.Context, cid xdr.Hash) (xdr.Hash, bool, error) {
	// Index-first (inventory #26, wasm two-hop item, 2026-08-11): the
	// genesis-complete contract_instance_changes timeline resolves the
	// CURRENT executable for contracts whose instance entry predates
	// live entry capture — the "not in the captured window yet" class
	// the ledger_entries_current path below cannot see. Newest row
	// wins under in-place upgrades; a SAC verdict surfaces as
	// ErrContractIsSAC exactly like the legacy path.
	if r.instanceChangesIndexAvailable(ctx) {
		h, ok, err := r.contractWasmHashIndexed(ctx, cid)
		if err == nil || errors.Is(err, ErrContractIsSAC) {
			// A SAC verdict is authoritative, not an index failure.
			return h, ok, err
		}
		// Genuine index error — fall through to the legacy read rather
		// than failing the whole resolution.
	}
	return r.contractWasmHashLegacy(ctx, cid)
}

// contractWasmHashLegacy is the pre-index resolution over
// ledger_entries_current — the capture-window-bound read kept as the
// fallback for deployments without contract_instance_changes.
func (r *ExplorerReader) contractWasmHashLegacy(ctx context.Context, cid xdr.Hash) (xdr.Hash, bool, error) {
	keys, err := instanceKeyXDR(cid)
	if err != nil {
		return xdr.Hash{}, false, err
	}
	// ledger_entries_current, not the changes log: the current-state MV
	// folds every insert (immune to the snapshot-row merge-loss defect,
	// site-audit 2026-07-03) and (entry_type, key_xdr) is a PK-prefix
	// lookup instead of a bloom-filtered scan.
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
			// Instance IS captured, but it's a SAC — no WASM to resolve,
			// ever. Newest-first ordering means this is the current
			// executable, so report it distinctly rather than falling
			// through to the generic "unresolved" 404.
			return xdr.Hash{}, false, ErrContractIsSAC
		}
	}
	return xdr.Hash{}, false, rows.Err()
}

// ContractCodeVersion is one entry in a contract's code-upgrade timeline: the
// ledger at which the contract's instance began pointing at WasmHash.
type ContractCodeVersion struct {
	Ledger    uint32
	CloseTime time.Time
	WasmHash  string
}

// contractCodeHistoryMaxRows caps the instance-change rows ContractCodeHistory
// pulls back (audit-2026-07-23 C-F1). Pre-fix the query had no LIMIT at all: a
// contract that rewrites its instance entry often — instance-STORAGE writes
// rewrite the same ledger key, not just `update_contract` upgrades — can match
// millions of rows, every one of which is transferred and XDR-decoded below.
// 10k is ~3 orders of magnitude above anything real (the r1 probe of this exact
// query shape returned FOUR rows for a live contract) while bounding the
// pathological case, and the response collapses to distinct executables anyway.
const contractCodeHistoryMaxRows = 10_000

// contractCodeHistoryQuery is ContractCodeHistory's SQL.
//
// The cap is applied NEWEST-first in the inner select and the surviving
// window is re-sorted ascending for the collapse loop below, so truncation
// drops the OLDEST changes and never the newest: the most valuable entry in
// an upgrade timeline is the CURRENT executable, and this reader's coverage
// is already "the captured window" rather than "since deploy" (see
// ContractCodeHistory's doc comment), so an older-tail gap is the same class
// of gap callers already render. `ORDER BY … DESC LIMIT n` also bounds the
// sort to n rows instead of materialising every match.
//
// explorerScanSettings: key_xdr is NOT a sort-key prefix on the append-log
// ledger_entry_changes (ORDER BY leads with ledger_seq), so this predicate
// is scan-shaped over the changes history — the pin bounds its thread
// fan-out (route-sweep 2026-07-29: /v1/contracts/{id}/code-history was in
// the 8s 503 class).
const contractCodeHistoryQuery = `SELECT ledger_seq, close_time, entry_xdr FROM (
			SELECT ledger_seq, close_time, entry_xdr, change_index, ingested_at
			FROM stellar.ledger_entry_changes
			WHERE entry_type = 'contract_data' AND key_xdr IN (?) AND entry_xdr != ''
			ORDER BY ledger_seq DESC, change_index DESC, ingested_at DESC
			LIMIT ?
		) ORDER BY ledger_seq ASC, change_index ASC, ingested_at ASC` + explorerScanSettings

// ContractCodeHistory returns a contract's WASM-hash timeline — the contract's
// "change over time" (ADR-0038 Phase C): every distinct executable the
// contract instance has pointed at, in chronological order, so an in-place
// `update_contract` upgrade surfaces as a new version. Reads the instance
// contract_data entry's executable across the captured changes and collapses
// consecutive identical hashes. Coverage = the captured entry-change window
// (same substrate as the "see the code" view); fills back with the Phase-C
// backfill. Empty (not an error) when the contract's instance isn't captured.
func (r *ExplorerReader) ContractCodeHistory(ctx context.Context, contractID string) ([]ContractCodeVersion, error) {
	dec, err := strkey.Decode(strkey.VersionByteContract, contractID)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: bad contract id %q: %w", contractID, err)
	}
	var cidHash xdr.Hash
	copy(cidHash[:], dec)

	// Index-first (inventory #26 item 3): the keyed
	// contract_instance_changes timeline turns this from a scan-shaped
	// key_xdr predicate over the whole changes log (8s+ cold, the last
	// persistent 503 class in the 2026-08-09 route sweep) into a
	// primary-key walk. Fallback keeps the legacy scan for deployments
	// without the index.
	if r.instanceChangesIndexAvailable(ctx) {
		return r.contractCodeHistoryIndexed(ctx, cidHash)
	}

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

// contractCodeHistoryIndexedQuery reads the keyed instance-executable
// timeline (deploy/clickhouse/contract_instance_changes.sql). The
// contract_hash predicate is the table's primary-key prefix, ascending
// order matches the collapse loop, and the same newest-preserving cap as
// the legacy scan bounds pathological instance-storage churn: the inner
// select keeps the NEWEST rows, the outer re-sorts ascending.
const contractCodeHistoryIndexedQuery = `SELECT ledger_seq, close_time, wasm_hash FROM (
			SELECT ledger_seq, close_time, wasm_hash, change_index
			FROM stellar.contract_instance_changes
			WHERE contract_hash = ? AND is_sac = 0 AND wasm_hash != ''
			ORDER BY ledger_seq DESC, change_index DESC
			LIMIT ?
		) ORDER BY ledger_seq ASC, change_index ASC`

// contractCodeHistoryIndexed is ContractCodeHistory's fast path over the
// keyed index: no XDR decode (the MV/backfill already extracted the
// executable verdict), collapse of consecutive identical hashes in Go —
// which also absorbs RMT pre-merge duplicate keys, since a duplicate row
// carries the same hash as its neighbour.
func (r *ExplorerReader) contractCodeHistoryIndexed(ctx context.Context, cid xdr.Hash) ([]ContractCodeVersion, error) {
	rows, err := r.conn.Query(ctx, contractCodeHistoryIndexedQuery,
		hex.EncodeToString(cid[:]), contractCodeHistoryMaxRows)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: contract code history (indexed): %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ContractCodeVersion
	var lastHash string
	for rows.Next() {
		var (
			seq       uint32
			closeTime time.Time
			h         string
		)
		if err := rows.Scan(&seq, &closeTime, &h); err != nil {
			return nil, fmt.Errorf("clickhouse: scan code history (indexed): %w", err)
		}
		if h == lastHash {
			continue // unchanged executable — not an upgrade
		}
		lastHash = h
		out = append(out, ContractCodeVersion{Ledger: seq, CloseTime: closeTime, WasmHash: h})
	}
	return out, rows.Err()
}

// contractWasmHashIndexed resolves the current executable from the
// keyed instance timeline: the newest captured instance write for the
// contract. ok=false with nil error = the instance was never written in
// all of history (the index is genesis-complete) — an authoritative
// not-found. ErrContractIsSAC mirrors the legacy path's verdict.
func (r *ExplorerReader) contractWasmHashIndexed(ctx context.Context, cid xdr.Hash) (xdr.Hash, bool, error) {
	rows, err := r.conn.Query(ctx,
		`SELECT is_sac, wasm_hash FROM stellar.contract_instance_changes
		  WHERE contract_hash = ?
		  ORDER BY ledger_seq DESC, change_index DESC
		  LIMIT 1`,
		hex.EncodeToString(cid[:]))
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

// instanceKeyXDR returns the base64 LedgerKey(s) for a contract's
// ScvLedgerKeyContractInstance contract_data entry — one per durability
// (persistent + temporary), since the lake stores the key XDR verbatim and the
// durability is part of it. An instance entry is always persistent in practice,
// but querying both keeps the match exact without that assumption.
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

// codeKeyXDR returns the base64 LedgerKey the lake stores in key_xdr for a
// contract_code entry.
//
// A CONTRACT_CODE key carries ONLY the wasm hash
// (xdr.LedgerKeyContractCode is a bare Hash), so — unlike instanceKeyXDR's
// contract_data keys — there is no durability variant to enumerate: one
// hash, one key. Verified byte-identical against stored key_xdr on r1.
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

// wasmCodeByHashQuery pins the lookup to the code entry's LedgerKey.
//
// ledger_entries_current, NOT the changes log: (entry_type, key_xdr) is this
// table's FULL primary key, so this is a mark-range lookup — measured on r1
// 2026-08-04 at 121,584 rows / 53.93 MiB / 34 ms, and the MISS costs the
// same as the hit.
//
// The pre-fix query scanned stellar.ledger_entry_changes (159.4B rows /
// 6.52 TiB) with no key predicate and filtered the hash in Go. 59% of that
// table sits in partitions holding ZERO contract_code rows, and a full pass
// is ~45-50s — six times the 8s explorerReadTimeout. It never completed:
// query_log showed 12/12 executions aborted at the deadline having read
// 3.31 GiB each, i.e. this endpoint had NEVER returned a 200 for a WASM
// contract. The key_xdr bloom on the changes log is not the answer either —
// same lookup measured 641M rows / 47.08 GiB / 31.6s.
//
// Equivalence, not just speed: every contract_code key in the change log
// exists in current-state (7,777 change rows across all 14 Soroban
// partitions, 0 missing), so the hit set is a proven superset. Duplicate
// rows per key differ only in ContractCodeEntryExt v0/v1 and
// lastModifiedLedgerSeq; the Code payload is content-addressed and
// sha256(cc.Code) == the hash in this very key (verified 34/34 on r1), so
// any row yields identical bytes.
//
// NO FINAL — deliberately. entry_type/key_xdr is the whole PK so FINAL buys
// no selectivity, and `FINAL ... AND entry_xdr != ”` applies the filter
// AFTER dedup: a 'removed' row would win the dedup and then be filtered
// out, turning code we still hold into a 404. Probed on r1 against a key
// carrying both a live and a removal row: FINAL -> 0 rows, no-FINAL -> 1.
//
// LIMIT 4, not 1: current-state holds up to 3 rows per key pre-merge. The
// small cap keeps the caller's cc.Hash guard able to skip an undecodable
// row, at measurably identical cost.
//
// No explorerScanSettings pin: that constant's own doc carves out keyed
// point reads on (entry_type, key_xdr), and the sibling contractWasmHash
// query on this table carries none. Measured unpinned at 97,789 rows /
// 50.45 MiB / 38 ms — there is no fan-out to bound.
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

// SACClassicAssetName resolves a contract to the classic asset its
// Stellar Asset Contract wraps: "native" or "CODE:GISSUER". found is
// false when the contract has no instance in the lake OR its
// executable is NOT StellarAsset (a WASM contract claiming an
// asset-shaped METADATA name must not be trusted — only stellar-core
// mints the StellarAsset executable, which is the trust anchor here).
//
// Board #40 (RFP audit): wallets look holdings up by contract
// address; a SAC lookup must land on the classic identity so it
// carries the classic asset's price.
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
	// Same table choice rationale as contractWasmHash (merge-loss immune,
	// PK-prefix lookup).
	const q = `SELECT entry_xdr FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'contract_data' AND key_xdr IN (?) AND entry_xdr != ''
		ORDER BY ledger_seq DESC LIMIT 1`
	rows, err := r.conn.Query(ctx, q, keys)
	if err != nil {
		return "", false, fmt.Errorf("clickhouse: SAC instance scan: %w", err)
	}
	defer func() { _ = rows.Close() }()
	// LIMIT 1 newest-first: a single row decides — if the newest
	// instance is not a SAC (or carries no metadata), no older row
	// can change that verdict.
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

// sacNameFromInstanceEntry decodes one contract-instance LedgerEntry
// and returns the SAC metadata name iff the executable is the
// core-minted StellarAsset type (the trust anchor — a WASM contract
// cannot claim it).
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

// SACAssetFromEvents infers which classic asset a contract's SAC
// events belong to, from the trailing sep0011_asset String topic that
// CAP-67 unified events carry ("CODE:GISSUER" or "native"). Used as
// the LAST fallback for SAC identification when the contract instance
// was never captured (deployed pre-lake + TTL-evicted before any
// checkpoint — structurally invisible to snapshots; ~55k such
// contracts measured in the 2026-07-03 site audit). The caller MUST
// cross-check by re-deriving the SAC address from the returned asset
// — the topic is attacker-influenceable on non-SAC contracts, the
// derivation is not.
func (r *ExplorerReader) SACAssetFromEvents(ctx context.Context, contractID string) (string, bool, error) {
	const q = `SELECT topics_xdr[length(topics_xdr)] FROM stellar.contract_events
		WHERE contract_id = ? AND length(topics_xdr) >= 3
		ORDER BY ledger_seq DESC LIMIT 1`
	rows, err := r.conn.Query(ctx, q, contractID)
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
