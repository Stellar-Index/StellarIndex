package explorer

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// WasmExportView is one exported contract function on the wire: its name and
// the wasm-ABI param/result value types. For a Soroban contract the names are
// the contract's public entry points (e.g. "swap", "deposit"); the types are
// the low-level wasm ABI (i64-tagged host values), not the Rust signature.
type WasmExportView struct {
	Name    string   `json:"name"`
	Params  []string `json:"params"`
	Results []string `json:"results"`
}

// ContractWasmView is the wire response for GET /v1/contracts/{id}/wasm: the resolved wasm hash
// and size, the exported function table (always present, parsed natively), and best-effort WAT
// and decompile pseudocode (empty without the wabt toolchain). source_note explains provenance
// and any degraded stages. size_bytes is small, so it stays a JSON number; there are no i128
// fields.
type ContractWasmView struct {
	ContractID string           `json:"contract_id"`
	WasmHash   string           `json:"wasm_hash"`
	SizeBytes  int              `json:"size_bytes"`
	Exports    []WasmExportView `json:"exports"`
	Wat        string           `json:"wat,omitempty"`
	Decompiled string           `json:"decompiled,omitempty"`
	SourceNote string           `json:"source_note"`
	// TTL is the instance entry's liveness at the lake watermark (see
	// ContractDetailView.TTL); archived also qualifies source_note.
	TTL *ContractTTLV `json:"ttl,omitempty"`
}

// ContractWasm serves GET /v1/contracts/{contract_id}/wasm: metadata, the exported function
// table, and WAT plus decompiled pseudocode when wabt is present, read on demand from the lake.
// Cache lifetime is set by wasmCacheControl.
//
// 404 when the wasm is not resolvable from the captured ledger_entry_changes window (the instance
// or code entry was not captured; see clickhouse.ErrContractWasmUnresolved).
func (h *Handler) ContractWasm(w http.ResponseWriter, r *http.Request) {
	if h.Reader == nil {
		h.unavailable(w, r)
		return
	}
	cid := r.PathValue("contract_id")
	if !canonical.IsContractID(cid) {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-contract-id",
			"Invalid contract id", http.StatusBadRequest,
			"the contract id must be a valid C-strkey")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	info, err := h.Reader.ContractWasm(ctx, cid)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		if errors.Is(err, clickhouse.ErrContractIsSAC) {
			// A Stellar Asset Contract runs built-in host logic and has no user WASM. A distinct 404
			// lets clients say "SAC, no WASM" rather than a misleading "not captured yet".
			h.WriteProblem(w, r, "https://api.stellarindex.io/errors/contract-is-sac",
				"Stellar Asset Contract — no WASM", http.StatusNotFound,
				"this contract is a Stellar Asset Contract (the built-in SAC host "+
					"logic behind a classic asset), not a user-uploaded WASM "+
					"module — there is no WASM bytecode to show")
			return
		}
		if errors.Is(err, clickhouse.ErrContractWasmUnresolved) {
			// No instance found (its entry predates the capture window). Check the SAC-wrapper
			// registry first: a contract wrapping a classic asset never has WASM, so the answer
			// is definitive even uncaptured.
			if h.IsKnownSAC(cid) {
				h.WriteProblem(w, r, "https://api.stellarindex.io/errors/contract-is-sac",
					"Stellar Asset Contract — no WASM", http.StatusNotFound,
					"this contract is a Stellar Asset Contract (the built-in SAC host "+
						"logic behind a classic asset), not a user-uploaded WASM "+
						"module — there is no WASM bytecode to show")
				return
			}
			// Event-derived SAC identification: many SACs have no captured instance (deployed
			// pre-lake or TTL-evicted before every checkpoint). Their CAP-67 events carry the
			// wrapped asset in the trailing sep0011 topic; the derivation cross-check makes it
			// spoof-proof, since a fake contract can emit the topic but not occupy the derived address.
			if assetName, found := h.sacAssetViaEvents(ctx, cid); found {
				h.WriteProblem(w, r, "https://api.stellarindex.io/errors/contract-is-sac",
					"Stellar Asset Contract — no WASM", http.StatusNotFound,
					"this contract is the Stellar Asset Contract for "+assetName+
						" — built-in SAC host logic, not a user-uploaded WASM module; "+
						"there is no bytecode to show. Asset detail: /v1/assets/"+assetName)
				return
			}
			h.WriteProblem(w, r, "https://api.stellarindex.io/errors/contract-wasm-not-found",
				"Contract WASM not found", http.StatusNotFound,
				"the contract's wasm could not be assembled from the lake — its "+
					"contract-instance or contract-code entry isn't in the captured "+
					"ledger-entry window")
			return
		}
		if retryableColdMiss(ctx, err) {
			h.Logger.Warn("explorer ContractWasm deadline exceeded", "contract", cid)
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/contract-wasm-timeout",
				"Contract WASM timed out")
			return
		}
		h.Logger.Error("explorer ContractWasm failed", "err", err, "contract", cid)
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	view := contractWasmView(info)
	h.setWasmLiveness(ctx, cid, &view)
	w.Header().Set("Cache-Control", wasmCacheControl(view))
	_, stale, _ := h.lakeTip(ctx)
	h.WriteJSON(w, view, stale)
}

