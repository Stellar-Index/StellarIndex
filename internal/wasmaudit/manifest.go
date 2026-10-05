// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

// Package wasmaudit holds the audited-WASM manifest and the per-hash replay
// gate: no Soroban replay decodes a WASM version without an audit.
package wasmaudit

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
)

// auditedWasmJSON is the set of WASM hashes the audit logs in
// docs/operations/wasm-audits/ have checked against the decoders. It changes
// only by PR, alongside the audit doc it cites.
//
//go:embed audited_wasm.json
var auditedWasmJSON []byte

// Entry is one audited WASM hash. One hash can serve several sources
// (reflector-dex and reflector-cex run the same oracle WASM).
type Entry struct {
	Sources []string `json:"sources"`
	Role    string   `json:"role"`
	Audited string   `json:"audited"`
	Doc     string   `json:"doc"`
}

// Covers reports whether the entry attests the hash for source.
func (e Entry) Covers(source string) bool { return slices.Contains(e.Sources, source) }

var wasmHashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Load parses the embedded manifest.
func Load() (map[string]Entry, error) { return parse(auditedWasmJSON) }

// parse refuses a duplicate key: encoding/json keeps the last one silently,
// which would drop an attestation without a diff anyone reads as a removal.
func parse(data []byte) (map[string]Entry, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("audited_wasm.json: %w", err)
	}
	if tok != json.Delim('{') {
		return nil, fmt.Errorf("audited_wasm.json: want a top-level object, got %v", tok)
	}
	m := make(map[string]Entry)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("audited_wasm.json: %w", err)
		}
		h, _ := tok.(string)
		if !wasmHashRE.MatchString(h) {
			return nil, fmt.Errorf("audited_wasm.json: %q is not a 64-char lower-hex wasm hash", h)
		}
		if _, dup := m[h]; dup {
			return nil, fmt.Errorf("audited_wasm.json: duplicate hash %s", h)
		}
		var e Entry
		if err := dec.Decode(&e); err != nil {
			return nil, fmt.Errorf("audited_wasm.json: %s: %w", h, err)
		}
		if len(e.Sources) == 0 || slices.Contains(e.Sources, "") {
			return nil, fmt.Errorf("audited_wasm.json: %s has no source", h)
		}
		if e.Doc == "" {
			return nil, fmt.Errorf("audited_wasm.json: %s cites no audit doc", h)
		}
		m[h] = e
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("audited_wasm.json: %w", err)
	}
	return m, nil
}
