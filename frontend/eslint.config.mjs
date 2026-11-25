// ESLint flat config: TypeScript, the rules of hooks and JSX accessibility.
import js from "@eslint/js";
import { defineConfig } from "eslint/config";
import tseslint from "typescript-eslint";
import reactHooks from "eslint-plugin-react-hooks";
import jsxA11y from "eslint-plugin-jsx-a11y-x";
import globals from "globals";

export default defineConfig(
  { ignores: ["dist/", "node_modules/", "coverage/"] },
  js.configs.recommended,
  tseslint.configs.recommended,
  {
    files: ["src/**/*.{ts,tsx}"],
    extends: [jsxA11y.configs.recommended],
    languageOptions: { globals: globals.browser },
    plugins: { "react-hooks": reactHooks },
    rules: {
      "react-hooks/rules-of-hooks": "error",
      // Missing effect dependencies cause stale closures; fix them, don't silence them.
      "react-hooks/exhaustive-deps": "error",
      "@typescript-eslint/no-unused-vars": ["error", { argsIgnorePattern: "^_", varsIgnorePattern: "^_", caughtErrors: "none" }],
    },
  },
  {
    // Rules that need the types (tsconfig.json covers src/, tests included).
    files: ["src/**/*.{ts,tsx}"],
    languageOptions: { parserOptions: { projectService: true, tsconfigRootDir: import.meta.dirname } },
    rules: {
      // A promise nobody awaits or catches. The async handlers here catch
      // their own errors and show them in state; calling one without waiting
      // is written `void handler()`, e.g. onClick={() => void handleSave()}.
      "@typescript-eslint/no-floating-promises": "error",
      "@typescript-eslint/no-misused-promises": "error",
      "@typescript-eslint/await-thenable": "error",
      // A default case covers the members a switch does not name.
      "@typescript-eslint/switch-exhaustiveness-check": ["error", { considerDefaultExhaustiveForUnions: true }],
      "@typescript-eslint/no-base-to-string": "error",
      // Not no-unnecessary-condition: it flags the `?.` and `??` that guard
      // API fields the types call required but older backends and Go's nil
      // slices (JSON null) leave out, and indexing (`list[0] ?? null`), which
      // the types call never undefined without noUncheckedIndexedAccess.
    },
  },
  {
    // No ellipsis at the end of a label or a progress message ("Install...",
    // "Loading builds..."): PatternFly uses one only for omitted or truncated
    // text, labels a dialog-opening action without it, and shows loading with
    // a Spinner. Test code may still quote old text.
    files: ["src/**/*.{ts,tsx}"],
    ignores: ["src/**/*.test.{ts,tsx}", "src/test/**"],
    rules: {
      "no-restricted-syntax": ["error",
        { selector: "Literal[value=/(\\.\\.\\.|\u2026)\\s*$/]", message: "No ellipsis at the end of UI text: label actions without it and show progress with a Spinner." },
        { selector: "JSXText[value=/(\\.\\.\\.|\u2026)\\s*$/]", message: "No ellipsis at the end of UI text: label actions without it and show progress with a Spinner." },
        { selector: "TemplateElement[tail=true][value.raw=/(\\.\\.\\.|\u2026)\\s*$/]", message: "No ellipsis at the end of UI text: label actions without it and show progress with a Spinner." },
      ],
    },
  },
  {
    files: ["**/*.cjs", "*.js"],
    languageOptions: { globals: globals.node, sourceType: "commonjs" },
    rules: { "@typescript-eslint/no-require-imports": "off" },
  },
  {
    // Served as-is to the browser before the bundle (public/theme-init.js).
    files: ["public/**/*.js"],
    languageOptions: { globals: globals.browser, sourceType: "script", ecmaVersion: 5 },
    rules: { "@typescript-eslint/no-unused-vars": ["error", { caughtErrors: "none" }] },
  },
  {
    files: ["*.mjs", "*.config.ts"],
    languageOptions: { globals: globals.node },
  },
);
