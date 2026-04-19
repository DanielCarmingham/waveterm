// Copyright 2026, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package blockcontroller

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wavetermdev/waveterm/pkg/filestore"
	"github.com/wavetermdev/waveterm/pkg/tmuxcc"
	"github.com/wavetermdev/waveterm/pkg/utilds"
	"github.com/wavetermdev/waveterm/pkg/wavebase"
	"github.com/wavetermdev/waveterm/pkg/waveobj"
	"github.com/wavetermdev/waveterm/pkg/wps"
	"github.com/wavetermdev/waveterm/pkg/wshrpc"
)

// parseCursorPos parses the "y;x" output of tmux's
// display-message '#{cursor_y};#{cursor_x}' query. Returns zero-based
// row and column.
func parseCursorPos(s string) (int, int, bool) {
	semi := strings.IndexByte(s, ';')
	if semi < 0 {
		return 0, 0, false
	}
	y, err := strconv.Atoi(strings.TrimSpace(s[:semi]))
	if err != nil {
		return 0, 0, false
	}
	x, err := strconv.Atoi(strings.TrimSpace(s[semi+1:]))
	if err != nil {
		return 0, 0, false
	}
	return y, x, true
}

// tmuxSendTimeout bounds each send-keys / resize-pane call. tmux
// usually replies in milliseconds; anything longer means the session is
// wedged and we'd rather surface an error than block the input loop.
const tmuxSendTimeout = 5 * time.Second

// TmuxController drives a single tmux pane as a waveterm block. It
// does not spawn a pty itself; it subscribes to a shared
// tmuxcc.Session (managed by tmuxcc.GlobalManager) and routes
// %output-events for its pane into the block's term file. Keystrokes
// and resize events travel back via send-keys -H and resize-pane.
type TmuxController struct {
	Lock *sync.Mutex

	TabId        string
	BlockId      string
	ConnName     string
	ProcStatus   string
	ProcExitCode int
	VersionTs    utilds.VersionTs
	RunLock      *atomic.Bool

	SessionHandle string
	Session       *tmuxcc.Session
	PaneID        string
	Subscription  *tmuxcc.Subscription
}

func MakeTmuxController(tabId string, blockId string, connName string) Controller {
	return &TmuxController{
		Lock:       &sync.Mutex{},
		TabId:      tabId,
		BlockId:    blockId,
		ConnName:   connName,
		ProcStatus: Status_Init,
		RunLock:    &atomic.Bool{},
	}
}

func (tc *TmuxController) WithLock(f func()) {
	tc.Lock.Lock()
	defer tc.Lock.Unlock()
	f()
}

func (tc *TmuxController) Start(ctx context.Context, blockMeta waveobj.MetaMapType, rtOpts *waveobj.RuntimeOpts, force bool) error {
	handle := blockMeta.GetString(waveobj.MetaKey_TmuxSessionHandle, "")
	if handle == "" {
		return fmt.Errorf("tmux block missing %q meta", waveobj.MetaKey_TmuxSessionHandle)
	}
	paneID := blockMeta.GetString(waveobj.MetaKey_TmuxPaneId, "")
	if paneID == "" {
		return fmt.Errorf("tmux block missing %q meta", waveobj.MetaKey_TmuxPaneId)
	}
	session := tmuxcc.GlobalManager().Get(handle)
	if session == nil {
		return fmt.Errorf("no tmux session with handle %q", handle)
	}
	mkCtx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()
	if err := filestore.WFS.MakeFile(mkCtx, tc.BlockId, wavebase.BlockFile_Term, nil, wshrpc.FileOpts{MaxSize: DefaultTermMaxFileSize, Circular: true}); err != nil {
		log.Printf("[tmuxcc] block %s make term file: %v (continuing)", tc.BlockId, err)
	}
	// Order matters: resize → capture → subscribe. Resize first so the
	// captured buffer reflects the block's actual dimensions. Capture
	// before subscribe so we seed xterm with the pane's current visible
	// state without double-counting events. The tiny window between
	// capture and subscribe can drop output but the subsequent stream
	// will correct any drift.
	if rtOpts != nil && rtOpts.TermSize.Rows > 0 && rtOpts.TermSize.Cols > 0 {
		resizeCtx, cancelResize := context.WithTimeout(context.Background(), tmuxSendTimeout)
		resizeCmd := fmt.Sprintf("resize-pane -t %s -x %d -y %d", paneID, rtOpts.TermSize.Cols, rtOpts.TermSize.Rows)
		if _, err := session.SendCommand(resizeCtx, resizeCmd); err != nil {
			log.Printf("[tmuxcc] block %s initial resize-pane: %v (continuing)", tc.BlockId, err)
		}
		cancelResize()
	}
	capCtx, cancelCap := context.WithTimeout(context.Background(), tmuxSendTimeout)
	capLines, err := session.SendCommand(capCtx, fmt.Sprintf("capture-pane -p -e -J -t %s", paneID))
	cancelCap()
	if err != nil {
		log.Printf("[tmuxcc] block %s capture-pane: %v (continuing)", tc.BlockId, err)
	} else if len(capLines) > 0 {
		// Trim trailing empty lines (unfilled pane rows) so xterm doesn't
		// render blank lines below the prompt.
		for len(capLines) > 0 && strings.TrimSpace(capLines[len(capLines)-1]) == "" {
			capLines = capLines[:len(capLines)-1]
		}
		seed := strings.Join(capLines, "\r\n")
		// Query tmux for the pane's current cursor position and emit an
		// ANSI cursor-position escape so xterm's cursor lands where
		// tmux says it is (right after the prompt, usually). Without
		// this, xterm's cursor sits at the end of the captured text,
		// which for a prompt line with trailing padding is wrong.
		curCtx, cancelCur := context.WithTimeout(context.Background(), tmuxSendTimeout)
		curLines, curErr := session.SendCommand(curCtx, fmt.Sprintf("display-message -p -t %s %s", paneID, strconv.Quote("#{cursor_y};#{cursor_x}")))
		cancelCur()
		if curErr == nil && len(curLines) > 0 {
			if y, x, ok := parseCursorPos(curLines[0]); ok {
				seed += fmt.Sprintf("\x1b[%d;%dH", y+1, x+1)
			} else {
				log.Printf("[tmuxcc] block %s cursor parse failed: %q", tc.BlockId, curLines[0])
			}
		}
		if err := HandleAppendBlockFile(tc.BlockId, wavebase.BlockFile_Term, []byte(seed)); err != nil {
			log.Printf("[tmuxcc] block %s seed append: %v (continuing)", tc.BlockId, err)
		}
	}
	sub, err := tmuxcc.GlobalManager().Subscribe(handle, tc.handleEvent)
	if err != nil {
		return err
	}
	tc.WithLock(func() {
		tc.SessionHandle = handle
		tc.Session = session
		tc.PaneID = paneID
		tc.Subscription = sub
		tc.ProcStatus = Status_Running
	})
	tc.sendUpdate()
	return nil
}

