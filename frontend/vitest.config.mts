import { defineConfig } from "vitest/config";

// Component and hook tests (src/**/*.test.ts[x]) run in jsdom. The older
// node:test suites in tests/*.cjs keep running through `node --test`.
export default defineConfig({
  test: {
    environment: "jsdom",
    include: ["src/**/*.test.{ts,tsx}"],
    setupFiles: ["./src/test/setup.ts"],
    // A test drives whole pages in jsdom (open a dialog, close it, reopen
    // it, finish the run): about 1s on a laptop, but several times that in a
    // CPU-limited build (an OpenShift build pod ran one past vitest's 5s
    // default). Nothing waits on real time; this only bounds a hung test.
    testTimeout: 20_000,
    restoreMocks: true,
    unstubGlobals: true,
  },
});
