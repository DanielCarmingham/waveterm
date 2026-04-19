// Copyright 2026, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package tmuxcc_test

import (
	"reflect"
	"testing"

	"github.com/wavetermdev/waveterm/pkg/tmuxcc"
)

func TestParseLayoutSinglePane(t *testing.T) {
	t.Parallel()
	n, err := tmuxcc.ParseLayout("bb62,80x24,0,0,0")
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if !n.IsLeaf() {
		t.Fatalf("expected leaf, got %#v", n)
	}
	if n.PaneID != "%0" {
		t.Fatalf("PaneID = %q, want %%0", n.PaneID)
	}
	if n.Width != 80 || n.Height != 24 {
		t.Fatalf("size = %dx%d, want 80x24", n.Width, n.Height)
	}
}

func TestParseLayoutHorizontalSplit(t *testing.T) {
	t.Parallel()
	n, err := tmuxcc.ParseLayout("7e31,80x24,0,0{40x24,0,0,0,40x24,40,0,1}")
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if n.IsLeaf() {
		t.Fatalf("expected container, got leaf")
	}
	if n.Split != "h" {
		t.Fatalf("Split = %q, want h", n.Split)
	}
	if len(n.Children) != 2 {
		t.Fatalf("want 2 children, got %d", len(n.Children))
	}
	if !reflect.DeepEqual(n.Panes(), []string{"%0", "%1"}) {
		t.Fatalf("Panes = %v, want [%%0 %%1]", n.Panes())
	}
	if n.Children[0].Width != 40 || n.Children[1].X != 40 {
		t.Fatalf("geometry off: %#v / %#v", n.Children[0], n.Children[1])
	}
}

func TestParseLayoutVerticalSplit(t *testing.T) {
	t.Parallel()
	n, err := tmuxcc.ParseLayout("7e31,80x24,0,0[80x12,0,0,0,80x12,0,12,1]")
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if n.Split != "v" {
		t.Fatalf("Split = %q, want v", n.Split)
	}
	if len(n.Children) != 2 {
		t.Fatalf("want 2 children, got %d", len(n.Children))
	}
	if n.Children[0].PaneID != "%0" || n.Children[1].PaneID != "%1" {
		t.Fatalf("panes = %q,%q", n.Children[0].PaneID, n.Children[1].PaneID)
	}
	if n.Children[1].Y != 12 {
		t.Fatalf("second child Y = %d, want 12", n.Children[1].Y)
	}
}

func TestParseLayoutNested(t *testing.T) {
	t.Parallel()
	// outer horizontal: left pane %0, right is vertical {top %1, bottom %2}
	n, err := tmuxcc.ParseLayout("abcd,80x24,0,0{40x24,0,0,0,40x24,40,0[40x12,40,0,1,40x12,40,12,2]}")
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if n.Split != "h" || len(n.Children) != 2 {
		t.Fatalf("outer: Split=%q children=%d", n.Split, len(n.Children))
	}
	if n.Children[0].PaneID != "%0" {
		t.Fatalf("left pane = %q, want %%0", n.Children[0].PaneID)
	}
	right := n.Children[1]
	if right.Split != "v" || len(right.Children) != 2 {
		t.Fatalf("right subtree: Split=%q children=%d", right.Split, len(right.Children))
	}
	if !reflect.DeepEqual(n.Panes(), []string{"%0", "%1", "%2"}) {
		t.Fatalf("Panes = %v", n.Panes())
	}
}

func TestParseLayoutThreePanesHorizontal(t *testing.T) {
	t.Parallel()
	n, err := tmuxcc.ParseLayout("abcd,90x24,0,0{30x24,0,0,0,30x24,30,0,1,30x24,60,0,2}")
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(n.Children) != 3 {
		t.Fatalf("want 3 children, got %d", len(n.Children))
	}
	if !reflect.DeepEqual(n.Panes(), []string{"%0", "%1", "%2"}) {
		t.Fatalf("Panes = %v", n.Panes())
	}
}

func TestFindSplitInfoHorizontal(t *testing.T) {
	t.Parallel()
	n, _ := tmuxcc.ParseLayout("abcd,80x24,0,0{40x24,0,0,0,40x24,40,0,1}")
	info, ok := n.FindSplitInfo("%1")
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if info.Split != "h" || info.Anchor != "%0" || info.Position != "after" {
		t.Fatalf("got %#v", info)
	}
}

func TestFindSplitInfoVertical(t *testing.T) {
	t.Parallel()
	n, _ := tmuxcc.ParseLayout("abcd,80x24,0,0[80x12,0,0,0,80x12,0,12,1]")
	info, ok := n.FindSplitInfo("%1")
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if info.Split != "v" || info.Anchor != "%0" || info.Position != "after" {
		t.Fatalf("got %#v", info)
	}
}

func TestFindSplitInfoNestedVertical(t *testing.T) {
	t.Parallel()
	// outer h: %0 | (v: %1, %2)
	n, _ := tmuxcc.ParseLayout("abcd,80x24,0,0{40x24,0,0,0,40x24,40,0[40x12,40,0,1,40x12,40,12,2]}")
	info, ok := n.FindSplitInfo("%2")
	if !ok {
		t.Fatalf("expected ok=true")
	}
	if info.Split != "v" || info.Anchor != "%1" || info.Position != "after" {
		t.Fatalf("got %#v", info)
	}
}

func TestParseLayoutViaNotification(t *testing.T) {
	t.Parallel()
	ev, err := tmuxcc.ParseNotification("%layout-change @1 7e31,80x24,0,0{40x24,0,0,0,40x24,40,0,1} 7e31,80x24,0,0{40x24,0,0,0,40x24,40,0,1} 1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	lc, ok := ev.(tmuxcc.EventLayoutChange)
	if !ok {
		t.Fatalf("expected EventLayoutChange, got %T", ev)
	}
	tree, err := tmuxcc.ParseLayout(lc.Layout)
	if err != nil {
		t.Fatalf("parse layout: %v", err)
	}
	if !reflect.DeepEqual(tree.Panes(), []string{"%0", "%1"}) {
		t.Fatalf("Panes = %v", tree.Panes())
	}
}
