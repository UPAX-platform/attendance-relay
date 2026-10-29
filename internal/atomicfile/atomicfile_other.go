//go:build !windows

package atomicfile

import "os"

// createTemp makes the temporary file. os.CreateTemp already creates it
// with mode 0600, which is private on Unix.
func createTemp(dir, pattern string, _ bool) (*os.File, error) {
	return os.CreateTemp(dir, pattern)
}

func secureDir(string) error { return nil }
