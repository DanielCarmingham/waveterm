// Copyright 2026, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

// Package tmuxcc implements tmux control-mode (tmux -CC) support.
//
// protocol.go parses tmux control-mode notification lines and encodes
// data for transmission back via send-keys. Higher-level session
// management (framing, command/response matching) lives in session.go.
package tmuxcc

import (
	"errors"
	"fmt"
	"strings"
)

// Event is a parsed tmux-CC notification line.
type Event interface {
	isTmuxEvent()
}

type EventOutput struct {
	PaneID string
	Data   []byte
}

type EventExtendedOutput struct {
	PaneID string
	AgeMs  int
	Data   []byte
}

type EventBegin struct {
	Timestamp string
	CmdNum    string
	Flags     string
}

type EventEnd struct {
	Timestamp string
	CmdNum    string
	Flags     string
}

type EventError struct {
	Timestamp string
	CmdNum    string
	Flags     string
}

type EventWindowAdd struct {
	WindowID string
}

type EventWindowClose struct {
	WindowID string
}

type EventWindowRenamed struct {
	WindowID string
	Name     string
}

type EventUnlinkedWindowAdd struct {
	WindowID string
}

type EventUnlinkedWindowClose struct {
	WindowID string
}

type EventUnlinkedWindowRenamed struct {
	WindowID string
	Name     string
}

type EventSessionChanged struct {
	SessionID string
	Name      string
}

type EventSessionRenamed struct {
	Name string
}

type EventSessionsChanged struct{}

type EventLayoutChange struct {
	WindowID      string
	Layout        string
	VisibleLayout string
	Flags         string
}

type EventPaneModeChanged struct {
	PaneID string
}

type EventSessionWindowChanged struct {
	SessionID string
	WindowID  string
}

type EventClientSessionChanged struct {
	ClientName  string
	SessionID   string
	SessionName string
}

type EventWindowPaneChanged struct {
	WindowID string
	PaneID   string
}

type EventExit struct {
	Reason string
}

type EventClientDetached struct {
	Name string
}

// EventUnknownNotification is emitted for any %-prefixed line we don't
// explicitly recognize. We keep these so callers can log / react rather
// than silently drop them.
type EventUnknownNotification struct {
	Name string
	Raw  string
}

func (EventOutput) isTmuxEvent()                {}
func (EventExtendedOutput) isTmuxEvent()        {}
func (EventBegin) isTmuxEvent()                 {}
func (EventEnd) isTmuxEvent()                   {}
func (EventError) isTmuxEvent()                 {}
func (EventWindowAdd) isTmuxEvent()             {}
func (EventWindowClose) isTmuxEvent()           {}
func (EventWindowRenamed) isTmuxEvent()         {}
func (EventUnlinkedWindowAdd) isTmuxEvent()     {}
func (EventUnlinkedWindowClose) isTmuxEvent()   {}
func (EventUnlinkedWindowRenamed) isTmuxEvent() {}
func (EventSessionChanged) isTmuxEvent()        {}
func (EventSessionRenamed) isTmuxEvent()        {}
func (EventSessionsChanged) isTmuxEvent()       {}
func (EventLayoutChange) isTmuxEvent()          {}
func (EventPaneModeChanged) isTmuxEvent()       {}
func (EventSessionWindowChanged) isTmuxEvent()  {}
func (EventClientSessionChanged) isTmuxEvent()  {}
func (EventWindowPaneChanged) isTmuxEvent()     {}
func (EventExit) isTmuxEvent()                  {}
func (EventClientDetached) isTmuxEvent()        {}
func (EventUnknownNotification) isTmuxEvent()   {}

// ErrNotNotification is returned by ParseNotification when the line does
// not start with '%'. It lets the session loop distinguish notification
// lines from raw command-response body lines.
var ErrNotNotification = errors.New("not a tmux-CC notification")

