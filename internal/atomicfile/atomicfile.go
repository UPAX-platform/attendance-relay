// Package atomicfile replaces files so that a crash or power cut leaves
// either the old content or the new, never an empty or partial file.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write replaces name with b: a temporary file is written and fsynced,
// renamed over name, and the directory fsynced.
func Write(name string, b []byte, perm os.FileMode) error {
	return write(name, b, perm, false)
}

// WritePrivate is Write for secrets: the file is readable by the service
// account and administrators only (mode 0600 on Unix; on Windows an ACL for
// SYSTEM and Administrators), from the moment the temporary file exists.
func WritePrivate(name string, b []byte) error {
	return write(name, b, 0o600, true)
}

// SecureDir limits dir so that only the service account and administrators
// can create or change files in it; everyone else may read. On Unix the
// directory's mode is left to its creator.
func SecureDir(dir string) error { return secureDir(dir) }

func write(name string, b []byte, perm os.FileMode, private bool) error {
	// A fresh, unpredictable name, created exclusively: nobody else can
	// have it open or have made it first.
	f, err := createTemp(filepath.Dir(name), filepath.Base(name)+".*.tmp", private)
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if !private {
		if err := f.Chmod(perm); err != nil {
			return fail(err)
		}
	}
	if _, err := f.Write(b); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, name); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(name)); err == nil {
		_ = d.Sync() // not supported on every platform (Windows); best effort
		d.Close()
	}
	return nil
}
