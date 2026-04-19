// Copyright 2026, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package blockcontroller

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/wavetermdev/waveterm/pkg/panichandler"
	"github.com/wavetermdev/waveterm/pkg/tmuxcc"
	"github.com/wavetermdev/waveterm/pkg/waveobj"
	"github.com/wavetermdev/waveterm/pkg/wcore"
	"github.com/wavetermdev/waveterm/pkg/wps"
)

// TmuxOrchestrator mirrors one tmux window's pane layout into a
// waveterm tab. It subscribes to a tmux session's events; on each
// %layout-change for the mirrored window, it diffs the pane set
// against the blocks it manages and creates/removes blocks to match.
//
// M2 scope: one window per orchestrator (whichever window contains the
// block that was explicitly created by the user). New panes land as a
// splitright of the first surviving existing block — faithful to
// tmux's geometry is a later polish pass.
type TmuxOrchestrator struct {
	mu sync.Mutex

	handle      string
	sessionName string // stable identity; survives wavesrv restart
	session     *tmuxcc.Session
	tabID       string
	windowID    string // set on first layout-change containing our seed pane

	paneBlocks map[string]string // tmux paneID -> waveterm blockID

	subscription *tmuxcc.Subscription
}

var (
	orchestratorMu sync.Mutex
	orchestrators  = make(map[string]*TmuxOrchestrator) // handle -> orchestrator
)

// EnsureTmuxOrchestrator registers a pane+block pair for a tmux
// session, creating an orchestrator if one doesn't already exist for
// that session. Call this from every TmuxController's Start so the
// session-wide orchestrator always knows about every materialized
// block — including blocks created externally via CreateBlock.
func EnsureTmuxOrchestrator(handle string, sessionName string, tabID string, paneID string, blockID string) error {
	if handle == "" || paneID == "" || blockID == "" {
		return fmt.Errorf("tmuxorchestrator: EnsureTmuxOrchestrator requires handle, paneID, blockID")
	}
	orchestratorMu.Lock()
	existing, ok := orchestrators[handle]
	orchestratorMu.Unlock()
	if ok {
		existing.mu.Lock()
		existing.paneBlocks[paneID] = blockID
		if existing.sessionName == "" {
			existing.sessionName = sessionName
		}
		existing.mu.Unlock()
		return nil
	}
	if tabID == "" {
		return fmt.Errorf("tmuxorchestrator: tabID required when creating orchestrator")
	}
	return StartTmuxOrchestrator(handle, sessionName, tabID, paneID, blockID)
}

// StartTmuxOrchestrator registers a seed pane+block pair and begins
// watching the tmux session for layout changes. Idempotent per handle:
// a second call replaces the prior orchestrator for that session.
func StartTmuxOrchestrator(handle string, sessionName string, tabID string, seedPaneID string, seedBlockID string) error {
	if handle == "" || tabID == "" || seedPaneID == "" || seedBlockID == "" {
		return fmt.Errorf("tmuxorchestrator: StartTmuxOrchestrator requires all args")
	}
	session := tmuxcc.GlobalManager().Get(handle)
	if session == nil {
		return fmt.Errorf("tmuxorchestrator: no tmux session with handle %q", handle)
	}
	orchestratorMu.Lock()
	if prev, ok := orchestrators[handle]; ok {
		prev.Stop()
	}
	o := &TmuxOrchestrator{
		handle:      handle,
		sessionName: sessionName,
		session:     session,
		tabID:       tabID,
		paneBlocks:  map[string]string{seedPaneID: seedBlockID},
	}
	orchestrators[handle] = o
	orchestratorMu.Unlock()
	sub, err := tmuxcc.GlobalManager().Subscribe(handle, o.handleEvent)
	if err != nil {
		orchestratorMu.Lock()
		delete(orchestrators, handle)
		orchestratorMu.Unlock()
		return err
	}
	o.mu.Lock()
	o.subscription = sub
	o.mu.Unlock()
	go o.bootstrapLayout(seedPaneID)
	return nil
}

