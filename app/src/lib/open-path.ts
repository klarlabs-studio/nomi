// Resolve + open filesystem paths from DiffPreview labels.
// Parity with extensions/vscode open_path — Tauri uses plugin-shell
// `open` (default app); vite preview / Playwright no-ops quietly.

export function resolveOpenPathFs(
  pathHint: string,
  workspaceRoot?: string,
): string | null {
  const cleaned = pathHint.trim();
  if (!cleaned || cleaned === "/dev/null") return null;
  // Unix absolute or Windows drive path.
  if (cleaned.startsWith("/") || /^[A-Za-z]:[\\/]/.test(cleaned)) {
    return cleaned;
  }
  if (workspaceRoot && workspaceRoot.trim()) {
    const root = workspaceRoot.replace(/[/\\]+$/, "");
    const rel = cleaned.replace(/^\.\//, "");
    return `${root}/${rel}`;
  }
  return cleaned;
}

function isTauriRuntime(): boolean {
  return (
    typeof window !== "undefined" &&
    typeof (window as unknown as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__ !==
      "undefined"
  );
}

/**
 * Open a path in the OS default app (editor / Finder). Safe outside
 * Tauri — logs and returns without throwing so browser preview stays quiet.
 */
export async function openPathInDesktop(
  pathHint: string,
  workspaceRoot?: string,
): Promise<void> {
  const fsPath = resolveOpenPathFs(pathHint, workspaceRoot);
  if (!fsPath) return;
  if (!isTauriRuntime()) {
    console.info("[nomi] open-path (browser):", fsPath);
    return;
  }
  try {
    const { open } = await import("@tauri-apps/plugin-shell");
    await open(fsPath);
  } catch (err) {
    console.error("[nomi] open-path failed:", err);
  }
}
