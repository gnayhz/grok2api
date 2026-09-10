import { useCallback, useEffect, useRef } from "react";

// Capture the signal when starting work. StrictMode can start a new effect
// lifetime; already-running work must keep the signal of its original owner.
export function useLifetimeSignal(): () => AbortSignal {
  const current = useRef<AbortController | null>(null);
  useEffect(() => {
    const owner = new AbortController();
    current.current = owner;
    return () => owner.abort();
  }, []);
  return useCallback(() => {
    if (!current.current) throw new DOMException("Component is not mounted", "AbortError");
    return current.current.signal;
  }, []);
}