// Stop detaches the orchestrator's subscription. The managed blocks
// are left in place; only the tmux→waveterm sync is halted.
func (o *TmuxOrchestrator) Stop() {
	o.mu.Lock()
	sub := o.subscription
	o.subscription = nil
	o.mu.Unlock()
	if sub != nil {
		sub.Unsubscribe()
	}
}

func (o *TmuxOrchestrator) handleEvent(ev tmuxcc.Event) {
	defer func() { panichandler.PanicHandler("tmuxorchestrator.handleEvent", recover()) }()
	switch v := ev.(type) {
	case tmuxcc.EventLayoutChange:
		o.onLayoutChange(v.WindowID, v.Layout)
	case tmuxcc.EventWindowClose:
		o.onWindowClose(v.WindowID)
	}
}

// bootstrapLayout runs once after Start: asks tmux for the current
// layout of our seed pane's window so we pick up any panes that
// already existed before the orchestrator registered.
func (o *TmuxOrchestrator) bootstrapLayout(seedPaneID string) {
	defer func() { panichandler.PanicHandler("tmuxorchestrator.bootstrapLayout", recover()) }()
	// list-panes -a -F "<window_id> <pane_id> <window_layout>" gives us
	// window id for any pane and the window's current layout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lines, err := o.session.SendCommand(ctx, fmt.Sprintf("list-panes -a -F %s", `"#{window_id} #{pane_id} #{window_layout}"`))
	if err != nil {
		log.Printf("[tmuxorchestrator] bootstrap list-panes: %v", err)
		return
	}
	for _, line := range lines {
		var windowID, paneID, layout string
		_, err := fmt.Sscanf(line, "%s %s %s", &windowID, &paneID, &layout)
		if err != nil {
			continue
		}
		if paneID == seedPaneID {
			o.onLayoutChange(windowID, layout)
			return
		}
	}
}

func (o *TmuxOrchestrator) onLayoutChange(windowID, layoutStr string) {
	tree, err := tmuxcc.ParseLayout(layoutStr)
	if err != nil {
		log.Printf("[tmuxorchestrator] parse layout %q: %v", layoutStr, err)
		return
	}
	panes := tree.Panes()

	o.mu.Lock()
	// Claim a window: the first layout-change that mentions our seed
	// pane wins. After that we only process our window's changes.
	if o.windowID == "" {
		for _, p := range panes {
			if _, ok := o.paneBlocks[p]; ok {
				o.windowID = windowID
				break
			}
		}
	}
	if o.windowID != windowID {
		o.mu.Unlock()
		return
	}

	current := make(map[string]bool, len(panes))
	for _, p := range panes {
		current[p] = true
	}
	var fallbackAnchor string
	for p, bid := range o.paneBlocks {
		if current[p] {
			fallbackAnchor = bid
			break
		}
	}
	var newPanes []string
	for _, p := range panes {
		if _, ok := o.paneBlocks[p]; !ok {
			newPanes = append(newPanes, p)
		}
	}
	var gonePanes []string
	for p := range o.paneBlocks {
		if !current[p] {
			gonePanes = append(gonePanes, p)
		}
	}
	paneBlocksCopy := make(map[string]string, len(o.paneBlocks))
	for p, b := range o.paneBlocks {
		paneBlocksCopy[p] = b
	}
	o.mu.Unlock()

	for _, newPane := range newPanes {
		anchorBlockID := fallbackAnchor
		splitType := wcore.LayoutActionDataType_SplitHorizontal
		position := "after"
		if info, ok := tree.FindSplitInfo(newPane); ok {
			if bid, found := paneBlocksCopy[info.Anchor]; found && bid != "" {
				anchorBlockID = bid
			}
			if info.Split == "v" {
				splitType = wcore.LayoutActionDataType_SplitVertical
			}
			if info.Position == "before" {
				position = "before"
			}
		}
		blockID, err := o.createBlockForPane(newPane, anchorBlockID, splitType, position)
		if err != nil {
			log.Printf("[tmuxorchestrator] create block for pane %s: %v", newPane, err)
			continue
		}
		o.mu.Lock()
		o.paneBlocks[newPane] = blockID
		o.mu.Unlock()
		paneBlocksCopy[newPane] = blockID
		if fallbackAnchor == "" {
			fallbackAnchor = blockID
		}
	}
	for _, gonePane := range gonePanes {
		o.mu.Lock()
		blockID := o.paneBlocks[gonePane]
		delete(o.paneBlocks, gonePane)
		o.mu.Unlock()
		if blockID == "" {
			continue
		}
		if err := o.deleteBlock(blockID); err != nil {
			log.Printf("[tmuxorchestrator] delete block %s: %v", blockID, err)
		}
	}
}

