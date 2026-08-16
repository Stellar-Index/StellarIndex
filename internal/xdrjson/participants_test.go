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

// addrArg builds an ScVal::Address argument for the given account G-strkey.
func addrArg(t *testing.T, g string) xdr.ScVal {
	t.Helper()
	acct := xdr.MustAddress(g)
	v, err := xdr.NewScVal(xdr.ScValTypeScvAddress, xdr.ScAddress{
		Type:      xdr.ScAddressTypeScAddressTypeAccount,
		AccountId: &acct,
	})
	if err != nil {
		t.Fatalf("NewScVal address: %v", err)
	}
	return v
}

// scContract builds a contract ScAddress from a ContractId.
func scContract(id xdr.ContractId) xdr.ScAddress {
	return xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id}
}

// addrAuthEntry builds a SorobanAuthorizationEntry whose Address credential
// names account g. Note an attacker can author this for ANY g with a garbage
// inner signature: at the XDR-decode layer the signature is unverified (the
// network only verifies it during apply, for consumed entries on successful
// txs), so a decoded auth entry naming g does NOT prove g authorized anything.
func addrAuthEntry(t *testing.T, contractID xdr.ContractId, g string) xdr.SorobanAuthorizationEntry {
	t.Helper()
	acct := xdr.MustAddress(g)
	creds, err := xdr.NewSorobanCredentials(
		xdr.SorobanCredentialsTypeSorobanCredentialsAddress,
		xdr.SorobanAddressCredentials{
			Address:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &acct},
			Signature: xdr.ScVal{Type: xdr.ScValTypeScvVoid},
		},
	)
	if err != nil {
		t.Fatalf("NewSorobanCredentials: %v", err)
	}
	fn, err := xdr.NewSorobanAuthorizedFunction(
		xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn,
		xdr.InvokeContractArgs{ContractAddress: scContract(contractID), FunctionName: "transfer"},
	)
	if err != nil {
		t.Fatalf("NewSorobanAuthorizedFunction: %v", err)
	}
	return xdr.SorobanAuthorizationEntry{
		Credentials:    creds,
		RootInvocation: xdr.SorobanAuthorizedInvocation{Function: fn},
	}
}

func testContractID() xdr.ContractId {
	var id xdr.ContractId
	for i := range id {
		id[i] = byte(i)
	}
	return id
}

// TestParticipantAccounts_InvokeContractAddressVsStringArg documents the Soroban
// InvokeContract discrimination and its final resolution. #79 added a per-TYPE
// allowlist so a string arg spelling a valid G-strkey is NOT a participant while
// an ScVal::Address arg WAS. That Address-arg inclusion was itself an injection
// vector (args are attacker-controlled call data), so the fix now excludes BOTH:
// the string arg (the retained #79 free-text defense) AND the Address arg. An
// InvokeContract op contributes no arg-derived participant at all.
func TestParticipantAccounts_InvokeContractAddressVsStringArg(t *testing.T) {
	contractID := testContractID()
	addrVal := addrArg(t, gAddr)
	// A string arg whose text is a valid G-strkey — free text, not an account.
	strVal, err := xdr.NewScVal(xdr.ScValTypeScvString, xdr.ScString(gAddr2))
	if err != nil {
		t.Fatalf("NewScVal string: %v", err)
	}
	hf, err := xdr.NewHostFunction(
		xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
		xdr.InvokeContractArgs{
			ContractAddress: scContract(contractID),
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
	if len(got) != 0 {
		t.Errorf("participants = %v, want none (neither the Address arg nor the string arg may be a participant)", got)
	}
}

// TestParticipantAccounts_InvokeContractAuthoredAuthDoesNotParticipate is the
// unconditional regression: neither an InvokeContract Address ARG nor a self-
// authored op.Auth Address entry naming a victim may make that victim a
// participant. Both are attacker-controllable at the XDR-decode layer — an auth-
// entry signature is verified only by the network during apply (and only for
// consumed require_auth entries on SUCCESSFUL txs), while the indexer decodes
// failed-tx op bodies too (extract.go calls this UNCONDITIONALLY). So an attacker
// builds a failing tx carrying a victim Address arg AND an Address auth entry
// naming the victim (garbage inner signature, no victim key needed); a consent
// gate on op.Auth would wrongly include the victim. The correct behavior is to
// include NEITHER.
//
// This subcase is red on origin/main's blanket #79 code (the arg loop includes
// the victim) AND on the superseded consent-gate commit (it reads the victim out
// of op.Auth) — the stronger, unconditional property both of those miss.
func TestParticipantAccounts_InvokeContractAuthoredAuthDoesNotParticipate(t *testing.T) {
	contractID := testContractID()
	// Victim (gAddr2) as an Address arg AND as the named authorizer in a self-
	// authored auth entry — the strongest attacker construction.
	hf, err := xdr.NewHostFunction(
		xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
		xdr.InvokeContractArgs{
			ContractAddress: scContract(contractID),
			FunctionName:    "transfer",
			Args:            []xdr.ScVal{addrArg(t, gAddr2)},
		},
	)
	if err != nil {
		t.Fatalf("NewHostFunction: %v", err)
	}
	b64 := mustBody(t, xdr.OperationTypeInvokeHostFunction, xdr.InvokeHostFunctionOp{
		HostFunction: hf,
		Auth:         []xdr.SorobanAuthorizationEntry{addrAuthEntry(t, contractID, gAddr2)},
	})
	got, err := xdrjson.ParticipantAccounts(b64)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("participants = %v, want none (victim %s must NOT be injectable via arg OR authored auth)", got, gAddr2)
	}
}
