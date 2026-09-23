// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// r1ReserveAccounts is supply.sdf_reserve_accounts as deployed
// (configs/ansible/roles/archival-node/defaults/main.yml,
// stellarindex_sdf_reserve_accounts — 16 accounts transcribed from
// stellar/dashboard common/lumens.js on 2026-07-02). The parser's
// definition of "the published set" is pinned to this list below: if
// the two ever disagree on the fixture, either the transcription or
// the parser is wrong, and the check must say which.
var r1ReserveAccounts = []string{
	"GD2D6JG6D3V52ZMPIYSVHYFKVNIMXGYVLYJQ3HYHG5YDPGJ3DCRGPLTP",
	"GA2VRL65L3ZFEDDJ357RGI3MAOKPJZ2Z3IJTPSC24I4KDTNFSVEQURRA",
	"GB6NVEN5HSUBKMYCE5ZOWSK5K23TBWRUQLZY3KNMXUZ3AQ2ESC4MY4AQ",
	"GATL3ETTZ3XDGFXX2ELPIKCZL7S5D2HY3VK4T7LRPD6DW5JOLAEZSZBA",
	"GAKGC35HMNB7A3Q2V5SQU6VJC2JFTZB6I7ZW77SJSMRCOX2ZFBGJOCHH",
	"GAPV2C4BTHXPL2IVYDXJ5PUU7Q3LAXU7OAQDP7KVYHLCNM2JTAJNOQQI",
	"GCVJDBALC2RQFLD2HYGQGWNFZBCOD2CPOTN3LE7FWRZ44H2WRAVZLFCU",
	"GC3ITNZSVVPOWZ5BU7S64XKNI5VPTRSBEXXLS67V4K6LEUETWBMTE7IH",
	"GBEVKAYIPWC5AQT6D4N7FC3XGKRRBMPCAMTO3QZWMHHACLHTMAHAM2TP",
	"GDUY7J7A33TQWOSOQGDO776GGLM3UQERL4J3SPT56F6YS4ID7MLDERI4",
	"GCPWKVQNLDPD4RNP5CAXME4BEDTKSSYRR4MMEL4KG65NEGCOGNJW7QI2",
	"GDKIJJIKXLOM2NRMPNQZUUYK24ZPVFC6426GZAEP3KUK6KEJLACCWNMX",
	"GDWXQOTIIDO2EUK4DIGIBLEHLME2IAJRNU6JDFS5B2ZTND65P7J36WQZ",
	"GAMGGUQKKJ637ILVDOSCT5X7HYSZDUPGXSUW67B2UKMG2HEN5TPWN3LQ",
	"GANII5Y2LABEBK74NWNKS4NREX2T52YTBGQDRDKVBFRIIF5VE4ORYOVY",
	"GBEZOC5U4TVH7ZY5N3FLYHTCZSI6VFGTULG7PBITLF5ZEBPJXFT46YZM",
}

// Two accounts that appear in the fixture but are NOT reserve accounts:
// the burn address (subtracted from TOTAL supply, not circulating) and
// the escrow SDF retired by commenting its row out. A parser that
// sweeps every G-strkey in the file would include both, and the live
// r1 list would then read as drifted by two on day one.
const (
	fixtureVoidAccount     = "GALAXYVOIDAOPZTDLHILAJQKCVVFMD4IKLXLSZV5YHO7VY74IWZILUTO"
	fixtureRetiredEscrow   = "GBA6XT7YBQOERXT656T74LYUVJ6MEIOC5EUETGAQNHQHEPUFPKCW5GYM"
	fixtureUpgradeReserve  = "GBEZOC5U4TVH7ZY5N3FLYHTCZSI6VFGTULG7PBITLF5ZEBPJXFT46YZM"
	fixtureDirectDevelopmt = "GB6NVEN5HSUBKMYCE5ZOWSK5K23TBWRUQLZY3KNMXUZ3AQ2ESC4MY4AQ"
)

