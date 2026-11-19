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
