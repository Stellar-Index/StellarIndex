// Package contractid provides the shared contract-identity registry
// that factory-anchored Soroban decoders use to gate Matches().
//
// Per ADR-0035, a decoder must only accept events from contracts that belong to its protocol: the
// protocol's factory (a hard-coded trust root) plus every child the factory creates, recursively.
// Topic symbols (`swap`, `deploy`, ...) are NOT unique across protocols, so topic alone
// mis-attributes foreign contracts' events.
//
// A Registry is seeded three ways, all through [Registry.Seed]: live (a factory creation event,
// which fires the persistence hook), DB warm (`protocol_contracts` via WithSeed), and
// genesis/reconcile (operator tooling walks the lake). Seed may run concurrently with reads, so
// the set is mutex-guarded.
package contractid

import "sync"

// Registry is a set of factory-descended child contract IDs (C-strkeys)
// plus the protocol's factory trust-root set, with an optional live-upsert
// persistence hook. The zero value is not usable — construct with [New].
//
// A protocol may have MORE THAN ONE factory (Blend has two pool factories that both deploy real
// pools). Gating the creation event on a single factory would silently drop the other's children,
// so the factory trust root is a SET, not a scalar.
type Registry struct {
	mu        sync.RWMutex
	set       map[string]struct{} // discovered children (factory descendants)
	factories map[string]struct{} // trust roots (hard-coded, verified)
	hook      func(childID, factoryID string, firstLedger uint32)
	attrs     map[string]Attrs // per-child metadata (e.g. token identities)
	attrHook  func(childID, factoryID string, firstLedger uint32, attrs Attrs)
}

// Attrs is optional per-child metadata a decoder persists beside the child id
// (a pool's token identities, say). Values are opaque strings to the registry.
type Attrs map[string]string

// New constructs a Registry, applying any options (WithFactories, WithSeed,
// WithHook).
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

// WithFactories sets the protocol's factory trust-root set: the contract IDs whose creation events
// the decoder honors and from which every child is seeded. Its completeness is load-bearing: a
// missing factory means its children's events are silently dropped.
func WithFactories(factoryIDs []string) Option {
	return func(r *Registry) {
		for _, id := range factoryIDs {
			if id != "" {
				r.factories[id] = struct{}{}
			}
		}
	}
}

// WithSeed pre-loads the child set from a durable warm (the
// `protocol_contracts` table). Does NOT fire the persistence hook — these
// IDs are already persisted. Safe to combine with WithHook.
func WithSeed(childIDs []string) Option {
	return func(r *Registry) {
		for _, id := range childIDs {
			if id != "" {
				r.set[id] = struct{}{}
			}
		}
	}
}

// WithHook installs the live-upsert callback fired by Seed when a factory creation event discovers
// a child. It receives the child's C-strkey, the deploying factory's C-strkey (provenance; a
// protocol can have several factories) and the creation ledger. It runs on the decode path, so
// keep it cheap: a timeout-bounded ExecContext is fine, a blocking network call is not. It is
// invoked WITHOUT the registry lock held.
func WithHook(fn func(childID, factoryID string, firstLedger uint32)) Option {
	return func(r *Registry) { r.hook = fn }
}

// WithAttrSeed pre-loads children together with their durable attributes. Like
// [WithSeed] it fires no hook and admits each child into the gate.
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

// WithAttrHook installs the callback [Registry.SeedWithAttrs] fires to persist
// a child's attributes. Same constraints as [WithHook]: cheap, and invoked
// without the registry lock held.
func WithAttrHook(fn func(childID, factoryID string, firstLedger uint32, attrs Attrs)) Option {
	return func(r *Registry) { r.attrHook = fn }
}

// Has reports whether contractID is a registered factory descendant.
// This is the load-bearing gate the decoder's Matches() consults for
// every non-creation event.
func (r *Registry) Has(contractID string) bool {
	r.mu.RLock()
	_, ok := r.set[contractID]
	r.mu.RUnlock()
	return ok
}

// IsFactory reports whether contractID is one of the protocol's trust-root
// factories. The decoder's Matches() consults this for the creation event
// (e.g. Blend `deploy`) — only a genuine factory may announce a child, so a
// foreign contract can't inject one into the registry.
func (r *Registry) IsFactory(contractID string) bool {
	r.mu.RLock()
	_, ok := r.factories[contractID]
	r.mu.RUnlock()
	return ok
}

// Factories returns the protocol's factory trust-root set (sorted-agnostic;
// callers needing order should sort). Used by the genesis-seed walk to
// enumerate every factory whose creation events to replay.
func (r *Registry) Factories() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.factories))
	for id := range r.factories {
		out = append(out, id)
	}
	return out
}

// Children returns the registered factory-descendant set (the gate's Has() members) in no
// guaranteed order. The completeness re-derive uses it as the contract-id prefilter for the CH lake
// read; with the factories ([Registry.GatedSet]) it is exactly what Matches() can accept, so the
// prefilter changes speed only.
func (r *Registry) Children() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.set))
	for id := range r.set {
		out = append(out, id)
	}
	return out
}

// GatedSet returns the union of the factory trust roots and the registered children: every contract
// whose events Matches() can accept. It is the tightest sound contract-id prefilter for a re-derive
// because factories must stream so their creation events self-seed children in-window, and children
// so their business events stream. It is deduped and unordered; sort for determinism.
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

// Seed registers a child contract (idempotent) and fires the persistence hook, if any. factoryID is
// the creation event's emitter and firstLedger its ledger; both are provenance only, not used by
// the gate. The hook fires on every call (the upsert downstream is idempotent), so a re-observed
// creation event refreshes the durable row harmlessly.
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

// SeedWithAttrs is [Registry.Seed] plus the child's attributes, which are
// retained in memory and passed to the attribute hook (if any).
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
