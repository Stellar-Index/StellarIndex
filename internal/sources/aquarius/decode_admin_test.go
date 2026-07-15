package aquarius

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Golden decode tests for the eight governance/upgrade admin event
// kinds (migration 0100, ROADMAP #89). Every topic/body blob below is
// an UNTOUCHED base64 SCVal captured from the r1 ClickHouse lake
// (stellar.contract_events) on 2026-07-10. Provenance note: every
// sample here comes from the FLAGGED parallel router deployment
// (CA7RQDMM...) or an as-yet-unidentified sibling system contract —
// see decode_admin.go's package doc for why (these kinds are rare
// enough on the canonical router that a full-history scoped query,
// not a LIMIT-3 unscoped one, was needed to find canonical-router
// occurrences at all: apply_upgrade x7, commit_upgrade x6,
// set_privileged_addrs x2, apply_transfer_ownership x1,
// commit_transfer_ownership x1 out of the current lake). Decode
// correctness does not depend on which contract emitted the bytes —
// gate-membership is tested separately in adapter_test.go using the
// standard synthetic-seed pattern.

func TestDecodeApplyUpgrade_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CA7RQDMMV6E53P5EDZA5GPWBZ33AMW2ZNO42XLI2RGRIAP4QXIARUOJQ",
		Ledger:     56144785,
		TxHash:     "4434edd45dff7cf409674b3b680ca4c8f68a3b0fd771edd85c8f63dfb9136d85",
		EventIndex: 1,
		Topic: []string{
			"AAAADwAAAA1hcHBseV91cGdyYWRlAAAA",
		},
		Value: "AAAAEAAAAAEAAAABAAAADQAAACCM8Q0UOantH40nYGytza51VYUcQO8Obkux/+L46812WA==",
	}
	if got := classify(e); got != EventApplyUpgrade {
		t.Fatalf("classify = %q, want %q", got, EventApplyUpgrade)
	}
	av, err := decodeAdminEvent(e, EventApplyUpgrade, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeAdminEvent: %v", err)
	}
	if av.Kind != AdminApplyUpgrade {
		t.Errorf("Kind = %q, want %q", av.Kind, AdminApplyUpgrade)
	}
	if av.Target != "8cf10d1439a9ed1f8d27606cadcdae7555851c40ef0e6e4bb1ffe2f8ebcd7658" {
		t.Errorf("Target = %q, want the hex-encoded 32-byte wasm hash", av.Target)
	}
}

func TestDecodeCommitUpgrade_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CA7RQDMMV6E53P5EDZA5GPWBZ33AMW2ZNO42XLI2RGRIAP4QXIARUOJQ",
		Ledger:     56144770,
		TxHash:     "14e43c313c84c3588c6ae347258343e73a83cb1a9b74d0c582f39012b2f25585",
		EventIndex: 0,
		Topic: []string{
			"AAAADwAAAA5jb21taXRfdXBncmFkZQAA",
		},
		Value: "AAAAEAAAAAEAAAABAAAADQAAACCM8Q0UOantH40nYGytza51VYUcQO8Obkux/+L46812WA==",
	}
	if got := classify(e); got != EventCommitUpgrade {
		t.Fatalf("classify = %q, want %q", got, EventCommitUpgrade)
	}
	av, err := decodeAdminEvent(e, EventCommitUpgrade, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeAdminEvent: %v", err)
	}
	// Same wasm hash as TestDecodeApplyUpgrade_realFixture's fixture —
	// commit_upgrade stages the SAME upgrade apply_upgrade later
	// applies (two-phase upgrade, real bytes confirm the pairing).
	if av.Target != "8cf10d1439a9ed1f8d27606cadcdae7555851c40ef0e6e4bb1ffe2f8ebcd7658" {
		t.Errorf("Target = %q, want the hex-encoded 32-byte wasm hash", av.Target)
	}
}

