import { useCallback, useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { pollDeviceAuthorization, startDeviceAuthorization, type DeviceSessionDTO } from "@/entities/account/account-api";
import { ApiError } from "@/shared/api/client";
import { useAbortController } from "@/shared/lib/use-abort-controller";

// One dialog owns both session creation and polling. Close, restart and page
// unmount cancel that owner; late results cannot install or finish another login.
export function useDeviceLogin(onComplete: () => void) {
  const { t } = useTranslation();
  const { ref, begin, cancel } = useAbortController();
  const [open, setOpen] = useState(false);
  const [session, setSession] = useState<DeviceSessionDTO | null>(null);
  const [status, setStatus] = useState<"starting" | "pending" | "failed">("starting");
  const close = useCallback(() => {
    cancel();
    setOpen(false);
    setSession(null);
  }, [cancel]);
  const start = async () => {
    const { signal } = begin();
    setOpen(true);
    setStatus("starting");
    setSession(null);
    try {
      const result = await startDeviceAuthorization(signal);
      if (signal.aborted) return;
      setSession(result);
      setStatus("pending");
    } catch (error) {
      if (signal.aborted) return;
      setStatus("failed");
      toast.error(error instanceof Error ? error.message : t("errors.generic"));
    }
  };

  useEffect(() => {
    if (!open || !session || status !== "pending" || !ref.current) return;
    const controller = new AbortController();
    const signal = AbortSignal.any([controller.signal, ref.current.signal]);
    let timer = 0;
    const poll = async () => {
      try {
        const result = await pollDeviceAuthorization(session.sessionId, signal);
        if (signal.aborted) return;
        if (result.status === "succeeded" || result.status === "syncFailed") {
          if (result.status === "succeeded") toast.success(t("accounts.created"));
          else toast.warning(t("accounts.createdWithSyncFailure"));
          close();
          onComplete();
          return;
        }
        timer = window.setTimeout(poll, session.intervalSeconds * 1000);
      } catch (error) {
        if (signal.aborted) return;
        if (error instanceof ApiError && error.status === 429) {
          timer = window.setTimeout(poll, (session.intervalSeconds + 5) * 1000);
          return;
        }
        setStatus("failed");
        toast.error(error instanceof Error ? error.message : t("errors.generic"));
      }
    };
    timer = window.setTimeout(poll, session.intervalSeconds * 1000);
    return () => { controller.abort(); window.clearTimeout(timer); };
  }, [open, session, status, ref, close, onComplete, t]);

  return { open, session, status, start, close, onOpenChange: (value: boolean) => { if (!value) close(); } };
}
