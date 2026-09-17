import { acceptSessionToken, currentSession, endSession, isCurrentSession, sessionAccessToken, type AdminSession } from "@/shared/auth/session";
import { decodeAuthTokensDTO } from "@/shared/auth/admin-dto";
import type { ApiDecoder } from "@/shared/api/decoder";
import { runtimeConfig } from "@/shared/config/runtime-config";
import { i18n } from "@/shared/i18n";

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly requestId?: string;

  constructor(status: number, code: string, message: string, requestId?: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.requestId = requestId;
  }
}

let refreshEntry: { session: AdminSession; promise: Promise<RefreshResult> } | null = null;
const refreshLockName = "grok2api:admin-session-refresh";
const sessionRefreshTimeoutMs = 5_000;
const maxEventStreamBufferCharacters = 1 << 20;
const eventStreamInactivityTimeoutMs = 60_000;

export type RefreshResult = "refreshed" | "invalid" | "unavailable" | "superseded";

// Detach caller signals at completion, and reject promptly even if a custom
// transport finishes late. Fetch still receives the signal to stop its body.
function requestScope(session: AdminSession, caller?: AbortSignal | null) {
  const controller = new AbortController();
  const signals = caller ? [session.signal, caller] : [session.signal];
  const abort = (event: Event) => controller.abort((event.target as AbortSignal).reason);
  for (const signal of signals) {
    if (signal.aborted) controller.abort(signal.reason);
    else signal.addEventListener("abort", abort, { once: true });
  }
  return { signal: controller.signal, dispose: () => signals.forEach(signal => signal.removeEventListener("abort", abort)) };
}

function abortable<T>(promise: Promise<T>, signal: AbortSignal): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const abort = () => reject(signal.reason);
    if (signal.aborted) abort();
    else signal.addEventListener("abort", abort, { once: true });
    promise.then(value => {
      signal.removeEventListener("abort", abort);
      if (signal.aborted) reject(signal.reason);
      else resolve(value);
    }, error => {
      signal.removeEventListener("abort", abort);
      reject(error);
    });
  });
}

// round 111 导出：创意工作台调用 /v1/* 拿到的 OpenAI 兼容错误码需要同一
// 套 apiErrors 查找，避免英文界面显示后端中文原文。
export function localizedErrorMessage(code: string, fallback: string): string {
  const key = `apiErrors.${code}`;
  return i18n.exists(key) ? i18n.t(key) : fallback;
}

