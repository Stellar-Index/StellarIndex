// Package domain holds the persisted data shapes that the storage tier and the compute/source
// packages share, so storage never imports upward (lint-imports.sh rule L/storage-below-compute).
//
// It is a leaf: nothing internal beyond internal/canonical. Origin packages keep their public
// names: a method-free type re-exports as an alias (`type StoredEvent = domain.MEVStoredEvent`);
// one with methods is a defined type over the shape (`type Observation domain.AccountObservation`),
// converted explicitly at the storage boundary, which the compiler enforces.
//
// Only plain persisted shapes belong here: no compute logic, no Sink/Scanner interfaces and no
// types whose fields pull in policy. Those stay grandfathered in scripts/ci/lint-imports.baseline.
package domain
