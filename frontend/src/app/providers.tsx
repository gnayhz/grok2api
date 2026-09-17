import { ThemeProvider } from "next-themes";
import type { ReactNode } from "react";
import { Toaster } from "sonner";

import { TooltipProvider } from "@/shared/ui/tooltip";
import { SessionQueryProvider } from "@/shared/auth/session-query-provider";
import { AuthProvider } from "@/shared/auth/auth-context";

export function AppProviders({ children }: { children: ReactNode }) {
  return (
    <ThemeProvider attribute="class" defaultTheme="system" enableSystem disableTransitionOnChange>
      <SessionQueryProvider>
        <AuthProvider>
          <TooltipProvider delayDuration={300}>
            {children}
            <Toaster richColors closeButton position="top-right" />
          </TooltipProvider>
        </AuthProvider>
      </SessionQueryProvider>
    </ThemeProvider>
  );
}

