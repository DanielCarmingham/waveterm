// Copyright 2026, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package tmuxcc

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wavetermdev/waveterm/pkg/panichandler"
	"github.com/wavetermdev/waveterm/pkg/remote/conncontroller"
)

// Manager is a registry of active tmux-CC sessions keyed by an opaque
// handle and (optionally) a caller-supplied name. Handles are generated
// on Start; callers can look up or tear down a session without holding a
// direct pointer.
//
// Manager also owns per-session event fan-out: multiple block
// controllers can subscribe to a single session and each gets every
// parsed notification. Filtering by pane or window is left to
// subscribers — the fan-out itself is broadcast.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*sessionSlot // handle -> slot
	byName   map[string]string       // name -> handle
}

type sessionSlot struct {
	handle  string
	name    string
	session *Session

	subsMu sync.Mutex
	subs   map[int]func(Event)
	nextID int
}

// Subscription is returned from Subscribe; call Unsubscribe to detach.
type Subscription struct {
	slot *sessionSlot
	id   int
}

var globalManager = &Manager{
	sessions: make(map[string]*sessionSlot),
	byName:   make(map[string]string),
}

// GlobalManager returns the process-wide session registry.
func GlobalManager() *Manager { return globalManager }

// Start spawns a new tmux-CC session and registers it. If cfg.OnEvent
// is set it is attached as an initial subscriber (useful for debug
// logging). The returned handle is opaque.
func (m *Manager) Start(ctx context.Context, cfg SessionConfig) (string, *Session, error) {
	return m.startNamed(ctx, "", cfg)
}

// StartNamed is like Start but associates the session with a name. If a
// session with that name already exists, the existing handle and
// session are returned and cfg is ignored.
func (m *Manager) StartNamed(ctx context.Context, name string, cfg SessionConfig) (string, *Session, error) {
	if name == "" {
		return "", nil, fmt.Errorf("tmuxcc: StartNamed requires a non-empty name")
	}
	m.mu.Lock()
	if h, ok := m.byName[name]; ok {
		slot := m.sessions[h]
		m.mu.Unlock()
		return h, slot.session, nil
	}
	m.mu.Unlock()
	return m.startNamed(ctx, name, cfg)
}

func (m *Manager) startNamed(ctx context.Context, name string, cfg SessionConfig) (string, *Session, error) {
	return m.startNamedUsing(ctx, name, cfg, func(wiredCfg SessionConfig) (*Session, error) {
		return StartSession(ctx, wiredCfg)
	})
}

// startNamedUsing is the shared registration + hook-wiring code used by
// both local and transport-based session starts. starter is invoked
// with an already-hooked SessionConfig to actually boot the Session.
func (m *Manager) startNamedUsing(ctx context.Context, name string, cfg SessionConfig, starter func(SessionConfig) (*Session, error)) (string, *Session, error) {
	handle := uuid.New().String()
	slot := &sessionSlot{
		handle: handle,
		name:   name,
		subs:   make(map[int]func(Event)),
	}
	if cfg.OnEvent != nil {
		slot.subs[slot.nextID] = cfg.OnEvent
		slot.nextID++
	}
	cfg.OnEvent = func(ev Event) { slot.dispatch(ev) }
	userOnExit := cfg.OnExit
	cfg.OnExit = func(err error) {
		m.mu.Lock()
		delete(m.sessions, handle)
		if name != "" && m.byName[name] == handle {
			delete(m.byName, name)
		}
		m.mu.Unlock()
		if userOnExit != nil {
			userOnExit(err)
		}
	}
	s, err := starter(cfg)
	if err != nil {
		return "", nil, err
	}
	slot.session = s
	m.mu.Lock()
	m.sessions[handle] = slot
	if name != "" {
		m.byName[name] = handle
	}
	m.mu.Unlock()
	return handle, s, nil
}

func (slot *sessionSlot) dispatch(ev Event) {
	slot.subsMu.Lock()
	listeners := make([]func(Event), 0, len(slot.subs))
	for _, fn := range slot.subs {
		listeners = append(listeners, fn)
	}
	slot.subsMu.Unlock()
	for _, fn := range listeners {
		func() {
			defer func() { panichandler.PanicHandler("tmuxcc.sessionSlot.dispatch", recover()) }()
			fn(ev)
		}()
	}
}

// Get returns the session registered under handle, or nil.
func (m *Manager) Get(handle string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	slot := m.sessions[handle]
	if slot == nil {
		return nil
	}
	return slot.session
}

// GetByName returns the handle and session registered under name, or
// ("", nil) if no such session exists.
func (m *Manager) GetByName(name string) (string, *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.byName[name]
	if !ok {
		return "", nil
	}
	slot := m.sessions[h]
	if slot == nil {
		return "", nil
	}
	return h, slot.session
}

// Subscribe attaches listener to the session identified by handle.
// Every parsed notification (regardless of pane) is delivered.
// Listeners run on the session's read goroutine and must not block.
func (m *Manager) Subscribe(handle string, listener func(Event)) (*Subscription, error) {
	if listener == nil {
		return nil, fmt.Errorf("tmuxcc: nil listener")
	}
	m.mu.Lock()
	slot := m.sessions[handle]
	m.mu.Unlock()
	if slot == nil {
		return nil, fmt.Errorf("tmuxcc: no session with handle %q", handle)
	}
	slot.subsMu.Lock()
	id := slot.nextID
	slot.nextID++
	slot.subs[id] = listener
	slot.subsMu.Unlock()
	return &Subscription{slot: slot, id: id}, nil
}