func readLumensFixture(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("testdata", "sdf_dashboard_common_lumens.js"))
	if err != nil {
		t.Fatal(err)
	}
	return string(src)
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestDiffReserveList pins the pure set difference on the three
// fixtures the finding asks for — equal, missing, extra — plus the
// invariants that make the verdict deterministic: order-independence,
// duplicate-tolerance and sorted output.
func TestDiffReserveList(t *testing.T) {
	a, b, c := "GAAA", "GBBB", "GCCC"
	cases := []struct {
		name                   string
		configured, published  []string
		wantMissing, wantExtra []string
	}{
		{"equal", []string{a, b, c}, []string{a, b, c}, nil, nil},
		{"equal regardless of order", []string{c, a, b}, []string{a, b, c}, nil, nil},
		{"equal with a duplicate on our side", []string{a, a, b, c}, []string{a, b, c}, nil, nil},
		{"missing: SDF added an account we do not exclude", []string{a, b}, []string{a, b, c}, []string{c}, nil},
		{"extra: SDF retired an account we still exclude", []string{a, b, c}, []string{a, b}, nil, []string{c}},
		{"both, sorted", []string{c, b}, []string{a, b}, []string{a}, []string{c}},
		{"empty config vs published: everything missing", nil, []string{b, a}, []string{a, b}, nil},
		{"both empty", nil, nil, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := diffReserveList(tc.configured, tc.published)
			if !reflect.DeepEqual(d.missing, tc.wantMissing) {
				t.Errorf("missing = %v, want %v", d.missing, tc.wantMissing)
			}
			if !reflect.DeepEqual(d.extra, tc.wantExtra) {
				t.Errorf("extra = %v, want %v", d.extra, tc.wantExtra)
			}
			if d.empty() != (len(tc.wantMissing) == 0 && len(tc.wantExtra) == 0) {
				t.Errorf("empty() = %v is inconsistent with %v/%v", d.empty(), d.missing, d.extra)
			}
		})
	}
}

// TestParsePublishedReserveList_RealSource parses the REAL
// common/lumens.js (as published 2026-09-18, testdata/) and pins the
// published set to r1's deployed 16: the accounts table's 15 live rows
// plus the network-upgrade reserve constant; not the commented-out
// escrow, not the burn address.
func TestParsePublishedReserveList_RealSource(t *testing.T) {
	got, err := parsePublishedReserveList(readLumensFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if want := sortedCopy(r1ReserveAccounts); !reflect.DeepEqual(got, want) {
		d := diffReserveList(want, got)
		t.Fatalf("published set != r1 deployed set: not-in-r1=%v not-published=%v\n got=%v", d.missing, d.extra, got)
	}
	for _, excluded := range []string{fixtureVoidAccount, fixtureRetiredEscrow} {
		for _, a := range got {
			if a == excluded {
				t.Errorf("%s must not be in the published reserve set", excluded)
			}
		}
	}
	// The upgrade reserve lives outside the accounts table; a parser
	// that only reads the table would report it as `extra` on r1.
	if i := sort.SearchStrings(got, fixtureUpgradeReserve); i >= len(got) || got[i] != fixtureUpgradeReserve {
		t.Errorf("networkUpgradeReserveAccount %s must be in the published reserve set", fixtureUpgradeReserve)
	}
	if d := diffReserveList(r1ReserveAccounts, got); !d.empty() {
		t.Errorf("r1's deployed list drifts from the fixture: %+v", d)
	}
}

// TestParsePublishedReserveList_ShapeChangeIsAnError — a source the
// parser does not recognise must be an error, never an empty or
// partial list. An empty list would diff every configured account as
// `extra`; a partial one would report phantom `extra` accounts. Both
// are false drift verdicts, and the caller maps the error to SKIP.
func TestParsePublishedReserveList_ShapeChangeIsAnError(t *testing.T) {
	fixture := readLumensFixture(t)
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"no accounts table", strings.Replace(fixture, "const accounts = {", "const reserveAccounts = {", 1), "no `const accounts"},
		{"empty accounts table", emptyAccountsTable(t, fixture), "no G-strkey entries"},
		{"upgrade reserve constant gone", strings.Replace(fixture, "networkUpgradeReserveAccount =", "upgradeReserve =", 1), "networkUpgradeReserveAccount"},
		{"html error page", "<html><body>rate limited</body></html>", "no `const accounts"},
		{"empty body", "", "no `const accounts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePublishedReserveList(tc.src)
			if err == nil {
				t.Fatalf("want error containing %q, got list of %d", tc.want, len(got))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the cause %q", err, tc.want)
			}
		})
	}
}

