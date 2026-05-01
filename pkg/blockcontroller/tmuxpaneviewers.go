// Copyright 2026, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package blockcontroller

import "sync"

// tmux's pane has exactly one size at a time; if multiple waveterm
// blocks view the same pane at different xterm sizes, last-writer-wins
// resizes produce visible chaos. The pane-viewer registry elects a
// single "driver" block per (handle, paneID) — the first to register.
// Only the driver issues resize-pane / resize-window. Non-drivers
// passively render whatever size tmux reports; visible mismatch is the
// frontend's problem (M6 crosshatch overlay).

type paneViewerKey struct {
	Handle string
	PaneID string
}

var (
	paneViewersMu sync.Mutex
	paneViewers   = make(map[paneViewerKey][]string)
)

// registerPaneViewer adds blockID to the viewer list for (handle,
// paneID). Idempotent. The first-registered block becomes the driver.
func registerPaneViewer(handle, paneID, blockID string) {
	if handle == "" || paneID == "" || blockID == "" {
		return
	}
	key := paneViewerKey{Handle: handle, PaneID: paneID}
	paneViewersMu.Lock()
	defer paneViewersMu.Unlock()
	for _, b := range paneViewers[key] {
		if b == blockID {
			return
		}
	}
	paneViewers[key] = append(paneViewers[key], blockID)
}

// unregisterPaneViewer removes blockID from the viewer list for
// (handle, paneID). If blockID was the driver, the next viewer in the
// list is promoted on the next isPaneDriver query.
func unregisterPaneViewer(handle, paneID, blockID string) {
	if handle == "" || paneID == "" || blockID == "" {
		return
	}
	key := paneViewerKey{Handle: handle, PaneID: paneID}
	paneViewersMu.Lock()
	defer paneViewersMu.Unlock()
	list := paneViewers[key]
	for i, b := range list {
		if b == blockID {
			paneViewers[key] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(paneViewers[key]) == 0 {
		delete(paneViewers, key)
	}
}

// isPaneDriver returns true if blockID is the elected driver for
// (handle, paneID), or if the registry has no entries (safe default —
// solo viewer always drives).
func isPaneDriver(handle, paneID, blockID string) bool {
	if handle == "" || paneID == "" || blockID == "" {
		return true
	}
	key := paneViewerKey{Handle: handle, PaneID: paneID}
	paneViewersMu.Lock()
	defer paneViewersMu.Unlock()
	list := paneViewers[key]
	if len(list) == 0 {
		return true
	}
	return list[0] == blockID
}
