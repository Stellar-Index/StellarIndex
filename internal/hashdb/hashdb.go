// Package hashdb is a tiny on-disk record of (ledger_seq → sha256 of LCM
// bytes), a drift detector against retroactive rewrites of upstream galexie
// objects. Upstream can rewrite a fetched ledger's bytes while staying
// chain-link consistent, so Tier A + Tier D pass; only a fingerprint of what we
// first saw catches it.
//
// Format: a 16-byte header followed by dense fixed-size records.
//
//	magic       [8]byte    // "rxhashdb"
//	version     uint32     // file format version (1)
//	startLedger uint32     // ledger of record 0 (records are dense)
//	then N records of: hash [32]byte  // sha256 of the LCM XDR bytes
//
// The sequence is implicit: `seq = startLedger + index`. No gaps. An all-zero
// record means "not yet written" and Verify() rejects it as ErrMissing.
//
// Concurrency: callers must serialise calls (one writer, one reader, never both
// at once). A record write is NOT atomic everywhere: 1 record in 128 straddles a
// 4 KiB page, and a torn record reads as non-zero (permanent false drift).
package hashdb

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// File-format constants.
const (
	magic         = "rxhashdb"
	formatVersion = uint32(1)
	headerSize    = 16 // 8 magic + 4 version + 4 startLedger
	recordSize    = 32 // sha256 length
)

// Sentinel errors.
var (
	// ErrOutOfRange: a ledger below startLedger is refused, never treated as record 0.
	ErrOutOfRange = errors.New("hashdb: ledger sequence outside file range")
	// ErrMissing: no record yet (past the last Append, or an unwritten zero slot).
	ErrMissing = errors.New("hashdb: no record for ledger")
	// ErrDrift: Verify saw a hash differing from the stored one. Escalate to ops; this is the
	// condition the package exists to detect.
	ErrDrift = errors.New("hashdb: hash drift — recorded != observed")
	// ErrBadMagic: the header magic does not match (foreign or truncated file).
	ErrBadMagic = errors.New("hashdb: bad magic — not a hashdb file")
	// ErrBadVersion: the file format version is unknown to this build.
	ErrBadVersion = errors.New("hashdb: unsupported file format version")
)

// DB is an open hashdb file. Not safe for concurrent use.
type DB struct {
	f           *os.File
	startLedger uint32
}

// Create opens a fresh hashdb file at path covering ledgers [startLedger, +inf), writing only the header; it fails if the file already exists.
func Create(path string, startLedger uint32) (*DB, error) {
	// #nosec G304 — `path` is operator-supplied, not request-derived; traversal protection belongs
	// to the caller.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("hashdb: create %s: %w", path, err)
	}
	hdr := make([]byte, headerSize)
	copy(hdr[:8], magic)
	binary.BigEndian.PutUint32(hdr[8:12], formatVersion)
	binary.BigEndian.PutUint32(hdr[12:16], startLedger)
	if _, err := f.WriteAt(hdr, 0); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("hashdb: write header: %w", err)
	}
	// Sync the header before handing the DB out: a crash in the writeback window can persist a
	// 0-byte file that fails Open on the next boot. Records are deliberately not synced; the
	// header is a once-per-file cost.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("hashdb: sync header: %w", err)
	}
	return &DB{f: f, startLedger: startLedger}, nil
}

// Open opens an existing hashdb file, returning ErrBadMagic / ErrBadVersion on header mismatch.
func Open(path string) (*DB, error) {
	// #nosec G304 — operator-supplied path; see Create() comment.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("hashdb: open %s: %w", path, err)
	}
	hdr := make([]byte, headerSize)
	if _, err := io.ReadFull(f, hdr); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("hashdb: read header: %w", err)
	}
	if string(hdr[:8]) != magic {
		_ = f.Close()
		return nil, ErrBadMagic
	}
	if v := binary.BigEndian.Uint32(hdr[8:12]); v != formatVersion {
		_ = f.Close()
		return nil, fmt.Errorf("%w: file is v%d, build supports v%d", ErrBadVersion, v, formatVersion)
	}
	start := binary.BigEndian.Uint32(hdr[12:16])
	return &DB{f: f, startLedger: start}, nil
}

// Close releases the underlying file handle. Idempotent.
func (db *DB) Close() error {
	if db.f == nil {
		return nil
	}
	err := db.f.Close()
	db.f = nil
	return err
}

// StartLedger returns the ledger sequence of record 0.
func (db *DB) StartLedger() uint32 { return db.startLedger }

// Hash returns the sha256 of the supplied LCM bytes, so callers need not import crypto/sha256 for the fixed-size value.
func Hash(lcmBytes []byte) [recordSize]byte {
	return sha256.Sum256(lcmBytes)
}

// recordOffset returns the byte offset of the record for `seq`. It
// does not bounds-check against the file's length — that's the
// caller's job.
func (db *DB) recordOffset(seq uint32) (int64, error) {
	if seq < db.startLedger {
		return 0, fmt.Errorf("%w: seq=%d < startLedger=%d", ErrOutOfRange, seq, db.startLedger)
	}
	return int64(headerSize) + int64(seq-db.startLedger)*int64(recordSize), nil
}

// Append writes the hash for ledger seq; write contiguously, since a skipped slot is zero-padded and reads back as ErrMissing.
func (db *DB) Append(seq uint32, h [recordSize]byte) error {
	off, err := db.recordOffset(seq)
	if err != nil {
		return err
	}
	if _, err := db.f.WriteAt(h[:], off); err != nil {
		return fmt.Errorf("hashdb: write record seq=%d: %w", seq, err)
	}
	return nil
}

// Get returns the recorded hash for ledger seq, or ErrMissing when the slot is past EOF or all-zero.
func (db *DB) Get(seq uint32) ([recordSize]byte, error) {
	var out [recordSize]byte
	off, err := db.recordOffset(seq)
	if err != nil {
		return out, err
	}
	n, err := db.f.ReadAt(out[:], off)
	if errors.Is(err, io.EOF) || (err == nil && n < recordSize) {
		return out, fmt.Errorf("%w: seq=%d (file truncated or never appended)", ErrMissing, seq)
	}
	if err != nil {
		return out, fmt.Errorf("hashdb: read record seq=%d: %w", seq, err)
	}
	if out == ([recordSize]byte{}) {
		return out, fmt.Errorf("%w: seq=%d (slot unwritten)", ErrMissing, seq)
	}
	return out, nil
}

// Verify compares observed against the stored hash for seq: nil on match, ErrDrift on mismatch, ErrMissing when unrecorded (callers wanting record-on-first-sight fall back to Append).
func (db *DB) Verify(seq uint32, observed [recordSize]byte) error {
	stored, err := db.Get(seq)
	if err != nil {
		return err
	}
	if stored != observed {
		return fmt.Errorf("%w: seq=%d stored=%x observed=%x", ErrDrift, seq, stored[:8], observed[:8])
	}
	return nil
}
