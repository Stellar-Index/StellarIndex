package currency

import (
	"strings"
	"testing"
)

func TestLoadEmbedded(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	if cat == nil {
		t.Fatal("LoadEmbedded returned nil catalogue")
	}
	if got := len(cat.All()); got < 10 {
		t.Errorf("seed catalogue has %d entries; want at least 10", got)
	}

	// Sanity-check a few well-known entries.
	cases := []struct {
		slug   string
		ticker string
		// hasStellar: must have a Stellar network entry with a code (not native-only).
		hasStellarClassic bool
	}{
		{"usdc", "USDC", true},
		{"xlm", "XLM", false},
		{"btc", "BTC", false},
		{"aqua", "AQUA", true},
	}
	for _, tc := range cases {
		t.Run(tc.slug, func(t *testing.T) {
			v, ok := cat.LookupBySlug(tc.slug)
			if !ok {
				t.Fatalf("LookupBySlug(%q): not found", tc.slug)
			}
			if v.Ticker != tc.ticker {
				t.Errorf("ticker = %q, want %q", v.Ticker, tc.ticker)
			}
			if tc.hasStellarClassic {
				se := v.StellarEntry()
				if se == nil {
					t.Fatalf("StellarEntry: nil for %s", tc.slug)
				}
				if se.Code == "" || se.Issuer == "" || se.AssetID == "" {
					t.Errorf("Stellar entry incomplete: code=%q issuer=%q asset_id=%q",
						se.Code, se.Issuer, se.AssetID)
				}
				// asset_id should be CODE-ISSUER format.
				want := se.Code + "-" + se.Issuer
				if se.AssetID != want {
					t.Errorf("asset_id %q != %q", se.AssetID, want)
				}
			}
		})
	}
}

func TestLookupByTicker_caseInsensitive(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	v1, ok1 := cat.LookupByTicker("USDC")
	v2, ok2 := cat.LookupByTicker("usdc")
	v3, ok3 := cat.LookupByTicker("UsDc")
	if !(ok1 && ok2 && ok3) {
		t.Fatalf("LookupByTicker: case sensitivity leaked (ok1=%v ok2=%v ok3=%v)", ok1, ok2, ok3)
	}
	if v1 != v2 || v2 != v3 {
		t.Error("LookupByTicker returned different pointers for case-variants — index is inconsistent")
	}
}

func TestLookupByStellarAssetID(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}

	const usdcID = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	v, ok := cat.LookupByStellarAssetID(usdcID)
	if !ok {
		t.Fatalf("LookupByStellarAssetID(%s): not found", usdcID)
	}
	if v.Slug != "usdc" {
		t.Errorf("slug = %q, want usdc", v.Slug)
	}

	// Native XLM is indexed as the literal "native" asset_id.
	v, ok = cat.LookupByStellarAssetID("native")
	if !ok {
		t.Fatal("LookupByStellarAssetID(native): not found")
	}
	if v.Ticker != "XLM" {
		t.Errorf("native ticker = %q, want XLM", v.Ticker)
	}

	// Unverified asset returns false.
	_, ok = cat.LookupByStellarAssetID("USDC-GBADISSUERSOMETHINGTHATWILLNEVERMATCHANYREALACCOUNTAB")
	if ok {
		t.Error("LookupByStellarAssetID returned a hit for an unverified asset")
	}
}

func TestStellarCollision(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}

	const realUSDCIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	const fakeIssuer = "GBADISSUERSOMETHINGTHATWILLNEVERMATCHANYREALACCOUNTAB"

	t.Run("real issuer = no collision", func(t *testing.T) {
		v, collision := cat.StellarCollision("USDC", realUSDCIssuer)
		if v == nil {
			t.Fatal("StellarCollision returned nil verified currency for real USDC")
		}
		if collision {
			t.Error("StellarCollision flagged the real USDC as a collision")
		}
	})

	t.Run("fake issuer with verified code = collision", func(t *testing.T) {
		v, collision := cat.StellarCollision("USDC", fakeIssuer)
		if v == nil {
			t.Fatal("StellarCollision returned nil for a code that IS verified")
		}
		if !collision {
			t.Error("StellarCollision missed the fake-issuer collision")
		}
		if v.Slug != "usdc" {
			t.Errorf("returned verified currency slug = %q, want usdc", v.Slug)
		}
	})

	t.Run("case-insensitive code match", func(t *testing.T) {
		_, collisionLower := cat.StellarCollision("usdc", fakeIssuer)
		_, collisionUpper := cat.StellarCollision("USDC", fakeIssuer)
		if collisionLower != collisionUpper {
			t.Errorf("case sensitivity in StellarCollision: lower=%v upper=%v", collisionLower, collisionUpper)
		}
	})

	t.Run("unknown code returns (nil, false)", func(t *testing.T) {
		v, collision := cat.StellarCollision("NOTAVERIFIEDCODE", fakeIssuer)
		if v != nil || collision {
			t.Errorf("StellarCollision for unknown code: v=%v collision=%v", v, collision)
		}
	})

	t.Run("empty inputs return (nil, false)", func(t *testing.T) {
		v, collision := cat.StellarCollision("", fakeIssuer)
		if v != nil || collision {
			t.Errorf("empty code: v=%v collision=%v", v, collision)
		}
		v, collision = cat.StellarCollision("USDC", "")
		if v != nil || collision {
			t.Errorf("empty issuer: v=%v collision=%v", v, collision)
		}
	})
}