// TestParsePublishedReserveList_RetirementAndAddition — the two
// upstream edits the check exists to catch, applied to the real
// source: SDF retires an account by commenting its row out (it must
// leave the published set), and adds one by inserting a row (it must
// enter it).
func TestParsePublishedReserveList_RetirementAndAddition(t *testing.T) {
	fixture := readLumensFixture(t)

	retired := strings.Replace(fixture,
		`  directDevelopment: "`+fixtureDirectDevelopmt+`",`,
		`  // directDevelopment: "`+fixtureDirectDevelopmt+`",`, 1)
	if retired == fixture {
		t.Fatal("fixture row for directDevelopment not found — fixture shape changed")
	}
	got, err := parsePublishedReserveList(retired)
	if err != nil {
		t.Fatal(err)
	}
	if d := diffReserveList(r1ReserveAccounts, got); !reflect.DeepEqual(d.extra, []string{fixtureDirectDevelopmt}) || len(d.missing) != 0 {
		t.Errorf("commenting out a row must surface it as extra on our side; got %+v", d)
	}

	const added = "GNEWNEWNEWNEWNEWNEWNEWNEWNEWNEWNEWNEWNEWNEWNEWNEWNEWNEWN"
	if len(added) != 56 {
		t.Fatalf("test key is %d chars, want 56", len(added))
	}
	appended := strings.Replace(fixture, "const accounts = {\n", "const accounts = {\n  sdfNewProgram: \""+added+"\",\n", 1)
	got, err = parsePublishedReserveList(appended)
	if err != nil {
		t.Fatal(err)
	}
	if d := diffReserveList(r1ReserveAccounts, got); !reflect.DeepEqual(d.missing, []string{added}) || len(d.extra) != 0 {
		t.Errorf("a new row must surface as missing on our side; got %+v", d)
	}
}

