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

// TestProcessLedger_EvictedKeysReachTheEntryDecoders pins that Soroban state archival is the one way a ledger entry
// leaves the live state WITHOUT a transaction touching it: when its TTL
// lapses, core evicts it at ledger close and reports it only in the
// LedgerCloseMeta's evicted-keys list. The three-phase walk reads
// transaction meta, so it could never see one.
//
// The consequence was not cosmetic. The decoders handled Restored — the
// other half of the same lifecycle — so an archived SAC balance's last
// write stood as the holder's current balance forever and the served supply
// component stayed permanently above the truth with no path to
// self-correct.
func TestProcessLedger_EvictedKeysReachTheEntryDecoders(t *testing.T) {
	env, proc := mkEntryWalkTx(t, 0x55, true, nil, []xdr.LedgerEntryChange{accountBalanceChange(1000)})
	lcm := mkEntryWalkLedger(4244,
		[]xdr.TransactionEnvelope{env},
		[]xdr.TransactionResultMeta{proc})
	dataKey := evictedContractDataKey(0x9e)
	lcm.V1.EvictedKeys = []xdr.LedgerKey{evictedTTLKey(0x9e), dataKey}

	spy := &entryChangeSpy{}
	d := New()
	d.AddEntryDecoder(spy)

	if _, err := d.ProcessLedger(lcm, testPassphrase); err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}

	var (
		evicted   *LedgerEntryChangeContext
		sawTTLKey bool
		maxTxSeq  uint32
		txChanges int
	)
	for i := range spy.seen {
		ctx := spy.seen[i]
		if ctx.Change.Type != xdr.LedgerEntryChangeTypeLedgerEntryRemoved || ctx.Change.Removed == nil {
			txChanges++
			if ctx.IntraLedgerSeq > maxTxSeq {
				maxTxSeq = ctx.IntraLedgerSeq
			}
			continue
		}
		switch ctx.Change.Removed.Type {
		case xdr.LedgerEntryTypeContractData:
			evicted = &spy.seen[i]
		case xdr.LedgerEntryTypeTtl:
			sawTTLKey = true
		default:
			t.Errorf("unexpected removed key type %v", ctx.Change.Removed.Type)
		}
	}

	if evicted == nil {
		t.Fatalf("the ledger's evicted contract-data key never reached the entry decoders; "+
			"saw %d changes — an evicted SAC balance then stays in the served supply forever",
			len(spy.seen))
	}
	if got, want := *evicted.Change.Removed.ContractData, *dataKey.ContractData; got.Contract != want.Contract ||
		got.Durability != want.Durability {
		t.Errorf("evicted change carries key %+v, want %+v", got, want)
	}
	if !sawTTLKey {
		t.Error("the paired TTL key was dropped; the walk must dispatch the eviction list as core reports it " +
			"and let each decoder's Matches discard what it does not watch")
	}
	if evicted.TxHash != "" || evicted.OpIndex != -1 {
		t.Errorf("evicted change stamped tx_hash=%q op_index=%d, want \"\" / -1 — "+
			"eviction is applied at ledger close and belongs to no transaction",
			evicted.TxHash, evicted.OpIndex)
	}
	if txChanges == 0 {
		t.Fatal("no transaction-phase change was walked; the fixture is not exercising the ordering assertion")
	}
	if evicted.IntraLedgerSeq <= maxTxSeq {
		t.Errorf("evicted change has IntraLedgerSeq %d <= the last transaction-phase change at %d: "+
			"a balance written earlier in this same ledger would win the last-writer-wins upsert "+
			"and the eviction would be discarded", evicted.IntraLedgerSeq, maxTxSeq)
	}
}

