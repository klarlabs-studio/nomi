import * as path from "node:path";

/**
 * Resolve a diff/write path to an absolute filesystem path.
 * Relative paths join the workspace root when available.
 */
export function resolveOpenPathFs(
  pathHint: string,
  workspaceRoot: string | undefined,
): string | null {
  const cleaned = pathHint.trim();
  if (!cleaned || cleaned === "/dev/null") return null;
  if (path.isAbsolute(cleaned)) return cleaned;
  if (workspaceRoot) return path.join(workspaceRoot, cleaned);
  return path.resolve(cleaned);
}