// writeReserveConfig writes a stellarindex.toml fragment carrying the
// given reserve list, padded with unrelated sections and an unknown
// key so the test proves the partial decode ignores everything but
// supply.sdf_reserve_accounts (config.Load would reject the unknown
// key and validate the whole file; the list check must not inherit
// either failure mode).
func writeReserveConfig(t *testing.T, accounts []string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("[storage]\npostgres_dsn = \"postgres://nobody@127.0.0.1/none\"\n\n")
	b.WriteString("[supply]\nsome_future_key = true\nsdf_reserve_accounts = [\n")
	for _, a := range accounts {
		b.WriteString("  \"" + a + "\",\n")
	}
	b.WriteString("]\n[supply.reserve_balances_stroops]\n")
	path := filepath.Join(t.TempDir(), "stellarindex.toml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestReconcileReserveList_Outcomes drives the check end to end against
// a stub of the published source and a temp config: in step, missing,
// extra, published source dark (SKIP — never a verdict), published
// source reshaped (SKIP), and config unreadable (a FAILURE of our own
// side, never a skip).
func TestReconcileReserveList_Outcomes(t *testing.T) {
	fixture := readLumensFixture(t)
	ctx := context.Background()
	c := &http.Client{Timeout: 5 * time.Second}

	serve := func(status int, body string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	live := serve(http.StatusOK, fixture)

	t.Run("in step: verified, no drift, ok", func(t *testing.T) {
		r := reconcileReserveList(ctx, c, writeReserveConfig(t, r1ReserveAccounts), live)
		if !r.verified() || !r.ok() || r.skipped || r.err != nil {
			t.Fatalf("want verified+ok, got %+v", r)
		}
		if len(r.configured) != 16 || len(r.published) != 16 {
			t.Errorf("configured=%d published=%d, want 16/16", len(r.configured), len(r.published))
		}
	})

	t.Run("missing: SDF publishes one we do not exclude", func(t *testing.T) {
		r := reconcileReserveList(ctx, c, writeReserveConfig(t, r1ReserveAccounts[:15]), live)
		if !r.verified() || r.ok() {
			t.Fatalf("want verified but not ok, got %+v", r)
		}
		if !reflect.DeepEqual(r.drift.missing, []string{r1ReserveAccounts[15]}) || len(r.drift.extra) != 0 {
			t.Errorf("drift = %+v, want missing=[%s]", r.drift, r1ReserveAccounts[15])
		}
	})

	t.Run("extra: we exclude one SDF no longer publishes", func(t *testing.T) {
		const stale = "GSTALESTALESTALESTALESTALESTALESTALESTALESTALESTALESTALE"
		r := reconcileReserveList(ctx, c, writeReserveConfig(t, append(sortedCopy(r1ReserveAccounts), stale)), live)
		if !r.verified() || r.ok() {
			t.Fatalf("want verified but not ok, got %+v", r)
		}
		if !reflect.DeepEqual(r.drift.extra, []string{stale}) || len(r.drift.missing) != 0 {
			t.Errorf("drift = %+v, want extra=[%s]", r.drift, stale)
		}
	})

	t.Run("published source dark: skipped, never a verdict", func(t *testing.T) {
		r := reconcileReserveList(ctx, c, writeReserveConfig(t, r1ReserveAccounts[:15]), serve(http.StatusBadGateway, "down"))
		if !r.skipped || r.verified() || r.ok() || r.err != nil {
			t.Fatalf("want skipped, got %+v", r)
		}
		if !r.drift.empty() {
			t.Errorf("a skipped check must carry no drift verdict, got %+v", r.drift)
		}
		if !strings.Contains(r.note, "skipped") {
			t.Errorf("note must say skipped: %q", r.note)
		}
	})

	t.Run("published source reshaped: skipped, not every-account-extra", func(t *testing.T) {
		r := reconcileReserveList(ctx, c, writeReserveConfig(t, r1ReserveAccounts), serve(http.StatusOK, "export const accounts = [];"))
		if !r.skipped || r.verified() {
			t.Fatalf("want skipped, got %+v", r)
		}
		if len(r.drift.extra) != 0 {
			t.Errorf("a reshaped source must not read as %d extra accounts", len(r.drift.extra))
		}
	})

	t.Run("config unreadable: our side, a failure not a skip", func(t *testing.T) {
		r := reconcileReserveList(ctx, c, filepath.Join(t.TempDir(), "absent.toml"), live)
		if r.err == nil || r.skipped || r.verified() || r.ok() {
			t.Fatalf("want err (not skipped), got %+v", r)
		}
	})

	t.Run("config with no supply section: empty configured set, everything missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stellarindex.toml")
		if err := os.WriteFile(path, []byte("[storage]\npostgres_dsn = \"x\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		r := reconcileReserveList(ctx, c, path, live)
		if !r.verified() || r.ok() {
			t.Fatalf("want verified drift, got %+v", r)
		}
		if len(r.drift.missing) != 16 {
			t.Errorf("missing = %d, want 16 (an unset list excludes nothing)", len(r.drift.missing))
		}
	})
}

// TestRenderServedValueProm_ReserveList pins the exposition contract
// for the list check: its verdict lives in its own
// stellarindex_sdf_reserve_list_drift{kind} family (both kinds always
// emitted when verified, so `> 0` alerts have a series to go quiet
// on), it shares served_value_skipped so a dark source is covered by
// persistently_skipped, it NEVER emits served_value_ok (its remedy is
// account-level, not a tolerance), and — the F5 rule — a skipped or
// config-failed run emits NO drift gauge at all.
func TestRenderServedValueProm_ReserveList(t *testing.T) {
	now := time.Unix(1_751_000_000, 0)
	value := []servedValueResult{{name: "a", relErr: 0.001, ok: true}}

	t.Run("verified drift", func(t *testing.T) {
		body := renderServedValueProm(value, &reserveListResult{
			configured: []string{"GA", "GB"}, published: []string{"GA", "GC", "GD"},
			drift: reserveListDrift{missing: []string{"GC", "GD"}, extra: []string{"GB"}},
		}, now)
		for _, want := range []string{
			"# HELP stellarindex_sdf_reserve_list_drift ",
			"# TYPE stellarindex_sdf_reserve_list_drift gauge",
			`stellarindex_sdf_reserve_list_drift{kind="missing"} 2`,
			`stellarindex_sdf_reserve_list_drift{kind="extra"} 1`,
			`stellarindex_served_value_skipped{check="sdf_reserve_list"} 0`,
			`stellarindex_served_value_ok{check="a"} 1`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("body missing %q:\n%s", want, body)
			}
		}
		if strings.Contains(body, `stellarindex_served_value_ok{check="sdf_reserve_list"}`) {
			t.Errorf("list check must not ride served_value_ok:\n%s", body)
		}
	})

	t.Run("verified in step emits explicit zeros", func(t *testing.T) {
		body := renderServedValueProm(value, &reserveListResult{configured: []string{"GA"}, published: []string{"GA"}}, now)
		for _, want := range []string{
			`stellarindex_sdf_reserve_list_drift{kind="missing"} 0`,
			`stellarindex_sdf_reserve_list_drift{kind="extra"} 0`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("body missing %q:\n%s", want, body)
			}
		}
	})

	t.Run("skipped emits skipped=1 and no drift gauge", func(t *testing.T) {
		body := renderServedValueProm(value, &reserveListResult{skipped: true}, now)
		if !strings.Contains(body, `stellarindex_served_value_skipped{check="sdf_reserve_list"} 1`) {
			t.Errorf("skipped list check must emit served_value_skipped=1:\n%s", body)
		}
		if strings.Contains(body, "stellarindex_sdf_reserve_list_drift{") {
			t.Errorf("skipped list check must emit NO drift gauge (F5):\n%s", body)
		}
	})

	t.Run("config failure emits no drift gauge and is not a skip", func(t *testing.T) {
		body := renderServedValueProm(value, &reserveListResult{err: os.ErrNotExist}, now)
		if !strings.Contains(body, `stellarindex_served_value_skipped{check="sdf_reserve_list"} 0`) {
			t.Errorf("config failure is our side, not a skip:\n%s", body)
		}
		if strings.Contains(body, "stellarindex_sdf_reserve_list_drift{") {
			t.Errorf("unverified list check must emit NO drift gauge:\n%s", body)
		}
	})

	t.Run("nil (check not run) emits no sample for it", func(t *testing.T) {
		// The family's HELP/TYPE header is unconditional (a header with
		// no samples is valid exposition, and the lint wants every
		// family's header beside its emitter); what must be absent is
		// any SAMPLE — a skipped=0 here would claim the check ran.
		body := renderServedValueProm(value, nil, now)
		for _, banned := range []string{
			`stellarindex_served_value_skipped{check="sdf_reserve_list"}`,
			"stellarindex_sdf_reserve_list_drift{",
		} {
			if strings.Contains(body, banned) {
				t.Errorf("a list check that did not run must leave no sample %q:\n%s", banned, body)
			}
		}
	})
}

// TestParsePublishedReserveList_UnrecognisedRowIsNeverAPartialList — an
// upstream reshape of ONE row must fail the whole parse (the caller
// skips), never yield the rows before it. A partial list reads as
// phantom `extra` accounts whose runbook remedy is deleting real SDF
// reserve accounts from our config.
func TestParsePublishedReserveList_UnrecognisedRowIsNeverAPartialList(t *testing.T) {
	fixture := readLumensFixture(t)
	const row = `  escrowJan2023: "GA2VRL65L3ZFEDDJ357RGI3MAOKPJZ2Z3IJTPSC24I4KDTNFSVEQURRA",`
	if !strings.Contains(fixture, row) {
		t.Fatal("fixture row for escrowJan2023 not found — fixture shape changed")
	}
	cases := []struct{ name, replacement string }{
		{"nested object row", `  escrowJan2023: { id: "GA2VRL65L3ZFEDDJ357RGI3MAOKPJZ2Z3IJTPSC24I4KDTNFSVEQURRA", retiredAt: null },`},
		{"spread of another table", `  ...legacyEscrows,`},
		{"computed value", `  escrowJan2023: pick("GA2VRL65L3ZFEDDJ357RGI3MAOKPJZ2Z3IJTPSC24I4KDTNFSVEQURRA"),`},
		{"missing comma between rows", `  escrowJan2023: "GA2VRL65L3ZFEDDJ357RGI3MAOKPJZ2Z3IJTPSC24I4KDTNFSVEQURRA"`},
		{"template literal value", "  escrowJan2023: `GA2VRL65L3ZFEDDJ357RGI3MAOKPJZ2Z3IJTPSC24I4KDTNFSVEQURRA`,"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePublishedReserveList(strings.Replace(fixture, row, tc.replacement, 1))
			if err == nil {
				t.Fatalf("an unrecognised row must fail the parse, got a list of %d of the 16 published", len(got))
			}
		})
	}
}

