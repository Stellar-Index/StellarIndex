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
	// Router/flagged-router single-hash body must NOT grow a staged
	// wasm_hash_N attribute — the arity-based decode reduces to
	// Target-only for a 1-element vec (guards the router path).
	if len(av.Attributes) != 0 {
		t.Errorf("single-hash apply_upgrade produced staged attrs %v, want none", av.Attributes)
	}
}

// TestDecodeApplyUpgrade_poolTwoHash_realFixture pins the REGISTERED-POOL
// wire generation: apply_upgrade carries a 2-element Vec[Bytes] (the
// applied wasm hash + one staged predecessor), which the old decoder
// rejected with `body length 2 != 1`, dropping every pool apply_upgrade
// (686 events / 320 pools). Real r1-lake bytes: pool
// CDKVJYMN34ZIEXSLNFYHVAFF6M6FM5E2U6OHXOTBKH2WLBULXOE53YDP, ledger
// 56,505,116 (2025-04-07), part of the protocol-wide staged WASM
// upgrade. element[0] → Target, element[1] → Attributes["wasm_hash_1"].
func TestDecodeApplyUpgrade_poolTwoHash_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CDKVJYMN34ZIEXSLNFYHVAFF6M6FM5E2U6OHXOTBKH2WLBULXOE53YDP",
		Ledger:     56505116,
		TxHash:     "f38a9207f724b9624ce39e15a1aef59197db4913bdc7cf74867db3906ef7a852",
		EventIndex: 2,
		Topic:      []string{"AAAADwAAAA1hcHBseV91cGdyYWRlAAAA"},
		Value:      "AAAAEAAAAAEAAAACAAAADQAAACAubx2u2HKIGsUr9jcygHbNgaO1pw5GUkRTo1QwnHo3hgAAAA0AAAAgWWrOi4VUNkeFEoIaLg7LApc7G60KQFfcVB/Qyk188Dc=",
	}
	if got := classify(e); got != EventApplyUpgrade {
		t.Fatalf("classify = %q, want %q", got, EventApplyUpgrade)
	}
	av, err := decodeAdminEvent(e, EventApplyUpgrade, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeAdminEvent (pool 2-hash apply_upgrade): %v", err)
	}
	if av.Kind != AdminApplyUpgrade {
		t.Errorf("Kind = %q, want %q", av.Kind, AdminApplyUpgrade)
	}
	if av.Target != "2e6f1daed872881ac52bf637328076cd81a3b5a70e46524453a354309c7a3786" {
		t.Errorf("Target = %q, want element[0] wasm hash", av.Target)
	}
	if got := av.Attributes["wasm_hash_1"]; got != "596ace8b855436478512821a2e0ecb02973b1bad0a4057dc541fd0ca4d7cf037" {
		t.Errorf("wasm_hash_1 = %v, want element[1] staged wasm hash", got)
	}
	if _, present := av.Attributes["wasm_hash_2"]; present {
		t.Error("2-element body must not produce wasm_hash_2")
	}
}

