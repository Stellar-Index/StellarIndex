package blnt

import (
	"encoding/base64"
	"errors"
	"testing"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

func testEvent(contractID, kind string) events.Event {
	return events.Event{
		Ledger:         GenesisLedger,
		LedgerClosedAt: "2026-10-10T00:00:00Z",
		ContractID:     contractID,
		TxHash:         "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Topic:          []string{scval.MustEncodeSymbol(kind)},
		Value:          scval.MustEncodeSymbol("body"),
	}
}

func TestDecoderMatchesOnlyRoleSpecificCanonicalEvents(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	tests := []struct {
		name     string
		contract string
		kind     string
		want     bool
	}{
		{"pool supply", MainnetFixedPool, "supply", true},
		{"second pool clawback", MainnetYieldBlentPool, "clawback", true},
		{"factory deploy", MainnetPoolFactory, "deploy", true},
		{"backstop tier deposit", MainnetBackstop, "deposit", true},
		{"emitter distribute", MainnetEmitter, "distribute", true},
		{"backfill conversion", MainnetBackfill, "swap_blnd", true},
		{"lockup funding", MainnetLockupBackfill, "fund", true},
		{"pool symbol on backfill", MainnetBackfill, "supply", false},
		{"backfill symbol on pool", MainnetFixedPool, "swap_blnd", false},
		{"foreign lookalike", "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4", "supply", false},
		{"unknown symbol", MainnetFixedPool, "not_blnt", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := d.Matches(testEvent(tc.contract, tc.kind)); got != tc.want {
				t.Errorf("Matches(%s, %s) = %v, want %v", tc.contract, tc.kind, got, tc.want)
			}
		})
	}
}

func TestRoleEventCatalogueClassifiesEveryDeclaredKind(t *testing.T) {
	t.Parallel()
	for role, kinds := range roleEvents {
		for _, kind := range kinds {
			ev := testEvent(MainnetFixedPool, kind)
			if got := classify(role, &ev); got != kind {
				t.Errorf("classify(%s, %s) = %q", role, kind, got)
			}
		}
	}
}

func TestDecoderDecodePreservesWireAndRole(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	ev := testEvent(MainnetBackstop, "claim")
	ev.Topic = append(ev.Topic, scval.MustEncodeSymbol("FirstLoss"))
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("Decode outputs = %d, want 1", len(out))
	}
	got := out[0].(Event)
	if got.Role != RoleBackstop || got.EventType != "claim" || got.EventKind() != BackstopEventKind {
		t.Fatalf("decoded identity = role %q type %q kind %q", got.Role, got.EventType, got.EventKind())
	}
	if len(got.TopicsXDR) != len(ev.Topic) || got.TopicsXDR[1] != ev.Topic[1] || got.ValueXDR != ev.Value {
		t.Fatal("Decode did not preserve the complete event wire")
	}
}

func TestDecoderRejectsMalformedXDR(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	ev := testEvent(MainnetFixedPool, "supply")
	ev.Value = "not-base64"
	_, err := d.Decode(ev)
	if !errors.Is(err, ErrMalformedEvent) {
		t.Fatalf("Decode error = %v, want ErrMalformedEvent", err)
	}
}

func TestFactoryDeployAdmitsChildPool(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	body, child := encodedContractAddress(t, 0x42)
	deploy := testEvent(MainnetPoolFactory, "deploy")
	deploy.Value = body
	if _, err := d.Decode(deploy); err != nil {
		t.Fatalf("Decode(deploy): %v", err)
	}
	if !d.Matches(testEvent(child, "supply")) {
		t.Fatalf("factory child %s was not admitted as a pool", child)
	}
}

func encodedContractAddress(t *testing.T, fill byte) (string, string) {
	t.Helper()
	var cid sdkxdr.ContractId
	for i := range cid {
		cid[i] = fill
	}
	addr := sdkxdr.ScAddress{Type: sdkxdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
	sv := sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvAddress, Address: &addr}
	raw, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal contract address: %v", err)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)
	parsed, err := scval.Parse(b64)
	if err != nil {
		t.Fatalf("parse encoded address: %v", err)
	}
	strkey, err := scval.AsAddressStrkey(parsed)
	if err != nil {
		t.Fatalf("strkey encoded address: %v", err)
	}
	return b64, strkey
}
