// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package rwa

// The curated arm admits and prices an address on a named third-party curator's word
// alone, so differences from external dashboards built on the same tables become rows.
// Its rows are served in their own array and total, never merged into the verified surface.
const (
	// BasisThirdPartyCurated — the row is on the page because a named
	// third-party curator lists it as a real-world asset. Not an
	// attestation, not a declaration, not an oracle.
	BasisThirdPartyCurated = "third_party_curated"
	// RecognitionThirdPartyCurator — the address was named by the
	// curator and by nobody this index treats as independent of it.
	RecognitionThirdPartyCurator = "third_party_curator"
)
