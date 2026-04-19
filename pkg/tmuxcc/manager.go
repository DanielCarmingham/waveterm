// Copyright 2026, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package tmuxcc

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/wavetermdev/waveterm/pkg/panichandler"
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
	s, err := StartSession(ctx, cfg)
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
	return m.StartNamed(ctx, name, cfg)
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
