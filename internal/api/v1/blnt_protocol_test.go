package v1

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type blntCountingContractsReader struct{}

func (blntCountingContractsReader) ListProtocolContracts(context.Context, string) ([]timescale.ProtocolContract, error) {
	return nil, nil
}

func (blntCountingContractsReader) ListSourceContractsFromProjection(context.Context, string) ([]string, error) {
	return nil, nil
}

func (blntCountingContractsReader) ProtocolContractIndex(context.Context) (map[string]string, error) {
	return nil, nil
}

func (blntCountingContractsReader) CountSourceContracts(context.Context, string) (int64, bool, error) {
	return 2, true, nil
}

func TestBLNTProtocolRegistryIsIndependentFromBlend(t *testing.T) {
	meta, ok := protocolByName("blnt")
	if !ok {
		t.Fatal("blnt is absent from protocolRegistry")
	}
	if meta.Category != "lending" {
		t.Fatalf("blnt category = %q, want lending", meta.Category)
	}
	if meta.GenesisLedger != blntGenesisLedger {
		t.Fatalf("blnt genesis = %d, want %d", meta.GenesisLedger, blntGenesisLedger)
	}
	if len(meta.Factories) != 1 || meta.Factories[0] != blntPoolFactory {
		t.Fatalf("blnt factories = %v, want [%s]", meta.Factories, blntPoolFactory)
	}
	if len(meta.EventKinds) != 0 {
		t.Fatalf("blnt event kinds = %v, want none until a dedicated decoder lands", meta.EventKinds)
	}

	blendMeta, ok := protocolByName("blend")
	if !ok {
		t.Fatal("blend is absent from protocolRegistry")
	}
	for _, factory := range blendMeta.Factories {
		if factory == blntPoolFactory {
			t.Fatal("BLNT factory is attributed to Blend")
		}
	}
	for _, contract := range blendMeta.ExtraContracts {
		if contract == blntBackstop || contract == blntEmitter || contract == blntBackfill || contract == blntLockupBackfill {
			t.Fatalf("BLNT module %s is attributed to Blend", contract)
		}
	}
}

func TestBLNTStaticRosterAndContractIndex(t *testing.T) {
	meta, _ := protocolByName("blnt")
	s := New(Options{})

	roster, err := s.rosterErr(context.Background(), meta)
	if err != nil {
		t.Fatalf("rosterErr: %v", err)
	}
	want := map[string]string{
		blntFixedPool:      "instance",
		blntYieldBlentPool: "instance",
		blntBackstop:       "module",
		blntEmitter:        "module",
		blntBackfill:       "module",
		blntLockupBackfill: "module",
	}
	classifyContractKinds(roster, meta.Factories)
	for _, row := range roster {
		kind, ok := want[row.ContractID]
		if !ok {
			t.Fatalf("unexpected BLNT roster contract %s", row.ContractID)
		}
		if row.Kind != kind {
			t.Errorf("%s kind = %q, want %q", row.ContractID, row.Kind, kind)
		}
		delete(want, row.ContractID)
	}
	if len(want) != 0 {
		t.Fatalf("BLNT roster missing contracts: %v", want)
	}

	byID, complete, failed := s.buildContractProtocolIndex(context.Background())
	if !complete || len(failed) != 0 {
		t.Fatalf("contract index complete=%v failed=%v", complete, failed)
	}
	for _, id := range []string{blntPoolFactory, blntFixedPool, blntYieldBlentPool, blntBackstop, blntEmitter, blntBackfill, blntLockupBackfill} {
		if got := byID[id]; got != "blnt" {
			t.Errorf("contract index[%s] = %q, want blnt", id, got)
		}
	}
}

func TestBLNTContractCountUsesDeduplicatedRoster(t *testing.T) {
	meta, _ := protocolByName("blnt")
	s := New(Options{ProtocolContracts: blntCountingContractsReader{}})

	if count, ok, err := s.countedContractTotal(context.Background(), meta); err != nil || ok {
		t.Fatalf("countedContractTotal = (%d, %v, %v), want roster fallback", count, ok, err)
	}
}
