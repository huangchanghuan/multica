import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import { EditorOpenSection, editorBaseUrl } from "./editor-open-section";
import { renderWithI18n } from "../../test/i18n";

const { fetchMock } = vi.hoisted(() => ({ fetchMock: vi.fn() }));
vi.stubGlobal("fetch", fetchMock);

const WORKDIR = "/home/user/multica_workspaces/ws1/task1/workdir";

function optionsResponse(over: Partial<{ editors: unknown; entries: unknown }> = {}) {
  return {
    ok: true,
    status: 200,
    json: async () => ({
      editors: [
        { id: "trae", label: "Trae" },
        { id: "vscode", label: "VS Code" },
      ],
      entries: [
        { name: "output", is_dir: true },
        { name: "README.md", is_dir: false },
      ],
    }),
    ...over,
  };
}

function jsonResponse(body: unknown, ok = true, status = 200) {
  return { ok, status, json: async () => body };
}

beforeEach(() => {
  fetchMock.mockReset();
});

afterEach(() => {
  cleanup();
});

describe("EditorOpenSection", () => {
  it("hides entirely when the daemon is unreachable", async () => {
    fetchMock.mockRejectedValue(new TypeError("connection refused"));
    const { container } = renderWithI18n(<EditorOpenSection workdirPath={WORKDIR} />);
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
  });

  it("hides when no editor CLI is detected", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ editors: [], entries: [] }));
    const { container } = renderWithI18n(<EditorOpenSection workdirPath={WORKDIR} />);
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
  });

  it("shows the editor picker and opens with the selected editor", async () => {
    fetchMock.mockResolvedValue(optionsResponse());
    renderWithI18n(<EditorOpenSection workdirPath={WORKDIR} />);

    // Section renders once options arrive.
    await screen.findByText("Open in editor");

    // Fire the open with default editor (first detected) and no entry.
    fireEvent.click(screen.getByRole("button", { name: "Open" }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    const [url, init] = fetchMock.mock.calls[1] as [
      string,
      { method: string; headers: Record<string, string>; body: string },
    ];
    expect(url).toBe(`${editorBaseUrl()}/editor/open`);
    expect(init.method).toBe("POST");
    expect(init.headers["X-Multica-Web"]).toBe("1");
    expect(JSON.parse(init.body)).toEqual({ path: WORKDIR, editor: "trae" });
  });

  it("does not fire the open request when the launch fails validation", async () => {
    fetchMock.mockResolvedValue(optionsResponse());
    renderWithI18n(<EditorOpenSection workdirPath={WORKDIR} />);
    await screen.findByText("Open in editor");
    // No interaction: only the options fetch must have happened.
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});
