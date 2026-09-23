package xdrjson_test

import (
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

const (
	gAddr  = "GA76J4PNGDYNW53RRKKY72IU5NVHTZN6GLHWCYZ2Z6L63XYMHYSTP4J2"
	gAddr2 = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
)

func mustBody(t *testing.T, typ xdr.OperationType, v any) string {
	t.Helper()
	body, err := xdr.NewOperationBody(typ, v)
	if err != nil {
		t.Fatalf("NewOperationBody: %v", err)
	}
	b64, err := xdr.MarshalBase64(body)
	if err != nil {
		t.Fatalf("MarshalBase64: %v", err)
	}
	return b64
}

func TestDecodeOperationBody_Payment(t *testing.T) {
	b64 := mustBody(t, xdr.OperationTypePayment, xdr.PaymentOp{
		Destination: xdr.MustMuxedAddress(gAddr),
		Asset:       xdr.MustNewNativeAsset(),
		Amount:      12345,
	})
	d, err := xdrjson.DecodeOperationBody(b64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d.Type != "payment" {
		t.Errorf("type = %q, want payment", d.Type)
	}
	if d.Fields["destination"] != gAddr {
		t.Errorf("destination = %v", d.Fields["destination"])
	}
	if d.Fields["asset"] != "native" {
		t.Errorf("asset = %v, want native", d.Fields["asset"])
	}
	if d.Fields["amount"] != "12345" {
		t.Errorf("amount = %v, want string 12345", d.Fields["amount"])
	}
}

func TestDecodeOperationBody_CreditAsset(t *testing.T) {
	credit := xdr.MustNewCreditAsset("USDC", gAddr2)
	b64 := mustBody(t, xdr.OperationTypePayment, xdr.PaymentOp{
		Destination: xdr.MustMuxedAddress(gAddr),
		Asset:       credit,
		Amount:      1,
	})
	d, _ := xdrjson.DecodeOperationBody(b64)
	if d.Fields["asset"] != "USDC-"+gAddr2 {
		t.Errorf("asset = %v, want USDC-<issuer> (dash form)", d.Fields["asset"])
	}
}

func TestDecodeOperationBody_ManageSellOffer(t *testing.T) {
	b64 := mustBody(t, xdr.OperationTypeManageSellOffer, xdr.ManageSellOfferOp{
		Selling: xdr.MustNewNativeAsset(),
		Buying:  xdr.MustNewCreditAsset("USDC", gAddr2),
		Amount:  500,
		Price:   xdr.Price{N: 7, D: 2},
		OfferId: 99,
	})
	d, err := xdrjson.DecodeOperationBody(b64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d.Type != "manage_sell_offer" {
		t.Errorf("type = %q", d.Type)
	}
	if d.Fields["selling"] != "native" || d.Fields["amount"] != "500" {
		t.Errorf("fields = %+v", d.Fields)
	}
	pr, ok := d.Fields["price"].(map[string]any)
	if !ok || pr["n"] != int32(7) || pr["d"] != int32(2) {
		t.Errorf("price = %+v", d.Fields["price"])
	}
}

func TestOpTypeName_Unknown(t *testing.T) {
	if got := xdrjson.OpTypeName(xdr.OperationType(9999)); got != "unknown_9999" {
		t.Errorf("got %q", got)
	}
}

func TestMemoTypeName(t *testing.T) {
	cases := map[string]string{
		"MemoTypeMemoNone": "none",
		"MemoTypeMemoText": "text",
		"MemoTypeMemoId":   "id",
		"MemoTypeMemoHash": "hash",
		"":                 "none",
	}
	for in, want := range cases {
		if got := xdrjson.MemoTypeName(in); got != want {
			t.Errorf("MemoTypeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeOperationBody_InvokeHostFunction_Args(t *testing.T) {
	// Build swap(to: Address, amount: i128 > 2^63, path: Vec<Symbol>) —
	// exercises the strkey, big-integer, and container display paths.
	var contractID xdr.ContractId
	for i := range contractID {
		contractID[i] = byte(i)
	}
	toAddr := xdr.MustAddress(gAddr)
	addrVal, err := xdr.NewScVal(xdr.ScValTypeScvAddress, xdr.ScAddress{
		Type:      xdr.ScAddressTypeScAddressTypeAccount,
		AccountId: &toAddr,
	})
	if err != nil {
		t.Fatalf("NewScVal address: %v", err)
	}
	amountVal, err := xdr.NewScVal(xdr.ScValTypeScvI128, xdr.Int128Parts{Hi: 1, Lo: 0})
	if err != nil {
		t.Fatalf("NewScVal i128: %v", err)
	}
	sym := xdr.ScSymbol("USDC")
	symVal, err := xdr.NewScVal(xdr.ScValTypeScvSymbol, sym)
	if err != nil {
		t.Fatalf("NewScVal symbol: %v", err)
	}
	vec := xdr.ScVec{symVal}
	vecVal, err := xdr.NewScVal(xdr.ScValTypeScvVec, &vec)
	if err != nil {
		t.Fatalf("NewScVal vec: %v", err)
	}

	hf, err := xdr.NewHostFunction(
		xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
		xdr.InvokeContractArgs{
			ContractAddress: xdr.ScAddress{
				Type:       xdr.ScAddressTypeScAddressTypeContract,
				ContractId: &contractID,
			},
			FunctionName: "swap",
			Args:         []xdr.ScVal{addrVal, amountVal, vecVal},
		},
	)
	if err != nil {
		t.Fatalf("NewHostFunction: %v", err)
	}
	b64 := mustBody(t, xdr.OperationTypeInvokeHostFunction, xdr.InvokeHostFunctionOp{
		HostFunction: hf,
	})

	d, err := xdrjson.DecodeOperationBody(b64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d.Type != "invoke_host_function" {
		t.Errorf("type = %q", d.Type)
	}
	if d.Fields["function"] != "invoke_contract" || d.Fields["function_name"] != "swap" {
		t.Errorf("fields = %+v", d.Fields)
	}
	if d.Fields["arg_count"] != 3 {
		t.Errorf("arg_count = %v, want 3", d.Fields["arg_count"])
	}
	args, ok := d.Fields["args"].([]string)
	if !ok || len(args) != 3 {
		t.Fatalf("args = %#v, want 3 display strings", d.Fields["args"])
	}
	if args[0] != gAddr {
		t.Errorf("args[0] = %q, want the account strkey", args[0])
	}
	// ADR-0003: 2^64 must render as the full decimal string, not a
	// truncated int64.
	if args[1] != "18446744073709551616" {
		t.Errorf("args[1] = %q, want 18446744073709551616", args[1])
	}
	if args[2] != "[USDC]" {
		t.Errorf("args[2] = %q, want [USDC]", args[2])
	}
}

// authNode builds a ContractFn SorobanAuthorizedInvocation with optional
// sub-invocations — mirrors the dispatcher's auth-tree test helper.
func authNode(seed byte, fn string, subs ...xdr.SorobanAuthorizedInvocation) xdr.SorobanAuthorizedInvocation {
	var cid xdr.ContractId
	for i := range cid {
		cid[i] = seed
	}
	return xdr.SorobanAuthorizedInvocation{
		Function: xdr.SorobanAuthorizedFunction{
			Type: xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn,
			ContractFn: &xdr.InvokeContractArgs{
				ContractAddress: xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid},
				FunctionName:    xdr.ScSymbol(fn),
			},
		},
		SubInvocations: subs,
	}
}

func invokeSwapHF(t *testing.T) xdr.HostFunction {
	t.Helper()
	var topID xdr.ContractId
	for i := range topID {
		topID[i] = 0xA0
	}
	hf, err := xdr.NewHostFunction(
		xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
		xdr.InvokeContractArgs{
			ContractAddress: xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &topID},
			FunctionName:    "swap",
		},
	)
	if err != nil {
		t.Fatalf("NewHostFunction: %v", err)
	}
	return hf
}

// TestDecodeOperationBody_InvokeHostFunction_AuthTree pins the 5.1 enhancement:
// the nested AUTHORIZATION tree (op.Auth → RootInvocation → SubInvocations) is
// surfaced under Fields["authorizations"] so the /tx view can render the
// contract-call structure it previously omitted.
func TestDecodeOperationBody_InvokeHostFunction_AuthTree(t *testing.T) {
	root := authNode(0xB0, "transfer", authNode(0xC0, "approve"))
	b64 := mustBody(t, xdr.OperationTypeInvokeHostFunction, xdr.InvokeHostFunctionOp{
		HostFunction: invokeSwapHF(t),
		Auth:         []xdr.SorobanAuthorizationEntry{{RootInvocation: root}},
	})

	d, err := xdrjson.DecodeOperationBody(b64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	tree, ok := d.Fields["authorizations"].([]xdrjson.AuthInvocation)
	if !ok || len(tree) != 1 {
		t.Fatalf("authorizations = %#v, want 1 root", d.Fields["authorizations"])
	}
	if tree[0].Kind != "invoke_contract" || tree[0].FunctionName != "transfer" {
		t.Fatalf("root = %+v, want transfer/invoke_contract", tree[0])
	}
	if tree[0].ContractID == "" {
		t.Fatalf("root ContractID should be a strkey, got empty: %+v", tree[0])
	}
	if len(tree[0].SubInvocations) != 1 || tree[0].SubInvocations[0].FunctionName != "approve" {
		t.Fatalf("sub = %+v, want one approve sub-invocation", tree[0].SubInvocations)
	}
}

// TestDecodeOperationBody_InvokeHostFunction_NoAuthNoTree guards against noise:
// a simple invoke with no auth entries must NOT emit an authorizations field.
func TestDecodeOperationBody_InvokeHostFunction_NoAuthNoTree(t *testing.T) {
	b64 := mustBody(t, xdr.OperationTypeInvokeHostFunction, xdr.InvokeHostFunctionOp{
		HostFunction: invokeSwapHF(t),
	})
	d, err := xdrjson.DecodeOperationBody(b64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v, present := d.Fields["authorizations"]; present {
		t.Fatalf("no-auth invoke must not emit authorizations, got %#v", v)
	}
}

// TestDecodeOperationBody_InvokeHostFunction_AuthCredentials pins GH-1138:
// the SorobanCredentials that authorized an auth entry's root — the address
// that actually signed a relayed invoke — must be surfaced, since the op's
// own source_account (the relayer) is not it.
func TestDecodeOperationBody_InvokeHostFunction_AuthCredentials(t *testing.T) {
	root := authNode(0xB0, "transfer")
	authAddr := xdr.MustAddress(gAddr2)
	entry := xdr.SorobanAuthorizationEntry{
		Credentials: xdr.SorobanCredentials{
			Type: xdr.SorobanCredentialsTypeSorobanCredentialsAddress,
			Address: &xdr.SorobanAddressCredentials{
				Address:                   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &authAddr},
				Nonce:                     42,
				SignatureExpirationLedger: 999,
				Signature:                 xdr.ScVal{Type: xdr.ScValTypeScvVoid},
			},
		},
		RootInvocation: root,
	}
	b64 := mustBody(t, xdr.OperationTypeInvokeHostFunction, xdr.InvokeHostFunctionOp{
		HostFunction: invokeSwapHF(t),
		Auth:         []xdr.SorobanAuthorizationEntry{entry},
	})

	d, err := xdrjson.DecodeOperationBody(b64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	tree, ok := d.Fields["authorizations"].([]xdrjson.AuthInvocation)
	if !ok || len(tree) != 1 {
		t.Fatalf("authorizations = %#v, want 1 root", d.Fields["authorizations"])
	}
	cred := tree[0].Credentials
	if cred == nil {
		t.Fatalf("root.Credentials is nil, want the address that authorized it")
	}
	if cred.Kind != "address" || cred.Address != gAddr2 {
		t.Fatalf("credentials = %+v, want kind=address address=%s", cred, gAddr2)
	}
	if cred.Nonce != "42" {
		t.Fatalf("credentials.Nonce = %q, want \"42\"", cred.Nonce)
	}
	if cred.SignatureExpirationLedger != 999 {
		t.Fatalf("credentials.SignatureExpirationLedger = %d, want 999", cred.SignatureExpirationLedger)
	}
}

// TestDecodeOperationBody_InvokeHostFunction_SourceAccountCredentials proves
// the source_account variant (the common case — the op's own source signed
// it) renders with no address/nonce, not a zero-valued address entry.
func TestDecodeOperationBody_InvokeHostFunction_SourceAccountCredentials(t *testing.T) {
	entry := xdr.SorobanAuthorizationEntry{
		Credentials:    xdr.SorobanCredentials{Type: xdr.SorobanCredentialsTypeSorobanCredentialsSourceAccount},
		RootInvocation: authNode(0xB0, "transfer"),
	}
	b64 := mustBody(t, xdr.OperationTypeInvokeHostFunction, xdr.InvokeHostFunctionOp{
		HostFunction: invokeSwapHF(t),
		Auth:         []xdr.SorobanAuthorizationEntry{entry},
	})
	d, err := xdrjson.DecodeOperationBody(b64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	tree := d.Fields["authorizations"].([]xdrjson.AuthInvocation)
	cred := tree[0].Credentials
	if cred == nil || cred.Kind != "source_account" || cred.Address != "" {
		t.Fatalf("credentials = %+v, want kind=source_account, no address", cred)
	}
}

// TestDecodeOperationBody_ManageData_NonUTF8Name pins GH-1140: a manage_data
// name is opaque XDR bytes (String64), not guaranteed UTF-8. name_base64 must
// carry the exact original bytes even when name itself, once JSON-marshaled,
// would show U+FFFD replacement characters.
func TestDecodeOperationBody_ManageData_NonUTF8Name(t *testing.T) {
	raw := []byte{'k', 0x01, 0xff, 0xfe}
	b64 := mustBody(t, xdr.OperationTypeManageData, xdr.ManageDataOp{
		DataName: xdr.String64(raw),
	})
	d, err := xdrjson.DecodeOperationBody(b64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, ok := d.Fields["name_base64"].(string)
	if !ok {
		t.Fatalf("name_base64 missing or wrong type: %#v", d.Fields["name_base64"])
	}
	decoded, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("decode name_base64: %v", err)
	}
	if string(decoded) != string(raw) {
		t.Fatalf("name_base64 round-trip = %q, want %q", decoded, raw)
	}
}

// u32p returns a pointer to an xdr.Uint32 — SetOptionsOp's fields are all
// optional pointers.
func u32p(v uint32) *xdr.Uint32 {
	x := xdr.Uint32(v)
	return &x
}

// TestDecodeOperationBody_SetOptions pins the set_options arm of GH-1134:
// every field (inflation dest, thresholds, home domain, signer) decodes.
func TestDecodeOperationBody_SetOptions(t *testing.T) {
	home := xdr.String32("example.com")
	b64 := mustBody(t, xdr.OperationTypeSetOptions, xdr.SetOptionsOp{
		MasterWeight: u32p(5),
		HomeDomain:   &home,
		Signer:       &xdr.Signer{Key: xdr.MustSigner(gAddr2), Weight: 3},
	})
	d, err := xdrjson.DecodeOperationBody(b64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d.Fields["master_weight"] != uint32(5) {
		t.Errorf("master_weight = %v", d.Fields["master_weight"])
	}
	if d.Fields["home_domain"] != "example.com" {
		t.Errorf("home_domain = %v", d.Fields["home_domain"])
	}
	signer, ok := d.Fields["signer"].(map[string]any)
	if !ok || signer["key"] != gAddr2 || signer["weight"] != uint32(3) {
		t.Errorf("signer = %+v", d.Fields["signer"])
	}
}

// TestDecodeOperationBody_CreateClaimableBalance pins GH-1134's
// create_claimable_balance arm: asset, amount, and the claimant list with
// its predicate tree.
func TestDecodeOperationBody_CreateClaimableBalance(t *testing.T) {
	b64 := mustBody(t, xdr.OperationTypeCreateClaimableBalance, xdr.CreateClaimableBalanceOp{
		Asset:  xdr.MustNewNativeAsset(),
		Amount: 25000,
		Claimants: []xdr.Claimant{{
			Type: xdr.ClaimantTypeClaimantTypeV0,
			V0: &xdr.ClaimantV0{
				Destination: xdr.MustAddress(gAddr2),
				Predicate:   xdr.ClaimPredicate{Type: xdr.ClaimPredicateTypeClaimPredicateUnconditional},
			},
		}},
	})
	d, err := xdrjson.DecodeOperationBody(b64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d.Fields["amount"] != "25000" || d.Fields["asset"] != "native" {
		t.Errorf("fields = %+v", d.Fields)
	}
	claimants, ok := d.Fields["claimants"].([]map[string]any)
	if !ok || len(claimants) != 1 {
		t.Fatalf("claimants = %#v", d.Fields["claimants"])
	}
	if claimants[0]["destination"] != gAddr2 {
		t.Errorf("claimant destination = %v", claimants[0]["destination"])
	}
	pred, ok := claimants[0]["predicate"].(map[string]any)
	if !ok || pred["type"] != "unconditional" {
		t.Errorf("predicate = %+v", claimants[0]["predicate"])
	}
}

// TestDecodeOperationBody_ClaimAndClawbackClaimableBalance pins the
// balance_id decode (hex, matching claimable_balance_seed.go's convention)
// for both ops that reference an existing claimable balance by id.
func TestDecodeOperationBody_ClaimAndClawbackClaimableBalance(t *testing.T) {
	var raw [32]byte
	for i := range raw {
		raw[i] = byte(i)
	}
	id := xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: (*xdr.Hash)(&raw)}
	wantHex := hex.EncodeToString(raw[:])

	claimB64 := mustBody(t, xdr.OperationTypeClaimClaimableBalance, xdr.ClaimClaimableBalanceOp{BalanceId: id})
	d, err := xdrjson.DecodeOperationBody(claimB64)
	if err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	if d.Fields["balance_id"] != wantHex {
		t.Errorf("claim balance_id = %v, want %s", d.Fields["balance_id"], wantHex)
	}

	clawbackB64 := mustBody(t, xdr.OperationTypeClawbackClaimableBalance, xdr.ClawbackClaimableBalanceOp{BalanceId: id})
	d, err = xdrjson.DecodeOperationBody(clawbackB64)
	if err != nil {
		t.Fatalf("decode clawback: %v", err)
	}
	if d.Fields["balance_id"] != wantHex {
		t.Errorf("clawback balance_id = %v, want %s", d.Fields["balance_id"], wantHex)
	}
}

// TestDecodeOperationBody_RevokeSponsorship pins GH-1134's revoke_sponsorship
// arm for both union cases: a sponsored ledger entry (here: a trustline) and
// a sponsored signer.
func TestDecodeOperationBody_RevokeSponsorship(t *testing.T) {
	ledgerKeyB64 := mustBody(t, xdr.OperationTypeRevokeSponsorship, xdr.RevokeSponsorshipOp{
		Type: xdr.RevokeSponsorshipTypeRevokeSponsorshipLedgerEntry,
		LedgerKey: &xdr.LedgerKey{
			Type: xdr.LedgerEntryTypeTrustline,
			TrustLine: &xdr.LedgerKeyTrustLine{
				AccountId: xdr.MustAddress(gAddr),
				Asset:     xdr.MustNewCreditAsset("USDC", gAddr2).ToTrustLineAsset(),
			},
		},
	})
	d, err := xdrjson.DecodeOperationBody(ledgerKeyB64)
	if err != nil {
		t.Fatalf("decode ledger_entry: %v", err)
	}
	if d.Fields["sponsorship_type"] != "ledger_entry" || d.Fields["account_id"] != gAddr {
		t.Errorf("fields = %+v", d.Fields)
	}
	if d.Fields["asset"] != "USDC-"+gAddr2 {
		t.Errorf("asset = %v", d.Fields["asset"])
	}

	signerB64 := mustBody(t, xdr.OperationTypeRevokeSponsorship, xdr.RevokeSponsorshipOp{
		Type: xdr.RevokeSponsorshipTypeRevokeSponsorshipSigner,
		Signer: &xdr.RevokeSponsorshipOpSigner{
			AccountId: xdr.MustAddress(gAddr),
			SignerKey: xdr.MustSigner(gAddr2),
		},
	})
	d, err = xdrjson.DecodeOperationBody(signerB64)
	if err != nil {
		t.Fatalf("decode signer: %v", err)
	}
	if d.Fields["sponsorship_type"] != "signer" || d.Fields["account_id"] != gAddr || d.Fields["signer_key"] != gAddr2 {
		t.Errorf("fields = %+v", d.Fields)
	}
}

// TestDecodeOperationBody_LiquidityPoolDepositWithdraw pins GH-1134's LP arms.
func TestDecodeOperationBody_LiquidityPoolDepositWithdraw(t *testing.T) {
	var poolID xdr.PoolId
	for i := range poolID {
		poolID[i] = byte(i + 1)
	}
	wantHex := hex.EncodeToString(poolID[:])

	depositB64 := mustBody(t, xdr.OperationTypeLiquidityPoolDeposit, xdr.LiquidityPoolDepositOp{
		LiquidityPoolId: poolID,
		MaxAmountA:      100,
		MaxAmountB:      200,
		MinPrice:        xdr.Price{N: 1, D: 2},
		MaxPrice:        xdr.Price{N: 3, D: 4},
	})
	d, err := xdrjson.DecodeOperationBody(depositB64)
	if err != nil {
		t.Fatalf("decode deposit: %v", err)
	}
	if d.Fields["liquidity_pool_id"] != wantHex || d.Fields["max_amount_a"] != "100" || d.Fields["max_amount_b"] != "200" {
		t.Errorf("fields = %+v", d.Fields)
	}

	withdrawB64 := mustBody(t, xdr.OperationTypeLiquidityPoolWithdraw, xdr.LiquidityPoolWithdrawOp{
		LiquidityPoolId: poolID,
		Amount:          50,
		MinAmountA:      10,
		MinAmountB:      20,
	})
	d, err = xdrjson.DecodeOperationBody(withdrawB64)
	if err != nil {
		t.Fatalf("decode withdraw: %v", err)
	}
	if d.Fields["liquidity_pool_id"] != wantHex || d.Fields["amount"] != "50" {
		t.Errorf("fields = %+v", d.Fields)
	}
}

// operationBodyFixture returns a representative base64 body for every
// xdr.OperationType this SDK knows, so TestDecodeOperationBody_FieldsExhaustive
// can drive DecodeOperationBody across all of them. ok=false means this test
// has no fixture for typ yet (kept separate from a decode failure).
func operationBodyFixture(t *testing.T, typ xdr.OperationType) (string, bool) {
	t.Helper()
	credit := xdr.MustNewCreditAsset("USDC", gAddr2)
	switch typ {
	case xdr.OperationTypeCreateAccount:
		return mustBody(t, typ, xdr.CreateAccountOp{Destination: xdr.MustAddress(gAddr), StartingBalance: 100}), true
	case xdr.OperationTypePayment:
		return mustBody(t, typ, xdr.PaymentOp{Destination: xdr.MustMuxedAddress(gAddr), Asset: xdr.MustNewNativeAsset(), Amount: 1}), true
	case xdr.OperationTypePathPaymentStrictReceive:
		return mustBody(t, typ, xdr.PathPaymentStrictReceiveOp{
			SendAsset: xdr.MustNewNativeAsset(), SendMax: 1,
			Destination: xdr.MustMuxedAddress(gAddr), DestAsset: credit, DestAmount: 1,
		}), true
	case xdr.OperationTypeManageSellOffer:
		return mustBody(t, typ, xdr.ManageSellOfferOp{Selling: xdr.MustNewNativeAsset(), Buying: credit, Amount: 1, Price: xdr.Price{N: 1, D: 1}, OfferId: 1}), true
	case xdr.OperationTypeCreatePassiveSellOffer:
		return mustBody(t, typ, xdr.CreatePassiveSellOfferOp{Selling: xdr.MustNewNativeAsset(), Buying: credit, Amount: 1, Price: xdr.Price{N: 1, D: 1}}), true
	case xdr.OperationTypeSetOptions:
		return mustBody(t, typ, xdr.SetOptionsOp{MasterWeight: u32p(1)}), true
	case xdr.OperationTypeChangeTrust:
		return mustBody(t, typ, xdr.ChangeTrustOp{Line: credit.ToChangeTrustAsset(), Limit: 1000}), true
	case xdr.OperationTypeAllowTrust:
		code4 := xdr.AssetCode4{'U', 'S', 'D', 'C'}
		return mustBody(t, typ, xdr.AllowTrustOp{
			Trustor: xdr.MustAddress(gAddr), Asset: xdr.AssetCode{Type: xdr.AssetTypeAssetTypeCreditAlphanum4, AssetCode4: &code4}, Authorize: 1,
		}), true
	case xdr.OperationTypeAccountMerge:
		return mustBody(t, typ, xdr.MustMuxedAddress(gAddr)), true
	case xdr.OperationTypeInflation:
		return mustBody(t, typ, nil), true
	case xdr.OperationTypeManageData:
		val := xdr.DataValue("v")
		return mustBody(t, typ, xdr.ManageDataOp{DataName: "name", DataValue: &val}), true
	case xdr.OperationTypeBumpSequence:
		return mustBody(t, typ, xdr.BumpSequenceOp{BumpTo: 5}), true
	case xdr.OperationTypeManageBuyOffer:
		return mustBody(t, typ, xdr.ManageBuyOfferOp{Selling: xdr.MustNewNativeAsset(), Buying: credit, BuyAmount: 1, Price: xdr.Price{N: 1, D: 1}, OfferId: 1}), true
	case xdr.OperationTypePathPaymentStrictSend:
		return mustBody(t, typ, xdr.PathPaymentStrictSendOp{
			SendAsset: xdr.MustNewNativeAsset(), SendAmount: 1,
			Destination: xdr.MustMuxedAddress(gAddr), DestAsset: credit, DestMin: 1,
		}), true
	case xdr.OperationTypeCreateClaimableBalance:
		return mustBody(t, typ, xdr.CreateClaimableBalanceOp{
			Asset: xdr.MustNewNativeAsset(), Amount: 1,
			Claimants: []xdr.Claimant{{Type: xdr.ClaimantTypeClaimantTypeV0, V0: &xdr.ClaimantV0{
				Destination: xdr.MustAddress(gAddr2), Predicate: xdr.ClaimPredicate{Type: xdr.ClaimPredicateTypeClaimPredicateUnconditional},
			}}},
		}), true
	case xdr.OperationTypeClaimClaimableBalance:
		var raw xdr.Hash
		return mustBody(t, typ, xdr.ClaimClaimableBalanceOp{
			BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &raw},
		}), true
	case xdr.OperationTypeBeginSponsoringFutureReserves:
		return mustBody(t, typ, xdr.BeginSponsoringFutureReservesOp{SponsoredId: xdr.MustAddress(gAddr)}), true
	case xdr.OperationTypeEndSponsoringFutureReserves:
		return mustBody(t, typ, nil), true
	case xdr.OperationTypeRevokeSponsorship:
		return mustBody(t, typ, xdr.RevokeSponsorshipOp{
			Type: xdr.RevokeSponsorshipTypeRevokeSponsorshipSigner,
			Signer: &xdr.RevokeSponsorshipOpSigner{
				AccountId: xdr.MustAddress(gAddr), SignerKey: xdr.MustSigner(gAddr2),
			},
		}), true
	case xdr.OperationTypeClawback:
		return mustBody(t, typ, xdr.ClawbackOp{Asset: credit, From: xdr.MustMuxedAddress(gAddr2), Amount: 1}), true
	case xdr.OperationTypeClawbackClaimableBalance:
		var raw xdr.Hash
		return mustBody(t, typ, xdr.ClawbackClaimableBalanceOp{
			BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &raw},
		}), true
	case xdr.OperationTypeSetTrustLineFlags:
		return mustBody(t, typ, xdr.SetTrustLineFlagsOp{Trustor: xdr.MustAddress(gAddr2), Asset: credit, SetFlags: 1}), true
	case xdr.OperationTypeLiquidityPoolDeposit:
		var poolID xdr.PoolId
		return mustBody(t, typ, xdr.LiquidityPoolDepositOp{LiquidityPoolId: poolID, MaxAmountA: 1, MaxAmountB: 1, MinPrice: xdr.Price{N: 1, D: 1}, MaxPrice: xdr.Price{N: 1, D: 1}}), true
	case xdr.OperationTypeLiquidityPoolWithdraw:
		var poolID xdr.PoolId
		return mustBody(t, typ, xdr.LiquidityPoolWithdrawOp{LiquidityPoolId: poolID, Amount: 1, MinAmountA: 1, MinAmountB: 1}), true
	case xdr.OperationTypeInvokeHostFunction:
		return mustBody(t, typ, xdr.InvokeHostFunctionOp{HostFunction: invokeSwapHF(t)}), true
	case xdr.OperationTypeExtendFootprintTtl:
		return mustBody(t, typ, xdr.ExtendFootprintTtlOp{ExtendTo: 100}), true
	case xdr.OperationTypeRestoreFootprint:
		return mustBody(t, typ, xdr.RestoreFootprintOp{}), true
	}
	return "", false
}

// TestDecodeOperationBody_FieldsExhaustive pins GH-1134: every operation type
// the SDK knows must produce either decoded Fields or be on the explicit
// genuinely-empty allowlist (inflation, end_sponsoring_future_reserves — the
// op body carries nothing else) — never silently fall back to raw_xdr because
// fillOpFields has no case for it. Mirrors result_codes_test.go's
// TestTxResultNamesExhaustive: it ranges the SDK's OWN valid enum values, not
// a fixed count, so a future protocol op type goes RED here until
// fillOpFields (and operationBodyFixture, for this test itself) gets it.
func TestDecodeOperationBody_FieldsExhaustive(t *testing.T) {
	genuinelyEmpty := map[xdr.OperationType]bool{
		xdr.OperationTypeInflation:                   true,
		xdr.OperationTypeEndSponsoringFutureReserves: true,
	}
	var probe xdr.OperationType
	seen := 0
	for c := int32(0); c <= 64; c++ {
		if !probe.ValidEnum(c) {
			continue
		}
		typ := xdr.OperationType(c)
		seen++
		b64, ok := operationBodyFixture(t, typ)
		if !ok {
			t.Errorf("op type %s (%d) has no fixture in operationBodyFixture — add one", typ.String(), c)
			continue
		}
		d, err := xdrjson.DecodeOperationBody(b64)
		if err != nil {
			t.Fatalf("decode %s: %v", typ.String(), err)
		}
		if genuinelyEmpty[typ] {
			continue
		}
		if len(d.Fields) == 0 {
			t.Errorf("op type %q has no decoded fields — falls back to raw_xdr (missing fillOpFields arm)", d.Type)
		}
	}
	if seen < 27 {
		t.Fatalf("only %d valid op types discovered; probe window too narrow?", seen)
	}
}
