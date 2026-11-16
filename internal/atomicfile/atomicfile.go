// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// Package atomicfile holds the file opens that stay safe when a privileged process writes into a directory another user owns.
package atomicfile

import (
	"errors"
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

// openFile is os.OpenFile; a variable so a test can see the flags each open carries.
var openFile = os.OpenFile

// openLockFile opens an existing lock file WITHOUT O_CREAT and creates it only when it is absent. In a sticky,
// group-writable directory Linux (fs.protected_regular) refuses an O_CREAT open of a file owned by neither the caller
// nor the directory's owner, root included, so a root run could not take a lock the service user had created.
func openLockFile(path string, mode os.FileMode) (*os.File, error) {
	const base = syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	for range 3 {
		f, err := openFile(path, os.O_RDWR|base, 0) //nolint:gosec // caller-supplied lock path; O_NOFOLLOW plus OpenLock's checks bound what it can reach
		if errors.Is(err, os.ErrPermission) {
			// flock needs no write access.
			f, err = openFile(path, os.O_RDONLY|base, 0) //nolint:gosec // as above
		}
		if !errors.Is(err, os.ErrNotExist) {
			return f, err
		}
		f, err = openFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR|base, mode) //nolint:gosec // as above
		if !errors.Is(err, os.ErrExist) {
			return f, err
		}
	}
	return nil, fmt.Errorf("lock file %q kept appearing and vanishing", path)
}

// OpenLock opens (creating if absent) a lock file without following a symlink, and refuses anything
// that is not a singly linked regular file: a caller that chowns or truncates the handle would
// otherwise act on whatever a planted link names.
func OpenLock(path string, mode os.FileMode) (*os.File, error) {
	f, err := openLockFile(path, mode)
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