func TestNilCatalogue(t *testing.T) {
	// Lookups on a nil catalogue should be safe — handlers may
	// invoke them when no catalogue is wired.
	var cat *Catalogue
	if v, ok := cat.LookupBySlug("usdc"); v != nil || ok {
		t.Errorf("nil catalogue LookupBySlug: v=%v ok=%v", v, ok)
	}
	if v, ok := cat.LookupByTicker("USDC"); v != nil || ok {
		t.Errorf("nil catalogue LookupByTicker: v=%v ok=%v", v, ok)
	}
	if v, ok := cat.LookupByStellarAssetID("native"); v != nil || ok {
		t.Errorf("nil catalogue LookupByStellarAssetID: v=%v ok=%v", v, ok)
	}
	if v, ok := cat.StellarCollision("USDC", "G123"); v != nil || ok {
		t.Errorf("nil catalogue StellarCollision: v=%v ok=%v", v, ok)
	}
}

func TestLoadFromBytes_validation(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string // expected substring in the error
	}{
		{
			"missing ticker",
			`verified_currencies:
  - slug: foo
    name: Foo
    networks: [{network: stellar}]
`,
			"ticker is required",
		},
		{
			"missing slug",
			`verified_currencies:
  - ticker: FOO
    name: Foo
    networks: [{network: stellar}]
`,
			"slug is required",
		},
		{
			"missing networks (non-fiat, non-reference_only)",
			`verified_currencies:
  - ticker: FOO
    slug: foo
    name: Foo
`,
			"a Stellar network entry is required",
		},
		{
			"duplicate slug",
			`verified_currencies:
  - ticker: FOO
    slug: foo
    name: Foo
    networks: [{network: stellar}]
  - ticker: BAR
    slug: foo
    name: Bar
    networks: [{network: stellar}]
`,
			"duplicate slug",
		},
		{
			"duplicate ticker",
			`verified_currencies:
  - ticker: FOO
    slug: foo
    name: Foo
    networks: [{network: stellar}]
  - ticker: FOO
    slug: bar
    name: Bar
    networks: [{network: stellar}]
`,
			"duplicate ticker",
		},
		{
			"two currencies claim same code on stellar",
			`verified_currencies:
  - ticker: USDC
    slug: usdc
    name: USD Coin
    networks:
      - network: stellar
        code: USDC
        issuer: GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN
  - ticker: USDC2
    slug: usdc2
    name: Other USDC
    networks:
      - network: stellar
        code: USDC
        issuer: GBADISSUERSOMETHINGTHATWILLNEVERMATCHANYREALACCOUNTAB
`,
			"claimed by both",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadFromBytes([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("LoadFromBytes: nil err, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want substring %q", err, tc.want)
			}
		})
	}
}

func TestCoinGeckoIDs(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	ids := cat.CoinGeckoIDs()
	if len(ids) == 0 {
		t.Fatal("CoinGeckoIDs: empty — seed entries are missing coingecko_id")
	}
	// Spot-check well-known entries that must have a CG slug.
	cases := map[string]string{
		"XLM":  "stellar",
		"USDC": "usd-coin",
		"BTC":  "bitcoin",
		"ETH":  "ethereum",
	}
	for ticker, want := range cases {
		got, ok := ids[ticker]
		if !ok {
			t.Errorf("CoinGeckoIDs: %s not present", ticker)
			continue
		}
		if got != want {
			t.Errorf("CoinGeckoIDs[%s] = %q, want %q", ticker, got, want)
		}
	}
	// Stellar-native tokens we don't expect CG to track shouldn't
	// appear (no `coingecko_id` in the seed).
	for _, ticker := range []string{"BLND", "PHO", "yUSDC"} {
		if _, ok := ids[ticker]; ok {
			t.Errorf("CoinGeckoIDs unexpectedly includes %s; the seed entry shouldn't have coingecko_id set", ticker)
		}
	}
}

