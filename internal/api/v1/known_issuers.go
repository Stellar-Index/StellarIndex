package v1

import "context"

// knownIssuers is a hand-curated fallback map from issuer G-strkey to (home_domain, org_name),
// applied at the wire boundary while `issuers.home_domain` is empty so top issuers render with
// readable names. Each entry is sourced from the issuer's public stellar.toml.
//
// Never add an entry without verifying the G-strkey controls the home_domain (for example via
// the stellar.toml ACCOUNTS array). A wrong mapping is worse than a null.
type knownIssuer struct {
	HomeDomain string
	OrgName    string
}

var knownIssuers = map[string]knownIssuer{
	// Circle — USDC. On-chain home_domain is circle.com, whose
	// stellar.toml lists this account.
	"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN": {
		HomeDomain: "circle.com",
		OrgName:    "Circle",
	},
	// Ultra Capital — yXLM, yUSDC.
	"GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55": {
		HomeDomain: "ultracapital.xyz",
		OrgName:    "Ultra Capital",
	},
	// Aquarius — AQUA governance token.
	"GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA": {
		HomeDomain: "aqua.network",
		OrgName:    "Aquarius",
	},
	// Stronghold — SHX.
	"GDSTRSHXHGJ7ZIVRBXEYE5Q74XUVCUSEKEBR7UCHEUUEK72N7I7KJ6JH": {
		HomeDomain: "stronghold.co",
		OrgName:    "Stronghold",
	},
	// MoneyGram — international remittance USDC.
	"GASD3HGFYGNNHTJVUZAYFRNPHIZHTBSCCN4TQYTQR3MOIIH4KOLLOWMD": {
		HomeDomain: "stellar.moneygram.com",
		OrgName:    "MoneyGram International",
	},
	// AnchorUSD. On-chain home_domain is the stablecoin subdomain.
	"GDUKMGUGDZQK6YHYA5Z6AY2G4XDSZPSZ3SW5UN3ARVMO6QSRDWP5YLEX": {
		HomeDomain: "stablecoin.anchorusd.com",
		OrgName:    "AnchorUSD",
	},
	// Issuers identified via their SAC wrappers — every entry verified by
	// cross-referencing the G-strkey against the issuer's stellar.toml
	// ACCOUNTS list.
	// Blend Capital — BLND governance token.
	"GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY": {
		HomeDomain: "blend.capital",
		OrgName:    "Blend Capital",
	},
	// Velo Labs — VELO.
	"GDM4RQUQQUVSKQA7S6EM7XBZP3FCGH4Q7CL6TABQ7B2BEJ5ERARM2M5M": {
		HomeDomain: "velo.org",
		OrgName:    "Velo Labs",
	},
	// Phoenix DEX — PHO governance token. On-chain home_domain is
	// the app subdomain.
	"GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO": {
		HomeDomain: "app.phoenix-hub.io",
		OrgName:    "Phoenix",
	},
	// FxDAO — USDx issuer, not Mykobo: the on-chain home_domain is
	// assets.fxdao.io and that domain's stellar.toml ACCOUNTS list
	// carries this G-strkey.
	"GAVH5ZWACAY2PHPUG4FL3LHHJIYIHOFPSIUGM2KHK25CJWXHAV6QKDMN": {
		HomeDomain: "assets.fxdao.io",
		OrgName:    "FxDAO",
	},
	// Ultra Capital (Ultra Stellar) acquired apay.io's wrapped BTC/ETH issuers. Their on-chain
	// home_domain is ultracapital.xyz; apay.io is a dead anchor and the wrong SEP-1 domain.
	"GDPJALI4AZKUU2W426U5WKMAT6CN3AJRPIIRYR2YM54TL2GDWO5O2MZM": {
		HomeDomain: "ultracapital.xyz",
		OrgName:    "Ultra Capital",
	},
	"GBFXOHVAS43OIWNIO7XLRJAHT3BICFEIKOJLZVXNT572MISM4CMGSOCC": {
		HomeDomain: "ultracapital.xyz",
		OrgName:    "Ultra Capital",
	},
	// LIBRE — Libre Capital.
	"GAYCCWKECNGDRHYU3UTREBD2XLC3CUQN6FV22TKM4WCQER3IWR7TF5CY": {
		HomeDomain: "libre.cx",
		OrgName:    "Libre",
	},
	// Circle EUR-pegged stablecoin (EURC). On-chain home_domain is
	// circle.com, as for the USDC issuer above.
	"GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2": {
		HomeDomain: "circle.com",
		OrgName:    "Circle (EURC)",
	},
	// Legitimate issuers found via a wider stellar.expert directory
	// sweep. Each verified — directory has a `name` and either no
	// `tags` or only neutral tags.
	"GDGTVWSM4MGS4T7Z6W4RPWOCHE2I6RDFCIFZGS3DOA63LWQTRNZNTTFF": {
		HomeDomain: "ultracapital.xyz",
		OrgName:    "UltraCapital (yUSDC)",
	},
	"GBXRPL45NPHCVMFFAYZVUVFFVKSIZ362ZXFP7I2ETNQ3QKZMFLPRDTD5": {
		HomeDomain: "fchain.io",
		OrgName:    "Firefly",
	},
	"GAKTLPC4ZV37SSCITQ5IS5AQ4WPF4CF4VZJQPPAROSGXMYOATF5U6XPR": {
		HomeDomain: "zeam.money",
		OrgName:    "Zeam.Money",
	},
	"GAROH4EV3WVVTRQKEY43GZK3XSRBEYETRVZ7SVG5LHWOAANSMCTJBB3U": {
		HomeDomain: "zeam.money",
		OrgName:    "Zeam.Money",
	},
	// On-chain home_domain is the sslx subdomain.
	"GBHFGY3ZNEJWLNO4LBUKLYOCEK4V7ENEBJGPRHHX7JU47GWHBREH37UR": {
		HomeDomain: "sslx.sl8.online",
		OrgName:    "sl8.online",
	},
	"GC6OYQJIZF3HFXCYPFCBXYXNGIBQ4TNSFUBUXQJOZWIP6F3YZK4QH3VQ": {
		HomeDomain: "scopuly.com",
		OrgName:    "Scopuly",
	},
	"GAB7STHVD5BDH3EEYXPI3OM7PCS4V443PYB5FNT6CFGJVPDLMKDM24WK": {
		HomeDomain: "lumenswap.io",
		OrgName:    "Lumenswap",
	},
	"GC4HS4CQCZULIOTGLLPGRAAMSBDLFRR6Y7HCUQG66LNQDISXKIXXADIM": {
		HomeDomain: "ixinium.io",
		OrgName:    "Ixinium",
	},
	"GBCB4WO6J4ET55RWK2SVX76LUQ4PQ7TCDHG2YFILQML7D6XR3HACLXAU": {
		HomeDomain: "xau.cl",
		OrgName:    "XAU CL",
	},
	"GAORYJ3KBDGIM7FFSKVUJHJ5NEFWIRDIAGGBJBJS7TY6ECZS53257IG4": {
		HomeDomain: "dogstarcoin.com",
		OrgName:    "Dogstarcoin",
	},
	"GA6HCMBLTZS5VYYBCATRBRZ3BZJMAFUDKYYF6AH6MVCMGWMRDNSWJPIH": {
		HomeDomain: "mobius.network",
		OrgName:    "Mobius",
	},
	"GDNUVPUOMWOF2ML5FA5E4HQDX7EHV3VCJTLLTO563PUMZKMHJUJIJSYI": {
		HomeDomain: "afreum.com",
		OrgName:    "Afreum",
	},
	"GALLBRBQHAPW5FOVXXHYWR6J4ZDAQ35BMSNADYGBW25VOUHUYRZM4XIL": {
		HomeDomain: "allbridge.io",
		OrgName:    "Allbridge",
	},
}