// TestDecodeCommitUpgrade_poolThreeHash_realFixture pins the
// REGISTERED-POOL 3-element commit_upgrade body (the proposed hash + two
// staged hashes) — the busiest of the arity variations the old `!= 1`
// guard rejected. Real r1-lake bytes: pool
// CDKVJYMN34ZIEXSLNFYHVAFF6M6FM5E2U6OHXOTBKH2WLBULXOE53YDP, ledger
// 62,265,929 (2026-04-24). element[0] → Target, [1] → wasm_hash_1,
// [2] → wasm_hash_2.
func TestDecodeCommitUpgrade_poolThreeHash_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CDKVJYMN34ZIEXSLNFYHVAFF6M6FM5E2U6OHXOTBKH2WLBULXOE53YDP",
		Ledger:     62265929,
		TxHash:     "f9bfe04a5b82777e669d0a7571e6912273a52957149ab41f9d51004790890e54",
		EventIndex: 7,
		Topic:      []string{"AAAADwAAAA5jb21taXRfdXBncmFkZQAA"},
		Value:      "AAAAEAAAAAEAAAADAAAADQAAACDxB34Ld9peYtWW4Trq5BYBBMrZnn738xg6bJtuyedHzQAAAA0AAAAgB7qzDrk/nRlLB5o/ArqGghroK51HcKOMa3uFJN+eKE8AAAANAAAAIPeTf46MBtjP0jgzqmEu0HRatu5q32atO/e7r1n9Ya4Z",
	}
	if got := classify(e); got != EventCommitUpgrade {
		t.Fatalf("classify = %q, want %q", got, EventCommitUpgrade)
	}
	av, err := decodeAdminEvent(e, EventCommitUpgrade, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeAdminEvent (pool 3-hash commit_upgrade): %v", err)
	}
	if av.Target != "f1077e0b77da5e62d596e13aeae4160104cad99e7ef7f3183a6c9b6ec9e747cd" {
		t.Errorf("Target = %q, want element[0] wasm hash", av.Target)
	}
	if got := av.Attributes["wasm_hash_1"]; got != "07bab30eb93f9d194b079a3f02ba86821ae82b9d4770a38c6b7b8524df9e284f" {
		t.Errorf("wasm_hash_1 = %v", got)
	}
	if got := av.Attributes["wasm_hash_2"]; got != "f7937f8e8c06d8cfd23833aa612ed0745ab6ee6adf66ad3bf7bbaf59fd61ae19" {
		t.Errorf("wasm_hash_2 = %v", got)
	}
}

// TestDecodeSetPrivilegedAddrs_pool5elt_realFixture pins a REGISTERED
// POOL emitting the 5-element set_privileged_addrs (post-57.7M wire
// generation) — proving the v2 branch decodes pool bytes exactly as it
// does the router's. Real r1-lake bytes: pool
// CBL7MWLEZ4SU6YC5XL4T3WXKNKNO2UQVDVONOQSW5VVCYFWORROHY4AM, ledger
// 57,697,794 (2025-06-24). The privileged-address set is protocol-wide,
// so the decoded values match the router's v2 fixture.
func TestDecodeSetPrivilegedAddrs_pool5elt_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CBL7MWLEZ4SU6YC5XL4T3WXKNKNO2UQVDVONOQSW5VVCYFWORROHY4AM",
		Ledger:     57697794,
		TxHash:     "30ba5813f4e584ce152865799460d53d5d16830450f7d2bedfa582373d3f6e1d",
		EventIndex: 0,
		Topic:      []string{"AAAADwAAABRzZXRfcHJpdmlsZWdlZF9hZGRycw=="},
		Value:      "AAAAEAAAAAEAAAAFAAAAEgAAAAAAAAAAr4UDYWd/ywvTsSRB0NRM2w7KoisPZcPb4fpZk+XD67QAAAASAAAAAAAAAABrB99Lh3p1xYtFgkxWsF7lnsSvirC4yXmnLxRYF7aVBAAAABIAAAAAAAAAADzAe929VHnCmayZRVHmn90SJaJYM9yQ/RXerE7FSrO8AAAAEAAAAAEAAAABAAAAEgAAAAAAAAAAPMrM0BiS9+voAw3nyHOzk4mPbaTXHVu8AMIg4+5A+4oAAAASAAAAAAAAAAB7/A6mWvQQVdc774FmP/p4xb1sMlRWTophyo0AhpckYw==",
	}
	av, err := decodeAdminEvent(e, EventSetPrivilegedAddrs, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeAdminEvent (pool 5-element set_privileged_addrs): %v", err)
	}
	if got := av.Attributes["addr_0"]; got != "GCXYKA3BM574WC6TWESEDUGUJTNQ5SVCFMHWLQ634H5FTE7FYPV3JH3X" {
		t.Errorf("addr_0 = %v", got)
	}
	if got := av.Attributes["addr_3"]; got != "GB57YDVGLL2BAVOXHPXYCZR77J4MLPLMGJKFMTUKMHFI2AEGS4SGGW7N" {
		t.Errorf("addr_3 (v2 trailing role address) = %v", got)
	}
	list, ok := av.Attributes["addr_list"].([]string)
	if !ok || len(list) != 1 || list[0] != "GA6MVTGQDCJPP27IAMG6PSDTWOJYTD3NUTLR2W54ADBCBY7OID5YUDSI" {
		t.Errorf("addr_list = %v", av.Attributes["addr_list"])
	}
}

