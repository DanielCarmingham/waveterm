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

// createNewWindowPane spawns a new tmux window in sessionName and
// returns its (only) pane id. Used when a block is created with just a
// session name (the tmux widget click) — each widget click owns the
// pane it spawns, so closing the block can safely kill the pane
// without disturbing other terminals already running in the session.
//
// Adopting a pre-existing pane (the earlier resolveFirstPane behavior)
// was destructive: closing the block would kill someone else's running
// shell. Always create our own.
func createNewWindowPane(session *tmuxcc.Session, sessionName string) (string, error) {
	cmdStr := fmt.Sprintf("new-window -t %s -P -F %s", tmuxShellQuote(sessionName), tmuxShellQuote("#{pane_id}"))
	qctx, cancel := context.WithTimeout(context.Background(), tmuxSendTimeout)
	defer cancel()
	lines, err := session.SendCommand(qctx, cmdStr)
	if err != nil {
		return "", fmt.Errorf("new-window: %w", err)
	}
	for _, ln := range lines {
		if pid := strings.TrimSpace(ln); strings.HasPrefix(pid, "%") {
			return pid, nil
		}
	}
	return "", fmt.Errorf("new-window: no pane id in output (%d line(s))", len(lines))
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

	LastPaneRows int
	LastPaneCols int
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
	if !isPaneDriver(handle, paneID, tc.BlockId) {
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
	// clicked tmux widget), spawn a new tmux window and bind to its
	// pane. Each widget click owns the pane it creates so subsequent
	// block-destroy can kill-pane without taking down unrelated
	// terminals running in the same session. The new pane id is
	// persisted back to block meta so a wavesrv restart re-attaches
	// instead of spawning yet another window.
	if paneID == "" && session != nil && sessionName != "" {
		newPane, err := createNewWindowPane(session, sessionName)
		if err != nil {
			return fmt.Errorf("create new tmux window for session %q: %w", sessionName, err)
		}
		paneID = newPane
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
	// Register as a viewer of this pane before any resize call so
	// driver election picks the right block. First-registered wins; a
	// later block opening the same pane will be a non-driver and skip
	// resize-pane.
	registerPaneViewer(handle, paneID, tc.BlockId)
	// Seed the pane-size meta from the current tmux state so the
	// frontend has a value to compare against before any
	// %layout-change event arrives. tmux fires layout-change only on
	// actual changes — for idle panes we'd otherwise see undefined
	// pane size.
	go tc.publishInitialPaneSize(session, paneID)
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
	if handle != "" && paneID != "" {
		unregisterPaneViewer(handle, paneID, tc.BlockId)
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
	case tmuxcc.EventLayoutChange:
		tc.handleLayoutChange(paneID, v.Layout)
	case tmuxcc.EventExit:
		tc.handleExit(v.Reason)
	}
}

// handleExit is called when tmux sends %exit (server killed, session
// destroyed, client detached). Surfaces a visible banner in the block,
// clears the stale pane id from meta so a subsequent restart spawns a
// fresh window pane via the session-name path, and marks the controller
// done so the header refresh-button appears.
func (tc *TmuxController) handleExit(reason string) {
	msg := "tmux session ended"
	if r := strings.TrimSpace(reason); r != "" {
		msg = fmt.Sprintf("tmux session ended: %s", r)
	}
	banner := fmt.Sprintf("\r\n\x1b[33m[%s — click the refresh icon to reconnect]\x1b[0m\r\n", msg)
	if err := HandleAppendBlockFile(tc.BlockId, wavebase.BlockFile_Term, []byte(banner)); err != nil {
		log.Printf("[tmuxcc] block %s exit banner: %v", tc.BlockId, err)
	}
	tc.clearStalePaneMeta()
	tc.markDone()
}

// clearStalePaneMeta wipes the tmux:paneid value from block meta. After
// %exit the recorded pane id is no longer valid; clearing it forces the
// next Start to create a new window pane via the session-name branch
// instead of running the stale-pane verification path.
func (tc *TmuxController) clearStalePaneMeta() {
	defer func() { panichandler.PanicHandler("tmuxcc.clearStalePaneMeta", recover()) }()
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()
	ctx = waveobj.ContextWithUpdates(ctx)
	oref := waveobj.MakeORef(waveobj.OType_Block, tc.BlockId)
	meta := waveobj.MetaMapType{
		waveobj.MetaKey_TmuxPaneId: "",
	}
	if err := wstore.UpdateObjectMeta(ctx, oref, meta, false); err != nil {
		log.Printf("[tmuxcc] block %s clear pane meta: %v", tc.BlockId, err)
		return
	}
	wcore.SendWaveObjUpdate(oref)
	updates := waveobj.ContextGetUpdatesRtn(ctx)
	wps.Broker.SendUpdateEvents(updates)
}

// publishInitialPaneSize queries tmux for our pane's current
// dimensions and persists them to block meta. Called from Start so the
// frontend's crosshatch overlay has a value to render against even
// when %layout-change won't fire for our idle pane.
func (tc *TmuxController) publishInitialPaneSize(session *tmuxcc.Session, paneID string) {
	defer func() { panichandler.PanicHandler("tmuxcc.publishInitialPaneSize", recover()) }()
	ctx, cancel := context.WithTimeout(context.Background(), tmuxSendTimeout)
	defer cancel()
	lines, err := session.SendCommand(ctx, fmt.Sprintf("list-panes -t %s -F %s", paneID, strconv.Quote("#{pane_height} #{pane_width}")))
	if err != nil || len(lines) == 0 {
		return
	}
	var rows, cols int
	if _, err := fmt.Sscanf(strings.TrimSpace(lines[0]), "%d %d", &rows, &cols); err != nil {
		return
	}
	if rows <= 0 || cols <= 0 {
		return
	}
	var changed bool
	tc.WithLock(func() {
		if tc.LastPaneRows != rows || tc.LastPaneCols != cols {
			tc.LastPaneRows = rows
			tc.LastPaneCols = cols
			changed = true
		}
	})
	if !changed {
		return
	}
	persistCtx, persistCancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer persistCancel()
	persistCtx = waveobj.ContextWithUpdates(persistCtx)
	oref := waveobj.MakeORef(waveobj.OType_Block, tc.BlockId)
	meta := waveobj.MetaMapType{
		waveobj.MetaKey_TmuxPaneRows: rows,
		waveobj.MetaKey_TmuxPaneCols: cols,
	}
	if err := wstore.UpdateObjectMeta(persistCtx, oref, meta, false); err != nil {
		log.Printf("[tmuxcc] block %s persist initial pane size: %v", tc.BlockId, err)
		return
	}
	wcore.SendWaveObjUpdate(oref)
	updates := waveobj.ContextGetUpdatesRtn(persistCtx)
	wps.Broker.SendUpdateEvents(updates)
}

// handleLayoutChange parses the tmux layout string for our pane's
// current size and persists it to block meta as tmux:panerows /
// tmux:panecols. The frontend uses these to render a crosshatch
// overlay when our xterm dimensions exceed the actual pane size
// (which happens to non-driver blocks viewing a shared pane).
func (tc *TmuxController) handleLayoutChange(paneID, layoutStr string) {
	if paneID == "" {
		return
	}
	tree, err := tmuxcc.ParseLayout(layoutStr)
	if err != nil {
		return
	}
	leaf := tree.FindPane(paneID)
	if leaf == nil {
		return
	}
	rows := leaf.Height
	cols := leaf.Width
	var changed bool
	tc.WithLock(func() {
		if tc.LastPaneRows != rows || tc.LastPaneCols != cols {
			tc.LastPaneRows = rows
			tc.LastPaneCols = cols
			changed = true
		}
	})
	if !changed {
		return
	}
	go func() {
		defer func() { panichandler.PanicHandler("tmuxcc.handleLayoutChange", recover()) }()
		ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
		defer cancel()
		ctx = waveobj.ContextWithUpdates(ctx)
		oref := waveobj.MakeORef(waveobj.OType_Block, tc.BlockId)
		meta := waveobj.MetaMapType{
			waveobj.MetaKey_TmuxPaneRows: rows,
			waveobj.MetaKey_TmuxPaneCols: cols,
		}
		if err := wstore.UpdateObjectMeta(ctx, oref, meta, false); err != nil {
			log.Printf("[tmuxcc] block %s persist pane size: %v", tc.BlockId, err)
			return
		}
		wcore.SendWaveObjUpdate(oref)
		updates := waveobj.ContextGetUpdatesRtn(ctx)
		wps.Broker.SendUpdateEvents(updates)
	}()
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
