import type { MaaSAudience, MaaSDatabase, MaaSDatabaseKind, MaaSDatabaseRequest, MaaSLimit, MaaSTokenRate } from "../../types";

/*
 * The MaaS dialogs' forms (Set up's database, Publish's audience and limit,
 * Change limit, Change who can use them, Change database): their state,
 * the same checks the server makes (pkg/cluster maas_database.go,
 * maas_subscription.go), and the request each one sends. The connection
 * URL lives only in this form's state and the one request: it is never
 * shown again, logged or kept.
 */

export interface DatabaseForm {
  /** "": none chosen yet (Change database preselects nothing). */
  kind: MaaSDatabaseKind | "";
  connectionUrl: string;
  showUrl?: boolean;
  /** The URL field was left (or Set up tried): its problem shows from then on, not at every keystroke. */
  urlTouched?: boolean;
}

export type LimitPreset = MaaSLimit["preset"];
export type WindowUnit = "s" | "m" | "h";

/** One custom rate as typed: whole numbers as text. */
export interface RateRow {
  limit: string;
  amount: string;
  unit: WindowUnit;
}

export interface LimitForm {
  preset: LimitPreset;
  rows: RateRow[];
}

export interface AudienceForm {
  everyone: boolean;
  /** One name per line. */
  groups: string;
  users: string;
}

export const MAX_RATES = 5;
export const MAX_NAMES = 20;
export const MAX_TOKENS = 1_000_000_000;

/** The docs' presets (quota-and-access-configuration.md), in the order the dialogs list them. */
export const LIMIT_PRESETS: { id: Exclude<LimitPreset, "custom">; label: string; description: string }[] = [
  { id: "free", label: "Free example: 100 tokens a minute", description: "The MaaS docs' free example: about one short answer a minute. The playground reaches it quickly." },
  { id: "premium", label: "Premium example: 100,000 tokens a day", description: "The MaaS docs' premium example." },
  { id: "unlimited", label: "Unlimited", description: "No cap: calls are never refused for usage. Usage is still counted and shows in MaaS's metrics." },
];

/** Where MaaS keeps its API keys: what each choice is, in the dialog's words, in the dialog's order. */
export const DATABASE_CHOICES: { id: MaaSDatabaseKind; label: string; description: string; notInDocs?: boolean }[] = [
  { id: "development", label: "Development database",
    description: "One PostgreSQL pod with no storage, set up like the MaaS docs' setup-database.sh. For trying MaaS out." },
  { id: "cnpg", label: "Persistent development database (CloudNativePG)", notInDocs: true,
    description: "Keeps API keys across restarts on a 1 GiB volume. Installs the CloudNativePG operator from certified-operators; it stays after Remove. Not for production." },
  { id: "external", label: "Your PostgreSQL",
    description: "For API keys that must last. This tool never changes or deletes your database." },
];

export const databaseLabel = (kind: string): string =>
  kind === "adopted" ? "A database of someone else's (maas-db-config not written by this tool)" : DATABASE_CHOICES.find((c) => c.id === kind)?.label ?? "None yet";

export const defaultDatabaseForm = (db: MaaSDatabase | undefined, except?: string): DatabaseForm => {
  if (except !== undefined) return { kind: "", connectionUrl: "" };
  const usable = (db?.choices ?? []).filter((c) => !c.blockedReason && c.id !== except).map((c) => c.id);
  const kind = db && usable.includes(db.default) ? db.default : usable[0] ?? db?.default ?? "development";
  return { kind, connectionUrl: "" };
};

export const defaultLimitForm = (limit?: MaaSLimit): LimitForm => {
  if (!limit) return { preset: "premium", rows: [{ limit: "10000", amount: "1", unit: "h" }] };
  const rows = (limit.rates ?? []).map((r) => {
    const m = /^(\d+)([smh])$/.exec(r.window);
    return { limit: String(r.limit), amount: m?.[1] ?? "1", unit: (m?.[2] ?? "m") as WindowUnit };
  });
  return { preset: limit.preset, rows: rows.length > 0 ? rows : [{ limit: "10000", amount: "1", unit: "h" }] };
};

export const defaultAudienceForm = (a?: MaaSAudience): AudienceForm => ({
  everyone: a ? a.everyone : true,
  groups: (a?.groups ?? []).join("\n"),
  users: (a?.users ?? []).join("\n"),
});

