package xdrjson_test

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/StellarIndex/stellar-index/internal/xdrjson"
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
