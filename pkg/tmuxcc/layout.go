// Copyright 2026, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package tmuxcc

import (
	"fmt"
	"strconv"
	"strings"
)

// LayoutNode is one node of a parsed tmux layout tree. A leaf holds a
// pane id (the numeric part of %<id>); a container holds children
// joined by Split. Size and Position are in tmux's cell coordinate
// space, which may not exactly match the waveterm block's pixel
// coordinates.
type LayoutNode struct {
	Width, Height int
	X, Y          int

	// For leaves
	PaneID string // e.g. "%42" — empty for containers

	// For containers
	Split    string        // "", "h" (left-right), "v" (top-bottom)
	Children []*LayoutNode
}

// IsLeaf reports whether the node represents a single tmux pane.
func (n *LayoutNode) IsLeaf() bool { return n.PaneID != "" }

// Panes returns every leaf pane id in document order.
func (n *LayoutNode) Panes() []string {
	var out []string
	n.walkPanes(&out)
	return out
}

func (n *LayoutNode) walkPanes(out *[]string) {
	if n == nil {
		return
	}
	if n.IsLeaf() {
		*out = append(*out, n.PaneID)
		return
	}
	for _, c := range n.Children {
		c.walkPanes(out)
	}
}

// ParseLayout parses a tmux layout string like
// "7e31,80x24,0,0{40x24,0,0,0,40x24,40,0,1}". The leading 4-char
// checksum is discarded.
func ParseLayout(s string) (*LayoutNode, error) {
	if len(s) < 5 || s[4] != ',' {
		return nil, fmt.Errorf("tmuxcc: layout missing checksum prefix: %q", s)
	}
	node, rest, err := parseCell(s[5:])
	if err != nil {
		return nil, err
	}
	if rest != "" {
		return nil, fmt.Errorf("tmuxcc: trailing input after layout: %q", rest)
	}
	return node, nil
}

// parseCell consumes one cell (leaf or container) from the start of s
// and returns the node plus the remainder of s AT the next delimiter
// (',' between siblings, closing '}' or ']', or end-of-string).
func parseCell(s string) (*LayoutNode, string, error) {
	w, rest, err := parseIntUpTo(s, "x")
	if err != nil {
		return nil, s, fmt.Errorf("layout width: %w", err)
	}
	rest = rest[1:] // skip 'x'
	h, rest, err := parseIntUpTo(rest, ",")
	if err != nil {
		return nil, s, fmt.Errorf("layout height: %w", err)
	}
	rest = rest[1:] // skip ','
	x, rest, err := parseIntUpTo(rest, ",")
	if err != nil {
		return nil, s, fmt.Errorf("layout x: %w", err)
	}
	rest = rest[1:] // skip ','
	// y is followed by ',' (leaf — pane id next), '{' / '[' (container),
	// or a cell terminator (',', '}', ']', EOF) for the uncommon bare
	// leaf form.
	y, rest, err := parseIntUpTo(rest, ",{[]")
	if err != nil {
		return nil, s, fmt.Errorf("layout y: %w", err)
	}
	node := &LayoutNode{Width: w, Height: h, X: x, Y: y}
	if rest == "" {
		return node, "", nil
	}
	switch rest[0] {
	case '{', '[':
		closer := byte('}')
		node.Split = "h"
		if rest[0] == '[' {
			closer = ']'
			node.Split = "v"
		}
		rest = rest[1:]
		for {
			child, rest2, err := parseCell(rest)
			if err != nil {
				return nil, s, err
			}
			node.Children = append(node.Children, child)
			if rest2 == "" {
				return nil, s, fmt.Errorf("unexpected end in container")
			}
			switch rest2[0] {
			case ',':
				rest = rest2[1:]
			case closer:
				return node, rest2[1:], nil
			default:
				return nil, s, fmt.Errorf("unexpected %q in container", rest2[0])
			}
		}
	case ',':
		// Look ahead: is the next char a digit (pane id)? If so, leaf.
		// Otherwise this comma belongs to the enclosing container.
		if len(rest) > 1 && rest[1] >= '0' && rest[1] <= '9' {
			pid, rest2, err := parseIntUpTo(rest[1:], ",}]")
			if err != nil {
				return nil, s, fmt.Errorf("layout pane id: %w", err)
			}
			node.PaneID = fmt.Sprintf("%%%d", pid)
			return node, rest2, nil
		}
		return node, rest, nil
	default:
		// Closing brace / bracket / etc — this cell has no pane id.
		return node, rest, nil
	}
}

// parseIntUpTo reads a non-negative integer from the start of s that
// is terminated by any byte in seps (or end of string). Returns the
// value and the remainder starting AT the terminator (or "" at EOF).
func parseIntUpTo(s string, seps string) (int, string, error) {
	idx := strings.IndexAny(s, seps)
	if idx < 0 {
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, s, err
		}
		return n, "", nil
	}
	n, err := strconv.Atoi(s[:idx])
	if err != nil {
		return 0, s, err
	}
	return n, s[idx:], nil
}
