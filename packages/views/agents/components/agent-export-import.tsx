"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import {
  AlertCircle,
  ArrowDownToLine,
  ArrowLeftRight,
  ArrowUpFromLine,
  CheckCircle2,
  Loader2,
  RefreshCw,
  Search,
  SkipForward,
  Upload,
} from "lucide-react";
import type {
  Agent,
  AgentExportFile,
  AgentImportConflictMode,
  AgentImportResult,
  AgentImportReport,
} from "@multica/core/types";
import { api } from "@multica/core/api";
import { AgentExportFileSchema } from "@multica/core/api/schemas";
import { useWorkspaceId } from "@multica/core/hooks";
import { runtimeDisplayLabel, runtimeListOptions } from "@multica/core/runtimes";
import {
  agentListOptions,
  workspaceKeys,
} from "@multica/core/workspace/queries";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import { Checkbox } from "@multica/ui/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@multica/ui/components/ui/dropdown-menu";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import { useT } from "../../i18n";
import { matchesPinyin } from "../../editor/extensions/pinyin-match";

/**
 * Agent configuration export/import (server: `GET /api/agents/export`,
 * `POST /api/agents/import`). Both endpoints are workspace owner/admin only;
 * the page hides this menu for everyone else, and the server enforces the same
 * gate.
 *
 * Export downloads a multica-agent-export JSON file (identical to the CLI's
 * `multica agent export` output) so it can be moved across instances or
 * imported back via the CLI. The file is per-workspace by default and can be
 * narrowed to a chosen set of agents — from the export dialog, from a single
 * row's menu, or from a multi-row selection in the list. Import restores such
 * a file; conflicts are handled per the chosen on_conflict mode
 * (skip / fail / rename).
 */

function downloadExportFile(file: AgentExportFile) {
  const blob = new Blob([JSON.stringify(file, null, 2)], {
    type: "application/json",
  });
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = `multica-agents-export-${new Date()
    .toISOString()
    .slice(0, 10)}.json`;
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  URL.revokeObjectURL(url);
}

/**
 * Downloads an export file for exactly the given agents and returns how many
 * agents it ended up carrying. Shared by the export dialog and the list's
 * batch action so both issue the same request.
 *
 * `include_archived` follows the selection: the server leaves archived agents
 * out unless asked, so exporting a chosen archived agent has to flip the flag
 * or the file would come back one agent short of what the user picked.
 */
export async function exportAgentsToFile(opts: {
  workspaceId: string;
  agents: readonly Agent[];
}): Promise<number> {
  const file = await api.exportAgents({
    workspace_id: opts.workspaceId,
    agent_ids: opts.agents.map((a) => a.id),
    ...(opts.agents.some((a) => a.archived_at)
      ? { include_archived: true }
      : {}),
  });
  if (file.agents.length === 0) return 0;
  downloadExportFile(file);
  return file.agents.length;
}

/**
 * The "Export agents" dialog: pick which agents go into the export file, then
 * download it. Opened from the header menu with no preset (so the whole
 * workspace is preselected) and from a row's menu or a multi-row selection
 * with that selection preselected — either way the user can still widen or
 * narrow it before exporting.
 */