func TestCoinMarketCapIDs(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	ids := cat.CoinMarketCapIDs()
	if len(ids) == 0 {
		t.Fatal("CoinMarketCapIDs: empty")
	}
	if got := ids["XLM"]; got != "512" {
		t.Errorf("CoinMarketCapIDs[XLM] = %q, want 512", got)
	}
	if got := ids["BTC"]; got != "1" {
		t.Errorf("CoinMarketCapIDs[BTC] = %q, want 1", got)
	}
}

// TestBrowseable_ExcludesReferenceOnly_DivergenceUnaffected: reference_only coins (BTC/ETH/...) are excluded from the browseable
// catalogue so /v1/assets stays Stellar-only, but must stay in CoinGeckoIDs()/CoinMarketCapIDs() for the divergence/aggregator
// reference-price pipeline.
func TestBrowseable_ExcludesReferenceOnly_DivergenceUnaffected(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}

	browseable := make(map[string]bool)
	for _, vc := range cat.Browseable() {
		browseable[vc.Ticker] = true
	}

	// Reference coins must NOT be browseable. USDT is here too: no
	// Tether-native Stellar issuer exists (SDEX USDT is third-party
	// bridged), so it's a reference price only.
	refOnly := []string{"BTC", "ETH", "SOL", "BNB", "XRP", "ADA", "DOGE", "AVAX", "POL", "DOT", "LINK", "UNI", "AAVE", "WBTC", "USDT"}
	for _, tk := range refOnly {
		if browseable[tk] {
			t.Errorf("%s is reference_only but appears in Browseable()", tk)
		}
	}

	// Stellar assets + fiats must REMAIN browseable. PYUSD is here:
	// it has an official Paxos Stellar issuer (added to the catalogue).
	for _, tk := range []string{"XLM", "USDC", "EURC", "PYUSD", "AQUA", "USD", "EUR"} {
		if !browseable[tk] {
			t.Errorf("%s should be browseable but is missing from Browseable()", tk)
		}
	}

	// Divergence invariant: every reference coin keeps its CG + CMC id.
	cg := cat.CoinGeckoIDs()
	cmc := cat.CoinMarketCapIDs()
	for _, tk := range refOnly {
		if cg[tk] == "" {
			t.Errorf("CoinGeckoIDs missing %s — divergence reference pair lost", tk)
		}
		if cmc[tk] == "" {
			t.Errorf("CoinMarketCapIDs missing %s — divergence reference pair lost", tk)
		}
	}

	// Browseable must be a strict subset of All().
	if len(cat.Browseable()) >= len(cat.All()) {
		t.Errorf("Browseable()=%d should be < All()=%d (reference coins removed)",
			len(cat.Browseable()), len(cat.All()))
	}
}

func TestTickers(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	tickers := cat.Tickers()
	if len(tickers) != len(cat.All()) {
		t.Errorf("Tickers count %d != catalogue size %d", len(tickers), len(cat.All()))
	}
	for _, ticker := range tickers {
		if ticker == "" {
			t.Error("empty ticker in Tickers slice")
		}
		if ticker != strings.ToUpper(ticker) {
			t.Errorf("ticker %q not upper-cased", ticker)
		}
	}
}

func TestSeedFiatEntries_HaveExpectedShape(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	fiat := cat.ByClass(ClassFiat)
	if len(fiat) < 10 {
		t.Fatalf("seed has %d fiat entries; want at least 10", len(fiat))
	}
	wantPresent := map[string]bool{
		"us-dollar":     false,
		"euro":          false,
		"chinese-yuan":  false,
		"japanese-yen":  false,
		"british-pound": false,
		"indian-rupee":  false,
	}
	for _, vc := range fiat {
		if vc.Class != ClassFiat {
			t.Errorf("ByClass returned non-fiat entry: %+v", vc)
		}
		if vc.CirculatingSupply == "" {
			t.Errorf("fiat entry %q missing circulating_supply", vc.Slug)
		}
		if len(vc.Issuance) != 0 {
			t.Errorf("fiat entry %q has networks (%d); fiat is network-agnostic",
				vc.Slug, len(vc.Issuance))
		}
		if _, ok := wantPresent[vc.Slug]; ok {
			wantPresent[vc.Slug] = true
		}
	}
	for slug, present := range wantPresent {
		if !present {
			t.Errorf("required fiat slug %q missing from seed", slug)
		}
	}
}

