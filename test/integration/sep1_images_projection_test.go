//go:build integration

package integration_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// AllSep1Images against a real Postgres.
//
// Every claim here needs the database. The scan is a LATERAL over
// jsonb_array_elements, which raises 22023 on anything that is not an
// array — and one such row fails the whole statement, blanking the logo
// map for every issuer. None of that exists in Go to unit-test, so the
// only honest test is one that EXECUTES the query against the shapes an
// attacker-authored stellar.toml can actually put in the column.
//
// What this pins is the OUTCOME: the right rows, and no error, across the
// whole hostile set in one pass. It does NOT distinguish the query's CASE
// guard from a plain WHERE guard — measured on this image the planner
// pushes either below the lateral. The CASE is there because that is a
// property of the plan rather than of the query; see allSep1ImagesQuery.
//
// It also re-runs the brand hijack from cold audit 2026-08-03 through the
// real SQL. The provenance rule lives in Go (timescale.sep1ImageFrom, unit
// tested), but "the rule is applied to what the query actually returns" is
// a claim about the two together.
//
// Production opens the store with timescale.Open and wires it into
// v1.New(v1.Options{Sep1Cache: store}) (cmd/stellarindex-api/main.go); this
// test uses that same constructor.
func TestAllSep1ImagesProjection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		circle   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		attacker = "GAS4V4XZ3JHFTGKHCTMTWIIVFHGLBUMSQMGZM4RIWYPQHNXBAOGZBKHR"
		junk     = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		noToml   = "GD6VWBXI6NY18X5RJVSCQBRQOFB5PVRAPKQ6IMDG5UAXNHMLRPQ7LTKO"
	)

	// Every payload shape reachable from a hostile or broken stellar.toml,
	// in ONE row set — the query must survive all of them in a single pass,
	// which is the property a per-shape unit test cannot establish.
	seedIssuers(t, ctx, store, []seedIssuer{
		{g: circle, homeDomain: "centre.io"},
		{g: attacker, homeDomain: "attacker.example"},
		{g: junk, homeDomain: "junk.example"},
		{g: noToml, homeDomain: "nothing.example"},
	})

	setPayload := func(g, payload string) {
		t.Helper()
		if _, uerr := store.DB().ExecContext(ctx,
			`UPDATE issuers SET sep1_payload = $2::jsonb WHERE g_strkey = $1`, g, payload,
		); uerr != nil {
			t.Fatalf("set payload for %s: %v", g, uerr)
		}
	}

	setPayload(circle, `{"OrgName":"Circle","Currencies":[
		{"Code":"USDC","Issuer":"`+circle+`","Image":"https://circle.com/usdc.svg"}
	]}`)

	// The hijack: the attacker's own TOML declares BOTH its own asset and
	// Circle's. Only its own may survive.
	setPayload(attacker, `{"OrgName":"Totally Circle","Currencies":[
		{"Code":"SCAM","Issuer":"`+attacker+`","Image":"https://attacker.example/scam.png"},
		{"Code":"USDC","Issuer":"`+circle+`","Image":"https://attacker.example/usdc.png"}
	]}`)

	// Shapes that must yield nothing AND must not error the whole scan.
	// A single 22023 here blanks the logo map for every issuer.
	setPayload(junk, `{"Currencies":[1,"two",null,true,
		{"Code":"NOIMG","Issuer":"`+junk+`"},
		{"Code":"EMPTY","Issuer":"`+junk+`","Image":""},
		{"Code":"NULLIMG","Issuer":"`+junk+`","Image":null},
		{"Code":"OBJIMG","Issuer":"`+junk+`","Image":{"nested":1}},
		{"Issuer":"`+junk+`","Image":"https://junk.example/nocode.png"},
		{"Code":"NOISS","Image":"https://junk.example/noissuer.png"},
		{"Code":"LOWER","issuer":"`+junk+`","image":"https://junk.example/lower.png"}
	]}`)

	for name, payload := range map[string]string{
		"no Currencies key":   `{"OrgName":"x"}`,
		"Currencies null":     `{"Currencies":null}`,
		"Currencies object":   `{"Currencies":{"Code":"X","Image":"https://e/x.png"}}`,
		"Currencies string":   `{"Currencies":"nope"}`,
		"Currencies empty":    `{"Currencies":[]}`,
		"payload is array":    `[1,2,3]`,
		"payload is scalar":   `"just a string"`,
		"payload is a numbr":  `42`,
		"payload is null-ish": `{"Currencies":[{"Code":null,"Issuer":null,"Image":null}]}`,
	} {
		setPayload(noToml, payload)
		got, gerr := store.AllSep1Images(ctx)
		if gerr != nil {
			t.Fatalf("%s: AllSep1Images errored — one issuer's payload blanked the whole map: %v", name, gerr)
		}
		// The two healthy issuers must be unaffected by the junk beside them.
		if len(got) != 2 {
			t.Errorf("%s: got %d images %+v, want 2 (Circle's USDC + the attacker's own SCAM)", name, len(got), got)
		}
	}

	// Back to a payload of its own, then assert the whole result exactly.
	setPayload(noToml, `{"Currencies":[]}`)
	got, err := store.AllSep1Images(ctx)
	if err != nil {
		t.Fatalf("AllSep1Images: %v", err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].Code < got[j].Code })

	want := []timescale.Sep1Image{
		{Code: "SCAM", Issuer: attacker, Image: "https://attacker.example/scam.png"},
		{Code: "USDC", Issuer: circle, Image: "https://circle.com/usdc.svg"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d images %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("image %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, img := range got {
		if img.Code == "USDC" && img.Issuer == circle && img.Image != "https://circle.com/usdc.svg" {
			t.Errorf("USDC's logo was served from a hostile TOML: %q — brand hijack", img.Image)
		}
	}
}
