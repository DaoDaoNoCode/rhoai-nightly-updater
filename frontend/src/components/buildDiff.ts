import type { NightlyStatus, RelatedImage, StatusResponse } from "../types";

/** "sha256:..." of an image reference, or "" when it has no digest. */
export function imageDigest(image: string | undefined): string {
  const at = image?.indexOf("@sha256:") ?? -1;
  return at >= 0 && image ? image.slice(at + 1) : "";
}

/** "rhoai-3.6" from "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:...", or "". */
export function imageTag(image: string | undefined): string {
  if (!image) return "";
  const ref = image.split("@")[0];
  const colon = ref.lastIndexOf(":");
  return colon > ref.lastIndexOf("/") ? ref.slice(colon + 1) : "";
}

/** A short, readable build reference: "rhoai-3.6 @ 4eff06d". */
export function shortBuildRef(image: string): string {
  const tag = imageTag(image);
  const digest = imageDigest(image).replace("sha256:", "").slice(0, 7);
  if (tag && digest) return `${tag} @ ${digest}`;
  return tag || digest || image;
}

export interface InstalledBuild {
  image: string;
  digest: string;
  tag: string;
  buildDate?: string;
  /** Where it came from: status.nightly (preferred) or the CatalogSource image. */
  source: "nightly" | "catalog";
}

const FBC_REPO = "quay.io/rhoai/rhoai-fbc-fragment";

/**
 * The installed FBC build. status.nightly (B5) is authoritative; without it
 * the nightly CatalogSource image is used, but only when the Subscription
 * installs from that catalog and the image is digest-pinned. Null when RHOAI
 * was not installed from a nightly build.
 */
export function installedBuild(status: Pick<StatusResponse, "catalogSource" | "subscription"> & { nightly?: NightlyStatus } | null): InstalledBuild | null {
  const inst = status?.nightly?.installed;
  if (inst?.image) {
    const digest = inst.digest || imageDigest(inst.image);
    if (digest) return { image: inst.image, digest, tag: inst.tag || imageTag(inst.image), buildDate: inst.buildDate, source: "nightly" };
  }
  const cat = status?.catalogSource;
  if (cat?.exists && !!cat.name && status?.subscription?.source === cat.name && (cat.image.startsWith(`${FBC_REPO}:`) || cat.image.startsWith(`${FBC_REPO}@`))) {
    const digest = imageDigest(cat.image);
    if (digest) return { image: cat.image, digest, tag: imageTag(cat.image), source: "catalog" };
  }
  return null;
}

export interface ImageChange {
  name: string;
  from?: RelatedImage;
  to?: RelatedImage;
}

export interface RepoChange {
  /** Repository URL, or "" when the images carry no git labels. */
  gitURL: string;
  /** "owner/repo" for display. */
  repo: string;
  fromCommit?: string;
  toCommit?: string;
  /** "changed": different commits; "rebuilt": same commit, new image; "unknown": no commit labels. */
  kind: "changed" | "rebuilt" | "unknown";
  images: ImageChange[];
}

export interface BuildComparison {
  repos: RepoChange[];
  added: RelatedImage[];
  removed: RelatedImage[];
  changedImages: number;
  unchangedImages: number;
}

export function repoName(gitURL: string): string {
  return gitURL.replace(/^https?:\/\/github\.com\//, "").replace(/\.git$/, "").replace(/\/$/, "") || gitURL;
}

/** GitHub compare URL between two commits of one repository, or "" when unknown. */
export function compareURL(change: Pick<RepoChange, "gitURL" | "fromCommit" | "toCommit">): string {
  if (!change.gitURL.startsWith("https://github.com/") || !change.fromCommit || !change.toCommit) return "";
  return `${change.gitURL.replace(/\/$/, "")}/compare/${change.fromCommit}...${change.toCommit}`;
}

/**
 * What differs between two builds (A08-5), using only what the API returns:
 * each related image's reference and its git labels. Images are matched by
 * name. Changed images are grouped per repository and commit pair, so one
 * GitHub compare link covers every image built from that repository.
 */
export function compareBuilds(fromImages: RelatedImage[], toImages: RelatedImage[]): BuildComparison {
  const from = new Map(fromImages.map((i) => [i.name, i]));
  const to = new Map(toImages.map((i) => [i.name, i]));
  const groups = new Map<string, RepoChange>();
  let changedImages = 0;
  let unchangedImages = 0;

  for (const [name, a] of from) {
    const b = to.get(name);
    if (!b) continue;
    if (a.image === b.image) {
      unchangedImages++;
      continue;
    }
    changedImages++;
    const gitURL = a.gitURL || b.gitURL || "";
    const known = !!a.gitCommit && !!b.gitCommit;
    const kind: RepoChange["kind"] = !known ? "unknown" : a.gitCommit === b.gitCommit ? "rebuilt" : "changed";
    const key = kind === "unknown" ? `unknown|${gitURL}` : `${gitURL}|${a.gitCommit}|${b.gitCommit}`;
    let group = groups.get(key);
    if (!group) {
      group = {
        gitURL,
        repo: gitURL ? repoName(gitURL) : "No git labels",
        fromCommit: known ? a.gitCommit : undefined,
        toCommit: known ? b.gitCommit : undefined,
        kind,
        images: [],
      };
      groups.set(key, group);
    }
    group.images.push({ name, from: a, to: b });
  }

  const kindRank = { changed: 0, rebuilt: 1, unknown: 2 };
  const repos = [...groups.values()].sort((x, y) => kindRank[x.kind] - kindRank[y.kind] || x.repo.localeCompare(y.repo));
  for (const r of repos) r.images.sort((x, y) => x.name.localeCompare(y.name));
  return {
    repos,
    added: toImages.filter((i) => !from.has(i.name)).sort((x, y) => x.name.localeCompare(y.name)),
    removed: fromImages.filter((i) => !to.has(i.name)).sort((x, y) => x.name.localeCompare(y.name)),
    changedImages,
    unchangedImages,
  };
}

export type SearchKind = "empty" | "image" | "commit" | "pr" | "invalid";

/** What the Build Explorer search box holds. */
export function classifySearch(raw: string): { kind: SearchKind; value: string } {
  const value = raw.trim();
  if (!value) return { kind: "empty", value };
  if (value.includes("/") || value.includes("@") || value.includes(":")) return { kind: "image", value };
  // "#123", "PR 123", "pr#123", "PR #123", or a bare number (up to 6 digits;
  // 7 or more digits may be an abbreviated commit SHA).
  const pr = value.match(/^(?:#|pr\s*#?\s*)(\d{1,7})$/i) ?? value.match(/^(\d{1,6})$/);
  if (pr && parseInt(pr[1], 10) > 0) return { kind: "pr", value: String(parseInt(pr[1], 10)) };
  if (/^[0-9a-f]{7,40}$/i.test(value)) return { kind: "commit", value: value.toLowerCase() };
  return { kind: "invalid", value };
}

/** Related images built from a commit (full SHA or a prefix of at least 7 characters). */
export function imagesBuiltFrom(images: RelatedImage[], sha: string): RelatedImage[] {
  const needle = sha.toLowerCase();
  return images.filter((i) => !!i.gitCommit && i.gitCommit.toLowerCase().startsWith(needle));
}
