package xdrjson_test

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

func TestParticipantAccounts_Payment(t *testing.T) {
	const dest = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	body, _ := xdr.NewOperationBody(xdr.OperationTypePayment, xdr.PaymentOp{
		Destination: xdr.MustMuxedAddress(dest),
		Asset:       xdr.MustNewNativeAsset(),
		Amount:      1,
	})
	b64, _ := xdr.MarshalBase64(body)
	got, err := xdrjson.ParticipantAccounts(b64)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || got[0] != dest {
		t.Errorf("participants = %v, want [%s]", got, dest)
	}
}

func TestParticipantAccounts_NoneForSelfContained(t *testing.T) {
	// manage_data has no counterparty account field → no participants.
	body, _ := xdr.NewOperationBody(xdr.OperationTypeManageData, xdr.ManageDataOp{DataName: "k"})
	b64, _ := xdr.MarshalBase64(body)
	got, _ := xdrjson.ParticipantAccounts(b64)
	if len(got) != 0 {
		t.Errorf("participants = %v, want none", got)
	}
}

// TestParticipantAccounts_ManageDataNameLooksLikeAccount is the W8.3 regression:
// a manage_data DataName / value that happens to spell a valid G-strkey must
// NOT inject that (attacker-chosen) address into another account's history. The
// pre-fix generic "any IsAccountID string field" scan added it as a participant;
// the type-aware extractor knows manage_data carries no account field.
func TestParticipantAccounts_ManageDataNameLooksLikeAccount(t *testing.T) {
	value := xdr.DataValue(gAddr2)
	body, _ := xdr.NewOperationBody(xdr.OperationTypeManageData, xdr.ManageDataOp{
		DataName:  xdr.String64(gAddr), // a valid G-strkey as opaque free text
		DataValue: &value,
	})
	b64, _ := xdr.MarshalBase64(body)
	got, err := xdrjson.ParticipantAccounts(b64)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("participants = %v, want none (manage_data free text must not become a participant)", got)
	}
}

// TestParticipantAccounts_InvokeContractAddressVsStringArg proves the same rule
// for Soroban: a genuine ScVal::Address account arg IS a participant, but a
// string arg that merely spells a valid G-strkey is NOT.
func TestParticipantAccounts_InvokeContractAddressVsStringArg(t *testing.T) {
	var contractID xdr.ContractId
	for i := range contractID {
		contractID[i] = byte(i)
	}
	acct := xdr.MustAddress(gAddr)
	addrVal, err := xdr.NewScVal(xdr.ScValTypeScvAddress, xdr.ScAddress{
		Type:      xdr.ScAddressTypeScAddressTypeAccount,
		AccountId: &acct,
	})
	if err != nil {
		t.Fatalf("NewScVal address: %v", err)
	}
	// A string arg whose text is a valid G-strkey — free text, not an account.
	strVal, err := xdr.NewScVal(xdr.ScValTypeScvString, xdr.ScString(gAddr2))
	if err != nil {
		t.Fatalf("NewScVal string: %v", err)
	}
	hf, err := xdr.NewHostFunction(
		xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
		xdr.InvokeContractArgs{
			ContractAddress: xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &contractID},
			FunctionName:    "transfer",
			Args:            []xdr.ScVal{addrVal, strVal},
		},
	)
	if err != nil {
		t.Fatalf("NewHostFunction: %v", err)
	}
	b64 := mustBody(t, xdr.OperationTypeInvokeHostFunction, xdr.InvokeHostFunctionOp{HostFunction: hf})
	got, err := xdrjson.ParticipantAccounts(b64)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || got[0] != gAddr {
		t.Errorf("participants = %v, want [%s] (Address arg included, string arg excluded)", got, gAddr)
	}
}
