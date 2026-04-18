// Copyright 2026, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package tmuxcc

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

// Manager is a registry of active tmux-CC sessions, keyed by an opaque
// handle. Handles are generated on Register and returned to callers so
// they can look up or tear down a session without holding a direct
// pointer.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
}

var globalManager = &Manager{sessions: make(map[string]*Session)}

// GlobalManager returns the process-wide session registry.
func GlobalManager() *Manager { return globalManager }

// Start spawns a new tmux-CC session and registers it with the
// manager. The returned handle is opaque; callers should use Get,
// Close, etc. to operate on the session. The caller's OnExit hook (if
// any) is chained after the manager's cleanup hook.
func (m *Manager) Start(ctx context.Context, cfg SessionConfig) (string, *Session, error) {
	handle := uuid.New().String()
	userOnExit := cfg.OnExit
	cfg.OnExit = func(err error) {
		m.mu.Lock()
		delete(m.sessions, handle)
		m.mu.Unlock()
		if userOnExit != nil {
			userOnExit(err)
		}
	}
	s, err := StartSession(ctx, cfg)
	if err != nil {
		return "", nil, err
	}
	m.mu.Lock()
	m.sessions[handle] = s
	m.mu.Unlock()
	return handle, s, nil
}

// Get returns the session registered under handle, or nil.
func (m *Manager) Get(handle string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[handle]
}

// Close terminates the session under handle. It's a no-op if the
// handle is unknown.
func (m *Manager) Close(handle string) error {
	m.mu.Lock()
	s := m.sessions[handle]
	m.mu.Unlock()
	if s == nil {
		return fmt.Errorf("tmuxcc: no session with handle %q", handle)
	}
	return s.Close()
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
