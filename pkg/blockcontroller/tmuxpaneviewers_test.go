// Copyright 2026, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package blockcontroller

import "testing"

func TestPaneViewerSingleViewerIsDriver(t *testing.T) {
	defer resetPaneViewers()
	registerPaneViewer("h1", "%1", "blockA")
	if !isPaneDriver("h1", "%1", "blockA") {
		t.Fatal("solo viewer should be driver")
	}
}

func TestPaneViewerFirstWins(t *testing.T) {
	defer resetPaneViewers()
	registerPaneViewer("h1", "%1", "blockA")
	registerPaneViewer("h1", "%1", "blockB")
	if !isPaneDriver("h1", "%1", "blockA") {
		t.Fatal("blockA should remain driver after blockB joins")
	}
	if isPaneDriver("h1", "%1", "blockB") {
		t.Fatal("blockB should not be driver while blockA is registered")
	}
}

func TestPaneViewerPromotionOnUnregister(t *testing.T) {
	defer resetPaneViewers()
	registerPaneViewer("h1", "%1", "blockA")
	registerPaneViewer("h1", "%1", "blockB")
	unregisterPaneViewer("h1", "%1", "blockA")
	if !isPaneDriver("h1", "%1", "blockB") {
		t.Fatal("blockB should be promoted to driver after blockA unregisters")
	}
}

func TestPaneViewerEmptyRegistryAllowsDrive(t *testing.T) {
	defer resetPaneViewers()
	if !isPaneDriver("h1", "%1", "blockA") {
		t.Fatal("unregistered block should be allowed to drive (safe default)")
	}
}

func TestPaneViewerKeyedByHandleAndPane(t *testing.T) {
	defer resetPaneViewers()
	registerPaneViewer("h1", "%1", "blockA")
	registerPaneViewer("h1", "%2", "blockB")
	registerPaneViewer("h2", "%1", "blockC")
	if !isPaneDriver("h1", "%1", "blockA") {
		t.Fatal("blockA driver of (h1,%1)")
	}
	if !isPaneDriver("h1", "%2", "blockB") {
		t.Fatal("blockB driver of (h1,%2)")
	}
	if !isPaneDriver("h2", "%1", "blockC") {
		t.Fatal("blockC driver of (h2,%1)")
	}
}

func TestPaneViewerIdempotentRegister(t *testing.T) {
	defer resetPaneViewers()
	registerPaneViewer("h1", "%1", "blockA")
	registerPaneViewer("h1", "%1", "blockA")
	registerPaneViewer("h1", "%1", "blockB")
	unregisterPaneViewer("h1", "%1", "blockA")
	if !isPaneDriver("h1", "%1", "blockB") {
		t.Fatal("blockB should be sole driver after one unregister of duplicated blockA")
	}
}

func resetPaneViewers() {
	paneViewersMu.Lock()
	defer paneViewersMu.Unlock()
	paneViewers = make(map[paneViewerKey][]string)
}
