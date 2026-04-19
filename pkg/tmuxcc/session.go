// Copyright 2026, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package tmuxcc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/wavetermdev/waveterm/pkg/panichandler"
)

const (
	defaultRows = 40
	defaultCols = 120

	// sendCommandTimeout bounds a single SendCommand call. tmux usually
	// responds in milliseconds; anything beyond this means tmux is wedged.
	sendCommandTimeout = 10 * time.Second
)

// SessionConfig configures a tmux control-mode session.
type SessionConfig struct {
	// Command is the full argv to spawn, e.g.
	//   ["tmux", "-CC", "new-session", "-A", "-s", "waveterm"]
	// The caller is responsible for including "-CC".
	Command []string

	// Rows/Cols size the pty; falls back to defaults if zero. tmux uses
	// these for its initial window dimensions.
	Rows int
	Cols int

	// OnEvent is invoked for every parsed notification. It runs on the
	// session's read goroutine, so handlers must not block.
	OnEvent func(Event)

	// OnExit is invoked once when the session terminates (from %exit,
	// process exit, or Close). err is nil for a clean %exit.
	OnExit func(err error)
}

// Session is a running tmux -CC control-mode session.
//
// Lifecycle: StartSession spawns the tmux process on a pty and kicks
// off a single read goroutine that parses lines into events. Events
// outside of a %begin/%end command-response bracket are dispatched to
// OnEvent. Events inside a bracket are captured as the response to the
// currently in-flight SendCommand.
//
// Thread-safety: SendCommand and Close are safe to call concurrently
// from any goroutine. OnEvent fires on one goroutine and must not
// block.
type Session struct {
	cfg SessionConfig
	cmd *exec.Cmd
	tty pty.Pty

	stdinMu sync.Mutex // serializes writes to tty

	// pendingMu guards pending. Commands are FIFO: tmux returns
	// responses in the order we sent them, so we match by position.
	pendingMu sync.Mutex
	pending   []*pendingCmd
	active    *pendingCmd // currently collecting response body, or nil

	closeOnce sync.Once
	doneCh    chan struct{}
	waitErr   error
}

type pendingCmd struct {
	cmd      string
	respCh   chan commandResult
	started  bool   // set true once %begin arrives
	cmdNum   string // captured from %begin, verified against %end
	buf      []string
}

type commandResult struct {
	lines []string
	err   error
}

// StartSession spawns tmux in control mode and begins streaming events
// to cfg.OnEvent. The caller owns the returned *Session and must call
// Close() to release the pty and reap the process.
//
// ctx is used only for cancelling the spawn itself (e.g. exec lookup);
// it is NOT tied to the lifetime of the tmux process. Bound-to-caller
// contexts from short-lived RPC handlers would otherwise kill tmux as
// soon as the RPC returned.
func StartSession(ctx context.Context, cfg SessionConfig) (*Session, error) {
	if len(cfg.Command) == 0 {
		return nil, errors.New("tmuxcc: empty Command")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, cols := cfg.Rows, cfg.Cols
	if rows <= 0 {
		rows = defaultRows
	}
	if cols <= 0 {
		cols = defaultCols
	}
	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return nil, fmt.Errorf("tmuxcc: starting tmux: %w", err)
	}
	s := &Session{
		cfg:    cfg,
		cmd:    cmd,
		tty:    ptmx,
		doneCh: make(chan struct{}),
	}
	go s.readLoop()
	go s.waitLoop()
	return s, nil
}

func (s *Session) readLoop() {
	defer func() {
		panichandler.PanicHandler("tmuxcc.Session.readLoop", recover())
	}()
	r := bufio.NewReaderSize(s.tty, 1<<16)
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			s.handleLine(strings.TrimRight(line, "\r\n"))
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.failPending(fmt.Errorf("tmuxcc: read: %w", err))
			} else {
				s.failPending(errors.New("tmuxcc: tmux exited"))
			}
			return
		}
	}
}

func (s *Session) handleLine(line string) {
	s.pendingMu.Lock()
	active := s.active
	s.pendingMu.Unlock()

	// Inside a %begin/%end bracket: every line is response body until we
	// see %end or %error matching the captured cmd-num. Notifications
	// are NOT interleaved with command responses in tmux's protocol.
	if active != nil {
		ev, err := ParseNotification(line)
		if err == nil {
			switch v := ev.(type) {
			case EventEnd:
				if v.CmdNum == active.cmdNum {
					s.finishActive(commandResult{lines: active.buf})
					return
				}
			case EventError:
				if v.CmdNum == active.cmdNum {
					s.finishActive(commandResult{lines: active.buf, err: fmt.Errorf("tmux: %s", strings.Join(active.buf, "\n"))})
					return
				}
			}
		}
		active.buf = append(active.buf, line)
		return
	}

	ev, err := ParseNotification(line)
	if err != nil {
		// Non-notification line outside a response bracket. Should not
		// happen with a well-behaved tmux but log defensively.
		if s.cfg.OnEvent != nil {
			s.cfg.OnEvent(EventUnknownNotification{Raw: line})
		}
		return
	}
	if begin, ok := ev.(EventBegin); ok {
		s.pendingMu.Lock()
		if len(s.pending) > 0 {
			s.pending[0].started = true
			s.pending[0].cmdNum = begin.CmdNum
			s.active = s.pending[0]
		} else {
			// %begin without a pending command — shouldn't happen. Allocate
			// an orphan entry so we still consume the body and don't get
			// stuck.
			s.active = &pendingCmd{cmdNum: begin.CmdNum}
		}
		s.pendingMu.Unlock()
		return
	}
	if exit, ok := ev.(EventExit); ok {
		if s.cfg.OnEvent != nil {
			s.cfg.OnEvent(exit)
		}
		s.failPending(nil)
		return
	}
	if s.cfg.OnEvent != nil {
		s.cfg.OnEvent(ev)
	}
}

