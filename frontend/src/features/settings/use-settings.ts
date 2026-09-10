import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef } from "react";
import { useForm } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { getSettings, resetSettings, resetSettingsRotation, updateSettings, type SettingsSnapshotDTO } from "@/features/settings/settings-api";
import { settingsSchema, toSettingsDTO, toSettingsForm, type SettingsForm } from "@/features/settings/settings-model";
import { useLifetimeSignal } from "@/shared/hooks/use-lifetime-signal";

export function useSettings() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const lifetimeSignal = useLifetimeSignal();
  const settingsQuery = useQuery({
    queryKey: ["settings"],
    queryFn: ({ signal }) => getSettings(signal),
    refetchInterval: (query) => query.state.data?.applyPending || query.state.data?.notification?.state === "failed" ? 3000 : false,
  });
  const form = useForm<SettingsForm>({ resolver: zodResolver(settingsSchema) });
  const editingBase = useRef<SettingsSnapshotDTO | null>(null);
  const installForm = (snapshot: SettingsSnapshotDTO) => {
    editingBase.current = snapshot;
    form.reset(toSettingsForm(snapshot.config));
  };
  const acceptSaved = async (snapshot: SettingsSnapshotDTO, message: string) => {
    const signal = lifetimeSignal();
    if (signal.aborted) return;
    await queryClient.cancelQueries({ queryKey: ["settings"] });
    if (signal.aborted) return;
    queryClient.setQueryData(["settings"], snapshot);
    void queryClient.invalidateQueries({ queryKey: ["system-info"] });
    installForm(snapshot);
    if (snapshot.applyPending) toast.info(t("settings.application.savedPending"));
    else toast.success(t(message));
  };
  const reportFailure = (error: unknown) => {
    if (lifetimeSignal().aborted) return;
    toast.error(error instanceof Error ? error.message : t("errors.generic"));
    // Refresh status after a conflict; the effect below preserves the dirty edit
    // and its CAS base until the user explicitly reloads the saved form.
    void queryClient.invalidateQueries({ queryKey: ["settings"] });
  };
  const updateMutation = useMutation({
    mutationFn: async (config: SettingsForm) => {
      const signal = lifetimeSignal();
      const revision = editingBase.current?.revision ?? "0";
      await queryClient.cancelQueries({ queryKey: ["settings"] });
      signal.throwIfAborted();
      return updateSettings(revision, toSettingsDTO(config), signal);
    },
    onSuccess: (snapshot) => acceptSaved(snapshot, "settings.saved"),
    onError: reportFailure,
  });
  const resetDefaultsMutation = useMutation({
    mutationFn: async () => {
      const signal = lifetimeSignal();
      const revision = editingBase.current?.revision ?? "0";
      await queryClient.cancelQueries({ queryKey: ["settings"] });
      signal.throwIfAborted();
      return resetSettings(revision, signal);
    },
    onSuccess: (snapshot) => acceptSaved(snapshot, "settings.resetToDefaultsSaved"),
    onError: reportFailure,
  });
  const resetRotationMutation = useMutation({
    mutationFn: async () => {
      const signal = lifetimeSignal();
      const revision = editingBase.current?.revision ?? "0";
      await queryClient.cancelQueries({ queryKey: ["settings"] });
      signal.throwIfAborted();
      return resetSettingsRotation(revision, signal);
    },
    onSuccess: (snapshot) => acceptSaved(snapshot, "settings.resetToDefaultsSaved"),
    onError: reportFailure,
  });
  const dirty = form.formState.isDirty;
  const saving = updateMutation.isPending || resetDefaultsMutation.isPending || resetRotationMutation.isPending;

  useEffect(() => {
    if (settingsQuery.data && (!editingBase.current || !dirty) && !saving) {
      editingBase.current = settingsQuery.data;
      form.reset(toSettingsForm(settingsQuery.data.config));
    }
  }, [form, settingsQuery.data, dirty, saving]);

  return {
    form,
    settingsQuery,
    updateMutation,
    resetDefaultsMutation,
    resetRotationMutation,
    reset: () => { if (settingsQuery.data) installForm(settingsQuery.data); },
  };
}