// Unsubscribe detaches the listener. Safe to call multiple times.
func (sub *Subscription) Unsubscribe() {
	if sub == nil || sub.slot == nil {
		return
	}
	sub.slot.subsMu.Lock()
	delete(sub.slot.subs, sub.id)
	sub.slot.subsMu.Unlock()
}

// Close terminates the session under handle. It's a no-op if the
// handle is unknown.
func (m *Manager) Close(handle string) error {
	m.mu.Lock()
	slot := m.sessions[handle]
	m.mu.Unlock()
	if slot == nil {
		return fmt.Errorf("tmuxcc: no session with handle %q", handle)
	}
	return slot.session.Close()
}

// EnsureLocalSession returns a handle and session for a locally-spawned
// "tmux -CC new-session -A -s <name>" session. If one is already
// registered under name, the existing handle/session are returned
// unchanged; otherwise a new one is spawned and registered.
//
// Events are logged to stderr with an [tmuxcc:<name>] prefix so a
// single subscriber style works for orchestrated + debug sessions
// alike.
//
// Sets "window-size manual" on the session so explicit resize-pane
// calls from waveterm take effect — tmux's default "latest" sizes
// panes to the last-attached client, which silently ignores our
// resize requests and causes xterm-vs-pane width mismatches.
func (m *Manager) EnsureLocalSession(ctx context.Context, name string) (string, *Session, error) {
	if name == "" {
		return "", nil, fmt.Errorf("tmuxcc: EnsureLocalSession requires a name")
	}
	if h, sess := m.GetByName(name); sess != nil {
		return h, sess, nil
	}
	cfg := SessionConfig{
		Command: []string{"tmux", "-CC", "new-session", "-A", "-s", name},
	}
	handle, sess, err := m.StartNamed(ctx, name, cfg)
	if err != nil {
		return "", nil, err
	}
	// Best-effort: tell tmux not to auto-resize the pane. Done in a
	// goroutine so a slow/unresponsive tmux doesn't block session
	// start, but log any error.
	go func() {
		cmdCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := sess.SendCommand(cmdCtx, "set-option -t "+name+" window-size manual"); err != nil {
			// Non-fatal: older tmux versions or existing sessions may
			// already have this set. Logged for diagnostic purposes.
			fmt.Printf("[tmuxcc] set window-size manual for %q: %v\n", name, err)
		}
	}()
	return handle, sess, nil
}

// EnsureRemoteSession is the SSH counterpart to EnsureLocalSession: it
// attaches to (or creates) a tmux -CC session on the remote host
// referred to by conn. The handle is keyed globally by a
// "<conn-name>/<session-name>" composite, so local and remote sessions
// with the same short name don't collide.
//
// As with the local variant, window-size=manual is set after spawn so
// explicit resize-window calls from waveterm actually propagate.
func (m *Manager) EnsureRemoteSession(ctx context.Context, conn *conncontroller.SSHConn, sessionName string) (string, *Session, error) {
	if conn == nil {
		return "", nil, fmt.Errorf("tmuxcc: EnsureRemoteSession requires a non-nil conn")
	}
	if sessionName == "" {
		return "", nil, fmt.Errorf("tmuxcc: EnsureRemoteSession requires a name")
	}
	key := conn.GetName() + "/" + sessionName
	if h, sess := m.GetByName(key); sess != nil {
		return h, sess, nil
	}
	// tmux -CC needs to run inside a shell on the remote side so that
	// env (PATH, locale) is resolved. Using the login shell keeps this
	// portable across remote shell choices.
	//
	// -A attaches if the named session exists, otherwise new-session.
	cmdStr := fmt.Sprintf("tmux -CC new-session -A -s %s", shellQuote(sessionName))
	cfg := SessionConfig{
		Rows: defaultRows,
		Cols: defaultCols,
	}
	handle, sess, err := m.startNamedUsing(ctx, key, cfg, func(wiredCfg SessionConfig) (*Session, error) {
		t, err := startSSHTransport(ctx, conn, cmdStr, defaultRows, defaultCols)
		if err != nil {
			return nil, err
		}
		return StartSessionWithTransport(ctx, wiredCfg, t)
	})
	if err != nil {
		return "", nil, err
	}
	go func() {
		cmdCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := sess.SendCommand(cmdCtx, "set-option -t "+shellQuote(sessionName)+" window-size manual"); err != nil {
			fmt.Printf("[tmuxcc] set window-size manual for %q: %v\n", key, err)
		}
	}()
	return handle, sess, nil
}

// shellQuote wraps s in single quotes for safe inclusion as a single
// tmux argument. Duplicated from the wshserver helper so tmuxcc stays
// self-contained.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Handles returns a snapshot of active handles.
func (m *Manager) Handles() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.sessions))
	for h := range m.sessions {
		out = append(out, h)
	}
	return out
}
