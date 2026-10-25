"use client";

import { useCallback, useEffect, useState } from "react";
import { ChevronRight, MonitorDown } from "lucide-react";
import { toast } from "sonner";
import { DAEMON_HEALTH_PORT } from "@multica/core/daemon/health-port";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import { useT } from "../../i18n";

export interface EditorOption {
  id: string;
  label: string;
}

export interface EditorEntry {
  name: string;
  is_dir: boolean;
}

export interface EditorOptions {
  editors: EditorOption[];
  entries: EditorEntry[];
}

export function editorBaseUrl(): string {
  return `http://127.0.0.1:${DAEMON_HEALTH_PORT}`;
}

export async function fetchEditorOptions(
  path: string,
  signal?: AbortSignal,
): Promise<EditorOptions> {
  const res = await fetch(
    `${editorBaseUrl()}/editor/options?path=${encodeURIComponent(path)}`,
    { signal },
  );
  if (!res.ok) throw new Error(`editor options status ${res.status}`);
  return (await res.json()) as EditorOptions;
}

export async function openInEditor(
  path: string,
  entry: string,
  editor: string,
): Promise<void> {
  const res = await fetch(`${editorBaseUrl()}/editor/open`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Multica-Web": "1" },
    body: JSON.stringify({ path, entry: entry || undefined, editor }),
  });
  if (!res.ok) throw new Error(`editor open status ${res.status}`);
}

/**
 * "Open in editor" in the issue sidebar (EDITOR_OPEN_PLAN.md): pick a
 * detected editor and a workdir entry, and have the local daemon spawn it.
 * Talks to the daemon's loopback health server straight from the browser, so
 * the button only appears for someone sitting at the daemon machine — anyone
 * else (or any browser that blocks loopback access) never sees the section:
 * the options fetch fails and the component renders nothing.
 */
export function EditorOpenSection({ workdirPath }: { workdirPath: string }) {
  const { t } = useT("issues");
  const [open, setOpen] = useState(true);
  const [options, setOptions] = useState<EditorOptions | null>(null);
  const [editorId, setEditorId] = useState<string>("");
  const [entryName, setEntryName] = useState<string>("");
  const [launching, setLaunching] = useState(false);

  useEffect(() => {
    const controller = new AbortController();
    fetchEditorOptions(workdirPath, controller.signal)
      .then((opts) => {
        if (opts.editors.length === 0) return; // no editor CLI on PATH
        setOptions(opts);
        setEditorId((current) => current || opts.editors[0]?.id || "");
      })
      .catch(() => {
        // Unreachable daemon, wrong port, cross-machine viewer, or a browser
        // that blocked the loopback request: degrade to no section.
      });
    return () => controller.abort();
  }, [workdirPath]);

  const handleOpen = useCallback(() => {
    if (!options || !editorId) return;
    setLaunching(true);
    openInEditor(workdirPath, entryName, editorId)
      .then(() => {
        const label =
          options.editors.find((e) => e.id === editorId)?.label ?? editorId;
        toast.success(t(($) => $.editor_open.opened, { editor: label }));
      })
      .catch(() => toast.error(t(($) => $.editor_open.open_failed)))
      .finally(() => setLaunching(false));
  }, [options, workdirPath, entryName, editorId, t]);

  if (!options) return null;

  const editorItems = options.editors.map((e) => ({ value: e.id, label: e.label }));
  const entryItems = [
    { value: "", label: t(($) => $.editor_open.location_root) },
    ...options.entries.map((e) => ({
      value: e.name,
      label: e.is_dir ? `${e.name}/` : e.name,
    })),
  ];

  return (
    <div>
      <button
        type="button"
        className={`flex w-full items-center gap-1 rounded-md px-2 py-1 text-caption font-medium transition-colors mb-2 hover:bg-accent/70 ${open ? "" : "text-muted-foreground hover:text-foreground"}`}
        onClick={() => setOpen(!open)}
        aria-expanded={open}
      >
        {t(($) => $.editor_open.section_title)}
        <ChevronRight
          className={`!size-3 shrink-0 stroke-[2.5] text-muted-foreground transition-transform ${open ? "rotate-90" : ""}`}
        />
      </button>
      {open && (
        <div className="grid grid-cols-[auto_1fr] items-center gap-x-2 gap-y-1.5 pl-2 pb-1">
          <label className="text-caption text-muted-foreground" htmlFor="editor-open-editor">
            {t(($) => $.editor_open.editor_label)}
          </label>
          <Select items={editorItems} value={editorId} onValueChange={(value) => setEditorId(value ?? "")}>
            <SelectTrigger id="editor-open-editor" className="h-7 text-caption">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {options.editors.map((editor) => (
                <SelectItem key={editor.id} value={editor.id} className="text-caption">
                  {editor.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <label className="text-caption text-muted-foreground" htmlFor="editor-open-entry">
            {t(($) => $.editor_open.location_label)}
          </label>
          <Select items={entryItems} value={entryName} onValueChange={(value) => setEntryName(value ?? "")}>
            <SelectTrigger id="editor-open-entry" className="h-7 text-caption">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="" className="text-caption">
                {t(($) => $.editor_open.location_root)}
              </SelectItem>
              {options.entries.map((entry) => (
                <SelectItem key={entry.name} value={entry.name} className="text-caption">
                  {entry.is_dir ? `${entry.name}/` : entry.name}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <span />
          <button
            type="button"
            className="flex items-center justify-center gap-1.5 rounded-md bg-primary px-2 py-1 text-caption font-medium text-primary-foreground transition-colors hover:bg-primary/90 disabled:opacity-50"
            disabled={launching}
            onClick={handleOpen}
          >
            <MonitorDown className="size-3.5 shrink-0" />
            {t(($) => $.editor_open.open)}
          </button>
        </div>
      )}
    </div>
  );
}
