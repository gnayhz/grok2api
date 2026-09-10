import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { subscribeSessionEnded } from "@/shared/auth/session";

function createQueryClient(): QueryClient {
  return new QueryClient({ defaultOptions: {
    queries: { retry: 1, staleTime: 15_000, refetchOnWindowFocus: false },
    mutations: { retry: 0 },
  } });
}

export function SessionQueryProvider({ children }: { children: ReactNode }) {
  const [client, setClient] = useState(createQueryClient);
  const current = useRef(client);
  useEffect(() => {
    const unsubscribe = subscribeSessionEnded(() => {
      const previous = current.current;
      const next = createQueryClient();
      current.current = next;
      // clear cancels queries and removes both caches. A fresh client also
      // isolates late mutation rollback/settled callbacks holding the old one.
      previous.clear();
      setClient(next);
    });
    return () => { unsubscribe(); current.current.clear(); };
  }, []);
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}
