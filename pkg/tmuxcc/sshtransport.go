// Copyright 2026, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package tmuxcc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/wavetermdev/waveterm/pkg/remote/conncontroller"
	"golang.org/x/crypto/ssh"
)

// sshTransport runs a command on a remote host via an existing
// conncontroller.SSHConn and exposes it as a tmuxcc.Transport. The
// command is typically "tmux -CC new-session -A -s <name>" — anything
// that emits the tmux control-mode protocol on stdout.
type sshTransport struct {
	session *ssh.Session
	stdin   io.WriteCloser
	stdout  io.Reader

	writeMu sync.Mutex

	waitOnce sync.Once
	waitErr  error
	waitDone chan struct{}
}

// startSSHTransport creates an SSH session on conn, requests a pty of
// the given size, and starts cmdStr remotely. Returns a Transport that
// owns the session; Close kills it.
func startSSHTransport(ctx context.Context, conn *conncontroller.SSHConn, cmdStr string, rows, cols int) (*sshTransport, error) {
	if conn == nil {
		return nil, errors.New("tmuxcc: nil SSHConn")
	}
	client := conn.GetClient()
	if client == nil {
		return nil, errors.New("tmuxcc: SSH client not available (not connected)")
	}
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("tmuxcc: ssh new session: %w", err)
	}
	// Give tmux a real pty so its control-mode output flushes line-at-
	// a-time (otherwise SSH may buffer).
	termModes := ssh.TerminalModes{
		ssh.ECHO: 0,
	}
	if err := session.RequestPty("xterm-256color", rows, cols, termModes); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("tmuxcc: ssh request pty: %w", err)
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("tmuxcc: ssh stdin pipe: %w", err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("tmuxcc: ssh stdout pipe: %w", err)
	}
	// Merge stderr into stdout so any remote-side error text still
	// surfaces in our read loop (and gets logged via
	// EventUnknownNotification).
	session.Stderr = session.Stdout
	if err := session.Start(cmdStr); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("tmuxcc: ssh start %q: %w", cmdStr, err)
	}
	t := &sshTransport{
		session:  session,
		stdin:    stdin,
		stdout:   stdout,
		waitDone: make(chan struct{}),
	}
	return t, nil
}

func (t *sshTransport) Read(p []byte) (int, error) { return t.stdout.Read(p) }

func (t *sshTransport) Write(p []byte) (int, error) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.stdin.Write(p)
}

func (t *sshTransport) Close() error {
	// Best-effort signal + close. Remote tmux will notice stdin EOF and
	// tear down the session.
	_ = t.stdin.Close()
	return t.session.Close()
}

func (t *sshTransport) SetSize(rows, cols int) error {
	// ssh.Session.WindowChange takes (height, width) — same as tmux.
	return t.session.WindowChange(rows, cols)
}

func (t *sshTransport) Wait() error {
	t.waitOnce.Do(func() {
		t.waitErr = t.session.Wait()
		close(t.waitDone)
	})
	<-t.waitDone
	return t.waitErr
}
