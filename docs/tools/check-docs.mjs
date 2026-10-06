#!/usr/bin/env node
// Checks the Markdown docs: every relative link and image path exists, every
// #anchor matches a heading (GitHub's slugs), and every file in docs/images
// is used. Exits 1 on a problem. Run: make docs-check
import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..");
const skip = new Set(["node_modules", ".git", ".claude", "dist", "tmp"]);

function markdownFiles(dir) {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    if (skip.has(name)) return [];
    if (statSync(path).isDirectory()) return markdownFiles(path);
    return name.endsWith(".md") ? [path] : [];
  });
}

/** GitHub's heading anchors: lower case, punctuation dropped, spaces to dashes, duplicates numbered. */
function anchors(md) {
  const seen = new Map();
  const out = new Set();
  for (const line of md.replace(/```[\s\S]*?```/g, "").split("\n")) {
    const m = line.match(/^#{1,6}\s+(.*)$/);
    if (!m) continue;
    let slug = m[1].replace(/<[^>]+>/g, "").replace(/`/g, "").replace(/\[([^\]]*)\]\([^)]*\)/g, "$1")
      .trim().toLowerCase().replace(/[^\p{L}\p{N}\s_-]/gu, "").replace(/\s/g, "-");
    const n = seen.get(slug) ?? 0;
    seen.set(slug, n + 1);
    if (n) slug += `-${n}`;
    out.add(slug);
  }
  return out;
}

const files = markdownFiles(root);
const problems = [];
const usedImages = new Set();
for (const file of files) {
  const md = readFileSync(file, "utf8").replace(/```[\s\S]*?```/g, "");
  const targets = [
    ...[...md.matchAll(/\]\(([^)\s]+)(?:\s+"[^"]*")?\)/g)].map((m) => m[1]),
    ...[...md.matchAll(/\b(?:src|srcset|href)="([^"]+)"/g)].map((m) => m[1]),
  ];
  for (const target of targets) {
    if (/^(https?:|mailto:)/.test(target)) continue;
    const [path, anchor] = target.split("#");
    const abs = path ? resolve(dirname(file), decodeURIComponent(path)) : file;
    const where = `${relative(root, file)}: ${target}`;
    if (!existsSync(abs)) { problems.push(`missing file  ${where}`); continue; }
    if (abs.startsWith(join(root, "docs", "images"))) usedImages.add(abs);
    if (anchor && abs.endsWith(".md") && !anchors(readFileSync(abs, "utf8")).has(anchor)) problems.push(`missing anchor ${where}`);
  }
}
for (const name of readdirSync(join(root, "docs", "images"))) {
  const abs = join(root, "docs", "images", name);
  if (!usedImages.has(abs)) problems.push(`unused image  docs/images/${name}`);
}
console.log(`${files.length} Markdown files, ${usedImages.size} images used`);
for (const p of problems) console.log(p);
process.exit(problems.length ? 1 : 0);