/** The connection URL's problem, or null (the server's checks, in its words; never quoting the URL). */
export function connectionUrlProblem(raw: string): string | null {
  const format = "Use postgresql://USER:PASSWORD@HOST:PORT/DATABASE?sslmode=require, with special characters in the password percent-encoded.";
  if (!raw) return "Enter your PostgreSQL's connection URL.";
  if (raw.length > 2048) return "The connection URL is longer than 2048 characters.";
  if (/[\s\p{Cc}]/u.test(raw)) return `The connection URL has a space or a control character. ${format}`;
  if (raw.includes("#")) return "The connection URL has a #: percent-encode it in the password as %23.";
  const m = /^(postgresql|postgres):\/\/([^/?]*)(\/[^?]*)?(\?.*)?$/.exec(raw);
  if (!m) return `The connection URL must begin with postgresql:// or postgres://. ${format}`;
  const authority = m[2];
  const at = authority.lastIndexOf("@");
  const user = at >= 0 ? authority.slice(0, at).split(":")[0] : "";
  const hostPort = at >= 0 ? authority.slice(at + 1) : authority;
  const port = /:(\d*)$/.exec(hostPort)?.[1];
  const host = port !== undefined ? hostPort.slice(0, hostPort.length - port.length - 1) : hostPort;
  if (!host) return `The connection URL names no host. ${format}`;
  if (!user) return `The connection URL names no user. ${format}`;
  if (port !== undefined && (!/^\d+$/.test(port) || Number(port) < 1 || Number(port) > 65535)) return "The connection URL's port must be 1 to 65535.";
  const db = (m[3] ?? "").replace(/^\//, "");
  if (!db || db.includes("/")) return `The connection URL names no database. ${format}`;
  return null;
}

export function databaseRequest(f: DatabaseForm): MaaSDatabaseRequest {
  return f.kind === "external" ? { kind: "external", connectionUrl: f.connectionUrl.trim() } : { kind: f.kind || "development" };
}

export function databaseProblem(f: DatabaseForm, db: MaaSDatabase | undefined): string | null {
  const blocked = db?.choices.find((c) => c.id === f.kind)?.blockedReason;
  if (blocked) return blocked;
  if (!f.kind) return "Choose a database.";
  return f.kind === "external" ? connectionUrlProblem(f.connectionUrl.trim()) : null;
}

/** One name per line: a name may have commas (an LDAP-style group). */
const names = (text: string) => [...new Set(text.split("\n").map((n) => n.trim()).filter(Boolean))];

/*
 * Group and user names are matched exactly by MaaS, as the identity
 * provider sends them (an OIDC group like /admins or oidc:ml is valid), so
 * only what cannot be a name is refused (pkg/cluster parseMaaSAudience).
 * Whether a group has an OpenShift Group is a warning, not a refusal.
 */
// eslint-disable-next-line no-control-regex
const CONTROL = /[\u0000-\u001f\u007f-\u009f]/;

const nameProblem = (what: "Group" | "User", n: string): string | null => {
  if (CONTROL.test(n)) return `${what} names cannot have a control character (a tab or a line break inside a name).`;
  if (n.length > 253) return `${what} "${n.slice(0, 40)}…" is longer than 253 characters.`;
  return null;
};

export function groupNameProblem(n: string): string | null {
  if (n === "system:authenticated") return "system:authenticated is everyone signed in: choose Everyone signed in.";
  return nameProblem("Group", n);
}

export const userNameProblem = (n: string): string | null => nameProblem("User", n);

/** The name can be an OpenShift Group's (apiserver-library-go uservalidation.go); one that cannot has no Group object. */
export const openShiftGroupName = (n: string): boolean => n !== "." && n !== ".." && n !== "~" && !/[/%:]/.test(n);

export function audienceRequest(f: AudienceForm): MaaSAudience {
  return f.everyone ? { everyone: true } : { everyone: false, groups: names(f.groups), users: names(f.users) };
}

export function audienceProblem(f: AudienceForm): string | null {
  if (f.everyone) return null;
  const groups = names(f.groups);
  const users = names(f.users);
  if (groups.length === 0 && users.length === 0) return "Name at least one group or user, or choose Everyone signed in.";
  if (groups.length > MAX_NAMES || users.length > MAX_NAMES) return `Name at most ${MAX_NAMES} groups and ${MAX_NAMES} users.`;
  for (const g of groups) {
    const p = groupNameProblem(g);
    if (p) return p;
  }
  for (const u of users) {
    const p = userNameProblem(u);
    if (p) return p;
  }
  return null;
}

export function limitRequest(f: LimitForm): MaaSLimit {
  if (f.preset !== "custom") return { preset: f.preset };
  return { preset: "custom", rates: f.rows.map((r) => ({ limit: Number(r.limit), window: `${Number(r.amount)}${r.unit}` })) };
}

/** Row i's problem (the CRD's rules: 1 to 1,000,000,000 tokens; 1 to 9999 s, m or h; each window once), or null. */
export function rateRowProblem(f: LimitForm, i: number): string | null {
  const r = f.rows[i];
  if (!/^\d+$/.test(r.limit) || Number(r.limit) < 1 || Number(r.limit) > MAX_TOKENS) return "Enter a whole number of tokens from 1 to 1,000,000,000. For no cap choose Unlimited.";
  if (!/^\d+$/.test(r.amount) || Number(r.amount) < 1 || Number(r.amount) > 9999) return "Enter a period of 1 to 9999 seconds, minutes or hours.";
  const w = `${Number(r.amount)}${r.unit}`;
  if (f.rows.slice(0, i).some((o) => `${Number(o.amount)}${o.unit}` === w)) return `Another limit is per ${windowText(w).replace(/^(a|an|every) /, "")} already.`;
  return null;
}

/** The limit's problem, or null. */
export function limitProblem(f: LimitForm, noUnlimited = false): string | null {
  if (f.preset === "unlimited" && noUnlimited) return "This cluster's MaaS has no unlimited option.";
  if (f.preset !== "custom") return null;
  if (f.rows.length === 0) return "Add at least one token rate limit.";
  if (f.rows.length > MAX_RATES) return `At most ${MAX_RATES} token rate limits.`;
  for (let i = 0; i < f.rows.length; i++) {
    const p = rateRowProblem(f, i);
    if (p) return p;
  }
  return null;
}

/** A valid custom limit in words: "Each user: 100,000 tokens a day and 10,000 tokens an hour; whichever runs out first stops them." */
export function limitSummary(f: LimitForm): string | null {
  if (f.preset !== "custom" || limitProblem(f)) return null;
  const parts = f.rows.map((r) => `${Number(r.limit).toLocaleString("en-US")} ${Number(r.limit) === 1 ? "token" : "tokens"} ${windowText(`${Number(r.amount)}${r.unit}`)}`);
  return parts.length === 1 ? `Each user: ${parts[0]}.` : `Each user: ${joinWords(parts)}; whichever runs out first stops them.`;
}

/** "a minute", "an hour", "a day", "every 30 seconds" (pkg/cluster windowText). */
export function windowText(w: string): string {
  if (w === "24h") return "a day";
  const m = /^(\d+)([smh])$/.exec(w);
  if (!m) return w;
  const unit = { s: "second", m: "minute", h: "hour" }[m[2] as WindowUnit];
  if (m[1] === "1") return unit === "hour" ? "an hour" : `a ${unit}`;
  return `every ${m[1]} ${unit}s`;
}

const joinWords = (parts: string[]) => parts.length <= 1 ? parts.join("") : `${parts.slice(0, -1).join(", ")} and ${parts[parts.length - 1]}`;

/** A limit in words (pkg/cluster maasLimit.text). */
export function limitText(l: MaaSLimit | undefined): string {
  if (!l) return "100 tokens a minute";
  if (l.unlimited || l.preset === "unlimited") return "Unlimited (usage still metered)";
  const rates: MaaSTokenRate[] = l.rates ?? [];
  return joinWords(rates.map((r) => `${r.limit.toLocaleString("en-US")} ${r.limit === 1 ? "token" : "tokens"} ${windowText(r.window)}`));
}

/** An audience in words. */
export function audienceText(a: MaaSAudience | undefined): string {
  if (!a || a.everyone) return "Everyone signed in to the cluster";
  const parts: string[] = [];
  if (a.groups?.length) parts.push(`${a.groups.length === 1 ? "group" : "groups"} ${a.groups.join(", ")}`);
  if (a.users?.length) parts.push(`${a.users.length === 1 ? "user" : "users"} ${a.users.join(", ")}`);
  return `Only ${joinWords(parts)}`;
}
