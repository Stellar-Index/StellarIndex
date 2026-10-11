package v1_test

// The PERSISTED half of auth-flag provenance on /v1/issuers/{g_strkey}.
//
// issuers_provenance_test.go pins what the read path concludes from a live AccountEntry; these
// pin what it does with what the drain already wrote, carried by handleIssuer from IssuerRow.

// A real r1 residue issuer: merged away at ledger 54,564,588, its pre-image
// still declaring `stellarbrunch.com`.
const mergedIssuerG = "GA2PQOJ26IP24ECRXEZ4BE6BEIB4HNDWSA2E6JVPFIP6KO6BKOEAZ6XW"

func u32(v uint32) *uint32 { return &v }

func boolPtr(v bool) *bool { return &v }
