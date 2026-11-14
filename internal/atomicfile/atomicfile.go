// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// Package atomicfile holds the file opens that stay safe when a privileged process writes into a directory another user owns.
package atomicfile

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// Write streams body into a temp file beside path, sets mode, and renames it over path.
//
// The temp name is random and opened O_EXCL. Several callers run as root and write into a
// directory a service user owns; a fixed "<path>.tmp" there can be pre-planted as a symlink,
// which the writer would follow and truncate.
func Write(path string, mode os.FileMode, body func(io.Writer) error) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp for %q: %w", path, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err = body(f); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Chmod(mode); err != nil {
		_ = f.Close()
		return fmt.Errorf("chmod %q: %w", tmp, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close %q: %w", tmp, err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %q -> %q: %w", tmp, path, err)
	}
	return nil
}

// WriteBytes is Write for an in-memory body.
func WriteBytes(path string, mode os.FileMode, data []byte) error {
	return Write(path, mode, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// OpenLock opens (creating if absent) a lock file without following a symlink, and refuses anything
// that is not a singly linked regular file: a caller that chowns or truncates the handle would
// otherwise act on whatever a planted link names.
func OpenLock(path string, mode os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, mode) //nolint:gosec // caller-supplied lock path; O_NOFOLLOW plus the checks below bound what it can reach
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !st.Mode().IsRegular() || (ok && sys.Nlink != 1) {
		_ = f.Close()
		return nil, fmt.Errorf("lock file %q is not a singly linked regular file", path)
	}
	return f, nil
}