// TestProcessLedger_NoEvictionsEmitsNothingExtra keeps the negative half
// honest: a ledger with an empty eviction list must walk exactly the
// transaction-phase changes it always did. The eviction phase is additive,
// which is also why it does not renumber IntraLedgerSeq (see
// [EntryWalkVersion]).
func TestProcessLedger_NoEvictionsEmitsNothingExtra(t *testing.T) {
	env, proc := mkEntryWalkTx(t, 0x66, true,
		[]xdr.LedgerEntryChange{accountBalanceChange(990)},
		[]xdr.LedgerEntryChange{accountBalanceChange(500)})
	lcm := mkEntryWalkLedger(4245,
		[]xdr.TransactionEnvelope{env},
		[]xdr.TransactionResultMeta{proc})

	spy := &entryChangeSpy{}
	d := New()
	d.AddEntryDecoder(spy)

	if _, err := d.ProcessLedger(lcm, testPassphrase); err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}
	if len(spy.seen) != 2 {
		t.Fatalf("walked %d changes, want exactly the 2 transaction-phase changes", len(spy.seen))
	}
	for i, ctx := range spy.seen {
		if got := balanceOf(ctx.Change); got != []int64{990, 500}[i] {
			t.Errorf("change %d = balance %d, want %d (order unchanged by the eviction phase)",
				i, got, []int64{990, 500}[i])
		}
		if ctx.IntraLedgerSeq != uint32(i) {
			t.Errorf("change %d has IntraLedgerSeq %d, want %d", i, ctx.IntraLedgerSeq, i)
		}
	}
}

// TestProcessLedger_UpgradesAreCountedAndLogged pins that a ledger's
// upgrade entries are observed (counted, logged with the ledger) without
// reaching any entry decoder.
func TestProcessLedger_UpgradesAreCountedAndLogged(t *testing.T) {
	env, proc := mkEntryWalkTx(t, 0x56, true, nil, []xdr.LedgerEntryChange{accountBalanceChange(1000)})
	lcm := mkEntryWalkLedger(4245,
		[]xdr.TransactionEnvelope{env},
		[]xdr.TransactionResultMeta{proc})
	ver, reserve := xdr.Uint32(30), xdr.Uint32(5000000)
	lcm.V1.UpgradesProcessing = []xdr.UpgradeEntryMeta{
		{Upgrade: xdr.LedgerUpgrade{Type: xdr.LedgerUpgradeTypeLedgerUpgradeVersion, NewLedgerVersion: &ver}},
		{Upgrade: xdr.LedgerUpgrade{Type: xdr.LedgerUpgradeTypeLedgerUpgradeBaseReserve, NewBaseReserve: &reserve}},
	}

	var buf bytes.Buffer
	spy := &entryChangeSpy{}
	d := New()
	d.SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	d.AddEntryDecoder(spy)

	if _, err := d.ProcessLedger(lcm, testPassphrase); err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}
	if got := d.Stats().LedgerUpgradeEntries; got != 2 {
		t.Errorf("Stats().LedgerUpgradeEntries = %d, want 2", got)
	}
	if log := buf.String(); !strings.Contains(log, "ledger=4245") || !strings.Contains(log, "LedgerUpgradeVersion") {
		t.Errorf("want an upgrade log naming ledger 4245 and its types, got %q", log)
	}
	if len(spy.seen) != 1 {
		t.Errorf("decoders saw %d changes, want only the 1 transaction change", len(spy.seen))
	}
}

// TestProcessLedger_UpgradesCountedWithoutEntryDecoders pins that upgrade
// observation does not depend on any entry decoder being registered.
func TestProcessLedger_UpgradesCountedWithoutEntryDecoders(t *testing.T) {
	env, proc := mkEntryWalkTx(t, 0x57, true, nil, []xdr.LedgerEntryChange{accountBalanceChange(1000)})
	lcm := mkEntryWalkLedger(4246,
		[]xdr.TransactionEnvelope{env},
		[]xdr.TransactionResultMeta{proc})
	ver := xdr.Uint32(30)
	lcm.V1.UpgradesProcessing = []xdr.UpgradeEntryMeta{
		{Upgrade: xdr.LedgerUpgrade{Type: xdr.LedgerUpgradeTypeLedgerUpgradeVersion, NewLedgerVersion: &ver}},
	}

	d := New()
	if _, err := d.ProcessLedger(lcm, testPassphrase); err != nil {
		t.Fatalf("ProcessLedger: %v", err)
	}
	if got := d.Stats().LedgerUpgradeEntries; got != 1 {
		t.Errorf("Stats().LedgerUpgradeEntries = %d, want 1", got)
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
