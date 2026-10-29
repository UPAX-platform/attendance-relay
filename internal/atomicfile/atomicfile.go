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
	return WriteWith(name, b, perm, nil)
}

// WriteWith is Write with prepare run on the temporary file before the
// rename (to restrict its permissions, say).
func WriteWith(name string, b []byte, perm os.FileMode, prepare func(tmp string) error) error {
	tmp := name + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if prepare != nil {
		if err := prepare(tmp); err != nil {
			os.Remove(tmp)
			return err
		}
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
