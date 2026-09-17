// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package rwa

// The CURATED arm — the third way a row reaches /v1/rwa/assets, and the
// only one that admits on a third party's word alone.
//
// The verified arms admit on evidence this index can check: an issuer's
// own domain-bound declaration, a curated directory's identity
// attestation, an oracle feed on the instrument, or two independent
// sources naming one address. The curated arm admits because a NAMED
// external curator lists the address as a real-world asset, and prices
// it because that curator publishes a price. Nothing about it is
// checked here, and the surface says so on every row.
//
// Why it exists at all: the external figures a reader compares this
// index against are produced from exactly such a curation — the
// "RWAs on Stellar" dashboard values Stellar's balances against two
// tables its author uploads. Without reading the same tables, the only
// honest answer to "why do you differ" was a document. With them, it is
// a row: here is what the curator counts, here is what we can verify,
// and here is the line between.
//
// Rows admitted on this arm are served in their OWN array under their
// OWN total. They are never merged into `assets`, `summary`, `by_class`
// or `by_issuer`, so a consumer that reads only the verified surface is
// unaffected, and a consumer that wants the curated view has to ask for
// it by name.
const (
	// BasisThirdPartyCurated — the row is on the page because a named
	// third-party curator lists it as a real-world asset. Not an
	// attestation, not a declaration, not an oracle.
	BasisThirdPartyCurated = "third_party_curated"
	// RecognitionThirdPartyCurator — the address was named by the
	// curator and by nobody this index treats as independent of it.
	RecognitionThirdPartyCurator = "third_party_curator"
)