// ParseNotification parses a single line of tmux-CC output. The line
// must not contain a trailing newline. If the line does not start with
// '%' (after any leading DCS prefix), ErrNotNotification is returned.
//
// Unknown notifications return EventUnknownNotification (not an error)
// so the caller can log them without aborting the session.
func ParseNotification(line string) (Event, error) {
	// tmux opens control mode by emitting a DCS-style prefix like
	// "\x1bP1000p" immediately before the first %begin. Strip any
	// "\x1bP...p" prefix so the real notification parses normally.
	if strings.HasPrefix(line, "\x1bP") {
		if end := strings.IndexByte(line[2:], 'p'); end >= 0 {
			line = line[2+end+1:]
		}
	}
	if !strings.HasPrefix(line, "%") {
		return nil, ErrNotNotification
	}
	name, rest := splitFirst(line[1:], ' ')
	switch name {
	case "output":
		return parseOutput(rest)
	case "extended-output":
		return parseExtendedOutput(rest)
	case "begin":
		ts, num, flags := splitThree(rest)
		return EventBegin{Timestamp: ts, CmdNum: num, Flags: flags}, nil
	case "end":
		ts, num, flags := splitThree(rest)
		return EventEnd{Timestamp: ts, CmdNum: num, Flags: flags}, nil
	case "error":
		ts, num, flags := splitThree(rest)
		return EventError{Timestamp: ts, CmdNum: num, Flags: flags}, nil
	case "window-add":
		return EventWindowAdd{WindowID: strings.TrimSpace(rest)}, nil
	case "window-close":
		return EventWindowClose{WindowID: strings.TrimSpace(rest)}, nil
	case "window-renamed":
		id, nm := splitFirst(rest, ' ')
		return EventWindowRenamed{WindowID: id, Name: nm}, nil
	case "unlinked-window-add":
		return EventUnlinkedWindowAdd{WindowID: strings.TrimSpace(rest)}, nil
	case "unlinked-window-close":
		return EventUnlinkedWindowClose{WindowID: strings.TrimSpace(rest)}, nil
	case "unlinked-window-renamed":
		id, nm := splitFirst(rest, ' ')
		return EventUnlinkedWindowRenamed{WindowID: id, Name: nm}, nil
	case "session-changed":
		id, nm := splitFirst(rest, ' ')
		return EventSessionChanged{SessionID: id, Name: nm}, nil
	case "session-renamed":
		return EventSessionRenamed{Name: rest}, nil
	case "sessions-changed":
		return EventSessionsChanged{}, nil
	case "layout-change":
		return parseLayoutChange(rest), nil
	case "pane-mode-changed":
		return EventPaneModeChanged{PaneID: strings.TrimSpace(rest)}, nil
	case "session-window-changed":
		sid, wid := splitFirst(rest, ' ')
		return EventSessionWindowChanged{SessionID: sid, WindowID: wid}, nil
	case "client-session-changed":
		// Format: %client-session-changed <client-name> $<sid> <session-name>
		clientName, r1 := splitFirst(rest, ' ')
		sid, sessName := splitFirst(r1, ' ')
		return EventClientSessionChanged{ClientName: clientName, SessionID: sid, SessionName: sessName}, nil
	case "window-pane-changed":
		wid, pid := splitFirst(rest, ' ')
		return EventWindowPaneChanged{WindowID: wid, PaneID: pid}, nil
	case "exit":
		return EventExit{Reason: rest}, nil
	case "client-detached":
		return EventClientDetached{Name: rest}, nil
	}
	return EventUnknownNotification{Name: name, Raw: line}, nil
}

func parseOutput(rest string) (Event, error) {
	paneID, data := splitFirst(rest, ' ')
	if paneID == "" {
		return nil, fmt.Errorf("malformed %%output: %q", rest)
	}
	return EventOutput{PaneID: paneID, Data: DecodeOutputData(data)}, nil
}

func parseExtendedOutput(rest string) (Event, error) {
	// Format: %extended-output %<pane> <age-ms> : <escaped-data>
	// Older tmux uses %extended-output %<pane> <age-ms> <data> without the
	// colon; tolerate both.
	paneID, rest2 := splitFirst(rest, ' ')
	if paneID == "" {
		return nil, fmt.Errorf("malformed %%extended-output: %q", rest)
	}
	ageStr, rest3 := splitFirst(rest2, ' ')
	if strings.HasPrefix(rest3, ": ") {
		rest3 = rest3[2:]
	} else if rest3 == ":" {
		rest3 = ""
	}
	age := 0
	for i := 0; i < len(ageStr); i++ {
		c := ageStr[i]
		if c < '0' || c > '9' {
			age = 0
			break
		}
		age = age*10 + int(c-'0')
	}
	return EventExtendedOutput{PaneID: paneID, AgeMs: age, Data: DecodeOutputData(rest3)}, nil
}

func parseLayoutChange(rest string) EventLayoutChange {
	// %layout-change @<id> <layout> <visible-layout> [flags]
	id, r1 := splitFirst(rest, ' ')
	layout, r2 := splitFirst(r1, ' ')
	visible, flags := splitFirst(r2, ' ')
	return EventLayoutChange{WindowID: id, Layout: layout, VisibleLayout: visible, Flags: flags}
}

// splitFirst splits s at the first occurrence of sep and returns the
// pair; if sep is not present, the entire string is returned as the
// first element and the second is empty.
func splitFirst(s string, sep byte) (string, string) {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}

func splitThree(s string) (string, string, string) {
	a, r1 := splitFirst(s, ' ')
	b, c := splitFirst(r1, ' ')
	return a, b, c
}

// DecodeOutputData reverses tmux's control-mode output escaping.
//
// tmux emits bytes 0x20-0x7E literally (except '\\' which becomes
// "\\134") and all other bytes as three-digit octal escapes "\\xxx".
// Anything that doesn't match a valid three-digit octal escape after a
// backslash is passed through literally — this matches iTerm2's decoder
// and is defensive against unexpected input.
func DecodeOutputData(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c != '\\' {
			out = append(out, c)
			i++
			continue
		}
		if i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b := (s[i+1]-'0')*64 + (s[i+2]-'0')*8 + (s[i+3] - '0')
			out = append(out, b)
			i += 4
			continue
		}
		out = append(out, c)
		i++
	}
	return out
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// EncodeSendKeysHex formats bytes as space-separated two-digit hex for
// use as arguments to `tmux send-keys -H -t <pane> ...`. The `-H` flag
// interprets each argument as a hex byte, bypassing tmux's key-name
// lookup, which is what we want when forwarding raw terminal input.
func EncodeSendKeysHex(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	const hex = "0123456789abcdef"
	buf := make([]byte, 0, len(data)*3)
	for i, b := range data {
		if i > 0 {
			buf = append(buf, ' ')
		}
		buf = append(buf, hex[b>>4], hex[b&0xf])
	}
	return string(buf)
}