func TestDecodeSetPrivilegedAddrs_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CA7RQDMMV6E53P5EDZA5GPWBZ33AMW2ZNO42XLI2RGRIAP4QXIARUOJQ",
		Ledger:     54150744,
		TxHash:     "123e9b76b6c9288eeeeb8eee3e8890b606df4bac34edca86fea07f2af3fee35d",
		EventIndex: 0,
		Topic: []string{
			"AAAADwAAABRzZXRfcHJpdmlsZWdlZF9hZGRycw==",
		},
		Value: "AAAAEAAAAAEAAAAEAAAAEgAAAAAAAAAAdcoKqsEbsyr0vqtizcS1v/F9m86ZJaBsJIfsPrVq9c4AAAASAAAAAAAAAAA9wKBfw2ZvhidSVPc8cyXssqJE8elIez+Oy0nXRkV6CAAAABIAAAAAAAAAABDqOTDnu1sCH7swm1DeeaF2JZ2VREpGXUW2rPs9Xi5jAAAAEAAAAAEAAAABAAAAEgAAAAAAAAAAEOo5MOe7WwIfuzCbUN55oXYlnZVESkZdRbas+z1eLmM=",
	}
	if got := classify(e); got != EventSetPrivilegedAddrs {
		t.Fatalf("classify = %q, want %q", got, EventSetPrivilegedAddrs)
	}
	av, err := decodeAdminEvent(e, EventSetPrivilegedAddrs, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeAdminEvent: %v", err)
	}
	if got := av.Attributes["addr_0"]; got != "GB24UCVKYEN3GKXUX2VWFTOEWW77C7M3Z2MSLIDMESD6YPVVNL245CPZ" {
		t.Errorf("addr_0 = %v", got)
	}
	if got := av.Attributes["addr_1"]; got != "GA64BIC7YNTG7BRHKJKPOPDTEXWLFISE6HUUQ6Z7R3FUTV2GIV5ARDK3" {
		t.Errorf("addr_1 = %v", got)
	}
	if got := av.Attributes["addr_2"]; got != "GAIOUOJQ465VWAQ7XMYJWUG6PGQXMJM5SVCEURS5IW3KZ6Z5LYXGG5AQ" {
		t.Errorf("addr_2 = %v", got)
	}
	list, ok := av.Attributes["addr_list"].([]string)
	if !ok || len(list) != 1 || list[0] != "GAIOUOJQ465VWAQ7XMYJWUG6PGQXMJM5SVCEURS5IW3KZ6Z5LYXGG5AQ" {
		t.Errorf("addr_list = %v, want [GAIOUOJQ465VWAQ7XMYJWUG6PGQXMJM5SVCEURS5IW3KZ6Z5LYXGG5AQ]", av.Attributes["addr_list"])
	}
}

func TestDecodeApplyTransferOwnership_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CA7RQDMMV6E53P5EDZA5GPWBZ33AMW2ZNO42XLI2RGRIAP4QXIARUOJQ",
		Ledger:     54294236,
		TxHash:     "ecfd42c073d2186417c0fbedd1695b0877df84a3c7550c05c87f2eb6dbaa6c71",
		EventIndex: 0,
		Topic: []string{
			"AAAADwAAABhhcHBseV90cmFuc2Zlcl9vd25lcnNoaXA=",
			"AAAADwAAAA5FbWVyZ2VuY3lBZG1pbgAA",
		},
		Value: "AAAAEAAAAAEAAAABAAAAEgAAAAAAAAAAjkvXmN6qivkBp0vhCkvm3JCPt77we4oWmY4nHrLiqCc=",
	}
	if got := classify(e); got != EventApplyTransferOwnership {
		t.Fatalf("classify = %q, want %q", got, EventApplyTransferOwnership)
	}
	av, err := decodeAdminEvent(e, EventApplyTransferOwnership, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeAdminEvent: %v", err)
	}
	if got := av.Attributes["role"]; got != "EmergencyAdmin" {
		t.Errorf("role = %v, want EmergencyAdmin", got)
	}
	if av.Target != "GCHEXV4Y32VIV6IBU5F6CCSL43OJBD5XX3YHXCQWTGHCOHVS4KUCOFP5" {
		t.Errorf("Target = %q", av.Target)
	}
}

func TestDecodeCommitTransferOwnership_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CA7RQDMMV6E53P5EDZA5GPWBZ33AMW2ZNO42XLI2RGRIAP4QXIARUOJQ",
		Ledger:     54294234,
		TxHash:     "5f03701053fecd63e55c0f17e65b769f314a1ae533ea6984173723b5b8902066",
		EventIndex: 0,
		Topic: []string{
			"AAAADwAAABljb21taXRfdHJhbnNmZXJfb3duZXJzaGlwAAAA",
			"AAAADwAAAA5FbWVyZ2VuY3lBZG1pbgAA",
		},
		Value: "AAAAEAAAAAEAAAABAAAAEgAAAAAAAAAAjkvXmN6qivkBp0vhCkvm3JCPt77we4oWmY4nHrLiqCc=",
	}
	if got := classify(e); got != EventCommitTransferOwnership {
		t.Fatalf("classify = %q, want %q", got, EventCommitTransferOwnership)
	}
	av, err := decodeAdminEvent(e, EventCommitTransferOwnership, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeAdminEvent: %v", err)
	}
	if got := av.Attributes["role"]; got != "EmergencyAdmin" {
		t.Errorf("role = %v, want EmergencyAdmin", got)
	}
	if av.Target != "GCHEXV4Y32VIV6IBU5F6CCSL43OJBD5XX3YHXCQWTGHCOHVS4KUCOFP5" {
		t.Errorf("Target = %q", av.Target)
	}
}

