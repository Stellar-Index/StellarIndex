package orchestrator

// fakeDecimalsLookup is a trivial aggregate.DecimalsLookup for tests —
// mirrors internal/api/v1's NonstandardDecimalsCache.Lookup shape without
// pulling in a storage dependency.
type fakeDecimalsLookup map[string]int

func (f fakeDecimalsLookup) Lookup(assetID string) (int, bool) {
	d, ok := f[assetID]
	return d, ok
}
