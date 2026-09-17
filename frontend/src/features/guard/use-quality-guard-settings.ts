import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef } from "react";
import { useForm } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { fetchQualityGuard, resetQualityGuard, updateQualityGuard, type QualityGuardConfig } from "@/entities/guard/quality-api";
import { qualityGuardSchema, toQualityGuardForm, toQualityGuardInput, type QualityGuardForm } from "@/entities/guard/quality-guard-model";
import { useLifetimeSignal } from "@/shared/hooks/use-lifetime-signal";

export function useQualityGuardSettings() {
  const { t } = useTranslation();
  const client = useQueryClient();
  const lifetimeSignal = useLifetimeSignal();
  const guardQuery = useQuery({ queryKey: ["quality", "guard"], queryFn: ({ signal }) => fetchQualityGuard(signal) });
  const form = useForm<QualityGuardForm>({ resolver: zodResolver(qualityGuardSchema) });
  const editingBase = useRef<QualityGuardConfig | null>(null);
  const install = (snapshot: QualityGuardConfig) => {
    editingBase.current = snapshot;
    form.reset(toQualityGuardForm(snapshot));
  };
  const accepted = async (snapshot: QualityGuardConfig) => {
    const signal = lifetimeSignal();
    if (signal.aborted) return;
    await client.cancelQueries({ queryKey: ["quality", "guard"], exact: true });
    if (signal.aborted) return;
    client.setQueryData(["quality", "guard"], snapshot);
    install(snapshot);
    // Refresh legacy read-only projections without writing their domain.
    void client.invalidateQueries({ queryKey: ["settings"] });
    toast.success(t("quality.guardConfig.saved"));
  };
  const rejected = (error: unknown) => {
    if (lifetimeSignal().aborted) return;
    toast.error(error instanceof Error ? error.message : t("errors.generic"));
    void client.invalidateQueries({ queryKey: ["quality", "guard"], exact: true });
  };
  const saveMutation = useMutation({
    mutationFn: async (values: QualityGuardForm) => {
      const signal = lifetimeSignal();
      const revision = editingBase.current?.revision ?? "0";
      await client.cancelQueries({ queryKey: ["quality", "guard"], exact: true });
      signal.throwIfAborted();
      return updateQualityGuard(toQualityGuardInput(revision, values), signal);
    },
    onSuccess: accepted, onError: rejected,
  });
  const resetMutation = useMutation({
    mutationFn: async () => {
      const signal = lifetimeSignal();
      const revision = editingBase.current?.revision ?? "0";
      await client.cancelQueries({ queryKey: ["quality", "guard"], exact: true });
      signal.throwIfAborted();
      return resetQualityGuard(revision, signal);
    },
    onSuccess: accepted, onError: rejected,
  });
  const dirty = form.formState.isDirty;
  const saving = saveMutation.isPending || resetMutation.isPending;
  useEffect(() => {
    if (guardQuery.data && (!editingBase.current || !dirty) && !saving) {
      editingBase.current = guardQuery.data;
      form.reset(toQualityGuardForm(guardQuery.data));
    }
  }, [form, guardQuery.data, dirty, saving]);
  return { form, guardQuery, saveMutation, resetMutation, saving,
    reload: () => { if (guardQuery.data) install(guardQuery.data); saveMutation.reset(); resetMutation.reset(); } };
}