func TestDecodeEnableEmergencyMode_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CAEYKKJ5LTBLVQ5EM6H433YFHKOUJRDWOW3NF355ZS3FHQZKHXLQIHKA",
		Ledger:     56144775,
		TxHash:     "419b4230b319d6535443acc50f1e922a9776be676e165f47c31338f449f3342c",
		EventIndex: 0,
		Topic: []string{
			"AAAADwAAABVlbmFibGVfZW1lcmdlbmN5X21vZGUAAAA=",
		},
		Value: "AAAAAQ==",
	}
	if got := classify(e); got != EventEnableEmergencyMode {
		t.Fatalf("classify = %q, want %q", got, EventEnableEmergencyMode)
	}
	av, err := decodeAdminEvent(e, EventEnableEmergencyMode, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeAdminEvent: %v", err)
	}
	if av.Admin != "" || av.Target != "" || len(av.Attributes) != 0 {
		t.Errorf("expected an empty-payload marker event, got admin=%q target=%q attrs=%v", av.Admin, av.Target, av.Attributes)
	}
}

func TestDecodeEnableEmergencyMode_rejectsNonVoidBody(t *testing.T) {
	e := &events.Event{
		Topic: []string{"AAAADwAAABVlbmFibGVfZW1lcmdlbmN5X21vZGUAAAA="},
		Value: "AAAACgAAAAAAAAAAAAAAAAAAAAA=", // I128, not void
	}
	if _, err := decodeAdminEvent(e, EventEnableEmergencyMode, rewardsClosedAtTest); err == nil {
		t.Error("expected error on non-void body")
	}
}

func TestDecodeDisableEmergencyMode_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CA7RQDMMV6E53P5EDZA5GPWBZ33AMW2ZNO42XLI2RGRIAP4QXIARUOJQ",
		Ledger:     54294239,
		TxHash:     "09e8a4af0893851bbd966769f819449a40a9e80d4b609f73c4a5aec950aa1563",
		EventIndex: 0,
		Topic: []string{
			"AAAADwAAABZkaXNhYmxlX2VtZXJnZW5jeV9tb2RlAAA=",
		},
		Value: "AAAAAQ==",
	}
	if got := classify(e); got != EventDisableEmergencyMode {
		t.Fatalf("classify = %q, want %q", got, EventDisableEmergencyMode)
	}
	av, err := decodeAdminEvent(e, EventDisableEmergencyMode, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeAdminEvent: %v", err)
	}
	if av.Admin != "" || av.Target != "" || len(av.Attributes) != 0 {
		t.Errorf("expected an empty-payload marker event, got admin=%q target=%q attrs=%v", av.Admin, av.Target, av.Attributes)
	}
}

func TestDecodePoolGaugeSwitchToken_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CBQDHNBFBZYE4MKPWBSJOPIYLW4SFSXAXUTSXJN76GNKYVYPCKWC6QUK",
		Ledger:     59270084,
		TxHash:     "795ca1edf536361904eaf9e830f80766febb6564e5e8e76c1f81e1138c2db983",
		EventIndex: 0,
		Topic: []string{
			"AAAADwAAABdwb29sX2dhdWdlX3N3aXRjaF90b2tlbgA=",
			"AAAAEgAAAAFQkI25aXl99CnhS5sIYZCU5/Wh49ZuSaRsWLA/BuP6Vg==",
		},
		Value: "AAAAEAAAAAEAAAABAAAAAAAAAAE=",
	}
	if got := classify(e); got != EventPoolGaugeSwitchToken {
		t.Fatalf("classify = %q, want %q", got, EventPoolGaugeSwitchToken)
	}
	av, err := decodeAdminEvent(e, EventPoolGaugeSwitchToken, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeAdminEvent: %v", err)
	}
	if av.Target != "CBIJBDNZNF4X35BJ4FFZWCDBSCKOP5NB4PLG4SNENRMLAPYG4P5FM6VN" {
		t.Errorf("Target = %q", av.Target)
	}
	if got := av.Attributes["switched"]; got != true {
		t.Errorf("switched = %v, want true", got)
	}
}
