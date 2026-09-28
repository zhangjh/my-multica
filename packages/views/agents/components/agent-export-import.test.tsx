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
const exportAgentsMock = vi.fn();
const listAgentsMock = vi.fn(async () => [] as unknown[]);

vi.mock("@multica/core/api", () => ({
  api: {
    exportAgents: (params?: unknown) => exportAgentsMock(params),
    importAgents: (file: unknown, opts?: unknown) =>
      importAgentsMock(file, opts),
  },
}));

vi.mock("@multica/core/workspace/queries", async (importOriginal) => {
  const actual = await importOriginal<
    typeof import("@multica/core/workspace/queries")
  >();
  return {
    ...actual,
    agentListOptions: () => ({
      queryKey: ["workspaces", "ws-1", "agents"],
      queryFn: () => listAgentsMock(),
    }),
  };
});

import { AgentExportDialog, AgentImportDialog } from "./agent-export-import";

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

function makeAgent(
  id: string,
  name: string,
  overrides: Record<string, unknown> = {},
) {
  return {
    id,
    workspace_id: "ws-1",
    runtime_id: "rt-1",
    name,
    description: "",
    instructions: "",
    avatar_url: null,
    runtime_mode: "local",
    runtime_config: {},
    max_concurrent_tasks: 1,
    owner_id: "user-1",
    archived_at: null,
    custom_args: [],
    visibility: "private",
    permission_mode: "private",
    invocation_targets: [],
    model: "claude",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    status: "idle",
    skills: [],
    archived_by: null,
    ...overrides,
  };
}

function exportFileFor(agents: Array<{ id: string; name: string }>) {
  return {
    version: 1,
    kind: "multica-agent-export",
    exported_at: "2026-09-28T00:00:00Z",
    agents: agents.map((a) => ({
      source_id: a.id,
      name: a.name,
      permission_mode: "private",
    })),
  };
}

function renderExportDialog({
  presetAgentIds,
  onOpenChange = vi.fn(),
}: {
  presetAgentIds?: readonly string[];
  onOpenChange?: (open: boolean) => void;
} = {}) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <QueryClientProvider client={queryClient}>
        <AgentExportDialog
          open
          onOpenChange={onOpenChange}
          presetAgentIds={presetAgentIds}
        />
      </QueryClientProvider>
    </I18nProvider>,
  );
  return { onOpenChange };
}

describe("AgentExportDialog", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    importAgentsMock.mockReset();
    exportAgentsMock.mockReset();
    listAgentsMock.mockReset();
    exportAgentsMock.mockImplementation(async () =>
      exportFileFor([{ id: "ag-1", name: "Alpha" }]),
    );
    listAgentsMock.mockResolvedValue([
      makeAgent("ag-1", "Alpha"),
      makeAgent("ag-2", "Bravo"),
      // Built-in agents are dropped by the server, so the picker hides them.
      makeAgent("ag-3", "Built-in", { system_key: "workspace_entry" }),
    ]);
  });

  afterEach(() => {
    cleanup();
    document.body.innerHTML = "";
  });

  it("preselects every exportable agent when opened with no preset", async () => {
    renderExportDialog();

    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Export 2 agents" }),
      ).toBeTruthy(),
    );
    expect(screen.queryByText("Built-in")).toBeNull();
  });

  it("starts from the caller's preset and lets the user widen it", async () => {
    renderExportDialog({ presetAgentIds: ["ag-2"] });

    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Export 1 agent" }),
      ).toBeTruthy(),
    );

    fireEvent.click(screen.getByRole("button", { name: "Alpha" }));
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Export 2 agents" }),
      ).toBeTruthy(),
    );

    fireEvent.click(screen.getByRole("button", { name: "Bravo" }));
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Export 1 agent" }),
      ).toBeTruthy(),
    );
  });

  it("requests only the selected agents", async () => {
    const { onOpenChange } = renderExportDialog({ presetAgentIds: ["ag-2"] });

    const submit = await screen.findByRole("button", { name: "Export 1 agent" });
    fireEvent.click(submit);

    await waitFor(() => expect(exportAgentsMock).toHaveBeenCalledTimes(1));
    expect(exportAgentsMock.mock.calls[0]![0]).toEqual({
      workspace_id: "ws-1",
      agent_ids: ["ag-2"],
    });
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });

  it("asks for archived agents when the selection holds one", async () => {
    listAgentsMock.mockResolvedValue([
      makeAgent("ag-1", "Alpha", { archived_at: "2026-01-01T00:00:00Z" }),
    ]);

    renderExportDialog({ presetAgentIds: ["ag-1"] });

    fireEvent.click(
      await screen.findByRole("button", { name: "Export 1 agent" }),
    );

    await waitFor(() => expect(exportAgentsMock).toHaveBeenCalledTimes(1));
    expect(exportAgentsMock.mock.calls[0]![0]).toEqual({
      workspace_id: "ws-1",
      agent_ids: ["ag-1"],
      include_archived: true,
    });
  });

  it("keeps the export button disabled with nothing selected", async () => {
    renderExportDialog();

    const clear = await screen.findByRole("button", { name: "Clear" });
    fireEvent.click(clear);

    const submit = screen.getByRole("button", { name: "Export 0 agents" });
    expect((submit as HTMLButtonElement).disabled).toBe(true);
    expect(exportAgentsMock).not.toHaveBeenCalled();
  });

  it("narrowing the search does not drop the hidden selections", async () => {
    renderExportDialog();

    await screen.findByRole("button", { name: "Export 2 agents" });
    fireEvent.change(screen.getByPlaceholderText("Search agents"), {
      target: { value: "alp" },
    });
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "Bravo" })).toBeNull(),
    );

    // Bravo is filtered out of the view but still counted, because a search
    // narrows the list, not the selection.
    expect(
      screen.getByRole("button", { name: "Export 2 agents" }),
    ).toBeTruthy();
  });
});
