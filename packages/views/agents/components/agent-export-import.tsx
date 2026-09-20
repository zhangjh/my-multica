"use client";

import { useRef, useState } from "react";
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
  SkipForward,
  Upload,
} from "lucide-react";
import type {
  AgentExportFile,
  AgentImportConflictMode,
  AgentImportResult,
  AgentImportReport,
} from "@multica/core/types";
import { api } from "@multica/core/api";
import { AgentExportFileSchema } from "@multica/core/api/schemas";
import { useWorkspaceId } from "@multica/core/hooks";
import { runtimeDisplayLabel, runtimeListOptions } from "@multica/core/runtimes";
import { workspaceKeys } from "@multica/core/workspace/queries";
import { Button } from "@multica/ui/components/ui/button";
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
import { Label } from "@multica/ui/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import { useT } from "../../i18n";

/**
 * Workspace-wide agent configuration export/import (server:
 * `GET /api/agents/export`, `POST /api/agents/import`). Both endpoints are
 * workspace owner/admin only; the page hides this menu for everyone else, and
 * the server enforces the same gate.
 *
 * Export downloads a multica-agent-export JSON file (identical to the CLI's
 * `multica agent export` output) so it can be moved across instances or
 * imported back via the CLI. Import restores such a file; conflicts are
 * handled per the chosen on_conflict mode (skip / fail / rename).
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
 * separately for testing; the menu itself is a dumb trigger.
 */
export function AgentExportImportActions({
  onImportRequest,
}: {
  onImportRequest: () => void;
}) {
  const { t } = useT("agents");
  const wsId = useWorkspaceId();
  const [exporting, setExporting] = useState(false);

  const handleExport = async () => {
    setExporting(true);
    try {
      const file = await api.exportAgents({ workspace_id: wsId });
      if (file.agents.length === 0) {
        toast.info(t(($) => $.export_import.import_file_empty));
        return;
      }
      downloadExportFile(file);
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
          onClick={() => void handleExport()}
          disabled={exporting}
          className="items-center gap-2"
        >
          {exporting ? (
            <Loader2 className="h-3.5 w-3.5 animate-spin" />
          ) : (
            <ArrowUpFromLine className="h-3.5 w-3.5" />
          )}
          {t(($) => $.export_import.export_action)}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}