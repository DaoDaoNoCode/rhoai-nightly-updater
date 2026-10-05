import { defineConfig } from "vitest/config";

// Component and hook tests (src/**/*.test.ts[x]) run in jsdom. The older
// node:test suites in tests/*.cjs keep running through `node --test`.
export default defineConfig({
  test: {
    environment: "jsdom",
    include: ["src/**/*.test.{ts,tsx}"],
    setupFiles: ["./src/test/setup.ts"],
    restoreMocks: true,
    unstubGlobals: true,
  },
});