export function AgentExportDialog({
  open,
  onOpenChange,
  presetAgentIds,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Pre-selected agent ids. Empty/undefined selects every exportable agent. */
  presetAgentIds?: readonly string[];
}) {
  const { t } = useT("agents");
  const wsId = useWorkspaceId();
  const { data: agents = [], isFetched } = useQuery(agentListOptions(wsId));

  // Product-managed agents (system_key) are skipped by the server because they
  // cannot be recreated on another instance. Hiding them here keeps the
  // "Export N agents" count honest.
  const exportable = useMemo(
    () =>
      agents
        .filter((a) => !a.system_key)
        .sort((a, b) => a.name.localeCompare(b.name)),
    [agents],
  );

  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [query, setQuery] = useState("");
  const [exporting, setExporting] = useState(false);
  // Whether this open already seeded its selection (see below).
  const [seeded, setSeeded] = useState(false);

  // Latest preset + list, read by the seeding effect below. Both have to stay
  // out of that effect's dependencies: the agent list refetches in the
  // background, and re-seeding on every refetch would throw away what the user
  // is still editing.
  const latest = useRef({ preset: presetAgentIds, exportable });
  useEffect(() => {
    latest.current = { preset: presetAgentIds, exportable };
  });

  // A fresh open starts from the caller's preset, or from every exportable
  // agent when there is none. Seeding waits for the list to arrive: doing it
  // on open alone would select nothing whenever the query was still in flight,
  // and the effect deliberately does not run again once it has seeded.
  useEffect(() => {
    if (!open) setSeeded(false);
  }, [open]);
  useEffect(() => {
    if (!open || seeded || !isFetched) return;
    const { preset, exportable: all } = latest.current;
    setQuery("");
    setSelected(
      new Set(preset && preset.length > 0 ? preset : all.map((a) => a.id)),
    );
    setSeeded(true);
  }, [open, seeded, isFetched, exportable]);

  const trimmedQuery = query.trim().toLowerCase();
  const visible = useMemo(
    () =>
      trimmedQuery
        ? exportable.filter(
            (a) =>
              a.name.toLowerCase().includes(trimmedQuery) ||
              matchesPinyin(a.name, trimmedQuery),
          )
        : exportable,
    [exportable, trimmedQuery],
  );

  // Resolving the selection against the current list drops ids for agents that
  // disappeared while the dialog was open, so the count and the request agree.
  const chosen = useMemo(() => {
    const byId = new Map(exportable.map((a) => [a.id, a]));
    return [...selected].flatMap((id) => {
      const agent = byId.get(id);
      return agent ? [agent] : [];
    });
  }, [exportable, selected]);

  const toggle = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const selectAllVisible = () => {
    setSelected((prev) => {
      const next = new Set(prev);
      for (const a of visible) next.add(a.id);
      return next;
    });
  };

  const handleClose = () => {
    if (exporting) return;
    onOpenChange(false);
  };

  const handleExport = async () => {
    if (chosen.length === 0) return;
    setExporting(true);
    try {
      const count = await exportAgentsToFile({
        workspaceId: wsId,
        agents: chosen,
      });
      if (count === 0) {
        toast.info(t(($) => $.export_import.export_none_to_export));
        return;
      }
      toast.success(t(($) => $.export_import.export_done, { count }));
      onOpenChange(false);
    } catch (error) {
      toast.error(
        t(($) => $.export_import.export_error, {
          message: error instanceof Error ? error.message : "",
        }),
      );
    } finally {
      setExporting(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={(v) => (v ? undefined : handleClose())}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="text-body">
            {t(($) => $.export_import.export_dialog_title)}
          </DialogTitle>
          <DialogDescription className="text-caption">
            {t(($) => $.export_import.export_dialog_description)}
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-2 py-1">
          <div className="flex items-center justify-between gap-2">
            <Label className="text-caption text-muted-foreground">
              {t(($) => $.export_import.export_agents_label, {
                count: chosen.length,
              })}
            </Label>
            <div className="flex items-center gap-1">
              <Button
                type="button"
                variant="ghost"
                size="sm"
                className="h-6 px-2 text-caption"
                disabled={exporting || visible.length === 0}
                onClick={selectAllVisible}
              >
                {t(($) => $.export_import.export_select_all)}
              </Button>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                className="h-6 px-2 text-caption"
                disabled={exporting || chosen.length === 0}
                onClick={() => setSelected(new Set())}
              >
                {t(($) => $.export_import.export_clear_selection)}
              </Button>
            </div>
          </div>

          <div className="rounded-lg border bg-card">
            <div className="relative border-b p-2">
              <Search className="pointer-events-none absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                placeholder={t(($) => $.export_import.export_search_placeholder)}
                className="h-8 pl-7 text-caption"
              />
            </div>

            <div className="max-h-64 space-y-0.5 overflow-y-auto p-1.5">
              {exportable.length === 0 ? (
                <div className="py-6 text-center text-caption text-muted-foreground">
                  {t(($) => $.export_import.export_list_empty)}
                </div>
              ) : visible.length === 0 ? (
                <div className="py-6 text-center text-caption text-muted-foreground">
                  {t(($) => $.export_import.export_list_no_match)}
                </div>
              ) : (
                visible.map((agent) => {
                  const isSelected = selected.has(agent.id);
                  return (
                    <button
                      key={agent.id}
                      type="button"
                      onClick={() => toggle(agent.id)}
                      aria-pressed={isSelected}
                      className={`flex w-full items-center gap-2.5 rounded-md px-2.5 py-2 text-left transition-colors ${
                        isSelected ? "bg-accent" : "hover:bg-accent/50"
                      }`}
                    >
                      {/* Indicator only — the wrapping <button> handles the
                          click, so the Checkbox is non-interactive itself. */}
                      <Checkbox
                        checked={isSelected}
                        tabIndex={-1}
                        className="pointer-events-none"
                      />
                      <span className="min-w-0 flex-1 truncate text-body font-medium">
                        {agent.name}
                      </span>
                      {agent.archived_at ? (
                        <Badge variant="secondary">
                          {t(($) => $.row.archived)}
                        </Badge>
                      ) : null}
                    </button>
                  );
                })
              )}
            </div>
          </div>
        </div>

        <DialogFooter>
          <Button
            type="button"
            variant="ghost"
            size="sm"
            onClick={handleClose}
            disabled={exporting}
          >
            {t(($) => $.export_import.import_cancel)}
          </Button>
          <Button
            type="button"
            size="sm"
            onClick={() => void handleExport()}
            disabled={chosen.length === 0 || exporting}
          >
            {exporting ? (
              <>
                <Loader2 className="h-3.5 w-3.5 animate-spin" />
                {t(($) => $.export_import.export_submitting)}
              </>
            ) : (
              t(($) => $.export_import.export_submit, { count: chosen.length })
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function ImportResultIcon({ status }: { status: AgentImportResult["status"] }) {
  switch (status) {
    case "created":
      return <CheckCircle2 className="h-3.5 w-3.5 shrink-0 text-green-600" />;
    case "renamed":
      return <RefreshCw className="h-3.5 w-3.5 shrink-0 text-blue-600" />;
    case "skipped":
      return <SkipForward className="h-3.5 w-3.5 shrink-0 text-amber-600" />;
    case "failed":
    default:
      return <AlertCircle className="h-3.5 w-3.5 shrink-0 text-destructive" />;
  }
}

function ImportResultsList({
  report,
}: {
  report: AgentImportReport;
}) {
  const { t } = useT("agents");
  return (
    <div className="space-y-1 overflow-y-auto">
      {report.results.map((result, index) => (
        <div
          key={`${result.source_id ?? result.name}-${index}`}
          className="flex items-start gap-2 rounded-xs px-2 py-1.5 text-caption"
        >
          <ImportResultIcon status={result.status} />
          <span className="min-w-0 flex-1 truncate">{result.name}</span>
          <span className="shrink-0 text-muted-foreground">
            {t(($) => $.export_import[`import_status_${result.status}`])}
          </span>
          {result.error && (
            <span className="max-w-[220px] shrink-0 truncate text-destructive">
              {result.error}
            </span>
          )}
        </div>
      ))}
    </div>
  );
}

const IMPORT_CONFLICT_OPTIONS: Array<{
  value: AgentImportConflictMode;
  key: "import_conflict_skip" | "import_conflict_fail" | "import_conflict_rename";
}> = [
  { value: "skip", key: "import_conflict_skip" },
  { value: "fail", key: "import_conflict_fail" },
  { value: "rename", key: "import_conflict_rename" },
];

/**
 * The "Import agents" dialog: pick a multica-agent-export JSON file, choose a
 * conflict policy and an optional fallback runtime, then submit. Shows a
 * per-agent result report afterwards and refreshes the agent list.
 */
export function AgentImportDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useT("agents");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const { data: runtimes = [] } = useQuery(runtimeListOptions(wsId));

  const fileInputRef = useRef<HTMLInputElement | null>(null);
  const [fileName, setFileName] = useState<string | null>(null);
  const [file, setFile] = useState<AgentExportFile | null>(null);
  const [fileError, setFileError] = useState<string | null>(null);
  const [onConflict, setOnConflict] = useState<AgentImportConflictMode>("skip");
  const [defaultRuntimeId, setDefaultRuntimeId] = useState<string>("");

  const [importing, setImporting] = useState(false);
  const [report, setReport] = useState<AgentImportReport | null>(null);

  const handleFileChange = (event: React.ChangeEvent<HTMLInputElement>) => {
    const selected = event.target.files?.[0];
    setFileError(null);
    if (!selected) {
      setFileName(null);
      setFile(null);
      return;
    }
    const reader = new FileReader();
    reader.onload = () => {
      try {
        let raw: unknown;
        try {
          raw = JSON.parse(String(reader.result));
        } catch {
          throw new Error(t(($) => $.export_import.import_invalid_file));
        }
        const parsed = AgentExportFileSchema.safeParse(raw);
        if (!parsed.success || parsed.data.agents.length === 0) {
          setFileError(t(($) => $.export_import.import_invalid_file));
          setFile(null);
          return;
        }
        setFile(parsed.data);
        setFileName(selected.name);
      } catch (error) {
        setFileError(
          error instanceof Error
            ? error.message
            : t(($) => $.export_import.import_invalid_file),
        );
        setFile(null);
      }
    };
    reader.readAsText(selected);
  };

  const handleSubmit = async () => {
    if (!file) return;
    setImporting(true);
    try {
      const result = await api.importAgents(file, {
        workspace_id: wsId,
        onConflict,
        ...(defaultRuntimeId ? { defaultRuntimeId } : {}),
      });
      if (result.error) {
        toast.error(t(($) => $.export_import.import_error, {
          message: result.error,
        }));
        setReport(result);
        return;
      }
      await Promise.all([
        qc.invalidateQueries({ queryKey: workspaceKeys.agents(wsId) }),
        qc.invalidateQueries({ queryKey: workspaceKeys.skills(wsId) }),
      ]);
      setReport(result);
    } catch (error) {
      setReport({
        error:
          error instanceof Error
            ? error.message
            : t(($) => $.export_import.import_error, { message: "" }),
        results: [],
      });
    } finally {
      setImporting(false);
    }
  };

  const handleClose = () => {
    if (importing) return;
    setFile(null);
    setFileName(null);
    setFileError(null);
    setReport(null);
    setDefaultRuntimeId("");
    onOpenChange(false);
  };

  return (
    <Dialog open={open} onOpenChange={(v) => (v ? undefined : handleClose())}>
      <DialogContent className="rounded-xl">
        <DialogHeader>
          <DialogTitle className="text-body">
            {t(($) => $.export_import.import_dialog_title)}
          </DialogTitle>
          <DialogDescription className="text-caption">
            {t(($) => $.export_import.import_dialog_description)}
          </DialogDescription>
        </DialogHeader>

        {report ? (
          <div className="space-y-3 py-2">
            {report.error ? (
              <div className="flex items-start gap-2 rounded-md bg-destructive/10 px-3 py-2 text-caption text-destructive">
                <AlertCircle className="mt-0.5 h-3.5 w-3.5 shrink-0" />
                <span>{report.error}</span>
              </div>
            ) : null}
            <p className="text-body font-medium">
              {t(($) => $.export_import.import_report_title)}
            </p>
            {report.results.length > 0 && (
              <ImportResultsList report={report} />
            )}
          </div>
        ) : (
          <div className="space-y-4 py-2">
            <div className="space-y-1.5">
              <Label className="text-caption text-muted-foreground">
                {t(($) => $.export_import.import_file_label)}
              </Label>
              <input
                ref={fileInputRef}
                type="file"
                accept="application/json,.json"
                className="hidden"
                onChange={handleFileChange}
              />
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="w-full justify-start"
                onClick={() => fileInputRef.current?.click()}
              >
                <Upload className="h-3.5 w-3.5 shink-0" />
                {fileName ?? t(($) => $.export_import.import_file_empty)}
              </Button>
              {fileError ? (
                <p className="text-caption text-destructive">{fileError}</p>
              ) : null}
            </div>

            <div className="space-y-1.5">
              <Label className="text-caption text-muted-foreground">
                {t(($) => $.export_import.import_conflict_label)}
              </Label>
              <Select
                items={IMPORT_CONFLICT_OPTIONS.map((o) => ({
                  value: o.value,
                  label: t(($) => $.export_import[o.key]),
                }))}
                value={onConflict}
                onValueChange={(v) => {
                  if (v) setOnConflict(v);
                }}
              >
                <SelectTrigger className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {IMPORT_CONFLICT_OPTIONS.map((o) => (
                    <SelectItem key={o.value} value={o.value}>
                      <span className="truncate">
                        {t(($) => $.export_import[o.key])}
                      </span>
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>

            <div className="space-y-1.5">
              <Label className="text-caption text-muted-foreground">
                {t(($) => $.export_import.import_runtime_label)}
              </Label>
              <Select
                items={runtimes.map((r) => ({
                  value: r.id,
                  label: runtimeDisplayLabel(r),
                }))}
                value={defaultRuntimeId}
                onValueChange={(v) => {
                  if (v) setDefaultRuntimeId(v);
                }}
              >
                <SelectTrigger className="w-full">
                  <SelectValue
                    placeholder={t(
                      ($) => $.export_import.import_runtime_placeholder,
                    )}
                  />
                </SelectTrigger>
                <SelectContent>
                  {runtimes.map((r) => (
                    <SelectItem key={r.id} value={r.id}>
                      <span className="truncate">{runtimeDisplayLabel(r)}</span>
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <p className="text-caption text-muted-foreground">
                {t(($) => $.export_import.import_runtime_placeholder)}
              </p>
            </div>
          </div>
        )}

        <DialogFooter>
          {report ? (
            <Button type="button" size="sm" onClick={handleClose}>
              {t(($) => $.export_import.import_report_done)}
            </Button>
          ) : (
            <>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                onClick={handleClose}
                disabled={importing}
              >
                {t(($) => $.export_import.import_cancel)}
              </Button>
              <Button
                type="button"
                size="sm"
                onClick={handleSubmit}
                disabled={!file || importing}
              >
                {importing ? (
                  <>
                    <Loader2 className="h-3.5 w-3.5 animate-spin" />
                    {t(($) => $.export_import.import_submitting)}
                  </>
                ) : (
                  t(($) => $.export_import.import_submit, {
                    count: file?.agents.length ?? 0,
                  })
                )}
              </Button>
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/**
 * Header action behind which export and import live. Rendered only for
 * workspace owners/admins (the server also enforces the gate). Exported
 * separately for testing; the menu itself is a dumb trigger — both items just
 * ask the page to open the matching dialog.
 */
export function AgentExportImportActions({
  onImportRequest,
  onExportRequest,
}: {
  onImportRequest: () => void;
  onExportRequest: () => void;
}) {
  const { t } = useT("agents");

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <Button
            type="button"
            variant="outline"
            size="sm"
            aria-label={t(($) => $.export_import.menu)}
            className="h-8 w-8 px-0 md:w-auto md:px-2.5"
          >
            <ArrowLeftRight aria-hidden="true" className="size-3.5" />
            <span className="hidden md:inline">
              {t(($) => $.export_import.menu)}
            </span>
          </Button>
        }
      />
      <DropdownMenuContent align="end" className="w-52">
        <DropdownMenuItem
          onClick={onImportRequest}
          className="items-center gap-2"
        >
          <ArrowDownToLine className="h-3.5 w-3.5" />
          {t(($) => $.export_import.import_action)}
        </DropdownMenuItem>
        <DropdownMenuItem
          onClick={onExportRequest}
          className="items-center gap-2"
        >
          <ArrowUpFromLine className="h-3.5 w-3.5" />
          {t(($) => $.export_import.export_action)}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
