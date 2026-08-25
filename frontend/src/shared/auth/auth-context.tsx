import { useCallback, useEffect, useState, type ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";

import {
  ApiError,
  apiRequest,
  decodeAdminDTO,
  decodeLoggedOut,
  decodeLoginResponseDTO,
  refreshAccessToken,
  setAccessToken,
  subscribeSessionInvalidated,
  type AdminDTO,
} from "@/shared/api/client";
import { AuthContext, type AuthStatus } from "@/shared/auth/auth-state";

export function AuthProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();
  const [admin, setAdmin] = useState<AdminDTO | null>(null);
  const [status, setStatus] = useState<AuthStatus>("restoring");

  const restoreSession = useCallback(async (): Promise<void> => {
    setStatus("restoring");
    const refreshResult = await refreshAccessToken();
    if (refreshResult === "invalid") {
      setAdmin(null);
      setStatus("anonymous");
      return;
    }
    if (refreshResult === "unavailable") {
      setStatus("unavailable");
      return;
    }

    try {
      const value = await apiRequest("/api/admin/v1/me", { retryAuth: false }, decodeAdminDTO);
      setAdmin(value);
      setStatus("authenticated");
    } catch (error) {
      setAccessToken(null);
      setAdmin(null);
      setStatus(error instanceof ApiError && error.status === 401 ? "anonymous" : "unavailable");
    }
  }, []);

  useEffect(() => {
    const unsubscribe = subscribeSessionInvalidated(() => {
      // 会话失效即清空查询缓存：react-query 缓存跨页面卸载存活，
      // 同浏览器后续登录的其他管理员会在 staleTime 窗口内首帧渲染
      // 上一位管理员的陈旧管理面数据。
      queryClient.clear();
      setAdmin(null);
      setStatus("anonymous");
    });

    const restoreTimer = window.setTimeout(() => {
      void restoreSession();
    }, 0);

    return () => {
      window.clearTimeout(restoreTimer);
      unsubscribe();
    };
  }, [restoreSession, queryClient]);

  async function login(username: string, password: string): Promise<void> {
    const response = await apiRequest("/api/admin/v1/auth/login", {
      method: "POST",
      body: { username, password },
      authenticated: false,
      retryAuth: false,
    }, decodeLoginResponseDTO);
    setAccessToken(response.tokens.accessToken);
    setAdmin(response.admin);
    setStatus("authenticated");
  }

  async function logout(): Promise<void> {
    try {
      await apiRequest("/api/admin/v1/auth/logout", {
        method: "POST",
        body: {},
        authenticated: false,
        retryAuth: false,
      }, decodeLoggedOut);
    } finally {
      setAccessToken(null);
      setAdmin(null);
      setStatus("anonymous");
    }
  }

  async function changePassword(currentPassword: string, newPassword: string): Promise<void> {
    await apiRequest("/api/admin/v1/me/password", {
      method: "PUT",
      body: { currentPassword, newPassword },
    }, () => undefined);
  }

  return (
    <AuthContext.Provider value={{ admin, status, retryRestore: restoreSession, login, logout, changePassword }}>
      {children}
    </AuthContext.Provider>
  );
}
