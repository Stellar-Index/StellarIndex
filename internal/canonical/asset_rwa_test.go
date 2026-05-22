package canonical

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestNewRWAAsset_accepted(t *testing.T) {
	cases := []string{"BENJI", "iBENJI", "GILTS", "CETES", "KTB", "TESOURO", "USTRY", "SPXU"}
	for _, code := range cases {
		t.Run(code, func(t *testing.T) {
			a, err := NewRWAAsset(code)
			if err != nil {
				t.Fatalf("NewRWAAsset(%q): %v", code, err)
			}
			if a.Type != AssetRWA {
				t.Errorf("Type = %q, want %q", a.Type, AssetRWA)
			}
			if a.Code != code {
				t.Errorf("Code = %q, want %q", a.Code, code)
			}
			if err := a.Validate(); err != nil {
				t.Errorf("Validate: %v", err)
			}
		})
	}
}

func TestNewRWAAsset_rejected(t *testing.T) {
	// Not allow-listed: a random code, empty, and BTC (a crypto code,
	// not RWA) — the variants must not bleed into each other.
	for _, code := range []string{"NOTANRWA", "", "BTC", "benji"} {
		t.Run(code, func(t *testing.T) {
			if _, err := NewRWAAsset(code); !errors.Is(err, ErrInvalidAsset) {
				t.Errorf("expected ErrInvalidAsset for %q, got %v", code, err)
			}
		})
	}
}

func TestRWAAsset_wireForm(t *testing.T) {
	a, err := NewRWAAsset("BENJI")
	if err != nil {
		t.Fatal(err)
	}
	if s := a.String(); s != "rwa:BENJI" {
		t.Errorf("String() = %q, want %q", s, "rwa:BENJI")
	}
	parsed, err := ParseAsset("rwa:BENJI")
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	if !parsed.Equal(a) {
		t.Errorf("round-trip lost info: %+v vs %+v", parsed, a)
	}
}

func TestRWAAsset_json(t *testing.T) {
	a, _ := NewRWAAsset("USTRY")
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"rwa:USTRY"` {
		t.Errorf("MarshalJSON = %q, want %q", b, `"rwa:USTRY"`)
	}
	var a2 Asset
	if err := json.Unmarshal(b, &a2); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if !a.Equal(a2) {
		t.Errorf("JSON round-trip lost info: %+v vs %+v", a, a2)
	}
}

func TestRWAAsset_validateRejectsForbiddenFields(t *testing.T) {
	// RWA carries only Code — an issuer or contract_id is invalid.
	withIssuer := Asset{Type: AssetRWA, Code: "BENJI", Issuer: "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"}
	if err := withIssuer.Validate(); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("expected ErrInvalidAsset for issuer on rwa asset, got %v", err)
	}
	withContract := Asset{Type: AssetRWA, Code: "BENJI", ContractID: "CA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"}
	if err := withContract.Validate(); !errors.Is(err, ErrInvalidAsset) {
		t.Errorf("expected ErrInvalidAsset for contract_id on rwa asset, got %v", err)
	}
}

func TestRWAAsset_distinctFromCrypto(t *testing.T) {
	// SolvBTC is crypto; an RWA code is rwa — the two allow-lists must
	// not overlap, and the variants are never Equal.
	if IsKnownRWA("SolvBTC") {
		t.Error("SolvBTC is a crypto code, must not be in the RWA allow-list")
	}
	if IsKnownCrypto("BENJI") {
		t.Error("BENJI is an RWA code, must not be in the crypto allow-list")
	}
}

func TestIsKnownRWA(t *testing.T) {
	if !IsKnownRWA("GILTS") {
		t.Error("GILTS should be known")
	}
	if IsKnownRWA("NOTANRWA") {
		t.Error("NOTANRWA should not be known")
	}
}
