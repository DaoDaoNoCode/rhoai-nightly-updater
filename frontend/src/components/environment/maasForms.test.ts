import { describe, expect, it } from "vitest";
import {
  audienceProblem, audienceRequest, audienceText, connectionUrlProblem, createdDatabaseInSentence, databaseProblem, databaseRequest, defaultDatabaseForm,
  defaultLimitForm, limitProblem, limitRequest, limitSummary, limitText, openShiftGroupName, windowText,
} from "./maasForms";
import type { MaaSDatabase } from "../../types";

const db: MaaSDatabase = {
  current: "", namespace: "redhat-ai-gateway-infra", needed: true, default: "development",
  choices: [{ id: "external", plan: ["a"] }, { id: "development", plan: ["b"] }, { id: "cnpg", plan: ["c"] }],
};

describe("the database form", () => {
  it("preselects the server's default, or the first usable choice", () => {
    expect(defaultDatabaseForm(db).kind).toBe("development");
    expect(defaultDatabaseForm({ ...db, choices: [db.choices[0], { id: "development", plan: [], blockedReason: "taken" }, db.choices[2]] }).kind).toBe("external");
    // Change database preselects nothing.
    expect(defaultDatabaseForm(db, "development").kind).toBe("");
    expect(databaseProblem({ kind: "", connectionUrl: "" }, db)).toBe("Choose a database.");
  });

  it("checks the URL as the server does, without quoting it", () => {
    const secret = "S3cretPw";
    expect(connectionUrlProblem(`postgresql://u:${secret}@db.example.com:5432/maas?sslmode=require`)).toBeNull();
    expect(connectionUrlProblem("postgres://u@db/maas")).toBeNull();
    for (const [raw, want] of [
      ["", "Enter"], [`mysql://u:${secret}@db/maas`, "must begin with postgresql://"], [`postgresql://u:${secret}@/maas`, "names no host"],
      ["postgresql://db/maas", "names no user"], [`postgresql://u:${secret}@db:99999/maas`, "port must be 1 to 65535"],
      [`postgresql://u:${secret}@db/`, "names no database"], [`postgresql://u:${secret} x@db/maas`, "space"], [`postgresql://u:${secret}#@db/maas`, "#"],
    ] as const) {
      const got = connectionUrlProblem(raw);
      expect(got, raw).toContain(want);
      expect(got).not.toContain(secret);
    }
  });

  it("sends the URL only for Your PostgreSQL", () => {
    expect(databaseRequest({ kind: "development", connectionUrl: "postgresql://u:p@h/d" })).toEqual({ kind: "development" });
    expect(databaseRequest({ kind: "external", connectionUrl: " postgresql://u:p@h/d " })).toEqual({ kind: "external", connectionUrl: "postgresql://u:p@h/d" });
    expect(databaseProblem({ kind: "cnpg", connectionUrl: "" }, { ...db, choices: [{ id: "cnpg", plan: [], blockedReason: "not in the catalog" }] })).toBe("not in the catalog");
  });
  it("names a database this tool created inside a sentence, without parentheses around a label that has its own", () => {
    expect(createdDatabaseInSentence("development")).toBe("the development database");
    expect(createdDatabaseInSentence("cnpg")).toBe("the persistent development database (CloudNativePG)");
    for (const kind of ["external", "adopted", ""]) expect(createdDatabaseInSentence(kind)).toBe("the database this tool created");
  });
});

