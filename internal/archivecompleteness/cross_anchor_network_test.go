// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package archivecompleteness

import "testing"

// TestNewCrossAnchorFiller_NetworkGuard is the cross-network-corruption
// proof (audit 2026-08-26): the built-in DefaultCrossAnchorSources are all
// PUBNET archives, so constructing a filler for a non-pubnet archive with
// no explicit sources MUST be refused — otherwise `archive-completeness
// fill` on a test-net VM downloads pubnet checkpoints and writes them into
// the test-net archive root, which galexie/indexer then ingest as test-net
// data.
func TestNewCrossAnchorFiller_NetworkGuard(t *testing.T) {
	dir := t.TempDir()

	// non-pubnet + no explicit sources → refuse.
	if _, err := NewCrossAnchorFiller(FillerOptions{ArchiveRoot: dir, Network: "testnet"}); err == nil {
		t.Error("NewCrossAnchorFiller(Network=testnet, no Sources) = nil error; want refusal — pubnet default sources would corrupt a testnet archive")
	}
	if _, err := NewCrossAnchorFiller(FillerOptions{ArchiveRoot: dir, Network: "futurenet"}); err == nil {
		t.Error("NewCrossAnchorFiller(Network=futurenet, no Sources) = nil error; want refusal")
	}

	// non-pubnet + explicit sources → allowed (operator's responsibility).
	if _, err := NewCrossAnchorFiller(FillerOptions{
		ArchiveRoot: dir,
		Network:     "testnet",
		Sources:     []Source{{Name: "testnet-mirror", URL: "https://history.example.test"}},
	}); err != nil {
		t.Errorf("NewCrossAnchorFiller(Network=testnet, explicit Sources) = %v; want nil", err)
	}

	// pubnet + no sources → allowed (uses the pubnet defaults).
	if _, err := NewCrossAnchorFiller(FillerOptions{ArchiveRoot: dir, Network: "pubnet"}); err != nil {
		t.Errorf("NewCrossAnchorFiller(Network=pubnet, no Sources) = %v; want nil", err)
	}

	// empty Network + no sources → allowed (back-compat: existing pubnet callers).
	if _, err := NewCrossAnchorFiller(FillerOptions{ArchiveRoot: dir}); err != nil {
		t.Errorf("NewCrossAnchorFiller(no Network, no Sources) = %v; want nil (pubnet back-compat)", err)
	}
}
