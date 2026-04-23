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
	"github.com/wavetermdev/waveterm/pkg/panichandler"
	"github.com/wavetermdev/waveterm/pkg/remote"
	"github.com/wavetermdev/waveterm/pkg/remote/conncontroller"
	"github.com/wavetermdev/waveterm/pkg/tmuxcc"
	"github.com/wavetermdev/waveterm/pkg/utilds"
	"github.com/wavetermdev/waveterm/pkg/wavebase"
	"github.com/wavetermdev/waveterm/pkg/waveobj"
	"github.com/wavetermdev/waveterm/pkg/wcore"
	"github.com/wavetermdev/waveterm/pkg/wps"
	"github.com/wavetermdev/waveterm/pkg/wshrpc"
	"github.com/wavetermdev/waveterm/pkg/wstore"
)

// resolveFirstPane asks the tmux server for the first pane id in
// sessionName. Used when a block was created with only a session name
// (a widget click) — we pick the initial pane for the user. Retries a
// few times to dodge the tmux -CC startup race that can return an
// empty list mid-handshake.
func resolveFirstPane(session *tmuxcc.Session, sessionName string) (string, error) {
	cmdStr := fmt.Sprintf("list-panes -t %s -F %s", tmuxShellQuote(sessionName), tmuxShellQuote("#{pane_id}"))
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		qctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		lines, err := session.SendCommand(qctx, cmdStr)
		cancel()
		if err != nil {
			lastErr = err
		} else {
			for _, ln := range lines {
				if pid := strings.TrimSpace(ln); strings.HasPrefix(pid, "%") {
					return pid, nil
				}
			}
			lastErr = fmt.Errorf("no pane id in list-panes output (%d line(s))", len(lines))
		}
		time.Sleep(150 * time.Millisecond)
	}
	return "", lastErr
}

// tmuxShellQuote wraps s in single quotes for tmux command arg safety.
func tmuxShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// persistTmuxBlockMeta writes resolved handle/pane back to block meta
// so the next Start doesn't re-run the pane-discovery code path.
// Best-effort; errors are logged and swallowed.
func persistTmuxBlockMeta(blockID, handle, paneID string) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()
	ctx = waveobj.ContextWithUpdates(ctx)
	meta := waveobj.MetaMapType{
		waveobj.MetaKey_TmuxSessionHandle: handle,
		waveobj.MetaKey_TmuxPaneId:        paneID,
	}
	oref := waveobj.MakeORef(waveobj.OType_Block, blockID)
	if err := wstore.UpdateObjectMeta(ctx, oref, meta, false); err != nil {
		log.Printf("[tmuxcc] persist meta for block %s: %v", blockID, err)
		return
	}
	wcore.SendWaveObjUpdate(oref)
	updates := waveobj.ContextGetUpdatesRtn(ctx)
	wps.Broker.SendUpdateEvents(updates)
}

// isRemoteConn returns true if connName refers to a non-local SSH
// connection. Empty or "local*" means local.
func isRemoteConn(connName string) bool {
	if connName == "" {
		return false
	}
	return !conncontroller.IsLocalConnName(connName) && !conncontroller.IsWslConnName(connName)
}

// resolveSSHConn looks up the SSH connection referenced by connName.
// Returns an error if the connection isn't known/connected.
func resolveSSHConn(connName string) (*conncontroller.SSHConn, error) {
	opts, err := remote.ParseOpts(connName)
	if err != nil {
		return nil, fmt.Errorf("parse connection %q: %w", connName, err)
	}
	conn := conncontroller.MaybeGetConn(opts)
	if conn == nil {
		return nil, fmt.Errorf("no connection found for %q (connect first)", connName)
	}
	if conn.DeriveConnStatus().Status != conncontroller.Status_Connected {
		return nil, fmt.Errorf("connection %q not connected", connName)
	}
	return conn, nil
}

// parseCursorState parses the ";"-separated output of tmux's
// display-message '#{cursor_y};#{cursor_x};#{pane_height}' query.
// Returns (cursor_y, cursor_x, pane_height) — cursor coords zero-based
// from top of the pane.
func parseCursorState(s string) (int, int, int, bool) {
	parts := strings.Split(strings.TrimSpace(s), ";")
	if len(parts) < 3 {
		return 0, 0, 0, false
	}
	cy, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, 0, false
	}
	cx, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, 0, false
	}
	ph, err := strconv.Atoi(strings.TrimSpace(parts[2]))
	if err != nil {
		return 0, 0, 0, false
	}
	return cy, cx, ph, true
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

