// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package archivecompleteness

import (
	"os/user"
	"strconv"
	"testing"
)

// TestNewCrossAnchorFiller_NetworkGuard pins that, since DefaultCrossAnchorSources are all PUBNET archives, a filler
// for a non-pubnet archive with no explicit sources is refused; otherwise `archive-completeness fill` would write
// pubnet checkpoints into a test-net root that galexie/indexer ingest as test-net data.
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

// TestNewCrossAnchorFiller_HalfBlankOwnerDoesNotDefaultToRoot pins that setting only one of OwnerUser/OwnerGroup
// leaves the other untouched (os.Chown's -1 sentinel) rather than Go's int zero value, uid/gid 0 (root).
func TestNewCrossAnchorFiller_HalfBlankOwnerDoesNotDefaultToRoot(t *testing.T) {
	dir := t.TempDir()

	// Look up the current process's own group so the lookup itself
	// succeeds without requiring root or a fixed test-fixture group.
	self, err := user.Current()
	if err != nil {
		t.Fatalf("user.Current() = %v", err)
	}
	group, err := user.LookupGroupId(self.Gid)
	if err != nil {
		t.Fatalf("user.LookupGroupId(%q) = %v", self.Gid, err)
	}

	f, err := NewCrossAnchorFiller(FillerOptions{
		ArchiveRoot: dir,
		OwnerGroup:  group.Name,
		// OwnerUser deliberately left blank.
	})
	if err != nil {
		t.Fatalf("NewCrossAnchorFiller(OwnerGroup only) = %v; want nil", err)
	}
	if f.ownerUID != -1 {
		t.Errorf("ownerUID = %d; want -1 (unset half must not default to root/uid 0)", f.ownerUID)
	}
	wantGID, err := strconv.Atoi(group.Gid)
	if err != nil {
		t.Fatalf("strconv.Atoi(%q) = %v", group.Gid, err)
	}
	if f.ownerGID != wantGID {
		t.Errorf("ownerGID = %d; want %d", f.ownerGID, wantGID)
	}
}
