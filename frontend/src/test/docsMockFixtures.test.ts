import { describe, expect, it } from "vitest";
import ts from "typescript";
import { existsSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";

// The docs mock (docs/tools/mock) answers the app's reads from hand-written
// fixtures, for the documentation screenshots. This type-checks what it
// answers, in every scenario, against the types the app reads them as: each
// answer, as JSON, becomes an object literal that must satisfy its type
// (a missing or misspelled field, a wrong value of a union, a field the type
// does not have).

const here = dirname(fileURLToPath(import.meta.url));
const SRC = resolve(here, "..");
const SCENARIOS = resolve(SRC, "../../docs/tools/mock/scenarios.mjs");

const IMPORTED = ["StatusResponse", "OperationStatusResponse", "UserPermissions", "ComponentsResponse", "DSCManager", "DiagnosticResult", "DashboardState", "ResourcesStatus", "MaaSStatus", "MaaSActionId"];

// What the backend sends (pkg/types) and types.ts does not declare yet; the
// app does not read it, and the mock sends it as the backend does. Remove
// an entry once types.ts has it.
const TYPE_GAPS = `
/** pkg/types MaaSAction.ID: also "change-database" (pkg/cluster maas_database.go). */
type WithChangeDatabase<T> = T extends MaaSActionId ? T | "change-database"
  : T extends (infer U)[] ? WithChangeDatabase<U>[]
  : T extends object ? { [K in keyof T]: WithChangeDatabase<T[K]> } : T;
`;

/** The app's type for each read (services/api.ts), with the gaps above. */
const TYPES: Record<string, string> = {
  // pkg/types StatusResponse.CatalogPod.
  "GET /api/status": "StatusResponse & { catalogPod?: unknown }",
  // pkg/types OperationMarker.ID and BootID.
  "GET /api/operation": "OperationStatusResponse & { interrupted?: { id?: string; bootId?: string } }",
  "GET /api/user/permissions": "UserPermissions",
  "GET /api/components": "ComponentsResponse",
  "GET /api/components/dsc": "DSCManager",
  "GET /api/diagnostics": "DiagnosticResult",
  "GET /api/dashboard/state": "DashboardState",
  "GET /api/resources/status": "ResourcesStatus",
  "GET /api/resources/projects": "{ projects: string[] | null }",
  "GET /api/resources/maas": "WithChangeDatabase<MaaSStatus>",
  "GET /api/activity": "NonNullable<StatusResponse[\"activity\"]>",
};

interface Mock {
  baseWorld: () => unknown;
  scenarios: Record<string, (world: unknown) => void>;
  reads: Record<string, (world: unknown) => unknown>;
}

/** One check per distinct answer: [read, scenarios that answer it, JSON]. */
function answers(): Array<{ read: string; scenarios: string[]; json: string }> {
  // Node loads it (require of an ES module): it lives outside this package.
  const mock = createRequire(import.meta.url)(SCENARIOS) as Mock;
  const byJSON = new Map<string, { read: string; scenarios: string[]; json: string }>();
  for (const [name, apply] of Object.entries(mock.scenarios)) {
    const world = mock.baseWorld();
    apply(world);
    for (const read of Object.keys(TYPES)) {
      const json = JSON.stringify(mock.reads[read](world));
      const key = `${read}\n${json}`;
      const seen = byJSON.get(key);
      if (seen) seen.scenarios.push(name);
      else byJSON.set(key, { read, scenarios: [name], json });
    }
  }
  return [...byJSON.values()];
}

/** The type errors of a virtual file next to this one. */
function typeErrors(source: string): Array<{ line: number; message: string }> {
  const file = join(here, "__docs_mock_answers__.ts");
  const config = ts.readConfigFile(resolve(SRC, "../tsconfig.json"), ts.sys.readFile);
  const { options } = ts.parseJsonConfigFileContent(config.config, ts.sys, resolve(SRC, ".."));
  const host = ts.createCompilerHost({ ...options, noEmit: true });
  const read = host.getSourceFile.bind(host);
  host.getSourceFile = (name, lang, onError, create) =>
    resolve(name) === file ? ts.createSourceFile(name, source, lang) : read(name, lang, onError, create);
  const program = ts.createProgram([file], { ...options, noEmit: true }, host);
  const sf = program.getSourceFile(file)!;
  return ts.getPreEmitDiagnostics(program, sf).map((d) => ({
    line: d.start === undefined ? 0 : sf.getLineAndCharacterOfPosition(d.start).line,
    message: ts.flattenDiagnosticMessageText(d.messageText, "\n"),
  }));
}

// The image build (Containerfile) copies only frontend/, without the docs;
// CI's test jobs run in the whole checkout, where they are.
describe.skipIf(!existsSync(SCENARIOS))("the docs mock", () => {
  it("answers every read, in every scenario, in the shape the app's types say", () => {
    const all = answers();
    const lines = [`import type { ${IMPORTED.join(", ")} } from "../types";`, ...TYPE_GAPS.trim().split("\n")];
    const where: string[] = [];
    for (const a of all) {
      where[lines.length] = `${a.read} (scenarios ${a.scenarios.slice(0, 3).join(", ")}${a.scenarios.length > 3 ? ", ..." : ""})`;
      lines.push(`void (${a.json} satisfies ${TYPES[a.read]});`);
    }
    const errors = typeErrors(lines.join("\n")).map((e) => `${where[e.line] ?? `line ${e.line}`}: ${e.message.slice(0, 600)}`);
    expect(all.length).toBeGreaterThan(Object.keys(TYPES).length);
    expect(errors).toEqual([]);
  });
});