async function parseResponse<T>(response: Response, decode: ApiDecoder<T>, signal?: AbortSignal): Promise<T> {
  const payload: unknown = await response.json().catch(() => null);
  signal?.throwIfAborted();
  if (!response.ok) {
    const error = readErrorEnvelope(payload);
    const code = error.code ?? "requestFailed";
    throw new ApiError(response.status, code, localizedErrorMessage(code, error.message ?? `HTTP ${response.status}`), error.requestId);
  }
  if (!isRecord(payload) || !("data" in payload)) {
    throw new ApiError(response.status, "invalidResponse", localizedErrorMessage("invalidResponse", "Server returned an invalid response"));
  }
  try {
    return decode(payload.data);
  } catch {
    throw new ApiError(response.status, "invalidResponse", localizedErrorMessage("invalidResponse", "Server returned an invalid response"));
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function readErrorEnvelope(payload: unknown): { code?: string; message?: string; requestId?: string } {
  if (!isRecord(payload) || !isRecord(payload.error)) return {};
  return {
    code: typeof payload.error.code === "string" ? payload.error.code : undefined,
    message: typeof payload.error.message === "string" ? payload.error.message : undefined,
    requestId: typeof payload.error.requestId === "string" ? payload.error.requestId : undefined,
  };
}

async function requestRefresh(session: AdminSession): Promise<RefreshResult> {
  if (!isCurrentSession(session)) return "superseded";
  const timeoutController = new AbortController();
  const timeout = window.setTimeout(() => timeoutController.abort(), sessionRefreshTimeoutMs);
  const scope = requestScope(session, timeoutController.signal);
  try {
    const response = await abortable(fetch(`${runtimeConfig.apiBaseUrl}/api/admin/v1/auth/refresh`, {
      method: "POST", credentials: "include", headers: { "Content-Type": "application/json" }, body: "{}", signal: scope.signal,
    }), scope.signal);
    if (response.status === 401) {
      void response.body?.cancel().catch(() => undefined);
      endSession(session);
      return "invalid";
    }
    const tokens = await abortable(parseResponse(response, decodeAuthTokensDTO, scope.signal), scope.signal);
    return acceptSessionToken(session, tokens.accessToken) ? "refreshed" : "superseded";
  } catch {
    return isCurrentSession(session) ? "unavailable" : "superseded";
  } finally {
    scope.dispose();
    window.clearTimeout(timeout);
  }
}

async function requestRefreshWithBrowserLock(session: AdminSession): Promise<RefreshResult> {
  if (!("locks" in navigator)) return requestRefresh(session);
  try {
    // Retain the bounded, best-effort cross-tab lock. Server rotation remains
    // authoritative; session checks also apply inside a delayed lock callback.
    return await navigator.locks.request(refreshLockName, { ifAvailable: true }, () => requestRefresh(session));
  } catch {
    return isCurrentSession(session) ? "unavailable" : "superseded";
  }
}

export async function refreshAccessToken(session: AdminSession = currentSession()): Promise<RefreshResult> {
  if (!isCurrentSession(session)) return "superseded";
  if (!refreshEntry || refreshEntry.session !== session) {
    const entry = { session, promise: requestRefreshWithBrowserLock(session) };
    refreshEntry = entry;
    void entry.promise.finally(() => { if (refreshEntry === entry) refreshEntry = null; });
  }
  return refreshEntry.promise;
}

type RequestOptions = Omit<RequestInit, "body"> & {
  body?: BodyInit | object;
  authenticated?: boolean;
  retryAuth?: boolean;
};

async function sendApiRequest(path: string, options: RequestOptions, session: AdminSession): Promise<Response> {
  const { authenticated = true, retryAuth, body, headers, ...requestInit } = options;
  void retryAuth;
  const requestHeaders = new Headers(headers);
  let requestBody: BodyInit | undefined;

  if (body instanceof FormData || typeof body === "string" || body instanceof Blob) {
    requestBody = body;
  } else if (body !== undefined) {
    requestHeaders.set("Content-Type", "application/json");
    requestBody = JSON.stringify(body);
  }
  const accessToken = sessionAccessToken(session);
  if (authenticated && accessToken) {
    requestHeaders.set("Authorization", `Bearer ${accessToken}`);
  }

  return fetch(`${runtimeConfig.apiBaseUrl}${path}`, {
    ...requestInit,
    body: requestBody,
    credentials: "include",
    headers: requestHeaders,
  });
}

async function withApiResponse<T>(path: string, options: RequestOptions, consume: (response: Response, signal: AbortSignal) => Promise<T>): Promise<T> {
  const session = currentSession();
  const scope = requestScope(session, options.signal);
  const { authenticated = true, retryAuth = true } = options;
  let response: Response | undefined;
  try {
    for (let attempt = 0; ; attempt++) {
      scope.signal.throwIfAborted();
      try {
        response = await abortable(sendApiRequest(path, { ...options, signal: scope.signal }, session), scope.signal);
      } catch (error) {
        scope.signal.throwIfAborted();
        if (error instanceof ApiError) throw error;
        throw new ApiError(0, "networkError", localizedErrorMessage("networkError", "Cannot reach the server. Check the network or retry later."));
      }
      if (response.status !== 401 || !authenticated) break;
      void response.body?.cancel().catch(() => undefined);
      if (!retryAuth || attempt > 0) {
        endSession(session);
        scope.signal.throwIfAborted();
      }
      const result = await abortable(refreshAccessToken(session), scope.signal);
      scope.signal.throwIfAborted();
      if (result === "refreshed") continue;
      throw new ApiError(503, "sessionRefreshUnavailable", localizedErrorMessage("sessionRefreshUnavailable", "Unable to refresh the session. Please retry."));
    }
    const result = await abortable(consume(response, scope.signal), scope.signal);
    scope.signal.throwIfAborted();
    return result;
  } catch (error) {
    void response?.body?.cancel().catch(() => undefined);
    throw error;
  } finally {
    scope.dispose();
  }
}

export async function apiRequest<T>(path: string, options: RequestOptions, decode: ApiDecoder<T>): Promise<T> {
  return withApiResponse(path, options, (response, signal) => parseResponse(response, decode, signal));
}

export type ApiStreamEvent<T> = {
  event: string;
  data: T;
};

// apiEventStream 使用现有管理员鉴权发起 POST SSE，并正确处理任意分块边界。
export async function apiEventStream<T>(path: string, options: RequestOptions, decode: ApiDecoder<T>, onEvent: (value: ApiStreamEvent<T>) => void): Promise<void> {
  return withApiResponse(path, options, (response, signal) => consumeEventStream(response, signal, decode, onEvent));
}

async function consumeEventStream<T>(response: Response, signal: AbortSignal, decode: ApiDecoder<T>, onEvent: (value: ApiStreamEvent<T>) => void): Promise<void> {
  if (!response.ok) {
    await parseResponse(response, decodeNever, signal);
  }
  if (!response.body) {
    throw new ApiError(response.status, "invalidResponse", localizedErrorMessage("invalidResponse", "Server returned an invalid response"));
  }
  const contentType = response.headers.get("Content-Type")?.toLowerCase() ?? "";
  if (!contentType.startsWith("text/event-stream")) {
    await response.body.cancel().catch(() => undefined);
    throw new ApiError(response.status, "invalidResponse", localizedErrorMessage("invalidResponse", "Server returned an invalid response"));
  }

  const reader = response.body.getReader();
  const cancel = () => { void reader.cancel().catch(() => undefined); };
  signal.addEventListener("abort", cancel, { once: true });
  const decoder = new TextDecoder();
  let buffer = "";
  const dispatch = (block: string) => {
    signal.throwIfAborted();
    let event = "message";
    const data: string[] = [];
    block.split("\n").forEach((line) => {
      const normalized = line.endsWith("\r") ? line.slice(0, -1) : line;
      if (normalized.startsWith("event:")) event = normalized.slice(6).trim();
      if (normalized.startsWith("data:")) data.push(normalized.slice(5).trimStart());
    });
    if (data.length === 0) return;
    let payload: T;
    try {
      payload = decode(JSON.parse(data.join("\n")) as unknown);
    } catch {
      throw new ApiError(response.status, "invalidResponse", localizedErrorMessage("invalidResponse", "Server returned an invalid response"));
    }
    onEvent({ event, data: payload });
  };

  try {
    for (;;) {
      const { done, value } = await readEventStreamChunk(reader, response.status, signal);
      signal.throwIfAborted();
      buffer += decoder.decode(value, { stream: !done });
      buffer = buffer.replaceAll("\r\n", "\n");
      let boundary = buffer.indexOf("\n\n");
      while (boundary >= 0) {
        if (boundary > maxEventStreamBufferCharacters) {
          throw new ApiError(response.status, "invalidResponse", localizedErrorMessage("invalidResponse", "Server returned an invalid response"));
        }
        dispatch(buffer.slice(0, boundary));
        buffer = buffer.slice(boundary + 2);
        boundary = buffer.indexOf("\n\n");
      }
      if (buffer.length > maxEventStreamBufferCharacters) {
        throw new ApiError(response.status, "invalidResponse", localizedErrorMessage("invalidResponse", "Server returned an invalid response"));
      }
      if (done) break;
    }
    if (buffer.trim()) dispatch(buffer);
  } catch (error) {
    await reader.cancel().catch(() => undefined);
    throw error;
  } finally {
    signal.removeEventListener("abort", cancel);
    reader.releaseLock();
  }
}

async function readEventStreamChunk(reader: ReadableStreamDefaultReader<Uint8Array>, status: number, signal: AbortSignal): Promise<ReadableStreamReadResult<Uint8Array>> {
  let timeout = 0;
  const inactivity = new Promise<never>((_, reject) => {
    timeout = window.setTimeout(() => {
      reject(new ApiError(status, "streamTimeout", localizedErrorMessage("streamTimeout", "The progress stream stopped responding")));
    }, eventStreamInactivityTimeoutMs);
  });
  try {
    return await abortable(Promise.race([reader.read(), inactivity]), signal);
  } finally {
    window.clearTimeout(timeout);
  }
}

export type ApiDownloadResult = {
  blob: Blob;
  headers: Headers;
};

export async function apiDownloadResponse(path: string, options: RequestOptions = {}): Promise<ApiDownloadResult> {
  return withApiResponse(path, options, async (response, signal) => {
    if (!response.ok) await parseResponse(response, decodeNever, signal);
    return { blob: await response.blob(), headers: response.headers };
  });
}

export async function apiDownload(path: string, options: RequestOptions = {}): Promise<Blob> {
  return (await apiDownloadResponse(path, options)).blob;
}

function decodeNever(): never {
  throw new Error("unexpected successful response");
}

export type PaginatedDTO<T> = {
  items: T[];
  page: number;
  pageSize: number;
  total: number;
};
