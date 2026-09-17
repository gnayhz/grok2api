import js from "@eslint/js";
import globals from "globals";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";
import tseslint from "typescript-eslint";

export default tseslint.config(
  {
    // Only the shadcn upstream template files stay exempt; hand-written
    // shared/ui code (operations.*, message-scroller.tsx, spinner.tsx) is linted.
    ignores: [
      "dist",
      "src/shared/ui/alert-dialog.tsx",
      "src/shared/ui/badge.tsx",
      "src/shared/ui/button.tsx",
      "src/shared/ui/calendar.tsx",
      "src/shared/ui/chart.tsx",
      "src/shared/ui/checkbox.tsx",
      "src/shared/ui/dialog.tsx",
      "src/shared/ui/dropdown-menu.tsx",
      "src/shared/ui/input.tsx",
      "src/shared/ui/label.tsx",
      "src/shared/ui/message.tsx",
      "src/shared/ui/popover.tsx",
      "src/shared/ui/select.tsx",
      "src/shared/ui/sheet.tsx",
      "src/shared/ui/switch.tsx",
      "src/shared/ui/table.tsx",
      "src/shared/ui/tabs.tsx",
      "src/shared/ui/textarea.tsx",
      "src/shared/ui/tooltip.tsx",
    ],
  },
  {
    extends: [js.configs.recommended, ...tseslint.configs.recommended],
    files: ["**/*.{ts,tsx}"],
    languageOptions: {
      ecmaVersion: 2022,
      globals: globals.browser,
    },
    plugins: {
      "react-hooks": reactHooks,
      "react-refresh": reactRefresh,
    },
    rules: {
      ...reactHooks.configs.recommended.rules,
      "react-refresh/only-export-components": ["warn", { allowConstantExport: true }],
      "@typescript-eslint/no-explicit-any": "error",
    },
  },
  {
    files: ["src/shared/**/*.{ts,tsx}"],
    rules: {
      "no-restricted-imports": [
        "error",
        {
          patterns: [
            {
              group: ["@/entities", "@/entities/**", "@/features", "@/features/**", "@/app", "@/app/**"],
              message: "shared must not import entities, features or app",
            },
          ],
        },
      ],
    },
  },
  {
    files: ["src/entities/**/*.{ts,tsx}"],
    rules: {
      "no-restricted-imports": [
        "error",
        {
          patterns: [
            {
              group: ["@/features", "@/features/*", "@/features/**", "@/app", "@/app/*", "@/app/**"],
              message: "entities own cross-page DTOs/API and must not import features or app",
            },
          ],
        },
      ],
    },
  },
  {
    files: ["src/features/**/*.{ts,tsx}"],
    rules: {
      "no-restricted-imports": [
        "error",
        {
          patterns: [
            {
              // Siblings inside one feature use relative imports; any
              // @/features/* alias import from feature code is therefore a
              // cross-feature dependency and must go through the app layer.
              group: ["@/features", "@/features/*", "@/features/**"],
              message: "features must not import other features; compose them in app/",
            },
            {
              group: ["@/app", "@/app/*", "@/app/**"],
              message: "features must not import the app layer; compose pages in app/",
            },
          ],
        },
      ],
    },
  },
);
