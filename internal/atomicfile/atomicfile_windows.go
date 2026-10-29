package atomicfile

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// privateSDDL grants SYSTEM (the service runs as LocalSystem) and
// Administrators full control, and protects the ACL from inheritance.
const privateSDDL = "D:P(A;;FA;;;SY)(A;;FA;;;BA)"

// createTemp makes the temporary file. A private one is created with its
// restrictive security descriptor already in place, so it is never
// readable by anyone else, not even briefly.
func createTemp(dir, pattern string, private bool) (*os.File, error) {
	if !private {
		return os.CreateTemp(dir, pattern)
	}
	sd, err := windows.SecurityDescriptorFromString(privateSDDL)
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	prefix, suffix, _ := strings.Cut(pattern, "*")
	for range 100 {
		var r [8]byte
		if _, err := rand.Read(r[:]); err != nil {
			return nil, err
		}
		name := filepath.Join(dir, prefix+hex.EncodeToString(r[:])+suffix)
		p, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return nil, err
		}
		h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) {
			continue
		}
		if err != nil {
			return nil, &os.PathError{Op: "open", Path: name, Err: err}
		}
		return os.NewFile(uintptr(h), name), nil
	}
	return nil, fmt.Errorf("atomicfile: no free temporary name in %s", dir)
}

// secureDir replaces dir's inherited ACL: SYSTEM and Administrators get
// full control, Users may only read (the health files `upax-relay status`
// shows). Files created in it inherit that; private files carry their own.
func secureDir(dir string) error {
	out, err := exec.Command("icacls", dir, "/inheritance:r",
		"/grant:r", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F", "*S-1-5-32-545:(OI)(CI)RX").CombinedOutput()
	if err != nil {
		return fmt.Errorf("restrict %s: %v: %s", dir, err, strings.TrimSpace(string(out)))
	}
	return nil
}
