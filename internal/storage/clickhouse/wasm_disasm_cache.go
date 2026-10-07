package clickhouse

import (
	"sync"
	"time"
)

// wasmDisasmCacheMax bounds resident entries. Disassembly text is the
// biggest per-entry payload this reader caches (up to maxDisasmOutputBytes
// each for wat + decompiled), so the cap is far smaller than the account
// caches' — a few hundred hot contracts covers the explorer's realistic
// working set while keeping worst-case memory bounded. On overflow the
// oldest entry is evicted.
const wasmDisasmCacheMax = 256

// wasmDisasmEntry is one contract's cached disassembly stage output.
type wasmDisasmEntry struct {
	wat        string
	decompiled string
	// toolNote carries only the wat/decompile note fragment (the join
	// buildWasmDisassembly would otherwise append inline) — NOT the export-parse
	// note the caller may have already set on info.ToolNote before this
	// stage runs.
	toolNote string
	cachedAt time.Time
}

// wasmDisasmCache is a bounded, permanent (no-TTL) cache of wabt output
// keyed by wasm hash. Unlike accountStateCache, staleness is not a concept
// here: the wasm bytes for a content-addressed hash never change, so a
// cached entry is valid for the life of the process — the cap exists only
// to bound memory, not to force re-computation.
//
// Only SUCCESSFUL tool runs are cached (see buildWasmDisassembly). A
// missing-tool or timed-out run is not cached, so a transient failure (load
// spike, tool not yet installed) is retried on the next request rather than
// pinned as a permanent false negative for that hash.
type wasmDisasmCache struct {
	mu      sync.Mutex
	entries map[string]wasmDisasmEntry
}

func newWasmDisasmCache() *wasmDisasmCache {
	return &wasmDisasmCache{entries: make(map[string]wasmDisasmEntry)}
}

// get is nil-safe: a zero-value reader (as built by some tests) behaves as
// a permanent miss, never a panic.
func (c *wasmDisasmCache) get(wasmHash string) (wasmDisasmEntry, bool) {
	if c == nil {
		return wasmDisasmEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[wasmHash]
	return e, ok
}

func (c *wasmDisasmCache) put(wasmHash string, e wasmDisasmEntry, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= wasmDisasmCacheMax {
		evictOldest(c.entries, func(e wasmDisasmEntry) time.Time { return e.cachedAt })
	}
	e.cachedAt = now
	c.entries[wasmHash] = e
}

// evictOldest drops the oldest-inserted entry — approximate LRU, same
// tradeoff as accountStateCache: one pass only when at capacity.
func evictOldest[E any](entries map[string]E, at func(E) time.Time) {
	var oldestKey string
	var oldestAt time.Time
	for k, existing := range entries {
		if oldestKey == "" || at(existing).Before(oldestAt) {
			oldestKey, oldestAt = k, at(existing)
		}
	}
	delete(entries, oldestKey)
}

// wasmModuleCacheMax bounds resident module blobs. A Soroban module is capped
// by network config (128 KiB today), so the worst case is tens of MiB.
const wasmModuleCacheMax = 256

// wasmModuleEntry is one wasm hash's lake blob plus its native export parse.
type wasmModuleEntry struct {
	code      []byte
	exports   []WasmExport
	parseNote string
	cachedAt  time.Time
}

// wasmModuleCache memoises the contract_code blob per wasm hash so a repeat
// request skips the lake read and the export parse. Like wasmDisasmCache it
// has no TTL: the bytes behind a content-addressed hash never change. A lake
// miss is not cached, so code that lands later is found on the next request.
type wasmModuleCache struct {
	mu      sync.Mutex
	entries map[string]wasmModuleEntry
}

func newWasmModuleCache() *wasmModuleCache {
	return &wasmModuleCache{entries: make(map[string]wasmModuleEntry)}
}

// get is nil-safe: a test-built reader behaves as a permanent miss.
func (c *wasmModuleCache) get(wasmHash string) (wasmModuleEntry, bool) {
	if c == nil {
		return wasmModuleEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[wasmHash]
	return e, ok
}

func (c *wasmModuleCache) put(wasmHash string, e wasmModuleEntry, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= wasmModuleCacheMax {
		evictOldest(c.entries, func(e wasmModuleEntry) time.Time { return e.cachedAt })
	}
	e.cachedAt = now
	c.entries[wasmHash] = e
}