func (tc *TmuxController) Stop(graceful bool, newStatus string, destroy bool) {
	var sub *tmuxcc.Subscription
	var statusChanged bool
	tc.WithLock(func() {
		sub = tc.Subscription
		tc.Subscription = nil
		if newStatus != tc.ProcStatus {
			tc.ProcStatus = newStatus
			statusChanged = true
		}
	})
	if sub != nil {
		sub.Unsubscribe()
	}
	if statusChanged {
		tc.sendUpdate()
	}
}

func (tc *TmuxController) GetRuntimeStatus() *BlockControllerRuntimeStatus {
	var rtn BlockControllerRuntimeStatus
	tc.WithLock(func() {
		rtn.BlockId = tc.BlockId
		rtn.Version = tc.VersionTs.GetVersionTs()
		rtn.ShellProcStatus = tc.ProcStatus
		rtn.ShellProcConnName = tc.ConnName
		rtn.ShellProcExitCode = tc.ProcExitCode
	})
	return &rtn
}

func (tc *TmuxController) GetConnName() string { return tc.ConnName }

func (tc *TmuxController) SendInput(input *BlockInputUnion) error {
	var session *tmuxcc.Session
	var paneID string
	tc.WithLock(func() {
		session = tc.Session
		paneID = tc.PaneID
	})
	if session == nil {
		return fmt.Errorf("tmux controller not started")
	}
	ctx, cancel := context.WithTimeout(context.Background(), tmuxSendTimeout)
	defer cancel()
	if len(input.InputData) > 0 {
		if err := session.SendPaneInput(ctx, paneID, input.InputData); err != nil {
			return fmt.Errorf("tmux send-keys: %w", err)
		}
	}
	if input.TermSize != nil && input.TermSize.Rows > 0 && input.TermSize.Cols > 0 {
		cmd := fmt.Sprintf("resize-pane -t %s -x %d -y %d", paneID, input.TermSize.Cols, input.TermSize.Rows)
		if _, err := session.SendCommand(ctx, cmd); err != nil {
			return fmt.Errorf("tmux resize-pane: %w", err)
		}
	}
	return nil
}

func (tc *TmuxController) handleEvent(ev tmuxcc.Event) {
	var paneID string
	tc.WithLock(func() { paneID = tc.PaneID })
	switch v := ev.(type) {
	case tmuxcc.EventOutput:
		if v.PaneID != paneID || len(v.Data) == 0 {
			return
		}
		if err := HandleAppendBlockFile(tc.BlockId, wavebase.BlockFile_Term, v.Data); err != nil {
			log.Printf("[tmuxcc] block %s append error: %v", tc.BlockId, err)
		}
	case tmuxcc.EventExtendedOutput:
		if v.PaneID != paneID || len(v.Data) == 0 {
			return
		}
		if err := HandleAppendBlockFile(tc.BlockId, wavebase.BlockFile_Term, v.Data); err != nil {
			log.Printf("[tmuxcc] block %s append error: %v", tc.BlockId, err)
		}
	case tmuxcc.EventExit:
		tc.markDone()
	}
}

func (tc *TmuxController) markDone() {
	var statusChanged bool
	tc.WithLock(func() {
		if tc.ProcStatus != Status_Done {
			tc.ProcStatus = Status_Done
			statusChanged = true
		}
	})
	if statusChanged {
		tc.sendUpdate()
	}
}

func (tc *TmuxController) sendUpdate() {
	rtStatus := tc.GetRuntimeStatus()
	wps.Broker.Publish(wps.WaveEvent{
		Event: wps.Event_ControllerStatus,
		Scopes: []string{
			waveobj.MakeORef(waveobj.OType_Tab, tc.TabId).String(),
			waveobj.MakeORef(waveobj.OType_Block, tc.BlockId).String(),
		},
		Data: rtStatus,
	})
}