// TestDecodeApplyTransferOwnership_pool_realFixture pins a REGISTERED
// POOL apply_transfer_ownership (2 topics + Vec[Address×1]) — real
// r1-lake bytes from pool
// CCSY43EHJAHT3NQDYKAMJXRFBEEH7OXDL3J3VNGO33UUSEXWNN27GBIZ, ledger
// 55,363,729 (2025-01-20). Same wire shape as the router's; only the
// emitter (a pool) and the new EmergencyAdmin address differ.
func TestDecodeApplyTransferOwnership_pool_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CCSY43EHJAHT3NQDYKAMJXRFBEEH7OXDL3J3VNGO33UUSEXWNN27GBIZ",
		Ledger:     55363729,
		TxHash:     "f37d1843cf5057b2a720f7b67ef8991be0a4d64803a726876a4c8452aacd8450",
		EventIndex: 4,
		Topic: []string{
			"AAAADwAAABhhcHBseV90cmFuc2Zlcl9vd25lcnNoaXA=",
			"AAAADwAAAA5FbWVyZ2VuY3lBZG1pbgAA",
		},
		Value: "AAAAEAAAAAEAAAABAAAAEgAAAAAAAAAAjZ8TsQ0UtoeVz2IvdFyDAyCnYnVjFIrAYK6Rb8sP8Ys=",
	}
	av, err := decodeAdminEvent(e, EventApplyTransferOwnership, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeAdminEvent (pool apply_transfer_ownership): %v", err)
	}
	if got := av.Attributes["role"]; got != "EmergencyAdmin" {
		t.Errorf("role = %v, want EmergencyAdmin", got)
	}
	if av.Target != "GCGZ6E5RBUKLNB4VZ5RC65C4QMBSBJ3COVRRJCWAMCXJC36LB7YYWEKM" {
		t.Errorf("Target = %q", av.Target)
	}
}

// TestDecodeCommitTransferOwnership_pool_realFixture — pool
// commit_transfer_ownership (real r1-lake bytes: pool
// CCSY43EHJAHT3NQDYKAMJXRFBEEH7OXDL3J3VNGO33UUSEXWNN27GBIZ, ledger
// 55,363,698).
func TestDecodeCommitTransferOwnership_pool_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CCSY43EHJAHT3NQDYKAMJXRFBEEH7OXDL3J3VNGO33UUSEXWNN27GBIZ",
		Ledger:     55363698,
		TxHash:     "f37626febf953456fbc4c3f775b5912c599959de0742c6c381a00c040ce5df3a",
		EventIndex: 4,
		Topic: []string{
			"AAAADwAAABljb21taXRfdHJhbnNmZXJfb3duZXJzaGlwAAAA",
			"AAAADwAAAA5FbWVyZ2VuY3lBZG1pbgAA",
		},
		Value: "AAAAEAAAAAEAAAABAAAAEgAAAAAAAAAAjZ8TsQ0UtoeVz2IvdFyDAyCnYnVjFIrAYK6Rb8sP8Ys=",
	}
	av, err := decodeAdminEvent(e, EventCommitTransferOwnership, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeAdminEvent (pool commit_transfer_ownership): %v", err)
	}
	if got := av.Attributes["role"]; got != "EmergencyAdmin" {
		t.Errorf("role = %v, want EmergencyAdmin", got)
	}
	if av.Target != "GCGZ6E5RBUKLNB4VZ5RC65C4QMBSBJ3COVRRJCWAMCXJC36LB7YYWEKM" {
		t.Errorf("Target = %q", av.Target)
	}
}

