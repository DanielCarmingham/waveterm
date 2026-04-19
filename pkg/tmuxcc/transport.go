// Copyright 2026, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package tmuxcc

import (
	"fmt"
	"io"
	"os/exec"
	"sync"

	"github.com/creack/pty"
)

// Transport is the bidirectional byte/escape pipe the Session reads
// and writes through. Local transports wrap a pty+exec.Cmd; remote
// transports wrap an ssh.Session. Session itself is transport-agnostic.
//
// Read/Write/Close block and propagate the usual errors. SetSize tells
// the backend about a new pty window size (ioctl locally, WindowChange
// over SSH). Wait blocks until the backend exits and returns the
// backend's terminal error, if any.
type Transport interface {
	io.ReadWriteCloser
	SetSize(rows, cols int) error
	Wait() error
}

// localTransport spawns a process on a local pty and exposes it as a
// Transport. It mirrors the old Session-internal bookkeeping.
type localTransport struct {
	cmd *exec.Cmd
	tty pty.Pty

	writeMu sync.Mutex
}

func startLocalTransport(command []string, rows, cols int) (*localTransport, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("tmuxcc: empty Command")
	}
	cmd := exec.Command(command[0], command[1:]...)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return nil, fmt.Errorf("tmuxcc: starting tmux: %w", err)
	}
	return &localTransport{cmd: cmd, tty: ptmx}, nil
}

func (t *localTransport) Read(p []byte) (int, error)  { return t.tty.Read(p) }

func (t *localTransport) Write(p []byte) (int, error) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.tty.Write(p)
}

func (t *localTransport) Close() error {
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	return t.tty.Close()
}

func (t *localTransport) SetSize(rows, cols int) error {
	return pty.Setsize(t.tty, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

func (t *localTransport) Wait() error {
	return t.cmd.Wait()
}
