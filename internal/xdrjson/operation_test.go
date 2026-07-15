package xdrjson_test

import (
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
