import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { RouterProvider } from "react-router-dom";

import { AppErrorBoundary } from "@/app/error-boundary";
import { AppProviders } from "@/app/providers";
import { router } from "@/app/router";
import "@/shared/i18n";
import "./index.css";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <AppErrorBoundary>
      <AppProviders>
        <RouterProvider router={router} />
      </AppProviders>
    </AppErrorBoundary>
  </StrictMode>,
);
