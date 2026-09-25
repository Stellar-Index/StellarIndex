package diagnostics

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
)

// TestBuildVerifyDispatcher_unionsProtocolContractsSeed pins CA2-A28:
// a pool admitted only via protocol_contracts (not yet in the
// in-code curated set) must be visible to verify-decoders when
// -seed-protocol-contracts is used, mirroring production's
// GatedRegistryOptions gate. Without the union, verify-decoders
// would report this pool's decoder silent even though it is emitting
// — a false diagnosis of a live decoder as dead.
func TestBuildVerifyDispatcher_unionsProtocolContractsSeed(t *testing.T) {
	const laterPool = "CBQ7Y7VDT6IHOWL7X5JQE5U3JFHFQD5H6QNRPY3EM52ELHKGXAA6UWSN" // not in comet.MainnetGatedSet()
	ev := events.Event{ContractID: laterPool, Topic: []string{comet.TopicSymbolPool, comet.TopicSymbolSwap}}

	disp, _, _ := buildVerifyDispatcher(config.OracleConfig{}, nil)
	if name, ok := disp.Recognize(ev); ok {
		t.Fatalf("pool %s must not match on the curated set alone, matched %q", laterPool, name)
	}

	seeded, _, _ := buildVerifyDispatcher(config.OracleConfig{}, map[string][]string{
		comet.SourceName: {laterPool},
	})
	if name, ok := seeded.Recognize(ev); !ok || name != comet.SourceName {
		t.Fatalf("pool %s must match comet once seeded from protocol_contracts, got name=%q ok=%v", laterPool, name, ok)
	}
}
