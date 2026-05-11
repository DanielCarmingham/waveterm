// Copyright 2026, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

import { MetaKeyAtomFnType, useWaveEnv, WaveEnvSubset } from "@/app/waveenv/waveenv";
import { cn } from "@/util/util";
import {
    autoUpdate,
    flip,
    FloatingPortal,
    offset,
    safePolygon,
    shift,
    useFloating,
    useHover,
    useInteractions,
} from "@floating-ui/react";
import * as jotai from "jotai";
import { useEffect, useRef, useState } from "react";

export type TmuxFlyoverEnv = WaveEnvSubset<{
    getBlockMetaKeyAtom: MetaKeyAtomFnType<
        "controller" | "connection" | "tmux:sessionname" | "tmux:paneid" | "tmux:panerows" | "tmux:panecols"
    >;
}>;

interface TmuxSessionFlyoverProps {
    blockId: string;
    placement?: "top" | "bottom" | "left" | "right";
    divClassName?: string;
}

export function TmuxSessionFlyover({ blockId, placement = "bottom", divClassName }: TmuxSessionFlyoverProps) {
    const waveEnv = useWaveEnv<TmuxFlyoverEnv>();
    const controller = jotai.useAtomValue(waveEnv.getBlockMetaKeyAtom(blockId, "controller"));
    const sessionName = jotai.useAtomValue(waveEnv.getBlockMetaKeyAtom(blockId, "tmux:sessionname"));
    const paneId = jotai.useAtomValue(waveEnv.getBlockMetaKeyAtom(blockId, "tmux:paneid"));
    const paneRows = jotai.useAtomValue(waveEnv.getBlockMetaKeyAtom(blockId, "tmux:panerows"));
    const paneCols = jotai.useAtomValue(waveEnv.getBlockMetaKeyAtom(blockId, "tmux:panecols"));
    const connection = jotai.useAtomValue(waveEnv.getBlockMetaKeyAtom(blockId, "connection"));

    const [isOpen, setIsOpen] = useState(false);
    const [isVisible, setIsVisible] = useState(false);
    const timeoutRef = useRef<number | null>(null);

    const { refs, floatingStyles, context } = useFloating({
        open: isOpen,
        onOpenChange: (open) => {
            if (open) {
                setIsOpen(true);
                if (timeoutRef.current != null) {
                    window.clearTimeout(timeoutRef.current);
                }
                timeoutRef.current = window.setTimeout(() => setIsVisible(true), 250);
            } else {
                setIsVisible(false);
                if (timeoutRef.current != null) {
                    window.clearTimeout(timeoutRef.current);
                }
                timeoutRef.current = window.setTimeout(() => setIsOpen(false), 200);
            }
        },
        placement,
        middleware: [offset(10), flip(), shift({ padding: 12 })],
        whileElementsMounted: autoUpdate,
    });

    useEffect(() => {
        return () => {
            if (timeoutRef.current != null) {
                window.clearTimeout(timeoutRef.current);
            }
        };
    }, []);

    const hover = useHover(context, { handleClose: safePolygon() });
    const { getReferenceProps, getFloatingProps } = useInteractions([hover]);

    if (controller !== "tmux") {
        return null;
    }

    const hostLabel = !connection || connection === "" ? "local" : connection;
    const sizeLabel = paneRows > 0 && paneCols > 0 ? `${paneCols}×${paneRows}` : null;

    return (
        <>
            <div ref={refs.setReference} {...getReferenceProps()} className={divClassName}>
                <i className="fa-sharp fa-solid fa-table-cells-large text-emerald-500" />
            </div>
            {isOpen && (
                <FloatingPortal>
                    <div
                        ref={refs.setFloating}
                        style={{
                            ...floatingStyles,
                            opacity: isVisible ? 1 : 0,
                            transition: "opacity 200ms ease",
                        }}
                        {...getFloatingProps()}
                        className={cn(
                            "bg-zinc-800 border border-border rounded-md px-3 py-2.5 text-xs text-foreground shadow-xl z-50 max-w-[320px]"
                        )}
                        onMouseDown={(e) => e.stopPropagation()}
                        onClick={(e) => e.stopPropagation()}
                    >
                        <div className="flex flex-col gap-1.5">
                            <div className="font-semibold flex items-center gap-2 text-secondary">
                                <i className="fa-sharp fa-solid fa-table-cells-large text-emerald-500" />
                                tmux session
                            </div>
                            <div className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5 text-xs">
                                <div className="text-muted">Session</div>
                                <div className="font-mono">{sessionName || "(unnamed)"}</div>
                                {paneId && (
                                    <>
                                        <div className="text-muted">Pane</div>
                                        <div className="font-mono">{paneId}</div>
                                    </>
                                )}
                                {sizeLabel && (
                                    <>
                                        <div className="text-muted">Size</div>
                                        <div className="font-mono">{sizeLabel}</div>
                                    </>
                                )}
                                <div className="text-muted">Host</div>
                                <div className="font-mono">{hostLabel}</div>
                            </div>
                        </div>
                    </div>
                </FloatingPortal>
            )}
        </>
    );
}

TmuxSessionFlyover.displayName = "TmuxSessionFlyover";