// TestParsePublishedReserveList_EquivalentSpellingsParseWhole — JS
// spellings of the SAME table (single quotes, quoted keys, trailing and
// block comments, a brace inside a comment) must yield the full
// published set, not a truncated one.
func TestParsePublishedReserveList_EquivalentSpellingsParseWhole(t *testing.T) {
	fixture := readLumensFixture(t)
	const (
		row1 = `  escrowJan2022: "GD2D6JG6D3V52ZMPIYSVHYFKVNIMXGYVLYJQ3HYHG5YDPGJ3DCRGPLTP",`
		row2 = `  sdfGrowth: "GCVJDBALC2RQFLD2HYGQGWNFZBCOD2CPOTN3LE7FWRZ44H2WRAVZLFCU",`
	)
	cases := []struct{ name, r1, r2 string }{
		{"single-quoted values", `  escrowJan2022: 'GD2D6JG6D3V52ZMPIYSVHYFKVNIMXGYVLYJQ3HYHG5YDPGJ3DCRGPLTP',`, `  sdfGrowth: 'GCVJDBALC2RQFLD2HYGQGWNFZBCOD2CPOTN3LE7FWRZ44H2WRAVZLFCU',`},
		{"quoted keys", `  "escrowJan2022": "GD2D6JG6D3V52ZMPIYSVHYFKVNIMXGYVLYJQ3HYHG5YDPGJ3DCRGPLTP",`, `  'sdfGrowth': "GCVJDBALC2RQFLD2HYGQGWNFZBCOD2CPOTN3LE7FWRZ44H2WRAVZLFCU",`},
		{"brace inside a comment", "  // retired {see escrowJan2021}\n" + row1, row2 + " /* hot: {a} */"},
		{"trailing line comment", row1 + " // 2022 escrow", row2},
	}
	want := sortedCopy(r1ReserveAccounts)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(strings.Replace(fixture, row1, tc.r1, 1), row2, tc.r2, 1)
			if src == fixture {
				t.Fatal("fixture rows not found — fixture shape changed")
			}
			got, err := parsePublishedReserveList(src)
			if err != nil {
				t.Fatal(err)
			}
			if d := diffReserveList(want, got); !d.empty() {
				t.Errorf("an equivalent spelling changed the published set: %+v", d)
			}
		})
	}
}

