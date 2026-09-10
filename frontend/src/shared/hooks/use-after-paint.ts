import { startTransition, useEffect, useState } from "react";

// Let navigation or a dialog paint its frame before mounting expensive content.
export function useAfterPaint(): boolean {
  const [ready, setReady] = useState(false);
  useEffect(() => {
    let frame = requestAnimationFrame(() => {
      frame = requestAnimationFrame(() => startTransition(() => setReady(true)));
    });
    return () => cancelAnimationFrame(frame);
  }, []);
  return ready;
}
