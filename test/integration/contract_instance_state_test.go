//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// instanceFixture returns a deterministic C-strkey and the base64 LedgerKey of
// its persistent ScvLedgerKeyContractInstance entry — the key the explorer's
// contract routes classify.
func instanceFixture(t *testing.T, tag string) (contractStrkey, instanceKeyXDR string) {
	t.Helper()
	sum := sha256.Sum256([]byte("contract-instance-state-it-" + tag))
	contractStrkey, err := strkey.Encode(strkey.VersionByteContract, sum[:])
	if err != nil {
		t.Fatalf("encode contract strkey: %v", err)
	}
	cid := xdr.ContractId(sum)
	key := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	instanceKeyXDR, err = xdr.MarshalBase64(key)
	if err != nil {
		t.Fatalf("marshal instance key: %v", err)
	}
	return contractStrkey, instanceKeyXDR
}

// TestContractInstanceState_TTLRowAndAbsence runs the explorer's instance read
// against a real ClickHouse: a TTL row for the instance key yields Known plus
// the newest live_until (so an archived instance is judged archived), and a
// contract with no lake evidence at all yields Known=false without error.
func TestContractInstanceState_TTLRowAndAbsence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	archivedContract, archivedKey := instanceFixture(t, "archived")
	neverDeployed, _ := instanceFixture(t, "never-deployed")
	lapsedAt := ttlAsOf - 1_000_000

	rows := []chstore.LedgerEntryChangeRow{
		ttlChangeRow(archivedKey, 72_100_001, 1, lapsedAt-5, 48),
		ttlChangeRow(archivedKey, 72_100_002, 1, lapsedAt, 48),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	defer func() { _ = er.Close() }()

	st, err := er.ContractInstanceState(ctx, archivedContract)
	if err != nil {
		t.Fatalf("ContractInstanceState(archived): %v", err)
	}
	if !st.Known || st.LiveUntil != lapsedAt {
		t.Fatalf("archived instance state = %+v, want Known with LiveUntil %d", st, lapsedAt)
	}
	if v := chstore.TTLVerdictAt(st.LiveUntil, ttlAsOf); v != chstore.TTLArchived {
		t.Errorf("verdict = %v, want TTLArchived", v)
	}

	st, err = er.ContractInstanceState(ctx, neverDeployed)
	if err != nil {
		t.Fatalf("ContractInstanceState(never deployed): %v", err)
	}
	if st.Known || st.LiveUntil != 0 {
		t.Errorf("never-deployed state = %+v, want zero", st)
	}
}
