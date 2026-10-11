package aquarius

import (
	"strings"
	"testing"
)

// sdkDecodeAssetTopic and sdkDecodeAddressTopic are the topic-slot decoders called once per
// Aquarius trade. Each must reject a base64 input that fails XDR parse, and an SCVal that is not
// an Address (asset topic) or G-or-C strkey (address topic). A partial or zero asset would
// mis-attribute trades to nil-asset rows.

func TestSdkDecodeAssetTopic_invalidBase64(t *testing.T) {
	if _, err := sdkDecodeAssetTopic("!!!not-base64!!!"); err == nil {
		t.Error("expected parse error for invalid base64, got nil")
	}
}

func TestSdkDecodeAssetTopic_notAnAddress(t *testing.T) {
	// Encode a Symbol where an Address is expected.
	bad := encodeSymbol(t, "I-am-not-an-address")
	_, err := sdkDecodeAssetTopic(bad)
	if err == nil {
		t.Error("expected error decoding Symbol as Address, got nil")
	}
}

func TestSdkDecodeAddressTopic_invalidBase64(t *testing.T) {
	if _, err := sdkDecodeAddressTopic("@@@bad@@@"); err == nil {
		t.Error("expected parse error for invalid base64, got nil")
	}
}

func TestSdkDecodeAddressTopic_notAnAddress(t *testing.T) {
	// Encode a Symbol where an Address is expected — same defensive
	// rejection as the asset variant.
	bad := encodeSymbol(t, "I-am-not-an-address")
	_, err := sdkDecodeAddressTopic(bad)
	if err == nil {
		t.Error("expected error decoding Symbol as Address, got nil")
	}
}

func TestSdkDecodeAssetTopic_happyPathContractAddress(t *testing.T) {
	// Verify the success path emits a Soroban-classed canonical.Asset
	// — keeps the test set complete (we have rejects + happy).
	c := makeContractStrkey(t, 0x77)
	topic := encodeContractAddrFromStrkey(t, c)
	asset, err := sdkDecodeAssetTopic(topic)
	if err != nil {
		t.Fatalf("sdkDecodeAssetTopic: %v", err)
	}
	// The canonical.Asset's String form should reference the
	// contract address — every asset in Aquarius is Soroban.
	got := asset.String()
	if !strings.Contains(got, c) {
		t.Errorf("asset.String() = %q, want it to contain %q", got, c)
	}
}