// TestDecodeEmergencyMode_pool_realFixtures pins the pool-emitted
// void-body emergency toggles (real r1-lake bytes: pool
// CAZERARL4EWIJYX2GRCJF32LG3S2QPL4CMPIGHNZHFAMMNXEARAY5RPP, ledgers
// 58,787,169 / 58,787,188 — a pool disabling trades under an incident
// then re-enabling minutes later).
func TestDecodeEmergencyMode_pool_realFixtures(t *testing.T) {
	enable := &events.Event{
		ContractID: "CAZERARL4EWIJYX2GRCJF32LG3S2QPL4CMPIGHNZHFAMMNXEARAY5RPP",
		Ledger:     58787169,
		TxHash:     "6c9b474bcc40ce00b4ed4635a5bb64ff21034ddf89f5519d3ce9ec7525dbdc8a",
		EventIndex: 0,
		Topic:      []string{"AAAADwAAABVlbmFibGVfZW1lcmdlbmN5X21vZGUAAAA="},
		Value:      "AAAAAQ==",
	}
	av, err := decodeAdminEvent(enable, EventEnableEmergencyMode, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeAdminEvent (pool enable_emergency_mode): %v", err)
	}
	if av.Kind != AdminEnableEmergencyMode || av.ContractID != enable.ContractID {
		t.Errorf("enable: Kind=%q ContractID=%q", av.Kind, av.ContractID)
	}
	if av.Admin != "" || av.Target != "" || len(av.Attributes) != 0 {
		t.Errorf("enable: expected empty-payload marker, got admin=%q target=%q attrs=%v", av.Admin, av.Target, av.Attributes)
	}

	disable := &events.Event{
		ContractID: "CAZERARL4EWIJYX2GRCJF32LG3S2QPL4CMPIGHNZHFAMMNXEARAY5RPP",
		Ledger:     58787188,
		TxHash:     "f67862765583f21946508e9d6599c4854eff4e233e6d4f9208def66304d6b6ca",
		EventIndex: 0,
		Topic:      []string{"AAAADwAAABZkaXNhYmxlX2VtZXJnZW5jeV9tb2RlAAA="},
		Value:      "AAAAAQ==",
	}
	dav, err := decodeAdminEvent(disable, EventDisableEmergencyMode, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeAdminEvent (pool disable_emergency_mode): %v", err)
	}
	if dav.Kind != AdminDisableEmergencyMode || dav.ContractID != disable.ContractID {
		t.Errorf("disable: Kind=%q ContractID=%q", dav.Kind, dav.ContractID)
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
	if len(av.Attributes) != 0 {
		t.Errorf("single-hash commit_upgrade produced staged attrs %v, want none", av.Attributes)
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

// TestDecodeSetPrivilegedAddrs_v2RealFixture pins the POST-57.7M wire
// generation (contract-schema-evolution): the same shape plus ONE
// trailing plain Address. Real r1-lake bytes — the canonical router's
// single 5-element `set_privileged_addrs` event (ledger 57,711,797,
// closed 2025-06-25), which was one of the 41 blind
// undecodable-but-matched events on aquarius's first full-range
// completeness reconcile (2026-08-01) while the decoder pinned
// arity==4.
func TestDecodeSetPrivilegedAddrs_v2RealFixture(t *testing.T) {
	e := &events.Event{
		ContractID: MainnetRouter,
		Ledger:     57711797,
		TxHash:     "767bc4de88834051edcb958ce3d56538b5f8946453136bf7d945d2bddaaaca8c",
		EventIndex: 0,
		Topic: []string{
			"AAAADwAAABRzZXRfcHJpdmlsZWdlZF9hZGRycw==",
		},
		Value: "AAAAEAAAAAEAAAAFAAAAEgAAAAAAAAAAr4UDYWd/ywvTsSRB0NRM2w7KoisPZcPb4fpZk+XD67QAAAASAAAAAAAAAABrB99Lh3p1xYtFgkxWsF7lnsSvirC4yXmnLxRYF7aVBAAAABIAAAAAAAAAADzAe929VHnCmayZRVHmn90SJaJYM9yQ/RXerE7FSrO8AAAAEAAAAAEAAAABAAAAEgAAAAAAAAAAPMrM0BiS9+voAw3nyHOzk4mPbaTXHVu8AMIg4+5A+4oAAAASAAAAAAAAAAB7/A6mWvQQVdc774FmP/p4xb1sMlRWTophyo0AhpckYw==",
	}
	if got := classify(e); got != EventSetPrivilegedAddrs {
		t.Fatalf("classify = %q, want %q", got, EventSetPrivilegedAddrs)
	}
	av, err := decodeAdminEvent(e, EventSetPrivilegedAddrs, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeAdminEvent (v2 5-element body): %v", err)
	}
	if got := av.Attributes["addr_0"]; got != "GCXYKA3BM574WC6TWESEDUGUJTNQ5SVCFMHWLQ634H5FTE7FYPV3JH3X" {
		t.Errorf("addr_0 = %v", got)
	}
	if got := av.Attributes["addr_1"]; got != "GBVQPX2LQ55HLRMLIWBEYVVQL3SZ5RFPRKYLRSLZU4XRIWAXW2KQIMMD" {
		t.Errorf("addr_1 = %v", got)
	}
	if got := av.Attributes["addr_2"]; got != "GA6MA665XVKHTQUZVSMUKUPGT7OREJNCLAZ5ZEH5CXPKYTWFJKZ3YSEK" {
		t.Errorf("addr_2 = %v", got)
	}
	list, ok := av.Attributes["addr_list"].([]string)
	if !ok || len(list) != 1 || list[0] != "GA6MVTGQDCJPP27IAMG6PSDTWOJYTD3NUTLR2W54ADBCBY7OID5YUDSI" {
		t.Errorf("addr_list = %v", av.Attributes["addr_list"])
	}
	if got := av.Attributes["addr_3"]; got != "GB57YDVGLL2BAVOXHPXYCZR77J4MLPLMGJKFMTUKMHFI2AEGS4SGGW7N" {
		t.Errorf("addr_3 (v2 trailing role address) = %v", got)
	}
}

// A 4-element (v1) body must NOT grow an addr_3 attribute, and any
// other arity still fails closed.
func TestDecodeSetPrivilegedAddrs_arityGuards(t *testing.T) {
	// v1 real fixture from TestDecodeSetPrivilegedAddrs_realFixture.
	e := &events.Event{
		ContractID: "CA7RQDMMV6E53P5EDZA5GPWBZ33AMW2ZNO42XLI2RGRIAP4QXIARUOJQ",
		Ledger:     54150744,
		TxHash:     "123e9b76b6c9288eeeeb8eee3e8890b606df4bac34edca86fea07f2af3fee35d",
		EventIndex: 0,
		Topic:      []string{"AAAADwAAABRzZXRfcHJpdmlsZWdlZF9hZGRycw=="},
		Value:      "AAAAEAAAAAEAAAAEAAAAEgAAAAAAAAAAdcoKqsEbsyr0vqtizcS1v/F9m86ZJaBsJIfsPrVq9c4AAAASAAAAAAAAAAA9wKBfw2ZvhidSVPc8cyXssqJE8elIez+Oy0nXRkV6CAAAABIAAAAAAAAAABDqOTDnu1sCH7swm1DeeaF2JZ2VREpGXUW2rPs9Xi5jAAAAEAAAAAEAAAABAAAAEgAAAAAAAAAAEOo5MOe7WwIfuzCbUN55oXYlnZVESkZdRbas+z1eLmM=",
	}
	av, err := decodeAdminEvent(e, EventSetPrivilegedAddrs, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("v1 decode: %v", err)
	}
	if _, present := av.Attributes["addr_3"]; present {
		t.Error("v1 4-element body must not produce addr_3")
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
