// Pure path helpers for the file browser. No React, no DOM — safe to unit test.
// Handles both "relative" (sandboxed to a root, POSIX-only) and "absolute"
// (real filesystem paths, Windows drive letters + backslashes) modes.

export type PathMode = "relative" | "absolute";

export const SEP = "/";

export function sanitizeRelativePath(raw: string): string {
  return raw
    .split(/[/\\]+/)
    .filter((s) => s && s !== "." && s !== "..")
    .join(SEP);
}

export function normalizePath(raw: string, mode: PathMode): string {
  if (mode === "relative") return sanitizeRelativePath(raw);
  return raw;
}

export function pathSep(path: string): string {
  return path.includes("\\") && !path.includes("/") ? "\\" : "/";
}

export function joinBrowserPath(parent: string, name: string, mode: PathMode): string {
  if (mode === "relative") return sanitizeRelativePath([parent, name].filter(Boolean).join(SEP));
  if (!parent) return name;
  const sep = pathSep(parent);
  return parent.endsWith("/") || parent.endsWith("\\") ? `${parent}${name}` : `${parent}${sep}${name}`;
}

export function trimTrailingSep(path: string): string {
  if (!path) return "";
  if (path === "/" || /^[A-Za-z]:[\\/]?$/.test(path)) return path;
  return path.replace(/[/\\]+$/, "");
}

export function unifiedPath(path: string): string {
  const unified = trimTrailingSep(path).replace(/\\/g, "/");
  return unified === "" && path.startsWith("/") ? "/" : unified;
}

export function samePath(a: string, b: string): boolean {
  const ua = unifiedPath(a);
  const ub = unifiedPath(b);
  if (/^[A-Za-z]:/.test(ua) || /^[A-Za-z]:/.test(ub)) return ua.toLowerCase() === ub.toLowerCase();
  return ua === ub;
}

export function pathWithin(path: string, root: string): boolean {
  const p = unifiedPath(path);
  const r = unifiedPath(root);
  if (samePath(p, r)) return true;
  const prefix = r.endsWith("/") ? r : `${r}/`;
  return p.startsWith(prefix);
}

export function parentBrowserPath(path: string, mode: PathMode): string {
  if (mode === "relative") {
    const parts = sanitizeRelativePath(path).split(SEP).filter(Boolean);
    parts.pop();
    return parts.join(SEP);
  }

  const trimmed = trimTrailingSep(path);
  if (!trimmed || trimmed === "/" || /^[A-Za-z]:[\\/]?$/.test(trimmed)) return trimmed;
  const slash = Math.max(trimmed.lastIndexOf("/"), trimmed.lastIndexOf("\\"));
  if (slash < 0) return "";
  if (slash === 0) return "/";
  const parent = trimmed.slice(0, slash);
  return /^[A-Za-z]:$/.test(parent) ? `${parent}${pathSep(trimmed)}` : parent;
}

export function basename(path: string): string {
  const trimmed = trimTrailingSep(path);
  return trimmed.split(/[/\\]+/).filter(Boolean).pop() || trimmed || path;
}

export function splitPath(path: string): string[] {
  return unifiedPath(path).split("/").filter(Boolean);
}

export function fsRoot(path: string): string {
  const drive = path.match(/^[A-Za-z]:[\\/]?/);
  if (drive) return drive[0].endsWith("\\") || drive[0].endsWith("/") ? drive[0] : `${drive[0]}\\`;
  return path.startsWith("/") ? "/" : "";
}

export function relativeSegments(path: string, root: string): string[] {
  if (!root || !pathWithin(path, root)) return splitPath(path);
  const p = unifiedPath(path);
  const r = unifiedPath(root);
  const rel = p.slice(r.length).replace(/^\/+/, "");
  return rel ? rel.split("/").filter(Boolean) : [];
}

export interface Breadcrumb {
  label: string;
  path: string;
  isRoot?: boolean;
}

export function buildBreadcrumbs(
  path: string,
  mode: PathMode,
  rootLabel: string,
  rootPath?: string,
): Breadcrumb[] {
  if (mode === "relative") {
    const parts = sanitizeRelativePath(path).split(SEP).filter(Boolean);
    const crumbs: Breadcrumb[] = [{ label: rootLabel, path: "", isRoot: true }];
    let acc = "";
    for (const p of parts) {
      acc = acc ? `${acc}${SEP}${p}` : p;
      crumbs.push({ label: p, path: acc });
    }
    return crumbs;
  }

  const root = rootPath || fsRoot(path);
  const rootCrumbPath = root || path;
  const crumbs: Breadcrumb[] = [{ label: rootLabel, path: rootCrumbPath, isRoot: true }];
  const segments = root ? relativeSegments(path, root) : splitPath(path);
  let acc = rootCrumbPath;
  for (const segment of segments) {
    if (!root && /^[A-Za-z]:$/.test(segment)) {
      acc = `${segment}\\`;
      continue;
    }
    acc = joinBrowserPath(acc, segment, "absolute");
    crumbs.push({ label: segment, path: acc });
  }
  return crumbs;
}
