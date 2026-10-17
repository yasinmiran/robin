// Package deploy installs the agent binary on a host over SFTP.
package deploy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"github.com/pkg/sftp"

	"github.com/yasinmiran/remotejump/internal/agentbin"
	"github.com/yasinmiran/remotejump/internal/sshmux"
)

// Target describes the host side, as the probe reports it.
type Target struct {
	// Dir is the absolute directory on the host that holds agent binaries.
	Dir string
	// PresentHash is the SHA-256 of the agent already installed there, if any.
	PresentHash string
	// SHA256Tool is the host's hashing tool: sha256sum, shasum, openssl,
	// busybox, or empty when the host has none.
	SHA256Tool string
}

// Host is what Ensure needs from the connection; *sshmux.Master provides it.
type Host interface {
	Output(ctx context.Context, remoteCmd string, stdin io.Reader) ([]byte, error)
	SFTP(ctx context.Context) (*sftp.Client, error)
}

// Ensure makes sure the agent a is installed in t.Dir and returns its path on
// the host. It uploads under a temporary name, verifies the SHA-256 on the
// host, and renames into place, so a partial or corrupt upload is never
// installed. Older agents in the directory are removed; one that is running
// keeps running, since Linux keeps an unlinked executable alive.
func Ensure(ctx context.Context, h Host, t Target, a agentbin.Agent) (string, error) {
	if !path.IsAbs(t.Dir) {
		return "", fmt.Errorf("deploy: agent dir %q is not absolute", t.Dir)
	}
	final := path.Join(t.Dir, a.Name())
	if strings.EqualFold(t.PresentHash, a.Hash) {
		return final, nil
	}

	c, err := h.SFTP(ctx)
	if err != nil {
		return "", fmt.Errorf("deploy: %w", err)
	}
	defer c.Close()
	if err := prepareDir(c, t.Dir); err != nil {
		return "", err
	}
	tmp, err := upload(c, t.Dir, a)
	if err != nil {
		return "", err
	}
	if err := install(ctx, h, c, t.SHA256Tool, tmp, final, a.Hash); err != nil {
		_ = c.Remove(tmp)
		return "", err
	}
	removeStale(c, t.Dir, a.Name())
	return final, nil
}

// prepareDir creates dir if needed and refuses one that other users could
// write to, since they could swap the binary between upload and exec.
func prepareDir(c *sftp.Client, dir string) error {
	fi, err := c.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := c.MkdirAll(dir); err != nil {
			return fmt.Errorf("deploy: create %s: %w", dir, err)
		}
		if err := c.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("deploy: chmod %s: %w", dir, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("deploy: %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("deploy: %s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("deploy: %s is writable by other users (mode %v)", dir, fi.Mode().Perm())
	}
	return nil
}

func upload(c *sftp.Client, dir string, a agentbin.Agent) (string, error) {
	tmp := path.Join(dir, "."+a.Name()+".tmp-"+rand.Text())
	f, err := c.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return "", fmt.Errorf("deploy: create %s: %w", tmp, err)
	}
	if err := write(f, a); err != nil {
		_ = c.Remove(tmp)
		return "", fmt.Errorf("deploy: upload %s: %w", tmp, err)
	}
	return tmp, nil
}

func write(f *sftp.File, a agentbin.Agent) (err error) {
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	if err := f.Chmod(0o700); err != nil {
		return err
	}
	src, err := a.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	// Sequential writes cost a round trip per 32 KiB packet.
	_, err = f.ReadFromWithConcurrency(src, 0)
	return err
}

func install(ctx context.Context, h Host, c *sftp.Client, tool, tmp, final, want string) error {
	got, err := remoteHash(ctx, h, c, tool, tmp)
	if err != nil {
		return fmt.Errorf("deploy: hash %s: %w", tmp, err)
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("deploy: uploaded agent is corrupt: sha256 on host %s, want %s", got, want)
	}
	if _, ok := c.HasExtension("posix-rename@openssh.com"); ok {
		err = c.PosixRename(tmp, final)
	} else {
		// Plain SFTP rename refuses to replace an existing file.
		_ = c.Remove(final)
		err = c.Rename(tmp, final)
	}
	if err != nil {
		return fmt.Errorf("deploy: install %s: %w", final, err)
	}
	return nil
}

// remoteHash hashes file on the host with tool, or by reading it back when
// the host has no tool or the tool fails.
func remoteHash(ctx context.Context, h Host, c *sftp.Client, tool, file string) (string, error) {
	if argv := hashCommand(tool, file); argv != nil {
		out, err := h.Output(ctx, sshmux.Quote(argv...), nil)
		if sum, ok := parseHash(out); err == nil && ok {
			return sum, nil
		}
	}
	f, err := c.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func hashCommand(tool, file string) []string {
	switch tool {
	case "sha256sum":
		return []string{"sha256sum", file}
	case "shasum":
		return []string{"shasum", "-a", "256", file}
	case "openssl":
		return []string{"openssl", "dgst", "-sha256", "-r", file}
	case "busybox":
		return []string{"busybox", "sha256sum", file}
	}
	return nil
}

// parseHash takes the digest from "<hex> <file>" output, the shape every
// supported tool prints.
func parseHash(out []byte) (string, bool) {
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", false
	}
	sum := strings.ToLower(fields[0])
	if _, err := hex.DecodeString(sum); err != nil || len(sum) != sha256.Size*2 {
		return "", false
	}
	return sum, true
}

// removeStale deletes other agent versions, and temporary files that an
// interrupted upload left behind. Fresh temporary files may belong to an
// upload in progress, so they stay.
func removeStale(c *sftp.Client, dir, keep string) {
	entries, err := c.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if name == keep || !e.Mode().IsRegular() {
			continue
		}
		old := strings.HasPrefix(name, agentbin.NamePrefix)
		abandoned := strings.HasPrefix(name, "."+agentbin.NamePrefix) && strings.Contains(name, ".tmp-") &&
			time.Since(e.ModTime()) > time.Hour
		if old || abandoned {
			_ = c.Remove(path.Join(dir, name))
		}
	}
}
