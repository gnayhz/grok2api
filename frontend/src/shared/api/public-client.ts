import { localizedErrorMessage } from "@/shared/api/client";
import { currentSession } from "@/shared/auth/session";
import { runtimeConfig } from "@/shared/config/runtime-config";

/**
 * Transport for the public, client-key authenticated `/v1/*` API.
 *
 * The admin session APIs use the shared `apiRequest`/`apiEventStream` client
 * with the admin bearer token; public calls must not. They still share the
 * transport contract: resolve the configured base URL, attach the caller's key
 * as a bearer token, decode the OpenAI-compatible error envelope, and abort
 * local transport when the admin session ends.
 */
export class PublicApiError extends Error {
	readonly status: number;
	readonly code?: string;

	constructor(status: number, message: string, code?: string) {
		super(message);
		this.name = "PublicApiError";
		this.status = status;
		this.code = code;
	}
}

export type PublicRequestOptions = {
	method?: "GET" | "POST";
	body?: BodyInit | Record<string, unknown>;
	headers?: HeadersInit;
	signal?: AbortSignal;
};

export type PublicApiResponse = {
	response: Response;
	/** Session-scoped signal that also carries the caller's abort signal. */
	signal: AbortSignal;
};

// This workbench is mounted inside the admin session. Its requests still use
// the explicitly selected client key; ending the UI session only cancels their
// local transport, without claiming to cancel an already accepted video job.
function publicRequestSignal(caller?: AbortSignal | null): AbortSignal {
	const scope = currentSession().signal;
	return caller ? AbortSignal.any([scope, caller]) : scope;
}

/**
 * Sends one public API request. `path` is relative to the public `/v1` root
 * (e.g. `/images/generations`), so a configured `publicApiBaseUrl` — not the
 * admin UI origin — decides where the request actually goes.
 */
export async function publicApiSend(apiKey: string, path: string, options: PublicRequestOptions = {}): Promise<PublicApiResponse> {
	const signal = publicRequestSignal(options.signal);
	const headers = new Headers(options.headers);
	headers.set("Authorization", `Bearer ${apiKey}`);
	let body: BodyInit | undefined;
	if (options.body instanceof FormData || typeof options.body === "string" || options.body instanceof Blob) {
		body = options.body;
	} else if (options.body !== undefined) {
		headers.set("Content-Type", "application/json");
		body = JSON.stringify(options.body);
	}
	const response = await fetch(`${runtimeConfig.publicApiBaseUrl}/v1${path}`, {
		method: options.method ?? "GET",
		headers,
		body,
		signal,
	});
	return { response, signal };
}

/** OpenAI-compatible error envelope: `{ error: {...} }` or a flat `{ code, message }`. */
function readPublicApiError(payload: unknown): { code?: string; message?: string } {
	if (!isRecord(payload)) return {};
	const error = isRecord(payload.error) ? payload.error : payload;
	return {
		code: typeof error.code === "string" ? error.code : undefined,
		message: typeof error.message === "string" ? error.message : undefined,
	};
}

export function publicApiErrorFrom(response: Response, payload: unknown, fallback: string): PublicApiError {
	const error = readPublicApiError(payload);
	const raw = error.message ?? fallback;
	// round 111: /v1/* error codes resolve through apiErrors so the English UI
	// never shows the backend's Chinese message.
	return new PublicApiError(response.status, error.code ? localizedErrorMessage(error.code, raw) : raw, error.code);
}

/** Consumes a failed response body and throws the decoded {@link PublicApiError}. */
export async function throwPublicApiError(response: Response, signal?: AbortSignal): Promise<never> {
	const text = await response.text().catch(() => "");
	signal?.throwIfAborted();
	throw publicApiErrorFrom(response, parseJson(text), text.trim() || response.statusText || `HTTP ${response.status}`);
}

/** JSON request helper for endpoints that answer with a single JSON document. */
export async function publicApiRequest(apiKey: string, path: string, options: PublicRequestOptions = {}): Promise<unknown> {
	const { response, signal } = await publicApiSend(apiKey, path, options);
	const text = await response.text();
	signal.throwIfAborted();
	const payload = parseJson(text);
	if (!response.ok) throw publicApiErrorFrom(response, payload, text.trim() || response.statusText || `HTTP ${response.status}`);
	if (payload === null) throw new PublicApiError(response.status, "The API returned a non-JSON response", "invalid_response");
	return payload;
}

function parseJson(value: string): unknown {
	if (!value.trim()) return null;
	try {
		return JSON.parse(value);
	} catch {
		return null;
	}
}

function isRecord(value: unknown): value is Record<string, unknown> {
	return typeof value === "object" && value !== null && !Array.isArray(value);
}
