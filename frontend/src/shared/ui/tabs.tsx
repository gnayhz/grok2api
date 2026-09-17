import * as TabsPrimitive from "@radix-ui/react-tabs";
import * as React from "react";

import { cn } from "@/shared/lib/cn";

export function Tabs({ className, ...props }: React.ComponentProps<typeof TabsPrimitive.Root>) {
  return <TabsPrimitive.Root className={cn("flex flex-col", className)} {...props} />;
}

type IndicatorGeometry = { left: number; top: number; width: number; height: number };

export const TabsList = React.forwardRef<React.ElementRef<typeof TabsPrimitive.List>, React.ComponentPropsWithoutRef<typeof TabsPrimitive.List> & { showIndicator?: boolean }>(
  function TabsList({ className, children, showIndicator = true, ...props }, forwardedRef) {
    const listRef = React.useRef<React.ElementRef<typeof TabsPrimitive.List> | null>(null);
    const [indicator, setIndicator] = React.useState<IndicatorGeometry | null>(null);
    const setListRef = React.useCallback((node: React.ElementRef<typeof TabsPrimitive.List> | null) => {
      listRef.current = node;
      if (typeof forwardedRef === "function") forwardedRef(node);
      else if (forwardedRef) (forwardedRef as React.MutableRefObject<React.ElementRef<typeof TabsPrimitive.List> | null>).current = node;
    }, [forwardedRef]);

    const updateIndicator = React.useCallback(() => {
      const list = listRef.current;
      const active = list?.querySelector<HTMLElement>('[role="tab"][data-state="active"]');
      if (!list || !active) {
        setIndicator(null);
        return;
      }
      const next = { left: active.offsetLeft, top: active.offsetTop, width: active.offsetWidth, height: active.offsetHeight };
      setIndicator((current) => current && current.left === next.left && current.top === next.top && current.width === next.width && current.height === next.height ? current : next);
    }, []);

    React.useLayoutEffect(() => {
      if (!showIndicator) return;
      const list = listRef.current;
      if (!list) return;
      updateIndicator();
      let frame: number | undefined;
      const scheduleUpdate = () => {
        if (frame !== undefined) return;
        frame = requestAnimationFrame(() => { frame = undefined; updateIndicator(); });
      };
      const mutationObserver = new MutationObserver(scheduleUpdate);
      mutationObserver.observe(list, { attributes: true, childList: true, subtree: true, attributeFilter: ["data-state"] });
      const resizeObserver = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(scheduleUpdate);
      resizeObserver?.observe(list);
      list.querySelectorAll<HTMLElement>('[role="tab"]').forEach((tab) => resizeObserver?.observe(tab));
      window.addEventListener("resize", scheduleUpdate);
      return () => {
        mutationObserver.disconnect();
        resizeObserver?.disconnect();
        window.removeEventListener("resize", scheduleUpdate);
        if (frame !== undefined) cancelAnimationFrame(frame);
      };
    }, [showIndicator, updateIndicator]);

    return (
      <TabsPrimitive.List ref={setListRef} className={cn("relative isolate inline-flex h-8 w-fit items-center gap-1 rounded-full bg-muted p-0.5", className)} {...props}>
        {showIndicator && indicator ? (
          <span
            aria-hidden="true"
            className="pointer-events-none absolute left-0 top-0 z-0 rounded-full bg-background shadow-sm transition-[transform,width,height] duration-200 ease-[cubic-bezier(0.22,1,0.36,1)] motion-reduce:transition-none"
            style={{ width: indicator.width, height: indicator.height, transform: `translate3d(${indicator.left}px, ${indicator.top}px, 0)` }}
          />
        ) : null}
        {children}
      </TabsPrimitive.List>
    );
  },
);

export function TabsTrigger({ className, ...props }: React.ComponentProps<typeof TabsPrimitive.Trigger>) {
  return (
    <TabsPrimitive.Trigger
      className={cn("relative z-10 inline-flex h-7 items-center justify-center rounded-full px-3 text-xs font-medium text-muted-foreground outline-none transition-colors hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50 data-[state=active]:text-foreground", className)}
      {...props}
    />
  );
}

export function TabsContent({ className, ...props }: React.ComponentProps<typeof TabsPrimitive.Content>) {
  return <TabsPrimitive.Content className={cn("min-w-0 outline-none", className)} {...props} />;
}
