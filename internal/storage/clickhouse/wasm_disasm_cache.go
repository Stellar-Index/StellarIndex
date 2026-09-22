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
	// buildWasmDisassembly used to append inline) — NOT the export-parse
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
		// Approximate LRU (oldest-inserted wins) — same tradeoff as
		// accountStateCache: one pass only when at capacity.
		var oldestKey string
		var oldestAt time.Time
		for k, existing := range c.entries {
			if oldestKey == "" || existing.cachedAt.Before(oldestAt) {
				oldestKey, oldestAt = k, existing.cachedAt
			}
		}
		delete(c.entries, oldestKey)
	}
	e.cachedAt = now
	c.entries[wasmHash] = e
}