func TestSeedClassDefaults(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	usdc, _ := cat.LookupBySlug("usdc")
	if usdc.Class != ClassStablecoin {
		t.Errorf("usdc.Class = %q, want stablecoin", usdc.Class)
	}
	xlm, _ := cat.LookupBySlug("xlm")
	if xlm.Class != ClassCrypto {
		t.Errorf("xlm.Class = %q, want crypto (default)", xlm.Class)
	}
	cny, ok := cat.LookupBySlug("chinese-yuan")
	if !ok || cny.Class != ClassFiat {
		t.Errorf("chinese-yuan.Class = %q, want fiat", cny.Class)
	}
}

// TestReferenceOnly_AllowsZeroNetworks: a reference_only entry (BTC/ETH/…) may carry zero network
// entries, since it exists only for the divergence/aggregator price cross-check. A non-fiat,
// non-reference_only entry still MUST carry a network entry.
func TestReferenceOnly_AllowsZeroNetworks(t *testing.T) {
	y := `verified_currencies:
  - ticker: REF
    slug: ref
    name: Reference Coin
    class: crypto
    coingecko_id: ref-coin
    reference_only: true
`
	cat, err := LoadFromBytes([]byte(y))
	if err != nil {
		t.Fatalf("reference_only entry with no networks should load: %v", err)
	}
	vc, ok := cat.LookupBySlug("ref")
	if !ok {
		t.Fatal("ref entry not loaded")
	}
	if len(vc.Issuance) != 0 {
		t.Errorf("reference_only entry has %d networks, want 0", len(vc.Issuance))
	}
	// Still feeds the reference-price pipeline.
	if cat.CoinGeckoIDs()["REF"] != "ref-coin" {
		t.Error("reference_only entry dropped from CoinGeckoIDs")
	}
}

// TestSeedBrowseable_StellarOnlyNetworks pins that no seed entry
// carries a non-Stellar network entry post Stellar-focus refactor.
func TestSeedBrowseable_StellarOnlyNetworks(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	for _, vc := range cat.All() {
		if len(vc.Issuance) > 1 {
			t.Errorf("%s has %d network entries; expected at most 1 (stellar)", vc.Ticker, len(vc.Issuance))
		}
		for _, n := range vc.Issuance {
			if n.Network != "stellar" {
				t.Errorf("%s carries non-Stellar network %q", vc.Ticker, n.Network)
			}
		}
	}
}

// TestLoadFromBytes_RejectsUnknownField: a misspelled optional key (e.g. "coingecko_di") must fail the load loudly, not parse to
// the zero value and silently drop the data.
func TestLoadFromBytes_RejectsUnknownField(t *testing.T) {
	yamlSrc := `verified_currencies:
  - ticker: FOO
    slug: foo
    name: Foo
    coingecko_di: foo-coin
    networks: [{network: stellar}]
`
	_, err := LoadFromBytes([]byte(yamlSrc))
	if err == nil {
		t.Fatal("LoadFromBytes: nil err, want error rejecting unknown field coingecko_di")
	}
	if !strings.Contains(err.Error(), "coingecko_di") {
		t.Errorf("err = %q, want it to name the unknown field coingecko_di", err)
	}
}

func TestLoadFromBytes_RejectsUnknownClass(t *testing.T) {
	y := `verified_currencies:
  - ticker: FOO
    slug: foo
    name: Foo
    class: not-a-real-class
    networks: [{network: stellar}]
`
	_, err := LoadFromBytes([]byte(y))
	if err == nil || !strings.Contains(err.Error(), "unknown class") {
		t.Errorf("expected 'unknown class' error, got %v", err)
	}
}

