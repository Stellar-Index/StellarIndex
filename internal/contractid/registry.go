// Package contractid is the shared contract-identity registry that factory-anchored Soroban
// decoders use to gate Matches().
//
// Per ADR-0035 a decoder accepts only events from its protocol's contracts: the hard-coded factory
// plus every child it creates, recursively. Topic symbols (`swap`, `deploy`, ...) are NOT unique
// across protocols, so topic alone mis-attributes foreign events. [Registry.Seed] feeds it from
// live creation events, the DB warm and operator reconcile, and may run concurrently with reads.
package contractid

import "sync"

// Registry is a set of factory-descended child contract IDs plus the factory trust-root set, with an optional persistence hook; build with [New]. The root is a SET because a protocol can have several factories (Blend has two), and gating on one would drop the other's children.
type Registry struct {
	mu        sync.RWMutex
	set       map[string]struct{} // discovered children (factory descendants)
	factories map[string]struct{} // trust roots (hard-coded, verified)
	hook      func(childID, factoryID string, firstLedger uint32)
	attrs     map[string]Attrs // per-child metadata (e.g. token identities)
	attrHook  func(childID, factoryID string, firstLedger uint32, attrs Attrs)
}

// Attrs is optional per-child metadata a decoder persists beside the child id; values are opaque to the registry.
type Attrs map[string]string

// New constructs a Registry, applying any options (WithFactories, WithSeed, WithHook).
func New(opts ...Option) *Registry {
	r := &Registry{
		set:       make(map[string]struct{}),
		factories: make(map[string]struct{}),
		attrs:     make(map[string]Attrs),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Option configures a Registry at construction time.
type Option func(*Registry)

// WithFactories sets the factory trust-root set whose creation events the decoder honors; completeness is load-bearing, since a missing factory silently drops its children's events.
func WithFactories(factoryIDs []string) Option {
	return func(r *Registry) {
		for _, id := range factoryIDs {
			if id != "" {
				r.factories[id] = struct{}{}
			}
		}
	}
}

// WithSeed pre-loads the child set from `protocol_contracts`; it does not fire the persistence hook (already persisted).
func WithSeed(childIDs []string) Option {
	return func(r *Registry) {
		for _, id := range childIDs {
			if id != "" {
				r.set[id] = struct{}{}
			}
		}
	}
}

// WithHook installs the live-upsert callback Seed fires on a discovered child (child C-strkey, deploying factory, creation ledger). It runs on the decode path, so keep it cheap and non-blocking on the network; it is called without the registry lock held.
func WithHook(fn func(childID, factoryID string, firstLedger uint32)) Option {
	return func(r *Registry) { r.hook = fn }
}

// WithAttrSeed pre-loads children with their durable attributes; like [WithSeed] it fires no hook and admits each child into the gate.
func WithAttrSeed(seed map[string]Attrs) Option {
	return func(r *Registry) {
		for id, a := range seed {
			if id == "" {
				continue
			}
			r.set[id] = struct{}{}
			r.attrs[id] = a
		}
	}
}

// WithAttrHook installs the callback [Registry.SeedWithAttrs] fires to persist attributes; like [WithHook] it must be cheap and runs without the registry lock.
func WithAttrHook(fn func(childID, factoryID string, firstLedger uint32, attrs Attrs)) Option {
	return func(r *Registry) { r.attrHook = fn }
}

// Has reports whether contractID is a registered factory descendant: the gate Matches() consults for every non-creation event.
func (r *Registry) Has(contractID string) bool {
	r.mu.RLock()
	_, ok := r.set[contractID]
	r.mu.RUnlock()
	return ok
}

// IsFactory reports whether contractID is a trust-root factory; Matches() consults it for the creation event so only a genuine factory can announce a child.
func (r *Registry) IsFactory(contractID string) bool {
	r.mu.RLock()
	_, ok := r.factories[contractID]
	r.mu.RUnlock()
	return ok
}

// Factories returns the factory trust-root set, unordered; the genesis-seed walk replays each one's creation events.
func (r *Registry) Factories() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.factories))
	for id := range r.factories {
		out = append(out, id)
	}
	return out
}

// Children returns the registered descendant set (Has() members), unordered; with the factories ([Registry.GatedSet]) it is exactly what Matches() can accept, so the re-derive prefilter changes speed only.
func (r *Registry) Children() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.set))
	for id := range r.set {
		out = append(out, id)
	}
	return out
}

// GatedSet returns the factories plus registered children, unordered and deduped: every contract Matches() can accept, and the tightest sound prefilter for a re-derive. Sort for determinism.
func (r *Registry) GatedSet() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.set)+len(r.factories))
	for id := range r.set {
		out = append(out, id)
	}
	for id := range r.factories {
		if _, isChild := r.set[id]; !isChild {
			out = append(out, id)
		}
	}
	return out
}

// Seed registers a child (idempotent) and fires the persistence hook on every call, since the downstream upsert is idempotent; factoryID and firstLedger are provenance only, not used by the gate.
func (r *Registry) Seed(childID, factoryID string, firstLedger uint32) {
	if childID == "" {
		return
	}
	r.mu.Lock()
	r.set[childID] = struct{}{}
	hook := r.hook
	r.mu.Unlock()
	if hook != nil {
		hook(childID, factoryID, firstLedger)
	}
}

// SeedWithAttrs is [Registry.Seed] plus the child's attributes, kept in memory and passed to the attribute hook.
func (r *Registry) SeedWithAttrs(childID, factoryID string, firstLedger uint32, attrs Attrs) {
	if childID == "" {
		return
	}
	r.mu.Lock()
	r.attrs[childID] = attrs
	hook := r.attrHook
	r.mu.Unlock()
	r.Seed(childID, factoryID, firstLedger)
	if hook != nil {
		hook(childID, factoryID, firstLedger, attrs)
	}
}

// AllAttrs returns a copy of every child's retained attributes.
func (r *Registry) AllAttrs() map[string]Attrs {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]Attrs, len(r.attrs))
	for id, a := range r.attrs {
		out[id] = a
	}
	return out
}

// Len returns the number of registered children. Operator/test visibility.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.set)
}