// TestReconcileReserveList_ImplausibleRetirementIsSkipped — a published
// table the grammar accepts but that retires more accounts at once than
// SDF plausibly does (e.g. rows moved to a second table) must not become
// a drift verdict whose remedy deletes reserve accounts: it is skipped,
// still naming the accounts, and the bound itself stays a verdict.
func TestReconcileReserveList_ImplausibleRetirementIsSkipped(t *testing.T) {
	fixture := readLumensFixture(t)
	c := &http.Client{Timeout: 5 * time.Second}
	serveWithout := func(t *testing.T, drop int) string {
		t.Helper()
		src := fixture
		for _, a := range r1ReserveAccounts[:drop] {
			src = dropTableRow(t, src, a)
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(src))
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	cfg := writeReserveConfig(t, r1ReserveAccounts)

	r := reconcileReserveList(context.Background(), c, cfg, serveWithout(t, maxPlausibleReserveRetirements))
	if !r.verified() || len(r.drift.extra) != maxPlausibleReserveRetirements {
		t.Fatalf("%d retirements is within bound and must be a verdict, got %+v", maxPlausibleReserveRetirements, r)
	}

	r = reconcileReserveList(context.Background(), c, cfg, serveWithout(t, maxPlausibleReserveRetirements+1))
	if !r.skipped || r.verified() || r.ok() {
		t.Fatalf("%d retirements at once must be skipped, not a drift verdict; got %+v", maxPlausibleReserveRetirements+1, r)
	}
	if len(r.drift.extra) != maxPlausibleReserveRetirements+1 || !strings.Contains(r.note, "implausible") {
		t.Errorf("the skip must still name the accounts and why: extra=%v note=%q", r.drift.extra, r.note)
	}
	if body := renderServedValueProm(nil, &r, time.Unix(0, 0)); strings.Contains(body, "stellarindex_sdf_reserve_list_drift{") {
		t.Errorf("a refused diff must not emit a drift gauge:\n%s", body)
	}
}

// dropTableRow deletes the whole accounts-table row (`key:` through the
// end of the line carrying the quoted account) from src.
func dropTableRow(t *testing.T, src, account string) string {
	t.Helper()
	i := strings.Index(src, `"`+account+`"`)
	if i < 0 {
		t.Fatalf("account %s not in fixture", account)
	}
	start := strings.LastIndex(src[:strings.LastIndex(src[:i], ":")], "\n") + 1
	end := i + strings.Index(src[i:], "\n") + 1
	return src[:start] + src[end:]
}

// emptyAccountsTable replaces every row of the fixture's accounts table
// with nothing, leaving `const accounts = {}`.
func emptyAccountsTable(t *testing.T, fixture string) string {
	t.Helper()
	const open = "const accounts = {"
	i := strings.Index(fixture, open)
	j := -1
	if i >= 0 {
		j = strings.Index(fixture[i:], "\n};")
	}
	if j < 0 {
		t.Fatal("accounts table not found — fixture shape changed")
	}
	return fixture[:i] + open + fixture[i+j+1:]
}