// onChainHomeDomain returns the issuer account's live self-declared home_domain from the CH
// account state, and whether an attempted read was untrustworthy (errored, deadline included).
// No explorer reader wired never attempts a read, so degraded=false and callers fall through to
// the curated map. "" with degraded=false is a verified absence. On-chain is consulted ahead of
// knownIssuers because the field is signed by the account's keys and follows rebrands, while the
// map goes stale silently and SEP-1 verification would resolve against a stale domain.
func (s *Server) onChainHomeDomain(ctx context.Context, issuer string) (domain string, degraded bool) {
	if s.Explorer == nil {
		return "", false
	}
	st, _, err := s.Explorer.AccountStateCached(ctx, issuer)
	if err != nil {
		return "", true
	}
	if !st.Exists {
		return "", false
	}
	return st.HomeDomain, false
}

// backfillHomeDomain fills an empty detail.HomeDomain for a classic asset: on-chain account
// state first, then s.StaticHomeDomain, then the curated knownIssuers map. Both asset-detail
// surfaces share it so their SEP-1 overlays verify against the same domain.
//
// degraded=true means the on-chain read itself failed (unwired, deadline, store error). The
// curated map is then deliberately NOT consulted: it would serve a stale domain, and let SEP-1
// verify against it, under the same 200 as a live read. Callers fold degraded into the stale flag.
func (s *Server) backfillHomeDomain(ctx context.Context, detail *AssetDetail) (degraded bool) {
	if detail.HomeDomain != nil && *detail.HomeDomain != "" {
		return false
	}
	if detail.Issuer == nil || *detail.Issuer == "" {
		return false
	}
	hd, degraded := s.onChainHomeDomain(ctx, *detail.Issuer)
	if degraded {
		return true
	}
	if hd == "" && s.StaticHomeDomain != nil {
		hd, _ = s.StaticHomeDomain(ctx, *detail.Issuer)
	}
	if hd == "" {
		hd, _ = enrichIssuer(*detail.Issuer, "", "")
	}
	if hd != "" {
		detail.HomeDomain = &hd
	}
	return false
}

// enrichIssuer fills empty home_domain / org_name with the curated fallback. Values the DB
// already populated win, since a sep1-refresh cron is more current than the static map.
func enrichIssuer(gStrkey, homeDomain, orgName string) (string, string) {
	if homeDomain != "" && orgName != "" {
		return homeDomain, orgName
	}
	known, ok := knownIssuers[gStrkey]
	if !ok {
		return homeDomain, orgName
	}
	if homeDomain == "" {
		homeDomain = known.HomeDomain
	}
	if orgName == "" {
		orgName = known.OrgName
	}
	return homeDomain, orgName
}