func (s *Session) finishActive(res commandResult) {
	s.pendingMu.Lock()
	active := s.active
	s.active = nil
	if len(s.pending) > 0 && s.pending[0] == active {
		s.pending = s.pending[1:]
	}
	s.pendingMu.Unlock()
	if active == nil || active.respCh == nil {
		return
	}
	select {
	case active.respCh <- res:
	default:
	}
}

func (s *Session) failPending(err error) {
	s.pendingMu.Lock()
	pending := s.pending
	s.pending = nil
	s.active = nil
	s.pendingMu.Unlock()
	for _, p := range pending {
		if p.respCh == nil {
			continue
		}
		select {
		case p.respCh <- commandResult{err: fmt.Errorf("tmuxcc: session closed: %w", err)}:
		default:
		}
	}
	s.closeOnce.Do(func() {
		s.waitErr = err
		close(s.doneCh)
		if s.cfg.OnExit != nil {
			go func() {
				defer func() { panichandler.PanicHandler("tmuxcc.Session.OnExit", recover()) }()
				s.cfg.OnExit(err)
			}()
		}
	})
}

func (s *Session) waitLoop() {
	defer func() { panichandler.PanicHandler("tmuxcc.Session.waitLoop", recover()) }()
	err := s.cmd.Wait()
	s.failPending(err)
	_ = s.tty.Close()
}

// SendCommand writes cmd to tmux (a newline is appended) and blocks
// until tmux returns the corresponding %end or %error. Response body
// lines are returned verbatim (no trailing newlines). A non-nil error
// is returned either for %error responses or for session-level
// failures.
func (s *Session) SendCommand(ctx context.Context, cmd string) ([]string, error) {
	if strings.ContainsAny(cmd, "\n\r") {
		return nil, errors.New("tmuxcc: command must not contain newlines")
	}
	pc := &pendingCmd{cmd: cmd, respCh: make(chan commandResult, 1)}
	s.pendingMu.Lock()
	s.pending = append(s.pending, pc)
	s.pendingMu.Unlock()

	s.stdinMu.Lock()
	_, err := io.WriteString(s.tty, cmd+"\n")
	s.stdinMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("tmuxcc: writing command: %w", err)
	}

	select {
	case res := <-pc.respCh:
		return res.lines, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(sendCommandTimeout):
		return nil, fmt.Errorf("tmuxcc: command timed out: %q", cmd)
	case <-s.doneCh:
		return nil, fmt.Errorf("tmuxcc: session closed before response: %w", s.waitErr)
	}
}

// SendPaneInput forwards raw terminal input bytes to the given pane via
// tmux `send-keys -H`. The bytes are hex-encoded so tmux pumps them
// through verbatim without key-name interpretation.
func (s *Session) SendPaneInput(ctx context.Context, paneID string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	cmd := fmt.Sprintf("send-keys -t %s -H %s", paneID, EncodeSendKeysHex(data))
	_, err := s.SendCommand(ctx, cmd)
	return err
}

// Resize updates the pty size and sends refresh-client -C to tmux so
// its client view matches. Per-pane resizes (if the waveterm layout
// changes) are handled separately via resize-pane.
func (s *Session) Resize(ctx context.Context, rows, cols int) error {
	if rows <= 0 || cols <= 0 {
		return fmt.Errorf("tmuxcc: invalid size %dx%d", cols, rows)
	}
	if err := pty.Setsize(s.tty, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)}); err != nil {
		return fmt.Errorf("tmuxcc: pty resize: %w", err)
	}
	cmd := fmt.Sprintf("refresh-client -C %d,%d", cols, rows)
	_, err := s.SendCommand(ctx, cmd)
	return err
}

// Close terminates the tmux process and releases the pty. Safe to call
// multiple times; later calls are no-ops.
func (s *Session) Close() error {
	select {
	case <-s.doneCh:
		return s.waitErr
	default:
	}
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	<-s.doneCh
	return s.waitErr
}

// Done returns a channel that is closed when the session terminates.
func (s *Session) Done() <-chan struct{} { return s.doneCh }

// WaitErr returns the final error after Done is closed. nil means the
// session exited cleanly (%exit without error).
func (s *Session) WaitErr() error {
	<-s.doneCh
	return s.waitErr
}