func (o *TmuxOrchestrator) onWindowClose(windowID string) {
	o.mu.Lock()
	if o.windowID != windowID {
		o.mu.Unlock()
		return
	}
	blocks := make([]string, 0, len(o.paneBlocks))
	for _, bid := range o.paneBlocks {
		blocks = append(blocks, bid)
	}
	o.paneBlocks = nil
	o.mu.Unlock()
	for _, bid := range blocks {
		if err := o.deleteBlock(bid); err != nil {
			log.Printf("[tmuxorchestrator] delete block %s on window-close: %v", bid, err)
		}
	}
}

func (o *TmuxOrchestrator) createBlockForPane(paneID, anchorBlockID, splitType, position string) (string, error) {
	meta := waveobj.MetaMapType{
		waveobj.MetaKey_View:              "term",
		waveobj.MetaKey_Controller:        "tmux",
		waveobj.MetaKey_TmuxSessionHandle: o.handle,
		waveobj.MetaKey_TmuxSessionName:   o.sessionName,
		waveobj.MetaKey_TmuxPaneId:        paneID,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = waveobj.ContextWithUpdates(ctx)
	blockData, err := wcore.CreateBlock(ctx, o.tabID, &waveobj.BlockDef{Meta: meta}, &waveobj.RuntimeOpts{})
	if err != nil {
		return "", fmt.Errorf("create block: %w", err)
	}
	var action waveobj.LayoutActionData
	if anchorBlockID != "" {
		if splitType == "" {
			splitType = wcore.LayoutActionDataType_SplitHorizontal
		}
		if position == "" {
			position = "after"
		}
		action = waveobj.LayoutActionData{
			ActionType:    splitType,
			BlockId:       blockData.OID,
			TargetBlockId: anchorBlockID,
			Position:      position,
		}
	} else {
		action = waveobj.LayoutActionData{
			ActionType: wcore.LayoutActionDataType_Insert,
			BlockId:    blockData.OID,
		}
	}
	if err := wcore.QueueLayoutActionForTab(ctx, o.tabID, action); err != nil {
		return "", fmt.Errorf("queue layout action: %w", err)
	}
	updates := waveobj.ContextGetUpdatesRtn(ctx)
	wps.Broker.SendUpdateEvents(updates)
	return blockData.OID, nil
}

func (o *TmuxOrchestrator) deleteBlock(blockID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = waveobj.ContextWithUpdates(ctx)
	if err := wcore.DeleteBlock(ctx, blockID, false); err != nil {
		return err
	}
	if err := wcore.QueueLayoutActionForTab(ctx, o.tabID, waveobj.LayoutActionData{
		ActionType: wcore.LayoutActionDataType_Remove,
		BlockId:    blockID,
	}); err != nil {
		return err
	}
	updates := waveobj.ContextGetUpdatesRtn(ctx)
	wps.Broker.SendUpdateEvents(updates)
	return nil
}