describe("the limit form", () => {
  it("sends a preset as it is and custom rates as numbers", () => {
    expect(limitRequest({ preset: "unlimited", rows: [] })).toEqual({ preset: "unlimited" });
    expect(limitRequest({ preset: "custom", rows: [{ limit: "500", amount: "1", unit: "m" }, { limit: "20000", amount: "24", unit: "h" }] }))
      .toEqual({ preset: "custom", rates: [{ limit: 500, window: "1m" }, { limit: 20000, window: "24h" }] });
  });

  it("refuses what the CRD refuses", () => {
    const one = (limit: string, amount: string, unit: "s" | "m" | "h" = "m") => limitProblem({ preset: "custom", rows: [{ limit, amount, unit }] });
    expect(one("100", "1")).toBeNull();
    expect(one("0", "1")).toContain("1 to 1,000,000,000");
    expect(one("1000000001", "1")).toContain("Unlimited");
    expect(one("1.5", "1")).toContain("whole number");
    expect(one("5", "10000", "s")).toContain("9999");
    expect(limitProblem({ preset: "custom", rows: [{ limit: "1", amount: "1", unit: "m" }, { limit: "2", amount: "1", unit: "m" }] })).toBe("Another limit is per minute already.");
    expect(limitProblem({ preset: "custom", rows: [] })).toContain("at least one");
  });

  it("reads an existing limit back into the form", () => {
    expect(defaultLimitForm({ preset: "custom", rates: [{ limit: 7, window: "30s" }] })).toEqual({ preset: "custom", rows: [{ limit: "7", amount: "30", unit: "s" }] });
    expect(defaultLimitForm().preset).toBe("premium");
    expect(limitSummary({ preset: "custom", rows: [{ limit: "100000", amount: "24", unit: "h" }, { limit: "10000", amount: "1", unit: "h" }] }))
      .toBe("Each user: 100,000 tokens a day and 10,000 tokens an hour; whichever runs out first stops them.");
    expect(limitSummary({ preset: "custom", rows: [{ limit: "0", amount: "1", unit: "h" }] })).toBeNull();
    expect(limitProblem({ preset: "unlimited", rows: [] }, true)).toBe("This cluster's MaaS has no unlimited option.");
    expect(limitProblem({ preset: "unlimited", rows: [] })).toBeNull();
  });

  it("says limits in words", () => {
    expect(limitText({ preset: "free", rates: [{ limit: 100, window: "1m" }] })).toBe("100 tokens a minute");
    expect(limitText({ preset: "premium", rates: [{ limit: 100000, window: "24h" }] })).toBe("100,000 tokens a day");
    expect(limitText({ preset: "unlimited", unlimited: true })).toBe("Unlimited (usage still metered)");
    expect(limitText({ preset: "custom", rates: [{ limit: 1, window: "2h" }, { limit: 5, window: "1h" }] })).toBe("1 token every 2 hours and 5 tokens an hour");
    expect(windowText("1s")).toBe("a second");
  });
});

describe("the audience form", () => {
  it("sends everyone, or the names one per line (a name may have commas)", () => {
    expect(audienceRequest({ everyone: true, groups: "x", users: "" })).toEqual({ everyone: true });
    expect(audienceRequest({ everyone: false, groups: "ml-team\r\n data \nml-team\ncn=admins,ou=groups", users: "system:serviceaccount:ci:runner" }))
      .toEqual({ everyone: false, groups: ["ml-team", "data", "cn=admins,ou=groups"], users: ["system:serviceaccount:ci:runner"] });
  });

  it("takes names as MaaS matches them: exactly, as the identity provider sends them", () => {
    expect(audienceProblem({ everyone: false, groups: "", users: "" })).toContain("at least one");
    expect(audienceProblem({ everyone: false, groups: "system:authenticated", users: "" })).toContain("Everyone signed in");
    expect(audienceProblem({ everyone: false, groups: "/admins\noidc:ml\na%b\n~", users: "alice:bob\nsystem:serviceaccount:Bad:x" })).toBeNull();
    expect(audienceProblem({ everyone: false, groups: "a\tb", users: "" })).toContain("control character");
    expect(audienceProblem({ everyone: false, groups: "", users: "x".repeat(254) })).toContain("253");
  });

  it("knows which names can be OpenShift groups", () => {
    for (const n of ["ml-team", "ML Team", "team.a"]) expect(openShiftGroupName(n), n).toBe(true);
    for (const n of ["/admins", "oidc:ml", "a%b", "..", ".", "~"]) expect(openShiftGroupName(n), n).toBe(false);
  });

  it("says the audience in words", () => {
    expect(audienceText({ everyone: true })).toBe("Everyone signed in to the cluster");
    expect(audienceText({ everyone: false, groups: ["a", "b"], users: ["x"] })).toBe("Only groups a, b and user x");
  });
});
