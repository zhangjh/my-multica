// @vitest-environment jsdom

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import type { AgentImportReport } from "@multica/core/types";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enAgents from "../../locales/en/agents.json";

const TEST_RESOURCES = { en: { common: enCommon, agents: enAgents } };

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "ws-1",
}));

vi.mock("@multica/core/runtimes", () => ({
  runtimeListOptions: () => ({
    queryKey: ["runtimes"],
    queryFn: async () => [],
  }),
  runtimeDisplayLabel: (runtime: { name: string }) => runtime.name,
}));

const importAgentsMock = vi.fn();

vi.mock("@multica/core/api", () => ({
  api: {
    exportAgents: vi.fn(),
    importAgents: (file: unknown, opts?: unknown) =>
      importAgentsMock(file, opts),
  },
}));

import { AgentImportDialog } from "./agent-export-import";

function parseFile(text: string): File {
  return new File([text], "agents-export.json", { type: "application/json" });
}

function minimalExportJson(): string {
  return JSON.stringify({
    version: 1,
    kind: "multica-agent-export",
    exported_at: "2026-09-20T00:00:00Z",
    source: { name: "Test WS" },
    runtimes: [{ source_id: "rt-1", name: "Runner", provider: "test" }],
    skills: [],
    agents: [
      {
        source_id: "ag-1",
        name: "Imported Bot",
        runtime_name: "Runner",
        permission_mode: "private",
        custom_env: { SECRET: "hunter2" },
      },
    ],
  });
}

function renderDialog({ open = true }: { open?: boolean } = {}) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const onOpenChange = vi.fn();
  render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <QueryClientProvider client={queryClient}>
        <AgentImportDialog open={open} onOpenChange={onOpenChange} />
      </QueryClientProvider>
    </I18nProvider>,
  );
  return { onOpenChange };
}

describe("AgentImportDialog", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    importAgentsMock.mockReset();
  });
  afterEach(() => {
    cleanup();
    document.body.innerHTML = "";
  });

  it("rejects a file that is not a multica agent export", async () => {
    renderDialog();
    const input = document.querySelector<HTMLInputElement>(
      'input[type="file"]',
    );
    expect(input).not.toBeNull();

    fireEvent.change(input!, { target: { files: [new File(["nope"], "x.txt")] } });
    await waitFor(() =>
      expect(screen.getByText(/doesn't look like a multica agent export/i)).toBeTruthy(),
    );

    const submit = screen.getByRole("button", { name: /Import 0 agents/i });
    expect((submit as HTMLButtonElement).disabled).toBe(true);
  });

  it("imports a valid file and shows the per-agent report", async () => {
    const report: AgentImportReport = {
      results: [
        {
          name: "Imported Bot",
          status: "created",
          id: "new-agent-id",
          source_id: "ag-1",
          runtime: "Runner",
        },
      ],
    };
    importAgentsMock.mockResolvedValue(report);

    renderDialog();
    const input = document.querySelector<HTMLInputElement>(
      'input[type="file"]',
    );
    fireEvent.change(input!, {
      target: { files: [parseFile(minimalExportJson())] },
    });
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: /Import 1 agents/i }),
      ).toBeTruthy(),
    );

    fireEvent.click(screen.getByRole("button", { name: /Import 1 agents/i }));
    await waitFor(() => expect(importAgentsMock).toHaveBeenCalledTimes(1));

    const [, opts] = importAgentsMock.mock.calls[0]!;
    expect(opts).toMatchObject({
      workspace_id: "ws-1",
      onConflict: "skip",
    });

    await waitFor(() =>
      expect(screen.getByText("Import results")).toBeTruthy(),
    );
    expect(screen.getByText("Imported Bot")).toBeTruthy();
    expect(screen.getByText("Created")).toBeTruthy();
  });

  it("sends the chosen conflict policy", async () => {
    importAgentsMock.mockResolvedValue({ results: [] });

    renderDialog();
    const input = document.querySelector<HTMLInputElement>(
      'input[type="file"]',
    );
    fireEvent.change(input!, {
      target: { files: [parseFile(minimalExportJson())] },
    });
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: /Import 1 agents/i }),
      ).toBeTruthy(),
    );

    fireEvent.click(screen.getByRole("button", { name: /Import 1 agents/i }));
    await waitFor(() =>
      expect(importAgentsMock.mock.calls[0]?.[1]).toMatchObject({
        onConflict: "skip",
      }),
    );
  });

  it("does not render anything when closed", () => {
    renderDialog({ open: false });
    expect(
      screen.queryByText(/Import agents/i),
    ).toBeNull();
    expect(document.querySelector('input[type="file"]')).toBeNull();
  });
});