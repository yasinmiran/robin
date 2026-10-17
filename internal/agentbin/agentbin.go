// Package agentbin holds the host agent binaries the CLI uploads, one gzipped
// static Linux build per architecture.
package agentbin

import (
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"
	"sync"
)

// The Makefile writes robin-agent-linux-<goarch>.gz here. README.md keeps
// the directory, and so the embed, valid in a fresh checkout.
//
//go:embed agents
var embedded embed.FS

const filePrefix, fileSuffix = "robin-agent-linux-", ".gz"

// NamePrefix starts the host file name of every agent version.
const NamePrefix = "robin-agent-"

// Agent is one bundled agent binary.
type Agent struct {
	Arch string
	// Hash is the hex SHA-256 of the uncompressed binary.
	Hash string
	// Size is the uncompressed size in bytes.
	Size int64

	fsys fs.FS
	file string
}

// Name is the agent's file name on the host. It changes with the content, so
// a host never runs a stale agent under a current name.
func (a Agent) Name() string {
	return NamePrefix + a.Hash[:min(len(a.Hash), 16)]
}

// Open returns the uncompressed binary.
func (a Agent) Open() (io.ReadCloser, error) {
	f, err := a.fsys.Open(a.file)
	if err != nil {
		return nil, err
	}
	z, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("agentbin: %s: %w", a.file, err)
	}
	return &gzipFile{Reader: z, f: f}, nil
}

type gzipFile struct {
	*gzip.Reader
	f fs.File
}

func (g *gzipFile) Close() error {
	return errors.Join(g.Reader.Close(), g.f.Close())
}

var cache sync.Map // goarch -> func() (Agent, error)

// For returns the embedded agent for goarch, hashing it on first use.
func For(goarch string) (Agent, error) {
	load, _ := cache.LoadOrStore(goarch, sync.OnceValues(func() (Agent, error) {
		return FromFS(bundled(), goarch)
	}))
	return load.(func() (Agent, error))()
}

// Names returns the host file name of every embedded agent, sorted. It is
// empty when the build bundled none.
func Names() []string {
	entries, _ := fs.ReadDir(bundled(), ".")
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		arch, ok := archOf(e.Name())
		if !ok {
			continue
		}
		if a, err := For(arch); err == nil {
			names = append(names, a.Name())
		}
	}
	slices.Sort(names)
	return names
}

// FromFS reads robin-agent-linux-<goarch>.gz from the root of fsys.
func FromFS(fsys fs.FS, goarch string) (Agent, error) {
	a := Agent{Arch: goarch, fsys: fsys, file: filePrefix + goarch + fileSuffix}
	if _, ok := archOf(a.file); !ok {
		return Agent{}, fmt.Errorf("agentbin: invalid architecture %q", goarch)
	}
	rc, err := a.Open()
	if err != nil {
		return Agent{}, fmt.Errorf("agentbin: no agent for linux/%s in this build (rebuild with make): %w", goarch, err)
	}
	defer rc.Close()
	h := sha256.New()
	if a.Size, err = io.Copy(h, rc); err != nil {
		return Agent{}, fmt.Errorf("agentbin: %s: %w", a.file, err)
	}
	a.Hash = hex.EncodeToString(h.Sum(nil))
	return a, nil
}

func archOf(file string) (string, bool) {
	arch, ok := strings.CutPrefix(file, filePrefix)
	if !ok {
		return "", false
	}
	arch, ok = strings.CutSuffix(arch, fileSuffix)
	return arch, ok && arch != "" && !strings.ContainsAny(arch, "/.")
}

func bundled() fs.FS {
	sub, err := fs.Sub(embedded, "agents")
	if err != nil {
		panic(err) // the embed pattern guarantees the directory
	}
	return sub
}
