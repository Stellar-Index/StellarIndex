package supply_test

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// TestLockedSet_IsEmpty — the IsEmpty discriminator drives a
// per-algorithm short-circuit; both nil-slice and empty-slice
// inputs must report empty.
func TestLockedSet_IsEmpty(t *testing.T) {
	tests := []struct {
		name string
		set  supply.LockedSet
		want bool
	}{
		{"zero value", supply.LockedSet{}, true},
		{"empty slices", supply.LockedSet{Accounts: []string{}, Contracts: []string{}}, true},
		{"only accounts", supply.LockedSet{Accounts: []string{"GA..."}}, false},
		{"only contracts", supply.LockedSet{Contracts: []string{"C..."}}, false},
		{"both populated", supply.LockedSet{Accounts: []string{"GA..."}, Contracts: []string{"C..."}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.set.IsEmpty(); got != tc.want {
				t.Errorf("IsEmpty() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPolicy_MaxSupplyOverride_Present — happy path: configured
// override returns a parsed *big.Int.
func TestPolicy_MaxSupplyOverride_Present(t *testing.T) {
	p := supply.Policy{
		MaxSupplyOverrides: map[string]string{"USDC:GA1": "21000000000000000"},
	}
	got, ok, err := p.MaxSupplyOverride("USDC:GA1")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got.String() != "21000000000000000" {
		t.Errorf("got = %s, want 21000000000000000", got)
	}
}

// TestPolicy_MaxSupplyOverride_AbsentOrEmpty — both "key missing"
// and "key present with empty value" fall through with ok=false; the
// empty-string sentinel is documented as "fall through to next
// source" in the Policy struct doc.
func TestPolicy_MaxSupplyOverride_AbsentOrEmpty(t *testing.T) {
	p := supply.Policy{
		MaxSupplyOverrides: map[string]string{"USDC:GA1": ""},
	}
	for _, key := range []string{"missing-key", "USDC:GA1"} {
		_, ok, err := p.MaxSupplyOverride(key)
		if err != nil {
			t.Errorf("%s: err = %v, want nil", key, err)
		}
		if ok {
			t.Errorf("%s: ok = true, want false", key)
		}
	}
}

// TestPolicy_MaxSupplyOverride_BadValue — non-numeric values must
// error rather than silently returning zero (which would coerce a
// typo'd YAML to a "supply has been entirely burned" reading).
func TestPolicy_MaxSupplyOverride_BadValue(t *testing.T) {
	p := supply.Policy{
		MaxSupplyOverrides: map[string]string{"X": "not-a-number"},
	}
	_, _, err := p.MaxSupplyOverride("X")
	if err == nil {
		t.Fatal("expected error for non-numeric override; got nil")
	}
	if !strings.Contains(err.Error(), "decimal integer") {
		t.Errorf("error message lacks the diagnostic phrase: %v", err)
	}
}

// TestPolicy_Validate_Clean — a policy with no entries is valid;
// the per-algorithm computers are responsible for handling the
// empty-config case (XLM returns total==circulating).
func TestPolicy_Validate_Clean(t *testing.T) {
	p := supply.Policy{}
	if err := p.Validate(); err != nil {
		t.Errorf("zero-value policy should validate; got %v", err)
	}
}

// TestPolicy_Validate_RejectsEmptyEntries — empty strings in the
// account / contract lists are typos in YAML; loud rejection at
// startup is better than producing a "balance for empty account ID"
// downstream.
func TestPolicy_Validate_RejectsEmptyEntries(t *testing.T) {
	p := supply.Policy{
		SDFReserveAccounts: []string{"GA...", ""},
		PerAsset: map[string]supply.LockedSet{
			"USDC:GA1": {Accounts: []string{""}, Contracts: []string{""}},
		},
		MaxSupplyOverrides: map[string]string{"X": "garbage"},
	}
	err := p.Validate()
	if err == nil {
		t.Fatal("expected validation errors; got nil")
	}
	// errors.Join formats multiple errors one per line.
	for _, want := range []string{
		"SDFReserveAccounts[1]",
		`PerAsset["USDC:GA1"].Accounts[0]`,
		`PerAsset["USDC:GA1"].Contracts[0]`,
		"decimal integer",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validate output missing %q: %v", want, err)
		}
	}
}

// Cold audit 2026-08-04: Validate accepted two config shapes that each
// silently corrupt XLM's published circulating supply.
func TestPolicyValidate_rejectsNegativeOverrideAndDuplicates(t *testing.T) {
	t.Run("negative max_supply override", func(t *testing.T) {
		p := supply.Policy{MaxSupplyOverrides: map[string]string{
			"USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN": "-1000",
		}}
		if err := p.Validate(); err == nil {
			t.Fatal("Validate accepted a negative max_supply override — it parses, so the asset then fails the store's CHECK (max_supply >= 0) on every tick forever while config validation reports the config is fine")
		}
	})

	t.Run("duplicate SDF reserve account", func(t *testing.T) {
		const acc = "GDUY7J7A33TQWOSOQGDO776GGLM3UQERL4J3SPT56F6YS4ID7MLDERI4"
		p := supply.Policy{SDFReserveAccounts: []string{acc, acc}}
		if err := p.Validate(); err == nil {
			t.Fatal("Validate accepted a duplicated reserve account — the readers sum per element with no dedup, so its balance is subtracted twice from circulating supply")
		}
	})

	t.Run("duplicate per-asset locked account", func(t *testing.T) {
		const acc = "GDUY7J7A33TQWOSOQGDO776GGLM3UQERL4J3SPT56F6YS4ID7MLDERI4"
		p := supply.Policy{PerAsset: map[string]supply.LockedSet{
			"AQUA:GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA": {
				Accounts: []string{acc, acc},
			},
		}}
		if err := p.Validate(); err == nil {
			t.Fatal("Validate accepted a duplicated per-asset locked account")
		}
	})

	t.Run("distinct accounts still pass", func(t *testing.T) {
		p := supply.Policy{SDFReserveAccounts: []string{
			"GDUY7J7A33TQWOSOQGDO776GGLM3UQERL4J3SPT56F6YS4ID7MLDERI4",
			"GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN7",
		}}
		if err := p.Validate(); err != nil {
			t.Fatalf("Validate rejected two distinct accounts: %v", err)
		}
	})
}
