// Copyright 2026, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package tmuxcc_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/wavetermdev/waveterm/pkg/tmuxcc"
)

func TestParseNotification(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		line string
		want tmuxcc.Event
	}{
		{
			name: "begin",
			line: "%begin 1700000000 1 0",
			want: tmuxcc.EventBegin{Timestamp: "1700000000", CmdNum: "1", Flags: "0"},
		},
		{
			name: "end",
			line: "%end 1700000000 1 0",
			want: tmuxcc.EventEnd{Timestamp: "1700000000", CmdNum: "1", Flags: "0"},
		},
		{
			name: "error",
			line: "%error 1700000000 2 0",
			want: tmuxcc.EventError{Timestamp: "1700000000", CmdNum: "2", Flags: "0"},
		},
		{
			name: "window-add",
			line: "%window-add @3",
			want: tmuxcc.EventWindowAdd{WindowID: "@3"},
		},
		{
			name: "window-close",
			line: "%window-close @3",
			want: tmuxcc.EventWindowClose{WindowID: "@3"},
		},
		{
			name: "window-renamed with space in name",
			line: "%window-renamed @3 my shiny window",
			want: tmuxcc.EventWindowRenamed{WindowID: "@3", Name: "my shiny window"},
		},
		{
			name: "session-changed",
			line: "%session-changed $2 waveterm",
			want: tmuxcc.EventSessionChanged{SessionID: "$2", Name: "waveterm"},
		},
		{
			name: "session-renamed",
			line: "%session-renamed project-alpha",
			want: tmuxcc.EventSessionRenamed{Name: "project-alpha"},
		},
		{
			name: "sessions-changed",
			line: "%sessions-changed",
			want: tmuxcc.EventSessionsChanged{},
		},
		{
			name: "layout-change four fields",
			line: "%layout-change @1 b25f,80x24,0,0,0 b25f,80x24,0,0,0 1",
			want: tmuxcc.EventLayoutChange{WindowID: "@1", Layout: "b25f,80x24,0,0,0", VisibleLayout: "b25f,80x24,0,0,0", Flags: "1"},
		},
		{
			name: "layout-change three fields",
			line: "%layout-change @1 b25f,80x24,0,0,0 b25f,80x24,0,0,0",
			want: tmuxcc.EventLayoutChange{WindowID: "@1", Layout: "b25f,80x24,0,0,0", VisibleLayout: "b25f,80x24,0,0,0"},
		},
		{
			name: "pane-mode-changed",
			line: "%pane-mode-changed %5",
			want: tmuxcc.EventPaneModeChanged{PaneID: "%5"},
		},
		{
			name: "exit with reason",
			line: "%exit server exited",
			want: tmuxcc.EventExit{Reason: "server exited"},
		},
		{
			name: "exit no reason",
			line: "%exit",
			want: tmuxcc.EventExit{},
		},
		{
			name: "client-detached",
			line: "%client-detached client-1",
			want: tmuxcc.EventClientDetached{Name: "client-1"},
		},
		{
			name: "unknown",
			line: "%never-heard-of-it foo bar",
			want: tmuxcc.EventUnknownNotification{Name: "never-heard-of-it", Raw: "%never-heard-of-it foo bar"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tmuxcc.ParseNotification(tc.line)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestParseNotificationNonNotification(t *testing.T) {
	t.Parallel()

	_, err := tmuxcc.ParseNotification("pane_id=%0")
	if !errors.Is(err, tmuxcc.ErrNotNotification) {
		t.Fatalf("expected ErrNotNotification, got %v", err)
	}
}

func TestParseOutput(t *testing.T) {
	t.Parallel()

	// hello\r\n — \r is 015, \n is 012
	ev, err := tmuxcc.ParseNotification("%output %0 hello\\015\\012")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out, ok := ev.(tmuxcc.EventOutput)
	if !ok {
		t.Fatalf("expected EventOutput, got %T", ev)
	}
	if out.PaneID != "%0" {
		t.Fatalf("PaneID = %q, want %%0", out.PaneID)
	}
	if !bytes.Equal(out.Data, []byte("hello\r\n")) {
		t.Fatalf("Data = %q, want %q", out.Data, "hello\r\n")
	}
}

func TestParseExtendedOutput(t *testing.T) {
	t.Parallel()

	// tmux emits this form when replaying scrollback on attach
	ev, err := tmuxcc.ParseNotification("%extended-output %2 500 : foo\\040bar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out, ok := ev.(tmuxcc.EventExtendedOutput)
	if !ok {
		t.Fatalf("expected EventExtendedOutput, got %T", ev)
	}
	if out.PaneID != "%2" || out.AgeMs != 500 {
		t.Fatalf("got pane=%q age=%d, want %%2/500", out.PaneID, out.AgeMs)
	}
	if !bytes.Equal(out.Data, []byte("foo bar")) {
		t.Fatalf("Data = %q, want %q", out.Data, "foo bar")
	}
}

func TestDecodeOutputData(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want []byte
	}{
		{in: "", want: []byte{}},
		{in: "hello", want: []byte("hello")},
		{in: "\\000", want: []byte{0x00}},
		{in: "\\377", want: []byte{0xff}},
		{in: "\\134", want: []byte{'\\'}},
		{in: "a\\015\\012b", want: []byte{'a', '\r', '\n', 'b'}},
		{in: "\\12", want: []byte("\\12")}, // malformed: too short, passed through literally
		{in: "\\89a", want: []byte("\\89a")}, // malformed: non-octal digit
		{in: "\\\\", want: []byte("\\\\")}, // malformed escape; passed through (real tmux uses \134, never \\)
	}

	for _, tc := range cases {
		got := tmuxcc.DecodeOutputData(tc.in)
		if !bytes.Equal(got, tc.want) {
			t.Errorf("DecodeOutputData(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEncodeSendKeysHex(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   []byte
		want string
	}{
		{in: nil, want: ""},
		{in: []byte{}, want: ""},
		{in: []byte{0x00}, want: "00"},
		{in: []byte{0xff}, want: "ff"},
		{in: []byte("ls\n"), want: "6c 73 0a"},
		{in: []byte{0x1b, '['}, want: "1b 5b"},
	}

	for _, tc := range cases {
		got := tmuxcc.EncodeSendKeysHex(tc.in)
		if got != tc.want {
			t.Errorf("EncodeSendKeysHex(% x) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
