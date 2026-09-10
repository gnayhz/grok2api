import { Fragment, useCallback, useEffect, useRef, useState, type ReactNode } from "react";

import { apiRequest, decodeAdminDTO, decodeLoggedOut, decodeLoginResponseDTO, refreshAccessToken, type AdminDTO } from "@/shared/api/client";
import { acceptSessionToken, currentSession, endSession, isCurrentSession, subscribeSessionEnded } from "@/shared/auth/session";
import { AuthContext, type AuthStatus } from "@/shared/auth/auth-state";

export function AuthProvider({ children }: { children: ReactNode }) {
  const [admin, setAdmin] = useState<AdminDTO | null>(null);
  const [generation, setGeneration] = useState(0);
  const [status, setStatus] = useState<AuthStatus>("restoring");
  const operation = useRef(0);
  const pendingLogout = useRef<Promise<void> | null>(null);

  const restoreSession = useCallback(async (): Promise<void> => {
    const sequence = ++operation.current;
    const session = currentSession();
    const ownsResult = () => operation.current === sequence && isCurrentSession(session);
    setStatus("restoring");
    const refreshResult = await refreshAccessToken(session);
    if (!ownsResult()) return;
    if (refreshResult === "unavailable") {
      setStatus("unavailable");
      return;
    }
    if (refreshResult !== "refreshed") return;
    try {
      const value = await apiRequest("/api/admin/v1/me", { retryAuth: false, signal: session.signal }, decodeAdminDTO);
      if (!ownsResult()) return;
      setAdmin(value);
      setStatus("authenticated");
    } catch {
      // Confirmed 401 goes through endSession in the API layer. Transport or
      // service failures retain the session and expose retryable unavailability.
      if (ownsResult()) setStatus("unavailable");
    }
  }, []);

  useEffect(() => subscribeSessionEnded(() => {
    if (admin !== null) setGeneration(value => value + 1);
    setAdmin(null);
    setStatus("anonymous");
  }), [admin]);

  useEffect(() => {
    const restoreTimer = window.setTimeout(() => { void restoreSession(); }, 0);
    return () => {
      window.clearTimeout(restoreTimer);
      endSession();
    };
  }, [restoreSession]);

  async function login(username: string, password: string): Promise<void> {
    const sequence = ++operation.current;
    // Local state changes immediately. Wait for a previous logout HTTP response
    // before creating a new cookie session in this tab.
    const previous = currentSession();
    await pendingLogout.current?.catch(() => undefined);
    if (sequence !== operation.current || !isCurrentSession(previous)) throw new DOMException("Login superseded", "AbortError");
    const session = endSession();
    const response = await apiRequest("/api/admin/v1/auth/login", {
      method: "POST", body: { username, password }, authenticated: false, retryAuth: false, signal: session.signal,
    }, decodeLoginResponseDTO);
    if (sequence !== operation.current || !acceptSessionToken(session, response.tokens.accessToken)) throw new DOMException("Login superseded", "AbortError");
    setAdmin(response.admin);
    setStatus("authenticated");
  }

  function logout(): Promise<void> {
    if (pendingLogout.current) return pendingLogout.current;
    operation.current++;
    endSession();
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 5_000);
    const request = apiRequest("/api/admin/v1/auth/logout", {
      method: "POST", body: {}, authenticated: false, retryAuth: false, signal: controller.signal,
    }, decodeLoggedOut).then(() => undefined).finally(() => {
      window.clearTimeout(timeout);
      if (pendingLogout.current === request) pendingLogout.current = null;
    });
    pendingLogout.current = request;
    return request;
  }

  async function changePassword(currentPassword: string, newPassword: string): Promise<void> {
    await apiRequest("/api/admin/v1/me/password", { method: "PUT", body: { currentPassword, newPassword } }, () => undefined);
  }

  return (
    <AuthContext.Provider value={{ admin, status, retryRestore: restoreSession, login, logout, changePassword }}>
      <Fragment key={generation}>{children}</Fragment>
    </AuthContext.Provider>
  );
}
