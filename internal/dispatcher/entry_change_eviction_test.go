package dispatcher

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// evictedContractDataKey is the ledger key core reports for an archived
// contract-data entry: the entry's own key, with no body (the entry is
// gone from the live state, so there is nothing to carry).
func evictedContractDataKey(seed byte) xdr.LedgerKey {
	var raw [32]byte
	raw[0] = seed
	cid := xdr.ContractId(raw)
	sym := xdr.ScSymbol("Balance")
	return xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract: xdr.ScAddress{
				Type:       xdr.ScAddressTypeScAddressTypeContract,
				ContractId: &cid,
			},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
}

// evictedTTLKey is the TTL key core evicts alongside the data key. No
// decoder watches it; it is here so the test walks the real shape of the
// list rather than a one-element idealisation of it.
func evictedTTLKey(seed byte) xdr.LedgerKey {
	var raw [32]byte
	raw[0] = seed
	return xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeTtl,
		Ttl:  &xdr.LedgerKeyTtl{KeyHash: xdr.Hash(raw)},
	}
}

// failingEvictedKeys is an evicted-key source whose read fails, the shape a
// future SDK would take if it started erroring instead of panicking.
type failingEvictedKeys struct{}

func (failingEvictedKeys) EvictedLedgerKeys() ([]xdr.LedgerKey, error) {
	return nil, errors.New("unsupported LedgerCloseMeta version")
}

// TestWalkEvictedKeys_UnreadableListIsCountedAndLogged pins that a failed
// evicted-key read is never silent: the ledger still lands, but the skip is
// counted for the alert and logged with the ledger a replay has to cover.
// Each dropped eviction would otherwise leave a served balance above the
// truth with nothing distinguishing it from a ledger that evicted nothing.
func TestWalkEvictedKeys_UnreadableListIsCountedAndLogged(t *testing.T) {
	var buf bytes.Buffer
	d := New()
	d.SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))

	dispatched := 0
	outs := d.walkEvictedKeys(failingEvictedKeys{}, 4244, func(int, xdr.LedgerEntryChange) []consumer.Event {
		dispatched++
		return nil
	})

	if outs != nil || dispatched != 0 {
		t.Fatalf("unreadable list dispatched %d changes / %d events, want none", dispatched, len(outs))
	}
	if got := d.Stats().EvictedKeysUnreadable; got != 1 {
		t.Errorf("Stats().EvictedKeysUnreadable = %d, want 1", got)
	}
	if log := buf.String(); !strings.Contains(log, "level=WARN") || !strings.Contains(log, "ledger=4244") {
		t.Errorf("want a WARN naming ledger 4244, got %q", log)
	}
}