// contractWasmView maps the reader's ContractWasmInfo to the wire shape,
// composing source_note from the static provenance line + any tool note.
func contractWasmView(info clickhouse.ContractWasmInfo) ContractWasmView {
	exports := make([]WasmExportView, len(info.Exports))
	for i, e := range info.Exports {
		exports[i] = WasmExportView{
			Name:    e.Name,
			Params:  nonNilStrings(e.Params),
			Results: nonNilStrings(e.Results),
		}
	}
	note := "wasm resolved from the certified ClickHouse lake " +
		"(contract instance → wasm hash → contract_code bytes, ADR-0034); " +
		"exports parsed natively; wat/decompiled are best-effort via wabt"
	if info.ToolNote != "" {
		note += " — " + info.ToolNote
	}
	return ContractWasmView{
		ContractID: info.ContractID,
		WasmHash:   info.WasmHash,
		SizeBytes:  info.SizeBytes,
		Exports:    exports,
		Wat:        info.Wat,
		Decompiled: info.Decompiled,
		SourceNote: note,
	}
}

// setWasmLiveness attaches the instance TTL and, for an archived instance,
// says so in source_note — the resolved wasm is its last executable, not
// something that can be invoked now.
func (h *Handler) setWasmLiveness(ctx context.Context, cid string, view *ContractWasmView) {
	st, ok := h.contractInstanceState(ctx, cid)
	if !ok {
		return
	}
	view.TTL = h.contractTTL(ctx, st.LiveUntil)
	if view.TTL != nil && view.TTL.State == ttlStateArchived {
		view.SourceNote = fmt.Sprintf("ARCHIVED: the contract instance's TTL lapsed at ledger %d "+
			"(lake watermark %d); invocations fail until it is restored — ",
			view.TTL.LiveUntil, view.TTL.AsOfLedger) + view.SourceNote
	}
}

// wasmCacheControl bounds the wasm view's cache lifetime. Wasm for a hash is immutable, but this
// URL is keyed on contract_id, and an in-place upgrade changes the bytes it serves, so long-lived
// caching would show pre-upgrade bytecode. It uses the standard catalogue band until the route is
// content-addressed; a ttl verdict (archival/restore) takes the tighter band regardless.
func wasmCacheControl(view ContractWasmView) string {
	if view.TTL != nil {
		return "public, max-age=300"
	}
	return "public, max-age=60, s-maxage=300"
}

// nonNilStrings returns a non-nil slice so the JSON renders [] not null for a
// no-param/no-result function.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// sacAssetViaEvents resolves an uncaptured-instance contract to the
// classic asset whose SAC it is, using the event-topic hint + the
// deterministic-derivation cross-check (never trust the topic alone).
// Returns the asset in canonical CODE-GISSUER (or "native") form.
func (h *Handler) sacAssetViaEvents(ctx context.Context, contractID string) (string, bool) {
	name, found, err := h.Reader.SACAssetFromEvents(ctx, contractID)
	if err != nil || !found {
		return "", false
	}
	asset, ok := canonical.SEP11SACAsset(name, contractID)
	if !ok {
		return "", false
	}
	return asset.String(), true
}
