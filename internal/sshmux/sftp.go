package sshmux

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

// SFTP starts an sftp subsystem session over the master. Closing the client
// stops the ssh process; cancelling ctx kills it.
func (m *Master) SFTP(ctx context.Context) (*sftp.Client, error) {
	cmd := exec.CommandContext(ctx, m.ssh, m.subsystemArgs("sftp")...)
	cmd.WaitDelay = time.Second
	var stderr tailBuffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("sshmux: start sftp: %w", err)
	}
	// NewClientPipe closes the writer on failure, which reaps the process.
	c, err := sftp.NewClientPipe(stdout, &processPipe{WriteCloser: stdin, cmd: cmd})
	if err != nil {
		return nil, fmt.Errorf("sshmux: sftp: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return c, nil
}

// processPipe is the sftp client's write side. Closing it ends the session's
// stdin and reaps the ssh process, killing it if it lingers.
type processPipe struct {
	io.WriteCloser
	cmd  *exec.Cmd
	once sync.Once
}

func (p *processPipe) Close() error {
	p.once.Do(func() {
		_ = p.WriteCloser.Close()
		kill := time.AfterFunc(5*time.Second, func() { _ = p.cmd.Process.Kill() })
		_ = p.cmd.Wait()
		kill.Stop()
	})
	return nil
}