func TestSeedDataIntegrity(t *testing.T) {
	// Sanity-check: every entry with a Stellar classic network entry
	// must have non-empty code, issuer, and asset_id, and the
	// asset_id must be code-issuer concatenation.
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	for _, vc := range cat.All() {
		for _, n := range vc.Issuance {
			if n.Network != "stellar" {
				continue
			}
			// Native XLM: asset_id == "native", code/issuer empty.
			if n.AssetID == "native" {
				if n.Code != "" || n.Issuer != "" {
					t.Errorf("%s native: expected empty code/issuer, got %q/%q",
						vc.Ticker, n.Code, n.Issuer)
				}
				continue
			}
			// Classic asset.
			if n.Code == "" || n.Issuer == "" || n.AssetID == "" {
				t.Errorf("%s stellar entry incomplete: code=%q issuer=%q asset_id=%q",
					vc.Ticker, n.Code, n.Issuer, n.AssetID)
				continue
			}
			want := n.Code + "-" + n.Issuer
			if n.AssetID != want {
				t.Errorf("%s asset_id = %q, want %q", vc.Ticker, n.AssetID, want)
			}
			// G-strkey must be 56 chars.
			if len(n.Issuer) != 56 || !strings.HasPrefix(n.Issuer, "G") {
				t.Errorf("%s issuer %q: not a valid G-strkey", vc.Ticker, n.Issuer)
			}
		}
	}
}

// TestStellarCollision_nativeAssetImpersonator: XLM's seed entry is `network: stellar` + `asset_id: native` with no classic
// `code`, yet must still enter byStellarCode; otherwise "XLM-G<attacker>" is served with no collision flag and the explorer
// badges it VERIFIED. No legitimate classic asset can carry XLM's code, so every issuer must collide.
func TestStellarCollision_nativeAssetImpersonator(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}

	const attacker = "GBEO62ZYAOEKVL4WMF5Q6VYTOJQUT7H2QYRDVFO5LT4W7VQPFDWVKUHO"
	v, collision := cat.StellarCollision("XLM", attacker)
	if v == nil {
		t.Fatal("StellarCollision(XLM, <attacker>) returned no verified currency — a classic asset bearing the native asset's ticker is unflagged, and the explorer badges it as verified")
	}
	if v.Ticker != "XLM" {
		t.Errorf("matched ticker %q, want XLM", v.Ticker)
	}
	if !collision {
		t.Error("collision = false for a classic XLM impersonator — no legitimate classic asset can carry XLM's code, so every issuer must collide")
	}

	// Case-insensitivity, same as the classic-code path.
	if _, c := cat.StellarCollision("xlm", attacker); !c {
		t.Error("lowercase \"xlm\" did not collide")
	}

	// The native asset itself is never queried through this path (it has
	// no code), and an empty code must stay a no-op.
	if v, c := cat.StellarCollision("", attacker); v != nil || c {
		t.Errorf("empty code = (%v, %v), want (nil, false)", v, c)
	}

	// A verified classic asset must still resolve to no collision for
	// its own issuer — proving the native branch didn't disturb it.
	if v, c := cat.StellarCollision("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"); v == nil || c {
		t.Errorf("real USDC = (%v, %v), want (USDC, false)", v, c)
	}
}

// TestStellarCollision_CoversEveryCatalogueTicker: impersonation must be reportable for every verified ticker naming an issued
// asset, including `reference_only` ones (USDT, BTC, ETH, XRP), so byStellarCode is not filled only in the Issuance loop.
// Sovereign currencies are carved out by class (SEP-1 anchors use the ISO code) but stay answerable via FiatDenomination.
func TestStellarCollision_CoversEveryCatalogueTicker(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	// An arbitrary issuer that cannot legitimately hold any verified
	// ticker — every hit below is therefore a true impersonation.
	const attacker = "GBEO62ZYQXBGDQEHPTMBHRJVUEBNMXAWZFPBQBLPJXLJKMQTOEVEDGRA"

	var missed, unanswerable, defamed []string
	fiat := 0
	for _, vc := range cat.All() {
		_, collision := cat.StellarCollision(vc.Ticker, attacker)
		if vc.Class == ClassFiat && vc.StellarEntry() == nil {
			fiat++
			if collision {
				defamed = append(defamed, vc.Ticker)
			}
			if _, ok := cat.FiatDenomination(vc.Ticker); !ok {
				unanswerable = append(unanswerable, vc.Ticker)
			}
			continue
		}
		if !collision {
			missed = append(missed, vc.Ticker)
		}
	}
	if len(missed) > 0 {
		t.Errorf("StellarCollision cannot speak about %d of %d verified tickers: %v\n"+
			"an attacker minting a classic asset with any of these codes is unflaggable",
			len(missed), len(cat.All()), missed)
	}
	if len(defamed) > 0 {
		t.Errorf("StellarCollision reports an impersonation for %d sovereign currency code(s): %v\n"+
			"a fiat code is a denomination with no issuer to impersonate, and SEP-1 codes an "+
			"anchor's deposit token with exactly it", len(defamed), defamed)
	}
	if len(unanswerable) > 0 {
		t.Errorf("%d fiat code(s) are in neither index — the catalogue cannot speak about them "+
			"at all: %v", len(unanswerable), unanswerable)
	}
	if fiat == 0 {
		t.Fatal("no issuance-free fiat entries in the catalogue — the carve-out proved nothing")
	}
}

