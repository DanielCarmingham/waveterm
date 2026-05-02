// Copyright 2026, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

import { Modal } from "@/app/modals/modal";
import { RpcApi } from "@/app/store/wshclientapi";
import { TabRpcClient } from "@/app/store/wshrpcutil";
import { modalsModel } from "@/store/modalmodel";
import { fireAndForget } from "@/util/util";
import * as keyutil from "@/util/keyutil";
import clsx from "clsx";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

export type TmuxSessionPickerProps = {
    defaultSession?: string;
    onSelect: (sessionName: string) => void;
};

export const TmuxSessionPicker = (props: TmuxSessionPickerProps) => {
    const { defaultSession = "waveterm", onSelect } = props;
    const [sessions, setSessions] = useState<string[]>([]);
    const [loading, setLoading] = useState(true);
    const [text, setText] = useState("");
    const [selectedIdx, setSelectedIdx] = useState(0);
    const inputRef = useRef<HTMLInputElement>(null);

    useEffect(() => {
        setLoading(true);
        fireAndForget(async () => {
            try {
                const resp = await RpcApi.TmuxListSessionsCommand(TabRpcClient, {});
                const list = (resp?.sessions ?? []).slice().sort((a, b) => a.localeCompare(b));
                setSessions(list);
                const defaultIdx = list.indexOf(defaultSession);
                if (defaultIdx >= 0) setSelectedIdx(defaultIdx);
            } catch (e) {
                console.warn("tmux list-sessions failed", e);
                setSessions([]);
            } finally {
                setLoading(false);
            }
        });
    }, [defaultSession]);

    useEffect(() => {
        inputRef.current?.focus();
    }, []);

    const filtered = useMemo(() => {
        const q = text.trim().toLowerCase();
        if (!q) return sessions;
        return sessions.filter((s) => s.toLowerCase().includes(q));
    }, [sessions, text]);

    const isExisting = sessions.includes(text.trim());

    const cancel = useCallback(() => {
        modalsModel.popModal();
    }, []);

    const submit = useCallback(
        (name: string) => {
            const trimmed = name.trim();
            if (!trimmed) return;
            modalsModel.popModal();
            onSelect(trimmed);
        },
        [onSelect]
    );

    const handleKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
        const we = keyutil.adaptFromReactOrNativeKeyEvent(e.nativeEvent);
        if (keyutil.checkKeyPressed(we, "Escape")) {
            e.preventDefault();
            cancel();
            return;
        }
        if (keyutil.checkKeyPressed(we, "Enter")) {
            e.preventDefault();
            if (filtered.length > 0 && selectedIdx >= 0 && selectedIdx < filtered.length && !isExisting) {
                submit(filtered[selectedIdx]);
            } else {
                submit(text);
            }
            return;
        }
        if (keyutil.checkKeyPressed(we, "ArrowDown")) {
            e.preventDefault();
            setSelectedIdx((i) => Math.min(filtered.length - 1, i + 1));
            return;
        }
        if (keyutil.checkKeyPressed(we, "ArrowUp")) {
            e.preventDefault();
            setSelectedIdx((i) => Math.max(0, i - 1));
            return;
        }
    };

    return (
        <Modal onClickBackdrop={cancel} onClose={cancel}>
            <div className="flex flex-col gap-3 min-w-[420px]">
                <div className="text-base font-semibold">Open tmux session</div>
                <input
                    ref={inputRef}
                    value={text}
                    onChange={(e) => {
                        setText(e.target.value);
                        setSelectedIdx(0);
                    }}
                    onKeyDown={handleKeyDown}
                    placeholder={`Type to filter or create — default: ${defaultSession}`}
                    className="bg-panelbg border border-border rounded px-2 py-1.5 text-sm focus:outline-none focus:border-accent"
                />
                <div className="flex flex-col max-h-72 overflow-y-auto -mx-1">
                    {loading ? (
                        <div className="text-secondary text-xs px-3 py-2">Loading…</div>
                    ) : filtered.length === 0 ? (
                        <div className="text-secondary text-xs px-3 py-2">
                            {sessions.length === 0
                                ? "No existing sessions. Type a name and press Enter to create one."
                                : "No matches. Press Enter to create a new session with this name."}
                        </div>
                    ) : (
                        filtered.map((name, idx) => (
                            <div
                                key={name}
                                onClick={() => submit(name)}
                                onMouseEnter={() => setSelectedIdx(idx)}
                                className={clsx(
                                    "px-3 py-1.5 rounded cursor-pointer flex items-center gap-2 text-sm",
                                    idx === selectedIdx ? "bg-accent/20 text-primary" : "hover:bg-hoverbg"
                                )}
                            >
                                <i className="fa fa-solid fa-window-restore text-secondary text-xs" />
                                <span>{name}</span>
                            </div>
                        ))
                    )}
                </div>
                <div className="text-secondary text-xs">
                    Enter attaches to the selected session, or creates a new one if the typed name doesn't match.
                </div>
            </div>
        </Modal>
    );
};

TmuxSessionPicker.displayName = "TmuxSessionPicker";
