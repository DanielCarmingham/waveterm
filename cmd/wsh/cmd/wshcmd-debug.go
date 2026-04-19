// Copyright 2026, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/wavetermdev/waveterm/pkg/waveobj"
	"github.com/wavetermdev/waveterm/pkg/wshrpc"
	"github.com/wavetermdev/waveterm/pkg/wshrpc/wshclient"
)

var debugCmd = &cobra.Command{
	Use:               "debug",
	Short:             "debug commands",
	PersistentPreRunE: preRunSetupRpcClient,
	Hidden:            true,
}

var debugBlockIdsCmd = &cobra.Command{
	Use:    "block",
	Short:  "list sub-blockids for block",
	RunE:   debugBlockIdsRun,
	Hidden: true,
}

var debugSendTelemetryCmd = &cobra.Command{
	Use:    "send-telemetry",
	Short:  "send telemetry",
	RunE:   debugSendTelemetryRun,
	Hidden: true,
}

var debugTmuxConnectCmd = &cobra.Command{
	Use:    "tmux-connect [session-name]",
	Short:  "start a tmux -CC session and stream events to wavesrv logs",
	Args:   cobra.MaximumNArgs(1),
	RunE:   debugTmuxConnectRun,
	Hidden: true,
}

var debugTmuxCloseCmd = &cobra.Command{
	Use:    "tmux-close <handle>",
	Short:  "close a tmux -CC session by handle",
	Args:   cobra.ExactArgs(1),
	RunE:   debugTmuxCloseRun,
	Hidden: true,
}

var debugTmuxBlockCmd = &cobra.Command{
	Use:    "tmux-block [session-name]",
	Short:  "create a term block rendering the first pane of a tmux -CC session (one-shot connect + createblock)",
	Args:   cobra.MaximumNArgs(1),
	RunE:   debugTmuxBlockRun,
	Hidden: true,
}

func init() {
	debugCmd.AddCommand(debugBlockIdsCmd)
	debugCmd.AddCommand(debugSendTelemetryCmd)
	debugCmd.AddCommand(debugTmuxConnectCmd)
	debugCmd.AddCommand(debugTmuxCloseCmd)
	debugCmd.AddCommand(debugTmuxBlockCmd)
	rootCmd.AddCommand(debugCmd)
}

func debugSendTelemetryRun(cmd *cobra.Command, args []string) error {
	err := wshclient.SendTelemetryCommand(RpcClient, nil)
	return err
}

func debugBlockIdsRun(cmd *cobra.Command, args []string) error {
	oref, err := resolveBlockArg()
	if err != nil {
		return err
	}
	blockInfo, err := wshclient.BlockInfoCommand(RpcClient, oref.OID, nil)
	if err != nil {
		return err
	}
	barr, err := json.MarshalIndent(blockInfo, "", "  ")
	if err != nil {
		return err
	}
	WriteStdout("%s\n", string(barr))
	return nil
}

func debugTmuxConnectRun(cmd *cobra.Command, args []string) error {
	data := wshrpc.CommandTmuxDevConnectData{}
	if len(args) > 0 {
		data.SessionName = args[0]
	}
	resp, err := wshclient.TmuxDevConnectCommand(RpcClient, data, nil)
	if err != nil {
		return err
	}
	WriteStdout("tmux session started, handle=%s\n", resp.Handle)
	WriteStdout("watch wavesrv stdout for [tmuxcc:...] event lines\n")
	return nil
}

func debugTmuxCloseRun(cmd *cobra.Command, args []string) error {
	return wshclient.TmuxDevCloseCommand(RpcClient, args[0], nil)
}

func debugTmuxBlockRun(cmd *cobra.Command, args []string) error {
	tabId := getTabIdFromEnv()
	if tabId == "" {
		return fmt.Errorf("no WAVETERM_TABID env var set")
	}
	sessionName := "waveterm-dev"
	if len(args) > 0 {
		sessionName = args[0]
	}
	connectData := wshrpc.CommandTmuxDevConnectData{SessionName: sessionName}
	resp, err := wshclient.TmuxDevConnectCommand(RpcClient, connectData, nil)
	if err != nil {
		return fmt.Errorf("tmux connect: %w", err)
	}
	if resp.PaneId == "" {
		return fmt.Errorf("tmux connect: no pane id returned (handle=%s) — session may be empty", resp.Handle)
	}
	meta := map[string]any{
		waveobj.MetaKey_View:              "term",
		waveobj.MetaKey_Controller:        "tmux",
		waveobj.MetaKey_TmuxSessionHandle: resp.Handle,
		waveobj.MetaKey_TmuxSessionName:   sessionName,
		waveobj.MetaKey_TmuxPaneId:        resp.PaneId,
	}
	createData := wshrpc.CommandCreateBlockData{
		TabId:    tabId,
		BlockDef: &waveobj.BlockDef{Meta: meta},
		Focused:  true,
	}
	oref, err := wshclient.CreateBlockCommand(RpcClient, createData, nil)
	if err != nil {
		return fmt.Errorf("create block failed: %w", err)
	}
	WriteStdout("tmux block created: block=%s handle=%s pane=%s\n", oref.OID, resp.Handle, resp.PaneId)
	return nil
}