// TestStellarCollision_DoesNotFlagTheGenuineIssuer guards the fix against
// over-reach: the real issuer of a verified Stellar asset must NOT be
// reported as colliding with itself.
func TestStellarCollision_DoesNotFlagTheGenuineIssuer(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	checked := 0
	for _, vc := range cat.All() {
		for _, n := range vc.Issuance {
			if n.Network != "stellar" || n.Issuer == "" || n.Code == "" {
				continue
			}
			if _, ok := cat.StellarCollision(n.Code, n.Issuer); ok {
				t.Errorf("%s: genuine issuer %s flagged as an impersonator of its own code %s",
					vc.Ticker, n.Issuer, n.Code)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no verified Stellar issuances found — the guard proved nothing")
	}
}

// TestStellarCollision_TickerOnlyVettedIssuanceRoute pins the documented way to admit a genuine Stellar issuance of a ticker-only
// entry: a `network: stellar` entry on the same catalogue entry exempts that issuer alone.
func TestStellarCollision_TickerOnlyVettedIssuanceRoute(t *testing.T) {
	const (
		anchor   = "GDUKMGUGDZQK6YHYA5Z6AY2G4XDSZPSZ3SW5UN3ARVMO6QSRDWP5YLEX"
		attacker = "GBEO62ZYQXBGDQEHPTMBHRJVUEBNMXAWZFPBQBLPJXLJKMQTOEVEDGRA"
		entry    = `verified_currencies:
  - ticker: BTC
    slug: bitcoin
    name: Bitcoin
    class: crypto
    reference_only: true
`
	)
	tickerOnly, err := LoadFromBytes([]byte(entry))
	if err != nil {
		t.Fatalf("LoadFromBytes(ticker-only): %v", err)
	}
	if _, coll := tickerOnly.StellarCollision("BTC", anchor); !coll {
		t.Error("ticker-only BTC: an unvetted issuer must be flagged")
	}

	vetted, err := LoadFromBytes([]byte(entry + `    networks:
      - network: stellar
        code: BTC
        issuer: ` + anchor + `
        asset_id: BTC-` + anchor + `
`))
	if err != nil {
		t.Fatalf("LoadFromBytes(vetted): %v", err)
	}
	if _, coll := vetted.StellarCollision("BTC", anchor); coll {
		t.Error("vetted BTC issuance: its issuer must not be flagged")
	}
	if _, coll := vetted.StellarCollision("BTC", attacker); !coll {
		t.Error("vetted BTC issuance: every other issuer must still be flagged")
	}
}

// TestLoadFromBytes_RejectsKnownAnchorsOutsideFiatDenomination: known_anchors
// is read only for an issuance-free fiat entry, so accepting it elsewhere
// would silently look like an impersonation allowlist that does nothing.
func TestLoadFromBytes_RejectsKnownAnchorsOutsideFiatDenomination(t *testing.T) {
	const anchor = "GDUKMGUGDZQK6YHYA5Z6AY2G4XDSZPSZ3SW5UN3ARVMO6QSRDWP5YLEX"
	cases := map[string]string{
		"reference_only crypto": `
  - ticker: BTC
    slug: bitcoin
    name: Bitcoin
    class: crypto
    reference_only: true
    known_anchors: ["` + anchor + `"]
`,
		"fiat with a Stellar issuance": `
  - ticker: EURX
    slug: eurx
    name: Euro token
    class: fiat
    networks:
      - network: stellar
        code: EURX
        issuer: ` + anchor + `
    known_anchors: ["` + anchor + `"]
`,
	}
	for name, body := range cases {
		if _, err := LoadFromBytes([]byte("verified_currencies:" + body)); err == nil {
			t.Errorf("%s: known_anchors accepted where nothing reads it", name)
		}
	}
	if _, err := LoadFromBytes([]byte(`verified_currencies:
  - ticker: USD
    slug: us-dollar
    name: US Dollar
    class: fiat
    networks: []
    known_anchors: ["` + anchor + `"]
`)); err != nil {
		t.Errorf("issuance-free fiat entry with known_anchors must load: %v", err)
	}
}

// TestFiatDenomination_ExcludesFiatWithSorobanOnlyIssuance: a fiat entry whose only Stellar issuance is a Soroban contract has a
// Stellar identity, so StellarCollision owns its ticker and FiatDenomination must not answer for it, matching the census test's
// carve-out.
func TestFiatDenomination_ExcludesFiatWithSorobanOnlyIssuance(t *testing.T) {
	const contract = "CBSJZEIO5C7KC2SF3MKSNXXJSW5G3VTNBX4ATMKUI3B2MR4JKM4R26YF"
	cat, err := LoadFromBytes([]byte(`verified_currencies:
  - ticker: EURX
    slug: eurx
    name: Euro on Soroban
    class: fiat
    networks:
      - network: stellar
        asset_id: ` + contract + `
        contract: ` + contract + `
`))
	if err != nil {
		t.Fatalf("LoadFromBytes: %v", err)
	}
	vc, _ := cat.LookupByTicker("EURX")
	if vc == nil || vc.StellarEntry() == nil {
		t.Fatal("fixture must load as a fiat entry with a Stellar issuance")
	}
	if v, ok := cat.FiatDenomination("EURX"); ok {
		t.Errorf("FiatDenomination(EURX) = %q; a fiat entry with a Stellar issuance is a Stellar identity, not a denomination", v.Ticker)
	}
	const attacker = "GBEO62ZYQXBGDQEHPTMBHRJVUEBNMXAWZFPBQBLPJXLJKMQTOEVEDGRA"
	if v, coll := cat.StellarCollision("EURX", attacker); v != vc || !coll {
		t.Errorf("StellarCollision(EURX, attacker) = (%v, %v), want (EURX, true): a classic EURX from any issuer impersonates the verified Soroban issuance", v, coll)
	}
}

func hasTicker(entries []*VerifiedCurrency, ticker string) bool {
	for _, vc := range entries {
		if vc.Ticker == ticker {
			return true
		}
	}
	return false
}

// TestCatalogue_StellarExternalSplit pins the split predicate: StellarIssued()
// (→ /v1/assets) and External() (→ /v1/external/assets) partition the catalogue
// by "has a Stellar on-chain issuance". Fiat + reference-only coins are external.
func TestCatalogue_StellarExternalSplit(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	stellar := cat.StellarIssued()
	external := cat.External()

	// Exact partition of the whole catalogue.
	if got := len(stellar) + len(external); got != len(cat.All()) {
		t.Fatalf("not a partition: %d stellar + %d external = %d, want %d",
			len(stellar), len(external), got, len(cat.All()))
	}
	for _, vc := range stellar {
		if vc.StellarEntry() == nil {
			t.Errorf("StellarIssued includes %q which has no Stellar entry", vc.Ticker)
		}
	}
	for _, vc := range external {
		if vc.StellarEntry() != nil {
			t.Errorf("External includes %q which HAS a Stellar entry", vc.Ticker)
		}
	}

	// Reference-only coins are external, Stellar stablecoins are not.
	if !hasTicker(external, "BTC") {
		t.Error("BTC (reference-only) must be in External()")
	}
	if hasTicker(stellar, "BTC") {
		t.Error("BTC must NOT be in StellarIssued()")
	}
	if !hasTicker(stellar, "USDC") {
		t.Error("USDC (Stellar stablecoin) must be in StellarIssued()")
	}
	if hasTicker(external, "USDC") {
		t.Error("USDC must NOT be in External()")
	}
}

// TestFiatCodedAnchorIsNotAnImpersonator: a catalogue fiat ticker (USD, EUR, ...) is a denomination nobody issues, and SEP-1
// anchors code dollar deposits as `USD`, so that is not impersonation. Fiat entries with no Stellar issuance go to byFiatCode
// (answerable via FiatDenomination); one gaining a verified Stellar issuance collides normally.
func TestFiatCodedAnchorIsNotAnImpersonator(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}

	// A real SEP-24 fiat anchor's issuer. Any G-account works for the
	// mechanism; the point is that it is NOT claiming to be the catalogue's
	// USD entry, because that entry has no issuer to claim to be.
	const anchorAccount = "GDUKMGUGDZQK6YHYA5Z6AY2G4XDSZPSZ3SW5UN3ARVMO6QSRDWP5YLEX"

	for _, ticker := range []string{"USD", "EUR", "GBP", "JPY"} {
		t.Run(ticker, func(t *testing.T) {
			entry, ok := cat.LookupByTicker(ticker)
			if !ok {
				t.Fatalf("catalogue has no %s entry; this check is out of date", ticker)
			}
			if entry.Class != ClassFiat {
				t.Fatalf("%s is class %q, want fiat; this check is out of date", ticker, entry.Class)
			}
			if entry.StellarEntry() != nil {
				t.Skipf("%s now has a verified Stellar issuance — the collision report is correct for it", ticker)
			}
			if _, collision := cat.StellarCollision(ticker, anchorAccount); collision {
				t.Errorf("StellarCollision(%q, anchor) reports an impersonation. A fiat ticker is a "+
					"denomination with no issuer, and SEP-1 codes a fiat anchor's deposit token with "+
					"exactly this code — so the anchor loses its market cap and is warned about for "+
					"following the spec", ticker)
			}
			// Silent is not the same as forgotten: the catalogue must
			// still be able to say what the code denominates, or a
			// future entry could drop out of both indexes unnoticed.
			denom, ok := cat.FiatDenomination(ticker)
			if !ok {
				t.Fatalf("FiatDenomination(%q) found nothing — the code is in neither index", ticker)
			}
			if denom != entry {
				t.Errorf("FiatDenomination(%q) = %q, want the catalogue's own %q entry",
					ticker, denom.Ticker, entry.Ticker)
			}
		})
	}

	// The control: the carve-out is by class, so reference-only tickers (USDT, XRP, BTC) must still report an impersonator, else the
	// whole check would be disarmed.
	for _, ticker := range []string{"USDT", "XRP", "BTC"} {
		t.Run("control/"+ticker, func(t *testing.T) {
			if _, ok := cat.LookupByTicker(ticker); !ok {
				t.Skipf("catalogue no longer holds %s", ticker)
			}
			if _, collision := cat.StellarCollision(ticker, anchorAccount); !collision {
				t.Errorf("StellarCollision(%q, third party) reports no impersonation; %s names a "+
					"token issued elsewhere, so every classic %s-G… is one", ticker, ticker, ticker)
			}
			if _, fiat := cat.FiatDenomination(ticker); fiat {
				t.Errorf("%s resolved as a fiat denomination; it is an issued asset", ticker)
			}
		})
	}
}

// TestLoadEmbeddedForNetwork pins the test-net scoping: Circle's testnet issuance answers usdc, native XLM is kept, an
// entry with no issuance on that net is dropped, and pubnet is the unscoped catalogue.
func TestLoadEmbeddedForNetwork(t *testing.T) {
	const circleTestnetUSDC = "USDC-GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5"
	test, err := LoadEmbeddedForNetwork("testnet")
	if err != nil {
		t.Fatalf("testnet: %v", err)
	}
	if vc, ok := test.LookupBySlug("usdc"); !ok || vc.StellarEntry().AssetID != circleTestnetUSDC {
		t.Errorf("testnet usdc = %+v, want %s", vc, circleTestnetUSDC)
	}
	if vc, ok := test.LookupByStellarAssetID(circleTestnetUSDC); !ok || vc.Slug != "usdc" {
		t.Errorf("testnet asset-id lookup = %+v, want usdc", vc)
	}
	if vc, ok := test.LookupBySlug("xlm"); !ok || vc.StellarEntry().AssetID != "native" {
		t.Errorf("testnet xlm = %+v, want native", vc)
	}
	if _, ok := test.LookupBySlug("aqua"); ok {
		t.Error("testnet kept aqua, which has no testnet issuance")
	}
	if _, ok := test.LookupBySlug("us-dollar"); !ok {
		t.Error("testnet dropped the us-dollar fiat entry")
	}

	future, err := LoadEmbeddedForNetwork("futurenet")
	if err != nil {
		t.Fatalf("futurenet: %v", err)
	}
	if _, ok := future.LookupBySlug("usdc"); ok {
		t.Error("futurenet kept usdc, which has no futurenet issuance")
	}

	pub, err := LoadEmbeddedForNetwork("pubnet")
	if err != nil {
		t.Fatalf("pubnet: %v", err)
	}
	if vc, ok := pub.LookupBySlug("usdc"); !ok || vc.StellarEntry().Issuer != "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN" {
		t.Errorf("pubnet usdc = %+v, want Circle's pubnet issuer", vc)
	}
}