// sizeTmuxForBlock tells tmux the new size for this block's pane. When
// the window has sibling panes (orchestrator tracks >1 pane) we use
// resize-pane so siblings expand/shrink to fit — dragging a split in
// waveterm translates to exactly one pane changing, as the user
// expects. For single-pane windows resize-pane is a no-op, so we use
// resize-window to grow or shrink the whole window.
func (tc *TmuxController) sizeTmuxForBlock(ctx context.Context, session *tmuxcc.Session, handle, paneID string, rows, cols int) error {
	if rows <= 0 || cols <= 0 {
		return nil
	}
	paneCount := PaneCountForHandle(handle)
	var cmd string
	if paneCount > 1 {
		cmd = fmt.Sprintf("resize-pane -t %s -x %d -y %d", paneID, cols, rows)
	} else {
		cmd = fmt.Sprintf("resize-window -t %s -x %d -y %d", paneID, cols, rows)
	}
	if _, err := session.SendCommand(ctx, cmd); err != nil {
		return fmt.Errorf("tmux %s: %w", cmdHead(cmd), err)
	}
	return nil
}

func cmdHead(cmd string) string {
	if i := strings.IndexByte(cmd, ' '); i > 0 {
		return cmd[:i]
	}
	return cmd
}

func (tc *TmuxController) Start(ctx context.Context, blockMeta waveobj.MetaMapType, rtOpts *waveobj.RuntimeOpts, force bool) error {
	handle := blockMeta.GetString(waveobj.MetaKey_TmuxSessionHandle, "")
	sessionName := blockMeta.GetString(waveobj.MetaKey_TmuxSessionName, "")
	connName := blockMeta.GetString(waveobj.MetaKey_Connection, "")
	paneID := blockMeta.GetString(waveobj.MetaKey_TmuxPaneId, "")
	if paneID == "" && sessionName == "" {
		return fmt.Errorf("tmux block needs either %q or %q in meta",
			waveobj.MetaKey_TmuxPaneId, waveobj.MetaKey_TmuxSessionName)
	}
	var session *tmuxcc.Session
	if handle != "" {
		session = tmuxcc.GlobalManager().Get(handle)
	}
	// Handle in block meta is an in-memory hint that doesn't survive
	// wavesrv restart. Fall back to the stable session name plus
	// optional connection, which reattaches to the existing tmux
	// server session (via new-session -A -s) and registers a fresh
	// handle. When connName is set and non-local, use SSH.
	if session == nil && sessionName != "" {
		ensureCtx, ensureCancel := context.WithTimeout(context.Background(), tmuxSendTimeout)
		var newHandle string
		var newSession *tmuxcc.Session
		var err error
		if isRemoteConn(connName) {
			var conn *conncontroller.SSHConn
			conn, err = resolveSSHConn(connName)
			if err == nil {
				newHandle, newSession, err = tmuxcc.GlobalManager().EnsureRemoteSession(ensureCtx, conn, sessionName)
			}
		} else {
			newHandle, newSession, err = tmuxcc.GlobalManager().EnsureLocalSession(ensureCtx, sessionName)
		}
		ensureCancel()
		if err != nil {
			return fmt.Errorf("reattach tmux session %q (conn=%q): %w", sessionName, connName, err)
		}
		handle = newHandle
		session = newSession
	}
	// When the block was created with just a session name (a freshly
	// clicked tmux widget), resolve the first pane ID from tmux. Both
	// for the runtime and as a sticky setting: persist it back to
	// block meta so next Start skips the lookup.
	if paneID == "" && session != nil && sessionName != "" {
		resolvedPane, err := resolveFirstPane(session, sessionName)
		if err != nil {
			return fmt.Errorf("resolve first pane for session %q: %w", sessionName, err)
		}
		paneID = resolvedPane
		go persistTmuxBlockMeta(tc.BlockId, handle, paneID)
	}
	if session == nil {
		return fmt.Errorf("no tmux session for block (handle=%q name=%q)", handle, sessionName)
	}
	// Verify the pane still exists on the tmux server. list-panes with
	// an explicit -t is reliable even during tmux -CC startup (unlike
	// display-message, which can return status-line fallback data
	// during the handshake race).
	verifyCtx, cancelVerify := context.WithTimeout(context.Background(), tmuxSendTimeout)
	_, verifyErr := session.SendCommand(verifyCtx, fmt.Sprintf("list-panes -t %s", paneID))
	cancelVerify()
	if verifyErr != nil && strings.Contains(verifyErr.Error(), "can't find pane") {
		tc.writeStalePaneMessage(paneID, sessionName)
		tc.WithLock(func() {
			tc.ProcStatus = Status_Done
		})
		tc.sendUpdate()
		return nil
	}
	mkCtx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()
	if err := filestore.WFS.MakeFile(mkCtx, tc.BlockId, wavebase.BlockFile_Term, nil, wshrpc.FileOpts{MaxSize: DefaultTermMaxFileSize, Circular: true}); err != nil {
		log.Printf("[tmuxcc] block %s make term file: %v (continuing)", tc.BlockId, err)
	}
	// On reconnect the term file already holds the previous session's
	// scrollback. Truncate so the fresh capture-pane seed isn't
	// appended to stale content — the broadcast tells the frontend to
	// clear xterm before the new seed streams in.
	if err := HandleTruncateBlockFile(tc.BlockId); err != nil {
		log.Printf("[tmuxcc] block %s truncate term file: %v (continuing)", tc.BlockId, err)
	}
	// Order matters: resize → capture → subscribe. Resize first so the
	// captured buffer reflects the block's actual dimensions. Capture
	// before subscribe so we seed xterm with the pane's current visible
	// state without double-counting events. The tiny window between
	// capture and subscribe can drop output but the subsequent stream
	// will correct any drift.
	if rtOpts != nil && rtOpts.TermSize.Rows > 0 && rtOpts.TermSize.Cols > 0 {
		resizeCtx, cancelResize := context.WithTimeout(context.Background(), tmuxSendTimeout)
		if err := tc.sizeTmuxForBlock(resizeCtx, session, handle, paneID, rtOpts.TermSize.Rows, rtOpts.TermSize.Cols); err != nil {
			log.Printf("[tmuxcc] block %s initial resize: %v (continuing)", tc.BlockId, err)
		}
		cancelResize()
	}
	capCtx, cancelCap := context.WithTimeout(context.Background(), tmuxSendTimeout)
	// -S - pulls from the beginning of the scrollback history (default
	// is the visible pane only). -J joins wrapped lines, -e preserves
	// escape sequences, -p prints to stdout.
	capLines, err := session.SendCommand(capCtx, fmt.Sprintf("capture-pane -p -e -J -S - -t %s", paneID))
	cancelCap()
	if err != nil {
		log.Printf("[tmuxcc] block %s capture-pane: %v (continuing)", tc.BlockId, err)
	} else if len(capLines) > 0 {
		seed := strings.Join(capLines, "\r\n")
		// Position the cursor using RELATIVE moves from the end of the
		// seed. The last rendered line IS the pane's last visible row,
		// so we move up (pane_height - 1 - cursor_y) rows from it to
		// land on the real cursor row, then to absolute column
		// cursor_x+1. Absolute ESC[y;xH positioning would be wrong
		// when xterm's row count doesn't match the tmux pane's.
		curCtx, cancelCur := context.WithTimeout(context.Background(), tmuxSendTimeout)
		curLines, curErr := session.SendCommand(curCtx, fmt.Sprintf("display-message -p -t %s %s", paneID, strconv.Quote("#{cursor_y};#{cursor_x};#{pane_height}")))
		cancelCur()
		if curErr == nil && len(curLines) > 0 {
			if cy, cx, ph, ok := parseCursorState(curLines[0]); ok {
				rowsUp := (ph - 1) - cy
				if rowsUp < 0 {
					rowsUp = 0
				}
				seed += "\r"
				if rowsUp > 0 {
					seed += fmt.Sprintf("\x1b[%dA", rowsUp)
				}
				seed += fmt.Sprintf("\x1b[%dG", cx+1)
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
	// Force tmux to resend the current pane state as a full redraw.
	// Without this, xterm's state (seeded from capture-pane) drifts
	// from what tmux thinks when other clients or resizes mutate the
	// pane, producing visible artifacts. Best-effort — don't block
	// Start on errors.
	go func() {
		defer func() { panichandler.PanicHandler("tmuxcc.TmuxController.refresh", recover()) }()
		ctx, cancel := context.WithTimeout(context.Background(), tmuxSendTimeout)
		defer cancel()
		if _, err := session.SendCommand(ctx, "refresh-client -l"); err != nil {
			log.Printf("[tmuxcc] block %s refresh-client: %v", tc.BlockId, err)
		}
	}()
	if err := EnsureTmuxOrchestrator(handle, sessionName, tc.TabId, paneID, tc.BlockId); err != nil {
		log.Printf("[tmuxcc] block %s orchestrator register: %v (continuing)", tc.BlockId, err)
	}
	// Initial title from tmux window name — done best-effort so a
	// slow/failing query doesn't block the Start path.
	go tc.setInitialTitle(session, paneID)
	tc.sendUpdate()
	return nil
}

func (tc *TmuxController) writeStalePaneMessage(paneID, sessionName string) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()
	_ = filestore.WFS.MakeFile(ctx, tc.BlockId, wavebase.BlockFile_Term, nil, wshrpc.FileOpts{MaxSize: DefaultTermMaxFileSize, Circular: true})
	msg := fmt.Sprintf("\r\n\x1b[33m[tmux pane %s no longer exists in session %q — close this block to clean up]\x1b[0m\r\n", paneID, sessionName)
	if err := HandleAppendBlockFile(tc.BlockId, wavebase.BlockFile_Term, []byte(msg)); err != nil {
		log.Printf("[tmuxcc] block %s stale-pane message: %v", tc.BlockId, err)
	}
}

func (tc *TmuxController) setInitialTitle(session *tmuxcc.Session, paneID string) {
	defer func() { panichandler.PanicHandler("tmuxcc.TmuxController.setInitialTitle", recover()) }()
	ctx, cancel := context.WithTimeout(context.Background(), tmuxSendTimeout)
	defer cancel()
	lines, err := session.SendCommand(ctx, fmt.Sprintf("display-message -p -t %s %s", paneID, strconv.Quote("#{window_name}")))
	if err != nil {
		return
	}
	if len(lines) == 0 {
		return
	}
	title := strings.TrimSpace(lines[0])
	if title == "" {
		return
	}
	if err := SetTmuxBlockTitle(tc.BlockId, title); err != nil {
		log.Printf("[tmuxcc] block %s set initial title: %v", tc.BlockId, err)
	}
}

func (tc *TmuxController) Stop(graceful bool, newStatus string, destroy bool) {
	var sub *tmuxcc.Subscription
	var session *tmuxcc.Session
	var paneID string
	var handle string
	var statusChanged bool
	tc.WithLock(func() {
		sub = tc.Subscription
		session = tc.Session
		paneID = tc.PaneID
		handle = tc.SessionHandle
		tc.Subscription = nil
		if newStatus != tc.ProcStatus {
			tc.ProcStatus = newStatus
			statusChanged = true
		}
	})
	if sub != nil {
		sub.Unsubscribe()
	}
	// On destroy, propagate the block close to tmux so the pane
	// disappears too. Also drop the pane from the orchestrator's map
	// so the subsequent layout-change doesn't race to delete this
	// already-being-deleted block.
	if destroy && session != nil && paneID != "" {
		if handle != "" {
			ForgetOrchestratorPane(handle, paneID)
		}
		go func() {
			defer func() { panichandler.PanicHandler("tmuxcc.TmuxController.killPane", recover()) }()
			killCtx, cancel := context.WithTimeout(context.Background(), tmuxSendTimeout)
			defer cancel()
			if _, err := session.SendCommand(killCtx, fmt.Sprintf("kill-pane -t %s", paneID)); err != nil {
				log.Printf("[tmuxcc] kill-pane %s: %v", paneID, err)
			}
		}()
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
	var handle string
	tc.WithLock(func() {
		session = tc.Session
		paneID = tc.PaneID
		handle = tc.SessionHandle
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
		if err := tc.sizeTmuxForBlock(ctx, session, handle, paneID, input.TermSize.Rows, input.TermSize.Cols); err != nil {
			return err
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
