import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

/** Query key namespace for everything Telegram-installation-related. Realtime
 * sync invalidates `installations(wsId)` on `telegram_installation:*` events
 * so the Settings panel updates without a manual refetch. `settings(wsId)`
 * carries the deployment-wide master-key state (admin surface). */
export const telegramKeys = {
  all: (wsId: string) => ["telegram", wsId] as const,
  installations: (wsId: string) => [...telegramKeys.all(wsId), "installations"] as const,
  settings: (wsId: string) => [...telegramKeys.all(wsId), "settings"] as const,
};

export const telegramInstallationsOptions = (wsId: string) =>
  queryOptions({
    queryKey: telegramKeys.installations(wsId),
    queryFn: () => api.listTelegramInstallations(wsId),
    enabled: !!wsId,
  });

/** Deployment-wide Telegram master-key state. Admin-gated on the server, so
 * callers should only enable this for owners/admins. */
export const telegramSettingsOptions = (wsId: string) =>
  queryOptions({
    queryKey: telegramKeys.settings(wsId),
    queryFn: () => api.getTelegramSettings(wsId),
    enabled: !!wsId,
  });
