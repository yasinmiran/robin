//go:build linux

package agent

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/yasinmiran/robin/internal/protocol"
)

// SessionFile is the name of the file in a session's run directory that
// holds its protocol.Session, as one line of JSON for the probe's sed.
const SessionFile = "session.json"

// WriteSession records s in dir, replacing the file in one rename so a
// reader never sees half of it.
func WriteSession(dir string, s protocol.Session) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+SessionFile+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, SessionFile))
}

// ReadSession reads the record WriteSession left in dir.
func ReadSession(dir string) (protocol.Session, error) {
	var s protocol.Session
	b, err := os.ReadFile(filepath.Join(dir, SessionFile))
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}
