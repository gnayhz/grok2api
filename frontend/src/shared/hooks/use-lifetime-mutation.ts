import { useMutation, type UseMutationOptions } from "@tanstack/react-query";

import { isAbortError } from "@/shared/lib/is-abort-error";
import { useLifetimeSignal } from "./use-lifetime-signal";

type LifetimeMutationOptions<Data, Variables, Failure> = Omit<UseMutationOptions<Data, Failure, Variables>,
  "mutationFn" | "onMutate" | "onSuccess" | "onError" | "onSettled"> & {
  mutationFn: (variables: Variables, signal: AbortSignal) => Promise<Data>;
  onSuccess?: (data: Data, variables: Variables) => unknown;
  onError?: (error: Failure, variables: Variables) => unknown;
  onSettled?: (data: Data | undefined, error: Failure | null, variables: Variables) => unknown;
};

type OwnedVariables<Variables> = { value: Variables; signal: AbortSignal };

// Capture ownership when submitting, before React Query schedules the work.
// StrictMode remounts, retries and late acknowledgements keep the original signal.
// Accepted server work retains its own lifecycle; a new page reconciles it by query.
export function useLifetimeMutation<Data, Variables = void, Failure = Error>(options: LifetimeMutationOptions<Data, Variables, Failure>) {
  const lifetimeSignal = useLifetimeSignal();
  const mutation = useMutation<Data, Failure, OwnedVariables<Variables>>({
    ...options,
    mutationFn: async ({ value, signal }) => {
      signal.throwIfAborted();
      const data = await options.mutationFn(value, signal);
      signal.throwIfAborted();
      return data;
    },
    onSuccess: (data, { value, signal }) => {
      if (!signal.aborted) return options.onSuccess?.(data, value);
    },
    onError: (error, { value, signal }) => {
      if (!signal.aborted && !isAbortError(error)) return options.onError?.(error, value);
    },
    onSettled: (data, error, { value, signal }) => {
      if (!signal.aborted) return options.onSettled?.(data, error, value);
    },
  });
  return {
    ...mutation,
    variables: mutation.variables?.value,
    mutate: (value: Variables) => mutation.mutate({ value, signal: lifetimeSignal() }),
    mutateAsync: (value: Variables) => mutation.mutateAsync({ value, signal: lifetimeSignal() }),
  };
}
