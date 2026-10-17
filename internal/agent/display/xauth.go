//go:build linux

package display

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"os"
	"path/filepath"
)

// Xauthority address families, from <X11/Xauth.h>.
const (
	familyLocal = 256
	familyWild  = 65535
)

func newCookie() ([]byte, error) {
	c := make([]byte, 16)
	_, err := rand.Read(c)
	return c, err
}

// writeXauthority writes an authority file granting cookie for display
// number num. Xvfb needs the file before it has picked a number, so an empty
// num writes a single wildcard entry that matches any display.
func writeXauthority(path string, cookie []byte, num string) error {
	var b bytes.Buffer
	if num != "" {
		if host, err := os.Hostname(); err == nil {
			putEntry(&b, familyLocal, host, num, cookie)
		}
	}
	putEntry(&b, familyWild, "", num, cookie)
	return writeFileAtomic(path, b.Bytes())
}

// putEntry appends one entry: family, then length-prefixed address, display
// number, auth name and data, all big-endian.
func putEntry(b *bytes.Buffer, family uint16, addr, num string, cookie []byte) {
	binary.Write(b, binary.BigEndian, family)
	for _, f := range [][]byte{[]byte(addr), []byte(num), []byte("MIT-MAGIC-COOKIE-1"), cookie} {
		binary.Write(b, binary.BigEndian, uint16(len(f)))
		b.Write(f)
	}
}

// writeFileAtomic writes a 0600 file that readers never see half-written.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
