// One browser-tab session owns its token and every in-flight management request.
// Refresh changes the token within that session; ending it replaces the scope.
export type AdminSession = {
  readonly signal: AbortSignal;
};

type SessionState = AdminSession & { controller: AbortController; token: string | null };
function createSession(): SessionState {
  const controller = new AbortController();
  return { controller, signal: controller.signal, token: null };
}
let current = createSession();
const endedListeners = new Set<() => void>();

export function currentSession(): AdminSession { return current; }
export function isCurrentSession(session: AdminSession): boolean { return session === current && !session.signal.aborted; }
export function sessionAccessToken(session: AdminSession): string | null { return isCurrentSession(session) ? current.token : null; }
export function acceptSessionToken(session: AdminSession, token: string): boolean {
  if (!isCurrentSession(session)) return false;
  current.token = token;
  return true;
}
export function endSession(session: AdminSession = current): AdminSession {
  if (!isCurrentSession(session)) return current;
  const previous = current;
  current = createSession();
  previous.controller.abort(new DOMException("Session ended", "AbortError"));
  endedListeners.forEach((listener) => listener());
  return current;
}
export function subscribeSessionEnded(listener: () => void): () => void {
  endedListeners.add(listener);
  return () => endedListeners.delete(listener);
}
